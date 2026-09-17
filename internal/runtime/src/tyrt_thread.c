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
 *     that blocks (sleep, join, a monitor wait), or the wait for the heap lock
 *     itself. All of them record where the thread stopped, spill the registers
 *     that would otherwise be invisible to a stack scan, and take themselves out
 *     of the set of threads the collector has to wait for.
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
 *     sleep, join, and every monitor wait; the start of a thread.
 *   - not a safepoint: a call that neither loops nor blocks nor allocates. A
 *     thread inside one -- a long computation in a native method, a blocking
 *     read() on a socket, a sleep inside libc -- does not reach a stop point
 *     until it comes back, and a collection waits for it. That is the price of
 *     cooperative safepoints: the collector cannot interrupt a thread that never
 *     cooperates. Every path in this runtime that blocks for a long time is a
 *     stop point for that reason; a native method that blocks outside the
 *     runtime would have to call ty_thread_stopped_begin/end around its wait.
 *
 * Registers. A stack scan sees what a thread has spilled, and a value a frame
 * holds only in a callee-saved register is not in any of that -- the register is
 * the only copy, and the collector reads memory. Every stop point therefore
 * begins with __builtin_unwind_init(), which makes the compiler spill the
 * callee-saved registers into the frame that is about to stop, above the point
 * the thread records, where the scan finds them. The one place this cannot reach
 * is the frame of the function that blocks *inside the platform layer*
 * (typlat_cond_wait, typlat_sleep_ns): its saves are below the recorded point,
 * so the collector starts its scan a fixed margin lower (TY_PARK_MARGIN in
 * tyrt.c), scanning a few hundred extra bytes conservatively. Everything the
 * runtime itself holds across such a wait it holds on its shadow stack, which is
 * exact and needs none of this.
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

#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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

/* The registry: every thread this runtime started, and the main thread. Held
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

/* --------------------------------------------------------------- the registry */

tythread *ty_thread_list(void) {
  return __atomic_load_n(&threads, __ATOMIC_ACQUIRE);
}

/* Add a fully built entry to the registry. The entry is complete before it is
   published, and readers walk the list with an acquire load, so the list can be
   walked without the lock -- which is what the collector does. */
static void registry_add(tythread *t) {
  typlat_mutex_lock(&world_mtx);
  t->next = __atomic_load_n(&threads, __ATOMIC_RELAXED);
  __atomic_store_n(&threads, t, __ATOMIC_RELEASE);
  n_live++;
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
  typlat_cond_broadcast(&world_cv);
  typlat_mutex_unlock(&world_mtx);
}

