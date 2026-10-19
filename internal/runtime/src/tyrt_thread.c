/* tyrt_thread.c - threads, monitors and the stop-the-world protocol.
 *
 * This file is what makes the runtime multi-threaded. It owns four things, and
 * they are one design rather than four features, so they are described together
 * here: the per-thread state (tyrt.h's tythread), the thread registry the
 * collector walks, the protocol by which a collection stops every other thread,
 * and the monitors `synchronized` and Object.wait are built on.
 *
 * ------------------------------------------------------------------ threads
 *
 * A Teyru thread is an operating system thread, created detached. Detached
 * because a thread the program never joins must not hold its stack until the
 * program ends, and because nothing in the language waits for a thread to leave
 * the operating system -- join() wait for the *program's* view of the thread
 * finishing, which is the `finished` flag, and that is set by the thread itself
 * before it returns. The registry entry outlives the thread on purpose: it is
 * what isAlive(), getId() and join() answer from, and a program may ask about a
 * thread long after it has ended.
 *
 * --------------------------------------------------------------- collection
 *
 * Allocation is per-thread: a thread bumps a pointer through its own slab of
 * the heap and touches no lock until the slab runs out. The collector is a
 * conservative mark-and-sweep over a heap the threads are actively writing, so
 * it has to stop the world before it traces anything, and stopping the world
 * means reaching a state where every thread's roots -- its stack and its
 * registers -- are still, and every thread is out of the heap.
 *
 * The protocol is cooperative, in the tradition of a safepoint:
 *
 *   - The collector takes the *heap* lock (ty_heap_lock), sets ty_stw_request,
 *     and waits for every other thread to report that it has stopped.
 *   - A thread reports that it has stopped by calling one of the stop points:
 *     a safepoint (ty_safepoint, which the generated code puts at the top of
 *     every loop body, and which the allocation slow path calls first), a call
 *     that blocks (sleep, join, a monitor wait, a socket read or write, a file
 *     or pipe read or write, a line from the console), or the wait for the heap
 *     lock itself. All of them record where the thread stopped, spill the
 *     registers that would otherwise be invisible to a stack scan, and take
 *     themselves out of the set of threads the collector has to wait for.
 *   - They wait, and start running again, when the collector clears the request,
 *     which happens only after the trace and the sweep are done. So a thread
 *     that stopped is still while the collector walks its stack, and the write
 *     ordering that matters is the one the two mutexes give: everything a thread
 *     did before it stopped is visible to the collector, and everything the
 *     collector did before it resumed is visible to the thread.
 *
 * What is a safepoint and what is not, honestly:
 *
 *   - a safepoint: the top of every loop body in generated code; the allocation
 *     slow path (before it takes the heap lock); the wait for the heap lock;
 *     sleep, join, and every monitor wait; the start of a thread; and every call
 *     in the runtime that waits for data to arrive or to be accepted -- the
 *     socket reads, writes, accept and connect waits, the TLS handshake and its
 *     reads and writes, the file and pipe reads and writes, and a line from the
 *     console. Each of those brackets its wait with ty_thread_stopped_begin and
 *     ty_thread_stopped_end, and the ones that hold an object across the wait --
 *     the byte array a read is filling or a write is draining, the string a
 *     write is draining -- put it on the shadow stack for the whole of the wait,
 *     because the runtime's pointer into it may live only in a register whose
 *     spill is below the stop point, where the scan of a stopped thread does not
 *     reach.
 *   - not a safepoint: a call that neither loops nor allocates nor waits. A
 *     thread inside one -- a long computation in a native method -- does not
 *     reach a stop point until it comes back, and a collection waits for it.
 *     That is the price of cooperative safepoints: the collector cannot
 *     interrupt a thread that never cooperates. The same is true of the calls
 *     that ask the kernel or the resolver a question and come back with an
 *     answer -- open, stat, opendir, readdir, getaddrinfo, and the buffered
 *     writes of the C library. Those are one call each and return on the
 *     system's own schedule rather than when a peer or a device decides to move
 *     data; a thread inside one stalls a collection for as long as the call
 *     takes, which is the price of leaving them out and is not the unbounded
 *     wait the calls above are. A native method that blocks on something of its
 *     own would have to call ty_thread_stopped_begin/end around its wait.
 *
 * Registers. A stack scan sees what a thread has spilled, and a value a frame
 * holds only in a callee-saved register is not in any of that -- the register is
 * the only copy, and the collector reads memory. Every stop point therefore
 * begins with __builtin_unwind_init(), which makes the compiler spill the
 * callee-saved registers into the frame that is about to stop, above the point
 * the thread records, where the scan finds them. The one place this cannot reach
 * is the frame of the function that blocks *below* the stop point -- the
 * platform layer's own wait (typlat_cond_wait, typlat_sleep_ns), the recv or
 * send under a socket call, and whatever a library between them is holding
 * (OpenSSL, for a TLS call): its saves are below the recorded point, so the
 * collector starts its scan a fixed margin lower (TY_PARK_MARGIN in tyrt.c),
 * scanning a few hundred extra bytes conservatively. Everything the runtime
 * itself holds across such a wait it holds on its shadow stack, which is exact
 * and needs none of this.
 *
 * ---------------------------------------------------------------- monitors
 *
 * A monitor is a mutex, an owner and a recursion count, in a hash table keyed by
 * the object's address. A table rather than a field of the object because a
 * field would be eight bytes in every object in every allocation -- including
 * every object of every program that never synchronizes on anything -- and would
 * change the instance layout the compiler computes field offsets from
 * (tyclass.refoffs). The table is a fixed array of buckets, each a list of
 * entries, and entries are never freed: a stale entry for a dead object would
 * otherwise have to be found and removed, and a new object that reuses a dead
 * object's address could not tell the difference. Lookups need no lock of their
 * own -- the lists are append-only, so a reader that loads a bucket head and
 * walks it is always looking at a list that cannot change under it -- and an
 * entry's own mutex guards its fields.
 *
 * Two invariants hold this together, and both of them were broken by the first
 * version of it, in ways that cost no more than a race on a slow machine:
 *
 *   - An object has exactly one entry. The entry is the whole of its monitor's
 *     state, so two entries for one object are two monitors, and threads that
 *     synchronize on the same object then neither exclude each other nor find
 *     each other's ownership: a thread that wakes from wait() and leaves the
 *     synchronized block it waited in is told it is not the owner, and two
 *     threads that wait on one object and are notified find only the waiters
 *     that happen to share their entry. mon_get (below) is where that is kept.
 *
 *   - Waiting to enter a monitor and waiting inside Object.wait are different
 *     waits and sleep on different condition variables: notify() has to wake a
 *     thread that was waiting in wait(), and it must not be spent on a thread
 *     that was only waiting for the monitor to be free. Object.wait is a count
 *     of notifications that only moves forward, never a count of unclaimed
 *     ones. The tymon comment below says the rest.
 */

/* The descriptor wait in ty_wait_fd is the platform layer's socket wait for a
   platform thread, so this file needs its declarations; on Windows the
   virtual-thread path never runs and ty_wait_fd is a stub. */
#define TYPLAT_WANT_SOCKETS 1
#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if !defined(_WIN32)
/* Virtual threads are continuations: a ucontext with its own stack, swapped in
   and out of a carrier thread. The POSIX implementation is the whole of it.
   Windows has no ucontext, and ty_thread_start_virtual0 there refuses by name
   rather than falling back to a platform thread -- see the end of this file. */
#include <sys/mman.h>
#include <ucontext.h>
#include <unistd.h>
#include <fcntl.h>
#include <poll.h>
#endif

/* ------------------------------------------------------------------- state */

/* The main thread's shadow stack. It is a static array, as it has always been;
   the threads this file starts allocate theirs (TY_SHADOW_THREAD entries), and
   the runtime pushes a handful of roots at a time, so either size is far more
   than any of them uses. */
static void *main_roots[TY_SHADOW_MAX];

static tythread main_thread = {
    .roots = main_roots,
    .id = 1,
    .state = TY_TH_RUNNING,
};

/* The thread-local pointer to the current thread's state. Static storage for the
   main thread, and set by the trampoline below for every other one. */
_Thread_local tythread *ty_self = &main_thread;

/* The registry: every thread this runtime started, the main thread, and one
   entry for every Thread object that has been built (an unstarted object's
   entry is not live; it is where its interrupt status lives). Held
   head-inserted and never removed from; see the file comment. */
static tythread *threads = NULL;
/* How many registered threads have not finished, including the caller, and how
   many of those are stopped (parked, blocked, or waiting for a lock). Both are
   guarded by world_mtx. A collection waits until every thread but itself is
   stopped: n_stopped >= n_live - 1 is that statement, and it needs no count of
   running threads, which is what makes a thread that finishes in the middle of
   the wait harmless. */
static int32_t n_live = 0;
static int32_t n_stopped = 0;
/* How many of the live threads are daemons. The program ends when the last
   non-daemon thread does, so ty_thread_join_all waits on n_live - n_daemon;
   this is the whole of daemon semantics on the runtime side. */
static int32_t n_daemon = 0;

/* The world lock. It guards the counters and the registry, and it is what
   stopped threads wait on. It is deliberately not the heap lock: the collector
   holds the heap lock for the whole collection, so a thread stopping for that
   collection must still be able to take this one. Nothing that runs under the
   heap lock ever takes a monitor, and nothing that runs under this lock takes
   the heap lock, so the two cannot deadlock against each other. */
static typlat_mutex world_mtx = TYPLAT_MUTEX_INITIALIZER;
static typlat_cond world_cv = TYPLAT_COND_INITIALIZER;

int32_t ty_stw_request = 0;

/* The id Thread.getId() reports for a thread object that has not been started
   yet: java.lang.Thread assigns one at construction and never reuses it, and so
   does this counter. The main thread is 1. */
static int64_t thread_seq = 1;

/* ----------------------------------------------------------------- stopping */

/* ty_park_point records where the calling thread stopped. It is called from
   TY_PARK_BEGIN, in the frame that is about to block, because that is the frame
   the scan of a stopped thread starts above. `park_sp` only ever moves down:
   stopping deeper than any earlier stop widens nothing, and a value from an
   earlier stop would start the scan below the frames that matter. */
