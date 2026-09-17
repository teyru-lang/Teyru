/* tyrt.h - Teyru native runtime: objects, strings, arrays, exceptions, GC. */
#ifndef TYRT_H
#define TYRT_H

#include <stdint.h>
#include <stddef.h>
#include <setjmp.h>
#include <string.h>

/* Every operating system call the runtime makes is behind this header: the
   locks, the threads, the clock, and the sockets. Nothing below this line
   includes a system header for one of them, so this runtime has one platform
   layer to port rather than one per file. */
#include "tyrt_plat.h"

typedef struct tyclass tyclass;
typedef struct tyobj tyobj;

struct tyobj {
  tyclass *cls;
};

typedef struct tystr {
  tyobj obj;
  int64_t len;
  char *data;
} tystr;

typedef struct tyarr {
  tyobj obj;
  int64_t len;
  char *data;
  int32_t esize;
  int32_t refs; /* 1 when elements are object references */
  /* The class the array promised for its elements, recorded by the `new T[]`
     the array came from. Java's arrays are covariant but their element type is
     fixed at creation, so a store through a wider view (Object[] o = new
     String[2]) has to reject a value the array never promised; elemcls is what
     ty_array_store_ref compares against. NULL means the array carries no
     promise and accepts any reference, which is what every array the runtime
     itself creates does. */
  tyclass *elemcls;
} tyarr;

typedef struct tymap {
  int32_t sel;
  void *fn;
} tymap;

/* ---- reflection metadata ----------------------------------------------

   One static table per class, written by the compiler, read by
   java.lang.reflect. Nothing here is built at run time, no object carries any
   of it, and the collector never walks it: a program pays for reflection in
   .data and in code, never in the object header or in a call.

   A method's fn is an invoker rather than the method itself: it takes the
   receiver and a void** of boxed arguments and answers with a boxed result,
   which is what Method.invoke has to do. The invoker is what the compiler
   emits per method, and it is the only place that knows the method's C
   signature. */

/* tymethod.kind */
#define TY_METH_INSTANCE 0
#define TY_METH_STATIC 1
#define TY_METH_CTOR 2
#define TY_METH_VARARGS 4

/* ---- annotations --------------------------------------------------------

   An annotation as this runtime carries it: the annotation type, and its
   elements as name/value pairs. Java hands back an implementation of the
   annotation interface, so a program writes ann.value(); that needs a class
   per annotation type, which the compiler does not synthesize yet, so an
   element is read by name here. The difference is in AGENTS.md §10. */

#define TY_ANN_UNSUPPORTED 0 /* an element value typing cannot carry yet */
#define TY_ANN_STRING 1
#define TY_ANN_INT 2
#define TY_ANN_LONG 3
#define TY_ANN_DOUBLE 4
#define TY_ANN_BOOL 5
#define TY_ANN_CLASS 6
#define TY_ANN_ENUM 7

typedef struct tyannoarg {
  const char *name;
  int32_t kind;
  int64_t ival;     /* int, long and boolean */
  double dval;
  const char *sval; /* a string, or an enum constant's name */
  tyclass *cval;    /* a class literal, or the enum's class */
} tyannoarg;

typedef struct tyannotation {
  tyclass *type;
  const tyannoarg *args;
  int32_t nargs;
} tyannotation;

typedef struct tyfield {
  const char *name;
  tyclass *type;
  tyclass *owner;
  int32_t off;  /* byte offset of the field in an instance, -1 for a static */
  int32_t mods; /* java.lang.reflect.Modifier bits */
  void *addr;   /* address of a static field, NULL for an instance field */
  int32_t prim; /* primitive kind when the type is a primitive, else 0 */
  const tyannotation *annos; /* the annotations written on the field, or NULL */
  int32_t nannos;
  /* the declared element type of a container field: the element of a Collection
     or an array, the value type of a Map, and NULL when the declared type
     carries none. Reflection erases it -- a List<Person> is a List -- so a
     reader that only has the erased type reads it here
     (Field.getElementType, lib/26_reflect.teyru). */
  tyclass *elem;
} tyfield;

typedef struct tymethod {
  const char *name;
  void *fn; /* void *(*)(void *self, void **args), boxed in and out */
  tyclass *owner;
  tyclass *ret;
  const tyclass **params;
  int32_t nparams;
  int32_t mods;
  int32_t kind;
  int32_t primret; /* primitive kind of the result, else 0 */
  const tyannotation *annos; /* the annotations written on the method, or NULL */
  int32_t nannos;
  /* reflection: one annotation list per parameter, and how many each holds.
     A parameter's annotations are written on the parameter, which is where
     @Value and @Autowired are read from. */
  const tyannotation *const *pannos;
  const int32_t *pnannos;
  /* the parameter names, which is what Java keeps when a program is compiled
     with -parameters and what a handler binding a request reads a parameter
     from: a route's @RequestParam takes the name the parameter was written
     with unless the annotation says otherwise. */
  const char *const *pnames;
} tymethod;

struct tyclass {
  const char *name;
  int32_t id;
  int32_t flags; /* 1 = interface, 2 = array, 4 = primitive wrapper */
  tyclass *super;
  int32_t niface;
  tyclass **ifaces;
  int32_t nvt;
  void **vtable;
  void *clinit;
  int32_t isize;
  int32_t isel;
  tymap *imap;
  int32_t nsub;
  tyclass **subs;
  int32_t nref;     /* number of traced reference fields */
  int32_t *refoffs; /* byte offsets of reference fields */
  /* reflection: the class's own modifiers, its fields, its methods (with the
     constructors), and its enum constants when it is an enum. Everything is
     static data; a class with nothing to report carries NULL and 0. */
  int32_t mods; /* java.lang.reflect.Modifier bits */
  int32_t prim; /* primitive kind when this class *is* a primitive, else 0 */
  const tyfield *fields;
  int32_t nfields;
  const tymethod *methods;
  int32_t nmethods;
  void **consts; /* enum constants, in declaration order */
  int32_t nconsts;
  /* reflection: the annotations written on the class itself */
  const tyannotation *annos;
  int32_t nannos;
};

/* Class handles installed by generated startup code. */
extern tyclass *TY_STRING;
extern tyclass *TY_ARRAY;
extern tyclass *TY_BOX[9];
extern tyclass *TY_OBJECT;

/* ---- exceptions ------------------------------------------------------- */
typedef struct tycatch {
  jmp_buf buf;
  struct tycatch *prev;
  tyobj *ex;
} tycatch;

