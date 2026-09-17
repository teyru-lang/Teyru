/* tyrt_reflect.c - the runtime side of java.lang.reflect.
 *
 * Everything here reads the static tables the compiler emits per class: the
 * class's modifiers, its fields (name, type, offset, flags), its methods and
 * constructors (name, types, flags, and an invoker), and its enum constants.
 * Nothing is built at run time, no object grows a field for reflection, and the
 * collector never walks any of it.
 *
 * A Method's invoker has one signature for every method in the program --
 * `void *(*)(void *self, void **args)` taking boxed arguments and answering a
 * boxed result -- because that is the only shape Method.invoke can call. The
 * compiler writes one per method; see internal/codegen/reflect.go.
 */

#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ------------------------------------------------------------------ helpers */

/* T is the class a Class value names. The value is either the wrapper
   ty_class_of_cls returns or the raw handle a class literal produces
   (`A.class` is `(tyobj*)&cls_A`), and ty_class_target accepts both. */
/* cm is a Class value cast through int64_t. A Class value comes in two forms --
   the object ty_class_of_cls hands out and the raw handle a class literal
   produces -- and the prelude holds either in a long, so every entry point
   below takes the handle and ty_class_target is the one place that knows which
   form arrived. */
static tyclass *T(int64_t cm) { return ty_class_target((void *)(intptr_t)cm); }

/* The reverse: an address of a class, handed back as the handle the prelude
   stores. */
static int64_t H(const void *p) { return (int64_t)(intptr_t)p; }

static void *builtin_ex(tyclass *k, const char *msg) { return ty_make_ex(k, msg); }

/* ------------------------------------------------------------- the box

   Reflection reads a primitive out of a field's raw storage and has to hand
   back the matching wrapper, and it puts one back the other way. The wrapper
   classes build their own instances in Teyru -- `Integer.valueOf(5)` is
   `new Integer(5)` and `intValue()` reads the class's own field
   (lib/04_boxing.teyru) -- but a runtime helper cannot call a method of the
   program it was linked into, so the two things a box is, beyond its class,
   are written out here: the allocation, and a payload at offset sizeof(tyobj)
   that is never wider than eight bytes. tyrt.h asserts that shape of every box
   struct and the compiler asserts the class it emits for each wrapper against
   it, so one pair of functions speaks for all eight kinds.

   The kind is the caller's business: a field's own type says which wrapper to
   build (ty_field_get), and an invoked argument was already checked against the
   parameter's type (checked_box). Neither this file nor the prelude re-reads a
   box that could be the wrong one. */

/* box_alloc builds the wrapper of class c around the width bytes at payload,
   which is the allocation and the store a constructor does. */
static void *box_alloc(tyclass *c, const void *payload, size_t width) {
  tyobj *o = (tyobj *)ty_alloc(sizeof(tyobj) + sizeof(int64_t));
  o->cls = c;
  memcpy((char *)o + sizeof(tyobj), payload, width);
  return o;
}

/* box_take is the other direction: the payload of o into dst, which the caller
   has typed. A null reference is the NullPointerException an unboxing
   conversion raises -- ty_rv_* never gets here with one, since checked_box
   answers IllegalArgumentException first. */
static void box_take(void *o, void *dst, size_t width) {
  if (!o) ty_npe();
  memcpy(dst, (char *)o + sizeof(tyobj), width);
}

/* named builds the message Java's reflection uses, "X.y" style, without a
   formatted-string call for every failure path. */
static char *describe(const char *a, const char *b) {
  size_t n = strlen(a) + strlen(b) + 2;
  char *s = (char *)malloc(n);
  if (!s) return NULL;
  snprintf(s, n, "%s.%s", a, b);
  return s;
}

static int32_t prim_size(int32_t kind) {
  switch (kind) {
  case 1: /* boolean */
  case 5: /* int */
  case 7: /* float */
    return 4;
  case 2: /* byte */
    return 1;
  case 3: /* short */
  case 4: /* char */
    return 2;
  case 8: /* double */
  case 6: /* long */
    return 8;
  }
  return 0;
}

/* ------------------------------------------------------- invoking */

/* Java wraps whatever an invoked method throws in InvocationTargetException,
   with the thrown object as its cause, and a program that reflects is written
   against that: catching InvocationTargetException and reading getCause is how
   it finds out what went wrong inside. The invoker runs under a catch frame of
   our own, so whatever comes out of one arrives here first.

   The caught object is pushed as a root before the wrapper is allocated: the
   only other reference to it is the frame on this C stack, and an allocation
   can collect. */