static void park_point(char *anchor) {
  /* The fast path in tyrt.h bumps a pointer and nothing else, so the slab's
     watermark trails behind it. The collector walks slabs by that watermark, so
     it has to be exact before the thread stops. */
  ty_heap_sync();
  /* The frame chain as it is now. A stopped thread's stack is still while a
     collection runs, so the chain it publishes here is the chain it has, and it
     is read from another thread through this field because C11 has no way to
     reach a thread-local of a thread that is not running. Unlike park_sp this is
     the *current* value and not a low-water mark: park_sp only moves down
     because a deeper value scans more, but a frame map that is not in the chain
     is a root the precise walk does not take, so this one is published as it is
     every time the thread stops. */
  ty_self->frames = ty_frames;
  if (!ty_self->park_sp || (uintptr_t)anchor < (uintptr_t)ty_self->park_sp) {
    ty_self->park_sp = anchor;
  }
}

/* TY_PARK_BEGIN records the stop point. A macro rather than a function for the
   reason above: a helper would return, and the recorded stack pointer would be
   pointing into a frame that no longer exists. __builtin_unwind_init is what
   makes the thread's registers part of the scan. */
#define TY_PARK_BEGIN()           \
  do {                            \
    __builtin_unwind_init();      \
    char ty_park_anchor;          \
    park_point(&ty_park_anchor);  \
  } while (0)

/* Take the calling thread out of the set a collection waits for. Called before
   the wait, never after: a thread is stopped from the moment it says so. */
static void stopped_begin(int32_t kind) {
  typlat_mutex_lock(&world_mtx);
  if (ty_self->stop_depth == 0 && ty_self->state == TY_TH_RUNNING) {
    ty_self->state = kind;
    n_stopped++;
  }
  ty_self->stop_depth++;
  /* A collector that is already waiting on the counters has to be woken; one
     that has not asked for anything yet is not waiting, and the counters are
     under this lock, so its first check sees this stop anyway. Waking every
     monitor wait's stop instead costs a broadcast per synchronized block that
     has to wait, which is most of what a contended program does. */
  if (__atomic_load_n(&ty_stw_request, __ATOMIC_ACQUIRE)) typlat_cond_broadcast(&world_cv);
  typlat_mutex_unlock(&world_mtx);
}

/* Put it back, once it may run again. A collection that started while the thread
   was stopped is waited out here, which is exactly what keeps the thread's stack
   still while the collector scans it: the request is cleared only after the
   scan, the trace and the sweep are over. */
static void stopped_end(void) {
  typlat_mutex_lock(&world_mtx);
  if (--ty_self->stop_depth == 0 && ty_self->state != TY_TH_RUNNING &&
      ty_self->state != TY_TH_DONE) {
    while (__atomic_load_n(&ty_stw_request, __ATOMIC_ACQUIRE)) {
      typlat_cond_wait(&world_cv, &world_mtx);
    }
    ty_self->state = TY_TH_RUNNING;
    n_stopped--;
  }
  typlat_mutex_unlock(&world_mtx);
}

void ty_thread_stopped_begin(void) {
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
}

void ty_thread_stopped_end(void) { stopped_end(); }

void ty_safepoint_slow(void) {
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_PARKED);
  /* No wait of its own: the thread has nothing left to do until the collection
     is over, so ending the stop is the whole of it. */
  stopped_end();
}

void ty_gc_stop_world(void) {
  typlat_mutex_lock(&world_mtx);
  /* A collection started from a thread that is itself stopped (it holds the heap
     lock, which counts as stopped) must not count itself among the threads it is
     waiting for. */
  int32_t was_stopped = ty_self->state != TY_TH_RUNNING;
  if (was_stopped) n_stopped--;
  __atomic_store_n(&ty_stw_request, 1, __ATOMIC_RELEASE);
  typlat_cond_broadcast(&world_cv);
  while (n_stopped < n_live - 1) {
    typlat_cond_wait(&world_cv, &world_mtx);
  }
  typlat_mutex_unlock(&world_mtx);
  /* The world stays stopped without this lock being held: every other thread is
     inside a stop point waiting for the request to clear. */
  ty_self->was_stopped_for_gc = was_stopped;
}

void ty_gc_resume_world(void) {
  typlat_mutex_lock(&world_mtx);
  if (ty_self->was_stopped_for_gc) n_stopped++;
  __atomic_store_n(&ty_stw_request, 0, __ATOMIC_RELEASE);
  typlat_cond_broadcast(&world_cv);
  typlat_mutex_unlock(&world_mtx);
}

/* The margin every generated function's stack check leaves for the runtime's
   own unwinding; see "the stack limit" below for what it is for. It is here
   because the virtual thread's stack is sized against it. */
#define TY_STACK_MARGIN (256 * 1024)

/* Defined below, with the rest of the stack-limit machinery; the carrier needs
   it before then. */
static void stack_limit_init(void);

/* ------------------------------------------------------- virtual threads
 *
 * A virtual thread is a continuation with its own stack, multiplexed onto a
 * small pool of carrier threads. It is not an operating system thread: starting
 * ten thousand of them costs ten thousand small stacks, not ten thousand kernel
 * threads, and the ones that are blocked cost nothing at all because they are
 * not on a carrier.
 *
 * ------------------------------------------------------------ continuations
 *
 * POSIX ucontext is the mechanism: getcontext/makecontext build a context on a
 * malloc'd stack, and swapcontext saves one context and restores another. A
 * virtual thread is switched out only at a point where it asked to block, so
 * the context is always a well-defined state of its own stack.
 *
 * A carrier runs one virtual thread at a time. Before it switches in it marks
 * itself stopped for the collector (its own stack is frozen while the virtual
 * thread runs, and it holds no roots), and after the switch it marks itself
 * running again. The virtual thread's own entry in the registry does the
 * ordinary stop-the-world bookkeeping through stopped_begin/stopped_end, so a
 * collection sees it exactly as it sees a platform thread: a RUNNING virtual
 * thread has to reach a safepoint, and a parked one is already stopped with its
 * stack still.
 *
 * The thread-local state a carrier has to change with the running thread is
 * small and named in one place: ty_self (the per-thread state), ty_frames (the
 * precise frame chain), ty_stack_limit (the stack-overflow check), and
 * ty_cur_catch (the exception handler chain). tls_enter/tls_leave below are
 * the only code that touches all four.
 *
 * ------------------------------------------------------------------ parking
 *
 * A blocking call asks ty_cond_wait / ty_wait_fd instead of the platform
 * layer's wait. For a platform thread those are the platform calls they always
 * were. For a virtual thread the call registers what it is waiting for -- a
 * condition variable, or a descriptor and a deadline -- and parks: it marks
 * itself stopped, saves its thread-local state, and switches back to the
 * carrier. The carrier then runs another virtual thread, and the scheduler's
 * poll loop makes the parked one runnable when its condition variable is
 * signalled, its descriptor becomes ready, or its deadline passes.
 *
 * The handshake that makes a wake-up unlosable is three states, not two. A
 * thread that is about to park is PARKING, not PARKED: a wake that arrives in
 * that window sets vwake_pending, and the park returns without switching out.
 * Only after the carrier has regained control (which means swapcontext has
 * saved the context) does the carrier move PARKING to PARKED, or straight to
 * RUNNABLE when a wake arrived meanwhile. Without that middle state a carrier
 * could dequeue a thread whose context had not been saved yet and jump into
 * a stale one.
 *
 * Condition variables need one more thing than a descriptor does: a virtual
 * thread is not asleep on the pthread condition variable, so a signal has to
 * find it. vt_vwq is the list of virtual waiters per condition variable, and
 * ty_cond_signal/ty_cond_broadcast walk it after the platform call. A thread
 * is added to that list while it still holds the caller's mutex, which is the
 * same atomicity pthread_cond_wait provides: the signaller holds the same
 * mutex, so it either sees the waiter or signalled before the wait began --
 * and a signal before the wait began is lost in both designs.
 */

/* The registry functions are defined below this section; the virtual thread
   start is the only caller that needs them from here. */
static void registry_insert(tythread *t, int32_t live);
static void registry_make_live(tythread *t);
static void registry_finish(tythread *t);
static tythread *registry_find(int64_t id);

/* A virtual thread's shadow stack. The runtime pushes one root per native call
   that holds an object across a wait, and those calls are leaves, so this is
   far more than any virtual thread uses -- and small enough that ten thousand
   of them are ten thousand pages, not ten thousand 512 KB blocks. */
#define TY_SHADOW_VIRTUAL 256

/* The usable stack of a virtual thread. It has to leave room above
   TY_STACK_MARGIN (the space the runtime's own unwinding needs below the
   generated check), which is 256 KB, so the smallest honest size is a little
   over twice that. The pages are not touched until the thread runs, so what a
   parked thread costs is its touched pages and its bookkeeping. */
#define TY_VSTACK_BYTES (512 * 1024)

/* The ucontext that goes below each stack, rounded up to the alignment
   makecontext wants for the stack it is given. */
#define TY_VCTX_BYTES ((sizeof(ucontext_t) + 15) & ~(size_t)15)

/* The carrier pool. Java's default is one carrier per processor; this caps it
   so a machine with many cores does not start a hundred threads for a program
   that has four virtual threads ready. TEYRU_VTHREAD_CARRIERS overrides it,
   which is also how the tests pin a single carrier to prove that a parked
   thread really does give its carrier up. */
#define TY_VT_CARRIERS_MAX 64

/* How many runnable virtual threads a carrier takes from the shared queue at
   once. Large enough that the queue lock is not on the per-switch path, small
   enough that a carrier that goes to sleep does not hold work another carrier
   could have run. */
#define TY_VT_BATCH 16

static void tls_enter(tythread *t) {
  ty_self = t;
  ty_frames = t->frames;
  ty_stack_limit = t->stack_limit;
  ty_cur_catch = (tycatch *)t->catch_chain;
}

static void tls_leave(tythread *t) {
  t->frames = ty_frames;
  t->catch_chain = ty_cur_catch;
}

/* ---------------------------------------------------------- scheduler state */

static typlat_mutex vt_mtx = TYPLAT_MUTEX_INITIALIZER;
static tythread *vt_runq_head = NULL, *vt_runq_tail = NULL;
/* Everything parked, for the deadline sweep. It is a list rather than a scan
   of the registry because the sweep runs once per expiry, not once per park. */
static tythread *vt_blocked = NULL;
/* The parked threads waiting on a descriptor, in a list of their own so that
   a carrier's poll set is the size of the descriptor waits and not the size of
   everything asleep. This is what keeps ten thousand sleepers from making
   every poll cost ten thousand: a sleeper is not in this list at all. The
   links are vwprev/vwnext, which a thread waiting on a descriptor does not
   otherwise use (it is not on a condition variable). */
static tythread *vt_fdwait = NULL;
/* The earliest deadline among the parked threads, or 0. Updated when a thread
   parks, recomputed by the sweep. It is what the carriers' poll timeout is
   made from, without walking the blocked list. */