static tythread *registry_find(int64_t id) {
  for (tythread *t = ty_thread_list(); t; t = t->next) {
    if (t->id == id) return t;
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
   which measures the one path that must not crash. */
#define TY_STACK_MARGIN (256 * 1024)

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
  /* The main thread's stack bounds are what ty_gc_init used to read for the one
     thread there was; they are now part of the thread's state, because a
     collection scans a different stack for every thread. */
  stack_limit_init();
  registry_add(&main_thread);
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
  typlat_cond_broadcast(&t->cv);
  typlat_mutex_unlock(&t->mtx);
  return NULL;
}

int64_t ty_thread_spawn(void *(*fn)(void *), void *arg, void *obj, int64_t id, tystr *name) {
  tythread *t = (tythread *)calloc(1, sizeof(tythread));
  spawn_packet *k = (spawn_packet *)malloc(sizeof(spawn_packet));
  if (!t || !k) abort();
  typlat_mutex_init(&t->mtx);
  typlat_cond_init(&t->cv);
  t->id = id;
  t->obj = obj;
  t->name = name;
  /* Registered before the thread runs, so that join() called the instant after
     start() finds something to wait for. The thread's own fields -- its stack
     bounds, its shadow stack, where it parked -- are filled in by the thread
     itself, and are read by the collector only after the thread has stopped,
     which is after it filled them in. */
  t->state = TY_TH_RUNNING;
  registry_add(t);

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
  return id;
}

void ty_thread_join(int64_t id) {
  tythread *t = registry_find(id);
  if (!t) return; /* never started: java.lang.Thread.join returns at once */
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  typlat_mutex_lock(&t->mtx);
  while (!t->finished) typlat_cond_wait(&t->cv, &t->mtx);
  typlat_mutex_unlock(&t->mtx);
  stopped_end();
}

int32_t ty_thread_alive(int64_t id) {
  tythread *t = registry_find(id);
  return t && !t->finished;
}

void ty_thread_sleep_ms(int64_t millis) {
  if (millis <= 0) {
    /* Java's own two cases: a negative duration is an IllegalArgumentException,
       and sleep(0) is a yield rather than a wait. */
    if (millis < 0) ty_throw(ty_illarg("timeout value is negative"));
    ty_thread_yield();
    return;
  }
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  /* The wait itself is the platform layer's: a millisecond argument is normal
     here, and the whole duration is what Java's Thread.sleep promises, so the
     layer restarts it after a signal rather than truncating it. */
  typlat_sleep_ns(millis * 1000000);
  stopped_end();
}

int64_t ty_thread_current_id(void) { return ty_self->id; }

void ty_thread_yield(void) {
  /* A yield is a good place to notice a collection, and it costs a load. */
  ty_safepoint();
  typlat_yield();
}

void *ty_thread_current_obj(void) { return ty_self->obj; }

void ty_thread_bind(void *obj) { ty_self->obj = obj; }

void ty_thread_join_all(void) {
  TY_PARK_BEGIN();
  stopped_begin(TY_TH_BLOCKED);
  typlat_mutex_lock(&world_mtx);
  /* Every thread but this one. A thread that is started while this waits is
     counted when it is registered, so the condition covers it too. */
  while (n_live > 1) typlat_cond_wait(&world_cv, &world_mtx);
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

int64_t ty_thread_start0(void *obj, int64_t id, tystr *name, int32_t sel) {
  body_args *b = (body_args *)malloc(sizeof(body_args));
  if (!b) abort();
  b->obj = obj;
  b->sel = sel;
  return ty_thread_spawn(thread_body, b, obj, id, name);
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
  typlat_thread owner;
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

void ty_sync_enter(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  while (m->owned && !typlat_thread_same(m->owner, typlat_thread_self())) {
    /* Waiting for a monitor is waiting: a collection must not wait for this
       thread, and while it is in the wait its stack does not change. */
    ty_thread_stopped_begin();
    typlat_cond_wait(&m->cv, &m->mtx);
    ty_thread_stopped_end();
  }
  m->owned = 1;
  m->owner = typlat_thread_self();
  m->count++;
  typlat_mutex_unlock(&m->mtx);
}

void ty_sync_exit(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || !typlat_thread_same(m->owner, typlat_thread_self())) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  if (--m->count == 0) {
    m->owned = 0;
    /* The monitor is free: one waiter may take it. Which one is up to the
       operating system, as it is in Java. */
    typlat_cond_broadcast(&m->cv);
  }
  typlat_mutex_unlock(&m->mtx);
}

void ty_mon_wait(void *obj, int64_t millis) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || !typlat_thread_same(m->owner, typlat_thread_self())) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
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
  typlat_cond_broadcast(&m->cv);
  /* The object stays a root while this thread waits on it: notify() has to be
     able to find the monitor, and the monitor is keyed by the object's address.
     The root is exact rather than conservative because the runtime holds it in a
     register whose spill the scan of this stopped thread may not reach. */
  TY_ROOT_PUSH(obj);
  /* The deadline is on the monotonic clock, which is what a duration has to be
     measured against, and what is handed to the wait below is the part of it
     that is left. The layer turns that duration into a deadline on whatever
     clock the condition variable actually uses -- on Linux the same monotonic
     one, on Windows the wall clock winpthreads insists on -- so the two clocks
     never have to be reconciled here. */
  int64_t deadline = millis < 0 ? -1 : ty_nanos() + millis * 1000000;
  while (m->notified == mine) {
    if (deadline < 0) {
      ty_thread_stopped_begin();
      typlat_cond_wait(&m->wcv, &m->mtx);
      ty_thread_stopped_end();
    } else {
      int64_t left = deadline - ty_nanos();
      if (left <= 0) break;
      ty_thread_stopped_begin();
      int timed_out = typlat_cond_timedwait(&m->wcv, &m->mtx, left);
      ty_thread_stopped_end();
      if (timed_out) break;
    }
  }
  m->waiters--;
  TY_ROOT_POP();
  /* Take the monitor back, exactly as entering it does. */
  while (m->owned && !typlat_thread_same(m->owner, typlat_thread_self())) {
    ty_thread_stopped_begin();
    typlat_cond_wait(&m->cv, &m->mtx);
    ty_thread_stopped_end();
  }
  m->owned = 1;
  m->owner = typlat_thread_self();
  m->count = saved;
  typlat_mutex_unlock(&m->mtx);
}

void ty_mon_notify(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || !typlat_thread_same(m->owner, typlat_thread_self())) {
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
    typlat_cond_signal(&m->wcv);
  }
  typlat_mutex_unlock(&m->mtx);
}

void ty_mon_notify_all(void *obj) {
  tymon *m = mon_get(obj);
  typlat_mutex_lock(&m->mtx);
  if (!m->owned || !typlat_thread_same(m->owner, typlat_thread_self())) {
    typlat_mutex_unlock(&m->mtx);
    ty_throw(ty_make_ex(TY_ILLMON, "current thread is not owner"));
  }
  if (m->waiters > 0) {
    /* Every thread that is waiting now is waiting for the counter as it was
       before this line, so one move of it releases all of them. */
    m->notified++;
    typlat_cond_broadcast(&m->wcv);
  }
  typlat_mutex_unlock(&m->mtx);
}