/* Per thread: see tyrt_thread.c's empty catch frames and ty_throw. */
extern _Thread_local tycatch *ty_cur_catch;

void ty_throw(void *e) __attribute__((noreturn));
void ty_uncaught(void *e) __attribute__((noreturn));

/* Preallocated exception classes (filled by generated code at startup). */
extern tyclass *TY_NPE, *TY_AIOOBE, *TY_SIOOBE, *TY_ARITH, *TY_CCE, *TY_NEGARR, *TY_ASSERT,
    *TY_ILLARG, *TY_ILLSTATE, *TY_NOSUCHELEM, *TY_UNSUP, *TY_ARRAYSTORE,
    *TY_ILLMON;

/* The exceptions java.lang.reflect throws by name. A program that never
   reflects never names one, and the generated startup leaves the pointer NULL
   then; the reflection entry points are the only code that reads them. */
extern tyclass *TY_CNF, *TY_NSFE, *TY_NSME, *TY_ILLACCESS, *TY_INVOCATION,
    *TY_INSTANTIATION;

void *ty_npe(void);
void *ty_aioobe(int64_t idx, int64_t len);
void *ty_sioobe(int64_t idx, int64_t len);
void *ty_arith(const char *msg);
void *ty_cce(tyclass *from, tyclass *to);
void *ty_negarr(void);
void *ty_arraystore(void);
void *ty_assertfail(const char *msg);

/* ---- allocation / GC -------------------------------------------------- */

/* The fast path lives in the header so the generated program allocates with an
   inlined bump-pointer check; only a full page or a pending collection falls
   back into the runtime. */
/* class flags: the class has been initialised */
#define TY_CLS_INIT 8
/* a thread is running this class's initializer right now. Set and cleared under
   the class's own monitor, which that thread holds for the whole initializer,
   so the only thread that can find it set is the one that set it -- which is
   what tells an initializer that reaches its own class (a cache built by the
   factory that reads it) apart from a thread arriving while another one
   initializes. The bit TY_CLS_INIT means "finished", and it is published only
   once the initializer has returned; this one means "running". */
#define TY_CLS_BUSY 32
/* the class describes an array: its payload is a tyarr whose element slots the
   collector has to trace when the array holds references */
#define TY_CLS_ARRAY 2
/* the class is a record: Java answers Class.isRecord with it, and a record's
   canonical constructor is the one its binding reads */
#define TY_CLS_RECORD 16
#define TY_HDR 16
#define TY_ALIGN 16

/* How many blocks a thread takes out of the heap's free lists at once. The
   sweep of a collection hands a whole slab of dead blocks back to those lists,
   and a thread that then allocates them one at a time pays the heap lock, the
   safepoint around it and a free-list write for every one of the thousands of
   them: measured at 14 ns an allocation, 31% of bench_invoke. A batch pays that
   once for TY_BATCH of them instead.

   It is a bound on memory a collection cannot reuse rather than a tuning knob:
   worst case a thread holds TY_BATCH blocks of every size class, and the
   classes run to 1 KB, so a thread that allocates one block of every class and
   then stops still holds under 2 MB. TY_NCLASS in tyrt.c is the number of
   classes and the size of the table below. */
#define TY_BATCH 64
#define TY_BATCH_CLASSES 64

/* ---- threads and the per-thread state ---------------------------------

   Everything the runtime keeps for one thread lives in the struct below,
   reached through one thread-local pointer. It is a struct rather than a set of
   thread-local variables because two of its fields have to be written by
   *another* thread: the collector resets a stopped thread's allocation budget
   after a collection and reads the stack pointer it parked at, and C11 has no
   way to address another thread's thread-local storage.

   The first four fields are the allocation buffer the fast path below bumps,
   which is why this header carries the struct at all: allocation is inlined
   into the generated program, so the layout it walks has to be visible to it.
   tyrt_thread.c explains the protocol these fields take part in. */
typedef struct tythread tythread;
struct tythread {
  /* The thread's own slab of the heap. `bump` walks it and `bump_end` is where
     it ends, so an allocation is a pointer comparison and a pointer bump; only
     a refill -- the slab running out, or the collection budget running out --
     enters the runtime at all, and one the batch below can serve does it
     without the heap lock. `chunk` is the slab itself, a tychunk the heap owns
     and the collector walks. */
  char *bump, *bump_end;
  int64_t alloc_since;
  void *chunk;
  /* The thread's own share of the free lists: what one batch taken from them
     under the heap lock left behind, indexed by the size class the blocks were
     filed under -- and a block's class is its size, so a block handed out here
     is exactly the size the request asked for and nothing is searched. This is
     where the drain of a collection's dead blocks is served from without the
     lock. collect_slabs empties every thread's table before the sweep rebuilds
     the free lists, so that no thread is left holding a link into a list the
     sweep rewrote. */
  void *batch[TY_BATCH_CLASSES];
  /* The shadow stack: the roots the runtime's own C code pushes around an
     object it holds across a call (tyrt_net.c and tyrt_reflect.c). Generated
     code needs none: its live references are on the C stack, which the
     collector scans conservatively. */
  void **roots;
  int64_t sp;
  /* The C stack this thread runs on, as the collector sees it: `stack_base` is
     the lowest address of the stack and `stack_top` the highest. `park_sp` is
     the stack pointer taken the last time the thread stopped, and a stopped
     thread is scanned from there upwards. It only ever moves down, so it stays
     a lower bound on what has to be scanned. */
  char *stack_base, *stack_top, *park_sp;
  typlat_thread tid;
  typlat_mutex mtx; /* guards the fields below, and signals a joiner */
  typlat_cond cv;
  int64_t id;             /* what Thread.getId() reports */
  void *obj;              /* the Teyru Thread object, NULL before one is bound */
  tystr *name;            /* the thread's name, for an uncaught exception */
  int32_t state;          /* one of TY_TH_* below */
  /* How many stop points this thread is inside. Stopping is not nested -- no
     path in the runtime blocks inside another blocking call -- and the depth is
     what keeps the transitions honest anyway: only the transition from 0 to 1
     takes the thread out of the collector's way, and only 1 to 0 puts it back. */
  int32_t stop_depth;
  /* Whether the thread collecting was itself stopped when it started, which is
     what a collection started from an allocation does: it holds the heap lock,
     and waiting for that lock counts as stopped. The collector must not count
     itself among the threads it is waiting for. */
  int32_t was_stopped_for_gc;
  /* Set by the thread itself as its last act before it returns: what join()
     waits for, and what isAlive() answers. The registry entry is not the
     thread's stack -- it outlives the thread -- so this, and not the operating
     system's idea of the thread, is what the program asks about. */
  int32_t finished;
  struct tythread *next;  /* the registry the collector walks */
};