static int64_t vt_next_deadline = 0;
static int vt_started = 0;
/* Carriers asleep waiting for a runnable thread. A runnable thread signals this
   variable's condition variable, which wakes exactly one of them -- the reason
   the run queue is not woken through the pipe the poller sleeps on: a pipe
   readiness wakes every poller, and with eight carriers that is eight futex
   wakeups for every virtual thread started. */
static typlat_cond vt_cv = TYPLAT_COND_INITIALIZER;
static int vt_waiting_carriers = 0;

#if !defined(_WIN32)
static int vt_wake_pipe[2] = {-1, -1};
#endif

/* Wake the poller: a descriptor wait has been added, or a deadline earlier than
   the one it is sleeping on has been. This is the only writer of the pipe, and
   the only thread that reads it is the poller, so a write wakes one thread. The
   write is non-blocking; a full pipe already means the poller will wake. */
static void vt_poll_wakeup(void) {
#if !defined(_WIN32)
  if (vt_wake_pipe[1] >= 0) {
    char b = 1;
    ssize_t ignored = write(vt_wake_pipe[1], &b, 1);
    (void)ignored;
  }
#endif
}

/* Wake one carrier waiting for a runnable thread. Called with vt_mtx held. */
static void vt_signal_locked(int many) {
  if (vt_waiting_carriers <= 0) return;
  if (many) typlat_cond_broadcast(&vt_cv);
  else typlat_cond_signal(&vt_cv);
}

/* ------------------------------------------------- condition variable waiters */

#define TY_VWQ_BUCKETS 64

typedef struct tyvwq {
  typlat_cond *cv;
  tythread *head, *tail;
  struct tyvwq *next;
} tyvwq;

static tyvwq *vt_vwq[TY_VWQ_BUCKETS];
static typlat_mutex vt_vwq_mtx = TYPLAT_MUTEX_INITIALIZER;

static uint32_t vt_vwq_bucket(typlat_cond *cv) {
  uintptr_t h = ((uintptr_t)cv) >> 4;
  h *= 0x9E3779B97F4A7C15ull;
  return (uint32_t)(h >> 32) & (TY_VWQ_BUCKETS - 1);
}

/* The list for one condition variable. The list is created on first use and
   never freed: condition variables in this runtime (a monitor's, a thread's,
   the world's) live as long as the process does. */
static tyvwq *vt_vwq_find(typlat_cond *cv, int create) {
  uint32_t b = vt_vwq_bucket(cv);
  for (tyvwq *q = vt_vwq[b]; q; q = q->next) {
    if (q->cv == cv) return q;
  }
  if (!create) return NULL;
  tyvwq *q = (tyvwq *)calloc(1, sizeof *q);
  if (!q) abort();
  q->cv = cv;
  q->next = vt_vwq[b];
  vt_vwq[b] = q;
  return q;
}

static void vt_vwq_add(typlat_cond *cv, tythread *t) {
  typlat_mutex_lock(&vt_vwq_mtx);
  tyvwq *q = vt_vwq_find(cv, 1);
  t->vwprev = q->tail;
  t->vwnext = NULL;
  if (q->tail) q->tail->vwnext = t;
  else q->head = t;
  q->tail = t;
  t->vwait_cv = cv;
  t->vwait_q = q;
  typlat_mutex_unlock(&vt_vwq_mtx);
}

/* Take a thread off the list it is on, if any. Idempotent: a thread woken by a
   signal has already been taken off by the signaller, and one woken by an
   interrupt has to be taken off here so a later notify does not spend itself
   on a thread that is no longer waiting. */
static void vt_vwq_detach(tythread *t) {
  typlat_mutex_lock(&vt_vwq_mtx);
  tyvwq *q = (tyvwq *)t->vwait_q;
  if (q) {
    if (t->vwprev) t->vwprev->vwnext = t->vwnext;
    else if (q->head == t) q->head = t->vwnext;
    if (t->vwnext) t->vwnext->vwprev = t->vwprev;
    else if (q->tail == t) q->tail = t->vwprev;
    t->vwprev = t->vwnext = NULL;
    t->vwait_cv = NULL;
    t->vwait_q = NULL;
  }
  typlat_mutex_unlock(&vt_vwq_mtx);
}

static tythread *vt_vwq_pop(typlat_cond *cv) {
  tythread *t = NULL;
  typlat_mutex_lock(&vt_vwq_mtx);
  tyvwq *q = vt_vwq_find(cv, 0);
  if (q && q->head) {
    t = q->head;
    q->head = t->vwnext;
    if (q->head) q->head->vwprev = NULL;
    else q->tail = NULL;
    t->vwprev = t->vwnext = NULL;
    t->vwait_cv = NULL;
    t->vwait_q = NULL;
  }
  typlat_mutex_unlock(&vt_vwq_mtx);
  return t;
}

/* ------------------------------------------------------- queues and waking */

/* The run queue and the blocked list share vprev/vnext: a thread is in exactly
   one of them, or in neither (running, or done). Both are guarded by vt_mtx. */
static void vt_runq_push_locked(tythread *t) {
  t->vnext = NULL;
  if (vt_runq_tail) vt_runq_tail->vnext = t;
  else vt_runq_head = t;
  vt_runq_tail = t;
}

static void vt_blocked_link_locked(tythread *t) {
  t->vprev = NULL;
  t->vnext = vt_blocked;
  if (vt_blocked) vt_blocked->vprev = t;
  vt_blocked = t;
}

static void vt_blocked_unlink_locked(tythread *t) {
  if (t->vprev) t->vprev->vnext = t->vnext;
  else if (vt_blocked == t) vt_blocked = t->vnext;
  if (t->vnext) t->vnext->vprev = t->vprev;
  t->vprev = t->vnext = NULL;
}

static void vt_fd_link_locked(tythread *t) {
  t->vwprev = NULL;
  t->vwnext = vt_fdwait;
  if (vt_fdwait) vt_fdwait->vwprev = t;
  vt_fdwait = t;
}

static void vt_fd_unlink_locked(tythread *t) {
  if (t->vwprev) t->vwprev->vwnext = t->vwnext;
  else if (vt_fdwait == t) vt_fdwait = t->vwnext;
  if (t->vwnext) t->vwnext->vwprev = t->vwprev;
  t->vwprev = t->vwnext = NULL;
}

/* Make a parked virtual thread runnable. A thread that is still running (or
   between "about to park" and the carrier having saved it) only records the
   wake: its park must return without switching out, because the thing it was
   about to wait for has already happened. */
static void vt_wake(tythread *t) {
  if (!t->is_virtual) return;
  int queued = 0;
  vt_vwq_detach(t);
  typlat_mutex_lock(&vt_mtx);
  if (t->vstate == TY_VT_PARKED) {
    t->vstate = TY_VT_RUNNABLE;
    vt_blocked_unlink_locked(t);
    if (t->vfd >= 0) vt_fd_unlink_locked(t);
    vt_runq_push_locked(t);
    queued = 1;
  } else {
    t->vwake_pending = 1;
  }
  if (queued) vt_signal_locked(0);
  typlat_mutex_unlock(&vt_mtx);
}

/* Wake every parked thread whose deadline has passed. Called by a carrier
   after each poll; the poll timeout is capped below so an earlier deadline
   that arrived after the poll began is seen within a few milliseconds. */
static int vt_expire_deadlines(void) {
  int64_t now = ty_nanos();
  int queued = 0;
  int64_t next = 0;
  typlat_mutex_lock(&vt_mtx);
  for (tythread *t = vt_blocked; t;) {
    tythread *after = t->vnext;
    if (t->vstate == TY_VT_PARKED) {
      if (t->vdeadline && t->vdeadline <= now) {
        t->vt_timedout = 1;
        t->vstate = TY_VT_RUNNABLE;
        vt_blocked_unlink_locked(t);
        if (t->vfd >= 0) vt_fd_unlink_locked(t);
        vt_runq_push_locked(t);
        queued = 1;
        /* Holding vt_mtx while taking the waiter-list lock is the one nesting
           order this file uses; the other direction never happens, because a
           signaller drops vt_vwq_mtx before it calls vt_wake. */
        vt_vwq_detach(t);
      } else if (t->vdeadline && (next == 0 || t->vdeadline < next)) {
        next = t->vdeadline;
      }
    }
    t = after;
  }
  vt_next_deadline = next;
  if (queued) vt_signal_locked(1);
  typlat_mutex_unlock(&vt_mtx);
  return queued;
}

/* Park the calling virtual thread. fd < 0 waits on a condition variable only;
   otherwise the carrier polls fd for `events` (1 read, 2 write). deadline is a
   monotonic nanosecond instant, or 0 for none. Returns 1 when the deadline
   passed. The caller must not hold a lock that a waker needs. */
static int vt_park(int32_t fd, int32_t events, int64_t deadline) {
  tythread *v = ty_self;
  tythread *c = v->vcarrier;
  int ready;
  v->vt_timedout = 0;
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  tls_leave(v);
  typlat_mutex_lock(&vt_mtx);
  if (v->vwake_pending) {
    v->vwake_pending = 0;
    ready = 1;
  } else {
    v->vstate = TY_VT_PARKING;
    v->vfd = fd;
    v->vevents = events;
    v->vdeadline = deadline;
    vt_blocked_link_locked(v);
    /* A deadline earlier than the one the carriers are asleep on has to wake
       them, or they would sleep through it. A later one needs nothing: they
       recompute after the current one. */
    if (deadline && (vt_next_deadline == 0 || deadline < vt_next_deadline)) {
      vt_next_deadline = deadline;
      ready = -1; /* wake the pollers, but still park */
    } else {
      ready = 0;
    }
  }
  typlat_mutex_unlock(&vt_mtx);
  if (ready < 0) {
    vt_poll_wakeup();
    ready = 0;
  }
  if (!ready) {
#if !defined(_WIN32)
    /* The carrier restores this thread's thread-local state before switching
       back in, so on return ty_self is this thread again. */
    tls_enter(c);
    swapcontext((ucontext_t *)v->vctx, (ucontext_t *)c->vctx);
#else
    abort(); /* unreachable: Windows never creates a virtual thread */
#endif
  }
  stopped_end();
  return v->vt_timedout;
}

/* Give the carrier up. Thread.yield() in a virtual thread, and the only place
   a virtual thread switches out without waiting for anything. */
