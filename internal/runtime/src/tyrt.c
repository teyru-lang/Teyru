/* tyrt.c - Teyru native runtime. */
#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include <float.h>
#include <stdarg.h>

#if defined(__has_feature)
#if __has_feature(address_sanitizer)
#define TY_ASAN 1
void __asan_unpoison_memory_region(void *, size_t);
#endif
#endif

/* Thread-local because a catch frame belongs to the thread that set it: two
   threads running the same generated code have two frames, and ty_throw has to
   longjmp to the one on this thread's stack. */
_Thread_local tycatch *ty_cur_catch = NULL;
tyclass *TY_STRING = NULL;
tyclass *TY_ARRAY = NULL;
tyclass *TY_BOX[9] = {0};
tyclass *TY_OBJECT = NULL;

tyclass *TY_NPE, *TY_AIOOBE, *TY_SIOOBE, *TY_ARITH, *TY_CCE, *TY_NEGARR, *TY_ASSERT;
tyclass *TY_ILLARG, *TY_ILLSTATE, *TY_NOSUCHELEM, *TY_UNSUP, *TY_ARRAYSTORE;
tyclass *TY_ILLMON;
/* StackOverflowError, which the runtime throws from a prologue check rather
   than from a call site: it is installed by the generated startup like the rest
   of them. */
tyclass *TY_SOE = NULL;

/* The lowest address a frame of this thread may have, and the whole of "no
   limit read yet". See the section of tyrt.h for what checks it and why a check
   below it throws rather than faults. */
_Thread_local char *ty_stack_limit = NULL;
/* java.lang.reflect's own exceptions. A program that never reflects never
   names one and the generated startup leaves it NULL. */
tyclass *TY_CNF, *TY_NSFE, *TY_NSME, *TY_ILLACCESS, *TY_INVOCATION,
    *TY_INSTANTIATION;

/* The class an array carries. The generated startup installs the program's own
   array class in TY_ARRAY; a program that links the runtime without that
   startup (hand-written C) would otherwise allocate arrays with no class at
   all, and every class-driven path -- instanceof, casts, string conversion and
   the collector's element tracing -- would be blind to them, which is how
   object arrays came to lose their elements. This built-in class keeps the
   runtime self-sufficient. It carries TY_CLS_ARRAY so the collector recognises
   it, and its super is bound to TY_OBJECT the first time an array is made, so a
   program that installs an Object class still gets working instanceof. The
   vtable mirrors the generated array class slot for slot (toString, hashCode,
   equals, getClass, clone) and spells toString the way it does. */
static tystr *default_array_tostring(void *o) {
  (void)o;
  return ty_str_intern("[array]");
}
static int32_t default_array_hashcode(void *o) { return ty_obj_hash(o); }
static int32_t default_array_equals(void *a, void *b) { return a == b; }
static void *default_array_getclass(void *o) { return ty_class_of(o); }
static void *default_array_clone(void *o) {
  tyarr *a = (tyarr *)o;
  return ty_array_clone(a, a->esize);
}
static void *default_array_vt[5] = {
    (void *)default_array_tostring, (void *)default_array_hashcode,
    (void *)default_array_equals, (void *)default_array_getclass,
    (void *)default_array_clone};
/* Designated rather than positional: the struct grows whenever reflection
   learns something new about a class, and a positional initializer would grow a
   silent zero in the wrong field every time. */
static tyclass default_array_cls = {
    .name = "[array]",
    .id = -1,
    .flags = TY_CLS_ARRAY,
    .nvt = 5,
    .vtable = default_array_vt,
};

static tyclass *array_class(void) {
  if (TY_ARRAY) return TY_ARRAY;
  if (!default_array_cls.super) default_array_cls.super = TY_OBJECT;
  return &default_array_cls;
}

/* ------------------------------------------------------------------ GC */

#define TY_HDR 16
#define TY_CHUNK (1u << 18)
#define TY_ALIGN 16

typedef struct tychunk {
  struct tychunk *next;
  size_t used, cap;
  char *mem;
  /* One bit per TY_ALIGN bytes of `mem`, set for every address that is the
     start of a block. Rebuilt at the beginning of each collection from the size
     words, and consulted by the root scan: a conservative stack word can point
     anywhere inside a live object, and following such an interior word as if it
     were an object traces a garbage class pointer -- and writes the mark bit
     into a live object's payload. */
  uint8_t *starts;
  /* Set by the collector when it marks an object here: whether anything in the
     slab is still live, which is what decides between walking the slab and
     handing the whole thing back to the system. The mark phase is where that is
     known, so the sweep below reads it instead of walking the blocks to find
     out. */
  uint8_t live_any;
} tychunk;

/* The slabs no thread is bumping in: a thread hands its slab here when the slab
   is exhausted and it takes a new one, and when the thread ends. Everything in
   this list can be reclaimed by a collection; a thread's private slab cannot,
   because the thread still walks it. */
static tychunk *chunks = NULL;

/* ------------------------------------------------------------------ frames */

/* The chain of frame maps this thread is inside. tyrt.h's tyframe section has
   the argument for what a map is and is not; what follows is the mechanics.

   Two frames are mapped by one function each, they are pushed and popped in
   order (one thread's stack is a stack), and the collector reads the chain of a
   stopped thread through tythread.frames, which the thread publishes at every
   stop point next to the stack pointer it parked at. */
_Thread_local tyframe *ty_frames = NULL;

/* Enter one generated frame: link its map into the chain and fill in what the
   generated code cannot compute about its own frame.

   `lo` is the frame's floor as nearly as this call can see it. This function is
   called from the frame being mapped, so its own frame lies entirely below that
   frame's own storage, and the caller's stack pointer when it called is the
   frame's floor plus whatever the call itself pushed. Both x86-64 and arm64
   build a frame record with the saved frame pointer and the return address at
   the top of the frame, which puts the caller's stack pointer at
   frame_address(0) + 16 in this frame; the eight bytes above that are the
   return address the call pushed, and they are excluded too. Erring low is the
   safe direction: an extent that reaches below the frame excludes words that
   belong to the frame below it -- the registers *it* spilled, which is where a
   value this frame holds only in a register would otherwise be found -- so the
   constant here is one pointer, not two. */
__attribute__((noinline)) void ty_frame_enter_(tyframe *f, void **slots, int32_t nslots, int32_t fno,
                                                 char *frame_addr) {
  f->prev = ty_frames;
  f->slots = slots;
  f->nslots = nslots;
  f->cap = nslots;
  f->fno = fno;
  f->lo = (char *)__builtin_frame_address(0) + sizeof(void *);
  f->hi = frame_addr + sizeof(void *);
  /* The register half of the map. A callee-saved register is the caller's
     value here, by the ABI; see tyrt.h's tyframe comment for why all of them
     are taken rather than the ones that hold a reference. */
  for (int32_t i = 0; i < TY_FRAME_REGS; i++) f->saved[i] = NULL;
#if defined(__x86_64__)
  /* Written as one store per register rather than as a local declared to live in
     a named one: a `register ... asm("rbx")` variable reads as uninitialized to
     clang and its -Wuninitialized, and there is nothing to initialize -- the
     value is the register's. A memory output says exactly what is meant. */
  __asm__ __volatile__("movq %%rbx, %0" : "=m"(f->saved[0]));
  __asm__ __volatile__("movq %%r12, %0" : "=m"(f->saved[1]));
  __asm__ __volatile__("movq %%r13, %0" : "=m"(f->saved[2]));
  __asm__ __volatile__("movq %%r14, %0" : "=m"(f->saved[3]));
  __asm__ __volatile__("movq %%r15, %0" : "=m"(f->saved[4]));
#elif defined(__aarch64__)
  __asm__ __volatile__("str x19, %0" : "=m"(f->saved[0]));
  __asm__ __volatile__("str x20, %0" : "=m"(f->saved[1]));
  __asm__ __volatile__("str x21, %0" : "=m"(f->saved[2]));
  __asm__ __volatile__("str x22, %0" : "=m"(f->saved[3]));
  __asm__ __volatile__("str x23, %0" : "=m"(f->saved[4]));
  __asm__ __volatile__("str x24, %0" : "=m"(f->saved[5]));
  __asm__ __volatile__("str x25, %0" : "=m"(f->saved[6]));
  __asm__ __volatile__("str x26, %0" : "=m"(f->saved[7]));
  __asm__ __volatile__("str x27, %0" : "=m"(f->saved[8]));
  __asm__ __volatile__("str x28, %0" : "=m"(f->saved[9]));
#endif
  ty_frames = f;
}

void ty_frame_put(tyframe *f, int32_t k, void *p) {
  if (k < 0 || k >= f->cap) {
    fprintf(stderr, "teyru: frame %d records word %d of a map of %d\n", f->fno, k, f->cap);
    abort();
  }
  f->slots[k] = p;
}

/* Leave it. Only the innermost frame may unlink itself: after a longjmp has
   rewound the chain to the landing point, the frames it dropped still run their
   epilogues as the stack unwinds under them, and each of those must leave the
   chain alone rather than relink a dead frame's predecessor. */
void ty_frame_leave(tyframe *f) {
  if (ty_frames == f) ty_frames = f->prev;
}
static void **roots_static = NULL; /* addresses of global slots */
static size_t nroots_static = 0, caproots_static = 0;
int64_t ty_gc_threshold = 4 << 20;
static int64_t live_bytes = 0;
static int gc_disabled = 0;

/* The two diagnostics switches, read once from the environment by ty_gc_init.

   TEYRU_GC_STRESS=N makes every Nth allocation collect, so that a program which
   would reach its budget only after a great deal of work is exercised through
   collections anyway; it is the setting the thread and collection tests are run
   under. N is a count of allocations, not of bytes, because "every 100
   allocations" is what a test can state about a shape it controls.

   TEYRU_GCTRACE=1 prints one line per collection: what asked for it, how long
   the world was stopped for, and the heap's size before and after with the live
   set in between.

   Both are off by default, and with both off every use of them below is one test
   of a static zero. */
static int64_t gc_stress_every = 0; /* 0: the byte budget, as always */
static int64_t gc_stress_count = 0;
static int gc_trace = 0;
/* TEYRU_GC_VERIFY=1 reads the words inside every mapped frame that the map does
   not name and reports each one that holds a live object. See verify_frame. */
static int gc_verify = 0;
/* TEYRU_GC_NOEXCL=1 keeps the conservative pass over the whole range: the
   diagnostic that says whether a difference the maps make is what broke a
   program. */
static int gc_noexcl = 0;

/* The slabs one collection walks: the shared list above, plus every registered
   thread's private slab (tyrt_thread.c). It is rebuilt at the start of each
   collection, because a thread's slab is only walkable while that thread is
   stopped and its watermark exact, and because the set changes as threads come
   and go. Both the block-start pass and the sweep iterate it, and so does
   valid_obj -- an object in a thread's private slab is as much an object as one
   in a shared slab, and a scan that only knew about the shared list would treat
   a pointer to it as garbage and free what it points at. */
static tychunk **gc_slabs = NULL;
static size_t gc_nslabs = 0, gc_capslabs = 0;
/* How many of the slabs above are shared (the first gc_nshared of them) and how
   many are a thread's private slab. Only a shared slab may be released
   wholesale. */
static size_t gc_nshared = 0;
/* The address range those slabs lie in: the lowest byte of the lowest slab and
   the number of bytes up to the highest byte of the highest one, rebuilt by
   collect_slabs together with the list. valid_obj tests a candidate word against
   it before it walks the list, because most of the words a conservative scan
   reads name something that is not in the heap at all -- a class descriptor, an
   address on a C stack, a small integer -- and without the test each one of them
   costs a walk of every slab. It is a filter and not a decision: a word inside
   the range is still tested against every slab one at a time, so every answer is
   the one the walk would have given. */
static char *gc_heap_lo = NULL;
static size_t gc_heap_span = 0;

/* The heap lock: held by a thread while it refills its slab, and by the
   collector for the whole of a collection. ty_heap_lock counts the caller as
   stopped while it waits, which is what keeps a collector holding this lock from
   waiting for the threads that want it. */
static typlat_mutex heap_mtx = TYPLAT_MUTEX_INITIALIZER;

/* Where a scan of a stopped thread starts, below the point the thread recorded
   when it stopped. The frame that blocks belongs to the platform layer's own
   wait (typlat_cond_wait, typlat_sleep_ns) and its saved registers are just
   below that point; none of them can be found by arithmetic, so a fixed margin
   is scanned conservatively instead. The words in it are stale stack words like
   the thousands the scan already walks, so the cost is a little retention and
   the alternative -- missing a register that holds the only reference to a live
   object -- is a crash. The scan never starts below the thread's own stack
   base. */
#define TY_PARK_MARGIN 1024

/* Free lists, declared here because the collector rebuilds them. */
#define TY_NCLASS 64
static void *freelist[TY_NCLASS];
static void *bigfree = NULL;
void ty_free_block(void *payload, size_t total);
/* The per-thread batch in tythread is indexed by size class, so the two class
   counts have to be the same number: a table smaller than the free lists would
   be written out of bounds by the class of the block being handed out. */
_Static_assert(TY_BATCH_CLASSES == TY_NCLASS, "the batch table is indexed by size class");

#define TY_MARK_BIT 1u
/* The high bit of the size word marks a block that is on a free list. The
   collector must not read the mark word of a free block -- that word holds the
   free list link -- and a stale pointer into a freed block must not be mistaken
   for an object, so the bit is checked before anything else. */
#define TY_FREE_BIT (1ull << 63)
#define TY_SIZE_MASK (~TY_FREE_BIT)

static inline uint64_t *hdr_of(void *obj) { return (uint64_t *)((char *)obj - TY_HDR); }

void ty_gc_register_static(void *p) {
  if (nroots_static == caproots_static) {
    caproots_static = caproots_static ? caproots_static * 2 : 64;
    roots_static = (void **)realloc(roots_static, caproots_static * sizeof(void *));
  }
  roots_static[nroots_static++] = p;
}

/* The inlined fast path in tyrt.h bumps a pointer and nothing else, so the
   thread's slab watermark trails behind it. Every reader of `used` has to see
   the true watermark first: a collection walks a stopped thread's slab by it,
   and the sweep must not stop below live objects or start inside one.

   Only the thread that owns a slab writes its watermark, so this needs no lock;
   the collector reads it only while the owner is stopped, and the handshake that
   stopped the thread is what orders the two. */
void ty_heap_sync(void) {
  tychunk *c = (tychunk *)ty_self->chunk;
  if (c) c->used = (size_t)(ty_self->bump - c->mem);
}

void ty_heap_lock(void) {
  /* Waiting for the heap is waiting: a thread that has not got the lock yet has
     done nothing to the heap, and a collection must not wait for it -- the
     collector is holding this very lock. */
  ty_thread_stopped_begin();
  typlat_mutex_lock(&heap_mtx);
}

void ty_heap_unlock(void) {
  typlat_mutex_unlock(&heap_mtx);
  ty_thread_stopped_end();
}

/* Hand this thread's slab to the shared heap. Called with the slab's watermark
   already exact, and with the thread stopped for a collection if one is running,
   so that a slab is never walked twice: the collector walks it through the
   registry while the thread owns it, and through the shared list afterwards. */
void ty_heap_detach(void) {
  ty_heap_lock();
  tychunk *c = (tychunk *)ty_self->chunk;
  if (c) {
    ty_heap_sync();
    c->next = chunks;
    chunks = c;
    ty_self->chunk = NULL;
    ty_self->bump = NULL;
    ty_self->bump_end = NULL;
  }
  ty_heap_unlock();
}

static tychunk *valid_obj_last = NULL;

/* valid_obj_forget drops the last-chunk cache: the sweep frees whole chunks,
   and a stale pointer would be a use after free. */
static void valid_obj_forget(void) { valid_obj_last = NULL; }

/* obj_in_chunk answers for one chunk: 1 valid object, 0 not an object, -1 the
   address is not in this chunk at all. */
static int obj_in_chunk(tychunk *c, char *p) {
  if (p < c->mem || p + TY_HDR > c->mem + c->used) return -1;
  /* The map is indexed by block header, and `p` is a payload, so step back a
     header first. A p that lies below the chunk's first payload wraps the
     index and is rejected by the bound below. */
  size_t i = (size_t)(p - TY_HDR - c->mem) / TY_ALIGN;
  if (i >= c->cap / TY_ALIGN) return -1;
  if (!(c->starts[i >> 3] & (uint8_t)(1u << (i & 7)))) return 0; /* interior word */
  uint64_t sz = *(uint64_t *)(p - TY_HDR);
  if (sz & TY_FREE_BIT) return 0; /* freed: the payload is a free list link */
  return sz >= TY_HDR;
}

/* `slab`, when not NULL, receives the slab the answer came from: the collector
   charges the object to it. It is written only when the answer is 1, so a caller
   that reads it has a real object in hand. */
static int valid_obj(char *p, tychunk **slab) {
  if (((uintptr_t)p) & (TY_ALIGN - 1)) return 0;
  /* Outside the range the slabs cover: no slab can hold this word, so the walk
     below would answer 0 for it anyway. Most of what a conservative scan reads
     lands here -- a class pointer, an address on a C stack, a small integer --
     and one word lands here once per traced object: an object whose class was
     emitted with a reference offset of 0, whose word at offset 0 is its own
     class pointer. */
  if ((uintptr_t)p - (uintptr_t)gc_heap_lo >= gc_heap_span) return 0;
  /* A collection tests thousands of candidate words, and they are almost always
     in the slab the previous one was in, so the last slab that answered is tried
     first: walking every slab every time makes a collection quadratic in the
     number of slabs. ty_gc clears the cache, because the sweep can free the slab
     it points at. */
  if (valid_obj_last) {
    int r = obj_in_chunk(valid_obj_last, p);
    if (r > 0 && slab) *slab = valid_obj_last;
    if (r >= 0) return r;
  }
  for (size_t i = 0; i < gc_nslabs; i++) {
    tychunk *c = gc_slabs[i];
    if (c == valid_obj_last) continue;
    int r = obj_in_chunk(c, p);
    if (r >= 0) {
      valid_obj_last = c;
      if (r > 0 && slab) *slab = c;
      return r;
    }
  }
  return 0;
}

#define TY_START_BYTES(cap) (((cap) / TY_ALIGN + 7) / 8)

static void mark_block_start(tychunk *c, char *p) {
  size_t i = (size_t)(p - c->mem) / TY_ALIGN;
  if (i < c->cap / TY_ALIGN) c->starts[i >> 3] |= (uint8_t)(1u << (i & 7));
}

/* Rebuilds the block-start map by walking every slab's size words. The walk uses
   the same rule as the sweep, so the two passes agree on where blocks begin. */
static void build_block_starts(void) {
  for (size_t i = 0; i < gc_nslabs; i++) {
    tychunk *c = gc_slabs[i];
    memset(c->starts, 0, TY_START_BYTES(c->cap));
    for (char *p = c->mem; p < c->mem + c->used;) {
      int64_t sz = (int64_t)(*(uint64_t *)p & TY_SIZE_MASK);
      if (sz < (int64_t)TY_HDR) break;
      mark_block_start(c, p);
      p += sz;
    }
  }
}

static void *mark_stack[1 << 20];
static size_t mark_sp = 0;

static void trace_object(void *obj);

static void mark_value(void *p) {
  if (!p) return;
  tychunk *slab = NULL;
  if (!valid_obj((char *)p, &slab)) return;
  uint64_t *h = hdr_of(p);
  if (h[1] & TY_MARK_BIT) return;
  h[1] |= TY_MARK_BIT;
  /* The collector's own bookkeeping, kept where the one pass that sees every
     live object is: which slabs hold something live -- what decides whether a
     slab is walked or handed back -- and how many bytes are live, which is what
     the next collection's budget is derived from. Deriving the same two answers
     from the heap afterwards means walking every block of every slab a second
     time. */
  slab->live_any = 1;
  live_bytes += (int64_t)(h[0] & TY_SIZE_MASK);
  if (mark_sp < (1 << 20)) mark_stack[mark_sp++] = p;
}

/* Whether a class describes an array. Class identity is the primary test because
   the emitted array class is registered without the array flag; the flag covers
   the built-in class the runtime installs and anything that sets it. Without one
   of the two, an array is traced as a plain object: its payload is not a set of
   reference fields, so its elements are never marked and a live array hands its
   elements to the sweep. */
static int is_array_class(tyclass *c) {
  return c && ((c->flags & TY_CLS_ARRAY) || c == TY_ARRAY);
}

static void trace_object(void *obj) {
  tyclass *c = ((tyobj *)obj)->cls;
  if (is_array_class(c)) { /* array */
    tyarr *a = (tyarr *)obj;
    if (a->refs) {
      void **d = (void **)a->data;
      for (int64_t i = 0; i < a->len; i++) mark_value(d[i]);
    }
    return;
  }
  if (c && c->nref) {
    for (int32_t i = 0; i < c->nref; i++) {
      mark_value(*(void **)((char *)obj + c->refoffs[i]));
    }
  }
}

/* Builds the list of slabs this collection walks: the shared ones, then the
   private slab of every registered thread. Called once the world is stopped, so
   no thread can take a slab, hand one back, or allocate into one while this
   runs. */
static void collect_slabs(void) {
  /* Every thread's batch of free blocks goes back to the heap here, before the
     sweep below rebuilds the free lists. A batch holds blocks whose size word
     still says "free" -- that is what filing them on a list sets -- so the
     sweep re-links every one of them, and a thread left holding a link into the
     list the sweep rewrote would hand the same block out twice. The world is
     stopped, so no thread is popping from its own table while it is emptied. */
  for (tythread *t = ty_thread_list(); t; t = t->next) {
    memset(t->batch, 0, sizeof t->batch);
  }
  gc_nslabs = 0;
  for (tychunk *c = chunks; c; c = c->next) {
    if (gc_nslabs == gc_capslabs) {
      gc_capslabs = gc_capslabs ? gc_capslabs * 2 : 16;
      gc_slabs = (tychunk **)realloc(gc_slabs, gc_capslabs * sizeof(tychunk *));
    }
    gc_slabs[gc_nslabs++] = c;
  }
  gc_nshared = gc_nslabs;
  for (tythread *t = ty_thread_list(); t; t = t->next) {
    if (!t->chunk) continue;
    if (gc_nslabs == gc_capslabs) {
      gc_capslabs = gc_capslabs ? gc_capslabs * 2 : 16;
      gc_slabs = (tychunk **)realloc(gc_slabs, gc_capslabs * sizeof(tychunk *));
    }
    gc_slabs[gc_nslabs++] = (tychunk *)t->chunk;
  }
  /* The range the filter in valid_obj tests against, over the same slabs. A
     collection with no slab at all leaves it empty, and then nothing is an
     object, which is also what the walk would say. */
  char *lo = NULL, *hi = NULL;
  for (size_t i = 0; i < gc_nslabs; i++) {
    tychunk *c = gc_slabs[i];
    /* Nothing is marked in any slab yet. The sweep of the previous collection
       cleared the mark bits, and this is what stands in for them while this one
       runs. */
    c->live_any = 0;
    if (!lo || c->mem < lo) lo = c->mem;
    if (!hi || c->mem + c->cap > hi) hi = c->mem + c->cap;
  }
  gc_heap_lo = lo;
  gc_heap_span = hi ? (size_t)(hi - lo) : 0;
}

/* How much of a collection's root scan was precise and how much was
   conservative: frames walked, words read out of a frame map, words read by the
   conservative pass. Only TEYRU_GCTRACE reads them. */
static int64_t gc_frames_walked = 0, gc_precise_words = 0, gc_conserv_words = 0;
/* The range this collection scans: set before the walk, read by frame_map_ok. */
static char *gc_scan_lo = NULL, *gc_scan_hi = NULL;

/* Scans one thread's stack conservatively, from where that thread stopped (or
   from this frame, for the thread that is collecting) up to the top of its
   stack. `lo` is a lower bound on where the thread's live frames begin, and
   everything between the two is read as if it were a pointer: a word that names
   an object keeps it, a word that names anything else is ignored. That is the
   conservative half of a conservative collector, and it is why an object stays
   alive for as long as any word of any stopped thread's stack happens to look
   like its address.

   What the frame maps changed is *where* this runs. It used to be the whole
   range, generated frames included; now it is the complement of the mapped
   extents, so the words inside a mapped frame that the map does not name are
   left alone. Everything that is not a generated frame's own storage -- the
   runtime's C frames, the platform's wait frames, the register spills a stop
   point writes, the prologue above the outermost generated frame, and every
   generated frame that emits no map -- is still read this way, which is what
   keeps the runtime's own discipline exactly as it was. */
static void scan_stack(char *lo, char *hi) {
  /* The scan steps a pointer at a time, so it has to start on a pointer
     boundary: a word read from a misaligned address is a value shifted by a few
     bits, which is not a pointer to anything, and a scan that starts at an
     unaligned stack address finds none of the references it is looking for.
     park_sp in particular is the address of a `char`, so it is unaligned by
     construction. Rounding down covers a byte or two more, which costs
     nothing. */
  lo = (char *)((uintptr_t)lo & ~(uintptr_t)(sizeof(void *) - 1));
  if (hi > lo) {
#ifdef TY_ASAN
    __asan_unpoison_memory_region(lo, (size_t)(hi - lo));
#endif
  }
  for (char *q = lo; q + sizeof(void *) <= hi; q += sizeof(void *)) {
    mark_value(*(void **)q);
    if (gc_trace) gc_conserv_words++;
  }
}

/* Whether a frame record and the array it names are consistent. */
static int frame_map_ok(tyframe *f, char *lo, char *hi) {
  if (f->nslots < 0 || f->nslots > (1 << 16)) return 0;
  if (f->nslots == 0) return 1;
  if (!f->slots) return 0;
  /* The words are the frame's own storage and the array that names them is too,
     so both lie on the thread's stack -- between the bottom of the range being
     scanned and the top of the thread's stack. A parameter is the one word that
     is not: the ABI hands the callee an argument in the *caller's* outgoing
     area, below the frame's own floor and so below the frame's extent, which is
     why the test is against the stack and not against the extent. */
  uintptr_t s = (uintptr_t)f->slots, e = s + (uintptr_t)f->nslots * sizeof(void *);
  if (s < (uintptr_t)lo || e > (uintptr_t)hi) return 0;
  for (int32_t i = 0; i < f->nslots; i++) {
    uintptr_t p = (uintptr_t)f->slots[i];
    if (p && (p < (uintptr_t)lo || p >= (uintptr_t)hi)) return 0;
  }
  return 1;
}

/* Marks one mapped frame: the words its map names, and the callee-saved
   registers the ABI handed it at entry. Precise in the sense that matters --
   every word here is one the compiler says holds a reference, or a register
   whose value a reference could be in -- and it is the whole of what a mapped
   frame contributes to the root set. Its other words are neither read nor
   needed: the runtime's other frames are, and they are outside the extent. */
static void mark_frame(tyframe *f) {
  /* This frame's words are a contiguous array inside the frame, so a record
     whose array is not inside the frame it names is not believed: it is one a
     frame left in the chain without unlinking itself, whose storage has since
     been reused. Nothing is lost by dropping it -- the words the map does not
     describe are the conservative pass's, and a frame that cannot be described
     is simply one the pass covers. */
  if (!frame_map_ok(f, gc_scan_lo, gc_scan_hi)) {
    if (gc_trace) fprintf(stderr, "teyru gc: frame %d has no usable map (n=%d slots=%p lo=%p hi=%p)\n", f->fno, f->nslots, (void*)f->slots, (void*)f->lo, (void*)f->hi);
    return;
  }
  if (gc_trace)
    fprintf(stderr, "  frame %d n=%d slots=%p lo=%p hi=%p\n", f->fno, f->nslots, (void *)f->slots,
            (void *)f->lo, (void *)f->hi);
  for (int32_t i = 0; i < f->nslots; i++) {
    /* A word the prologue declared and no registration has filled yet: the
       declaration it stands for is one this execution has not reached -- inside
       a branch, or a loop body, or after the point that is collecting -- and a
       frame's map is walked from the moment the frame is entered, so those
       words are zero, and zero means there is no value yet rather than a value
       at address zero. */
    if (!f->slots[i]) continue;
    mark_value(*(void **)f->slots[i]);
    if (gc_trace) gc_precise_words++;
  }
  for (int32_t i = 0; i < TY_FRAME_REGS; i++) {
    mark_value(f->saved[i]);
    if (gc_trace) gc_precise_words++;
  }
  if (gc_trace) gc_frames_walked++;
  if (gc_trace) gc_frames_walked++;
}

/* Whether a frame's recorded extent may be used to exclude words from the
   conservative pass. A record whose bounds are inverted, outside the range
   being scanned, or empty is ignored: the frame's words are then scanned the
   old way, which is over-approximation and never a missing root. */
static int extent_usable(tyframe *f, char *lo, char *hi) {
  char *flo = (char *)f->lo, *fhi = (char *)f->hi;
  if (!flo || !fhi) return 0;
  if ((uintptr_t)flo >= (uintptr_t)fhi) return 0;
  if ((uintptr_t)flo < (uintptr_t)lo || (uintptr_t)fhi > (uintptr_t)hi) return 0;
  if (((uintptr_t)fhi - (uintptr_t)flo) > (16u << 20)) return 0;
  return 1;
}

/* The conservative pass over one thread: everything in [lo, hi) that no mapped
   frame covers. The chain runs from the innermost frame outwards, so the frames
   are in increasing address order and the gaps between them are walked once, in
   order, by a cursor that only moves up.

   A frame the cursor has already passed -- one entirely below the point reached
   so far, which a chain of records from two different stack depths would give --
   is left alone rather than allowed to move the cursor back, so no word is
   scanned twice and none is skipped. */
static void scan_unmapped(tyframe *chain, char *lo, char *hi) {
  char *cur = lo;
  for (tyframe *f = chain; f; f = f->prev) {
    if (!extent_usable(f, lo, hi)) continue;
    char *flo = (char *)f->lo, *fhi = (char *)f->hi;
    if ((uintptr_t)flo > (uintptr_t)cur) scan_stack(cur, flo);
    if ((uintptr_t)fhi > (uintptr_t)cur) cur = fhi;
  }
  if ((uintptr_t)cur < (uintptr_t)hi) scan_stack(cur, hi);
}