/* What a thread is doing, as the stop-the-world protocol cares: a RUNNING
   thread is the only kind that can be holding a reference the collector has not
   seen yet, and so the only kind a collection waits for. A thread is PARKED at
   a safepoint (see ty_safepoint) or BLOCKED inside a runtime call that waits
   (a sleep, a join, a monitor); DONE threads are not scanned at all -- their
   stacks are going away with them. */
#define TY_TH_RUNNING 0
#define TY_TH_PARKED 1
#define TY_TH_BLOCKED 2
#define TY_TH_DONE 3

extern _Thread_local tythread *ty_self;

#define TY_SHADOW_MAX (1 << 20)
/* A spawned thread's shadow stack. The runtime pushes a handful of roots at a
   time -- one per native call that holds an object across a call that can
   collect -- so this is far more than any thread needs; the main thread keeps
   the larger static block it has always had. */
#define TY_SHADOW_THREAD (1 << 16)
#define TY_ROOT_PUSH(v) (ty_self->roots[ty_self->sp++] = (void *)(v))
#define TY_ROOT_POP() (--ty_self->sp)

/* ---- stop the world ----------------------------------------------------

   Set while a collection is stopping the world: a thread that reads it set has
   to park before it touches the heap again. It is read through __atomic and not
   through a lock, because the check sits on loop back-edges and has to cost one
   load and one predictable branch. */
extern int32_t ty_stw_request;
void ty_safepoint_slow(void);

/* ty_safepoint is the cooperative half of the stop-the-world protocol. A thread
   that arrives here while a collection is running stops -- its stack and
   registers made visible to the collector -- and runs again only once the
   collection is over. The generated code puts one at the top of every loop
   body, so a loop that allocates nothing and calls nothing still gets stopped;
   the runtime puts one on the allocation slow path and inside every call that
   blocks.

   What is *not* a safepoint, and what it costs: a thread inside a call that
   neither loops nor blocks -- a long calculation, a read() on a socket, a sleep
   inside libc -- never reaches one, and a collection waits for every running
   thread to reach one. Such a thread therefore stalls the collector (and with
   it every other thread that wants to allocate) until it comes back. This is
   documented at the top of tyrt_thread.c together with the rest of the
   protocol. */
static inline void ty_safepoint(void) {
  if (__atomic_load_n(&ty_stw_request, __ATOMIC_ACQUIRE)) ty_safepoint_slow();
}

/* ---- allocation -------------------------------------------------------- */

extern int64_t ty_gc_threshold;
void *ty_alloc_slow(size_t total);

static inline void *ty_alloc(size_t size) {
  size_t total = (size + TY_HDR + TY_ALIGN - 1) & ~(size_t)(TY_ALIGN - 1);
  tythread *me = ty_self;
  char *p = me->bump;
  if (p + total > me->bump_end || me->alloc_since > ty_gc_threshold) {
    return ty_alloc_slow(total);
  }
  me->bump = p + total;
  me->alloc_since += (int64_t)total;
  *(uint64_t *)p = (uint64_t)total;
  *(uint64_t *)(p + 8) = 0;
  void *obj = p + TY_HDR;
  memset(obj, 0, total - TY_HDR);
  return obj;
}
void *ty_alloc_arr(int64_t len, size_t elemsize);
void ty_gc_init(void);
void ty_gc(void);
void ty_gc_register_static(void *p);
void ty_free_block(void *payload, size_t total);

/* ---- narrowing a floating-point value to an integer -------------------- */

/* Java's narrowing conversion of a floating-point value to an integral type is
   not C's: C leaves a value that does not fit undefined, and what a machine
   does with it is not an answer -- x86-64's cvttsd2si gives 0x80000000, another
   machine traps, and an optimiser folding a constant expression may give
   anything at all (the four builds of `(long) 1.0e20` this replaced answered
   160, 0, -9223372036854775808 and 48). JLS 5.1.3 gives the answer instead:
   NaN is 0, either infinity is the maximum, a value too large is the maximum,
   a value too small is the minimum, and anything else is the value truncated
   toward zero.

   Both back ends call these for a conversion from float or double to an
   integral type. A conversion to a type narrower than int is this int and then
   the truncation to that type, which is the two steps JLS 5.1.3 states; the
   callers do the second step. A float source is widened to double on the way
   in, which is exact and is the order the conversion is defined in. */
int32_t ty_d2i(double d);
int64_t ty_d2l(double d);

/* ---- the heap, as tyrt_thread.c needs to see it ------------------------ */

/* The lock every thread takes to refill its slab and to sweep. It is not the
   lock the stop-the-world protocol uses: a collector holds this one across the
   whole collection, so a thread parking for that collection cannot be waiting
   for it. */
void ty_heap_lock(void);
void ty_heap_unlock(void);
/* Publish this thread's slab watermark. The inlined fast path bumps a pointer
   and nothing else, so the slab's `used` watermark trails behind it; the
   collector walks slabs by that watermark and has to see the true one. Called
   wherever a thread stops (it is about to be walked) and by the thread that
   hands its own slab back. */
void ty_heap_sync(void);
/* Give this thread's slab back to the shared heap, which is what makes its
   free space available to the other threads and lets a collection reclaim the
   whole slab once nothing in it is live. */
void ty_heap_detach(void);
/* The collection itself, for a caller that already holds the heap lock -- the
   allocation slow path, which is where nearly every collection is asked for. */
void ty_gc_locked(void);

/* ---- the registry, as tyrt.c needs to see it --------------------------- */

tythread *ty_thread_list(void);
void ty_thread_init(void);
void ty_gc_stop_world(void);
void ty_gc_resume_world(void);

/* Stop the calling thread for a collection, from here until the matching
   ty_thread_stopped_end: the caller is doing something that leaves it out of the
   heap (waiting for the heap lock, most of all), and a collection must not wait
   for it. Both are tyrt_thread.c's; tyrt.c's heap lock is the caller that
   matters. */
void ty_thread_stopped_begin(void);
void ty_thread_stopped_end(void);

/* ---- threads ----------------------------------------------------------- */

/* The body of a spawned thread: typlat_thread_begin is handed this, it installs
   the new thread's runtime state (its shadow stack, its stack bounds, its place
   in the registry), runs fn(arg), marks itself finished and wakes a joiner. It
   returns NULL. */