static void *call_invoker(void *fn, void *self, void **a) {
  tycatch frame;
  frame.prev = ty_cur_catch;
  frame.frames = ty_frames;
  frame.ex = NULL;
  ty_cur_catch = &frame;
  if (setjmp(frame.buf) == 0) {
    void *r = ((void *(*)(void *, void **))fn)(self, a);
    ty_cur_catch = frame.prev;
    return r;
  }
  ty_cur_catch = frame.prev;
  TY_ROOT_PUSH(frame.ex);
  tyobj *e = (tyobj *)ty_make_ex(TY_INVOCATION, NULL);
  /* make_ex writes the message and a null cause; the cause is the second word
     of the payload, which is where Throwable keeps it. */
  ((void **)((char *)e + sizeof(tyobj)))[1] = frame.ex;
  TY_ROOT_POP();
  ty_throw(e);
  return NULL;
}

/* The field table of a class, or NULL when the class has none. `declared`
   asks for the class's own fields; otherwise the public ones of the class and
   of every superclass, which is what Java's getFields answers with. */
static const tyfield *field_at(tyclass *k, int32_t declared, int32_t i) {
  if (!k) ty_throw(builtin_ex(TY_NPE, "class is null"));
  if (declared) {
    if (i < 0 || i >= k->nfields) ty_throw(builtin_ex(TY_AIOOBE, "field index"));
    return &k->fields[i];
  }
  for (tyclass *c = k; c; c = c->super) {
    for (int32_t j = 0; j < c->nfields; j++) {
      if (!(c->fields[j].mods & 0x0001)) continue; /* not public */
      if (i-- == 0) return &c->fields[j];
    }
  }
  ty_throw(builtin_ex(TY_AIOOBE, "field index"));
  return NULL;
}

static int32_t field_count(tyclass *k, int32_t declared) {
  if (!k) return 0;
  if (declared) return k->nfields;
  int32_t n = 0;
  for (tyclass *c = k; c; c = c->super) {
    for (int32_t j = 0; j < c->nfields; j++) {
      if (c->fields[j].mods & 0x0001) n++;
    }
  }
  return n;
}

/* Methods exclude constructors, as Java's do: getDeclaredMethods does not
   return them and getConstructors does not return anything else. */
static int32_t is_ctor(const tymethod *m) { return m->kind & TY_METH_CTOR; }

static const tymethod *method_at(tyclass *k, int32_t declared, int32_t i) {
  if (!k) ty_throw(builtin_ex(TY_NPE, "class is null"));
  if (declared) {
    for (int32_t j = 0; j < k->nmethods; j++) {
      if (is_ctor(&k->methods[j])) continue;
      if (i-- == 0) return &k->methods[j];
    }
    ty_throw(builtin_ex(TY_AIOOBE, "method index"));
  }
  for (tyclass *c = k; c; c = c->super) {
    for (int32_t j = 0; j < c->nmethods; j++) {
      const tymethod *m = &c->methods[j];
      if (is_ctor(m) || !(m->mods & 0x0001)) continue; /* not public */
      if (i-- == 0) return m;
    }
  }
  ty_throw(builtin_ex(TY_AIOOBE, "method index"));
  return NULL;
}

static int32_t method_count(tyclass *k, int32_t declared) {
  if (!k) return 0;
  int32_t n = 0;
  if (declared) {
    for (int32_t j = 0; j < k->nmethods; j++) {
      if (!is_ctor(&k->methods[j])) n++;
    }
    return n;
  }
  for (tyclass *c = k; c; c = c->super) {
    for (int32_t j = 0; j < c->nmethods; j++) {
      const tymethod *m = &c->methods[j];
      if (!is_ctor(m) && (m->mods & 0x0001)) n++;
    }
  }
  return n;
}

static const tymethod *ctor_at(tyclass *k, int32_t i) {
  if (!k) ty_throw(builtin_ex(TY_NPE, "class is null"));
  for (int32_t j = 0; j < k->nmethods; j++) {
    if (!is_ctor(&k->methods[j])) continue;
    if (i-- == 0) return &k->methods[j];
  }
  ty_throw(builtin_ex(TY_AIOOBE, "constructor index"));
  return NULL;
}

static int32_t ctor_count(tyclass *k) {
  if (!k) return 0;
  int32_t n = 0;
  for (int32_t j = 0; j < k->nmethods; j++) {
    if (is_ctor(&k->methods[j])) n++;
  }
  return n;
}

/* ------------------------------------------------------------- annotations */

static const tyannotation *ann_at(int64_t ah) {
  return (const tyannotation *)(intptr_t)ah;
}

int64_t ty_ann_type(int64_t ah) {
  const tyannotation *a = ann_at(ah);
  return a ? H(a->type) : 0;
}

int32_t ty_ann_argcount(int64_t ah) {
  const tyannotation *a = ann_at(ah);
  return a ? a->nargs : 0;
}