/* What the maps do not cover, reported rather than ignored: TEYRU_GC_VERIFY=1
   makes a collection read the words inside every mapped frame that the map does
   not name and print each one that names a live object. It is the missing-root
   check. A word there that names an object is one of two things -- a value the
   compiler kept somewhere the map cannot name (a spill slot of its own, a
   callee-saved register saved by a frame between two mapped ones), which is a
   root the precise pass did not take and therefore a use-after-free waiting to
   happen, or a stale word whose object is live anyway, which is only
   retention -- and the two are the same word as far as this can tell. What the
   report is for is that the first kind cannot appear quietly: the second kind
   repeats the same object every collection and disappears when the map grows a
   slot for it. */
static void verify_frame(tyframe *f) {
  if (!extent_usable(f, f->lo, f->hi)) return;
  char *lo = (char *)((uintptr_t)f->lo & ~(uintptr_t)(sizeof(void *) - 1));
  char *hi = (char *)f->hi;
  for (char *q = lo; q + sizeof(void *) <= hi; q += sizeof(void *)) {
    int named = 0;
    for (int32_t i = 0; i < f->nslots; i++) {
      if ((char *)f->slots[i] == q) {
        named = 1;
        break;
      }
    }
    if (named) continue;
    void *v = *(void **)q;
    if (v && valid_obj((char *)v, NULL)) {
      fprintf(stderr, "teyru gc: verify: frame %d word +%ld holds %p, which no map names\n", f->fno,
              (long)(q - lo), v);
    }
  }
}

/* The heap's size in bytes: every chunk's watermark, the shared ones and the
   private slab of every registered thread. Read with the world stopped, which
   is the only time the watermarks are exact and the set cannot change, and from
   the chunk lists rather than from gc_slabs -- the sweep releases chunks that
   gc_slabs still names, and reading a released chunk's watermark is reading
   freed memory. Only TEYRU_GCTRACE calls this. */
static size_t heap_used(void) {
  size_t n = 0;
  for (tychunk *c = chunks; c; c = c->next) n += c->used;
  for (tythread *t = ty_thread_list(); t; t = t->next) {
    if (t->chunk) n += ((tychunk *)t->chunk)->used;
  }
  return n;
}

/* One collection, with `trigger` naming what asked for it: the budget, the
   stress switch, or a program calling System.gc. The name is read only by
   TEYRU_GCTRACE. */
static void gc_collect(const char *trigger) {
  if (gc_disabled) return;
  int64_t started = gc_trace ? typlat_monotonic_ns() : 0;
  size_t before = 0;
  /* This thread's own slab, before anything reads a watermark. */
  ty_heap_sync();
  /* Everything else stops here. A thread that is mid-allocation cannot be: it
     would be holding the heap lock, which is held right now by this thread. */
  ty_gc_stop_world();
  collect_slabs();
  if (gc_trace) before = heap_used();
  build_block_starts();
  mark_sp = 0;
  /* Counted by mark_value, the one place that sees every live object exactly
     once. */
  live_bytes = 0;
  gc_frames_walked = gc_precise_words = gc_conserv_words = 0;
  /* roots: every thread's shadow stack, and its stack and registers */
  for (tythread *t = ty_thread_list(); t; t = t->next) {
    if (t->state == TY_TH_DONE) continue; /* its stack is going away with it */
    for (int64_t i = 0; i < t->sp; i++) mark_value(t->roots[i]);
    /* The thread's preallocated StackOverflowError. It is reachable from
       nowhere else -- the thread holds the only reference, in a struct the
       collector does not scan -- and a collection that swept it would leave the
       thread's next overflow throwing a freed object. */
    mark_value(t->soe);
    if (t == ty_self) continue;
    if (!t->park_sp) continue; /* it has not stopped yet: ty_gc_stop_world would not have returned */
    char *hi = t->stack_top ? t->stack_top : t->park_sp + 0x10000;
    char *lo = t->park_sp - TY_PARK_MARGIN;
    if (t->stack_base && lo < t->stack_base) lo = t->stack_base;
    gc_scan_lo = lo;
    gc_scan_hi = hi;
    /* Its frames, precisely: the map of every generated frame it is inside, with
       the registers each of them was entered with. `t->frames` is what the
       thread published the last time it stopped, which is now -- it is stopped
       at a stop point, and the chain it published there is the chain it has. */
    for (tyframe *f = t->frames; f; f = f->prev) mark_frame(f);
    /* And everything the maps do not cover, conservatively: the runtime's own
       frames, the platform's wait frames, the register spills of the stop point,
       and any generated frame that emitted no map. */
    if (gc_noexcl) scan_stack(lo, hi); else scan_unmapped(t->frames, lo, hi);
    if (gc_verify) {
      for (tyframe *f = t->frames; f; f = f->prev) verify_frame(f);
    }
  }
  /* roots: registered globals */
  for (size_t i = 0; i < nroots_static; i++) {
    if (roots_static[i]) mark_value(*(void **)roots_static[i]);
  }
  /* roots: this thread's native stack and registers (conservative). The
     registers are the ones setjmp saves; on another thread they were spilled by
     __builtin_unwind_init at the point it stopped. */
  jmp_buf regs;
  setjmp(regs);
  char *sp = (char *)&regs;
  char *hi = ty_self->stack_top ? ty_self->stack_top : sp + 0x10000;
  gc_scan_lo = sp;
  gc_scan_hi = hi;
  for (tyframe *f = ty_frames; f; f = f->prev) mark_frame(f);
  if (gc_noexcl) scan_stack(sp, hi); else scan_unmapped(ty_frames, sp, hi);
  if (gc_verify) {
    for (tyframe *f = ty_frames; f; f = f->prev) verify_frame(f);
  }
  /* trace */
  while (mark_sp) {
    void *o = mark_stack[--mark_sp];
    trace_object(o);
  }
  /* Sweep. Free lists are rebuilt from scratch so that blocks belonging to a
     reclaimed chunk can never be handed out again. A chunk with no live object
     is returned to the system instead of being walked on every later cycle;
     this keeps the cost of a collection proportional to live data rather than
     to everything ever allocated. Which chunks those are was settled while
     marking -- the mark bit and the chunk's own flag are set together -- so this
     pass reads one flag per chunk and never walks a chunk it is going to
     release.
     A thread's private slab is walked like any other, but never released: the
     thread is still bumping through it, and it will hand it back itself once it
     has filled it. */
  for (int i = 0; i < TY_NCLASS; i++) freelist[i] = NULL;
  bigfree = NULL;
  for (size_t si = 0; si < gc_nslabs; si++) {
    tychunk *c = gc_slabs[si];
    /* the first gc_nshared entries are the shared list (collect_slabs) */
    int32_t shared = si < gc_nshared;
    if (shared && !c->live_any && c->used > 0) {
      for (tychunk **pp = &chunks; *pp; pp = &(*pp)->next) {
        if (*pp == c) {
          *pp = c->next;
          break;
        }
      }
      free(c->mem);
      free(c->starts);
      free(c);
      continue;
    }
    for (char *p = c->mem; p < c->mem + c->used;) {
      uint64_t raw = *(uint64_t *)p;
      int64_t sz = (int64_t)(raw & TY_SIZE_MASK);
      if (sz < (int64_t)TY_HDR) break;
      if (raw & TY_FREE_BIT) {
        /* The lists above were emptied, so a block that was already free has no
           list to be on any more: link it back in. Leaving it alone made every
           block freed before this collection unreachable to the allocator for
           the rest of the program, so demand the free lists could have served
           grew the heap by a fresh chunk instead. The blocks of a chunk that is
           released wholesale above never reach here, so nothing is linked into
           memory that is about to be freed. */
        ty_free_block(p + TY_HDR, (size_t)sz);
      } else if (*(uint64_t *)(p + 8) & TY_MARK_BIT) {
        *(uint64_t *)(p + 8) &= ~(uint64_t)TY_MARK_BIT;
      } else {
        ty_free_block(p + TY_HDR, (size_t)sz);
      }
      p += sz;
    }
  }
  valid_obj_forget();
  /* Every thread's budget starts again: nothing allocated before this point is
     due to be collected again until the new threshold is reached. The threads
     are stopped, so their counters are this thread's to reset. */
  for (tythread *t = ty_thread_list(); t; t = t->next) t->alloc_since = 0;
  if (gc_stress_every) {
    /* Under the stress switch the budget is not the trigger: -1 keeps the fast
       path handing every allocation to ty_alloc_slow, which counts them. */
    ty_gc_threshold = -1;
  } else {
    ty_gc_threshold = live_bytes * 2;
    if (ty_gc_threshold < (4 << 20)) ty_gc_threshold = 4 << 20;
  }
  size_t after = gc_trace ? heap_used() : 0;
  int64_t stopped_for = gc_trace ? typlat_monotonic_ns() - started : 0;
  ty_gc_resume_world();
  if (gc_trace) {
    /* After the world is running again: printing inside the stop would charge
       every other thread for the time it takes to write the line, and the pause
       the line reports would then include the writing of it. */
    fprintf(stderr,
            "teyru gc: trigger=%s pause=%.3fms heap=%llukB->%llukB live=%llukB"
            " frames=%lld precise=%lld conservative=%lld\n",
            trigger, (double)stopped_for / 1e6, (unsigned long long)(before / 1024),
            (unsigned long long)(after / 1024), (unsigned long long)(live_bytes / 1024),
            (long long)gc_frames_walked, (long long)gc_precise_words, (long long)gc_conserv_words);
  }
}

void ty_gc_locked(void) { gc_collect("budget"); }

void ty_gc(void) {
  ty_heap_lock();
  gc_collect("explicit");
  ty_heap_unlock();
}

/* The live set as of the last collection, in bytes. System.liveBytes() is the
   program's view of it, and the tests that check what a collection kept use it:
   the number is the collector's own accounting -- the sizes of the blocks the
   mark phase reached -- and not a measure taken around it, so "this object is
   being kept alive" is a statement the collector makes rather than one a
   program infers from the heap's size. Before the first collection it is 0,
   which is also what an empty live set reports: a program that asks has to
   collect first, and System.gc() is what does it. */
int64_t ty_gc_live_bytes(void) { return live_bytes; }

/* ------------------------------------------------------------------ allocation */

static int size_class(size_t sz) {
  size_t i = (sz + TY_ALIGN - 1) / TY_ALIGN;
  return i < TY_NCLASS ? (int)i : -1;
}

void ty_free_block(void *payload, size_t total) {
  int k = size_class(total);
  void *p = (char *)payload - TY_HDR;
  /* the free list link lives in the second header word, so the block is marked
     free first: the collector reads that bit before the link */
  *(uint64_t *)p = (uint64_t)total | TY_FREE_BIT;
  if (k >= 0) {
    *(void **)((char *)p + 8) = freelist[k];
    freelist[k] = p;
  } else {
    *(void **)((char *)p + 8) = bigfree;
    bigfree = p;
  }
}

/* A block of `total` bytes from the free lists, or NULL when they hold none
   that fits. */
static void *take_free(size_t total) {
  int k = size_class(total);
  if (k >= 0) {
    if (freelist[k]) {
      void *p = freelist[k];
      freelist[k] = *(void **)((char *)p + 8);
      return p;
    }
  } else {
    void **pp = &bigfree;
    while (*pp) {
      void *p = *pp;
      uint64_t sz = *(uint64_t *)p & TY_SIZE_MASK;
      if (sz >= total) {
        *pp = *(void **)((char *)p + 8);
        /* The caller rewrites this block's size word to `total`. A larger block
           reused for a smaller request has to give its tail back here, or the
           tail becomes a hole the chunk walk reads as a payload-sized header --
           it would then free an interior address and the next allocation could
           land inside a live object. Both sizes are 16-aligned, so the tail is
           never smaller than a header. */
        if (sz > total) ty_free_block((char *)p + total + TY_HDR, (size_t)(sz - total));
        return p;
      }
      pp = (void **)((char *)p + 8);
    }
  }
  return NULL;
}

/* Up to TY_BATCH blocks of size class k, taken out of the heap's free list into
   this thread's own list, and the first of them handed back to the caller. The
   caller holds the heap lock, so the batch is taken in one piece and paid for
   once.

   Every block in freelist[k] has size k * TY_ALIGN -- ty_free_block filed it by
   the same rounding size_class does -- so the block handed out here is exactly
   the size the request asked for, with no tail to give back and no size word to
   change beyond writing the class back. */
static void *take_batch(int k) {
  tythread *me = ty_self;
  int n = 0;
  while (n < TY_BATCH && freelist[k]) {
    void *p = freelist[k];
    freelist[k] = *(void **)((char *)p + 8);
    *(void **)((char *)p + 8) = me->batch[k];
    me->batch[k] = p;
    n++;
  }
  if (!n) return NULL;
  void *p = me->batch[k];
  me->batch[k] = *(void **)((char *)p + 8);
  return p;
}

/* A fresh slab of at least `total` usable bytes, owned by the calling thread. */
static tychunk *chunk_new(size_t total) {
    size_t cap = total > TY_CHUNK ? ((total + TY_CHUNK - 1) & ~(size_t)(TY_CHUNK - 1)) : TY_CHUNK;
  tychunk *c = (tychunk *)malloc(sizeof(tychunk));
    c->mem = (char *)malloc(cap);
    c->starts = (uint8_t *)calloc(TY_START_BYTES(cap), 1);
    c->cap = cap;
    c->used = 0;
  c->next = NULL;
  c->live_any = 0;
  if (!c->mem || !c->starts) abort();
  return c;
}

/* The block at p handed to the caller: its size word, the cleared mark word, and
   the zeroed payload every Teyru object is promised. */
static void *block_object(char *p, size_t total) {
  *(uint64_t *)p = (uint64_t)total;
  *(uint64_t *)((char *)p + 8) = 0;
  void *obj = p + TY_HDR;
  memset(obj, 0, total - TY_HDR);
  return obj;
}

/* Builds the object at `p` and holds it on this thread's shadow stack until the
   caller has handed it out. Called with the heap lock held, for a block that
   came from memory a collection can already see -- off a free list, or the
   first block of the slab just taken.

   Why it is needed. This thread counts as stopped until ty_heap_unlock's
   stopped_end returns -- that is what keeps a collector holding the heap lock
   from waiting for a thread that is only waiting for that lock -- so a
   collection can start in the window between taking the block and handing it
   out, walk the block's slab, read exactly what the block still says (a free
   block, or a size word malloc never wrote), and take it for free space: it
   re-links the block onto the free list, or releases the whole slab it is in,
   and the same memory is handed out twice -- or handed out after it has gone
   back to the system. A four-thread allocation stress reproduced that as a
   SIGSEGV inside block_object's memset, always in a slab the same collection
   had just released.

   Why the header and the class word are written first. Marking an object is not
   the end of what the collector does with it: a marked object is traced, and a
   block just taken off a free list still holds its previous tenant's bytes, so
   a root that was pushed before those bytes were gone would have the collector
   read that dead object's class pointer and fields as if they were this one's
   -- and for a block whose last tenant was an array, its stale element pointer
   and length, which is a walk of memory that may not be mapped at all. The
   class word is the only part of the payload anything reads before the caller
   sets it, so writing it zero is what makes tracing this object a no-op, and it
   is written here; the rest of the payload is zeroed by the caller after the
   lock is dropped, so that a large array is not zeroed under the heap lock.

   A block the bump path hands out needs none of this, and must not be treated
   as if it did: it lies beyond the watermark the thread's slab had when it last
   stopped, and a collection walks a slab only up to that watermark, so no
   collection can see it as a block in the first place. */
static void *handout_begin(char *p, size_t total) {
  *(uint64_t *)p = (uint64_t)total;
  *(uint64_t *)((char *)p + 8) = 0;
  void *obj = p + TY_HDR;
  *(void **)obj = NULL;
  TY_ROOT_PUSH(obj);
  return obj;
}

void *ty_alloc_slow(size_t total) {
  /* The batch first, and before the lock: a block this thread took from the
     free list in a batch of TY_BATCH is handed out here without the heap lock
     at all, which is the whole point of taking it in a batch. The budget is
     tested here for the same reason the fast path tests it -- a budget that ran
     out while this list still had blocks would otherwise postpone the
     collection for as long as the list lasts -- and a thread that has spent its
     budget takes the locked path below and collects. A batch hit is
     deliberately not a safepoint, which the locked path is: generated code
     already stops at every loop head and every call, and the budget sends the
     thread down the locked path -- which stops -- as soon as it runs out. */
  tythread *me = ty_self;
  int k = size_class(total);
  /* `state == TY_TH_RUNNING` is what makes popping here safe without the heap
     lock. A collection empties every batch in collect_slabs, and it can only
     have got there if this thread was stopped: a running thread is exactly
     what ty_gc_stop_world waits for. So a thread that is stopped -- one inside
     a blocking call, or the collector itself -- does not touch this list, and
     takes the locked path below instead, where the lock and the world protocol
     already order it against the collection. */
  if (k >= 0 && me->state == TY_TH_RUNNING && me->batch[k] &&
      me->alloc_since <= ty_gc_threshold) {
    void *p = me->batch[k];
    me->batch[k] = *(void **)((char *)p + 8);
    me->alloc_since += (int64_t)total;
    return block_object(p, total);
  }
  /* Stop first: a collection may be asked for while this runs, and a thread must
     not be holding the heap lock while it waits for one to end. */
  ty_safepoint();
  ty_heap_lock();
  /* The budget may be what sent us here rather than the slab running out -- the
     fast path tests both -- so a collection comes first, and then the thread's
     own slab is tried again: it is still this thread's memory, and a collection
     neither moves it nor takes it away. */
  if (gc_stress_every) {
    /* TEYRU_GC_STRESS: every Nth allocation collects, whatever the byte budget
       says. The count is of entries into this function, which under the switch
       is every allocation: the budget is -1, so the inlined fast path hands
       each one over instead of bumping its slab. Every allocation therefore
       also takes the heap lock and stops at a safepoint, which is the point --
       a stress run is meant to exercise the paths a well-behaved program walks
       rarely, not to be quick. The counter is one for the whole process and is
       only ever read by this switch. */
    if (++gc_stress_count >= gc_stress_every) {
      gc_stress_count = 0;
      gc_collect("stress");
    }
  } else if (me->alloc_since > ty_gc_threshold) {
    gc_collect("budget");
  }
  char *p = me->bump;
  if (!(p + total > me->bump_end)) {
    me->bump = p + total;
    me->alloc_since += (int64_t)total;
    ty_heap_unlock();
    return block_object(p, total);
  }
  /* The slab is exhausted. Free blocks come as a batch when they are in a size
     class at all: the sweep links a whole slab of dead blocks onto these lists
     at every collection, and every one of them drained one at a time is a lock
     and a safepoint. A big block -- one over the largest class -- is taken one
     at a time as it always was: they are large, they are few, and a batch of
     them would hold more memory than the threads that want them. */
  p = k >= 0 ? take_batch(k) : NULL;
  if (!p) p = take_free(total);
  void *obj = NULL;
  int rooted = 0;
  if (p) {
    obj = handout_begin(p, total);
    rooted = 1;
  }
  if (!p) {
    /* Refill: a slab of this thread's own. The old one goes back to the shared
       heap, where the collector can reclaim it and where its free blocks are
       available to everybody. What is left of it -- the tail this block did not
       fit in -- stays beyond the watermark, so no walk ever reads it as a block
       header. A full slab is not a reason to collect: ty_gc_threshold is the
       adaptive budget (twice the live set after the last collection, 4MB floor)
       and the free lists were consulted above, so collecting here would re-trace
       a live set the threshold has not asked for yet. Grow instead; the next
       threshold crossing pays for it. */
    if (me->chunk) {
      ty_heap_sync();
      tychunk *old = (tychunk *)me->chunk;
      old->next = chunks;
      chunks = old;
    }
    tychunk *c = chunk_new(total);
    me->chunk = c;
    me->bump = c->mem + total;
    me->bump_end = c->mem + c->cap;
    c->used = total;
    p = c->mem;
    /* This slab is fresh and its watermark is this one block, so the block is
       inside the region a collection walks -- and its size word is whatever
       malloc left there until handout_begin writes it. It is in the same
       position as a block off a free list, and gets the same treatment. */
    obj = handout_begin(p, total);
    rooted = 1;
  }
  me->alloc_since += (int64_t)total;
  ty_heap_unlock();
  /* The hand-out is done: the block is the caller's object now, and its
     reference is in the caller's frame before this thread can stop again. What
     is left of block_object -- zeroing the payload -- is done here, outside the
     heap lock, so that a large array is not zeroed while everybody else waits
     for the heap. */
  if (rooted) {
    memset(obj, 0, total - TY_HDR);
    TY_ROOT_POP();
  }
  return obj;
}

/* ---- narrowing a floating-point value to an integer -------------------- */

/* The two conversions both back ends emit, with the answers JLS 5.1.3 gives.
   The comparisons are the whole of it: a value at or above the bound is the
   bound, one at or below the lower bound is that bound, and everything else
   fits and is truncated toward zero by the cast. NaN has to be tested first
   because it compares false against both bounds and would otherwise be cast --
   which is the undefined case this function exists to avoid.

   The bounds are written as the exact powers of two, not as MAX_VALUE: 2^31 and
   2^63 are exactly representable as doubles while 2147483647 and
   9223372036854775807 are not, and a rounded bound would clamp one value late.
   (The largest double below 2^63 is 2^63 - 1024, which still fits in an int64,
   so nothing between the bound and the largest representable value is lost.) */
int32_t ty_d2i(double d) {
  if (d != d) return 0;              /* NaN */
  if (d >= 2147483648.0) return INT32_MAX;
  if (d <= -2147483648.0) return INT32_MIN;
  return (int32_t)d;
}

int64_t ty_d2l(double d) {
  if (d != d) return 0;              /* NaN */
  if (d >= 9223372036854775808.0) return INT64_MAX;
  if (d <= -9223372036854775808.0) return INT64_MIN;
  return (int64_t)d;
}

void ty_gc_init(void) {
  for (int i = 0; i < TY_NCLASS; i++) freelist[i] = NULL;
  /* The two diagnostics switches, read once, here, before the program runs:
     what they set is read on the allocation slow path and inside a collection,
     and neither of those is a place to call getenv. */
  const char *stress = getenv("TEYRU_GC_STRESS");
  if (stress && *stress) {
    char *end = NULL;
    long n = strtol(stress, &end, 10);
    if (end != stress && *end == '\0' && n > 0) {
      gc_stress_every = (int64_t)n;
      /* Every allocation has to reach ty_alloc_slow for the count to be a count
         of allocations, and the budget is what sends it there: -1 is below any
         allocation size the fast path has accumulated. */
      ty_gc_threshold = -1;
    } else {
      /* Silently ignoring this would leave a run that was meant to be a stress
         run measuring ordinary behaviour, and a report that said "under
         TEYRU_GC_STRESS" while nothing was stressed. Say so, and carry on
         without it. */
      fprintf(stderr, "teyru: TEYRU_GC_STRESS=%s is not a number of allocations; the switch is off\n", stress);
    }
  }
  const char *trace = getenv("TEYRU_GCTRACE");
  gc_trace = trace && *trace && strcmp(trace, "0") != 0;
  /* TEYRU_GC_VERIFY=1 checks the frame maps against the conservative pass: see
     verify_frame. It costs a walk of every mapped frame's words per collection,
     so it is for a diagnosis and not for a run. */
  const char *verify = getenv("TEYRU_GC_VERIFY");
  gc_verify = verify && *verify && strcmp(verify, "0") != 0;
  const char *noexcl = getenv("TEYRU_GC_NOEXCL");
  gc_noexcl = noexcl && *noexcl && strcmp(noexcl, "0") != 0;
}

void *ty_alloc_arr(int64_t len, size_t elemsize) {
  if (len < 0) ty_throw((tyobj *)ty_negarr());
  size_t bytes = (size_t)len * elemsize;
  if (len != 0 && bytes / (size_t)len != elemsize) ty_throw((tyobj *)ty_negarr());
  tyarr *a = (tyarr *)ty_alloc(sizeof(tyarr) + bytes);
  a->obj.cls = array_class();
  a->len = len;
  a->data = (char *)a + sizeof(tyarr);
  a->esize = (int32_t)elemsize;
  a->refs = 0;
  /* no promise about the elements: the caller that knows the element class
     (the generated code for `new T[]`) records it, and only then is a store
     into this array checked against it */
  a->elemcls = NULL;
  return a;
}

/* ------------------------------------------------------------------ exceptions */

/* Java's uncaught handler prints the throwable's own toString -- that is what
   ThreadGroup.uncaughtException does -- and nothing else. Composing the class
   name here as well would double it, and would print the class in front of a
   toString a program had overridden. */
static void print_uncaught(void *p, const char *where, int wherelen) {
  tyobj *e = (tyobj *)p;
  /* The check goes off for the report, and this is not an optimization: the
     toString below is generated code, and it begins with the same prologue
     check every generated function does. Reporting a StackOverflowError from
     below the limit therefore threw another one from inside the reporting,
     which reported another one, until the stack really did run out and the
     process died of a fault -- with nothing printed, which is the worst of the
     three possible outcomes. Nothing after this point recurses into the
     program's own code, and the thread ends when the report is done either way
     (exit here, the end of the thread's start function there), so there is
     nothing the check would have protected. What is left of the margin is what
     the report runs in, and it is far more than a toString needs. */
  ty_stack_limit = NULL;
  tystr *s = ((tystr *(*)(void *))e->cls->vtable[0])(e);
  char *msg = s ? TY_STR_DATA(s) : (char *)"?";
  fprintf(stderr, "Exception in thread \"%.*s\" %.*s\n", wherelen, where,
          (int)(s ? s->blen : 1), msg);
}

void ty_uncaught(void *p) {
  print_uncaught(p, "main", 4);
  exit(1);
}

/* A spawned thread's handler differs in one thing: it lets the process run on.
   Java's is the same -- the line is printed, the thread ends, and the program
   does not -- and the thread's name is what says which thread it was. */
void ty_uncaught_thread(void *e, tythread *t) {
  tystr *name = t ? t->name : NULL;
  print_uncaught(e, name ? TY_STR_DATA(name) : "main", name ? (int)name->blen : 4);
}

void ty_throw(void *e) {
  if (!ty_cur_catch) ty_uncaught(e);
  ty_cur_catch->ex = (tyobj*)e;
  /* The landing frame's map, not the call's. longjmp drops every frame between
     here and the setjmp that armed this catch frame, and those frames' map
     records are stack storage that is about to be reused by whatever runs next;
     the chain has to be rewound to what it was when the catch was armed, or the
     collector would walk dead frames and never reach the live ones below the
     landing point -- the frames whose references it is the maps' job to find.
     Rewinding here rather than in each landing pad is what makes it true for
     every catch frame there is: generated code arms these frames and so does the
     runtime (tyrt2.c, tyrt_reflect.c, tyrt_thread.c), and none of them has to
     know that a collection exists. It happens before the jump so there is no
     window in which the chain and the stack disagree. */
  ty_frames = ty_cur_catch->frames;
  longjmp(ty_cur_catch->buf, 1);
}


void *ty_npe(void) {
  tystr *m = ty_str_intern("null");
  ty_throw(ty_make_ex(TY_NPE, "null"));
  return m;
}
void *ty_aioobe(int64_t idx, int64_t len) {
  char buf[128];
  snprintf(buf, sizeof buf, "Index %lld out of bounds for length %lld", (long long)idx, (long long)len);
  ty_throw(ty_make_ex(TY_AIOOBE, buf));
  return NULL;
}
/* The same report for a string or a buffer, which Java keeps apart from an
   array's: both are IndexOutOfBoundsExceptions, so a program that catches that
   one catches either, but a program that catches ArrayIndexOutOfBoundsException
   -- "an array index was wrong" -- must not be answering a String's. */
void *ty_sioobe(int64_t idx, int64_t len) {
  char buf[128];
  snprintf(buf, sizeof buf, "Index %lld out of bounds for length %lld", (long long)idx, (long long)len);
  ty_throw(ty_make_ex(TY_SIOOBE, buf));
  return NULL;
}
void *ty_arith(const char *msg) {
  ty_throw(ty_make_ex(TY_ARITH, msg));
  return NULL;
}
void *ty_cce(tyclass *from, tyclass *to) {
  char buf[256];
  snprintf(buf, sizeof buf, "class %s cannot be cast to class %s", from ? from->name : "?", to ? to->name : "?");
  ty_throw(ty_make_ex(TY_CCE, buf));
  return NULL;
}
void *ty_arraystore(void) {
  ty_throw(ty_make_ex(TY_ARRAYSTORE, "array element type mismatch"));
  return NULL;
}
void *ty_negarr(void) {
  ty_throw(ty_make_ex(TY_NEGARR, "Negative array size"));
  return NULL;
}
void *ty_assertfail(const char *msg) {
  ty_throw(ty_make_ex(TY_ASSERT, msg ? msg : "assertion failed"));
  return NULL;
}

/* StackOverflowError is the one exception the runtime cannot build where it is
   thrown. Every other one is made from a frame with the whole stack still
   under it, and ty_make_ex may allocate; this one is thrown by a function whose
   own frame is already past the limit, and an allocation there would be a call
   into the allocator from the last 256 KB of a stack, with the collector, the
   heap lock and a safepoint behind it. So the object is made at thread start,
   kept in the thread's state, and marked by the collector as a root for as long
   as the thread is in the registry (ty_gc_locked) -- which is what the
   alternative, a plain malloc outside the heap, would have had to give up:
   AGENTS.md requires a traced object to come from ty_alloc.

   The message stays NULL, as Java's is for this error: throwable's toString
   prints the class name alone when there is no message, and a StackOverflowError
   built with an empty string would print "teyru.StackOverflowError: ". */
void ty_stack_overflow_reserve(void) {
  tythread *me = ty_self;
  if (!me || me->soe || !TY_SOE) return;
  tyobj *o = (tyobj *)ty_alloc(sizeof(tyobj) + 2 * sizeof(void *));
  o->cls = TY_SOE;
  ((void **)((char *)o + sizeof(tyobj)))[0] = NULL; /* the message */
  ((void **)((char *)o + sizeof(tyobj)))[1] = NULL; /* the cause */
  me->soe = o;
}