void *ty_thread_start(void *(*fn)(void *), void *arg);

/* Create a thread that runs ty_thread_start(fn, arg). obj is the Teyru Thread
   object the thread runs for: it stays a root for as long as the thread lives
   (the thread pushes it onto its own shadow stack), and it is what
   Thread.currentThread() answers with inside the new thread. id is the id that
   object was given when it was built, so Thread.getId() is the same number
   before and after start(). name is what an uncaught exception prints. Returns
   id. */
int64_t ty_thread_spawn(void *(*fn)(void *), void *arg, void *obj, int64_t id, tystr *name);
/* The id Thread.getId() reports for a Thread object that was never started: a
   counter that only ever goes up, as java.lang.Thread's does. */
int64_t ty_thread_next_id(void);
void ty_thread_join(int64_t id);
int32_t ty_thread_alive(int64_t id);
void ty_thread_sleep_ms(int64_t millis);
int64_t ty_thread_current_id(void);
void ty_thread_yield(void);
/* The Teyru Thread object of the calling thread, or NULL when the calling
   thread has none yet (the main thread before anything asks for it). */
void *ty_thread_current_obj(void);
/* Bind this object to the calling thread, so that currentThread() answers with
   it from here on. The prelude's Thread.currentThread() uses it for the main
   thread, which no start() ever created an object for. */
void ty_thread_bind(void *obj);
/* Wait for every other thread to finish, which is what the end of main does:
   Java's process ends when its last non-daemon thread does, and a program that
   starts a thread and returns should not lose the thread's output. */
void ty_thread_join_all(void);

/* The C half of lib/35_thread.teyru's Thread.start(): obj is the Thread object
   to run, sel is the interface selector of Runnable.run, which is how a thread
   reaches the program's own run() without the runtime knowing the layout of a
   class it was not compiled against. */
int64_t ty_thread_start0(void *obj, int64_t id, tystr *name, int32_t sel);

/* A thread's uncaught exception: the same line the main thread's handler
   prints, with the thread's name, after which the thread ends and the process
   carries on -- again, what Java does. */
void ty_uncaught_thread(void *e, tythread *t);

/* ---- monitors ---------------------------------------------------------- */

/* The monitor of an object: a mutex, an owner and a recursion count, kept in a
   hash table keyed by the object's address rather than in a field of every
   object. A field would cost eight bytes in every allocation in the program --
   including every program that never synchronizes on anything -- and would
   change the instance layout the compiler computes field offsets from. */
/* ty_sync_enter/ty_sync_exit are what a `synchronized` block compiles to, and
   they are declared with the rest of the runtime below; what Object.wait below
   needs is the same monitor, held or given up. */
/* Object.wait/monitor's own semantics: release the monitor, wait for a notify
   or for `millis` to pass, take it back. A negative timeout waits forever. */
void ty_mon_wait(void *obj, int64_t millis);
void ty_mon_notify(void *obj);
void ty_mon_notify_all(void *obj);

/* ---- strings ---------------------------------------------------------- */
tystr *ty_str_new(const char *data, int64_t len);
tystr *ty_str_intern(const char *data);
int64_t ty_str_len(tystr *s);
tystr *ty_str_concat(tystr *a, tystr *b);
int32_t ty_str_eq(tystr *a, tystr *b);
int32_t ty_str_cmp(tystr *a, tystr *b);
int32_t ty_str_hash(tystr *s);
tystr *ty_str_of_int(int64_t v);
tystr *ty_str_of_bool(int32_t v);
tystr *ty_str_of_char(uint16_t c);
tystr *ty_str_of_double(double v);
tystr *ty_str_of_float(float v);
tystr *ty_str_of_long(int64_t v);
tystr *ty_str_of_obj(void *o);
int32_t ty_obj_hash(void *o);
int32_t ty_obj_eq(void *a, void *b);
tystr *ty_str_upper(tystr *s);
tystr *ty_str_lower(tystr *s);
tystr *ty_str_trim(tystr *s);
tystr *ty_str_sub(tystr *s, int32_t from, int32_t to);
int32_t ty_str_indexof(tystr *s, tystr *sub);
int32_t ty_str_charat(tystr *s, int32_t i);
int32_t ty_str_contains(tystr *s, tystr *sub);
int32_t ty_str_starts(tystr *s, tystr *p);
int32_t ty_str_ends(tystr *s, tystr *p);
tystr *ty_str_replace(tystr *s, uint16_t a, uint16_t b);
int32_t ty_str_isempty(tystr *s);
int32_t ty_str_toint(tystr *s);

/* ---- interfaces / casts ---------------------------------------------- */
/* Declared before the arrays: the array store test below calls ty_instanceof
   for the values whose class is not the one the array promised. */
void ty_itab_slow(void) __attribute__((noreturn));

/* Interface dispatch.
 *
 * The per-class map is sparse: one entry per selector the class answers for,
 * sorted by selector, so a class pays for what it implements rather than for
 * every selector the program declares. The dense table this replaced was one
 * slot per selector -- 4.4 KB of .data per class, 792 KB in a hello world.
 *
 * It is inline because nearly every call site passes a selector the compiler
 * folded to a constant, and then this is a compare against that constant
 * instead of a loop: a class that answers for one method, which is most of
 * them, costs one load of the map and one of the entry. Out of line it was a
 * call plus the loop's bookkeeping, and 2,000,000 dispatches in bench_oop
 * were 20% slower than the indexed load it replaced. bench_oop is back to
 * where it was with this. */
static inline void *ty_itab(void *p, int32_t sel) {
  tyobj *o = (tyobj *)p;
  if (!o) ty_npe();
  for (tyclass *k = o->cls; k; k = k->super) {
    tymap *m = k->imap;
    for (int32_t i = 0, n = k->isel; i < n; i++) {
      if (m[i].sel == sel) return m[i].fn;
    }
  }
  /* No class in the chain answers for the selector. There is no function to
     call, so this never comes back: it throws a catchable error. */
  ty_itab_slow();
}

int32_t ty_instanceof(void *o, tyclass *c);
void *ty_checkcast(void *o, tyclass *c);

/* ty_class_is_sub reports whether k is c, or inherits from it: the walk both
   instanceof and Class.isAssignableFrom need, in one place. */
int32_t ty_class_is_sub(tyclass *k, tyclass *c);

/* ty_class_target answers with the class a Class value names, for either form
   the value comes in as (see ty_class_target in tyrt_reflect.c). ty_class_make
   builds the wrapper for a class, which is what Class.forName and the members
   handed back by reflection need. */