static const tyannoarg *arg_at(int64_t ah, int32_t i) {
  const tyannotation *a = ann_at(ah);
  if (!a || i < 0 || i >= a->nargs) return NULL;
  return &a->args[i];
}

tystr *ty_ann_argname(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  return g ? ty_str_new(g->name, (int64_t)strlen(g->name)) : NULL;
}

int32_t ty_ann_argkind(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  return g ? g->kind : TY_ANN_UNSUPPORTED;
}

int64_t ty_ann_argint(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  return g ? g->ival : 0;
}

double ty_ann_argdouble(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  return g ? g->dval : 0;
}

tystr *ty_ann_argstr(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  if (!g || !g->sval) return NULL;
  return ty_str_new(g->sval, (int64_t)strlen(g->sval));
}

int64_t ty_ann_argclass(int64_t ah, int32_t i) {
  const tyannoarg *g = arg_at(ah, i);
  return g && g->cval ? H(g->cval) : 0;
}

int32_t ty_ann_argindex(int64_t ah, tystr *name) {
  const tyannotation *a = ann_at(ah);
  if (!a || !name) return -1;
  for (int32_t i = 0; i < a->nargs; i++) {
    const char *have = a->args[i].name;
    if (strlen(have) == (size_t)name->len && memcmp(have, name->data, (size_t)name->len) == 0) {
      return i;
    }
  }
  return -1;
}

/* Whether two annotations are the same annotation with the same elements,
   which is Java's annotation equality: same type, same element names, same
   values. It lives here because the comparison is one loop over the kinds, and
   written in the prelude it would be that loop once per value kind. */
int32_t ty_ann_same(int64_t a1, int64_t a2) {
  const tyannotation *x = ann_at(a1), *y = ann_at(a2);
  if (x == y) return 1;
  if (!x || !y) return 0;
  if (x->type != y->type || x->nargs != y->nargs) return 0;
  for (int32_t i = 0; i < x->nargs; i++) {
    const tyannoarg *p = &x->args[i], *q = &y->args[i];
    if (strcmp(p->name, q->name) != 0 || p->kind != q->kind) return 0;
    switch (p->kind) {
      case TY_ANN_STRING:
      case TY_ANN_ENUM: {
        const char *ps = p->sval ? p->sval : "", *qs = q->sval ? q->sval : "";
        if (strcmp(ps, qs) != 0) return 0;
        if (p->kind == TY_ANN_ENUM && p->cval != q->cval) return 0;
        break;
      }
      case TY_ANN_CLASS:
        if (p->cval != q->cval) return 0;
        break;
      case TY_ANN_DOUBLE:
        if (p->dval != q->dval) return 0;
        break;
      default:
        if (p->ival != q->ival) return 0;
        break;
    }
  }
  return 1;
}

/* The annotations an element carries. Class-level ones live in the class
   record, member ones in the field or method record, so a caller asks the
   element it already has a handle for. */
int32_t ty_class_anncount(int64_t cm) {
  tyclass *k = T(cm);
  return k ? k->nannos : 0;
}

int64_t ty_class_annat(int64_t cm, int32_t i) {
  tyclass *k = T(cm);
  if (!k || i < 0 || i >= k->nannos) return 0;
  return H(&k->annos[i]);
}

int32_t ty_field_anncount(int64_t cm, int32_t declared, int32_t i) {
  const tyfield *f = field_at(T(cm), declared, i);
  return f ? f->nannos : 0;
}

int64_t ty_field_annat(int64_t cm, int32_t declared, int32_t i, int32_t at) {
  const tyfield *f = field_at(T(cm), declared, i);
  if (!f || at < 0 || at >= f->nannos) return 0;
  return H(&f->annos[at]);
}

int32_t ty_method_anncount(int64_t cm, int32_t declared, int32_t i) {
  const tymethod *m = method_at(T(cm), declared, i);
  return m ? m->nannos : 0;
}

/* A constructor's annotations. The constructor index is not a method-table
   index -- constructors are listed in the table but counted separately -- so
   the record is reached through the same lookup the other constructor
   accessors use. */
/* A parameter's name, which the emitter writes down: a route binds a request
   parameter whose name is the one the parameter was written with. */
tystr *ty_method_paramname(int64_t cm, int32_t declared, int32_t i, int32_t p) {
  const tymethod *m = method_at(T(cm), declared, i);
  if (!m || !m->pnames || p < 0 || p >= m->nparams) return NULL;
  const char *n = m->pnames[p];
  return n ? ty_str_new(n, (int64_t)strlen(n)) : NULL;
}

/* A parameter's annotations, which is where a @Value or an @Autowired written
   on a parameter is read from. */