void ty_stack_overflow(void) {
  tythread *me = ty_self;
  /* A program the compiler built always has one: the generated startup reserves
     it before main runs and every thread start reserves its own. The failure is
     named rather than worked around, because the alternative -- allocating one
     here -- is the thing this whole path exists to not do. */
  if (!me || !me->soe) ty_unimplemented("stack overflow before the runtime reserved StackOverflowError");
  ty_throw(me->soe);
}

/* iface_reaches reports whether k, or any interface it implements, is c.
 *
 * A class's iface list is the interfaces it *declares*; the ones beyond that
 * come from two directions -- the superclass chain, and the implemented
 * interfaces' own super-interfaces. Following both is what makes
 * `x instanceof Collection` true for a value whose class says
 * `implements List` and whose List says `extends Collection`.
 *
 * The depth bound is a guard against a malformed class table: a cycle in the
 * interface graph is not valid Java, and looping forever in the runtime would
 * turn a bad program into a hung one. */
static int iface_reaches(tyclass *k, tyclass *c, int depth) {
  if (!k || depth > 32) return 0;
  for (int32_t i = 0; i < k->niface; i++) {
    if (k->ifaces[i] == c) return 1;
    if (iface_reaches(k->ifaces[i], c, depth + 1)) return 1;
  }
  return 0;
}

/* ty_class_is_sub is the walk instanceof and Class.isAssignableFrom share: is
   k the class c, or does it inherit from it? An interface test looks at every
   class in the chain, not only the one a value was built from, because a
   subclass inherits the interfaces its superclasses implement and did not
   redeclare them. */
int32_t ty_class_is_sub(tyclass *k, tyclass *c) {
  if (!k || !c) return 0;
  if (k == c) return 1;
  if (c->flags & 1) {
    for (tyclass *s = k; s; s = s->super)
      if (iface_reaches(s, c, 0)) return 1;
    return 0;
  }
  for (tyclass *s = k->super; s; s = s->super)
    if (s == c) return 1;
  return 0;
}

int32_t ty_instanceof(void *p, tyclass *c) {
  tyobj *o = (tyobj *)p;
  if (!o) return 0;
  return ty_class_is_sub(o->cls, c);
}

void *ty_checkcast(void *o, tyclass *c) {
  if (!o) return NULL;
  if (ty_instanceof(o, c)) return o;
  return ty_cce(((tyobj *)o)->cls, c);
}

/* The cold half of interface dispatch: no class in the chain answers for the
   selector, so there is no function to call. ty_itab in tyrt.h is the inline
   half and calls this; it is noreturn, which is what lets the inline version
   fall off the end without a value. */
void ty_itab_slow(void) {
  ty_throw((tyobj *)ty_make_ex(TY_UNSUP, "no implementation for this interface method"));
}

/* ------------------------------------------------------------------ strings */

/* The bytes of a string the runtime builds are canonical WTF-8. Three rules
   hold everywhere below.

   A code point below 0x10000 is one code unit and one sequence of one, two or
   three bytes; a surrogate that stands alone is that same three-byte form, so
   UTF-8's reserved encoding of D800..DFFF is load-bearing here rather than
   malformed. An astral code point is two code units and one four-byte
   sequence. And a high surrogate followed by its low one is always stored as
   that four-byte sequence and never as the two three-byte ones, which is what
   makes byte equality and UTF-16 equality the same relation: ty_str_eq,
   ty_str_cmp and the hash stay byte operations, and a string built by
   substring or by appending one char at a time is byte-identical to the same
   text written as one literal. */

/* The length in bytes of the sequence that starts at p. */
static int seq_len(const char *p) {
  unsigned char a = (unsigned char)p[0];
  if (a < 0x80) return 1;
  if ((a & 0xE0) == 0xC0) return 2;
  if ((a & 0xF0) == 0xE0) return 3;
  return 4;
}

/* The same, held inside the string. A string this file builds is canonical, so
   a sequence never runs past the last byte and the clamp never bites; a string
   built from a byte range by an older byte-oriented helper can hold a truncated
   sequence, and a walk that trusted the lead byte would then read past the
   object. The clamp is what makes that a wrong answer rather than a fault. */
static int seq_len_in(const char *d, int64_t n, int64_t at) {
  int len = seq_len(d + at);
  return at + len > n ? (int)(n - at) : len;
}

/* The code point the sequence at p encodes, given its length in bytes. A lone
   surrogate decodes to itself; a four-byte sequence is a whole code point. */
static int32_t seq_cp(const char *p, int n) {
  unsigned char a = (unsigned char)p[0];
  switch (n) {
    case 1: return a;
    case 2: return ((a & 0x1F) << 6) | ((unsigned char)p[1] & 0x3F);
    case 3:
      return ((a & 0x0F) << 12) | (((unsigned char)p[1] & 0x3F) << 6) |
             ((unsigned char)p[2] & 0x3F);
    default:
      return ((a & 0x07) << 18) | (((unsigned char)p[1] & 0x3F) << 12) |
             (((unsigned char)p[2] & 0x3F) << 6) | ((unsigned char)p[3] & 0x3F);
  }
}

/* Writes cp as WTF-8 and answers the number of bytes: four above 0xFFFF, the
   three-byte form for a surrogate, two or one below. */
static int seq_put(char *p, int32_t cp) {
  if (cp < 0x80) {
    p[0] = (char)cp;
    return 1;
  }
  if (cp < 0x800) {
    p[0] = (char)(0xC0 | (cp >> 6));
    p[1] = (char)(0x80 | (cp & 0x3F));
    return 2;
  }
  if (cp < 0x10000) {
    p[0] = (char)(0xE0 | (cp >> 12));
    p[1] = (char)(0x80 | ((cp >> 6) & 0x3F));
    p[2] = (char)(0x80 | (cp & 0x3F));
    return 3;
  }
  p[0] = (char)(0xF0 | (cp >> 18));
  p[1] = (char)(0x80 | ((cp >> 12) & 0x3F));
  p[2] = (char)(0x80 | ((cp >> 6) & 0x3F));
  p[3] = (char)(0x80 | (cp & 0x3F));
  return 4;
}

/* The UTF-16 length of the bytes, and whether they are all ASCII. Heads are
   what is counted, and a four-byte sequence is the one that is two units. The
   ASCII test runs a word at a time because it is the common answer and the
   answer the whole read API is fast for. */
static int64_t str_measure(const char *data, int64_t len, int *ascii) {
  const unsigned char *d = (const unsigned char *)data;
  int64_t i = 0, u = 0;
  int plain = 1;
  while (i + 8 <= len) {
    uint64_t w;
    memcpy(&w, d + i, 8);
    if (w & UINT64_C(0x8080808080808080)) break;
    i += 8;
    u += 8;
  }
  for (; i < len; i++) {
    unsigned char c = d[i];
    if (c < 0x80) {
      u++;
    } else if ((c & 0xC0) != 0x80) {
      plain = 0;
      u++;
      if (c >= 0xF0) u++;
    }
  }
  *ascii = plain;
  return u;
}

/* One string, one allocation: the header, the bytes, their terminator, and the
   breadcrumb table's space (which this leaves unbuilt). Callers that already
   know both lengths come here directly; ty_str_new measures first. */
static tystr *str_alloc(int64_t blen, int64_t ulen, int ascii) {
  size_t nbc = ascii ? 0 : (size_t)(ulen >> 6) + 1;
  size_t pad = ascii ? 0 : 3; /* room to align the table after the NUL */
  tystr *s = (tystr *)ty_alloc(sizeof(tystr) + (size_t)blen + 1 + pad + nbc * sizeof(int32_t));
  s->obj.cls = TY_STRING;
  s->blen = blen;
  s->ulen = (int32_t)ulen;
  s->flags = ascii ? TY_SF_ASCII : 0;
  return s;
}

tystr *ty_str_new(const char *data, int64_t len) {
  int ascii;
  int64_t ulen;
  tystr *s;
  if (!data) {
    /* An uninitialised string: the caller writes the bytes itself, so there is
       nothing to measure yet. The header claims the worst case -- every byte a
       sequence of its own -- which is what keeps the breadcrumb space large
       enough for whatever ends up written, and the caller is responsible for
       writing exactly `len` bytes and no NUL among them. */
    s = str_alloc(len, len, 0);
    TY_STR_DATA(s)[len] = 0;
    return s;
  }
  ulen = str_measure(data, len, &ascii);
  s = str_alloc(len, ulen, ascii);
  memcpy(TY_STR_DATA(s), data, (size_t)len);
  TY_STR_DATA(s)[len] = 0;
  return s;
}

tystr *ty_str_intern(const char *data) { return ty_str_new(data, (int64_t)strlen(data)); }

/* Finishes a string whose bytes a caller wrote into a block it got from
   ty_str_new(NULL, n), which is how repeat and replace(String,String) assemble
   a result out of pieces: it folds a high surrogate that ended up next to its
   low one into the four-byte sequence the two units are -- the rule that makes
   byte equality and UTF-16 equality one relation -- and fills in the measured
   byte length, code unit count and ASCII bit.

   The fold can only shorten a string, and the block was sized for the longest
   it could have been, so the breadcrumb table still fits where the new byte
   length puts it: the table starts at TY_STR_BC_OFF(blen), which only moves
   left, and the space reserved for it was computed from the larger length.
   The table is unbuilt here and stays unbuilt -- TY_SF_BC is the last thing
   anything sets. */
static void str_finish(tystr *s) {
  char *d = TY_STR_DATA(s);
  int64_t n = s->blen, i = 0, w = 0;
  int64_t u = 0;
  int ascii = 1;
  while (i < n) {
    int len = seq_len_in(d, n, i);
    if (len == 4) {
      u += 2;
    } else if (len == 3) {
      int32_t cp = seq_cp(d + i, 3);
      if (cp >= 0xD800 && cp <= 0xDBFF && i + 6 <= n && (unsigned char)d[i + 3] == 0xED) {
        int32_t lo = seq_cp(d + i + 3, 3);
        if (lo >= 0xDC00 && lo <= 0xDFFF) {
          seq_put(d + w, 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00));
          w += 4;
          u += 2;
          i += 6;
          continue;
        }
      }
      ascii = 0;
      u += 1;
    } else {
      if (len != 1) ascii = 0;
      u += 1;
    }
    if (w != i) memmove(d + w, d + i, (size_t)len);
    w += len;
    i += len;
  }
  d[w] = 0;
  s->blen = w;
  s->ulen = (int32_t)u;
  s->flags = ascii ? TY_SF_ASCII : 0;
}

/* ---- index conversion -------------------------------------------------- */

/* Java indexes a string by UTF-16 code unit and the storage is WTF-8, so
   charAt(i) has to find the sequence that holds unit i. The two ends of that
   translation are the ASCII fast path -- where a unit index and a byte offset
   are the same number -- and the breadcrumb table the header reserves room
   for: entry k is the byte offset of code unit 64*k, so reaching unit i
   decodes at most TY_BC_UNITS sequences instead of i. The table is built on
   first use, because a string that is never indexed should not pay for the
   walk, and it costs one entry per 64 units rather than one per unit. */

/* Fills the table. Two threads can be here at once -- a literal in .data is
   shared, and so is any string two threads reached -- and they compute the
   same numbers, so the race is on publication and not on the answer: entries
   go in with relaxed stores and the flag that says the table is ready goes in
   last with a release, which is what a reader's acquire load pairs with. An
   entry is written only when the walk reaches the unit it indexes, so a string
   whose ulen over-claims -- which no path in this file leaves behind, and
   str_finish is the one that could -- would leave zeroes there and answer a
   wrong index rather than read past its own block. */
static void str_build_bc(tystr *s) {
  const char *d = TY_STR_DATA(s);
  int32_t *bc = TY_STR_BC(s);
  int64_t at = 0;
  int32_t u = 0;
  __atomic_store_n(&bc[0], 0, __ATOMIC_RELAXED);
  while (at < s->blen) {
    int n = seq_len_in(d, s->blen, at);
    at += n;
    u += n == 4 ? 2 : 1;
    if ((u & (TY_BC_UNITS - 1)) == 0 && (u >> 6) < TY_STR_NBC(s))
      __atomic_store_n(&bc[u >> 6], (int32_t)at, __ATOMIC_RELAXED);
  }
  __atomic_store_n(&s->flags, __atomic_load_n(&s->flags, __ATOMIC_RELAXED) | TY_SF_BC,
                   __ATOMIC_RELEASE);
}

/* The byte offset of the sequence that holds code unit i1, and how many units
   into that sequence i1 is -- 1 only when i1 names the low half of an astral
   character, whose two units share one four-byte sequence. Callers have
   checked i1 against ulen. */
static int64_t str_byte_of(tystr *s, int64_t i1, int *into) {
  const char *d = TY_STR_DATA(s);
  int64_t at;
  int32_t u;
  *into = 0;
  if (i1 <= 0) return 0;
  if (TY_STR_ASCII(s)) return i1;
  if (!(__atomic_load_n(&s->flags, __ATOMIC_ACQUIRE) & TY_SF_BC)) str_build_bc(s);
  {
    int64_t block = i1 >> 6;
    at = __atomic_load_n(&TY_STR_BC(s)[block], __ATOMIC_RELAXED);
    u = (int32_t)(block << 6);
  }
  for (;;) {
    int n = seq_len_in(d, s->blen, at);
    int w = n == 4 ? 2 : 1;
    if (u + w > i1) {
      *into = (int)(i1 - u);
      return at;
    }
    at += n;
    u += w;
    if (at >= s->blen) { /* only a truncated sequence reaches this */
      *into = 0;
      return s->blen > 0 ? s->blen - 1 : 0;
    }
  }
}

/* The code unit at index i1. Callers have checked i1 against ulen. */
static uint16_t str_unit_at(tystr *s, int64_t i1) {
  int into;
  int64_t at = str_byte_of(s, i1, &into);
  const char *d = TY_STR_DATA(s);
  int n = seq_len_in(d, s->blen, at);
  if (n == 4) {
    int32_t pair = seq_cp(d + at, 4) - 0x10000;
    return (uint16_t)(into ? 0xDC00 + (pair & 0x3FF) : 0xD800 + (pair >> 10));
  }
  return (uint16_t)seq_cp(d + at, n);
}

/* A cursor over a string's code units. Sequential work -- searching, hashing,
   comparing, the case conversions, the formatter -- walks with one of these:
   it is one decode a unit and needs no index, where reaching a unit by number
   has to find its sequence through the breadcrumb table first. */
typedef struct {
  tystr *s;
  int64_t at; /* the sequence the next unit comes from */
  int half;   /* the next unit is the low half of an astral character */
} tyucur;

/* Positions the cursor at unit zero, which is the one position that needs no
   table: a sequential walk from here should not build one. */
static void ucur_init(tyucur *c, tystr *s) {
  c->s = s;
  c->at = 0;
  c->half = 0;
}

/* Positions the cursor at code unit i1, which must be below ulen. */
static void ucur_at(tyucur *c, tystr *s, int64_t i1) {
  int into = 0;
  c->s = s;
  if (i1 >= s->ulen) {
    c->at = s->blen;
    c->half = 0;
    return;
  }
  c->at = str_byte_of(s, i1, &into);
  c->half = into;
}

/* The unit the cursor is on, after which it is one unit further along. An
   astral character is two units out of one sequence: the first call answers
   its high surrogate and stays, the second answers the low one and steps over
   the four bytes. */
static uint16_t ucur_next(tyucur *c) {
  const char *d = TY_STR_DATA(c->s);
  int n;
  /* A walk that has run past the last byte answers zeroes and keeps advancing
     rather than reading the block's own padding; only a string whose ulen
     over-claims can get here, and nothing in this file leaves one behind. */
  if (c->at >= c->s->blen) {
    c->half = 0;
    c->at++;
    return 0;
  }
  n = seq_len_in(d, c->s->blen, c->at);
  if (n == 4) {
    int32_t pair = seq_cp(d + c->at, 4) - 0x10000;
    if (c->half) {
      c->half = 0;
      c->at += 4;
      return (uint16_t)(0xDC00 + (pair & 0x3FF));
    }
    c->half = 1;
    return (uint16_t)(0xD800 + (pair >> 10));
  }
  c->half = 0;
  c->at += n;
  return (uint16_t)seq_cp(d + c->at - n, n);
}

/* Whether code unit i1 begins a sequence. It does not only when i1 is the low
   half of an astral character, whose two units share one four-byte sequence --
   and a range that begins or ends between such a pair cannot be a copy of the
   bytes between its ends, because the half left over is a lone surrogate with
   an encoding of its own. */
static int unit_is_high(int32_t c) { return c >= 0xD800 && c <= 0xDBFF; }
static int unit_is_low(int32_t c) { return c >= 0xDC00 && c <= 0xDFFF; }

static int str_on_boundary(tystr *s, int64_t i1) {
  int into = 0;
  if (i1 <= 0 || i1 >= s->ulen) return 1;
  str_byte_of(s, i1, &into);
  return into == 0;
}

/* The byte offset of the sequence holding unit i1, which is where a walk
   starting at that unit begins. */
static int64_t str_byte_at(tystr *s, int64_t i1) {
  int into = 0;
  if (i1 >= s->ulen) return s->blen;
  return str_byte_of(s, i1, &into);
}

/* The code unit index a byte offset falls on. Every caller has an offset a
   canonical needle can only have matched at -- a needle never begins with a
   continuation byte, and a continuation byte is what lies inside a sequence --
   so counting the sequence heads before the offset is the whole answer. */
static int64_t unit_of_byte(tystr *s, int64_t at) {
  const char *d = TY_STR_DATA(s);
  int64_t i = 0, u = 0;
  if (TY_STR_ASCII(s)) return at;
  while (i < at && i < s->blen) {
    int n = seq_len_in(d, s->blen, i);
    if (n == 4) {
      if (i + 4 > at) return u + 1; /* inside a pair: the low half's index */
      u += 2;
    } else {
      u++;
    }
    i += n;
  }
  return u;
}

/* Defined below, and declared here because str_slice's mid-pair case -- the one
   that cannot be a copy of bytes -- builds its answer through it. */
tystr *ty_str_of_units(const uint16_t *u, int64_t n);

/* A range of code units of another string. The two ends are usually sequence
   boundaries and the range is the bytes between them; when one of them cuts an
   astral character in half -- substring(1, 2) of one emoji -- the leftover half
   is a lone surrogate, so the units are written out again. */
static tystr *str_slice(tystr *s, int64_t from, int64_t to) {
  if (from >= to) return ty_str_new(TY_STR_DATA(s), 0);
  if (str_on_boundary(s, from) && str_on_boundary(s, to)) {
    int64_t a = from == 0 ? 0 : str_byte_at(s, from);
    int64_t b = to >= s->ulen ? s->blen : str_byte_at(s, to);
    return ty_str_new(TY_STR_DATA(s) + a, b - a);
  }
  {
    int64_t n = to - from, i;
    uint16_t *tmp = (uint16_t *)malloc((size_t)n * sizeof(uint16_t));
    tyucur c;
    tystr *r;
    ucur_at(&c, s, from);
    for (i = 0; i < n; i++) tmp[i] = ucur_next(&c);
    r = ty_str_of_units(tmp, n);
    free(tmp);
    return r;
  }
}

/* One string out of code units, in the two passes every constructor of one
   uses: the first measures what the second will write. Measuring first is what
   lets the string be one allocation of exactly the right size -- the
   breadcrumb table's offset depends on the byte length, so it cannot be
   decided after the bytes are written. A high surrogate followed by its low
   one is written as the single four-byte sequence they are, and it counts two
   code units either way.

   This and ty_str_units are where the two representations meet: a String is
   WTF-8 bytes and a builder's buffer is code units, and every crossing goes
   through one of these rather than through a loop written again at the call
   site. */
tystr *ty_str_of_units(const uint16_t *u, int64_t n) {
  int64_t blen = 0, i;
  int ascii = 1;
  tystr *s;
  char *p;
  for (i = 0; i < n; i++) {
    uint16_t c = u[i];
    if (c < 0x80) {
      blen += 1;
    } else if (c < 0x800) {
      blen += 2;
      ascii = 0;
    } else if (c >= 0xD800 && c <= 0xDBFF && i + 1 < n && u[i + 1] >= 0xDC00 &&
               u[i + 1] <= 0xDFFF) {
      blen += 4;
      ascii = 0;
      i++;
    } else {
      blen += 3;
      ascii = 0;
    }
  }
  s = str_alloc(blen, n, ascii);
  p = TY_STR_DATA(s);
  for (i = 0; i < n; i++) {
    uint16_t c = u[i];
    if (c >= 0xD800 && c <= 0xDBFF && i + 1 < n && u[i + 1] >= 0xDC00 &&
        u[i + 1] <= 0xDFFF) {
      p += seq_put(p, 0x10000 + ((c - 0xD800) << 10) + (u[i + 1] - 0xDC00));
      i++;
    } else {
      p += seq_put(p, c);
    }
  }
  *p = 0;
  return s;
}

/* The other direction: the code units of a string, written into a caller's
   array, which must hold ulen of them. A builder appends through this, so a
   string that arrived from anywhere lands in its buffer as units and an astral
   character is the two halves Java would see. */
int64_t ty_str_units(tystr *s, uint16_t *out) {
  int64_t i;
  if (!s) ty_npe();
  if (TY_STR_ASCII(s)) {
    const char *d = TY_STR_DATA(s);
    for (i = 0; i < s->blen; i++) out[i] = (unsigned char)d[i];
  } else {
    tyucur c;
    ucur_init(&c, s);
    for (i = 0; i < s->ulen; i++) out[i] = ucur_next(&c);
  }
  return s->ulen;
}

/* Whether the last code unit is a high surrogate with no low one after it, and
   whether the first is a low surrogate with no high one before it. The pair of
   tests is what ty_str_concat and the builder use to join the halves; both are
   three-byte-sequence tests, since a paired surrogate is never stored alone. */
static int str_ends_high(tystr *a) {
  const char *d = TY_STR_DATA(a);
  int64_t n = a->blen;
  if (n < 3) return 0;
  unsigned char c1 = (unsigned char)d[n - 3], c2 = (unsigned char)d[n - 2], c3 = (unsigned char)d[n - 1];
  if ((c1 & 0xF0) != 0xE0 || (c2 & 0xC0) != 0x80 || (c3 & 0xC0) != 0x80) return 0;
  int32_t cp = ((c1 & 0x0F) << 12) | ((c2 & 0x3F) << 6) | (c3 & 0x3F);
  return cp >= 0xD800 && cp <= 0xDBFF;
}
static int str_starts_low(tystr *b) {
  const unsigned char *d = (const unsigned char *)TY_STR_DATA(b);
  if (b->blen < 3) return 0;
  if ((d[0] & 0xF0) != 0xE0 || (d[1] & 0xC0) != 0x80 || (d[2] & 0xC0) != 0x80) return 0;
  int32_t cp = ((d[0] & 0x0F) << 12) | ((d[1] & 0x3F) << 6) | (d[2] & 0x3F);
  return cp >= 0xDC00 && cp <= 0xDFFF;
}

/* String.length() is the number of UTF-16 code units, which is what a Java
   program counts with and what every index in the API means. It is not the
   number of bytes the string is stored in: "中文" is two units and six bytes,
   and a supplementary character is two units and four bytes. */
int64_t ty_str_len(tystr *s) {
  if (!s) ty_npe();
  return s->ulen;
}

/* The byte length, which is what the byte-oriented boundary means: getBytes,
   a socket write, a file. Nothing in the language's own index arithmetic wants
   this number. */
int64_t ty_str_blen(tystr *s) {
  if (!s) ty_npe();
  return s->blen;
}

tystr *ty_str_concat(tystr *a, tystr *b) {
  if (!a) a = ty_str_intern("null");
  if (!b) b = ty_str_intern("null");
  /* A high surrogate ending one half and the low surrogate starting the other
     are one character, not two, and the bytes have to say so. It costs two
     16-bit compares and only pays off for a string built a char at a time --
     which is exactly what `+` in a loop does. */
  int join = str_ends_high(a) && str_starts_low(b);
  const char *da = TY_STR_DATA(a), *db = TY_STR_DATA(b);
  int64_t na = join ? a->blen - 3 : a->blen;
  int64_t nb = join ? b->blen - 3 : b->blen;
  /* The join costs two bytes and no code unit: the two units that were encoded
     separately are the same two units inside one sequence. */
  tystr *r = str_alloc(na + nb + (join ? 4 : 0), a->ulen + b->ulen,
                       TY_STR_ASCII(a) && TY_STR_ASCII(b));
  char *out = TY_STR_DATA(r);
  memcpy(out, da, (size_t)na);
  if (join) {
    int32_t hi = seq_cp(da + na, 3), lo = seq_cp(db, 3);
    out += seq_put(out + na, 0x10000 + ((hi - 0xD800) << 10) + (lo - 0xDC00));
  } else {
    out += na;
  }
  memcpy(out, db + (join ? 3 : 0), (size_t)nb);
  TY_STR_DATA(r)[r->blen] = 0;
  return r;
}

int32_t ty_str_eq(tystr *a, tystr *b) {
  if (a == b) return 1;
  if (!a || !b) return 0;
  if (a->blen != b->blen) return 0;
  return memcmp(TY_STR_DATA(a), TY_STR_DATA(b), (size_t)a->blen) == 0;
}

int32_t ty_str_cmp(tystr *a, tystr *b) {
  int64_t i, n;
  if (a == b) return 0;
  if (!a) return -1;
  if (!b) return 1;
  /* compareTo is defined on code units and answers their difference, not a
     sign: "中".compareTo("文") is -5978 and not -1. Byte order does not give
     that answer -- a supplementary character begins with a byte above 0xF0 and
     so sorts after U+E000..U+FFFF, where Java sorts it before -- so the walk
     is over units. Where both strings are ASCII a byte is a unit and the
     comparison is a byte comparison. */
  n = a->ulen < b->ulen ? a->ulen : b->ulen;
  if (TY_STR_ASCII(a) && TY_STR_ASCII(b)) {
    const char *pa = TY_STR_DATA(a), *pb = TY_STR_DATA(b);
    int r = memcmp(pa, pb, (size_t)n);
    if (r) {
      for (i = 0; pa[i] == pb[i]; i++) {
      }
      return (int32_t)(unsigned char)pa[i] - (int32_t)(unsigned char)pb[i];
    }
  } else {
    tyucur ca, cb;
    ucur_init(&ca, a);
    ucur_init(&cb, b);
    for (i = 0; i < n; i++) {
      int32_t x = ucur_next(&ca), y = ucur_next(&cb);
      if (x != y) return x - y;
    }
  }
  return (int32_t)(a->ulen - b->ulen);
}

/* String.hashCode(), which is 31*h + c over the UTF-16 code units. A string
   built from a supplementary character hashes the two surrogate halves in
   order, so the answer is the JDK's for those too, and it is not the same
   answer as hashing the four UTF-8 bytes. ASCII is the fast path: there a byte
   is a unit. */
int32_t ty_str_hash(tystr *s) {
  int32_t h = 0;
  int64_t i;
  if (!s) ty_npe();
  if (TY_STR_ASCII(s)) {
    const char *d = TY_STR_DATA(s);
    for (i = 0; i < s->blen; i++) h = 31 * h + (unsigned char)d[i];
    return h;
  }
  {
    tyucur c;
    ucur_init(&c, s);
    for (i = 0; i < s->ulen; i++) h = 31 * h + ucur_next(&c);
  }
  return h;
}

/* The identity hash of an object, which is what Object.hashCode means for a
   class that does not override it. The generated Object.hashCode wrapper calls
   this function, and that wrapper is what a non-overriding class has in its
   vtable slot, so this must NOT dispatch: doing so would call itself.

   An overriding class is reached by dispatching at the call site instead, which
   the emitter does for any receiver whose static type has subclasses. The
   address is stable because the collector never moves objects. */
int32_t ty_obj_hash(void *o) {
  if (!o) return 0;
  uintptr_t p = (uintptr_t)o;
  return (int32_t)((p >> 4) ^ (p >> 32) ^ (p >> 20));
}

/* Object.equals for a class that does not override it. Same reasoning as above:
   identity, and the call site dispatches when an override may exist. */
int32_t ty_obj_eq(void *a, void *b) { return a == b; }

tystr *ty_str_of_long(int64_t v) {
  char buf[32];
  int n = snprintf(buf, sizeof buf, "%lld", (long long)v);
  return ty_str_new(buf, n);
}
tystr *ty_str_of_int(int64_t v) { return ty_str_of_long(v); }
tystr *ty_str_of_bool(int32_t v) { return ty_str_intern(v ? "true" : "false"); }
tystr *ty_str_of_char(uint16_t c) {
  char buf[4];
  int n = 0;
  if (c < 0x80) {
    buf[n++] = (char)c;
  } else if (c < 0x800) {
    buf[n++] = (char)(0xC0 | (c >> 6));
    buf[n++] = (char)(0x80 | (c & 0x3F));
  } else {
    buf[n++] = (char)(0xE0 | (c >> 12));
    buf[n++] = (char)(0x80 | ((c >> 6) & 0x3F));
    buf[n++] = (char)(0x80 | (c & 0x3F));
  }
  return ty_str_new(buf, n);
}
/* Shortest representation that reads back exactly, formatted the way Java's
   Double.toString does: plain decimal when 1e-3 <= |v| < 1e7, scientific
   otherwise, and always with a fractional part.

   The search runs from two significant digits up, which is Java's lower bound
   (the digits after the point are never empty: 1.0, not 1) and is what makes
   the subnormals come out right -- Double.MIN_VALUE is 4.9E-324 and not the
   4.94065645841247E-324 that rounding its exact value to fifteen digits gives.
   is_float narrows the round-trip test to float precision, so a float is
   spelled with the digits that bring back that float and not the nearest
   double's. */