static void vt_yield(void) {
  tythread *v = ty_self;
  tythread *c = v->vcarrier;
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_PARKED);
  tls_leave(v);
  typlat_mutex_lock(&vt_mtx);
  v->vstate = TY_VT_RUNNABLE;
  vt_runq_push_locked(v);
  vt_signal_locked(0);
  typlat_mutex_unlock(&vt_mtx);
#if !defined(_WIN32)
  tls_enter(c);
  swapcontext((ucontext_t *)v->vctx, (ucontext_t *)c->vctx);
#endif
  stopped_end();
}

/* --------------------------------------------------------- the wait wrappers */

void ty_cond_wait(typlat_cond *c, typlat_mutex *m) {
  if (!ty_self->is_virtual) {
    ty_thread_stopped_begin();
    typlat_cond_wait(c, m);
    ty_thread_stopped_end();
    return;
  }
  tythread *self = ty_self;
  /* Added while `m` is still held: a signaller holds `m` too, so it cannot
     signal between the add and the release. */
  vt_vwq_add(c, self);
  typlat_mutex_unlock(m);
  vt_park(-1, 0, 0);
  typlat_mutex_lock(m);
  vt_vwq_detach(self);
}

int32_t ty_cond_timedwait(typlat_cond *c, typlat_mutex *m, int64_t timeout_ns) {
  if (!ty_self->is_virtual) {
    int r;
    ty_thread_stopped_begin();
    r = typlat_cond_timedwait(c, m, timeout_ns);
    ty_thread_stopped_end();
    return r;
  }
  tythread *self = ty_self;
  int64_t deadline = timeout_ns < 0 ? 0 : ty_nanos() + timeout_ns;
  vt_vwq_add(c, self);
  typlat_mutex_unlock(m);
  int timed_out = vt_park(-1, 0, deadline);
  typlat_mutex_lock(m);
  vt_vwq_detach(self);
  return timed_out;
}

void ty_cond_signal(typlat_cond *c) {
  typlat_cond_signal(c);
  tythread *t = vt_vwq_pop(c);
  if (t) vt_wake(t);
}

void ty_cond_broadcast(typlat_cond *c) {
  typlat_cond_broadcast(c);
  for (;;) {
    tythread *t = vt_vwq_pop(c);
    if (!t) break;
    vt_wake(t);
  }
}

int32_t ty_wait_fd(int32_t fd, int32_t events, int32_t timeout_ms) {
  if (!ty_self->is_virtual) {
    /* events is 1 for readable and 2 for writable (the convention the virtual
       path's poll set uses); the platform call takes a for_write flag. Getting
       this backwards polls a listening socket for writability and then blocks
       in accept, which is exactly the timeout t98_net_basic reported. */
    return typlat_socket_wait(fd, events == 2, timeout_ms);
  }
  int64_t deadline = timeout_ms < 0 ? 0 : ty_nanos() + (int64_t)timeout_ms * 1000000;
  if (vt_park(fd, events, deadline)) return 0;
  return 1;
}

/* --------------------------------------------------------------- the carriers */

#if !defined(_WIN32)

/* ---------------------------------------------------------- the stack arena
 *
 * A stack per virtual thread is a lot of address space and, if each one were
 * its own mmap, two syscalls per thread -- which is most of what starting ten
 * thousand of them costs. Stacks are carved out of large mmap'd chunks
 * instead, and a finished thread's stack goes on a free list for the next one:
 * a program that starts a million short-lived threads makes one mmap per 32
 * threads, not two per thread.
 *
 * There is no per-stack guard page. The generated prologue check is what stops
 * a recursion, with 256 KB of margin below the limit for the runtime's own
 * unwinding, so a generated frame would have to be larger than that margin to
 * reach the end of the stack at all; the platform-thread path's OS guard is a
 * backstop this one does without to keep starting a thread cheap. A fault
 * below a virtual thread's stack is still reported by ty_stack_fault, because
 * that compares against the running thread's own stack_base.
 */
/* Chunks are large so that the mmap count stays small. Transparent huge pages
   are turned off for them: a stack is touched at its top and its ucontext
   once, so a 2 MB page costs more to fault and zero than the 4 KB pages the
   same region would need, and it is never reused densely enough to pay that
   back (measured: 22 ms for ten thousand contexts without huge pages, 193 ms
   with them). */
#define TY_VARENA_CHUNK (64 * 1024 * 1024)

typedef struct vtchunk {
  char *base;
  size_t cap, used;
  struct vtchunk *next;
} vtchunk;

static vtchunk *vt_chunks = NULL;
static char *vt_free_stacks = NULL;
static typlat_mutex vt_stack_mtx = TYPLAT_MUTEX_INITIALIZER;

/* noinline for the same -Wclobbered reason as registry_find: the caller
   reaches getcontext. */