int32_t ty_method_paramanncount(int64_t cm, int32_t declared, int32_t i, int32_t p) {
  const tymethod *m = method_at(T(cm), declared, i);
  if (!m || !m->pnannos || p < 0 || p >= m->nparams) return 0;
  return m->pnannos[p];
}

int64_t ty_method_paramannat(int64_t cm, int32_t declared, int32_t i, int32_t p, int32_t at) {
  const tymethod *m = method_at(T(cm), declared, i);
  if (!m || !m->pannos || p < 0 || p >= m->nparams) return 0;
  const tyannotation *list = m->pannos[p];
  if (!list || at < 0 || at >= m->pnannos[p]) return 0;
  return H(&list[at]);
}

/* A constructor's parameter annotations, reached through the constructor
   lookup rather than the method table: a constructor's index is not a
   method-table index. */
tystr *ty_ctor_paramname(int64_t cm, int32_t i, int32_t p) {
  const tymethod *m = ctor_at(T(cm), i);
  if (!m || !m->pnames || p < 0 || p >= m->nparams) return NULL;
  const char *n = m->pnames[p];
  return n ? ty_str_new(n, (int64_t)strlen(n)) : NULL;
}

int32_t ty_ctor_paramanncount(int64_t cm, int32_t i, int32_t p) {
  const tymethod *m = ctor_at(T(cm), i);
  if (!m || !m->pnannos || p < 0 || p >= m->nparams) return 0;
  return m->pnannos[p];
}

int64_t ty_ctor_paramannat(int64_t cm, int32_t i, int32_t p, int32_t at) {
  const tymethod *m = ctor_at(T(cm), i);
  if (!m || !m->pannos || p < 0 || p >= m->nparams) return 0;
  const tyannotation *list = m->pannos[p];
  if (!list || at < 0 || at >= m->pnannos[p]) return 0;
  return H(&list[at]);
}

int32_t ty_ctor_anncount(int64_t cm, int32_t i) {
  const tymethod *m = ctor_at(T(cm), i);
  return m ? m->nannos : 0;
}

int64_t ty_ctor_annat(int64_t cm, int32_t i, int32_t at) {
  const tymethod *m = ctor_at(T(cm), i);
  if (!m || at < 0 || at >= m->nannos) return 0;
  return H(&m->annos[at]);
}

int64_t ty_method_annat(int64_t cm, int32_t declared, int32_t i, int32_t at) {
  const tymethod *m = method_at(T(cm), declared, i);
  if (!m || at < 0 || at >= m->nannos) return 0;
  return H(&m->annos[at]);
}

/* -------------------------------------------------------------- Class */

int64_t ty_class_handle(void *c) { return H(ty_class_target(c)); }

int32_t ty_class_mods(int64_t cm) {
  tyclass *k = T(cm);
  return k ? k->mods : 0;
}

int32_t ty_class_primkind(int64_t cm) {
  tyclass *k = T(cm);
  return k ? k->prim : 0;
}

int32_t ty_class_isinterface(int64_t cm) {
  return (T(cm)->flags & 1) != 0;
}

/* The superclass, or NULL where Java has none: Object, an interface, a
   primitive. Java answers null for those three and ty_class_superof is what
   Class.getSuperclass hands that answer back through. */
int64_t ty_class_superof(int64_t cm) {
  tyclass *k = T(cm);
  if (k->flags & 1) return 0;
  return H(k->super);
}

int32_t ty_class_isprim(int64_t cm) { return ty_class_primkind(cm) != 0; }

int32_t ty_class_isarray(int64_t cm) {
  tyclass *k = T(cm);
  return k && (k->flags & TY_CLS_ARRAY) ? 1 : 0;
}

int32_t ty_class_isenum(int64_t cm) {
  tyclass *k = T(cm);
  return k && (k->mods & 0x4000) ? 1 : 0; /* Modifier.ENUM */
}

int32_t ty_class_isrecord(int64_t cm) {
  tyclass *k = T(cm);
  return k && (k->flags & TY_CLS_RECORD) ? 1 : 0;
}

int32_t ty_class_isannotation(int64_t cm) {
  tyclass *k = T(cm);
  return k && (k->mods & 0x2000) ? 1 : 0; /* Modifier.ANNOTATION */
}

int32_t ty_class_isinstance(int64_t cm, void *o) {
  tyclass *k = T(cm);
  if (!k) return 0;
  if (k->prim) return 0; /* nothing is an instance of a primitive */
  return ty_instanceof(o, k);
}

int32_t ty_class_assignable(int64_t cm, int64_t other) {
  tyclass *to = T(cm), *from = T(other);
  if (!to || !from) return 0;
  if (to == from) return 1;
  if (to->prim || from->prim) return 0;
  /* an array class accepts any array, which is the one class every array
     value in a program has (see the array classes in the runtime) */
  if ((to->flags & TY_CLS_ARRAY) && (from->flags & TY_CLS_ARRAY)) return 1;
  return ty_class_is_sub(from, to);
}