static int fmt_generic(char *buf, size_t cap, long double v, int lo, int hi, int is_float) {
  char tmp[96];
  int prec = hi;
  for (int p = lo; p <= hi; p++) {
    snprintf(tmp, sizeof tmp, "%.*Le", p - 1, v);
    long double back = strtod(tmp, NULL);
    if (is_float) back = (float)back;
    if (back == v) { prec = p; break; }
  }
  snprintf(tmp, sizeof tmp, "%.*Le", prec - 1, v);
  /* split "[-]d.dddde±XX" into digits and an exponent */
  char digits[64];
  int nd = 0;
  int neg = tmp[0] == '-';
  for (char *q = tmp; *q && *q != 'e' && *q != 'E'; q++) {
    if (*q >= '0' && *q <= '9') digits[nd++] = *q;
  }
  int exp10 = 0;
  char *e = strpbrk(tmp, "eE");
  if (e) exp10 = atoi(e + 1);
  /* strip trailing zeros of the fractional part */
  while (nd > 1 && digits[nd - 1] == '0') nd--;
  /* build the Java form */
  char out[96];
  int n = 0;
  if (neg) out[n++] = '-';
  int pointPos = exp10 + 1; /* position of the decimal point within digits */
  if (pointPos > -3 && pointPos <= 7) {
    if (pointPos <= 0) {
      out[n++] = '0';
      out[n++] = '.';
      for (int i = 0; i < -pointPos; i++) out[n++] = '0';
      for (int i = 0; i < nd; i++) out[n++] = digits[i];
    } else {
      for (int i = 0; i < pointPos; i++) out[n++] = i < nd ? digits[i] : '0';
      out[n++] = '.';
      if (nd <= pointPos) {
        out[n++] = '0';
      } else {
        for (int i = pointPos; i < nd; i++) out[n++] = digits[i];
      }
    }
  } else {
    out[n++] = digits[0];
    out[n++] = '.';
    if (nd > 1) {
      for (int i = 1; i < nd; i++) out[n++] = digits[i];
    } else {
      out[n++] = '0';
    }
    out[n++] = 'E';
    n += snprintf(out + n, sizeof out - (size_t)n, "%d", exp10);
  }
  if (n >= (int)cap) n = (int)cap - 1;
  memcpy(buf, out, (size_t)n);
  buf[n] = 0;
  return n;
}

static int fmt_double(char *buf, size_t cap, double v) {
  if (v != v) return snprintf(buf, cap, "NaN");
  if (v == 1.0 / 0.0) return snprintf(buf, cap, "Infinity");
  if (v == -1.0 / 0.0) return snprintf(buf, cap, "-Infinity");
  /* signbit, not `v < 0`: Java spells the negative zero "-0.0" and the two
     zeros compare equal, so the sign has to be read from the bits. */
  if (v == 0.0) return snprintf(buf, cap, signbit(v) ? "-0.0" : "0.0");
  return fmt_generic(buf, cap, (long double)v, 2, 17, 0);
}

static int fmt_float(char *buf, size_t cap, float v) {
  if (v != v) return snprintf(buf, cap, "NaN");
  if (v == 1.0f / 0.0f) return snprintf(buf, cap, "Infinity");
  if (v == -1.0f / 0.0f) return snprintf(buf, cap, "-Infinity");
  if (v == 0.0f) return snprintf(buf, cap, signbit(v) ? "-0.0" : "0.0");
  return fmt_generic(buf, cap, (long double)v, 2, 9, 1);
}

tystr *ty_str_of_double(double v) {
  char buf[96];
  int n = fmt_double(buf, sizeof buf, v);
  return ty_str_new(buf, n);
}
tystr *ty_str_of_float(float v) {
  char buf[96];
  int n = fmt_float(buf, sizeof buf, v);
  return ty_str_new(buf, n);
}

tystr *ty_object_tostring(void *o) {
  if (!o) return ty_str_intern("null");
  char buf[128];
  /* The name is the one the JDK uses (decision D8), which is what Class.getName()
     answers too: a default toString that said teyru.Object while getName() said
     java.lang.Object would be two answers to one question. */
  int n = snprintf(buf, sizeof buf, "%s@%llx", ty_class_jname(((tyobj *)o)->cls), (unsigned long long)(uintptr_t)o);
  return ty_str_new(buf, n);
}

tystr *ty_str_of_obj(void *o) {
  if (!o) return ty_str_intern("null");
  return ((tystr *(*)(void *))((tyobj *)o)->cls->vtable[0])(o);
}

/* ty_str_upper and ty_str_lower are not here: a case conversion is a question
   about code points and, for the final sigma, about the word a code point sits
   in, so both are defined beside the Unicode tables they read (see
   "the case mappings of a String", further down). */
tystr *ty_str_trim(tystr *s) {
  if (!s) return NULL;
  int64_t a = 0, b = s->blen;
  while (a < b && (unsigned char)TY_STR_DATA(s)[a] <= ' ') a++;
  while (b > a && (unsigned char)TY_STR_DATA(s)[b - 1] <= ' ') b--;
  return ty_str_new(TY_STR_DATA(s) + a, b - a);
}
/* Whether the units of `p` equal the units of `s` starting at k. Only the two
   places where a match may begin or end in the middle of an astral character
   need this: everywhere else a byte comparison answers, because a canonical
   needle can only match at a sequence start. */
static int str_unit_range_eq(tystr *s, int64_t k, tystr *p) {
  tyucur a, b;
  int64_t i;
  if (k < 0 || k + p->ulen > s->ulen) return 0;
  ucur_at(&a, s, k);
  ucur_init(&b, p);
  for (i = 0; i < p->ulen; i++)
    if (ucur_next(&a) != ucur_next(&b)) return 0;
  return 1;
}

/* Whether a match of `sub` can sit at a unit position where its bytes are not
   the haystack's bytes. It can exactly when the needle begins with a low
   surrogate or ends with a high one: those are the two ends that can be half of
   a pair in the haystack, where the needle spells the half alone and the
   haystack spells the pair as one four-byte sequence. Every other needle's
   bytes are the haystack's bytes wherever its units match. */
static int str_cuts_pair(tystr *sub) {
  if (sub->ulen == 0) return 0;
  return unit_is_low(str_unit_at(sub, 0)) || unit_is_high(str_unit_at(sub, sub->ulen - 1));
}

/* The smallest unit index k >= from where `sub` occurs, or -1.

   Where the needle's bytes are the haystack's bytes wherever its units match --
   which is every needle but the surrogate-cutting one, and every `from` but one
   inside a pair -- the scan is over bytes: a canonical needle can only match at
   a sequence start, so a byte match is a unit match, the scan gets to be
   memcmp, and the answer is that offset translated back into a unit index.
   Otherwise the walk is over units, which is slower and is the only thing that
   can see a match that sits inside a four-byte sequence. */
static int64_t str_find(tystr *s, tystr *sub, int64_t from) {
  const char *d, *p;
  int64_t at, limit;
  if (sub->ulen == 0) return from <= s->ulen ? from : (int64_t)s->ulen;
  if (from < 0) from = 0;
  if (from > s->ulen - sub->ulen) return -1;
  /* A needle that can sit inside a pair, or a range that starts inside one,
     needs the unit walk: the byte scan cannot see those matches at all, and it
     would answer -1 where Java answers an index. */
  if (str_cuts_pair(sub) || !str_on_boundary(s, from)) {
    tyucur c;
    int64_t k;
    uint16_t first = str_unit_at(sub, 0);
    ucur_at(&c, s, from);
    for (k = from; k + sub->ulen <= s->ulen; k++) {
      if (ucur_next(&c) == first && str_unit_range_eq(s, k, sub)) return k;
    }
    return -1;
  }
  d = TY_STR_DATA(s);
  p = TY_STR_DATA(sub);
  at = from == 0 ? 0 : str_byte_at(s, from);
  limit = s->blen - sub->blen;
  for (; at <= limit; at++) {
    int64_t k;
    if (d[at] != p[0]) continue;
    if (memcmp(d + at, p, (size_t)sub->blen) != 0) continue;
    k = unit_of_byte(s, at);
    if (k >= from) return k;
  }
  return -1;
}

/* substring is over units, and its answer may hold half of an astral
   character: "a中b".substring(1, 2) is 中, and the same call on "😀" (whose
   boundaries are 0 and 2) is asked for 0..1 by the caller that wants half of
   it, which is a lone surrogate -- a string Java has. */
tystr *ty_str_sub(tystr *s, int32_t from, int32_t to) {
  if (!s) return NULL;
  if (from < 0) ty_throw((tyobj *)ty_sioobe(from, s->ulen));
  if (to > s->ulen) ty_throw((tyobj *)ty_sioobe(to, s->ulen));
  if (from > to) ty_throw((tyobj *)ty_sioobe(to, s->ulen));
  return str_slice(s, from, to);
}
int32_t ty_str_indexof(tystr *s, tystr *sub) {
  if (!s || !sub) ty_npe();
  return (int32_t)str_find(s, sub, 0);
}
/* substring(int): the same call with the end left off, and the same answer --
   Java's substring(from) is substring(from, length()). */
tystr *ty_str_sub_from(tystr *s, int32_t from) {
  if (!s) return NULL;
  if (from < 0 || from > s->ulen) ty_throw((tyobj *)ty_sioobe(from, s->ulen));
  return str_slice(s, from, s->ulen);
}
int32_t ty_str_charat(tystr *s, int32_t i) {
  if (!s || i < 0 || i >= s->ulen) ty_throw((tyobj *)ty_sioobe(i, s ? s->ulen : 0));
  return str_unit_at(s, i);
}
int32_t ty_str_contains(tystr *s, tystr *sub) { return ty_str_indexof(s, sub) >= 0; }
/* startsWith and endsWith are unit comparisons whose common case is a byte
   comparison: a byte prefix of a canonical string is a prefix of its units,
   and so is a byte suffix, because the bytes of a canonical needle that sit at
   the end of a canonical haystack can only begin where a sequence does. What
   the byte test misses is the needle that ends or begins in the middle of an
   astral character -- "\uD83D" is a prefix of "😀" in Java -- and the unit walk
   behind it answers that. */
int32_t ty_str_starts(tystr *s, tystr *p) {
  if (!s || !p) return 0;
  if (p->ulen > s->ulen) return 0;
  if (p->blen <= s->blen && memcmp(TY_STR_DATA(s), TY_STR_DATA(p), (size_t)p->blen) == 0) return 1;
  return str_unit_range_eq(s, 0, p);
}
int32_t ty_str_ends(tystr *s, tystr *p) {
  if (!s || !p) return 0;
  if (p->ulen > s->ulen) return 0;
  if (p->blen <= s->blen &&
      memcmp(TY_STR_DATA(s) + s->blen - p->blen, TY_STR_DATA(p), (size_t)p->blen) == 0)
    return 1;
  return str_unit_range_eq(s, s->ulen - p->ulen, p);
}
/* replace(char, char) is over code units, and either side of it can be half of
   an astral character, so the two units that meet where the change happened may
   belong together. Building the unit array and letting the string constructor
   join is what gets that right -- and it is also what keeps
   "\uD83Dx".replace('x', '\uDE00') one character rather than two. */
tystr *ty_str_replace(tystr *s, uint16_t a, uint16_t b) {
  uint16_t *tmp;
  tystr *r;
  int64_t i;
  if (!s) return NULL;
  if (s->ulen == 0) return ty_str_new(TY_STR_DATA(s), 0);
  tmp = (uint16_t *)malloc((size_t)s->ulen * sizeof(uint16_t));
  if (TY_STR_ASCII(s)) {
    for (i = 0; i < s->blen; i++) {
      uint16_t c = (unsigned char)TY_STR_DATA(s)[i];
      tmp[i] = c == a ? b : c;
    }
  } else {
    tyucur c;
    ucur_init(&c, s);
    for (i = 0; i < s->ulen; i++) {
      uint16_t u = ucur_next(&c);
      tmp[i] = u == a ? b : u;
    }
  }
  r = ty_str_of_units(tmp, s->ulen);
  free(tmp);
  return r;
}
int32_t ty_str_isempty(tystr *s) {
  if (!s) ty_npe();
  return s->blen == 0;
}
int32_t ty_str_toint(tystr *s) { return s ? (int32_t)strtoll(TY_STR_DATA(s), NULL, 10) : 0; }

/* ------------------------------------------------------------------ patterns */

/* JEP 507: a primitive type pattern matches when the boxed value survives the
   conversion to the pattern's type unchanged. Widening between integral types
   is always exact; everything else is checked by converting back. */
int32_t ty_prim_match(void *o, int32_t kind, void *out, int32_t boxed) {
  if (!o) return 0;
  tyclass *c = ((tyobj *)o)->cls;
  if (!(c->flags & 4)) return 0; /* not a box at all */

  int32_t bk = 0;
  for (int32_t i = 1; i <= 8; i++) {
    if (c == TY_BOX[i]) {
      bk = i;
      break;
    }
  }

  /* JEP 507 has two rules and the static type of the operand picks which one
     applies (javac 25 --enable-preview was the oracle for both):

     - A reference operand carries a box, and the box has to be exactly the
       pattern's type: an Integer matches `int i` but not `long l`, even though
       the value would convert exactly.
     - A primitive operand (the compiler boxes it to get here, hence the flag)
       matches when the conversion to the pattern's type is exact, so
       `long v = 5; v instanceof int i` is true and 5000000000L is not. */
  if (boxed) {
    if (bk != kind) return 0;
    switch (kind) {
    case 1: *(int32_t *)out = ((tyboolbox *)o)->v; return 1;
    case 2: *(int8_t *)out = ((tybytebox *)o)->v; return 1;
    case 3: *(int16_t *)out = ((tyshortbox *)o)->v; return 1;
    case 4: *(uint16_t *)out = ((tycharbox *)o)->v; return 1;
    case 5: *(int32_t *)out = ((tyintbox *)o)->v; return 1;
    case 6: *(int64_t *)out = ((tylongbox *)o)->v; return 1;
    case 7: *(float *)out = ((tyfloatbox *)o)->v; return 1;
    case 8: *(double *)out = ((tydoublebox *)o)->v; return 1;
    }
    return 0;
  }

  int64_t iv = 0;
  double dv = 0;
  int is_floating = 0;
  if (bk == 0) return 0;
  if (bk == 1) return 0; /* a Boolean is only a boolean; it carries no number */
  if (bk == 2) iv = ((tybytebox *)o)->v;
  else if (bk == 3) iv = ((tyshortbox *)o)->v;
  else if (bk == 4) iv = ((tycharbox *)o)->v;
  else if (bk == 5) iv = ((tyintbox *)o)->v;
  else if (bk == 6) iv = ((tylongbox *)o)->v;
  else if (bk == 7) { dv = ((tyfloatbox *)o)->v; is_floating = 1; }
  else if (bk == 8) { dv = ((tydoublebox *)o)->v; is_floating = 1; }

  switch (kind) {
  case 1: return 0;
  case 2:
    if (is_floating) { if (dv != (double)(int8_t)dv) return 0; iv = (int64_t)dv; }
    if (iv < -128 || iv > 127) return 0;
    *(int8_t *)out = (int8_t)iv;
    return 1;
  case 3:
    if (is_floating) { if (dv != (double)(int16_t)dv) return 0; iv = (int64_t)dv; }
    if (iv < -32768 || iv > 32767) return 0;
    *(int16_t *)out = (int16_t)iv;
    return 1;
  case 4:
    if (is_floating) { if (dv != (double)(uint16_t)dv) return 0; iv = (int64_t)dv; }
    if (iv < 0 || iv > 65535) return 0;
    *(uint16_t *)out = (uint16_t)iv;
    return 1;
  case 5:
    if (is_floating) { if (dv != (double)(int32_t)dv) return 0; iv = (int64_t)dv; }
    if (iv < INT32_MIN || iv > INT32_MAX) return 0;
    *(int32_t *)out = (int32_t)iv;
    return 1;
  case 6:
    if (is_floating) { if (dv != (double)(int64_t)dv) return 0; iv = (int64_t)dv; }
    *(int64_t *)out = iv;
    return 1;
  case 7:
    if (is_floating) { *(float *)out = (float)dv; return 1; }
    *(float *)out = (float)iv;
    return (int64_t)*(float *)out == iv;
  case 8:
    if (is_floating) { *(double *)out = dv; return 1; }
    *(double *)out = (double)iv;
    return (int64_t)*(double *)out == iv;
  }
  return 0;
}

/* ------------------------------------------------------------------ arrays */

tyarr *ty_array_new(int64_t len, int64_t elemsize) { return ty_alloc_arr(len, (size_t)elemsize); }
int64_t ty_array_len(tyarr *a) {
  if (!a) ty_npe();
  return a->len;
}
tyarr *ty_array_clone(tyarr *a, int64_t elemsize) {
  if (!a) ty_npe();
  tyarr *r = ty_alloc_arr(a->len, (size_t)elemsize);
  r->obj.cls = array_class();
  r->esize = a->esize;
  r->refs = a->refs;
  /* Java's clone keeps the array's runtime element type, so a copy of a
     String[] is still a String[] for the store check below */
  r->elemcls = a->elemcls;
  memcpy(r->data, a->data, (size_t)(a->len * elemsize));
  return r;
}
void *ty_arr_ptr(tyarr *a, int64_t i) {
  if (!a || i < 0 || i >= a->len) ty_throw((tyobj *)ty_aioobe(i, a ? a->len : 0));
  return (char *)a->data + (size_t)i * a->esize;
}
void *ty_arr_slot_ref(tyarr *a, int64_t i) {
  if (!a || i < 0 || i >= a->len) ty_throw((tyobj *)ty_aioobe(i, a ? a->len : 0));
  return (char *)a->data + (size_t)i * 8;
}
void *ty_arr_ref(tyarr *a, int64_t i) { return *(void **)ty_arr_slot_ref(a, i); }



/* ------------------------------------------------------------------ boxing */

#define DEFBOX(NAME, IDX, CT, JT, CONV)                                     \
  typedef struct NAME##Box { tyobj obj; JT v; } NAME##Box;             \
  void *ty_box_##NAME(JT v) {                                          \
    NAME##Box *b = (NAME##Box *)ty_alloc(sizeof(NAME##Box));           \
    b->obj.cls = TY_BOX[IDX];                                          \
    b->v = v;                                                          \
    return b;                                                          \
  }                                                                    \
  JT ty_unbox_##NAME(void *o) {                                        \
    if (!o) ty_npe();                                                  \
    return ((NAME##Box *)o)->v;                                        \
  }

DEFBOX(int, 5, tyint, int32_t, )
DEFBOX(long, 6, tylong, int64_t, )
DEFBOX(short, 3, tyshort, int16_t, )
DEFBOX(byte, 2, tybyte, int8_t, )
DEFBOX(char, 4, tychar, uint16_t, )
DEFBOX(bool, 1, tybool, int32_t, )

void *ty_box_double(double v) {
  tydoublebox *b = (tydoublebox *)ty_alloc(sizeof(tydoublebox));
  b->obj.cls = TY_BOX[8];
  b->v = v;
  return b;
}
double ty_unbox_double(void *o) {
  if (!o) ty_npe();
  return ((tydoublebox *)o)->v;
}
void *ty_box_float(float v) {
  tyfloatbox *b = (tyfloatbox *)ty_alloc(sizeof(tyfloatbox));
  b->obj.cls = TY_BOX[7];
  b->v = v;
  return b;
}
float ty_unbox_float(void *o) {
  if (!o) ty_npe();
  return ((tyfloatbox *)o)->v;
}

/* ------------------------------------------------------------------ output */

void ty_str_write(FILE *f, tystr *s) {
  const char *d;
  int64_t i, run = 0;
  if (!s) {
    fputs("null", f);
    return;
  }
  d = TY_STR_DATA(s);
  /* An ASCII string holds no surrogate, so its bytes are what the encoder
     writes. Everything else is walked, and the bytes between two lone
     surrogates are written in one go: a string of Korean text has 0xED as a
     lead byte all through it, and writing those one sequence at a time would
     turn a text write into a call per character. */
  if (TY_STR_ASCII(s) || !memchr(d, 0xED, (size_t)s->blen)) {
    fwrite(d, 1, (size_t)s->blen, f);
    return;
  }
  for (i = 0; i < s->blen;) {
    int len = seq_len_in(d, s->blen, i);
    if (len == 3) {
      int32_t cp = seq_cp(d + i, 3);
      if (unit_is_high(cp) || unit_is_low(cp)) {
        if (i > run) fwrite(d + run, 1, (size_t)(i - run), f);
        fputc('?', f);
        i += 3;
        run = i;
        continue;
      }
    }
    i += len;
  }
  if (s->blen > run) fwrite(d + run, 1, (size_t)(s->blen - run), f);
}
void ty_print_str(tystr *s) { ty_str_write(stdout, s); }
void ty_println_str(tystr *s) { ty_print_str(s); putchar('\n'); }
void ty_print_int(int64_t v) { printf("%lld", (long long)v); }
void ty_println_int(int64_t v) { printf("%lld\n", (long long)v); }
void ty_print_double(double v) {
  char buf[96];
  int n = fmt_double(buf, sizeof buf, v);
  fwrite(buf, 1, (size_t)n, stdout);
}
void ty_println_double(double v) {
  ty_print_double(v);
  putchar('\n');
}
void ty_print_float(float v) { ty_print_str(ty_str_of_float(v)); }
void ty_println_float(float v) { ty_print_float(v); putchar('\n'); }
void ty_print_char(uint16_t c) {
  if (c < 0x80) putchar((int)c);
  else ty_str_write(stdout, ty_str_of_char(c));
}
void ty_println_char(uint16_t c) { ty_print_char(c); putchar('\n'); }
void ty_print_bool(int32_t v) { fputs(v ? "true" : "false", stdout); }
void ty_println_bool(int32_t v) { fputs(v ? "true\n" : "false\n", stdout); }
void ty_print_obj(void *o) { ty_print_str(ty_str_of_obj(o)); }
void ty_println_obj(void *o) { ty_print_obj(o); putchar('\n'); }
void ty_println_void(void) { putchar('\n'); }

/* The monitors `synchronized` and Object.wait are built on live in
   tyrt_thread.c, next to the stop-the-world protocol they interact with. */

/* ==================================================================== java.lang
 *
 * The helpers below complete the java.lang surface the prelude declares. They
 * live in this file rather than next to their cousins in tyrt2.c because the
 * build keeps one translation unit per concern and this one is the language's:
 * everything here answers a question the JDK answers, and the answer is Java's,
 * not C's -- which is the whole reason the helpers exist instead of a #define.
 */

/* ------------------------------------------------------------------ Math */

/* Java's Math.round is floor(v + 0.5) *as long as the result is in range*: NaN
   is 0 and anything at or past the end of the range saturates. C's cast of an
   out-of-range double to int64_t is undefined instead, and it is why the bounds
   are tested before the cast rather than after. `(double)INT64_MAX` is 2^63 --
   the same double the comparison wants, since a double cannot hold 2^63 - 1. */
int64_t ty_math_round_long(double v) {
  if (v != v) return 0;
  if (v <= (double)INT64_MIN) return INT64_MIN;
  if (v >= (double)INT64_MAX) return INT64_MAX;
  return (int64_t)floor(v + 0.5);
}

int32_t ty_math_round_int(float v) {
  if (v != v) return 0;
  if (v <= (float)INT32_MIN) return INT32_MIN;
  if (v >= (float)INT32_MAX) return INT32_MAX;
  return (int32_t)floorf(v + 0.5f);
}

/* Java's max and min are not `a > b ? a : b`: a NaN wins over a number (`a` is
   tested, and a NaN `b` is returned by the comparison), and the two zeros are
   ordered so that max(-0.0, 0.0) is +0.0 and min(-0.0, 0.0) is -0.0. The
   asymmetry -- max tests `a` for negative zero, min tests `b` -- is the JDK's,
   and it is what makes both results come out right. */
static int64_t bits_of_double(double d) {
  int64_t b;
  memcpy(&b, &d, 8);
  return b;
}
static int32_t bits_of_float(float f) {
  int32_t b;
  memcpy(&b, &f, 4);
  return b;
}
double ty_math_max_double(double a, double b) {
  if (a != a) return a;
  if (a == 0.0 && b == 0.0 && bits_of_double(a) == (int64_t)0x8000000000000000LL) return b;
  return a >= b ? a : b;
}
double ty_math_min_double(double a, double b) {
  if (a != a) return a;
  if (a == 0.0 && b == 0.0 && bits_of_double(b) == (int64_t)0x8000000000000000LL) return b;
  return a <= b ? a : b;
}
float ty_math_max_float(float a, float b) {
  if (a != a) return a;
  if (a == 0.0f && b == 0.0f && bits_of_float(a) == (int32_t)0x80000000) return b;
  return a >= b ? a : b;
}
float ty_math_min_float(float a, float b) {
  if (a != a) return a;
  if (a == 0.0f && b == 0.0f && bits_of_float(b) == (int32_t)0x80000000) return b;
  return a <= b ? a : b;
}

float ty_abs_float(float v) { return fabsf(v); }

/* Math.abs of an integer is the one case where the obvious `v < 0 ? -v : v` is
   undefined: negating the most negative value overflows, which in C is not a
   value at all. Java defines it -- abs(MIN_VALUE) is MIN_VALUE -- so the
   negation goes through the unsigned type, where it is well defined and gives
   the same bits back. */
int32_t ty_math_abs_int(int32_t v) { return v < 0 ? (int32_t)(0u - (uint32_t)v) : v; }
int64_t ty_math_abs_long(int64_t v) { return v < 0 ? (int64_t)(0ull - (uint64_t)v) : v; }

/* floorDiv/floorMod round toward negative infinity, where C's / and % truncate
   toward zero: -7 / 2 is -3 in C and -4 in Java. MIN_VALUE / -1 overflows in
   both languages and C's answer for it is a trap rather than a value, so it is
   answered here the way the JVM answers it, with MIN_VALUE. */
int32_t ty_math_floor_div_int(int32_t a, int32_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1 && a == INT32_MIN) return INT32_MIN;
  int32_t r = a / b;
  if ((a ^ b) < 0 && r * b != a) r--;
  return r;
}
int64_t ty_math_floor_div_long(int64_t a, int64_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1 && a == INT64_MIN) return INT64_MIN;
  int64_t r = a / b;
  if ((a ^ b) < 0 && r * b != a) r--;
  return r;
}
int32_t ty_math_floor_mod_int(int32_t a, int32_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1) return 0;
  int32_t r = a % b;
  if ((a ^ b) < 0 && r != 0) r += b;
  return r;
}
int64_t ty_math_floor_mod_long(int64_t a, int64_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1) return 0;
  int64_t r = a % b;
  if ((a ^ b) < 0 && r != 0) r += b;
  return r;
}

/* signum: the sign of a number, and 0 for both zeros, for NaN and for an
   infinity... except that an infinity has a sign, which is the point of the
   function: Math.signum(-Infinity) is -1.0. */
double ty_signum_double(double v) {
  if (v != v) return 0.0;
  if (v == 0.0) return v; /* keeps the zero's sign, as Java does */
  return v > 0.0 ? 1.0 : -1.0;
}
float ty_signum_float(float v) {
  if (v != v) return 0.0f;
  if (v == 0.0f) return v;
  return v > 0.0f ? 1.0f : -1.0f;
}

/* Math.toRadians and toDegrees are one multiplication each in the JDK, by the
   constant PI/180 and its reciprocal folded at compile time -- not `deg / 180.0
   * PI`, which differs from it in the last bit for arguments like 3.0. javac 25
   was the oracle: toRadians(3.0) is 0.05235987755982989, which is the folded
   form, and toDegrees(1e-10) is 5.729577951308233E-9, likewise. The divisions
   below are folded by the C compiler with the same round-to-nearest rule, so
   they give the same two doubles the JDK's compiler does. */
double ty_math_to_radians(double deg) { return deg * (3.14159265358979323846 / 180.0); }
double ty_math_to_degrees(double rad) { return rad * (180.0 / 3.14159265358979323846); }

/* Math.cbrt through the C library alone is off by an ulp more often than not --
   cbrt(27.0) comes back 3.0000000000000004 -- because glibc's cbrt is accurate
   to an ulp rather than to the nearest double, while Java's is fdlibm's and is
   correctly rounded for every input. A cube root that prints as 3.0000000000000004
   looks like a bug in the program that computed it, so the library's answer is
   treated as a starting point: the two neighbouring doubles are computed too and
   the one whose cube is nearest x wins. The cubes are formed in long double,
   whose 64-bit mantissa is more than the 53 a comparison of three candidates
   needs, and where long double is the same as double -- on a platform without
   the wider type -- the check would compare equal cubes and the library's answer
   stands unchanged. */
double ty_math_cbrt(double x) {
  double y, best, lo, hi, d;
  long double bx, bd;
  int k;
  if (x != x || x == 0.0 || x == 1.0 / 0.0 || x == -1.0 / 0.0) return cbrt(x);
  y = cbrt(x);
#if LDBL_MANT_DIG > DBL_MANT_DIG
  bx = (long double)x;
  best = y;
  bd = fabsl((long double)y * y * y - bx);
  lo = y;
  hi = y;
  /* two doubles each way, because the library's error is sometimes two ulps
     rather than one (cbrt(1e-10) is), and the search has to contain the
     correctly rounded answer before it can find it */
  for (k = 0; k < 2; k++) {
    lo = nextafter(lo, -1.0 / 0.0);
    hi = nextafter(hi, 1.0 / 0.0);
    d = fabsl((long double)lo * lo * lo - bx);
    if (d < bd) { best = lo; bd = d; }
    d = fabsl((long double)hi * hi * hi - bx);
    if (d < bd) { best = hi; bd = d; }
  }
  return best;
#else
  return y;
#endif
}

/* ------------------------------------------------------------------ System */

/* getenv hands back a copy: the value the C library returns belongs to it and
   the collector must not be told to trace it. An unset variable is null, which
   is what Java's getenv answers. */
tystr *ty_getenv(tystr *name) {
  if (!name) ty_npe();
  char *v = getenv(TY_STR_DATA(name));
  return v ? ty_str_intern(v) : NULL;
}

/* System.getProperty for the handful of keys a program can be expected to ask
   for. There is no java.* namespace behind this runtime, so those keys are
   unknown, which is the same answer Java gives for a key it does not know. */