tyclass *ty_class_target(void *c);
void *ty_class_make(int64_t h, tyclass *clscls);

/* ---- arrays ----------------------------------------------------------- */
tyarr *ty_array_new(int64_t len, int64_t elemsize);
int64_t ty_array_len(tyarr *a);
tyarr *ty_array_clone(tyarr *a, int64_t elemsize);

/* Store the reference v into element i of a, applying the array store check
   Java applies to a covariant store: a value whose class the array's element
   class never promised is an ArrayStoreException, not a silent write. The null
   test and the bounds test come first, in Java's order.

   selem is the element class at the store site. It lets the two common cases
   skip the test, which is what keeps the check off the hot path:

   - an array that carries no promise (elemcls == NULL) accepts anything, as it
     did before element classes were recorded;
   - an array whose promise is exactly selem cannot fail this store. The
     compiler accepted the store, so the value's static type is assignable to
     selem, and assignability is about the class hierarchy: whatever value
     arrives, its class is a subtype of selem -- which is the class this array
     promised. The test would always pass, so it is not made.

   Anything else is checked: the value's own class first, then its subtyping,
   so `Object[] o = new String[2]; o[0] = "x"` costs one compare.

   The first test is deliberately `a->elemcls != selem` rather than the two
   tests it spells out, so that the case a caller spends its time in -- a store
   into an array that really is an selem[] -- costs one load, one compare and
   one branch. Everything the first test does not answer falls into the second
   test, which is written the long way round because it has to be right, not
   quick.

   The generated code inlines this, so the fast path is a compare and a store;
   the compiler's generated store sites pass &cls_<element type> for selem. */
static inline void ty_array_store_ref(tyarr *a, tyclass *selem, int64_t i, void *v) {
  if (!a) ty_npe();
  if (i < 0 || i >= a->len) ty_aioobe(i, a->len);
  if (a->elemcls != selem && a->elemcls && v &&
      ((tyobj *)v)->cls != a->elemcls && !ty_instanceof(v, a->elemcls)) {
    ty_arraystore();
  }
  ((void **)a->data)[i] = v;
}
void *ty_arr_ptr(tyarr *a, int64_t i);
void *ty_arr_slot_ref(tyarr *a, int64_t i);
void *ty_arr_ref(tyarr *a, int64_t i);

/* ---- boxing -----------------------------------------------------------
   Boxing, unboxing, the six Number conversions, the wrappers' hashing,
   equality, ordering and text forms are Teyru: lib/04_boxing.teyru holds them
   as methods of the wrapper classes, and the compiler calls those methods
   where it used to call a helper here. What stays in C is what the language
   cannot state -- the box's memory (the tyintbox..tyshortbox structs at the
   end of this file, which the reflection path reads through a raw pointer)
   and the bit views of a float and a double (ty_double_bits, ty_float_bits
   and the raw forms beside them). */

/* Primitive type patterns (JEP 507). ty_prim_match reports whether the operand
   matches the requested primitive kind and stores the converted value through
   out. kind uses the same numbering as TY_BOX: 1 boolean, 2 byte, 3 short,
   4 char, 5 int, 6 long, 7 float, 8 double.

   boxed says the operand was a reference and therefore carries a box: JEP 507
   then requires the box to be exactly the pattern's type (an Integer matches
   `int i` but not `long l`). When the operand was a primitive, which the
   compiler boxes to get here, only the conversion has to be exact, so
   `long v = 5; v instanceof int i` matches but 5000000000L does not. */
int32_t ty_prim_match(void *o, int32_t kind, void *out, int32_t boxed);

/* ---- misc ------------------------------------------------------------- */
void ty_sync_enter(void *lock);
void ty_sync_exit(void *lock);
void ty_println_str(tystr *s);
void ty_print_str(tystr *s);
void ty_print_int(int64_t v);
void ty_println_int(int64_t v);
void ty_print_double(double v);
void ty_println_double(double v);
void ty_print_float(float v);
void ty_println_float(float v);
void ty_print_char(uint16_t c);
void ty_println_char(uint16_t c);
void ty_print_bool(int32_t v);
void ty_println_bool(int32_t v);
void ty_print_obj(void *o);
void ty_println_obj(void *o);
void ty_println_void(void);
void ty_init(void);
tystr *ty_readln(void);
void ty_unimplemented(const char *what) __attribute__((noreturn));

/* ---- prelude helpers --------------------------------------------------- */
void ty_clinit(tyclass *c);
void *ty_class_of(void *o);
tystr *ty_class_name(void *c);

/* ---- reflection ---------------------------------------------------------

   The natives java.lang.reflect is written on. Everything here reads the
   static tables the compiler emits; nothing builds a table at run time.

   cm arguments are a Class *value* cast through int64_t: the prelude holds one
   in a long field, because a Class value is either the object ty_class_of_cls
   returns or the raw handle a class literal produces, and both are accepted by
   ty_class_target. i arguments are indices into the class's own tables, and
   the prelude only asks for indices a count it read first allows. */

/* The tyclass a Class value names, whichever form it came in as. */
tyclass *ty_class_target(void *c);

/* Every entry point below takes cm: a Class value cast through int64_t, which
   is how the prelude holds one (a Class value is either the object
   ty_class_of_cls returns or the raw handle a class literal produces). One
   that hands a class back returns another cm, 0 for null. */
int64_t ty_class_handle(void *c);
int32_t ty_class_mods(int64_t cm);
int32_t ty_class_primkind(int64_t cm);
int32_t ty_class_isprim(int64_t cm);
int32_t ty_class_isarray(int64_t cm);
int32_t ty_class_isenum(int64_t cm);
int32_t ty_class_isrecord(int64_t cm);
int32_t ty_class_isannotation(int64_t cm);
int32_t ty_class_isinterface(int64_t cm);
int32_t ty_class_isinstance(int64_t cm, void *o);
int32_t ty_class_assignable(int64_t cm, int64_t other);
int32_t ty_class_ifacecount(int64_t cm);
int64_t ty_class_ifaceat(int64_t cm, int32_t i);
tystr *ty_class_simplename(int64_t cm);
int32_t ty_class_enumcount(int64_t cm);
void *ty_class_enumat(int64_t cm, int32_t i);
int64_t ty_class_superof(int64_t cm);

/* forName: the name is matched against the program's own class names, the
   binary names Java uses ("teyru.List", "main.Outer$Inner"). clscls is the
   program's teyru.Class, which the call site hands over so the object it
   returns is an instance of it. */