int32_t ty_class_ifacecount(int64_t cm) {
  tyclass *k = T(cm);
  return k ? k->niface : 0;
}

int64_t ty_class_ifaceat(int64_t cm, int32_t i) {
  tyclass *k = T(cm);
  if (!k || i < 0 || i >= k->niface) return 0;
  return H(k->ifaces[i]);
}

tystr *ty_class_simplename(int64_t cm) {
  tyclass *k = T(cm);
  if (!k || !k->name) return NULL;
  const char *n = k->name;
  const char *dot = strrchr(n, '.');
  const char *base = dot ? dot + 1 : n;
  const char *dollar = strrchr(base, '$');
  if (dollar && dollar[1]) base = dollar + 1;
  return ty_str_new(base, (int64_t)strlen(base));
}

int32_t ty_class_enumcount(int64_t cm) {
  tyclass *k = T(cm);
  return k ? k->nconsts : 0;
}

void *ty_class_enumat(int64_t cm, int32_t i) {
  tyclass *k = T(cm);
  if (!k || i < 0 || i >= k->nconsts) return NULL;
  /* The constants are allocated by the class's own initializer, so asking for
     one runs it first -- which is what Java does when getEnumConstants reads
     the enum's values(). Without it a class a program only named by its class
     literal answered null for every constant. */
  ty_clinit(k);
  return k->consts[i];
}

/* forName answers from the program's own class table, which generated startup
   fills in. The name is the binary name Java uses: "teyru.List",
   "main.Outer$Inner". Array and primitive names are not loadable in Java
   either -- Class.forName("int") throws -- so they are not accepted here. */
void *ty_class_forname_in(tystr *name, tyclass **table, int32_t count, tyclass *clscls) {
  if (!name) ty_throw(builtin_ex(TY_NPE, "name is null"));
  for (int32_t i = 0; i < count; i++) {
    tyclass *k = table[i];
    if (k && k->name && strlen(k->name) == (size_t)name->len &&
        strncmp(k->name, name->data, (size_t)name->len) == 0) {
      return ty_class_make(H(k), clscls);
    }
  }
  /* The message is the name that was asked for. It is built as a string object
     so the bytes are the collector's to free, rather than a C buffer this
     function would leak on the way out. */
  tystr *asked = ty_str_new(name->data, name->len);
  ty_throw(builtin_ex(TY_CNF, asked->data));
  return NULL;
}

/* ----------------------------------------------------------------- fields */

int32_t ty_class_fieldcount(int64_t cm, int32_t declared) {
  return field_count(T(cm), declared);
}

tystr *ty_field_name(int64_t cm, int32_t declared, int32_t i) {
  return ty_str_intern(field_at(T(cm), declared, i)->name);
}

int64_t ty_field_type(int64_t cm, int32_t declared, int32_t i) {
  return H(field_at(T(cm), declared, i)->type);
}

/* The declared element type of a container field, or 0 when the field's
   declared type carries none -- the answer is what the compiler wrote into the
   descriptor (elem), and it is what makes a List<Person> readable as one. */
int64_t ty_field_elem(int64_t cm, int32_t declared, int32_t i) {
  return H(field_at(T(cm), declared, i)->elem);
}

int64_t ty_field_owner(int64_t cm, int32_t declared, int32_t i) {
  return H(field_at(T(cm), declared, i)->owner);
}

int32_t ty_field_mods(int64_t cm, int32_t declared, int32_t i) {
  return field_at(T(cm), declared, i)->mods;
}

static void *field_addr(const tyfield *f, void *self) {
  if (f->off < 0) {
    if (!f->addr) ty_throw(builtin_ex(TY_NPE, "static field has no address"));
    return f->addr;
  }
  if (!self) ty_throw(builtin_ex(TY_NPE, "field access on null"));
  return (char *)self + f->off;
}

void *ty_field_get(int64_t cm, int32_t declared, int32_t i, void *self) {
  const tyfield *f = field_at((tyclass *)(intptr_t)cm, declared, i);
  void *p = field_addr(f, self);
  switch (f->prim) {
  case 1:
    return box_alloc(TY_BOX[1], p, sizeof(int32_t));
  case 2:
    return box_alloc(TY_BOX[2], p, sizeof(int8_t));
  case 3:
    return box_alloc(TY_BOX[3], p, sizeof(int16_t));
  case 4:
    return box_alloc(TY_BOX[4], p, sizeof(uint16_t));
  case 5:
    return box_alloc(TY_BOX[5], p, sizeof(int32_t));
  case 6:
    return box_alloc(TY_BOX[6], p, sizeof(int64_t));
  case 7:
    return box_alloc(TY_BOX[7], p, sizeof(float));
  case 8:
    return box_alloc(TY_BOX[8], p, sizeof(double));
  }
  return *(void **)p;
}