tystr *ty_get_property(tystr *key) {
  if (!key) ty_npe();
  const char *k = TY_STR_DATA(key);
  if (strcmp(k, "line.separator") == 0) return ty_str_intern("\n");
  if (strcmp(k, "file.separator") == 0) return ty_str_intern("/");
  if (strcmp(k, "path.separator") == 0) return ty_str_intern(":");
  if (strcmp(k, "file.encoding") == 0) return ty_str_intern("UTF-8");
  if (strcmp(k, "java.io.tmpdir") == 0) return ty_str_intern("/tmp");
#ifdef _WIN32
  return NULL;
#else
  if (strcmp(k, "os.name") == 0) return ty_str_intern("Linux");
  if (strcmp(k, "os.arch") == 0) return ty_str_intern(
#ifdef __x86_64__
      "amd64"
#elif defined(__aarch64__)
      "aarch64"
#else
      ""
#endif
  );
  {
    const char *env = NULL;
    if (strcmp(k, "user.name") == 0) env = getenv("USER");
    else if (strcmp(k, "user.home") == 0) env = getenv("HOME");
    else if (strcmp(k, "user.dir") == 0) {
      static char cwd[4096];
      if (getcwd(cwd, sizeof cwd)) return ty_str_intern(cwd);
      return NULL;
    }
    return env ? ty_str_intern(env) : NULL;
  }
#endif
}

/* read() is the one method java.io.InputStream declares abstract: one byte, or
   -1 at end of input. The byte is returned as an int because 0..255 all have to
   be distinguishable from the -1 that means end of file. */
int32_t ty_in_read(void *self) {
  (void)self;
  int c = getchar();
  return c == EOF ? -1 : (int32_t)(unsigned char)c;
}
tystr *ty_in_readln(void *self) {
  (void)self;
  return ty_readln();
}

/* ------------------------------------------------------------------ int ops */

int32_t ty_div_int(int32_t a, int32_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1 && a == INT32_MIN) return INT32_MIN;
  return a / b;
}
int64_t ty_div_long(int64_t a, int64_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1 && a == INT64_MIN) return INT64_MIN;
  return a / b;
}
int32_t ty_rem_int(int32_t a, int32_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1) return 0;
  return a % b;
}
int64_t ty_rem_long(int64_t a, int64_t b) {
  if (b == 0) ty_throw((tyobj *)ty_arith("/ by zero"));
  if (b == -1) return 0;
  return a % b;
}

/* ------------------------------------------------------------- Character */

/* Classification, and the case mapping of one code point.

   Every answer here is the JDK's, and the JDK's answers are Unicode's: the
   tables of tyrt_unicode.c are generated from Unicode 15.0.0's own data files
   (internal/tools/genunicode), and what this section adds is the mapping from
   those tables onto java.lang.Character -- which categories a predicate is,
   which property bits a predicate is, and the two case conversions that are
   about a whole string rather than about one code point.

   It is deliberately not <ctype.h>: those functions answer for the current
   locale, and the answer a program gets must not depend on LC_CTYPE. Nor is it
   a range of ASCII any more: the classification of a code point above the BMP
   is answered as exactly as the classification of 'a', which is what Java does
   and what a program comparing two strings from different scripts needs. */
#define TY_ASCII_UPPER(c) ((c) >= 'A' && (c) <= 'Z')
#define TY_ASCII_LOWER(c) ((c) >= 'a' && (c) <= 'z')
#define TY_ASCII_DIGIT(c) ((c) >= '0' && (c) <= '9')
#define TY_ASCII_ALPHA(c) (TY_ASCII_UPPER(c) || TY_ASCII_LOWER(c))

/* The letters are Lu, Ll, Lt, Lm and Lo, which are the five categories the
   general category numbers 1..5 -- a fact about how the table was written
   (genunicode numbers them as Character's constants are numbered), not about
   which characters they are. */
#define TY_CAT_IS_LETTER(t) ((t) >= TY_UC_UPPERCASE_LETTER && (t) <= TY_UC_OTHER_LETTER)

int32_t ty_char_type(int32_t cp) { return ty_uc_category(cp); }
int32_t ty_is_letter(int32_t cp) { return TY_CAT_IS_LETTER(ty_uc_category(cp)); }
int32_t ty_is_digit(int32_t cp) { return ty_uc_category(cp) == TY_UC_DECIMAL_DIGIT_NUMBER; }
int32_t ty_is_letter_or_digit(int32_t cp) { return ty_is_letter(cp) || ty_is_digit(cp); }
/* Alphabetic is the letters, the letter numbers and Other_Alphabetic, which is
   the combining marks and modifier letters of the scripts that write a vowel
   with one: U+0345 COMBINING GREEK YPOGEGRAMMENI is alphabetic and is not a
   letter. */
int32_t ty_is_alphabetic(int32_t cp) {
  return ty_is_letter(cp) || ty_uc_category(cp) == TY_UC_LETTER_NUMBER ||
         (ty_uc_props(cp) & TY_UC_OTHER_ALPHABETIC) != 0;
}
/* Uppercase and lowercase are their categories plus Other_Uppercase and
   Other_Lowercase: U+2160 ROMAN NUMERAL ONE is uppercase and U+02B0 MODIFIER
   LETTER SMALL H is lowercase, and neither is an Lu or an Ll. */
int32_t ty_is_upper_case(int32_t cp) {
  return ty_uc_category(cp) == TY_UC_UPPERCASE_LETTER || (ty_uc_props(cp) & TY_UC_OTHER_UPPERCASE) != 0;
}
int32_t ty_is_lower_case(int32_t cp) {
  return ty_uc_category(cp) == TY_UC_LOWERCASE_LETTER || (ty_uc_props(cp) & TY_UC_OTHER_LOWERCASE) != 0;
}
int32_t ty_is_title_case(int32_t cp) { return ty_uc_category(cp) == TY_UC_TITLECASE_LETTER; }
/* Whether a character carries case at all, which the Final_Sigma condition of
   the lowercase mapping asks: Character.isCased is these three categories plus
   the two Other_* properties, and nothing else. */
static int uc_is_cased(int32_t cp) {
  return ty_is_upper_case(cp) || ty_is_lower_case(cp) || ty_is_title_case(cp);
}
/* The whitespace strip() strips and isWhitespace() accepts, which in Java are
   the same set. It is the White_Space property with the JDK's four exceptions
   and its four additions (genunicode says which), and it is deliberately not
   the set trim() strips -- trim takes every code unit <= ' ', which is a
   strictly larger set -- and the two must not be confused, since Java keeps
   them apart. */
int32_t ty_is_whitespace(int32_t cp) { return (ty_uc_props(cp) & TY_UC_WHITE_SPACE) != 0; }
/* isSpaceChar is the three separator categories and not the whitespace set:
   U+00A0 NO-BREAK SPACE is a space character and is not whitespace, and U+001C
   is whitespace and is not a space character. */
int32_t ty_is_space_char(int32_t cp) {
  int32_t t = ty_uc_category(cp);
  return t == TY_UC_SPACE_SEPARATOR || t == TY_UC_LINE_SEPARATOR || t == TY_UC_PARAGRAPH_SEPARATOR;
}
int32_t ty_is_defined(int32_t cp) {
  return cp >= 0 && cp <= 0x10FFFF && ty_uc_category(cp) != TY_UC_UNASSIGNED;
}
/* Character.toUpperCase(int), toLowerCase(int) and toTitleCase(int) are the
   *simple* mapping: one code point in, one code point out, and a character with
   no mapping of its own answers with itself -- U+00DF LATIN SMALL LETTER SHARP
   S is unchanged by toUpperCase, and the two characters it becomes in a string
   are a question about the string (see below). */
int32_t ty_char_upper(int32_t cp) { return ty_uc_case(cp, 0); }
int32_t ty_char_lower(int32_t cp) { return ty_uc_case(cp, 1); }
int32_t ty_char_title(int32_t cp) { return ty_uc_case(cp, 2); }
int32_t ty_char_compare(uint16_t a, uint16_t b) { return a < b ? -1 : (a > b ? 1 : 0); }
tystr *ty_char_tostr_val(uint16_t c) { return ty_str_of_char(c); }
int32_t ty_char_hash_val(uint16_t c) { return (int32_t)c; }

/* Java's getNumericValue answers the numeric value of a numeral (U+216B ROMAN
   NUMERAL TWELVE is 12), the value of a digit in any radix up to 36, -1 for a
   character with neither, and -2 for a character whose numeric value is not a
   non-negative int -- U+00BD VULGAR FRACTION ONE HALF and U+16B60, whose value
   is 10^10. */
int32_t ty_char_numeric(int32_t cp) { return ty_uc_numeric_value(cp); }
/* digit() asks the same question with a radix, and answers -1 for a value that
   is not below the radix as well as for a character that is not a digit: 'f' is
   a digit in radix 16 (15) and is not one in radix 10. A radix outside 2..36
   has no digits at all. */
int32_t ty_char_digit(int32_t cp, int32_t radix) {
  int32_t v;
  if (radix < 2 || radix > 36) return -1;
  v = ty_uc_digit_value(cp);
  return (v < 0 || v >= radix) ? -1 : v;
}

/* ------------------------------------------ the case mappings of a String */

/* A string's case conversion is not the case conversion of each of its code
   points, in three ways.

     - A code point can map to more than one. U+00DF LATIN SMALL LETTER SHARP S
       uppercases to "SS", U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE
       lowercases to "i" and a combining dot above, and the ligatures expand
       the same way. Those mappings are SpecialCasing.txt's, and the ones read
       here are its unconditional entries: the rest are conditioned on a
       language (Turkish, Azeri, Lithuanian), and there is no locale here to
       condition on.
     - U+03A3 GREEK CAPITAL LETTER SIGMA lowercases to U+03C2 the final form
       when it ends a cased word and to U+03C3 otherwise. That is the one
       conditional mapping that is not about a language, and the JDK applies it
       for every locale: "ΟΔΟΣ".toLowerCase() ends in the final form.
     - A supplementary character is one character: it is two code units, and
       folding the units would leave both of them alone.

   So the walk is over code points, and each one is asked for its full mapping
   rather than its simple one. */

/* Whether a code point can be inside a word, which is what the Final_Sigma
   condition is about. The letters, the digits and the marks can; an ideographic
   character cannot, because Java's word iterator breaks a run of them into one
   word each -- which is why the sigma of "ΑΣ中Α" is final here and in the JDK. */
static int uc_word_char(int32_t cp) {
  if ((ty_uc_props(cp) & TY_UC_IDEOGRAPHIC) != 0) return 0;
  switch (ty_uc_category(cp)) {
    case TY_UC_UPPERCASE_LETTER:
    case TY_UC_LOWERCASE_LETTER:
    case TY_UC_TITLECASE_LETTER:
    case TY_UC_MODIFIER_LETTER:
    case TY_UC_OTHER_LETTER:
    case TY_UC_DECIMAL_DIGIT_NUMBER:
    case TY_UC_LETTER_NUMBER:
    case TY_UC_OTHER_NUMBER:
    case TY_UC_NON_SPACING_MARK:
    case TY_UC_COMBINING_SPACING_MARK:
    case TY_UC_ENCLOSING_MARK:
      return 1;
    default:
      return 0;
  }
}
/* The punctuation a word continues through. This set is measured against the
   JDK rather than read off UAX #29, because the JDK's word iterator is its own
   rule-based one and is not UAX #29: it keeps a hyphen inside a word, it joins
   through a period or an apostrophe only when a letter is on both sides, and it
   breaks at a middle dot or a comma. See AGENTS.md §10 for what the difference
   is measured to be. */
static int uc_joins(int32_t cp) {
  return cp == '"' || cp == '\'' || cp == '-' || cp == '.' || cp == 0xAD ||
         ty_uc_category(cp) == TY_UC_CONNECTOR_PUNCTUATION;
}
/* A mark or a format character attaches to the character before it and so can
   never be a word boundary itself. U+200D ZERO WIDTH JOINER is one of these,
   which is why a word continues across it. */
static int uc_transparent(int32_t cp) {
  switch (ty_uc_category(cp)) {
    case TY_UC_NON_SPACING_MARK:
    case TY_UC_COMBINING_SPACING_MARK:
    case TY_UC_ENCLOSING_MARK:
    case TY_UC_FORMAT:
      return 1;
    default:
      return 0;
  }
}

/* The code points of a string, by unit index. codePointAt and codePointBefore
   in tyrt.h raise on an index outside the string; these are for callers that
   have already established the index is inside it. */
static int32_t uc_cp_at(tystr *s, int64_t i) {
  uint16_t c = str_unit_at(s, i);
  if (unit_is_high(c) && i + 1 < s->ulen) {
    uint16_t d = str_unit_at(s, i + 1);
    if (unit_is_low(d)) return 0x10000 + ((c - 0xD800) << 10) + (d - 0xDC00);
  }
  return c;
}
static int32_t uc_cp_before(tystr *s, int64_t i) {
  uint16_t c = str_unit_at(s, i - 1);
  if (unit_is_low(c) && i >= 2) {
    uint16_t d = str_unit_at(s, i - 2);
    if (unit_is_high(d)) return 0x10000 + ((d - 0xD800) << 10) + (c - 0xDC00);
  }
  return c;
}
static int64_t uc_cp_len(tystr *s, int64_t i) {
  uint16_t c = str_unit_at(s, i);
  return (unit_is_high(c) && i + 1 < s->ulen && unit_is_low(str_unit_at(s, i + 1))) ? 2 : 1;
}
/* The unit index of the character before i, with everything that attaches to it
   skipped: the base character is what a word boundary is decided between. */
static int64_t uc_base_before(tystr *s, int64_t i) {
  int64_t j = i - 1;
  while (j > 0 && uc_transparent(uc_cp_before(s, j + 1))) j--;
  return j;
}
/* The unit index of the character after the one starting at i, with everything
   that attaches to it skipped. */
static int64_t uc_base_after(tystr *s, int64_t i) {
  int64_t j = i + uc_cp_len(s, i);
  while (j < s->ulen && uc_transparent(uc_cp_at(s, j))) j += uc_cp_len(s, j);
  return j;
}

/* No word boundary at unit position i, which is between the characters on
   either side of it. Only the positions inside the string are asked about: the
   two ends are boundaries, and the callers say so by their loop conditions. */
static int str_no_boundary(tystr *s, int64_t i) {
  int32_t c1, c2;
  int64_t j, k;
  if (i <= 0 || i >= s->ulen) return 0;
  c2 = uc_cp_at(s, i);
  if (uc_transparent(c2)) return 1;
  j = uc_base_before(s, i);
  c1 = uc_cp_at(s, j);
  if (uc_word_char(c1) && uc_word_char(c2)) return 1;
  if (uc_joins(c1) && TY_CAT_IS_LETTER(ty_uc_category(c2))) {
    k = uc_base_before(s, j);
    if (k < j && TY_CAT_IS_LETTER(ty_uc_category(uc_cp_at(s, k)))) return 1;
  }
  if (TY_CAT_IS_LETTER(ty_uc_category(c1)) && uc_joins(c2)) {
    k = uc_base_after(s, i);
    if (k < s->ulen && TY_CAT_IS_LETTER(ty_uc_category(uc_cp_at(s, k)))) return 1;
  }
  return 0;
}

/* Java's Final_Sigma condition, which is what decides whether U+03A3 is the
   final form: the character at unit index `index` is final-cased when there is
   a cased character before it in its word and none after it in the same word.
   The JDK asks this of its BreakIterator; str_no_boundary is what answers here,
   and what it does not reproduce is recorded in AGENTS.md §10. */
static int str_final_cased(tystr *s, int64_t index) {
  int64_t i = index;
  while (i > 0 && str_no_boundary(s, i)) {
    if (uc_is_cased(uc_cp_before(s, i))) {
      int64_t j = index + uc_cp_len(s, index);
      while (j < s->ulen && str_no_boundary(s, j)) {
        if (uc_is_cased(uc_cp_at(s, j))) return 0;
        j += uc_cp_len(s, j);
      }
      return 1;
    }
    i = uc_base_before(s, i);
  }
  return 0;
}

/* The full case mapping of the character starting at unit `at`, written to out
   as code points: the number written is returned, and it is never more than
   TY_UC_MAX_EXPANSION. */
static int32_t str_case_of(tystr *s, int64_t at, int32_t upper, int32_t *out) {
  int32_t cp = uc_cp_at(s, at);
  int32_t n;
  if (!upper && cp == 0x3A3) {
    out[0] = str_final_cased(s, at) ? 0x3C2 : 0x3C3;
    return 1;
  }
  n = ty_uc_special(cp, upper, out, TY_UC_MAX_EXPANSION);
  if (n > 0) return n;
  out[0] = ty_uc_case(cp, upper ? 0 : 1);
  return 1;
}

/* The case conversion of a whole string. An ASCII string is mapped byte by byte
   in place: no code point below U+0080 has a full case mapping, so the
   arithmetic is the whole answer, and it is what nearly every call wants. */
static tystr *str_case(tystr *s, int32_t upper) {
  int32_t buf[TY_UC_MAX_EXPANSION];
  int64_t at, i, units = 0, k = 0;
  uint16_t *tmp;
  tystr *r;
  if (TY_STR_ASCII(s)) {
    r = ty_str_new(TY_STR_DATA(s), s->blen);
    for (at = 0; at < r->blen; at++) {
      unsigned char c = (unsigned char)TY_STR_DATA(r)[at];
      if (upper) {
        if (c >= 'a' && c <= 'z') TY_STR_DATA(r)[at] = (char)(c - 32);
      } else if (c >= 'A' && c <= 'Z') {
        TY_STR_DATA(r)[at] = (char)(c + 32);
      }
    }
    return r;
  }
  /* The answer is a string, and a string is allocated once: the units are
     counted first, then written. An expansion can be longer than what it
     expands, which is why the count is not the input's. */
  for (at = 0; at < s->ulen;) {
    int32_t n = str_case_of(s, at, upper, buf);
    for (i = 0; i < n; i++) units += buf[i] >= 0x10000 ? 2 : 1;
    at += uc_cp_len(s, at);
  }
  tmp = (uint16_t *)malloc((size_t)(units > 0 ? units : 1) * sizeof(uint16_t));
  for (at = 0; at < s->ulen;) {
    int32_t n = str_case_of(s, at, upper, buf);
    for (i = 0; i < n; i++) {
      if (buf[i] >= 0x10000) {
        int32_t v = buf[i] - 0x10000;
        tmp[k++] = (uint16_t)(0xD800 + (v >> 10));
        tmp[k++] = (uint16_t)(0xDC00 + (v & 0x3FF));
      } else {
        tmp[k++] = (uint16_t)buf[i];
      }
    }
    at += uc_cp_len(s, at);
  }
  r = ty_str_of_units(tmp, units);
  free(tmp);
  return r;
}
tystr *ty_str_upper(tystr *s) { return s ? str_case(s, 1) : NULL; }
tystr *ty_str_lower(tystr *s) { return s ? str_case(s, 0) : NULL; }

/* ---------------------------------------------------- the wrappers' values */

int32_t ty_byte_hash_val(int32_t v) { return v; }
int32_t ty_short_hash_val(int32_t v) { return v; }
int32_t ty_int_hash_val(int32_t v) { return v; }
/* Long.hashCode(long) is the two halves folded together, and Double's is the
   same fold over the bits -- Java defines both that way so that a map keyed by
   the box and a map keyed by the primitive agree. */
int32_t ty_long_hash_val(int64_t v) { return (int32_t)(v ^ (int64_t)((uint64_t)v >> 32)); }
int32_t ty_double_hash_val(double v) {
  int64_t b = bits_of_double(v);
  return (int32_t)(b ^ (int64_t)((uint64_t)b >> 32));
}
int32_t ty_bool_hash_val(int32_t v) { return v ? 1231 : 1237; }
/* Boolean.hashCode() is that same class hash and not the raw value the
   unboxing helper answers with; a Boolean's %h used to print 1. */
int32_t ty_bool_hash_box(void *o) { return ty_unbox_bool(o) ? 1231 : 1237; }
int32_t ty_identity_hash(void *o) { return ty_obj_hash(o); }

tystr *ty_byte_tostr_val(int32_t v) { return ty_str_of_int(v); }
tystr *ty_short_tostr_val(int32_t v) { return ty_str_of_int(v); }
tystr *ty_float_tostr_val(float v) { return ty_str_of_float(v); }

/* Radix formatting. The digits come out least significant first into a buffer
   that is filled from the end, so no reversal is needed. A radix outside 2..36
   is not an error in Java: toString and toUnsignedString fall back to 10, and
   the fallback is here rather than in Teyru because it is what the JDK does. */
static tystr *radix_str(uint64_t mag, int neg, int32_t radix) {
  char buf[72];
  int32_t i = 72;
  if (mag == 0) buf[--i] = '0';
  while (mag) {
    int32_t d = (int32_t)(mag % (uint64_t)radix);
    buf[--i] = (char)(d < 10 ? '0' + d : 'a' + d - 10);
    mag /= (uint64_t)radix;
  }
  if (neg) buf[--i] = '-';
  return ty_str_new(buf + i, 72 - i);
}
tystr *ty_radix_string_int(int32_t v, int32_t radix) {
  if (radix < 2 || radix > 36) radix = 10;
  if (v < 0) return radix_str((uint64_t)(0u - (uint32_t)v), 1, radix);
  return radix_str((uint64_t)(uint32_t)v, 0, radix);
}
tystr *ty_radix_string_long(int64_t v, int32_t radix) {
  if (radix < 2 || radix > 36) radix = 10;
  if (v < 0) return radix_str(0ull - (uint64_t)v, 1, radix);
  return radix_str((uint64_t)v, 0, radix);
}
/* toBinaryString, toOctalString and toHexString read the value as unsigned:
   -1 is all ones, which is what makes them useful on a mask. */
tystr *ty_unsigned_string_int(int32_t v, int32_t radix) {
  if (radix < 2 || radix > 36) radix = 10;
  return radix_str((uint64_t)(uint32_t)v, 0, radix);
}
tystr *ty_unsigned_string_long(int64_t v, int32_t radix) {
  if (radix < 2 || radix > 36) radix = 10;
  return radix_str((uint64_t)v, 0, radix);
}

/* Bit twiddling, all of it defined by the JDK down to the empty cases: the
   count of leading zeros of zero is the width of the type, reverse reverses the
   bit order, and a rotation by more than the width is a rotation by the width's
   remainder. The builtins the compilers provide are used where they exist,
   because they compile to a single instruction. */
int32_t ty_int_bit_count(int32_t v) { return __builtin_popcount((uint32_t)v); }
int32_t ty_long_bit_count(int64_t v) { return __builtin_popcountll((uint64_t)v); }
int32_t ty_int_nlz(int32_t v) { return v == 0 ? 32 : __builtin_clz((uint32_t)v); }
int32_t ty_int_ntz(int32_t v) { return v == 0 ? 32 : __builtin_ctz((uint32_t)v); }
int32_t ty_long_nlz(int64_t v) { return v == 0 ? 64 : __builtin_clzll((uint64_t)v); }
int32_t ty_long_ntz(int64_t v) { return v == 0 ? 64 : __builtin_ctzll((uint64_t)v); }
int32_t ty_int_highest_one(int32_t v) { return v == 0 ? 0 : (int32_t)(1u << (31 - ty_int_nlz(v))); }
int32_t ty_int_lowest_one(int32_t v) { return v == 0 ? 0 : (int32_t)((uint32_t)v & (0u - (uint32_t)v)); }
int64_t ty_long_highest_one(int64_t v) { return v == 0 ? 0 : (int64_t)(1ull << (63 - ty_long_nlz(v))); }
int64_t ty_long_lowest_one(int64_t v) { return v == 0 ? 0 : (int64_t)((uint64_t)v & (0ull - (uint64_t)v)); }
int32_t ty_int_reverse(int32_t v) {
  uint32_t x = (uint32_t)v;
  x = ((x & 0x55555555u) << 1) | ((x >> 1) & 0x55555555u);
  x = ((x & 0x33333333u) << 2) | ((x >> 2) & 0x33333333u);
  x = ((x & 0x0F0F0F0Fu) << 4) | ((x >> 4) & 0x0F0F0F0Fu);
  x = ((x & 0x00FF00FFu) << 8) | ((x >> 8) & 0x00FF00FFu);
  return (int32_t)((x << 16) | (x >> 16));
}
int64_t ty_long_reverse(int64_t v) {
  uint64_t x = (uint64_t)v;
  x = ((x & 0x5555555555555555ull) << 1) | ((x >> 1) & 0x5555555555555555ull);
  x = ((x & 0x3333333333333333ull) << 2) | ((x >> 2) & 0x3333333333333333ull);
  x = ((x & 0x0F0F0F0F0F0F0F0Full) << 4) | ((x >> 4) & 0x0F0F0F0F0F0F0F0Full);
  x = ((x & 0x00FF00FF00FF00FFull) << 8) | ((x >> 8) & 0x00FF00FF00FF00FFull);
  x = ((x & 0x0000FFFF0000FFFFull) << 16) | ((x >> 16) & 0x0000FFFF0000FFFFull);
  return (int64_t)((x << 32) | (x >> 32));
}
int32_t ty_int_reverse_bytes(int32_t v) { return (int32_t)__builtin_bswap32((uint32_t)v); }
int64_t ty_long_reverse_bytes(int64_t v) { return (int64_t)__builtin_bswap64((uint64_t)v); }
/* A rotation masks its distance to the width of the type, so rotateLeft(v, 32)
   of an int is v and not undefined: `32 & 31` is 0 and the shift is by zero. */
int32_t ty_int_rotate_left(int32_t v, int32_t d) {
  d &= 31;
  return (int32_t)(((uint32_t)v << d) | ((uint32_t)v >> ((32 - d) & 31)));
}
int32_t ty_int_rotate_right(int32_t v, int32_t d) {
  d &= 31;
  return (int32_t)(((uint32_t)v >> d) | ((uint32_t)v << ((32 - d) & 31)));
}
int64_t ty_long_rotate_left(int64_t v, int32_t d) {
  d &= 63;
  return (int64_t)(((uint64_t)v << d) | ((uint64_t)v >> ((64 - d) & 63)));
}
int64_t ty_long_rotate_right(int64_t v, int32_t d) {
  d &= 63;
  return (int64_t)(((uint64_t)v >> d) | ((uint64_t)v << ((64 - d) & 63)));
}
int32_t ty_int_signum(int32_t v) { return (v >> 31) | (int32_t)((uint32_t)(-(uint32_t)v) >> 31); }
int64_t ty_long_signum(int64_t v) { return (v >> 63) | (int64_t)((uint64_t)(-(uint64_t)v) >> 63); }
int32_t ty_int_cmp_unsigned(int32_t a, int32_t b) {
  uint32_t x = (uint32_t)a, y = (uint32_t)b;
  return x < y ? -1 : (x > y ? 1 : 0);
}
int64_t ty_long_cmp_unsigned(int64_t a, int64_t b) {
  uint64_t x = (uint64_t)a, y = (uint64_t)b;
  return x < y ? -1 : (x > y ? 1 : 0);
}
/* The predicates behind Double.isInfinite/isFinite and Float's, written as
   comparisons rather than isnan/isinf so that they stay true under any
   floating-point flags the C compiler is given: a NaN is the only value that
   is not equal to itself, and the infinities are the only ones equal to the
   folded 1.0/0.0. */
int32_t ty_double_is_infinite(double v) { return (v == 1.0 / 0.0 || v == -1.0 / 0.0) ? 1 : 0; }
int32_t ty_double_is_finite(double v) {
  return (v == v && v != 1.0 / 0.0 && v != -1.0 / 0.0) ? 1 : 0;
}
int32_t ty_float_is_infinite(float v) { return (v == 1.0f / 0.0f || v == -1.0f / 0.0f) ? 1 : 0; }
int32_t ty_float_is_finite(float v) {
  return (v == v && v != 1.0f / 0.0f && v != -1.0f / 0.0f) ? 1 : 0;
}
int32_t ty_float_isnan(float v) { return v != v ? 1 : 0; }
/* The bit views behind Double.doubleToLongBits/longBitsToDouble and Float's.
   Java tells the two NaN readings apart: floatToIntBits collapses every NaN to
   one pattern (0x7fc00000, 0x7ff8000000000000) while floatToRawIntBits hands
   back the bits the value really has, so the raw form needs an entry of its
   own rather than sharing this one. The copies go through memcpy because
   reading a double through an int64_t pointer is what C forbids. */
int64_t ty_double_raw_bits(double v) {
  int64_t b;
  memcpy(&b, &v, sizeof b);
  return b;
}
int64_t ty_double_bits(double v) {
  if (v != v) return (int64_t)0x7ff8000000000000LL;
  return ty_double_raw_bits(v);
}
double ty_bits_double(int64_t b) {
  double v;
  memcpy(&v, &b, sizeof v);
  return v;
}
int32_t ty_float_raw_bits(float v) {
  int32_t b;
  memcpy(&b, &v, sizeof b);
  return b;
}
int32_t ty_float_bits(float v) {
  if (v != v) return (int32_t)0x7fc00000;
  return ty_float_raw_bits(v);
}
float ty_bits_float(int32_t b) {
  float v;
  memcpy(&v, &b, sizeof v);
  return v;
}
/* Boolean.compare is `x == y ? 0 : (x ? 1 : -1)`, not a subtraction: the two
   values are 0 and 1 and Java orders false first, which a subtraction of the
   raw ints would also give -- but writing the rule down keeps it from
   depending on that. */
int32_t ty_bool_compare(int32_t a, int32_t b) {
  if (a == b) return 0;
  return a ? 1 : -1;
}
/* Every wrapper's equals, and the one place the class of the argument matters.
   Java's contract is `o instanceof Integer && value == ((Integer)o).intValue()`
   -- the class first, the value second -- so an Integer never equals a Long,
   and Boolean.TRUE never equals an Integer holding 1. tyrt2.c's helpers
   (ty_int_equals and its siblings) compare the values alone, which made every
   numeric wrapper equal every other one carrying the same number; Byte and
   Short had no helper at all and their equals compiled to a constant false.
   One function serves all eight wrappers because the receiver's class says
   which wrapper this is: if the two classes differ the answer is already no,
   and if they agree the payload decides. */