/* Annotations. Every entry takes the handle of a tyannotation record, which is
   static data: the prelude holds one in a long, the way it holds a class. */
int64_t ty_ann_type(int64_t ah);
int32_t ty_ann_argcount(int64_t ah);
tystr *ty_ann_argname(int64_t ah, int32_t i);
int32_t ty_ann_argkind(int64_t ah, int32_t i);
int64_t ty_ann_argint(int64_t ah, int32_t i);
double ty_ann_argdouble(int64_t ah, int32_t i);
tystr *ty_ann_argstr(int64_t ah, int32_t i);
int64_t ty_ann_argclass(int64_t ah, int32_t i);
int32_t ty_ann_argindex(int64_t ah, tystr *name); /* -1 when absent */
int32_t ty_ann_same(int64_t a, int64_t b);

int32_t ty_class_anncount(int64_t cm);
int64_t ty_class_annat(int64_t cm, int32_t i);
int32_t ty_field_anncount(int64_t cm, int32_t declared, int32_t i);
int64_t ty_field_annat(int64_t cm, int32_t declared, int32_t i, int32_t at);
int32_t ty_method_anncount(int64_t cm, int32_t declared, int32_t i);
int32_t ty_method_paramanncount(int64_t cm, int32_t declared, int32_t i, int32_t p);
tystr *ty_method_paramname(int64_t cm, int32_t declared, int32_t i, int32_t p);
int64_t ty_method_paramannat(int64_t cm, int32_t declared, int32_t i, int32_t p, int32_t at);
int64_t ty_method_annat(int64_t cm, int32_t declared, int32_t i, int32_t at);
int32_t ty_ctor_anncount(int64_t cm, int32_t i);
int64_t ty_ctor_annat(int64_t cm, int32_t i, int32_t at);
int32_t ty_ctor_paramanncount(int64_t cm, int32_t i, int32_t p);
tystr *ty_ctor_paramname(int64_t cm, int32_t i, int32_t p);
int64_t ty_ctor_paramannat(int64_t cm, int32_t i, int32_t p, int32_t at);

/* forName: the name is matched against the table the call site passes -- the
   program's own class names, the binary names Java uses ("teyru.List",
   "main.Outer$Inner"). clscls is the program's teyru.Class, handed over so the
   object returned is an instance of it.

   The table is an argument rather than a global because of what a global
   costs: an array naming every class pins every class, its tables and its
   interface tables into every executable, whether or not the program can ever
   reach forName. The emitter writes it inside the generated forNameOf, where
   link-time optimisation drops it together with the function. */
void *ty_class_forname_in(tystr *name, tyclass **table, int32_t count, tyclass *clscls);

int32_t ty_class_fieldcount(int64_t cm, int32_t declared);
tystr *ty_field_name(int64_t cm, int32_t declared, int32_t i);
int64_t ty_field_type(int64_t cm, int32_t declared, int32_t i);
int64_t ty_field_elem(int64_t cm, int32_t declared, int32_t i);
int64_t ty_field_owner(int64_t cm, int32_t declared, int32_t i);
int32_t ty_field_mods(int64_t cm, int32_t declared, int32_t i);
void *ty_field_get(int64_t cm, int32_t declared, int32_t i, void *self);
/* accessible is Field.setAccessible(true), which is what lets a final field be
   written, as it is in Java. */
void ty_field_set(int64_t cm, int32_t declared, int32_t i, void *self, void *v, int32_t accessible);

int32_t ty_class_methodcount(int64_t cm, int32_t declared);
tystr *ty_method_name(int64_t cm, int32_t declared, int32_t i);
int64_t ty_method_owner(int64_t cm, int32_t declared, int32_t i);
int32_t ty_method_mods(int64_t cm, int32_t declared, int32_t i);
int32_t ty_method_kind(int64_t cm, int32_t declared, int32_t i);
int64_t ty_method_ret(int64_t cm, int32_t declared, int32_t i);
int32_t ty_method_paramcount(int64_t cm, int32_t declared, int32_t i);
int64_t ty_method_param(int64_t cm, int32_t declared, int32_t i, int32_t p);
void *ty_method_invoke(int64_t cm, int32_t declared, int32_t i, void *self, tyarr *args);

int32_t ty_class_ctorcount(int64_t cm);
int32_t ty_ctor_mods(int64_t cm, int32_t i);
int32_t ty_ctor_paramcount(int64_t cm, int32_t i);
int64_t ty_ctor_param(int64_t cm, int32_t i, int32_t p);
void *ty_ctor_new(int64_t cm, int32_t i, tyarr *args);
void *ty_class_newinst(int64_t cm);

/* Argument conversion for Method.invoke and Constructor.newInstance. Java boxes
   the arguments and checks each one against the parameter's type: a null or a
   foreign box for a primitive parameter is an IllegalArgumentException, and so
   is a reference of the wrong class. The plain ty_unbox_* helpers read the box
   they are given without asking what it is, which is right for the prelude --
   it knows -- and wrong here, where the argument came from a caller that does
   not. */
int32_t ty_rv_int(void *o);
int64_t ty_rv_long(void *o);
double ty_rv_double(void *o);
float ty_rv_float(void *o);
int16_t ty_rv_short(void *o);
int8_t ty_rv_byte(void *o);
uint16_t ty_rv_char(void *o);
int32_t ty_rv_bool(void *o);
void *ty_rv_ref(void *o, tyclass *want);

/* SHA-1 of a byte array, which RFC 6455's handshake asks for. */
tyarr *ty_sha1_bytes(tyarr *data);

/* java.lang.reflect.Array */
void *ty_reflect_array_new(int64_t component, int32_t len);
int32_t ty_reflect_array_len(void *a);
void *ty_reflect_array_get(void *a, int32_t i);
void ty_reflect_array_set(void *a, int32_t i, void *v);