/* A final field is refused the way Java refuses it, and letting the caller
   say it has made the field accessible -- which Field.setAccessible(true)
   records -- turns the refusal off, as it does in Java. */
void ty_field_set(int64_t cm, int32_t declared, int32_t i, void *self, void *v, int32_t accessible) {
  const tyfield *f = field_at((tyclass *)(intptr_t)cm, declared, i);
  /* A static final field is refused however the caller asked, which is what
     javac does: the JDK stopped letting a write through even after
     setAccessible. setAccessible is what makes an *instance* final writable,
     and that is the case Java still allows. */
  if ((f->mods & 0x0008) && (f->mods & 0x0010)) {
    ty_throw(builtin_ex(TY_ILLACCESS, "can not set a static final field"));
  }
  if ((f->mods & 0x0010) && !accessible) { /* Modifier.FINAL */
    char *msg = describe(f->owner ? f->owner->name : "?", f->name);
    ty_throw(builtin_ex(TY_ILLACCESS, msg ? msg : "field is final"));
  }
  void *p = field_addr(f, self);
  switch (f->prim) {
  case 1:
    box_take(v, p, sizeof(int32_t));
    return;
  case 2:
    box_take(v, p, sizeof(int8_t));
    return;
  case 3:
    box_take(v, p, sizeof(int16_t));
    return;
  case 4:
    box_take(v, p, sizeof(uint16_t));
    return;
  case 5:
    box_take(v, p, sizeof(int32_t));
    return;
  case 6:
    box_take(v, p, sizeof(int64_t));
    return;
  case 7:
    box_take(v, p, sizeof(float));
    return;
  case 8:
    box_take(v, p, sizeof(double));
    return;
  }
  *(void **)p = v;
}

/* ---------------------------------------------------------------- methods */

int32_t ty_class_methodcount(int64_t cm, int32_t declared) {
  return method_count(T(cm), declared);
}

tystr *ty_method_name(int64_t cm, int32_t declared, int32_t i) {
  return ty_str_intern(method_at(T(cm), declared, i)->name);
}

int64_t ty_method_owner(int64_t cm, int32_t declared, int32_t i) {
  return H(method_at(T(cm), declared, i)->owner);
}

int32_t ty_method_mods(int64_t cm, int32_t declared, int32_t i) {
  return method_at(T(cm), declared, i)->mods;
}

int32_t ty_method_kind(int64_t cm, int32_t declared, int32_t i) {
  return method_at(T(cm), declared, i)->kind;
}

int64_t ty_method_ret(int64_t cm, int32_t declared, int32_t i) {
  return H(method_at(T(cm), declared, i)->ret);
}

int32_t ty_method_paramcount(int64_t cm, int32_t declared, int32_t i) {
  return method_at(T(cm), declared, i)->nparams;
}

int64_t ty_method_param(int64_t cm, int32_t declared, int32_t i, int32_t p) {
  const tymethod *m = method_at(T(cm), declared, i);
  if (p < 0 || p >= m->nparams) return 0;
  return H(m->params[p]);
}

/* invoke is a virtual call, as it is in Java: the invoker the compiler wrote
   for the method dispatches through the receiver's own class when the method
   is virtual, so an override runs. The receiver has to be an instance of the
   class the method was found on, or Java throws IllegalArgumentException. */
void *ty_method_invoke(int64_t cm, int32_t declared, int32_t i, void *self, tyarr *args) {
  const tymethod *m = method_at((tyclass *)(intptr_t)cm, declared, i);
  void **a = args && args->len ? (void **)args->data : NULL;
  if (args && args->len != m->nparams) {
    char *msg = describe(m->owner ? m->owner->name : "?", m->name);
    ty_throw(builtin_ex(TY_ILLARG, msg ? msg : "wrong number of arguments"));
  }
  if (!(m->kind & TY_METH_STATIC)) {
    if (!self) ty_throw(builtin_ex(TY_NPE, "invoke on null"));
    if (!ty_instanceof(self, m->owner)) {
      char *msg = describe(m->owner ? m->owner->name : "?", m->name);
      ty_throw(builtin_ex(TY_ILLARG, msg ? msg : "receiver is not an instance"));
    }
  } else {
    self = NULL;
  }
  return call_invoker(m->fn, self, a);
}

int32_t ty_class_ctorcount(int64_t cm) { return ctor_count(T(cm)); }