int32_t ty_box_equals(void *a, void *b) {
  tyclass *ca, *cb;
  int32_t i;
  if (!a) ty_npe();
  if (!b) return 0;
  ca = ((tyobj *)a)->cls;
  cb = ((tyobj *)b)->cls;
  if (ca != cb) return 0;
  for (i = 1; i <= 8; i++)
    if (ca == TY_BOX[i]) break;
  switch (i) {
  case 1: return ((tyboolbox *)a)->v == ((tyboolbox *)b)->v;
  case 2: return ((tybytebox *)a)->v == ((tybytebox *)b)->v;
  case 3: return ((tyshortbox *)a)->v == ((tyshortbox *)b)->v;
  case 4: return ((tycharbox *)a)->v == ((tycharbox *)b)->v;
  case 5: return ((tyintbox *)a)->v == ((tyintbox *)b)->v;
  case 6: return ((tylongbox *)a)->v == ((tylongbox *)b)->v;
  /* the two floating types compare through floatToIntBits, so NaN equals NaN
     and 0.0f is not -0.0f -- the same collapse ty_float_bits and ty_double_bits
     already give hashCode and compare */
  case 7: return ty_float_bits(((tyfloatbox *)a)->v) == ty_float_bits(((tyfloatbox *)b)->v);
  default: return ty_double_bits(((tydoublebox *)a)->v) == ty_double_bits(((tydoublebox *)b)->v);
  }
}

/* -------------------------------------------------------------- parsing */

/* The code unit at index i, with the ASCII case answered from the string's own
   bytes: the parsers walk a string one unit at a time, and the strings they are
   handed are nearly always ASCII, where a unit index is a byte index. */
static int32_t str_unit_fast(tystr *s, int64_t i) {
  if (TY_STR_ASCII(s)) return (unsigned char)TY_STR_DATA(s)[i];
  return (int32_t)str_unit_at(s, i);
}

/* The digit loop of Java's parsers, written the JDK's way: the accumulator is
   negative and the limit is the type's minimum, so that the most negative value
   of a type needs no special case, and every overflow test is a comparison
   before the operation that would overflow rather than after it. 0 means Java's
   parser would reject the string: an empty string, a lone sign, a digit outside
   the radix, or a value that does not fit the type.

   It walks code units and asks Character.digit about each one, which is what
   Java's parser does -- so "１２３" is 123, since U+FF11 is a decimal digit, and
   an OSMANYA digit above the BMP is not a number, since Java reads its two
   surrogate units as two characters that are not digits. */
static int parse_int(tystr *s, int32_t radix, int64_t limit_pos, int64_t limit_neg,
                     int64_t *out) {
  int64_t n = s->ulen;
  int64_t i = 0, result = 0, limit = limit_pos, multmin;
  int neg = 0;
  int32_t first;
  if (n <= 0 || radix < 2 || radix > 36) return 0;
  first = str_unit_fast(s, 0);
  if (first < '0') {
    if (first == '-') { neg = 1; limit = limit_neg; }
    else if (first != '+') return 0;
    if (n == 1) return 0;
    i = 1;
  }
  multmin = limit / radix;
  for (; i < n; i++) {
    int32_t digit = ty_char_digit(str_unit_fast(s, i), radix);
    if (digit < 0 || result < multmin) return 0;
    result *= radix;
    if (result < limit + digit) return 0;
    result -= digit;
  }
  *out = neg ? result : -result;
  return 1;
}
int32_t ty_str_parsable_int(tystr *s, int32_t radix) {
  int64_t v;
  if (!s) return 0;
  return parse_int(s, radix, -(int64_t)INT32_MAX, INT32_MIN, &v);
}
int64_t ty_str_parsable_long(tystr *s, int32_t radix) {
  int64_t v;
  if (!s) return 0;
  return parse_int(s, radix, -INT64_MAX, INT64_MIN, &v);
}
/* The two conversions below run only on a string the parsable test above has
   already accepted, so the failure return is unreachable and the value is the
   one Java would produce. They exist as functions of their own because the
   radix has to reach them: parseXxx is a Teyru method that has to raise
   NumberFormatException itself, since the runtime cannot raise a class whose
   tyclass the generated startup never installs. */
int32_t ty_str_toint_radix(tystr *s, int32_t radix) {
  int64_t v = 0;
  if (!s) ty_npe();
  parse_int(s, radix, -(int64_t)INT32_MAX, INT32_MIN, &v);
  return (int32_t)v;
}
int64_t ty_str_tolong_radix(tystr *s, int32_t radix) {
  int64_t v = 0;
  if (!s) ty_npe();
  parse_int(s, radix, -INT64_MAX, INT64_MIN, &v);
  return v;
}

/* Float and double literals have their own grammar, and it is not the one
   strtod accepts: strtod skips leading space, stops at trailing rubbish, and
   accepts "inf" and "0x1.8" without an exponent. The test below is Java's
   grammar, and it is deliberately narrower than strtod's, so that anything the
   conversion is handed is a string strtod reads the same way the JDK's reader
   does. */
static int parsable_dec_part(const char *d, int64_t i, int64_t end) {
  int64_t p = i;
  int digits = 0, ed = 0;
  while (p < end && TY_ASCII_DIGIT(d[p])) { p++; digits++; }
  if (p < end && d[p] == '.') {
    p++;
    while (p < end && TY_ASCII_DIGIT(d[p])) { p++; digits++; }
  }
  if (digits == 0) return 0;
  if (p < end && (d[p] == 'e' || d[p] == 'E')) {
    p++;
    if (p < end && (d[p] == '+' || d[p] == '-')) p++;
    while (p < end && TY_ASCII_DIGIT(d[p])) { p++; ed++; }
    if (ed == 0) return 0;
  }
  return p == end;
}
static int parsable_hex_part(const char *d, int64_t i, int64_t end) {
  int64_t p = i + 2;
  int digits = 0, ed = 0;
  while (p < end && ty_char_numeric((unsigned char)d[p]) >= 0 && ty_char_numeric((unsigned char)d[p]) < 16) { p++; digits++; }
  if (p < end && d[p] == '.') {
    p++;
    while (p < end && ty_char_numeric((unsigned char)d[p]) >= 0 && ty_char_numeric((unsigned char)d[p]) < 16) { p++; digits++; }
  }
  if (digits == 0) return 0;
  /* the binary exponent is mandatory here: "0x1.8" is not a literal Java reads */
  if (p >= end || (d[p] != 'p' && d[p] != 'P')) return 0;
  p++;
  if (p < end && (d[p] == '+' || d[p] == '-')) p++;
  while (p < end && TY_ASCII_DIGIT(d[p])) { p++; ed++; }
  if (ed == 0) return 0;
  return p == end;
}
static int parsable_float_str(const char *d, int64_t n) {
  int64_t i = 0, end = n;
  /* Double.parseDouble and Float.parseFloat ignore leading and trailing
     whitespace, "as if by the String.trim() method": every code unit <= ' ' at
     either end and nothing else, so a U+00A0 is not whitespace to them and is
     refused below. The conversion itself needs no trimming -- strtod skips the
     leading whitespace and stops at the trailing whitespace -- so only this
     grammar test has to look past it. */
  while (i < end && (unsigned char)d[i] <= ' ') i++;
  while (end > i && (unsigned char)d[end - 1] <= ' ') end--;
  if (i < n && (d[i] == '+' || d[i] == '-')) i++;
  if (n - i == 3 && memcmp(d + i, "NaN", 3) == 0) return 1;
  if (n - i == 8 && memcmp(d + i, "Infinity", 8) == 0) return 1;
  if (i >= end) return 0;
  if (d[end - 1] == 'f' || d[end - 1] == 'F' || d[end - 1] == 'd' || d[end - 1] == 'D') end--;
  if (i >= end) return 0;
  if (end - i > 2 && d[i] == '0' && (d[i + 1] == 'x' || d[i + 1] == 'X'))
    return parsable_hex_part(d, i, end);
  return parsable_dec_part(d, i, end);
}
int32_t ty_str_parsable_double(tystr *s) {
  return s ? parsable_float_str(TY_STR_DATA(s), s->blen) : 0;
}
int32_t ty_str_parsable_float(tystr *s) {
  return s ? parsable_float_str(TY_STR_DATA(s), s->blen) : 0;
}
/* The conversion itself is strtod/strtof, which are correctly rounded -- the
   same rounding Java's reader performs, and the reason a value read here and
   printed with %f matches what Java prints. The trailing f/F/d/D suffix needs
   no handling: strtod stops at it. Java's two spellings of the special values
   stop at nothing, since strtod reads "Infinity" and "NaN" too. */
double ty_str_todouble_val(tystr *s) { return s ? strtod(TY_STR_DATA(s), NULL) : 0.0; }
float ty_str_tofloat_val(tystr *s) { return s ? strtof(TY_STR_DATA(s), NULL) : 0.0f; }

/* --------------------------------------------------------------- String */

/* The case-insensitive comparisons: compareToIgnoreCase, equalsIgnoreCase and
   regionMatches(ignoreCase,...) are Java's algorithm, which is a unit at a time
   first -- what nearly every comparison is, and it needs no index arithmetic --
   and only where two units differ does it look for the supplementary character
   one of them may be half of, because "𐐨" and "𐐀" are one character each and
   folding their two units would leave both alone. */
/* Java's compareCodePointCI: the difference between two folded code points, 0
   when they are equal ignoring case. The fold is two steps -- uppercase first
   and lowercase of *that* -- and the two steps are not the same as lowercasing
   once: the dotless i (U+0131) and 'i' are equal here because both uppercase to
   'I', and neither lowercases to the other. */
static int32_t uc_cmp_ci(int32_t a, int32_t b) {
  int32_t ua, ub;
  if (a == b) return 0;
  ua = ty_char_upper(a);
  ub = ty_char_upper(b);
  if (ua == ub) return 0;
  ua = ty_char_lower(ua);
  ub = ty_char_lower(ub);
  return ua - ub;
}
/* Java's codePointIncluding: the code point the unit at `index` is part of,
   within the range [start, end) of the comparison, negated when the unit turned
   out to be one half of it -- the caller then walks one unit further. */
static int32_t str_cp_including(tystr *s, int32_t unit, int64_t index, int64_t start, int64_t end) {
  if (unit_is_high(unit)) {
    if (index + 1 < end) {
      uint16_t c = str_unit_at(s, index + 1);
      if (unit_is_low(c)) return -(0x10000 + ((unit - 0xD800) << 10) + (c - 0xDC00));
    }
  } else if (unit_is_low(unit) && index > start) {
    uint16_t c = str_unit_at(s, index - 1);
    if (unit_is_high(c)) return -(0x10000 + ((c - 0xD800) << 10) + (unit - 0xDC00));
  }
  return unit;
}
int32_t ty_str_cmp_ic(tystr *a, tystr *b) {
  int64_t k1, k2, tlast, olast;
  if (!a || !b) ty_npe();
  tlast = a->ulen;
  olast = b->ulen;
  for (k1 = 0, k2 = 0; k1 < tlast && k2 < olast; k1++, k2++) {
    int32_t cp1 = str_unit_fast(a, k1), cp2 = str_unit_fast(b, k2), diff;
    if (cp1 == cp2 || uc_cmp_ci(cp1, cp2) == 0) continue;
    cp1 = str_cp_including(a, cp1, k1, 0, tlast);
    if (cp1 < 0) { k1++; cp1 = -cp1; }
    cp2 = str_cp_including(b, cp2, k2, 0, olast);
    if (cp2 < 0) { k2++; cp2 = -cp2; }
    diff = uc_cmp_ci(cp1, cp2);
    if (diff != 0) return diff;
  }
  return (int32_t)(tlast - olast);
}
int32_t ty_str_eq_ic(tystr *a, tystr *b) {
  if (a == b) return 1;
  if (!a || !b || a->ulen != b->ulen) return 0;
  return ty_str_cmp_ic(a, b) == 0;
}
/* regionMatches, the case-sensitive and the case-insensitive one in a single
   helper because the bounds are the same and only the comparison differs.
   Where both ranges begin and end on sequence boundaries the bytes are the
   comparison: canonical encoding is a function of the unit sequence, so equal
   units give equal bytes. The case-insensitive comparison is Java's
   compareToCIImpl over the two ranges, which is why it is written the same way
   as compareToIgnoreCase above rather than as a second fold. */
int32_t ty_str_region_matches(tystr *s, int32_t ignore_case, int32_t toff,
                              tystr *o, int32_t ooff, int32_t len) {
  int64_t i, ia, ib, la, lb;
  if (!s || !o) ty_npe();
  if (toff < 0 || ooff < 0) return 0;
  if (toff > s->ulen - len || ooff > o->ulen - len) return 0;
  if (len <= 0) return 1; /* a negative length compares nothing, as in Java */
  if (!ignore_case && str_on_boundary(s, toff) && str_on_boundary(s, toff + len) &&
      str_on_boundary(o, ooff) && str_on_boundary(o, ooff + len)) {
    int64_t a = str_byte_at(s, toff), b = str_byte_at(o, ooff);
    int64_t na = str_byte_at(s, toff + len) - a, nb = str_byte_at(o, ooff + len) - b;
    return na == nb && memcmp(TY_STR_DATA(s) + a, TY_STR_DATA(o) + b, (size_t)na) == 0;
  }
  if (!ignore_case) {
    tyucur ca, cb;
    ucur_at(&ca, s, toff);
    ucur_at(&cb, o, ooff);
    for (i = 0; i < len; i++)
      if (ucur_next(&ca) != ucur_next(&cb)) return 0;
    return 1;
  }
  la = (int64_t)toff + len;
  lb = (int64_t)ooff + len;
  for (ia = toff, ib = ooff; ia < la && ib < lb; ia++, ib++) {
    int32_t cp1 = str_unit_fast(s, ia), cp2 = str_unit_fast(o, ib);
    if (cp1 == cp2 || uc_cmp_ci(cp1, cp2) == 0) continue;
    cp1 = str_cp_including(s, cp1, ia, toff, la);
    if (cp1 < 0) { ia++; cp1 = -cp1; }
    cp2 = str_cp_including(o, cp2, ib, ooff, lb);
    if (cp2 < 0) { ib++; cp2 = -cp2; }
    if (uc_cmp_ci(cp1, cp2) != 0) return 0;
  }
  return 1;
}
int32_t ty_str_starts_from(tystr *s, tystr *p, int32_t from) {
  if (!s || !p) ty_npe();
  if (from < 0 || from > s->ulen - p->ulen) return 0;
  /* the same rule as startsWith: a byte prefix where the offset is a boundary,
     a unit walk where it is not or where the needle ends inside a pair */
  if (str_on_boundary(s, from)) {
    int64_t at = from == 0 ? 0 : str_byte_at(s, from);
    if (at + p->blen <= s->blen &&
        memcmp(TY_STR_DATA(s) + at, TY_STR_DATA(p), (size_t)p->blen) == 0)
      return 1;
  }
  return str_unit_range_eq(s, from, p);
}

/* indexOf(int) searches for one code unit, and indexOf(int) with a value above
   0xFFFF searches for the two units of that code point -- Java's rule, and the
   reason a supplementary character can be found by either spelling. */
static int64_t find_cp(tystr *s, int32_t ch, int64_t from) {
  int64_t i;
  if (ch < 0 || ch > 0x10FFFF) return -1;
  if (from < 0) from = 0;
  if (from >= s->ulen) return -1;
  if (ch <= 0xFFFF) {
    uint16_t want = (uint16_t)ch;
    if (TY_STR_ASCII(s)) {
      const char *d = TY_STR_DATA(s);
      if (want >= 0x80) return -1;
      for (i = from; i < s->blen; i++)
        if ((unsigned char)d[i] == want) return i;
      return -1;
    }
    {
      tyucur c;
      ucur_at(&c, s, from);
      for (i = from; i < s->ulen; i++)
        if (ucur_next(&c) == want) return i;
      return -1;
    }
  }
  {
    int32_t hi = 0xD800 + ((ch - 0x10000) >> 10);
    int32_t lo = 0xDC00 + ((ch - 0x10000) & 0x3FF);
    tyucur c;
    uint16_t a, b;
    if (TY_STR_ASCII(s) || from > s->ulen - 2) return -1;
    ucur_at(&c, s, from);
    b = ucur_next(&c); /* the unit at from */
    for (i = from; i + 1 < s->ulen; i++) {
      a = b;
      b = ucur_next(&c);
      if (a == hi && b == lo) return i;
    }
    return -1;
  }
}
int32_t ty_str_indexof_ch(tystr *s, int32_t c) {
  if (!s) ty_npe();
  return (int32_t)find_cp(s, c, 0);
}
int32_t ty_str_indexof_ch_from(tystr *s, int32_t c, int32_t from) {
  if (!s) ty_npe();
  return (int32_t)find_cp(s, c, from); /* a negative from is 0, Java's rule */
}
int32_t ty_str_indexof_from(tystr *s, tystr *sub, int32_t from) {
  if (!s || !sub) ty_npe();
  return (int32_t)str_find(s, sub, from);
}
int32_t ty_str_lastindexof(tystr *s, tystr *sub) {
  if (!s || !sub) ty_npe();
  return ty_str_lastindexof_from(s, sub, (int32_t)s->ulen);
}
/* Java's lastIndexOf answers the largest k <= from at which the needle starts;
   for a needle that is empty that is min(from, length). `from` is clamped the
   way Java clamps it, by the unit counts. The needle that can sit inside a pair
   -- or a `from` that does -- takes the unit walk, because the byte scan cannot
   see those matches; where it can, the scan runs backwards by bytes and the
   answer is the first offset it finds, capped at the sequence holding `from`. */
int32_t ty_str_lastindexof_from(tystr *s, tystr *sub, int32_t from) {
  const char *d, *p;
  int64_t at, max;
  if (!s || !sub) ty_npe();
  if (from < 0) return -1;
  max = s->ulen - sub->ulen;
  if (from > max) from = (int32_t)max;
  if (sub->ulen == 0) return from < 0 ? -1 : from;
  if (from < 0) return -1;
  if (str_cuts_pair(sub) || !str_on_boundary(s, from)) {
    int64_t k;
    for (k = from; k >= 0; k--)
      if (str_unit_range_eq(s, k, sub)) return (int32_t)k;
    return -1;
  }
  d = TY_STR_DATA(s);
  p = TY_STR_DATA(sub);
  at = str_byte_at(s, from);
  if (at > s->blen - sub->blen) at = s->blen - sub->blen;
  for (; at >= 0; at--) {
    if (d[at] != p[0]) continue;
    if (memcmp(d + at, p, (size_t)sub->blen) == 0) return (int32_t)unit_of_byte(s, at);
  }
  return -1;
}
static int64_t rfind_cp(tystr *s, int32_t ch, int64_t from) {
  int64_t i;
  if (ch < 0 || ch > 0x10FFFF) return -1;
  if (from < 0) return -1;
  if (ch > 0xFFFF) {
    int32_t hi = 0xD800 + ((ch - 0x10000) >> 10);
    int32_t lo = 0xDC00 + ((ch - 0x10000) & 0x3FF);
    if (from > s->ulen - 2) from = s->ulen - 2;
    for (i = from; i >= 0; i--)
      if (str_unit_at(s, i) == (uint16_t)hi && str_unit_at(s, i + 1) == (uint16_t)lo)
        return i;
    return -1;
  }
  {
    uint16_t want = (uint16_t)ch;
    if (TY_STR_ASCII(s)) {
      const char *d = TY_STR_DATA(s);
      if (from > s->blen - 1) from = s->blen - 1;
      if (want >= 0x80) return -1;
      for (i = from; i >= 0; i--)
        if ((unsigned char)d[i] == want) return i;
      return -1;
    }
    if (from > s->ulen - 1) from = s->ulen - 1;
    for (i = from; i >= 0; i--)
      if (str_unit_at(s, i) == want) return i;
    return -1;
  }
}
int32_t ty_str_lastindexof_ch(tystr *s, int32_t c) {
  if (!s) ty_npe();
  return (int32_t)rfind_cp(s, c, s->ulen);
}
int32_t ty_str_lastindexof_ch_from(tystr *s, int32_t c, int32_t from) {
  if (!s) ty_npe();
  return (int32_t)rfind_cp(s, c, from);
}
/* strip is Java's strip: the whitespace isWhitespace accepts, and nothing else.
   It is not trim, which takes every code unit <= ' ' and is kept for
   compatibility with the same method in Java. The whitespace is Unicode's, so
   U+3000 IDEOGRAPHIC SPACE is stripped and U+00A0 NO-BREAK SPACE is not; both
   walks are over code points, because a supplementary character is one
   character and its two units must not be asked the question separately. */
static int64_t strip_left(tystr *s) {
  int64_t i = 0;
  while (i < s->ulen) {
    int32_t cp = uc_cp_at(s, i);
    if (cp != ' ' && cp != '\t' && !ty_is_whitespace(cp)) break;
    i += uc_cp_len(s, i);
  }
  return i;
}
static int64_t strip_right(tystr *s) {
  int64_t b = s->ulen;
  while (b > 0) {
    int32_t cp = uc_cp_before(s, b);
    if (cp != ' ' && cp != '\t' && !ty_is_whitespace(cp)) break;
    b -= uc_cp_len(s, b - 1);
  }
  return b;
}
int32_t ty_str_isblank(tystr *s) {
  if (!s) ty_npe();
  return strip_left(s) == s->ulen;
}
tystr *ty_str_strip(tystr *s) {
  int64_t a, b;
  if (!s) ty_npe();
  a = strip_left(s);
  b = strip_right(s);
  return str_slice(s, a, b > a ? b : a);
}
tystr *ty_str_strip_leading(tystr *s) {
  if (!s) ty_npe();
  return str_slice(s, strip_left(s), s->ulen);
}
tystr *ty_str_strip_trailing(tystr *s) {
  if (!s) ty_npe();
  return str_slice(s, 0, strip_right(s));
}
/* Java's repeat: a negative count is an IllegalArgumentException, a count of
   zero is the empty string, and the result is the receiver repeated. */
tystr *ty_str_repeat(tystr *s, int32_t n) {
  tystr *r;
  int64_t i;
  int join;
  if (!s) ty_npe();
  if (n < 0) ty_throw((tyobj *)ty_illarg("count is negative"));
  if (n == 0 || s->blen == 0) return ty_str_new(TY_STR_DATA(s), 0);
  /* A receiver ending in a high surrogate and beginning with a low one has the
     two halves meet at every seam, and what meets is one four-byte sequence, not
     two three-byte ones -- so the copies are laid down naively and str_finish
     folds the seams, which also means the block has to be sized for the longer
     form. An ASCII receiver cannot have a seam: it holds no surrogate. */
  join = n > 1 && str_ends_high(s) && str_starts_low(s);
  r = str_alloc((int64_t)n * s->blen, (int64_t)n * s->ulen, TY_STR_ASCII(s));
  for (i = 0; i < n; i++) memcpy(TY_STR_DATA(r) + i * s->blen, TY_STR_DATA(s), (size_t)s->blen);
  if (join) str_finish(r);
  return r;
}
/* replace(String, String) is a literal replacement done left to right, and the
   target may be empty: Java matches an empty pattern at every position,
   including before and after the string, which is what produces "-a-b" out of
   "ab".replace("", "-"). */
tystr *ty_str_replace_str(tystr *s, tystr *a, tystr *b) {
  int64_t i = 0, n = 0, at;
  char *p;
  tystr *r;
  if (!s || !a || !b) ty_npe();
  /* Each piece can leave a high surrogate at the end of what has been written
     and the next can begin with its low one, so the assembled bytes are
     finished rather than simply measured. */
  if (a->blen == 0) {
    n = b->blen * (s->blen + 1) + s->blen;
  } else {
    while (i + a->blen <= s->blen) {
      if (memcmp(TY_STR_DATA(s) + i, TY_STR_DATA(a), (size_t)a->blen) == 0) { n += b->blen; i += a->blen; }
      else { n++; i++; }
    }
    n += s->blen - i;
  }
  r = ty_str_new(NULL, n);
  p = TY_STR_DATA(r);
  if (a->blen == 0) {
    for (i = 0; i <= s->blen; i++) {
      memcpy(p, TY_STR_DATA(b), (size_t)b->blen); p += b->blen;
      if (i < s->blen) *p++ = TY_STR_DATA(s)[i];
    }
  } else {
    i = 0;
    while (i + a->blen <= s->blen) {
      if (memcmp(TY_STR_DATA(s) + i, TY_STR_DATA(a), (size_t)a->blen) == 0) {
        memcpy(p, TY_STR_DATA(b), (size_t)b->blen); p += b->blen;
        i += a->blen;
      } else {
        *p++ = TY_STR_DATA(s)[i++];
      }
    }
    for (at = i; at < s->blen; at++) *p++ = TY_STR_DATA(s)[at];
  }
  str_finish(r);
  return r;
}

/* -------------------------------------------------------------- char[] */

/* toCharArray hands out the code units, not the bytes: "中文".toCharArray() is
   the two chars, and an astral character is the two halves the array has room
   for. */
tyarr *ty_str_tochararray(tystr *s) {
  tyarr *a;
  int64_t i;
  if (!s) ty_npe();
  a = ty_array_new(s->ulen, 2);
  if (TY_STR_ASCII(s)) {
    for (i = 0; i < s->blen; i++) ((uint16_t *)a->data)[i] = (unsigned char)TY_STR_DATA(s)[i];
  } else {
    tyucur c;
    ucur_init(&c, s);
    for (i = 0; i < s->ulen; i++) ((uint16_t *)a->data)[i] = ucur_next(&c);
  }
  return a;
}

/* String(char[]) and String(char[], int, int): the array is the code units,
   which is what makes new String("中文".toCharArray()) "中文" and a lone
   surrogate possible. */
tystr *ty_str_of_chars(tyarr *chars) {
  if (!chars) ty_npe();
  return ty_str_of_units((const uint16_t *)chars->data, chars->len);
}
tystr *ty_str_of_chars_part(tyarr *chars, int32_t off, int32_t count) {
  if (!chars) ty_npe();
  if (off < 0 || count < 0 || off > chars->len - count) ty_sioobe(off, chars->len);
  return ty_str_of_units((const uint16_t *)chars->data + off, count);
}

/* String(int[] codePoints, int offset, int count): the array holds code
   points, so a supplementary one is a single element and becomes its two
   units. Java rejects a value that is not a code point with an
   IllegalArgumentException rather than producing one. */
tystr *ty_str_of_ints(tyarr *cp, int32_t off, int32_t count) {
  int64_t i, n = 0;
  uint16_t *tmp;
  tystr *r;
  if (!cp) ty_npe();
  if (off < 0 || count < 0 || off > cp->len - count) ty_sioobe(off, cp->len);
  tmp = count > 0 ? (uint16_t *)malloc((size_t)count * 2 * sizeof(uint16_t)) : NULL;
  for (i = 0; i < count; i++) {
    int32_t c = ((int32_t *)cp->data)[off + i];
    if (c < 0 || c > 0x10FFFF) {
      char msg[64];
      free(tmp);
      snprintf(msg, sizeof msg, "Not a valid Unicode code point: 0x%X", (unsigned)c);
      ty_throw((tyobj *)ty_illarg(msg));
    }
    if (c < 0x10000) {
      tmp[n++] = (uint16_t)c;
    } else {
      int32_t v = c - 0x10000;
      tmp[n++] = (uint16_t)(0xD800 + (v >> 10));
      tmp[n++] = (uint16_t)(0xDC00 + (v & 0x3FF));
    }
  }
  r = ty_str_of_units(tmp, n);
  free(tmp);
  return r;
}

/* String.getChars(srcBegin, srcEnd, dst, dstBegin): copy a range of code units
   into a char[], which is Java's contract -- it is the one accessor that names
   the array as the destination rather than the answer. */
void ty_str_get_chars(tystr *s, int32_t from, int32_t to, tyarr *dst, int32_t at) {
  int64_t i;
  if (!s || !dst) ty_npe();
  if (from < 0 || to > s->ulen || from > to) ty_sioobe(from, s->ulen);
  if (at < 0 || to - from > dst->len - at) ty_sioobe(at, dst->len);
  if (TY_STR_ASCII(s)) {
    for (i = from; i < to; i++)
      ((uint16_t *)dst->data)[at + i - from] = (unsigned char)TY_STR_DATA(s)[i];
    return;
  }
  {
    tyucur c;
    ucur_at(&c, s, from);
    for (i = from; i < to; i++) ((uint16_t *)dst->data)[at + i - from] = ucur_next(&c);
  }
}

/* indexOf and the code point family, in the order the plan lists them: the
   index arithmetic walks a supplementary character as the two units it is, and
   an index that lands between the two is a position Java's callers may name. */
int32_t ty_str_code_point_at(tystr *s, int32_t i) {
  uint16_t c;
  if (!s || i < 0 || i >= s->ulen) ty_throw((tyobj *)ty_sioobe(i, s ? s->ulen : 0));
  c = str_unit_at(s, i);
  if (unit_is_high(c) && i + 1 < s->ulen) {
    uint16_t d = str_unit_at(s, i + 1);
    if (unit_is_low(d)) return 0x10000 + ((c - 0xD800) << 10) + (d - 0xDC00);
  }
  return c;
}

/* codePointBefore indexes the unit before `i`, and the pair it may be the low
   half of is the one that ends there. */
int32_t ty_str_code_point_before(tystr *s, int32_t i) {
  uint16_t c;
  if (!s || i < 1 || i > s->ulen) ty_throw((tyobj *)ty_sioobe(i - 1, s ? s->ulen : 0));
  c = str_unit_at(s, i - 1);
  if (unit_is_low(c) && i >= 2) {
    uint16_t d = str_unit_at(s, i - 2);
    if (unit_is_high(d)) return 0x10000 + ((d - 0xD800) << 10) + (c - 0xDC00);
  }
  return c;
}

/* codePointCount counts the code points a range of units spells, which is the
   unit count less one for every pair wholly inside the range. A range whose
   ends cut a pair does not count that pair: the halves are counted as the units
   they are, which is what Java's arithmetic says too. */