__attribute__((noinline)) static char *vt_stack_alloc(size_t bytes) {
  typlat_mutex_lock(&vt_stack_mtx);
  if (vt_free_stacks) {
    char *p = vt_free_stacks;
    /* The free list's link lives in the stack's own first bytes. */
    memcpy(&vt_free_stacks, p, sizeof p);
    typlat_mutex_unlock(&vt_stack_mtx);
    return p;
  }
  vtchunk *c = vt_chunks;
  if (!c || c->used + bytes > c->cap) {
    size_t cap = TY_VARENA_CHUNK;
    if (bytes > cap) cap = bytes;
    char *mem = (char *)mmap(NULL, cap, PROT_READ | PROT_WRITE,
                             MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
    if (mem == MAP_FAILED) abort();
#if defined(MADV_NOHUGEPAGE)
    (void)madvise(mem, cap, MADV_NOHUGEPAGE);
#endif
    c = (vtchunk *)malloc(sizeof *c);
    if (!c) abort();
    c->base = mem;
    c->cap = cap;
    c->used = 0;
    c->next = vt_chunks;
    vt_chunks = c;
  }
  char *p = c->base + c->used;
  c->used += bytes;
  typlat_mutex_unlock(&vt_stack_mtx);
  return p;
}

static void vt_stack_free(char *p) {
  typlat_mutex_lock(&vt_stack_mtx);
  memcpy(p, &vt_free_stacks, sizeof p);
  vt_free_stacks = p;
  typlat_mutex_unlock(&vt_stack_mtx);
}

/* Release a finished virtual thread's continuation. Called by the carrier
   after the final switch, which is the first moment the stack is not in use. */
static void vt_reap(tythread *v) {
  v->vctx = NULL; /* it is the first bytes of the block vstack names */
  if (v->vstack) {
    vt_stack_free(v->vstack);
    v->vstack = NULL;
  }
}

/* After a virtual thread has switched out, move it from PARKING to PARKED, or
   straight to RUNNABLE when a wake arrived in the window. */
static void vt_after_switch(tythread *v) {
  int wake = 0;
  typlat_mutex_lock(&vt_mtx);
  if (v->vstate == TY_VT_PARKING) {
    if (v->vwake_pending) {
      v->vwake_pending = 0;
      v->vstate = TY_VT_RUNNABLE;
      vt_blocked_unlink_locked(v);
      vt_runq_push_locked(v);
      vt_signal_locked(0);
    } else {
      v->vstate = TY_VT_PARKED;
      /* The poller built its set before this thread parked, so it has to be
         told about a new descriptor. */
      if (v->vfd >= 0) {
        vt_fd_link_locked(v);
        wake = 1;
      }
    }
  }
  typlat_mutex_unlock(&vt_mtx);
  if (wake) vt_poll_wakeup();
}

/* The poller: the one thread that waits for descriptors and deadlines. Every
   worker carrier waits on vt_cv for a runnable thread instead, so a wakeup
   wakes one carrier rather than all of them. The poll set is the wake pipe
   plus one entry per parked virtual thread waiting on a descriptor; the
   timeout is the earliest deadline. Only descriptor waits are in the list, so
   ten thousand sleeping threads still poll a one-entry set. */
static void *vt_poller_main(void *arg) {
  tythread *p = (tythread *)arg;
  ty_self = p;
  stack_limit_init();
  /* The poller is stopped for the collector for its whole life, like every
     carrier: its stack holds no roots. */
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  for (;;) {
    int nfds = 1, cap = 8;
    struct pollfd *pfd = (struct pollfd *)malloc((size_t)cap * sizeof *pfd);
    tythread **who = (tythread **)malloc((size_t)cap * sizeof *who);
    if (!pfd || !who) abort();
    pfd[0].fd = vt_wake_pipe[0];
    pfd[0].events = POLLIN;
    pfd[0].revents = 0;
    who[0] = NULL;
    typlat_mutex_lock(&vt_mtx);
    for (tythread *t = vt_fdwait; t; t = t->vwnext) {
      if (nfds == cap) {
        cap *= 2;
        struct pollfd *np = (struct pollfd *)realloc(pfd, (size_t)cap * sizeof *pfd);
        tythread **nw = (tythread **)realloc(who, (size_t)cap * sizeof *who);
        if (!np || !nw) abort();
        pfd = np;
        who = nw;
      }
      pfd[nfds].fd = t->vfd;
      pfd[nfds].events = t->vevents == 2 ? POLLOUT : POLLIN;
      pfd[nfds].revents = 0;
      who[nfds] = t;
      nfds++;
    }
    int64_t next = vt_next_deadline;
    typlat_mutex_unlock(&vt_mtx);
    int timeout_ms = -1;
    if (next) {
      int64_t d = next - ty_nanos();
      if (d < 0) d = 0;
      timeout_ms = (int)((d + 999999) / 1000000);
    }
    int r = poll(pfd, (nfds_t)nfds, timeout_ms);
    if (r < 0) {
      /* poll itself failed -- a descriptor closed under us is the usual cause.
         Wake everybody waiting on one: their own call then reports the error,
         where sleeping on a descriptor nobody will ever signal is a hang. */
      for (int i = 1; i < nfds; i++) vt_wake(who[i]);
    } else if (r > 0) {
      if (pfd[0].revents & POLLIN) {
        char buf[256];
        while (read(vt_wake_pipe[0], buf, sizeof buf) > 0) {
        }
      }
      for (int i = 1; i < nfds; i++) {
        if (pfd[i].revents) vt_wake(who[i]);
      }
    }
    free(pfd);
    free(who);
    vt_expire_deadlines();
  }
  return NULL;
}

/* A worker carrier: take a runnable virtual thread and run it until it blocks
   or finishes, then take the next. The carrier's own stack is frozen for the
   whole time a virtual thread runs on it, which is why it counts as stopped
   for the collector for that whole time. */
static void *vt_carrier_main(void *arg) {
  tythread *c = (tythread *)arg;
  ty_self = c;
  stack_limit_init();
  /* A carrier is stopped for the collector for its whole life, and says so
     once. Its own stack holds no roots -- the scheduler deals in malloc'd
     bookkeeping, never in heap objects -- so it does not matter that the
     collector may read it while the carrier is running; what matters is that
     the virtual thread running on it does the ordinary transition, and that
     nothing here takes the world lock twice per switch. */
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  for (;;) {
    tythread *v = c->vlocal_head;
    if (v) {
      c->vlocal_head = v->vnext;
      if (!c->vlocal_head) c->vlocal_tail = NULL;
      v->vnext = NULL;
    } else {
      typlat_mutex_lock(&vt_mtx);
      while (!vt_runq_head) {
        vt_waiting_carriers++;
        typlat_cond_wait(&vt_cv, &vt_mtx);
        vt_waiting_carriers--;
      }
      /* Take a batch, not one thread: the shared queue's lock is the one
         point every carrier and every start meets at, and taking sixteen at a
         time divides its traffic by sixteen. */
      v = vt_runq_head;
      tythread *last = v;
      int n = 1;
      while (n < TY_VT_BATCH && last->vnext) {
        last = last->vnext;
        n++;
      }
      vt_runq_head = last->vnext;
      if (!vt_runq_head) vt_runq_tail = NULL;
      last->vnext = NULL;
      c->vlocal_head = v->vnext;
      c->vlocal_tail = v->vnext ? last : NULL;
      v->vnext = NULL;
      typlat_mutex_unlock(&vt_mtx);
    }
    v->vstate = TY_VT_RUNNING;
    v->vcarrier = c;
    tls_leave(c);
    tls_enter(v);
    swapcontext((ucontext_t *)c->vctx, (ucontext_t *)v->vctx);
    /* Back on this carrier. ty_self is still v at this instant; the switch is
       the first thing undone. */
    tls_enter(c);
    if (v->finished) vt_reap(v);
    else vt_after_switch(v);
  }
  return NULL;
}

static int vt_carrier_count(void) {
  const char *e = getenv("TEYRU_VTHREAD_CARRIERS");
  int n = (e && *e) ? atoi(e) : 0;
  if (n <= 0) {
    long c = sysconf(_SC_NPROCESSORS_ONLN);
    n = c > 0 ? (int)c : 1;
    if (n > 8) n = 8;
  }
  if (n > TY_VT_CARRIERS_MAX) n = TY_VT_CARRIERS_MAX;
  return n < 1 ? 1 : n;
}

static void vt_start_scheduler(void) {
  /* The common case, after the first virtual thread: one load instead of a
     lock the carrier is also taking to dequeue. */
  if (__atomic_load_n(&vt_started, __ATOMIC_ACQUIRE)) return;
  typlat_mutex_lock(&vt_mtx);
  if (vt_started) {
    typlat_mutex_unlock(&vt_mtx);
    return;
  }
  __atomic_store_n(&vt_started, 1, __ATOMIC_RELEASE);
  if (pipe(vt_wake_pipe) != 0) abort();
  for (int i = 0; i < 2; i++) {
    (void)fcntl(vt_wake_pipe[i], F_SETFL, O_NONBLOCK);
    (void)fcntl(vt_wake_pipe[i], F_SETFD, FD_CLOEXEC);
  }
  int n = vt_carrier_count();
  /* One poller, then the workers. The poller is a carrier like the others --
     it is counted as stopped and has a context -- but it never runs a virtual
     thread; it is the thread that can sleep in poll() while the workers sleep
     on the run-queue condition variable. */
  for (int i = 0; i <= n; i++) {
    tythread *c = (tythread *)calloc(1, sizeof(tythread));
    if (!c) abort();
    typlat_mutex_init(&c->mtx);
    typlat_cond_init(&c->cv);
    /* Negative ids: a carrier has no Thread object, and every id a program
       can name is positive. registry_find by a program's id never finds one. */
    c->id = -(int64_t)(i + 1);
    c->daemon = 1;
    c->started = 1;
    c->state = TY_TH_RUNNING;
    c->vstate = TY_VT_RUNNING;
    c->vctx = calloc(1, sizeof(ucontext_t));
    if (!c->vctx) abort();
    registry_insert(c, 1);
    void *(*fn)(void *) = i == 0 ? vt_poller_main : vt_carrier_main;
    if (typlat_thread_begin(&c->tid, fn, c) != 0) abort();
  }
  typlat_mutex_unlock(&vt_mtx);
}

/* ---------------------------------------------------------- starting one */

static void vthread_entry(void) {
  tythread *t = ty_self;
  /* The thread was registered stopped: it becomes running here, once a
     collection that was already in progress has finished. Its StackOverflowError
     was reserved by the thread that started it -- reserving it here would give
     every virtual thread a slab of its own just to hold one small object. */
  stopped_end();
  ty_safepoint();

  tycatch frame;
  frame.prev = ty_cur_catch;
  frame.frames = ty_frames;
  frame.ex = NULL;
  ty_cur_catch = &frame;
  if (setjmp(frame.buf) == 0) {
    ((void (*)(void *))ty_itab(t->obj, t->vsel))(t->obj);
  } else {
    ty_cur_catch = frame.prev;
    ty_uncaught_thread(frame.ex, t);
  }
  ty_cur_catch = frame.prev;

  /* Finished, in the order a platform thread's start uses: give the slab back,
     wait out any collection, then become DONE, then tell a joiner. */
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  ty_heap_detach();
  stopped_end();
  registry_finish(t);

  if (t->roots) {
    if (t->sp > 0) t->sp--;
    free(t->roots);
    t->roots = NULL;
    t->sp = 0;
  }
  typlat_mutex_lock(&t->mtx);
  t->finished = 1;
  ty_cond_broadcast(&t->cv);
  typlat_mutex_unlock(&t->mtx);

  /* Never returns: the carrier releases the stack once it has control again. */
  tythread *c = t->vcarrier;
  tls_leave(t);
  tls_enter(c);
  swapcontext((ucontext_t *)t->vctx, (ucontext_t *)c->vctx);
  abort();
}

#endif /* !_WIN32 */

int64_t ty_thread_start_virtual0(void *obj, int64_t id, tystr *name, int32_t sel) {
#if defined(_WIN32)
  /* No fibers in this runtime. Refusing by name is the only answer that does
     not lie about what the thread is: running the work on a platform thread
     would make isVirtual() true of something that is not. */
  ty_throw(ty_make_ex(TY_UNSUP, "virtual threads are not supported on this platform"));
  return 0;
#else
  vt_start_scheduler();
  tythread *t = registry_find(id);
  int32_t reused = t && !t->started;
  if (!reused) {
    t = (tythread *)calloc(1, sizeof(tythread));
    if (!t) abort();
    typlat_mutex_init(&t->mtx);
    typlat_cond_init(&t->cv);
    t->id = id;
  }
  t->obj = obj;
  t->name = name;
  t->daemon = 0;
  t->is_virtual = 1;
  t->vsel = sel;
  t->finished = 0;
  t->started = 1;
  /* Born stopped: it has not run, so a collection must not wait for it, and it
     is counted as stopped from the instant it is live. stop_depth is 1 so that
     the entry's stopped_end is the matching half. */
  t->state = TY_TH_BLOCKED;
  t->stop_depth = 1;
  t->vstate = TY_VT_RUNNABLE;
  t->vt_timedout = 0;
  t->vwake_pending = 0;
  t->vfd = -1;
  t->vevents = 0;
  t->vdeadline = 0;

  /* The stack and its ucontext, one block from the arena; see vt_stack_alloc
     for the guard-page tradeoff. The ucontext sits below the stack, where the
     stack's own frames never reach (the check stops 256 KB above stack_base).
     getcontext is needed even though makecontext overwrites the machine
     context: glibc's makecontext builds on fields getcontext initialises, and
     a context that was only zeroed crashes on the first switch. */
  size_t bytes = TY_VSTACK_BYTES;
  char *mem = vt_stack_alloc(TY_VCTX_BYTES + bytes);
  t->vstack = mem;
  t->vstack_bytes = TY_VCTX_BYTES + bytes;
  t->stack_base = mem + TY_VCTX_BYTES;
  t->stack_top = t->stack_base + bytes;
  t->stack_limit = t->stack_base + TY_STACK_MARGIN;
  ucontext_t *uc = (ucontext_t *)mem;
  if (getcontext(uc) != 0) abort();
  uc->uc_stack.ss_sp = t->stack_base;
  uc->uc_stack.ss_size = bytes;
  uc->uc_link = NULL;
  t->vctx = uc;
  makecontext(uc, vthread_entry, 0);

  /* The shadow stack, with the Thread object already on it: the thread has to
     keep the object it is running alive from the moment it exists, not from
     the moment it first runs. */
  t->roots = (void **)malloc(TY_SHADOW_VIRTUAL * sizeof(void *));
  if (!t->roots) abort();
  t->sp = 0;
  t->roots[t->sp++] = obj;

  /* Live and stopped in one step under the world lock: a collection that
     starts between the two would otherwise wait for a thread that is not on a
     carrier and never stops. */
  typlat_mutex_lock(&world_mtx);
  if (reused) {
    n_live++;
    n_stopped++;
  } else {
    t->next = __atomic_load_n(&threads, __ATOMIC_RELAXED);
    __atomic_store_n(&threads, t, __ATOMIC_RELEASE);
    n_live++;
    n_stopped++;
  }
  typlat_mutex_unlock(&world_mtx);

  /* The thread's preallocated StackOverflowError, made here rather than on the
     thread's own first instruction: the throw path may not allocate, and a
     virtual thread that only sleeps would otherwise take a 256 KB slab out of
     the heap just to hold this one object. It is a root from now on because the
     registry entry names it (tyrt.c's gc_mark_roots marks t->soe). */
  if (TY_SOE && !t->soe) {
    tyobj *o = (tyobj *)ty_alloc(sizeof(tyobj) + 2 * sizeof(void *));
    o->cls = TY_SOE;
    ((void **)((char *)o + sizeof(tyobj)))[0] = NULL; /* the message */
    ((void **)((char *)o + sizeof(tyobj)))[1] = NULL; /* the cause */
    t->soe = o;
  }

  typlat_mutex_lock(&vt_mtx);
  vt_runq_push_locked(t);
  vt_signal_locked(0);
  typlat_mutex_unlock(&vt_mtx);
  return t->id;
#endif
}

/* --------------------------------------------------------------- the registry */

tythread *ty_thread_list(void) {
  return __atomic_load_n(&threads, __ATOMIC_ACQUIRE);
}

/* Add a fully built entry to the registry. The entry is complete before it is
   published, and readers walk the list with an acquire load, so the list can be
   walked without the lock -- which is what the collector does.

   `live` says whether the entry belongs to a running thread. A Thread object
   that has not started has an entry too (see ty_thread_next_id), because
   interrupt status lives on the object and is readable before start(); such an
   entry is in the list -- interrupt() and isInterrupted() find it there -- but
   is not counted as a thread the process has to wait for. */
static void registry_insert(tythread *t, int32_t live) {
  typlat_mutex_lock(&world_mtx);
  t->next = __atomic_load_n(&threads, __ATOMIC_RELAXED);
  __atomic_store_n(&threads, t, __ATOMIC_RELEASE);
  if (live) {
    n_live++;
    if (t->daemon) n_daemon++;
  }
  typlat_mutex_unlock(&world_mtx);
}

/* An entry that was registered when its Thread object was built becomes live
   when start() runs. It is already in the list, so this only counts it. */
static void registry_make_live(tythread *t) {
  typlat_mutex_lock(&world_mtx);
  n_live++;
  if (t->daemon) n_daemon++;
  typlat_mutex_unlock(&world_mtx);
}

/* The thread is finished: it is no longer running, no longer stopped, and no
   longer of interest to a collection. Announced on world_cv because joining is
   waiting on n_live. */
static void registry_finish(tythread *t) {
  typlat_mutex_lock(&world_mtx);
  if (t->state != TY_TH_RUNNING && t->state != TY_TH_DONE) n_stopped--;
  t->state = TY_TH_DONE;
  n_live--;
  if (t->daemon) n_daemon--;
  typlat_cond_broadcast(&world_cv);
  typlat_mutex_unlock(&world_mtx);
}

/* noinline only because GCC's -Wclobbered misattributes this loop's induction
   variable to the caller it inlines it into (a caller that reaches
   getcontext), and the runtime is built with -Wall -Wextra. It is not a hot
   path: interrupt, join and isAlive call it once per call. */
__attribute__((noinline)) static tythread *registry_find(int64_t id) {
  tythread *t = ty_thread_list();
  while (t) {
    if (t->id == id) return t;
    t = t->next;
  }
  return NULL;
}

/* ------------------------------------------------------------- the stack limit */

/* How much of the stack a generated function may still be past ty_stack_limit
   without the runtime being in trouble. The check that stops a program runs at
   function entry, so between two checks a frame of any size can appear; and
   what has to fit below the limit is everything the runtime does after the
   check answers: the throw, the longjmp, and -- when nothing catches it -- the
   uncaught handler's toString of the error and the write of the line it makes,
   all of which are C frames the check knows nothing about. 256 KB is far more
   than those need, and it is small next to the 8 MB a thread is given by
   default, so a program that recurses legitimately still gets nearly all of its
   stack.

   The alternative is a margin tuned down until an uncaught overflow crashes,
   which measures the one path that must not crash.

   The value itself is defined near the top of the file, because the virtual
   thread's stack has to be sized against it. */

/* How far below the lowest address of a thread's stack a fault may be and still
   be that stack running off its end. The address a stack that ran out faults at
   is in the guard region the operating system keeps below the stack -- one page
   on Linux, and measured there (glibc, x86-64, both compilers): a recursion
   whose frame was 64 KB and one whose frame was 4 MB both faulted within 17 KB
   below the lowest address pthread_getattr_np reports, which is the page the
   kernel refused to map plus what the compiler's own stack probes touched on
   the way. 64 KB is that with room to spare, and it is small enough that an
   unrelated wild pointer is not reported as a stack overflow. */
#define TY_STACK_GUARD_REACH (64 * 1024)

/* Read the calling thread's stack bounds, derive the limit its frames are
   checked against, and give it the alternate signal stack the backstop's
   handler runs on. Called once per thread, before the thread runs any generated
   code. */
static void stack_limit_init(void) {
  char *low = NULL, *high = NULL;
  typlat_thread_stack_bounds(&low, &high);
  ty_self->stack_base = low;
  ty_self->stack_top = high;
  /* A thread whose bounds cannot be read has no limit to check against, and the
     collector cannot scan it either: it is the same failure, and the only
     answer to it is to run without the check rather than to guess an address. */
  ty_stack_limit = low ? low + TY_STACK_MARGIN : NULL;
  /* Published in the thread's state as well: a carrier runs many virtual
     threads, so the limit has to travel with the thread the same way the frame
     chain does. */
  ty_self->stack_limit = ty_stack_limit;
  typlat_fault_altstack_install();
}

int ty_stack_fault(void *addr) {
  char *base = ty_self ? ty_self->stack_base : NULL;
  if (!addr || !base) return 0;
  char *a = (char *)addr;
  if (a >= base || (uintptr_t)(base - a) > TY_STACK_GUARD_REACH) return 0;
  /* write and not fprintf: this runs in a signal handler, on the alternate
     stack, and a handle on stdio it cannot take would turn a report into a
     deadlock. It is a constant string, so there is nothing to format. */
  static const char msg[] = "stack overflow in native code\n";
  ssize_t ignored = write(2, msg, sizeof msg - 1);
  (void)ignored;
  abort();
}

void ty_thread_init(void) {
  main_thread.tid = typlat_thread_self();
  /* The main thread's entry is static, so its mutex and condition variable are
     not initialised by ty_thread_spawn the way a spawned thread's are. Sleep,
     join and interrupt use them for the main thread too. */
  typlat_mutex_init(&main_thread.mtx);
  typlat_cond_init(&main_thread.cv);
  /* The main thread's stack bounds are what ty_gc_init used to read for the one
     thread there was; they are now part of the thread's state, because a
     collection scans a different stack for every thread. */
  stack_limit_init();
  registry_insert(&main_thread, 1);
}

/* ------------------------------------------------------------------ threads */

/* What typlat_thread_begin is handed: the thread's registry entry, and the
   function the caller wanted run on it. */
typedef struct {
  tythread *t;
  void *(*fn)(void *);
  void *arg;
} spawn_packet;

/* The body of a spawned thread. typlat_thread_begin cannot be given the same
   function as the documented entry point, because the entry point is handed the
   caller's argument and this one has to be handed the runtime's bookkeeping: the
   packet is freed here, so that nothing the new thread runs touches memory the
   spawner owns. */
static void *thread_trampoline(void *p) {
  spawn_packet k = *(spawn_packet *)p;
  free(p);
  ty_self = k.t;
  return ty_thread_start(k.fn, k.arg);
}

void *ty_thread_start(void *(*fn)(void *), void *arg) {
  tythread *t = ty_self;
  /* The new thread's shadow stack. The runtime pushes a handful of roots, so
     this is small next to the C stack it sits beside, and it is freed at the end
     of the thread. */
  t->roots = (void **)malloc(TY_SHADOW_THREAD * sizeof(void *));
  if (!t->roots) abort();
  t->sp = 0;
  t->tid = typlat_thread_self();
  t->state = TY_TH_RUNNING;
  t->stop_depth = 0;
  t->park_sp = NULL;
  if (t->obj) {
    /* The Teyru Thread object the program made stays a root for as long as the
       thread can reach its own run(): the program may have dropped its last
       reference to it, and a collector that frees it would free the very object
       this thread is about to call. */
    TY_ROOT_PUSH(t->obj);
  }
  /* Every thread starts with the same two calls: read its own stack bounds, so
     that the collector can scan the whole of it, and stop once, so that a
     collection already in progress does not start tracing while this thread is
     still setting itself up. */
  stack_limit_init();
  ty_safepoint();
  /* The thread's preallocated StackOverflowError, after the stop: making one
     allocates, and this thread must be out of the way of a collection that is
     already running before it touches the heap. */
  ty_stack_overflow_reserve();

  /* An exception that leaves run() is this thread's business, not the
     process's: Java prints the line and ends the thread, and the rest of the
     program carries on. The frame is the same one generated try/catch uses. */
  tycatch frame;
  frame.prev = ty_cur_catch;
  frame.frames = ty_frames;
  frame.ex = NULL;
  ty_cur_catch = &frame;
  if (setjmp(frame.buf) == 0) {
    fn(arg);
  } else {
    ty_cur_catch = frame.prev;
    ty_uncaught_thread(frame.ex, t);
  }
  ty_cur_catch = frame.prev;

  /* Finished. The order matters: the slab goes back to the shared heap, any
     collection that is walking this thread is waited out, and only then does the
     thread become DONE. It is DONE before the stack is unmapped -- a thread that
     finishes returns from here and its stack is released -- which is what keeps
     a collection from scanning a stack that is going away. */
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  ty_heap_detach();
  stopped_end();
  registry_finish(t);

  if (t->obj) TY_ROOT_POP();
  free(t->roots);
  t->roots = NULL;
  /* Ring the joiner without holding the world lock: join() wakes on this, and
     the entry it wakes on is not part of the world protocol. */
  typlat_mutex_lock(&t->mtx);
  t->finished = 1;
  /* A joiner may be a virtual thread, which is parked rather than asleep on
     this condition variable. */
  ty_cond_broadcast(&t->cv);
  typlat_mutex_unlock(&t->mtx);
  return NULL;
}

int64_t ty_thread_spawn(void *(*fn)(void *), void *arg, void *obj, int64_t id, tystr *name,
                        int32_t daemon) {
  spawn_packet *k = (spawn_packet *)malloc(sizeof(spawn_packet));
  if (!k) abort();
  /* The entry was made when the Thread object was built. Reusing it keeps the
     interrupt status that may have been set before start() -- which is what
     java.lang.Thread does -- and avoids a second entry for one id. */
  tythread *t = registry_find(id);
  int32_t reused = t && !t->started;
  if (!reused) {
    t = (tythread *)calloc(1, sizeof(tythread));
    if (!t) abort();
    typlat_mutex_init(&t->mtx);
    typlat_cond_init(&t->cv);
    t->id = id;
  }
  t->obj = obj;
  t->name = name;
  t->daemon = daemon;
  /* Registered before the thread runs, so that join() called the instant after
     start() finds something to wait for. The thread's own fields -- its stack
     bounds, its shadow stack, where it parked -- are filled in by the thread
     itself, and are read by the collector only after the thread has stopped,
     which is after it filled them in. */
  t->state = TY_TH_RUNNING;
  t->finished = 0;
  t->started = 1;
  if (reused) {
    /* Already in the list: this is the entry ty_thread_next_id made. */
    registry_make_live(t);
  } else {
    registry_insert(t, 1);
  }

  k->t = t;
  k->fn = fn;
  k->arg = arg;
  /* A detached thread releases its own stack when it returns: nothing in the
     language waits for a thread to leave the operating system, and one that is
     never joined must not keep its stack until the program ends. */
  int err = typlat_thread_begin(&t->tid, thread_trampoline, k);
  if (err != 0) {
    /* A thread that cannot be created is a failure the program has to see: a
       Thread object whose start() silently did nothing would leave isAlive()
       answering false with no reason anywhere. The entry stays in the registry
       as a finished one, so that join() on it returns instead of waiting for a
       thread that was never born. */
    registry_finish(t);
    free(k);
    char msg[128];
    snprintf(msg, sizeof msg, "cannot create a thread: %s", strerror(err));
    ty_throw(ty_illegal_state(msg));
  }
  return t->id;
}

int64_t ty_thread_next_id(void) {
  int64_t id;
  typlat_mutex_lock(&world_mtx);
  id = ++thread_seq;
  typlat_mutex_unlock(&world_mtx);
  /* The entry exists from construction, not from start(): java.lang.Thread's
     interrupt status belongs to the Thread object, and a program may interrupt
     one that has not started and read the status back. It is registered as not
     live, so it neither keeps the process alive nor is scanned; start() makes
     it live (ty_thread_spawn). */
  tythread *t = (tythread *)calloc(1, sizeof(tythread));
  if (!t) abort();
  typlat_mutex_init(&t->mtx);
  typlat_cond_init(&t->cv);
  t->id = id;
  t->state = TY_TH_DONE;
  t->finished = 1;
  registry_insert(t, 0);
  return id;
}

int32_t ty_thread_join(int64_t id) {
  tythread *t = registry_find(id);
  if (!t) return 0; /* never started: java.lang.Thread.join returns at once */
  tythread *self = ty_self;
  int32_t intr = 0;
  typlat_mutex_lock(&t->mtx);
  /* Published so interrupt() can reach the condition variable this wait is
     asleep on: the target's, which is not the joiner's own. A self-join has
     nothing to publish -- the target's variable is already the one this thread
     waits on -- and publishing it would take this thread's mutex twice. */
  if (t != self) __atomic_store_n(&self->join_target, t, __ATOMIC_SEQ_CST);
  int32_t waited = 0;
  while (!t->finished) {
    if (__atomic_load_n(&self->interrupted, __ATOMIC_SEQ_CST)) {
      intr = 1;
      break;
    }
    waited = 1;
    /* The wait itself is the stop point (ty_cond_wait brackets it), and for a
       virtual thread it parks rather than blocking the carrier. */
    ty_cond_wait(&t->cv, &t->mtx);
  }
  if (t != self) __atomic_store_n(&self->join_target, NULL, __ATOMIC_SEQ_CST);
  /* An interrupt that arrived as the target finished is still an interrupt:
     java.lang.Thread.join throws when the status is set by the time a wait it
     actually entered ends. A thread that was already finished when join() was
     called never waited at all, and JDK's join leaves the status alone then --
     it is a wait on the target object that throws, and there was none. */
  if (waited && !intr && __atomic_load_n(&self->interrupted, __ATOMIC_SEQ_CST)) intr = 1;
  if (intr) __atomic_store_n(&self->interrupted, 0, __ATOMIC_SEQ_CST);
  typlat_mutex_unlock(&t->mtx);
  return intr;
}

int32_t ty_thread_alive(int64_t id) {
  tythread *t = registry_find(id);
  return t && !t->finished;
}

/* A forward declaration: the monitor struct is defined below, with the rest of
   the monitor machinery, and this is the only thread function that has to look
   inside it. */
static void interrupt_wake(tythread *t);

void ty_thread_interrupt(int64_t id) {
  tythread *t = registry_find(id);
  if (!t) return;
  /* The status is a property of the Thread object, so it is set even when the
     thread has not started or has finished -- java.lang.Thread does the same,
     and isInterrupted() answers it. Only a live thread has something to wake. */
  __atomic_store_n(&t->interrupted, 1, __ATOMIC_SEQ_CST);
  typlat_mutex_lock(&t->mtx);
  int32_t done = t->finished;
  typlat_mutex_unlock(&t->mtx);
  /* A thread that interrupts itself is not inside a blocking call that could
     be woken -- it is running this -- and waking it would mean taking its own
     mutex from under itself. */
  if (!done && t != ty_self) interrupt_wake(t);
}

int32_t ty_thread_is_interrupted(int64_t id) {
  tythread *t = registry_find(id);
  if (!t) return 0;
  return __atomic_load_n(&t->interrupted, __ATOMIC_SEQ_CST);
}

int32_t ty_thread_clear_interrupted(void) {
  return __atomic_exchange_n(&ty_self->interrupted, 0, __ATOMIC_SEQ_CST);
}

int32_t ty_thread_current_daemon(void) { return ty_self->daemon; }

int32_t ty_thread_sleep_ms(int64_t millis) {
  if (millis <= 0) {
    /* Java's own two cases: a negative duration is an IllegalArgumentException,
       and sleep(0) is a yield rather than a wait. */
    if (millis < 0) ty_throw(ty_illarg("timeout value is negative"));
    ty_thread_yield();
    /* sleep(0) is a yield, but it still notices an interrupt that is already
       set: the JDK throws InterruptedException for sleep(0) on an interrupted
       thread, and clears the status. */
    return __atomic_exchange_n(&ty_self->interrupted, 0, __ATOMIC_SEQ_CST);
  }
  tythread *self = ty_self;
  int32_t intr = 0;
  /* The wait is on the thread's own condition variable rather than in the
     platform layer's sleep, because interrupt() has to be able to end it: a
     signal could do that too, but a predicate and a broadcast is the same
     mechanism join() and Object.wait already use, and it needs no handler. The
     deadline is recomputed every time round so that a wake-up that was not an
     interrupt (a collection's broadcast, or the operating system) sleeps for
     the part of the duration that is left rather than starting it again. */
  typlat_mutex_lock(&self->mtx);
  int64_t ns = millis > INT64_MAX / 1000000 ? INT64_MAX : millis * 1000000;
  int64_t now = ty_nanos();
  int64_t deadline = ns > INT64_MAX - now ? INT64_MAX : now + ns;
  for (;;) {
    if (__atomic_load_n(&self->interrupted, __ATOMIC_SEQ_CST)) {
      intr = 1;
      break;
    }
    int64_t left = deadline - ty_nanos();
    if (left <= 0) break;
    /* For a platform thread this is the same pthread wait it always was, with
       the stop point bracketing it; for a virtual thread it parks, and the
       carrier runs somebody else until the deadline. */
    ty_cond_timedwait(&self->cv, &self->mtx, left);
  }
  if (intr) __atomic_store_n(&self->interrupted, 0, __ATOMIC_SEQ_CST);
  typlat_mutex_unlock(&self->mtx);
  return intr;
}

int64_t ty_thread_current_id(void) { return ty_self->id; }

void ty_thread_yield(void) {
  /* A yield is a good place to notice a collection, and it costs a load. */
  ty_safepoint();
#if !defined(_WIN32)
  if (ty_self->is_virtual) {
    /* Give the carrier to another virtual thread: a virtual thread that never
       blocks would otherwise keep it until it finished. */
    vt_yield();
    return;
  }
#endif
  typlat_yield();
}

void *ty_thread_current_obj(void) { return ty_self->obj; }

void ty_thread_bind(void *obj) { ty_self->obj = obj; }

void ty_thread_join_all(void) {
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  typlat_mutex_lock(&world_mtx);
  /* Every non-daemon thread but this one. A daemon that is still running when
     the last non-daemon thread ends does not keep the process alive -- that is
     what a daemon is for -- and a thread that is started while this waits is
     counted when it is registered, so the condition covers it too. */
  while (n_live - n_daemon > 1) typlat_cond_wait(&world_cv, &world_mtx);
  typlat_mutex_unlock(&world_mtx);
  stopped_end();
}

/* ------------------------------------------------------------------- running

 * A Teyru thread runs one method: the run() of its Thread object. The runtime
 * cannot call it by name -- it does not know the class the compiler generated --
 * so the prelude hands over the object and the interface selector of
 * Runnable.run, and the call is the same interface dispatch the generated code
 * makes. */

typedef struct {
  void *obj;
  int32_t sel;
} body_args;

static void *thread_body(void *p) {
  body_args b = *(body_args *)p;
  free(p);
  ((void (*)(void *))ty_itab(b.obj, b.sel))(b.obj);
  return NULL;
}

int64_t ty_thread_start0(void *obj, int64_t id, tystr *name, int32_t daemon, int32_t sel) {
  body_args *b = (body_args *)malloc(sizeof(body_args));
  if (!b) abort();
  b->obj = obj;
  b->sel = sel;
  return ty_thread_spawn(thread_body, b, obj, id, name, daemon);
}

/* ----------------------------------------------------------------- monitors */

/* Buckets, and the entries in them. A bucket count that is a power of two keeps
   the hash to a shift and a mask. */
#define TY_MON_BUCKETS 64

typedef struct tymon {
  void *key; /* the object, never NULL for a live entry */
  typlat_mutex mtx;
  /* Two condition variables, because the entry has two different sets of
     sleepers and one variable cannot tell them apart. cv carries "the monitor
     is free": everybody waiting to enter it, and everybody that gave it up in
     wait() and is waiting to take it back. wcv carries Object.wait's own
     notification, and only the threads inside wait() ever sleep on it.

     One variable for both was the second bug here. notify() signals one thread,
     and with a single variable the one it signalled could be a thread that was
     only waiting to enter the monitor: that thread would look at its own
     condition, find the monitor still held, and go back to sleep, while the
     thread the notification was for was never woken -- and a notification is
     not repeated, so a program that hands a value over with notify() and then
     waits with no timeout never ran again. */
  typlat_cond cv;
  typlat_cond wcv;
  tythread *owner;
  int32_t owned;
  int32_t count;   /* recursion depth */
  int32_t waiters; /* threads inside wait() */
  /* How many notifications this monitor has raised. A waiter takes this
     counter before it gives the monitor up and returns when it has moved, which
     is what makes a notification unlosable: notify() runs under the mutex, so
     every thread that registered itself as waiting before it did has this
     counter at its old value and cannot miss the change, and a thread that
     enters wait() afterwards takes the new value and sleeps for the next one.
     A count of unclaimed notifications -- which is what this was -- cannot
     promise that: a notification raised for one waiter can be claimed by
     another that arrived later, and notifyAll() writing the count of waiters
     over it discards any that were still unclaimed. */
  int64_t notified;
  struct tymon *next;
} tymon;

static tymon *mon_buckets[TY_MON_BUCKETS];

static uint32_t mon_bucket(void *obj) {
  /* Objects are aligned, so the low bits of the address say nothing; mix what is
     left. A plain mask on the shifted address would put every object of a
     thousand-object array in one bucket, because they are a thousand pointers
     apart. */
  uintptr_t h = ((uintptr_t)obj) >> 4;
  h *= 0x9E3779B97F4A7C15ull;
  return (uint32_t)(h >> 32) & (TY_MON_BUCKETS - 1);
}

static tymon *mon_make(void *obj) {
  tymon *m = (tymon *)calloc(1, sizeof(tymon));
  if (!m) abort();
  typlat_mutex_init(&m->mtx);
  /* Java's wait has a relative timeout, and a relative timeout needs a clock
     that does not jump. Which clock these two variables are built on is the
     platform layer's answer, and the layer is also what converts the duration
     below into a deadline on it -- on Windows that is not the monotonic clock
     at all. */
  typlat_cond_init(&m->cv);
  typlat_cond_init(&m->wcv);
  m->key = obj;
  return m;
}

/* The monitor of an object, created on first use. The lists are append-only --
   entries are never freed -- so this needs no lock: a reader walks a list that
   cannot change under it.

   One object has exactly one monitor, and that is the invariant this function
   has to keep, because every other function here works out of the entry it
   finds and the entry it finds is the whole of the monitor's state. The two
   reads of the bucket head below are therefore one read: the head a caller
   walks to look for the object is the head its compare-and-swap has to still
   find unchanged. Reading the head a second time for the swap -- which is what
   this did, and the bug that made the monitors lose their state -- let two
   threads that both missed the object publish an entry for it: the first one's
   swap succeeded against the head it had read, the second one's swap, taken
   against a head read after the first had published, succeeded too, and from
   then on mon_get handed one of the two to each caller. Two threads that took
   different entries for the same object did not exclude each other, and each
   found the other's thread not to be the owner of the monitor it held. A swap
   against the head that was walked fails when anybody else has published
   anything since, the losing entry is thrown away, and the loop looks again and
   finds the winner's. */
static tymon *mon_get(void *obj) {
  uint32_t b = mon_bucket(obj);
  for (;;) {
    tymon *head = __atomic_load_n(&mon_buckets[b], __ATOMIC_ACQUIRE);
    tymon *m;
    for (m = head; m; m = m->next) {
      if (m->key == obj) return m;
    }
    m = mon_make(obj);
    m->next = head;
    if (__atomic_compare_exchange_n(&mon_buckets[b], &head, m, 0, __ATOMIC_RELEASE,
                                    __ATOMIC_ACQUIRE)) {
      return m;
    }
    /* Somebody inserted an entry -- for this object or any other -- first: use
       the list as it is now. Nothing has seen this one, so it can go. */
    typlat_mutex_destroy(&m->mtx);
    typlat_cond_destroy(&m->cv);
    typlat_cond_destroy(&m->wcv);
    free(m);
  }
}

/* Wake a thread that interrupt() has just marked. A blocked thread is asleep on
   one of three condition variables, and this broadcasts the one it published:
   its own (sleep), the monitor's (Object.wait), or the target's (join).

   The publishes and the loads here are seq_cst rather than release/acquire
   because the flag and the published wait are two variables written in
   opposite orders by the two threads, and a release store plus an acquire load
   would leave the classic store-buffer interleaving possible: the interrupter
   stores the flag then reads a stale NULL, the waiter stores the monitor then
   reads a stale zero, and the waiter sleeps through an interrupt that will
   never be repeated. Seq_cst puts both stores in one total order, so at least
   one of the two reads sees the other's store. */
static void interrupt_wake(tythread *t) {
  /* The virtual-aware broadcast, not the platform one: a virtual thread
     asleep in sleep, join or Object.wait is not on the pthread condition
     variable at all, and this is what finds it. For a platform target the
     two calls are the same. */
  typlat_mutex_lock(&t->mtx);
  ty_cond_broadcast(&t->cv);
  typlat_mutex_unlock(&t->mtx);
  tymon *m = (tymon *)__atomic_load_n(&t->wait_mon, __ATOMIC_SEQ_CST);
  if (m) {
    typlat_mutex_lock(&m->mtx);
    ty_cond_broadcast(&m->wcv);
    typlat_mutex_unlock(&m->mtx);
  }
  tythread *jt = (tythread *)__atomic_load_n(&t->join_target, __ATOMIC_SEQ_CST);
  if (jt) {
    typlat_mutex_lock(&jt->mtx);
    ty_cond_broadcast(&jt->cv);
    typlat_mutex_unlock(&jt->mtx);
  }
}

void ty_sync_enter(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  while (m->owned && m->owner != ty_self) {
    /* Waiting for a monitor is waiting: a collection must not wait for this
       thread, and while it is in the wait its stack does not change. The wait
       is the stop point, and a virtual thread parks here instead of blocking
       its carrier. */
    ty_cond_wait(&m->cv, &m->mtx);
  }
  m->owned = 1;
  m->owner = ty_self;
  m->count++;
  typlat_mutex_unlock(&m->mtx);
}

void ty_sync_exit(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || m->owner != ty_self) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  if (--m->count == 0) {
    m->owned = 0;
    /* The monitor is free: one waiter may take it. Which one is up to the
       operating system, as it is in Java. */
    ty_cond_broadcast(&m->cv);
  }
  typlat_mutex_unlock(&m->mtx);
}