int32_t ty_ctor_mods(int64_t cm, int32_t i) { return ctor_at(T(cm), i)->mods; }

int32_t ty_ctor_paramcount(int64_t cm, int32_t i) { return ctor_at(T(cm), i)->nparams; }

int64_t ty_ctor_param(int64_t cm, int32_t i, int32_t p) {
  const tymethod *m = ctor_at(T(cm), i);
  if (p < 0 || p >= m->nparams) return 0;
  return H(m->params[p]);
}

void *ty_ctor_new(int64_t cm, int32_t i, tyarr *args) {
  tyclass *k = (tyclass *)(intptr_t)cm;
  const tymethod *m = ctor_at(k, i);
  void **a = args && args->len ? (void **)args->data : NULL;
  if (args && args->len != m->nparams) {
    ty_throw(builtin_ex(TY_ILLARG, "wrong number of arguments"));
  }
  return call_invoker(m->fn, NULL, a);
}

/* Class.newInstance calls the no-argument constructor, as it does in Java, and
   refuses an abstract class or an interface the way Java refuses those. */
void *ty_class_newinst(int64_t cm) {
  tyclass *k = T(cm);
  if (!k) ty_throw(builtin_ex(TY_NPE, "class is null"));
  if (k->prim || (k->flags & TY_CLS_ARRAY)) {
    ty_throw(builtin_ex(TY_INSTANTIATION, "cannot instantiate a primitive or an array class"));
  }
  if ((k->flags & 1) || (k->mods & 0x0400)) { /* interface or abstract */
    ty_throw(builtin_ex(TY_INSTANTIATION, k->name));
  }
  for (int32_t j = 0; j < k->nmethods; j++) {
    const tymethod *m = &k->methods[j];
    if (is_ctor(m) && m->nparams == 0) {
      return call_invoker(m->fn, NULL, NULL);
    }
  }
  ty_throw(builtin_ex(TY_INSTANTIATION, k->name));
  return NULL;
}

/* ------------------------------------------------------------------ Array */

/* Teyru's arrays are one runtime class that records what it promised for its
   elements, so a component type is that promise: Array.get boxes according to
   it and a store of the wrong class is the ArrayStoreException Java raises.
   For a primitive component the promise is the primitive's own class, whose
   kind is what tells Array.get which box to build. */
void *ty_reflect_array_new(int64_t component, int32_t len) {
  tyclass *k = T(component);
  if (!k) ty_throw(builtin_ex(TY_NPE, "component type is null"));
  if (len < 0) ty_throw(ty_negarr());
  int64_t es = k->prim ? prim_size(k->prim) : (int64_t)sizeof(void *);
  tyarr *a = ty_array_new(len, es);
  a->refs = k->prim ? 0 : 1;
  a->elemcls = k;
  return a;
}

int32_t ty_reflect_array_len(void *o) {
  if (!o) ty_throw(builtin_ex(TY_NPE, "array is null"));
  if (!(((tyobj *)o)->cls->flags & TY_CLS_ARRAY)) {
    ty_throw(builtin_ex(TY_ILLARG, "not an array"));
  }
  return (int32_t)((tyarr *)o)->len;
}

void *ty_reflect_array_get(void *o, int32_t i) {
  if (!o) ty_throw(builtin_ex(TY_NPE, "array is null"));
  tyarr *a = (tyarr *)o;
  if (!(((tyobj *)o)->cls->flags & TY_CLS_ARRAY)) {
    ty_throw(builtin_ex(TY_ILLARG, "not an array"));
  }
  if (i < 0 || i >= a->len) ty_throw(ty_aioobe(i, a->len));
  int32_t kind = a->elemcls ? a->elemcls->prim : 0;
  if (a->refs) return ((void **)a->data)[i];
  switch (kind) {
  case 1:
    return box_alloc(TY_BOX[1], &((int32_t *)a->data)[i], sizeof(int32_t));
  case 2:
    return box_alloc(TY_BOX[2], &((int8_t *)a->data)[i], sizeof(int8_t));
  case 3:
    return box_alloc(TY_BOX[3], &((int16_t *)a->data)[i], sizeof(int16_t));
  case 4:
    return box_alloc(TY_BOX[4], &((uint16_t *)a->data)[i], sizeof(uint16_t));
  case 5:
    return box_alloc(TY_BOX[5], &((int32_t *)a->data)[i], sizeof(int32_t));
  case 6:
    return box_alloc(TY_BOX[6], &((int64_t *)a->data)[i], sizeof(int64_t));
  case 7:
    return box_alloc(TY_BOX[7], &((float *)a->data)[i], sizeof(float));
  case 8:
    return box_alloc(TY_BOX[8], &((double *)a->data)[i], sizeof(double));
  }
  /* An array that predates the promise, or one built by the runtime, still has
     to answer: the element size says how wide the slot is, and 4 and 8 are
     answered as an int, a long or a double by the values a program can store
     through the same slot. Refusing is worse than a guess here: this is the
     case of an array the program never created through this API. */
  switch (a->esize) {
  case 1:
    return box_alloc(TY_BOX[2], &((int8_t *)a->data)[i], sizeof(int8_t));
  case 2:
    return box_alloc(TY_BOX[3], &((int16_t *)a->data)[i], sizeof(int16_t));
  case 4:
    return box_alloc(TY_BOX[5], &((int32_t *)a->data)[i], sizeof(int32_t));
  default:
    return box_alloc(TY_BOX[6], &((int64_t *)a->data)[i], sizeof(int64_t));
  }
}