int32_t ty_str_code_point_count(tystr *s, int32_t from, int32_t to) {
  int64_t i, n;
  if (!s) ty_npe();
  if (from < 0 || to > s->ulen || from > to) ty_throw((tyobj *)ty_sioobe(from, s->ulen));
  n = to - from;
  if (n == 0 || TY_STR_ASCII(s)) return (int32_t)n;
  for (i = from; i < to;) {
    uint16_t c = str_unit_at(s, i++);
    if (unit_is_high(c) && i < to && unit_is_low(str_unit_at(s, i))) {
      n--;
      i++;
    }
  }
  return (int32_t)n;
}

/* offsetByCodePoints walks code points from an index, forwards or backwards,
   and an index that lands between the units of a pair is a position it starts
   from or arrives at. Walking off either end is an IndexOutOfBoundsException in
   Java; this runtime has no handle for that class, so the string one --
   its subclass, and what a `catch (IndexOutOfBoundsException)` still catches --
   is what comes out. */
int32_t ty_str_offset_by_code_points(tystr *s, int32_t i, int32_t n) {
  int64_t at;
  if (!s) ty_npe();
  if (i < 0 || i > s->ulen) ty_throw((tyobj *)ty_sioobe(i, s->ulen));
  at = i;
  if (n >= 0) {
    int64_t left = n;
    while (left > 0) {
      uint16_t c;
      if (at >= s->ulen) ty_throw((tyobj *)ty_sioobe(at, s->ulen));
      c = str_unit_at(s, at++);
      if (unit_is_high(c) && at < s->ulen && unit_is_low(str_unit_at(s, at))) at++;
      left--;
    }
  } else {
    int64_t left = n;
    while (left < 0) {
      uint16_t c;
      if (at <= 0) ty_throw((tyobj *)ty_sioobe(at - 1, s->ulen));
      c = str_unit_at(s, --at);
      if (unit_is_low(c) && at > 0 && unit_is_high(str_unit_at(s, at - 1))) at--;
      left++;
    }
  }
  return (int32_t)at;
}

/* The UTF-8 bytes of the string, encoded the way the JDK's UTF-8 encoder does.
   The storage is already UTF-8 for everything a string can hold except a
   surrogate without a partner, which UTF-8 has no encoding for, so the walk is
   a copy that turns those into the one byte the JDK puts there: '?'. */
tyarr *ty_str_getbytes(tystr *s) {
  const char *d;
  int64_t i, n = 0;
  tyarr *a;
  char *out;
  if (!s) ty_npe();
  d = TY_STR_DATA(s);
  for (i = 0; i < s->blen;) {
    int len = seq_len_in(d, s->blen, i);
    if (len == 3) {
      int32_t cp = seq_cp(d + i, 3);
      n += (cp >= 0xD800 && cp <= 0xDFFF) ? 1 : 3;
    } else {
      n += len;
    }
    i += len;
  }
  a = ty_array_new(n, 1);
  out = a->data;
  for (i = 0; i < s->blen;) {
    int len = seq_len_in(d, s->blen, i);
    if (len == 3) {
      int32_t cp = seq_cp(d + i, 3);
      if (cp >= 0xD800 && cp <= 0xDFFF) {
        *out++ = '?';
        i += 3;
        continue;
      }
    }
    memcpy(out, d + i, (size_t)len);
    out += len;
    i += len;
  }
  return a;
}

/* ---- bytes to a string: java.nio.charset.UTF_8's decoder -------------- */

/* The value utf8_next answers for a byte sequence that is not a character. The
   decoder replaces it with U+FFFD; the constant is not a code point. */
#define UTF8_BAD 0x7FFFFFFF

/* One step of the decoder: the code point the sequence at `at` spells, and how
   many bytes it took. An ill-formed sequence consumes its maximal subpart --
   the longest prefix that could still have been a sequence -- so "\xE4\xB8"
   truncated is one replacement and "\xE4" before an 'A' is one replacement
   followed by the 'A'. That is the JDK's decoder, and these are its cases:
   only a wrong *continuation* is rejected at the second byte, so a lead that
   could still have become a well-formed sequence is carried to the third, and
   a surrogate -- which three bytes can spell, and UTF-8 does not allow -- is
   rejected once the code point is known rather than by its lead byte. */
static int32_t utf8_next(const unsigned char *d, int64_t n, int64_t at, int *used) {
  unsigned char b1 = d[at];
  if (b1 < 0x80) {
    *used = 1;
    return b1;
  }
  if (b1 < 0xC2) { /* a continuation with nothing before it, or an overlong lead */
    *used = 1;
    return UTF8_BAD;
  }
  if (b1 < 0xE0) {
    if (at + 1 >= n || (d[at + 1] & 0xC0) != 0x80) {
      *used = 1;
      return UTF8_BAD;
    }
    *used = 2;
    return ((b1 & 0x1F) << 6) | (d[at + 1] & 0x3F);
  }
  if (b1 < 0xF0) {
    if (at + 1 >= n) {
      *used = 1;
      return UTF8_BAD;
    }
    /* 0xE0 followed by a continuation below 0xA0 is overlong, and that much is
       known from the second byte alone; anything else that is a continuation
       could still be a character, so it is carried to the third byte. */
    if ((b1 == 0xE0 && (d[at + 1] & 0xE0) == 0x80) || (d[at + 1] & 0xC0) != 0x80) {
      *used = 1;
      return UTF8_BAD;
    }
    if (at + 2 >= n) {
      *used = 2;
      return UTF8_BAD;
    }
    if ((d[at + 2] & 0xC0) != 0x80) {
      *used = 2;
      return UTF8_BAD;
    }
    {
      int32_t cp = ((b1 & 0x0F) << 12) | ((d[at + 1] & 0x3F) << 6) | (d[at + 2] & 0x3F);
      if (cp >= 0xD800 && cp <= 0xDFFF) {
        *used = 3;
        return UTF8_BAD;
      }
      *used = 3;
      return cp;
    }
  }
  if (b1 < 0xF5) {
    int lo = b1 == 0xF0 ? 0x90 : 0x80; /* 0xF0 below 0x90 would be overlong */
    int hi = b1 == 0xF4 ? 0x8F : 0xBF; /* above 0x10FFFF is not a code point */
    if (at + 1 >= n || d[at + 1] < lo || d[at + 1] > hi) {
      *used = 1;
      return UTF8_BAD;
    }
    if (at + 2 >= n || (d[at + 2] & 0xC0) != 0x80) {
      *used = 2;
      return UTF8_BAD;
    }
    if (at + 3 >= n || (d[at + 3] & 0xC0) != 0x80) {
      *used = 3;
      return UTF8_BAD;
    }
    {
      int32_t cp = ((b1 & 0x07) << 18) | ((d[at + 1] & 0x3F) << 12) | ((d[at + 2] & 0x3F) << 6) |
                   (d[at + 3] & 0x3F);
      if (cp > 0x10FFFF) { /* 0xF5..0xF7 lead here, with four bytes of them */
        *used = 4;
        return UTF8_BAD;
      }
      *used = 4;
      return cp;
    }
  }
  *used = 1;
  return UTF8_BAD;
}

/* Bytes from outside the runtime become a string here, and everything that is
   not a well-formed UTF-8 sequence becomes U+FFFD: a socket's half-finished
   write, a file that is not text, a name in an encoding that is not this one.
   Nothing downstream then has to wonder whether a string's bytes are
   sequences, which is what makes the index arithmetic above safe. */
tystr *ty_str_of_utf8(const char *d, int64_t n) {
  const unsigned char *p = (const unsigned char *)d;
  int64_t at = 0, blen = 0, ulen = 0;
  int ascii = 1;
  tystr *s;
  char *out;
  while (at < n) {
    int used;
    int32_t cp = utf8_next(p, n, at, &used);
    at += used;
    if (cp == UTF8_BAD) {
      blen += 3;
      ulen += 1;
      ascii = 0;
    } else if (cp < 0x80) {
      blen += 1;
      ulen += 1;
    } else if (cp < 0x800) {
      blen += 2;
      ulen += 1;
      ascii = 0;
    } else if (cp < 0x10000) {
      blen += 3;
      ulen += 1;
      ascii = 0;
    } else {
      blen += 4;
      ulen += 2;
      ascii = 0;
    }
  }
  s = str_alloc(blen, ulen, ascii);
  out = TY_STR_DATA(s);
  for (at = 0; at < n;) {
    int used;
    int32_t cp = utf8_next(p, n, at, &used);
    at += used;
    out += seq_put(out, cp == UTF8_BAD ? 0xFFFD : cp);
  }
  *out = 0;
  return s;
}

/* byte[] (a range of it) to a String, for String(byte[]),
   String(byte[],int,int) and the prelude's own Net.stringFrom. The range check
   is Java's: the offset may equal the length, a count that runs past the end is
   an IndexOutOfBoundsException naming the length. */
tystr *ty_str_of_bytes(tyarr *b, int32_t off, int32_t len) {
  if (!b) ty_npe();
  if (off < 0 || len < 0 || off > b->len - len) ty_sioobe(off, b->len);
  return ty_str_of_utf8((const char *)b->data + off, len);
}

/* String(byte[]): the whole array, checked against the array's own length so
   that a null array is a NullPointerException and not a read of nothing. */
tystr *ty_str_of_bytes_all(tyarr *b) {
  if (!b) ty_npe();
  return ty_str_of_utf8((const char *)b->data, b->len);
}

/* intern keeps one object per content. Java's pool is the literal pool as well,
   and a literal is already shared by the compiler (emit_expr interns literals
   into one static), so the two agree for a literal; a string built at run time
   joins the pool the first time it is interned, which is the guarantee that
   matters -- equal content gives equal identity. */
typedef struct ty_intern {
  tystr *s;
  struct ty_intern *next;
} ty_intern;
static ty_intern **intern_tab;
static int64_t intern_cap, intern_used;
tystr *ty_str_interned(tystr *s) {
  uint32_t h;
  int64_t i;
  ty_intern *e;
  if (!s) ty_npe();
  if (!intern_tab) {
    intern_cap = 101;
    intern_tab = (ty_intern **)calloc((size_t)intern_cap, sizeof(ty_intern *));
  } else if (intern_used * 2 > intern_cap) {
    int64_t ncap = intern_cap * 2;
    ty_intern **nt = (ty_intern **)calloc((size_t)ncap, sizeof(ty_intern *));
    for (i = 0; i < intern_cap; i++) {
      ty_intern *p = intern_tab[i];
      while (p) {
        ty_intern *nx = p->next;
        uint32_t j = (uint32_t)ty_str_hash(p->s) % (uint32_t)ncap;
        p->next = nt[j];
        nt[j] = p;
        p = nx;
      }
    }
    free(intern_tab);
    intern_tab = nt;
    intern_cap = ncap;
  }
  h = (uint32_t)ty_str_hash(s) % (uint32_t)intern_cap;
  for (e = intern_tab[h]; e; e = e->next)
    if (e->s->blen == s->blen && memcmp(TY_STR_DATA(e->s), TY_STR_DATA(s), (size_t)s->blen) == 0) return e->s;
  e = (ty_intern *)malloc(sizeof(ty_intern));
  e->s = s;
  e->next = intern_tab[h];
  intern_tab[h] = e;
  intern_used++;
  return s;
}

/* -------------------------------------------- StringBuilder/StringBuffer */

/* The builder lives in one runtime object (tySB, allocated by ty_sb_new) and
   the two classes share it, so one helper set serves both. The helpers return
   the receiver: Java's builder methods return `this` so that calls chain, and
   these are what the chaining compiles to. */
/* ty_sb_new only covers `new StringBuilder()`, the form codegen routes through
   specialNew; a constructor that takes an argument is compiled as an ordinary
   allocation followed by a call to the Teyru constructor, so the object
   arrives here with ty_alloc's zeroes in its fields and no buffer at all. The
   capacity is floored at 1 rather than left at 0 (which `new StringBuilder(0)`
   asks for): both growth loops in the runtime double a capacity and a zero
   never leaves zero, so a zero-capacity builder could never be appended to.
   capacity() is the only place the difference shows. Java throws
   NegativeArraySizeException for a negative one, and so does this. */
void *ty_sb_init(void *p, int64_t cap) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (cap < 0) ty_throw((tyobj *)ty_negarr());
  if (sb->buf) return p;
  sb->cap = cap > 0 ? cap : 1;
  sb->buf = (uint16_t *)malloc((size_t)sb->cap * sizeof(uint16_t));
  return p;
}
static void lang_sb_insert_raw(tySB *sb, int64_t at, const uint16_t *u, int64_t n) {
  ty_sb_reserve(sb, n);
  memmove(sb->buf + at + n, sb->buf + at, (size_t)(sb->len - at) * sizeof(uint16_t));
  memcpy(sb->buf + at, u, (size_t)n * sizeof(uint16_t));
  sb->len += n;
}
/* The same for a String, whose bytes have to become units first. The shift
   happens before the units are written so that a string being inserted into
   the builder it came out of -- `sb.insert(0, sb.toString())` -- still reads
   its source from the receiver's own buffer only after the move. */
static void lang_sb_insert_str(tySB *sb, int64_t at, tystr *s) {
  if (!s || s->ulen == 0) return;
  ty_sb_reserve(sb, s->ulen);
  memmove(sb->buf + at + s->ulen, sb->buf + at, (size_t)(sb->len - at) * sizeof(uint16_t));
  ty_str_units(s, sb->buf + at);
  sb->len += s->ulen;
}
/* Every index check below is Java's: an offset outside 0..length is a
   StringIndexOutOfBoundsException -- a buffer's, not an array's, which is why
   these call ty_sioobe and the array helpers call ty_aioobe. */
static tySB *sb_checked(void *p, int64_t at) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (at < 0 || at > sb->len) ty_sioobe(at, sb->len);
  return sb;
}
void *ty_sb_insert_str(void *p, int32_t at, tystr *s) {
  tySB *sb = sb_checked(p, at);
  if (!s) ty_npe();
  lang_sb_insert_str(sb, at, s);
  return p;
}
void *ty_sb_insert_obj(void *p, int32_t at, void *o) {
  return ty_sb_insert_str(p, at, ty_str_of_obj(o));
}
void *ty_sb_insert_int(void *p, int32_t at, int64_t v) {
  return ty_sb_insert_str(p, at, ty_str_of_int(v));
}
void *ty_sb_insert_long(void *p, int32_t at, int64_t v) {
  return ty_sb_insert_str(p, at, ty_str_of_int(v));
}
void *ty_sb_insert_float(void *p, int32_t at, float v) {
  return ty_sb_insert_str(p, at, ty_str_of_float(v));
}
void *ty_sb_insert_double(void *p, int32_t at, double v) {
  return ty_sb_insert_str(p, at, ty_str_of_double(v));
}
void *ty_sb_insert_bool(void *p, int32_t at, int32_t v) {
  return ty_sb_insert_str(p, at, ty_str_of_bool(v));
}
void *ty_sb_insert_char(void *p, int32_t at, uint16_t c) {
  tySB *sb = sb_checked(p, at);
  /* one unit, which may be half of an astral character: the builder holds
     units, so a pair appended or inserted one half at a time is the character */
  lang_sb_insert_raw(sb, at, &c, 1);
  return p;
}
void *ty_sb_insert_chars(void *p, int32_t at, tyarr *chars) {
  tySB *sb = sb_checked(p, at);
  if (!chars) ty_npe();
  lang_sb_insert_raw(sb, at, (const uint16_t *)chars->data, chars->len);
  return p;
}
void *ty_sb_append_chars(void *p, tyarr *chars) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (!chars) ty_npe();
  ty_sb_reserve(sb, chars->len);
  memcpy(sb->buf + sb->len, chars->data, (size_t)chars->len * sizeof(uint16_t));
  sb->len += chars->len;
  return p;
}
void *ty_sb_append_float(void *p, float v) { return ty_sb_append_str(p, ty_str_of_float(v)); }
int32_t ty_sb_capacity(void *p) { return (int32_t)((tySB *)p)->cap; }
int32_t ty_sb_isempty(void *p) { return ((tySB *)p)->len == 0; }
void *ty_sb_ensure(void *p, int32_t cap) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (cap > sb->cap) {
    ty_sb_reserve(sb, cap - sb->len);
  }
  return p;
}
int32_t ty_sb_charat(void *p, int32_t at) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (at < 0 || at >= sb->len) ty_sioobe(at, sb->len);
  return sb->buf[at];
}
void *ty_sb_set_charat(void *p, int32_t at, uint16_t c) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (at < 0 || at >= sb->len) ty_sioobe(at, sb->len);
  sb->buf[at] = c;
  return p;
}
/* delete is half open: [from, to), and to == length is allowed. deleteCharAt
   is delete(at, at+1), which is the one place the range is not clamped: an
   index outside the text is refused. `to` past the end is *clamped* rather
   than refused -- Java's delete answers sb.delete(2, 100) by deleting to the
   end, and only a start outside 0..length or past the end throws. An empty
   range is a no-op. */
void *ty_sb_delete(void *p, int32_t from, int32_t to) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (to > sb->len) to = sb->len;
  if (from < 0 || from > sb->len || from > to) ty_sioobe(from, sb->len);
  memmove(sb->buf + from, sb->buf + to, (size_t)(sb->len - to) * sizeof(uint16_t));
  sb->len -= to - from;
  return p;
}
void *ty_sb_delete_charat(void *p, int32_t at) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (at < 0 || at >= sb->len) ty_sioobe(at, sb->len);
  memmove(sb->buf + at, sb->buf + at + 1, (size_t)(sb->len - at - 1) * sizeof(uint16_t));
  sb->len--;
  return p;
}
/* replace has the same clamping, and it is the one builder method that refuses
   a null String with NullPointerException rather than inserting "null": Java
   reads str.length() after the bounds check, so an out-of-range range is the
   StringIndexOutOfBoundsException and a null replacement inside one is the
   NullPointerException. */
void *ty_sb_replace(void *p, int32_t from, int32_t to, tystr *s) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (to > sb->len) to = sb->len;
  if (from < 0 || from > sb->len || from > to) ty_sioobe(from, sb->len);
  if (!s) ty_npe();
  ty_sb_delete(p, from, to);
  lang_sb_insert_str(sb, from, s);
  return p;
}
/* Java's reverse: the units are reversed, and then every half of an astral
   character that the reversal put in the wrong order is swapped back with its
   partner, so "ab中" gives "中ba" and an emoji survives the trip. */
void *ty_sb_reverse(void *p) {
  tySB *sb = (tySB *)p;
  int64_t i;
  if (!sb) ty_npe();
  if (sb->len >= 2) {
    int64_t n = sb->len - 1;
    for (i = (n - 1) >> 1; i >= 0; i--) {
      uint16_t t = sb->buf[i];
      sb->buf[i] = sb->buf[n - i];
      sb->buf[n - i] = t;
    }
  }
  for (i = 0; i + 1 < sb->len; i++) {
    if (unit_is_low(sb->buf[i]) && unit_is_high(sb->buf[i + 1])) {
      uint16_t t = sb->buf[i];
      sb->buf[i] = sb->buf[i + 1];
      sb->buf[i + 1] = t;
      i++;
    }
  }
  return p;
}
/* setLength truncates or extends. Java fills the extension with '\0' rather
   than leaving whatever the buffer held, which is why the memset is there. */
void *ty_sb_set_length(void *p, int32_t n) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (n < 0) ty_sioobe(n, sb->len);
  if (n > sb->len) {
    ty_sb_reserve(sb, n - sb->len);
    memset(sb->buf + sb->len, 0, (size_t)(n - sb->len) * sizeof(uint16_t));
  }
  sb->len = n;
  return p;
}
/* A builder's buffer is units and a needle is a String, so the needle is
   decoded once into units and the scan is then a memcmp over them. */
static uint16_t *sb_needle(tystr *s) {
  uint16_t *u = (uint16_t *)malloc((size_t)(s->ulen > 0 ? s->ulen : 1) * sizeof(uint16_t));
  ty_str_units(s, u);
  return u;
}
static int32_t sb_find(tySB *sb, tystr *s, int64_t from) {
  int64_t i, n = s->ulen;
  uint16_t *needle;
  if (n == 0) return from <= sb->len ? (int32_t)from : -1;
  if (from < 0) from = 0;
  if (from > sb->len - n) return -1;
  needle = sb_needle(s);
  for (i = from; i + n <= sb->len; i++) {
    if (memcmp(sb->buf + i, needle, (size_t)n * sizeof(uint16_t)) == 0) {
      free(needle);
      return (int32_t)i;
    }
  }
  free(needle);
  return -1;
}
int32_t ty_sb_indexof(void *p, tystr *s) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (!s) ty_npe();
  return sb_find(sb, s, 0);
}
int32_t ty_sb_indexof_from(void *p, tystr *s, int32_t from) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (!s) ty_npe();
  if (from < 0) from = 0;
  return sb_find(sb, s, from);
}
int32_t ty_sb_lastindexof(void *p, tystr *s) {
  tySB *sb = (tySB *)p;
  int64_t i, n;
  uint16_t *needle;
  if (!sb) ty_npe();
  if (!s) ty_npe();
  n = s->ulen;
  if (n == 0) return (int32_t)sb->len;
  if (n > sb->len) return -1;
  needle = sb_needle(s);
  for (i = sb->len - n; i >= 0; i--) {
    if (memcmp(sb->buf + i, needle, (size_t)n * sizeof(uint16_t)) == 0) {
      free(needle);
      return (int32_t)i;
    }
  }
  free(needle);
  return -1;
}
tystr *ty_sb_substring(void *p, int32_t from) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (from < 0 || from > sb->len) ty_sioobe(from, sb->len);
  return ty_str_of_units(sb->buf + from, sb->len - from);
}
tystr *ty_sb_substring_to(void *p, int32_t from, int32_t to) {
  tySB *sb = (tySB *)p;
  if (!sb) ty_npe();
  if (from < 0 || to > sb->len || from > to) ty_sioobe(from, sb->len);
  return ty_str_of_units(sb->buf + from, to - from);
}

/* --------------------------------------------------------- String.format */

/* A growable byte buffer, malloc'd rather than allocated from the collector:
   the formatter holds at most one collector-visible string at a time -- the
   result, built at the very end -- so there is nothing for a collection to keep
   alive in the middle of a conversion. */
typedef struct {
  char *buf;
  int64_t len, cap;
} fmtbuf;

static void fmtb_init(fmtbuf *b) {
  b->cap = 64;
  b->len = 0;
  b->buf = (char *)malloc((size_t)b->cap);
}

/* The scratch buffers one String.format is holding while it runs.

   Java reports a bad format by throwing, and a throw here is a longjmp to
   whatever handler surrounds the call: every buffer between the loop and that
   handler becomes unreachable at once, so freeing them at the normal exits is
   not enough -- a program that catches a formatting error in a loop would leak
   the format under test every time. These buffers are malloc'd on purpose (the
   collector is no place for a scratch buffer a nested allocation could
   collect), so the throw helpers free them on the way out instead.

   The runtime has no threads and the format helpers below are one call deep,
   so a plain list is enough; it is emptied whenever a format ends, normally or
   not. */
#define FMT_LIVE_MAX 8
static fmtbuf *fmt_live[FMT_LIVE_MAX];
static int fmt_live_n;

static void fmt_hold(fmtbuf *b) {
  if (fmt_live_n < FMT_LIVE_MAX) fmt_live[fmt_live_n++] = b;
}
static void fmt_drop(fmtbuf *b) {
  int i;
  for (i = 0; i < fmt_live_n; i++) {
    if (fmt_live[i] == b) {
      fmt_live[i] = fmt_live[--fmt_live_n];
      return;
    }
  }
}
/* The buffers below the current format belong to a format that is still
   running -- `%s` on an object whose toString calls String.format again puts
   two on the list at once -- so a throw frees down to the innermost one and no
   further. */
static int fmt_base;

static void fmt_abandon(void) {
  while (fmt_live_n > fmt_base) {
    free(fmt_live[--fmt_live_n]->buf);
  }
}
static void fmtb_need(fmtbuf *b, int64_t extra) {
  if (b->len + extra <= b->cap) return;
  while (b->len + extra > b->cap) b->cap *= 2;
  b->buf = (char *)realloc(b->buf, (size_t)b->cap);
}
static void fmtb_add(fmtbuf *b, const char *d, int64_t n) {
  if (n <= 0) return;
  fmtb_need(b, n);
  memcpy(b->buf + b->len, d, (size_t)n);
  b->len += n;
}
static void fmtb_ch(fmtbuf *b, char c) { fmtb_add(b, &c, 1); }
static void fmtb_rep(fmtbuf *b, char c, int64_t n) {
  if (n <= 0) return;
  fmtb_need(b, n);
  memset(b->buf + b->len, c, (size_t)n);
  b->len += n;
}

/* The significant decimal digits of a finite double: the shortest decimal that
   reads back as the same value. These are the digits Java's conversions round.
   `%.2f` of 1.005 is "1.01" because the shortest decimal is "1.005" and it is
   that string, not the binary value 1.00499999999999989, which gets rounded.
   The search is the one Double.toString makes (see fmt_generic): round to p
   digits and keep the first p that reads back. The digits are left-aligned to
   a decimal exponent E -- the value is 0.<digits> * 10^(E+1) -- so dig[0] is
   the digit in the 10^E place. */
static int64_t shortest_digits(double v, char *dig) {
  char tmp[96];
  char *q, *e;
  int prec = 17, n = 0, p;
  for (p = 15; p <= 17; p++) {
    snprintf(tmp, sizeof tmp, "%.*Le", p - 1, (long double)v);
    if ((long double)strtod(tmp, NULL) == (long double)v) { prec = p; break; }
  }
  snprintf(tmp, sizeof tmp, "%.*Le", prec - 1, (long double)v);
  for (q = tmp; *q && *q != 'e' && *q != 'E'; q++)
    if (TY_ASCII_DIGIT(*q)) dig[n++] = *q;
  while (n > 1 && dig[n - 1] == '0') n--;
  dig[n] = 0;
  e = strpbrk(tmp, "eE");
  return e ? (int64_t)atoi(e + 1) : 0;
}

/* Round `keep` digits half up, carrying into the digits before them. A carry
   out of the leading digit -- 9.99 with one digit -- leaves "1" followed by
   zeros and tells the caller the decimal exponent grew by one. */
static void round_sig(char *d, int keep, int *carried) {
  int j = keep - 1;
  *carried = 0;
  while (j >= 0 && d[j] == '9') d[j--] = '0';
  if (j < 0) {
    d[0] = '1';
    *carried = 1;
  } else {
    d[j]++;
  }
}

/* The unsigned body of a %f conversion: the integer digits, a point, and prec
   fraction digits, rounded half up. Digits past the shortest representation
   are zeros -- Java prints `%.20f` of 0.1 as 0.1 followed by nineteen zeros,
   not as the value's exact binary expansion. */
static void fmt_fixed_body(fmtbuf *b, double v, int32_t prec, int alt) {
  char dig[32], *d;
  int64_t E, base, total, i, p;
  int nd;
  E = shortest_digits(v, dig);
  nd = (int)strlen(dig);
  if (prec < 0) prec = 6;
  base = E >= 0 ? E + 1 : 1;
  total = base + prec;
  d = (char *)malloc((size_t)total + 2);
  for (i = 0; i < total; i++) {
    if (i < base) {
      d[i] = (E >= 0 && i < nd) ? dig[i] : '0';
    } else {
      int64_t j = E + (i - base) + 1; /* the digit in that fraction place */
      d[i] = (j >= 0 && j < nd) ? dig[j] : '0';
    }
  }
  if (E < 0) d[0] = '0';
  p = E + prec + 1;
  if (p >= 0 && p < nd && dig[p] >= '5') {
    int carried;
    round_sig(d, (int)total, &carried);
    if (carried) {
      /* every digit rolled over: the integer part gained a place, and all of
         the old digits are zeros now */
      fmtb_ch(b, '1');
      fmtb_rep(b, '0', base);
      if (prec > 0 || alt) {
        fmtb_ch(b, '.');
        fmtb_rep(b, '0', prec);
      }
      free(d);
      return;
    }
  }
  fmtb_add(b, d, base);
  if (prec > 0 || alt) {
    fmtb_ch(b, '.');
    fmtb_add(b, d + base, prec);
  }
  free(d);
}

/* The unsigned body of an %e conversion: one digit, a point, prec digits, and
   an exponent with a sign and at least two digits, the way Java prints it. The
   rounding carry can lift the exponent, which is how 9.99 with one digit comes
   out as 1.0e+01. */
static void fmt_sci_body(fmtbuf *b, double v, int32_t prec, int upper, int alt) {
  char dig[32], out[40], t[24];
  int64_t E;
  int nd, i, n, carried = 0;
  E = shortest_digits(v, dig);
  nd = (int)strlen(dig);
  if (prec < 0) prec = 6;
  for (i = 0; i < prec + 1; i++) out[i] = i < nd ? dig[i] : '0';
  if (nd > prec + 1 && dig[prec + 1] >= '5') {
    round_sig(out, prec + 1, &carried);
    if (carried) E++;
  }
  fmtb_ch(b, out[0]);
  if (prec > 0 || alt) {
    fmtb_ch(b, '.');
    fmtb_add(b, out + 1, prec);
  }
  fmtb_ch(b, upper ? 'E' : 'e');
  {
    int64_t a = E < 0 ? -E : E;
    n = 0;
    do {
      t[n++] = (char)('0' + (int)(a % 10));
      a /= 10;
    } while (a);
    while (n < 2) t[n++] = '0';
    fmtb_ch(b, E < 0 ? '-' : '+');
    while (n > 0) fmtb_ch(b, t[--n]);
  }
}

/* Java's %g: the precision counts significant digits, the exponent of the value
   *after* the rounding picks between the fixed and the scientific form -- which
   is why 999999.9 comes out scientific while 100000 does not -- and the digits
   are never stripped, so %g of 1.0 is "1.00000". */