tystr *ty_str_ident(tystr *s);
tystr *ty_str_copy(tystr *s);
int32_t ty_str_eq_obj(tystr *s, void *o);
tystr *ty_str_sub_from(tystr *s, int32_t from);
int64_t ty_str_tolong(tystr *s);
double ty_str_todouble(tystr *s);
float ty_str_tofloat(tystr *s);
int32_t ty_str_tobool(tystr *s);
int32_t ty_abs_int(int32_t v);
int64_t ty_abs_long(int64_t v);
double ty_abs_double(double v);
float ty_abs_float(float v);
int32_t ty_max_int(int32_t a, int32_t b);
int32_t ty_min_int(int32_t a, int32_t b);
int64_t ty_max_long(int64_t a, int64_t b);
int64_t ty_min_long(int64_t a, int64_t b);
double ty_max_double(double a, double b);
double ty_min_double(double a, double b);
float ty_max_float(float a, float b);
float ty_min_float(float a, float b);
int64_t ty_round(double v);
int32_t ty_round_float(float v);
int32_t ty_floor_div_int(int32_t a, int32_t b);
int64_t ty_floor_div_long(int64_t a, int64_t b);
int32_t ty_floor_mod_int(int32_t a, int32_t b);
int64_t ty_floor_mod_long(int64_t a, int64_t b);
double ty_signum_double(double v);
float ty_signum_float(float v);
double ty_to_radians(double deg);
double ty_to_degrees(double rad);
double ty_random(void);
int32_t ty_isnan(double v);
int32_t ty_is_digit(uint16_t c);
int32_t ty_is_letter(uint16_t c);
int32_t ty_is_space(uint16_t c);
int64_t ty_millis(void);
int64_t ty_nanos(void);
void ty_exit(int32_t code);
void ty_arraycopy(void *src, int32_t spos, void *dst, int32_t dpos, int32_t len);
void *ty_illarg(const char *msg);
void *ty_illegal_state(const char *msg);
void *ty_make_ex(tyclass *c, const char *msg);
int32_t ty_obj_equal(void *a, void *b);
int32_t ty_enum_ordinal(void *o);
void *ty_enum_name(void *o);
int32_t ty_enum_compare(void *a, void *b);

typedef struct { tyobj obj; int32_t v; } tyintbox;
typedef struct { tyobj obj; int64_t v; } tylongbox;
typedef struct { tyobj obj; double v; } tydoublebox;
typedef struct { tyobj obj; float v; } tyfloatbox;
typedef struct { tyobj obj; uint16_t v; } tycharbox;
typedef struct { tyobj obj; int32_t v; } tyboolbox;
typedef struct { tyobj obj; int8_t v; } tybytebox;
typedef struct { tyobj obj; int16_t v; } tyshortbox;
/* Every box is a header and a payload of at most eight bytes, which is what
   lets the reflection path open one with a single width-sized copy
   (tyrt_reflect.c) while each wrapper class holds its value in a field of its
   own (lib/04_boxing.teyru). The compiler asserts the same shape of the classes
   it emits, field by field (emit.go's structOf), so a wrapper whose value grew
   would stop both builds here rather than at the first reflection call that
   read or wrote past the end of one. */
_Static_assert(sizeof(tyboolbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tybytebox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tyshortbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tycharbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tyintbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tylongbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tyfloatbox) == sizeof(tyobj) + 8, "a box is a header and a payload");
_Static_assert(sizeof(tydoublebox) == sizeof(tyobj) + 8, "a box is a header and a payload");
typedef struct { tyobj obj; int32_t ordinal; tystr *name; } tyEnumBase;
typedef struct { tyobj obj; int64_t len, cap; char *buf; } tySB;

void *ty_sb_new(void);
void *ty_sb_init(void *sb, int64_t cap);
void *ty_sb_append_str(void *sb, tystr *s);
void *ty_sb_append_int(void *sb, int64_t v);
void *ty_sb_append_long(void *sb, int64_t v);
void *ty_sb_append_double(void *sb, double v);
void *ty_sb_append_bool(void *sb, int32_t v);
void *ty_sb_append_obj(void *sb, void *o);
void *ty_sb_append_char(void *sb, uint16_t c);
tystr *ty_sb_tostring(void *sb);
int32_t ty_sb_len(void *sb);

/* Object methods dispatched from generated code */
tystr *ty_object_tostring(void *o);
int32_t ty_object_hash(tyobj *o);
int32_t ty_object_equals(tyobj *a, tyobj *b);

/* enum helpers */

int32_t ty_div_int(int32_t a, int32_t b);
int64_t ty_div_long(int64_t a, int64_t b);
int32_t ty_rem_int(int32_t a, int32_t b);
int64_t ty_rem_long(int64_t a, int64_t b);

/* ---------------------------------------------------------------- java.lang
 *
 * The rest of java.lang. Every helper here exists because Java's answer to a
 * question differs from C's answer to the same question: rounding at the ends
 * of a range, the order of the two zeros, the radix of a number, what counts as
 * a letter, what String.format prints. The prelude declares the method and this
 * header declares the helper it runs, so a class in lib/ and its C are two
 * halves of one function.
 */

/* Math: Java's semantics where C's differ. abs and max/min for the integer
   types are the ones tyrt2.c already had, moved here because negating the most
   negative value is undefined in C and Java defines it. */
int32_t ty_math_abs_int(int32_t v);
int64_t ty_math_abs_long(int64_t v);
float ty_abs_float(float v);
double ty_math_max_double(double a, double b);
double ty_math_min_double(double a, double b);
float ty_math_max_float(float a, float b);
float ty_math_min_float(float a, float b);
int64_t ty_math_round_long(double v);
int32_t ty_math_round_int(float v);
int32_t ty_math_floor_div_int(int32_t a, int32_t b);
int64_t ty_math_floor_div_long(int64_t a, int64_t b);
int32_t ty_math_floor_mod_int(int32_t a, int32_t b);
int64_t ty_math_floor_mod_long(int64_t a, int64_t b);
double ty_signum_double(double v);
float ty_signum_float(float v);
double ty_math_to_radians(double deg);
double ty_math_to_degrees(double rad);

/* System */
tystr *ty_getenv(tystr *name);
tystr *ty_get_property(tystr *key);
int32_t ty_in_read(void *self);
tystr *ty_in_readln(void *self);

/* Character */
int32_t ty_is_whitespace(uint16_t c);
int32_t ty_is_letter_or_digit(uint16_t c);
int32_t ty_is_upper_case(uint16_t c);
int32_t ty_is_lower_case(uint16_t c);
int32_t ty_is_alphabetic(uint16_t c);
int32_t ty_char_upper(uint16_t c);
int32_t ty_char_lower(uint16_t c);
int32_t ty_char_numeric(uint16_t c);
int32_t ty_char_digit(uint16_t c, int32_t radix);

/* The wrappers: parsing, radix formatting and the bit twiddling Integer and
   Long expose. A parse either succeeds or throws, which is why each type has a
   parsable test beside the conversion it guards. */