void ty_reflect_array_set(void *o, int32_t i, void *v) {
  if (!o) ty_throw(builtin_ex(TY_NPE, "array is null"));
  tyarr *a = (tyarr *)o;
  if (!(((tyobj *)o)->cls->flags & TY_CLS_ARRAY)) {
    ty_throw(builtin_ex(TY_ILLARG, "not an array"));
  }
  if (i < 0 || i >= a->len) ty_throw(ty_aioobe(i, a->len));
  int32_t kind = a->elemcls ? a->elemcls->prim : 0;
  if (a->refs) {
    ty_array_store_ref(a, a->elemcls, i, v);
    return;
  }
  switch (kind) {
  case 1:
    box_take(v, &((int32_t *)a->data)[i], sizeof(int32_t));
    return;
  case 2:
    box_take(v, &((int8_t *)a->data)[i], sizeof(int8_t));
    return;
  case 3:
    box_take(v, &((int16_t *)a->data)[i], sizeof(int16_t));
    return;
  case 4:
    box_take(v, &((uint16_t *)a->data)[i], sizeof(uint16_t));
    return;
  case 5:
    box_take(v, &((int32_t *)a->data)[i], sizeof(int32_t));
    return;
  case 6:
    box_take(v, &((int64_t *)a->data)[i], sizeof(int64_t));
    return;
  case 7:
    box_take(v, &((float *)a->data)[i], sizeof(float));
    return;
  case 8:
    box_take(v, &((double *)a->data)[i], sizeof(double));
    return;
  }
  switch (a->esize) {
  case 1:
    {
      int32_t x;
      box_take(v, &x, sizeof x);
      ((int8_t *)a->data)[i] = (int8_t)x;
    }
    return;
  case 2:
    {
      int32_t x;
      box_take(v, &x, sizeof x);
      ((int16_t *)a->data)[i] = (int16_t)x;
    }
    return;
  case 4:
    box_take(v, &((int32_t *)a->data)[i], sizeof(int32_t));
    return;
  default:
    box_take(v, &((int64_t *)a->data)[i], sizeof(int64_t));
    return;
  }
}

/* ------------------------------------------------------------ arguments */

/* checked_box is the null and type test every argument conversion starts with:
   Java answers IllegalArgumentException for both, and the message names the
   parameter type so that a program which got it wrong can see why. */
static void *checked_box(void *o, int32_t kind) {
  if (!o) ty_throw(builtin_ex(TY_ILLARG, "argument is null for a primitive parameter"));
  if (TY_BOX[kind] && ((tyobj *)o)->cls != TY_BOX[kind]) {
    ty_throw(builtin_ex(TY_ILLARG, "argument type mismatch"));
  }
  return o;
}

int32_t ty_rv_int(void *o) { int32_t v; box_take(checked_box(o, 5), &v, sizeof v); return v; }
int64_t ty_rv_long(void *o) { int64_t v; box_take(checked_box(o, 6), &v, sizeof v); return v; }
double ty_rv_double(void *o) { double v; box_take(checked_box(o, 8), &v, sizeof v); return v; }
float ty_rv_float(void *o) { float v; box_take(checked_box(o, 7), &v, sizeof v); return v; }
int16_t ty_rv_short(void *o) { int16_t v; box_take(checked_box(o, 3), &v, sizeof v); return v; }
int8_t ty_rv_byte(void *o) { int8_t v; box_take(checked_box(o, 2), &v, sizeof v); return v; }
uint16_t ty_rv_char(void *o) { uint16_t v; box_take(checked_box(o, 4), &v, sizeof v); return v; }
int32_t ty_rv_bool(void *o) { int32_t v; box_take(checked_box(o, 1), &v, sizeof v); return v; }

void *ty_rv_ref(void *o, tyclass *want) {
  if (o && want && !ty_instanceof(o, want)) {
    ty_throw(builtin_ex(TY_ILLARG, "argument type mismatch"));
  }
  return o;
}