static void fmt_gen_body(fmtbuf *b, double v, int32_t prec, int upper) {
  char dig[32], out[40], t[24];
  int64_t E;
  int nd, i, n, P, carried = 0;
  E = shortest_digits(v, dig);
  nd = (int)strlen(dig);
  P = prec < 0 ? 6 : prec;
  if (P < 1) P = 1;
  for (i = 0; i < P; i++) out[i] = i < nd ? dig[i] : '0';
  if (nd > P && dig[P] >= '5') {
    round_sig(out, P, &carried);
    if (carried) E++;
  }
  if (E < -4 || E >= P) {
    fmtb_ch(b, out[0]);
    if (P > 1) {
      fmtb_ch(b, '.');
      fmtb_add(b, out + 1, P - 1);
    }
    fmtb_ch(b, upper ? 'E' : 'e');
    {
      int64_t a = E < 0 ? -E : E;
      n = 0;
      do {
        t[n++] = (char)('0' + (int)(a % 10));
        a /= 10;
      } while (a);
      while (n < 2) t[n++] = '0';
      fmtb_ch(b, E < 0 ? '-' : '+');
      while (n > 0) fmtb_ch(b, t[--n]);
    }
  } else if (E >= 0) {
    fmtb_add(b, out, E + 1);
    if (P - 1 - E > 0) {
      fmtb_ch(b, '.');
      fmtb_add(b, out + E + 1, P - 1 - E);
    }
  } else {
    fmtb_add(b, "0.", 2);
    fmtb_rep(b, '0', -E - 1);
    fmtb_add(b, out, P);
  }
}

/* %a and %A: the binary value in hexadecimal, one digit before the point and as
   many after it as the precision asks for. Without a precision the fraction is
   the 52 mantissa bits as thirteen digits with the trailing zeros removed --
   but never all of them -- which is what makes %a of 1.0 "0x1.0p0" and %a of
   Double.MIN_VALUE "0x0.0000000000001p-1022". */
static void fmt_hex_body(fmtbuf *b, double v, int32_t prec, int upper) {
  int64_t bits = bits_of_double(v);
  uint64_t mant = (uint64_t)bits & 0xFFFFFFFFFFFFFull;
  int exp = (int)((bits >> 52) & 0x7FF);
  int lead, i, keep;
  int64_t e2;
  char hex[24], t[24];
  int n;
  if (exp == 0) {
    /* a subnormal, or a zero: Java writes %a of 0.0 as "0x0.0p0" and only a
       subnormal carries the smallest exponent */
    lead = 0;
    e2 = mant == 0 ? 0 : -1022;
  } else {
    lead = 1;
    mant |= (1ull << 52);
    e2 = (int64_t)exp - 1023;
  }
  for (i = 0; i < 13; i++)
    hex[i] = "0123456789abcdef"[(mant >> (48 - 4 * i)) & 0xF];
  if (prec < 0) {
    keep = 13;
    while (keep > 1 && hex[keep - 1] == '0') keep--;
  } else {
    keep = prec < 1 ? 1 : prec; /* Java prints at least one fraction digit */
    if (keep < 13) {
      /* round half up on the hexadecimal digits: everything from `keep` on is
         weighed against half of the digit in that place */
      int up = hex[keep] > '8';
      if (hex[keep] == '8')
        for (i = keep + 1; i < 13; i++)
          if (hex[i] != '0') up = 1;
      if (up) {
        int j = keep - 1;
        while (j >= 0 && hex[j] == 'f') hex[j--] = '0';
        if (j < 0) lead++;
        else hex[j]++;
      }
    }
    for (i = 13; i < keep; i++) hex[i] = '0';
  }
  if (lead > 1) {
    /* 0x1.ff rounded to one digit is 0x2.0, which Java writes as 0x1.0p1 */
    lead = 1;
    e2++;
  }
  fmtb_add(b, upper ? "0X" : "0x", 2);
  fmtb_ch(b, (char)('0' + lead));
  fmtb_ch(b, '.');
  for (i = 0; i < keep; i++) {
    char c = hex[i];
    if (upper && c >= 'a') c = (char)(c - 32);
    fmtb_ch(b, c);
  }
  fmtb_ch(b, upper ? 'P' : 'p');
  {
    int64_t a = e2 < 0 ? -e2 : e2;
    n = 0;
    do {
      t[n++] = (char)('0' + (int)(a % 10));
      a /= 10;
    } while (a);
    if (e2 < 0) fmtb_ch(b, '-');
    while (n > 0) fmtb_ch(b, t[--n]);
  }
}

/* ------------------------------------------------------------- the flags */

typedef struct {
  int minus, plus, space, zero, comma, alt, paren;
  int32_t width; /* -1 when the format gave none */
} fmtflags;

/* Java's padding rule, in one place: the sign or the radix prefix goes first,
   then -- if the zero flag is on and the conversion is not left justified -- the
   zeros that fill the width, then the digits. Grouping, when it is on, is
   already inside the body.

   The width counts characters and the body is bytes, so the caller says how
   many code units its body is: for every conversion but %s the two are the same
   number and the argument is `blen`, and for %s it is the string's ulen. That
   is what makes String.format("[%5s|%-4s]", "中", "ab中") pad to five and four
   rather than to the six and five bytes the two strings occupy. */
static void fmt_put(fmtbuf *b, const fmtflags *f, const char *prefix, int64_t plen,
                    const char *body, int64_t blen, int64_t bunits) {
  /* The parentheses flag replaces the minus of a negative number, and the
     closing parenthesis is part of the field the width counts -- so `%(10d` of
     -42 is "      (42)", six spaces and four characters. */
  int paren = f->paren && plen == 1 && prefix[0] == '-';
  int64_t total = plen + bunits + (paren ? 1 : 0);
  int64_t pad = f->width > 0 && f->width > total ? f->width - total : 0;
  if (paren) prefix = "(";
  if (f->minus) {
    fmtb_add(b, prefix, plen);
    fmtb_add(b, body, blen);
    if (paren) fmtb_ch(b, ')');
    fmtb_rep(b, ' ', pad);
  } else if (f->zero) {
    fmtb_add(b, prefix, plen);
    fmtb_rep(b, '0', pad);
    fmtb_add(b, body, blen);
    if (paren) fmtb_ch(b, ')');
  } else {
    fmtb_rep(b, ' ', pad);
    fmtb_add(b, prefix, plen);
    fmtb_add(b, body, blen);
    if (paren) fmtb_ch(b, ')');
  }
}

/* Grouping is three digits to a comma, counted from the right. */
static int group_digits(const char *d, int n, char *out) {
  int i, o = 0;
  for (i = 0; i < n; i++) {
    if (i > 0 && (n - i) % 3 == 0) out[o++] = ',';
    out[o++] = d[i];
  }
  return o;
}

/* A null argument prints as the four letters of its own name -- uppercased by
   the conversions that uppercase, cut short by the precision and padded by the
   width, but never zero filled, so %08d of a null is "    null". That is
   Java's rule for every conversion that takes an argument except %b, whose
   null is a "false" instead. */
static void fmt_null(fmtbuf *out, const fmtflags *f, int upper, int32_t P) {
  char body[4] = {'n', 'u', 'l', 'l'};
  int64_t blen = 4;
  fmtflags nf = *f;
  int k;
  if (P >= 0 && blen > P) blen = P;
  if (upper)
    for (k = 0; k < blen; k++) body[k] = (char)ty_char_upper((unsigned char)body[k]);
  nf.zero = 0;
  fmt_put(out, &nf, "", 0, body, blen, blen);
}

/* A number formatted into `t` is moved into the body, with its integer part
   grouped when the comma flag asked for it. A scientific body has no integer
   part to group and passes through untouched. */
static void move_number(fmtbuf *body, fmtbuf *t, const fmtflags *f) {
  int64_t dot = 0;
  char *g;
  int gn;
  if (!f->comma || memchr(t->buf, 'e', (size_t)t->len) != NULL) {
    fmtb_add(body, t->buf, t->len);
    return;
  }
  while (dot < t->len && t->buf[dot] != '.') dot++;
  g = (char *)malloc((size_t)dot * 2 + 2);
  gn = group_digits(t->buf, (int)dot, g);
  fmtb_add(body, g, gn);
  free(g);
  fmtb_add(body, t->buf + dot, t->len - dot);
}

/* ------------------------------------------------------------- arguments */

static const char *arg_class(void *o) {
  if (!o) return "null";
  return ((tyobj *)o)->cls->name;
}
static int arg_is_javalang(void *o) {
  int i;
  if (!o) return 0;
  if (((tyobj *)o)->cls == TY_STRING) return 1;
  for (i = 1; i <= 8; i++)
    if (((tyobj *)o)->cls == TY_BOX[i]) return 1;
  return 0;
}

/* Java refuses an argument a conversion has no meaning for, and names the class
   it got instead: "d != java.lang.Character". The exception is
   IllegalFormatConversionException, which the runtime cannot raise, so its
   message travels on IllegalArgumentException. */
static void bad_arg(char conv, void *o) {
  char msg[160];
  const char *name = arg_class(o);
  if (arg_is_javalang(o)) {
    /* Java names its own classes by their binary name; here the class carries
       this runtime's package, so the simple name is what is left of it */
    const char *dot = strrchr(name, '.');
    if (dot) name = dot + 1;
    snprintf(msg, sizeof msg, "%c != java.lang.%s", conv, name);
  } else {
    snprintf(msg, sizeof msg, "%c != %s", conv, name);
  }
  fmt_abandon();
  ty_throw(ty_illarg(msg));
}
/* Java's flag check: a combination the conversion gives no meaning to is an
   error rather than something to ignore, so `%,x` and `%+s` stop the program
   the way they do in Java. The flags are listed in the order Java's own
   toString prints them. */
static int flag_str(char *out, const fmtflags *f) {
  int n = 0;
  if (f->minus) out[n++] = '-';
  if (f->alt) out[n++] = '#';
  if (f->plus) out[n++] = '+';
  if (f->space) out[n++] = ' ';
  if (f->zero) out[n++] = '0';
  if (f->comma) out[n++] = ',';
  if (f->paren) out[n++] = '(';
  out[n] = 0;
  return n;
}
/* A flag the conversion gives no meaning to: "Conversion = x, Flags = ,". */
static void bad_flags(char conv, const fmtflags *f) {
  char msg[80], fl[8];
  flag_str(fl, f);
  snprintf(msg, sizeof msg, "Conversion = %c, Flags = %s", conv, fl);
  fmt_abandon();
  ty_throw(ty_illarg(msg));
}
/* A flag combination that contradicts itself, and a flag on a conversion that
   takes none: Java names the flags alone, in quotes. */
static void bad_flag_set(const fmtflags *f) {
  char msg[80], fl[8];
  flag_str(fl, f);
  snprintf(msg, sizeof msg, "Flags = '%s'", fl);
  fmt_abandon();
  ty_throw(ty_illarg(msg));
}
/* A '-' with no width has nothing to justify against, and Java quotes the
   specifier back, exactly as it was written. */
static void bad_width(tystr *fmt, int64_t spec0, int64_t speclen) {
  char msg[80];
  int64_t n = speclen;
  if (n > 76) n = 76;
  memcpy(msg, TY_STR_DATA(fmt) + spec0, (size_t)n);
  msg[n] = 0;
  fmt_abandon();
  ty_throw(ty_illarg(msg));
}
static void bad_int(const char *prefix, int64_t v) {
  char msg[32];
  snprintf(msg, sizeof msg, "%s%d", prefix, (int)v);
  fmt_abandon();
  ty_throw(ty_illarg(msg));
}
static void check_flags(char conv, const fmtflags *f, tystr *fmt, int64_t spec0, int64_t speclen) {
  int numeric = strchr("doxXeEfgGaA", conv) != NULL;
  int signed_ok = strchr("deEfgGaA", conv) != NULL;
  int alt_ok = strchr("oxXeEfgGaA", conv) != NULL;
  int group_ok = strchr("dfgG", conv) != NULL;
  int paren_ok = strchr("deEfgG", conv) != NULL;
  /* '-' and '0' contradict each other, and Java refuses the pair by name */
  if (f->minus && f->zero) bad_flag_set(f);
  if ((f->zero && !numeric) || (f->plus && !signed_ok) || (f->space && !signed_ok) ||
      (f->alt && !alt_ok) || (f->comma && !group_ok) || (f->paren && !paren_ok)) {
    bad_flags(conv, f);
  }
  if (f->minus && f->width < 0) bad_width(fmt, spec0, speclen);
}

/* Which box an argument is. The numbering is the runtime's: 1 boolean, 2 byte,
   3 short, 4 char, 5 int, 6 long, 7 float, 8 double. */
static int box_kind(void *o) {
  int i;
  if (!o) return 0;
  for (i = 1; i <= 8; i++)
    if (((tyobj *)o)->cls == TY_BOX[i]) return i;
  return -1;
}
/* The arguments %d, %o and %x accept: Java widens a Byte, a Short or an Integer
   and refuses everything else, so a Character is not an integer here. */
static int64_t int_arg(char conv, void *o) {
  switch (box_kind(o)) {
    case 2: return (int64_t)(int8_t)((tybytebox *)o)->v;
    case 3: return (int64_t)((tyshortbox *)o)->v;
    case 5: return (int64_t)((tyintbox *)o)->v;
    case 6: return ((tylongbox *)o)->v;
    default: bad_arg(conv, o);
  }
  return 0;
}
/* The arguments %c accepts: a Character, or the three integer wrappers that fit
   in a char. A Long is refused. */
static int64_t char_arg(char conv, void *o) {
  switch (box_kind(o)) {
    case 2: return (int64_t)(int8_t)((tybytebox *)o)->v;
    case 3: return (int64_t)((tyshortbox *)o)->v;
    case 4: return (int64_t)((tycharbox *)o)->v;
    case 5: return (int64_t)((tyintbox *)o)->v;
    default: bad_arg(conv, o);
  }
  return 0;
}
/* The arguments %e, %f, %g and %a accept: Float and Double only. An Integer is
   a conversion error in Java, not a numeric widening. */
static double float_arg(char conv, void *o) {
  switch (box_kind(o)) {
    case 7: return (double)((tyfloatbox *)o)->v;
    case 8: return ((tydoublebox *)o)->v;
    default: bad_arg(conv, o);
  }
  return 0.0;
}
/* The sign Java prints: the sign bit, except that a NaN is never signed and
   never takes the + or the space flag either -- Java compares the value with
   zero, and every comparison with a NaN is false. */
static int neg_sign(double v) { return v == v && signbit(v) != 0; }

static int radix_digits(uint64_t m, int radix, int upper, char *out) {
  char t[70];
  int n = 0, i;
  do {
    int d = (int)(m % (uint64_t)radix);
    t[n++] = (char)(d < 10 ? '0' + d : (upper ? 'A' : 'a') + d - 10);
    m /= (uint64_t)radix;
  } while (m);
  for (i = 0; i < n; i++) out[i] = t[n - 1 - i];
  return n;
}

/* The next argument, or Java's complaint about the specifier that wanted one:
   "Format specifier '%d'", quoted from the format exactly as it was written. */
static void *next_arg(tyarr *args, int64_t *ai, int64_t fixed, tystr *fmt, int64_t spec0, int64_t speclen) {
  char msg[80];
  int64_t n = speclen;
  int64_t have = args ? args->len : 0;
  /* An explicit "N$" names its argument and leaves the running count alone,
     which is why `%s %1$s` prints the first argument twice. */
  if (fixed >= 0) {
    if (fixed >= have) goto missing;
    return ((void **)args->data)[fixed];
  }
  if (*ai >= have) goto missing;
  return ((void **)args->data)[(*ai)++];
missing:
  if (n > 48) n = 48;
  memcpy(msg, "Format specifier '", 18);
  memcpy(msg + 18, TY_STR_DATA(fmt) + spec0, (size_t)n);
  msg[18 + n] = '\'';
  msg[19 + n] = 0;
  fmt_abandon();
  ty_throw(ty_illarg(msg));
  return NULL;
}

/* pick_arg answers with the argument one conversion uses. Java's `%<` reuses
   the argument the previous conversion used and leaves the running count where
   it was, so "%s %<s" prints the same argument twice; anything else takes the
   next one, or the one an explicit "N$" named. A null reference is a value, so
   "there was no previous" is its own flag rather than a null argument. */
static void *pick_arg(tyarr *args, int64_t *ai, int64_t fixed, void **last, int *have_last,
                      int relative, tystr *fmt, int64_t spec0, int64_t speclen) {
  if (relative) {
    if (!*have_last) {
      char msg[80];
      int64_t n = speclen;
      if (n > 48) n = 48;
      memcpy(msg, "Format specifier '", 18);
      memcpy(msg + 18, TY_STR_DATA(fmt) + spec0, (size_t)n);
      msg[18 + n] = '\'';
      msg[19 + n] = 0;
      fmt_abandon();
  ty_throw(ty_illarg(msg));
    }
    return *last;
  }
  void *o = next_arg(args, ai, fixed, fmt, spec0, speclen);
  *last = o;
  *have_last = 1;
  return o;
}

/* ------------------------------------------------------- format assembly */

/* Only the conversions Java defines for String.format are here. The ones this
   runtime cannot answer are answered loudly: %t and %T need a clock formatting
   layer there is no room for, and they stop the program through
   ty_unimplemented rather than printing something plausible. An unknown
   conversion is a program error in Java and is reported as one. */
tystr *ty_str_format(tystr *fmt, tyarr *args) {
  fmtbuf out;
  int64_t i = 0, ai = 0, fixed = -1;
  void *last = NULL;
  int have_last = 0, relative = 0;
  if (!fmt) ty_npe();
  int outer_base = fmt_base;
  fmt_base = fmt_live_n;
  fmtb_init(&out);
  fmt_hold(&out);
  while (i < fmt->blen) {
    fmtflags f;
    char conv, c = TY_STR_DATA(fmt)[i];
    int64_t spec0;
    int32_t P;
    if (c != '%') {
      fmtb_ch(&out, c);
      i++;
      continue;
    }
    spec0 = i;
    i++;
    /* Java's optional "N$" chooses the argument by position rather than in
       order. It stands before the flags, so it is read first -- but only when
       a '$' follows, because the same digits are otherwise a width. */
    fixed = -1;
    relative = 0;
    {
      int64_t save = i, idx = 0;
      while (i < fmt->blen && TY_ASCII_DIGIT(TY_STR_DATA(fmt)[i])) {
        idx = idx * 10 + (TY_STR_DATA(fmt)[i] - '0');
        i++;
      }
      if (i > save && i < fmt->blen && TY_STR_DATA(fmt)[i] == '$') {
        fixed = idx - 1;
        i++;
      } else if (i == save && i < fmt->blen && TY_STR_DATA(fmt)[i] == '<') {
        /* `%<` names the previous argument, so it is read where the "N$"
           would have been */
        relative = 1;
        i++;
      } else {
        i = save;
      }
    }
    f.minus = f.plus = f.space = f.zero = f.comma = f.alt = f.paren = 0;
    f.width = -1;
    for (;;) {
      char fc;
      int *slot;
      const char *at;
      if (i >= fmt->blen) ty_unimplemented("String.format: conversion is cut short");
      fc = TY_STR_DATA(fmt)[i];
      at = strchr("-#+ 0,(", fc);
      if (!at) break;
      switch ((int)(at - "-#+ 0,(")) {
        case 0: slot = &f.minus; break;
        case 1: slot = &f.alt; break;
        case 2: slot = &f.plus; break;
        case 3: slot = &f.space; break;
        case 4: slot = &f.zero; break;
        case 5: slot = &f.comma; break;
        default: slot = &f.paren; break;
      }
      /* Java refuses a flag written twice rather than letting the second one
         stand for the first: "%  s" is a DuplicateFormatFlags, "%-s" is not. */
      if (*slot) {
        char msg[48];
        snprintf(msg, sizeof msg, "Flags = '%c'", fc);
        ty_throw(ty_illarg(msg));
      }
      *slot = 1;
      i++;
    }
    {
      int32_t w = 0;
      int has = 0;
      while (i < fmt->blen && TY_ASCII_DIGIT(TY_STR_DATA(fmt)[i])) {
        has = 1;
        w = w * 10 + (TY_STR_DATA(fmt)[i] - '0');
        i++;
      }
      if (has) f.width = w;
    }
    P = -1;
    if (i < fmt->blen && TY_STR_DATA(fmt)[i] == '.') {
      int32_t pr = 0;
      i++;
      while (i < fmt->blen && TY_ASCII_DIGIT(TY_STR_DATA(fmt)[i])) {
        pr = pr * 10 + (TY_STR_DATA(fmt)[i] - '0');
        i++;
      }
      P = pr;
    }
    if (i >= fmt->blen) ty_unimplemented("String.format: conversion is cut short");
    conv = TY_STR_DATA(fmt)[i];
    i++;
    /* Java gives an integer or a character conversion no precision to work
       with, and says so with the offending number as the message */
    if (P >= 0 && strchr("doxXcC", conv) != NULL) bad_int("", P);
    /* %% and %n are the two conversions that are not an argument. Java gives
       %n no flag, no width and no precision at all, and % only a width. */
    if (conv == 'n') {
      if (f.minus || f.alt || f.plus || f.space || f.zero || f.comma || f.paren) bad_flag_set(&f);
      if (f.width >= 0) bad_int("", f.width);
      if (P >= 0) bad_int("", P);
      /* Java's line separator is the platform's; this runtime only runs on
         the ones whose is a newline */
      fmtb_ch(&out, '\n');
      continue;
    }
    if (conv == '%') {
      if (f.minus && f.width < 0) bad_width(fmt, spec0, i - spec0);
      if (f.alt || f.plus || f.space || f.zero || f.comma || f.paren) bad_flag_set(&f);
      fmt_put(&out, &f, "", 0, "%", 1, 1);
      continue;
    }
    check_flags(conv, &f, fmt, spec0, i - spec0);

    switch (conv) {
      case 's': case 'S': case 'b': case 'B': case 'h': case 'H': case 'c': case 'C': {
        void *o = pick_arg(args, &ai, fixed, &last, &have_last, relative, fmt, spec0, i - spec0);
        char *heap = NULL;
        const char *body;
        int64_t blen, k, bunits;
        if (conv == 'b' || conv == 'B') {
          /* a Boolean prints its value, anything else non-null prints "true",
             and the word is five letters long when the value is false */
          const char *t = (box_kind(o) == 1) ? (((tyboolbox *)o)->v ? "true" : "false")
                                             : (o ? "true" : "false");
          body = t;
          blen = (int64_t)strlen(t);
          if (P >= 0 && blen > P) blen = P;
          bunits = blen; /* the word is ASCII: a byte is a code unit */
        } else if (conv == 'h' || conv == 'H') {
          /* Java's %h is Integer.toHexString(arg.hashCode()), so it is the
             object's own hashCode that answers and not the identity the
             runtime hands out from ty_obj_hash -- a String hashes by its
             characters, an Integer by its value. The slot is the second of
             Object's vtable, after toString. */
          tystr *s = o ? ty_unsigned_string_int(
                             ((int32_t(*)(void *))((tyobj *)o)->cls->vtable[1])(o), 16)
                       : ty_str_intern("null");
          body = TY_STR_DATA(s);
          blen = s->blen;
          if (P >= 0 && blen > P) blen = P;
          bunits = blen; /* hexadecimal digits are ASCII too */
        } else if (conv == 'c' || conv == 'C') {
          /* a null argument prints as "null" here too, which is why the
             character is fetched only once there is an argument to fetch it
             from */
          tystr *s = o ? ty_str_of_char((uint16_t)char_arg(conv, o)) : ty_str_intern("null");
          body = TY_STR_DATA(s);
          blen = s->blen;
          bunits = s->ulen;
        } else {
          /* The precision of %s cuts the string to that many characters, and a
             character is a code unit: %.1s of one emoji is its high surrogate,
             which is the string Java's Formatter hands to substring. Cutting
             bytes instead would leave a string that is not a prefix of units. */
          tystr *s = ty_str_of_obj(o);
          if (P >= 0 && P < s->ulen) s = str_slice(s, 0, P);
          body = TY_STR_DATA(s);
          blen = s->blen;
          bunits = s->ulen;
        }
        if (conv == 'S' || conv == 'H' || conv == 'C' || conv == 'B') {
          heap = (char *)malloc((size_t)(blen > 0 ? blen : 1));
          for (k = 0; k < blen; k++)
            heap[k] = (char)ty_char_upper((unsigned char)body[k]);
          body = heap;
        }
        fmt_put(&out, &f, "", 0, body, blen, bunits);
        free(heap);
        break;
      }
      case 'd': case 'o': case 'x': case 'X': {
        void *o = pick_arg(args, &ai, fixed, &last, &have_last, relative, fmt, spec0, i - spec0);
        int kind;
        int64_t v;
        char raw[32], grp[80];
        const char *prefix = "";
        int64_t plen = 0, blen;
        int n;
        /* a null argument is not an integer Java refuses, it is the word
           "null": the conversion never sees a value to complain about */
        if (!o) {
          fmt_null(&out, &f, conv == 'X', -1);
          break;
        }
        kind = box_kind(o);
        v = int_arg(conv, o);
        if (conv == 'd') {
          uint64_t mag;
          if (v < 0) {
            prefix = "-";
            plen = 1;
            mag = 0 - (uint64_t)v;
          } else {
            mag = (uint64_t)v;
            if (f.plus) {
              prefix = "+";
              plen = 1;
            } else if (f.space) {
              prefix = " ";
              plen = 1;
            }
          }
          n = radix_digits(mag, 10, 0, raw);
        } else {
          /* the binary, octal and hex forms read the value as unsigned, and a
             Long is 64 bits wide where the other wrappers are 32 */
          uint64_t m = kind == 6 ? (uint64_t)v : (uint64_t)(uint32_t)v;
          n = radix_digits(m, conv == 'o' ? 8 : 16, conv == 'X', raw);
          if (f.alt) {
            if (conv == 'o') {
              /* Java's %#o always leads with a zero, even when the digits are
                 already a single zero: %#o of 0 is "00" */
              prefix = "0";
              plen = 1;
            } else {
              prefix = conv == 'X' ? "0X" : "0x";
              plen = 2;
            }
          }
        }
        if (f.comma) {
          blen = group_digits(raw, n, grp);
        } else {
          memcpy(grp, raw, (size_t)n);
          blen = n;
        }
        fmt_put(&out, &f, prefix, plen, grp, blen, blen);
        break;
      }
      case 'e': case 'E': case 'f': case 'g': case 'G': case 'a': case 'A': {
        void *o = pick_arg(args, &ai, fixed, &last, &have_last, relative, fmt, spec0, i - spec0);
        double v;
        int upper = conv == 'E' || conv == 'G' || conv == 'A';
        fmtbuf body, t;
        const char *prefix = "";
        int64_t plen = 0;
        if (!o) {
          fmt_null(&out, &f, upper, P);
          break;
        }
        v = float_arg(conv, o);
        fmtb_init(&body);
        fmt_hold(&body);
        if (v != v) {
          /* a NaN takes no sign and no zero padding, whatever the flags say */
          fmtb_add(&body, upper ? "NAN" : "NaN", 3);
          f.zero = 0;
        } else if (isinf(v)) {
          /* an infinity is signed like any other number -- Java prints
             "-Infinity" and "+(Infinity)" for the parenthesised negative --
             but the zero flag still does not fill around it */
          if (neg_sign(v)) {
            prefix = "-";
            plen = 1;
          } else if (f.plus) {
            prefix = "+";
            plen = 1;
          } else if (f.space) {
            prefix = " ";
            plen = 1;
          }
          fmtb_add(&body, upper ? "INFINITY" : "Infinity", 8);
          f.zero = 0;
        } else {
          if (neg_sign(v)) {
            prefix = "-";
            plen = 1;
          } else if (f.plus) {
            prefix = "+";
            plen = 1;
          } else if (f.space) {
            prefix = " ";
            plen = 1;
          }
          fmtb_init(&t);
          fmt_hold(&t);
          if (conv == 'a' || conv == 'A') {
            fmt_hex_body(&t, v, P, upper);
          } else if (conv == 'e' || conv == 'E') {
            fmt_sci_body(&t, v, P, upper, f.alt);
          } else if (conv == 'g' || conv == 'G') {
            fmt_gen_body(&t, v, P, upper);
          } else {
            fmt_fixed_body(&t, v, P, f.alt);
          }
          move_number(&body, &t, &f);
          free(t.buf);
          fmt_drop(&t);
        }
        fmt_put(&out, &f, prefix, plen, body.buf, body.len, body.len);
        free(body.buf);
        fmt_drop(&body);
        break;
      }
      case 't': case 'T':
        ty_unimplemented("String.format: %t is not implemented");
        break;
      default: {
        char msg[64];
        snprintf(msg, sizeof msg, "Conversion = '%c'", conv);
        fmt_abandon();
        ty_throw(ty_illarg(msg));
      }
    }
  }
  {
    tystr *r = ty_str_new(out.buf, out.len);
    free(out.buf);
    fmt_drop(&out);
    fmt_base = outer_base;
    return r;
  }
}

void ty_init(void) {
  ty_gc_init();
  /* Bring up the platform: Winsock, which has to be started before the first
     socket exists, and the standard streams, which have to be in binary mode
     before the first line is written to them. A no-op on POSIX, and it is here
     rather than at the first use because a program's startup is the one point
     every program passes through, before any thread is running. */
  typlat_init();
  /* The backstop: the handler for a fault the prologue check did not catch, and
     the alternate stack its own handler needs to run on when the fault was the
     stack. Installed here, before any thread exists, so that the process-wide
     half of it is installed exactly once and by a thread nothing else can race
     with. */
  /* TEYRU_NO_FAULT_HANDLER=1 leaves the runtime's own SIGSEGV handler off, so
     that a debugger or a sanitiser sees the fault the runtime would otherwise
     report as a stack overflow or swallow as a re-raise. It is a diagnosis
     switch and not a behaviour: the handler only ever turns a fault into a
     message. */
  const char *nofault = getenv("TEYRU_NO_FAULT_HANDLER");
  if (!(nofault && *nofault && strcmp(nofault, "0") != 0)) typlat_fault_handler_install(ty_stack_fault);
  /* The main thread takes its place in the registry here, with the stack bounds
     the collector scans it by. Nothing has allocated yet: the first allocation
     is what takes the main thread's slab, exactly as it takes every other
     thread's. */
  ty_thread_init();
}