int32_t ty_mon_wait(void *obj, int64_t millis) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || m->owner != ty_self) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  /* An interrupt that arrived before the wait starts is thrown rather than
     slept through, and the monitor is still held when this returns -- which is
     where the caller's synchronized block expects it. */
  if (__atomic_load_n(&ty_self->interrupted, __ATOMIC_SEQ_CST)) {
    __atomic_store_n(&ty_self->interrupted, 0, __ATOMIC_SEQ_CST);
    typlat_mutex_unlock(&m->mtx);
    return 1;
  }
  /* wait() gives the monitor up: the notification can only arrive from another
     thread if this one is not holding the lock while it waits. */
  int32_t saved = m->count;
  /* Taken before the monitor is given up, and under it: a notification that
     arrives from here on moves this, so this wait cannot miss one, and one that
     arrived before it does not wake this wait -- which is what Object.wait
     says. */
  int64_t mine = m->notified;
  m->owned = 0;
  m->count = 0;
  m->waiters++;
  /* The monitor is free now, and everybody waiting to enter it -- or to take it
     back after a wait of their own -- has to be told, because that is a
     different wait from this one. */
  ty_cond_broadcast(&m->cv);
  /* The object stays a root while this thread waits on it: notify() has to be
     able to find the monitor, and the monitor is keyed by the object's address.
     The root is exact rather than conservative because the runtime holds it in a
     register whose spill the scan of this stopped thread may not reach. */
  TY_ROOT_PUSH(obj);
  /* Published so that interrupt() can broadcast this monitor's wait variable:
     the waiter sleeps on the monitor's, not on its own. The store is seq_cst
     and the interrupter's load of it is too, which is what makes one of the two
     orderings below impossible: either this store is seen and the broadcast
     arrives, or the interrupter's store of the flag is seen by the test in the
     loop below before this thread sleeps. See interrupt_wake. */
  __atomic_store_n(&ty_self->wait_mon, m, __ATOMIC_SEQ_CST);
  /* The deadline is on the monotonic clock, which is what a duration has to be
     measured against, and what is handed to the wait below is the part of it
     that is left. The layer turns that duration into a deadline on whatever
     clock the condition variable actually uses -- on Linux the same monotonic
     one, on Windows the wall clock winpthreads insists on -- so the two clocks
     never have to be reconciled here. */
  int32_t intr = 0;
  int64_t deadline = millis < 0 ? -1 : ty_nanos() + millis * 1000000;
  for (;;) {
    if (__atomic_load_n(&ty_self->interrupted, __ATOMIC_SEQ_CST)) {
      intr = 1;
      break;
    }
    if (m->notified != mine) break;
    if (deadline < 0) {
      ty_cond_wait(&m->wcv, &m->mtx);
    } else {
      int64_t left = deadline - ty_nanos();
      if (left <= 0) break;
      if (ty_cond_timedwait(&m->wcv, &m->mtx, left)) break;
    }
  }
  __atomic_store_n(&ty_self->wait_mon, NULL, __ATOMIC_SEQ_CST);
  m->waiters--;
  TY_ROOT_POP();
  /* Take the monitor back, exactly as entering it does. */
  while (m->owned && m->owner != ty_self) {
    ty_cond_wait(&m->cv, &m->mtx);
  }
  m->owned = 1;
  m->owner = ty_self;
  m->count = saved;
  /* Same rule as join: an interrupt that arrived while the monitor was being
     taken back is thrown rather than left pending, and the monitor is held when
     it is. */
  if (!intr && __atomic_load_n(&ty_self->interrupted, __ATOMIC_SEQ_CST)) intr = 1;
  typlat_mutex_unlock(&m->mtx);
  /* The status is cleared before the exception is thrown, as java.lang.Object
     promises, and the monitor is held again when it is: this returns to a
     Teyru `wait` wrapper, which throws while the synchronized block still owns
     the monitor, exactly as the JDK's native wait does. */
  if (intr) {
    __atomic_store_n(&ty_self->interrupted, 0, __ATOMIC_SEQ_CST);
    return 1;
  }
  return 0;
}

void ty_mon_notify(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || m->owner != ty_self) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  /* One notification, and one sleeper on wcv is woken. A notify with nobody
     waiting is lost, which is what java.lang.Object.notify says -- nobody took
     the old value of the counter, so nobody sees it change. The counter is a
     level and not a claim on one particular thread, so a waiter that wakes by
     itself, or that the operating system wakes, can also see it move and return
     from a wait sound with no notification being lost; Java allows a wait to
     return for no reason at all, which is why every wait has to be written in a
     loop with its condition re-tested. */
  if (m->waiters > 0) {
    m->notified++;
    /* The virtual-aware signal: a waiter that parked is not on the pthread
       condition variable, and this is what finds it. */
    ty_cond_signal(&m->wcv);
  }
  typlat_mutex_unlock(&m->mtx);
}

void ty_mon_notify_all(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || m->owner != ty_self) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  if (m->waiters > 0) {
    /* Every thread that is waiting now is waiting for the counter as it was
       before this line, so one move of it releases all of them. */
    m->notified++;
    ty_cond_broadcast(&m->wcv);
  }
  typlat_mutex_unlock(&m->mtx);
}