int32_t ty_str_parsable_int(tystr *s, int32_t radix);
int64_t ty_str_parsable_long(tystr *s, int32_t radix);
int32_t ty_str_parsable_double(tystr *s);
int32_t ty_str_parsable_float(tystr *s);
int32_t ty_str_toint_radix(tystr *s, int32_t radix);
int64_t ty_str_tolong_radix(tystr *s, int32_t radix);
tystr *ty_radix_string_int(int32_t v, int32_t radix);
tystr *ty_radix_string_long(int64_t v, int32_t radix);
tystr *ty_unsigned_string_int(int32_t v, int32_t radix);
tystr *ty_unsigned_string_long(int64_t v, int32_t radix);
float ty_str_tofloat_val(tystr *s);
double ty_str_todouble_val(tystr *s);
int32_t ty_int_bit_count(int32_t v);
int32_t ty_long_bit_count(int64_t v);
int32_t ty_int_nlz(int32_t v);
int32_t ty_int_ntz(int32_t v);
int32_t ty_long_nlz(int64_t v);
int32_t ty_long_ntz(int64_t v);
int32_t ty_int_highest_one(int32_t v);
int32_t ty_int_lowest_one(int32_t v);
int64_t ty_long_highest_one(int64_t v);
int64_t ty_long_lowest_one(int64_t v);
int32_t ty_int_reverse(int32_t v);
int32_t ty_int_reverse_bytes(int32_t v);
int64_t ty_long_reverse(int64_t v);
int64_t ty_long_reverse_bytes(int64_t v);
int32_t ty_int_rotate_left(int32_t v, int32_t d);
int32_t ty_int_rotate_right(int32_t v, int32_t d);
int64_t ty_long_rotate_left(int64_t v, int32_t d);
int64_t ty_long_rotate_right(int64_t v, int32_t d);
int32_t ty_int_signum(int32_t v);
int64_t ty_long_signum(int64_t v);
int32_t ty_int_sum(int32_t a, int32_t b);
int64_t ty_long_sum(int64_t a, int64_t b);
double ty_double_sum(double a, double b);
float ty_float_sum(float a, float b);
int32_t ty_int_cmp_unsigned(int32_t a, int32_t b);
int64_t ty_long_cmp_unsigned(int64_t a, int64_t b);
int64_t ty_double_bits(double v);
double ty_bits_double(int64_t bits);
int32_t ty_float_bits(float v);
float ty_bits_float(int32_t bits);
int32_t ty_double_is_infinite(double v);
int32_t ty_float_is_infinite(float v);
int32_t ty_double_is_finite(double v);
int32_t ty_float_is_finite(float v);
int32_t ty_float_isnan(float v);
int64_t ty_double_raw_bits(double v);
int32_t ty_float_raw_bits(float v);
double ty_math_cbrt(double x);
/* System.identityHashCode: the Object hash without dispatching to an override,
   and 0 for a null, which is what Java answers. */
int32_t ty_identity_hash(void *o);

/* String */
int32_t ty_str_cmp_ic(tystr *a, tystr *b);
int32_t ty_str_eq_ic(tystr *a, tystr *b);
int32_t ty_str_starts_from(tystr *s, tystr *p, int32_t from);
int32_t ty_str_indexof_from(tystr *s, tystr *sub, int32_t from);
int32_t ty_str_indexof_ch(tystr *s, int32_t c);
int32_t ty_str_indexof_ch_from(tystr *s, int32_t c, int32_t from);
int32_t ty_str_lastindexof(tystr *s, tystr *sub);
int32_t ty_str_lastindexof_from(tystr *s, tystr *sub, int32_t from);
int32_t ty_str_lastindexof_ch(tystr *s, int32_t c);
int32_t ty_str_lastindexof_ch_from(tystr *s, int32_t c, int32_t from);
tystr *ty_str_replace_str(tystr *s, tystr *a, tystr *b);
tystr *ty_str_repeat(tystr *s, int32_t n);
tystr *ty_str_strip(tystr *s);
tystr *ty_str_strip_leading(tystr *s);
tystr *ty_str_strip_trailing(tystr *s);
int32_t ty_str_isblank(tystr *s);
tyarr *ty_str_tochararray(tystr *s);
tyarr *ty_str_getbytes(tystr *s);
tystr *ty_str_of_chars(tyarr *chars);
tystr *ty_str_of_chars_part(tyarr *chars, int32_t off, int32_t count);
tystr *ty_str_interned(tystr *s);
/* The regex-shaped methods: Teyru has no regular expression engine, so these
   answer for a pattern that is a literal -- no metacharacter can change what it
   matches -- and fail loudly for one that is not. */
/* String.format */
tystr *ty_str_format(tystr *fmt, tyarr *args);

/* StringBuilder and StringBuffer */
void *ty_sb_insert_str(void *sb, int32_t at, tystr *s);
void *ty_sb_insert_obj(void *sb, int32_t at, void *o);
void *ty_sb_insert_int(void *sb, int32_t at, int64_t v);
void *ty_sb_insert_char(void *sb, int32_t at, uint16_t c);
void *ty_sb_insert_double(void *sb, int32_t at, double v);
void *ty_sb_insert_bool(void *sb, int32_t at, int32_t v);
void *ty_sb_insert_chars(void *sb, int32_t at, tyarr *chars);
void *ty_sb_delete(void *sb, int32_t from, int32_t to);
void *ty_sb_delete_charat(void *sb, int32_t at);
void *ty_sb_replace(void *sb, int32_t from, int32_t to, tystr *s);
void *ty_sb_reverse(void *sb);
void *ty_sb_set_charat(void *sb, int32_t at, uint16_t c);
int32_t ty_sb_charat(void *sb, int32_t at);
int32_t ty_sb_capacity(void *sb);
int32_t ty_sb_indexof(void *sb, tystr *s);
int32_t ty_sb_indexof_from(void *sb, tystr *s, int32_t from);
int32_t ty_sb_lastindexof(void *sb, tystr *s);
void *ty_sb_set_length(void *sb, int32_t n);
void *ty_sb_ensure(void *sb, int32_t cap);
tystr *ty_sb_substring(void *sb, int32_t from);
tystr *ty_sb_substring_to(void *sb, int32_t from, int32_t to);
void *ty_sb_append_float(void *sb, float v);
void *ty_sb_append_chars(void *sb, tyarr *chars);
void *ty_sb_insert_long(void *sb, int32_t at, int64_t v);
void *ty_sb_insert_float(void *sb, int32_t at, float v);
int32_t ty_sb_isempty(void *sb);
tystr *ty_str_format_arg(tyarr *args, int32_t i);

#endif /* TYRT_H */
