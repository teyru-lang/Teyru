package codegen

// This file is the LLVM back end. It lowers a checked program straight to LLVM
// IR, so the program is compiled by LLVM without a C compiler ever seeing it:
// the module is assembled here, from the same semantic model the C back end
// uses, and only the runtime (internal/runtime/src) is still C.
//
// What it must agree with is not the C emitter's spelling but the runtime's
// ABI, because the runtime only knows the ABI:
//
//   - an object is a `tyobj` header followed by its fields at the offsets
//     util.FieldLayout computes, and the runtime's own ty_alloc fast path is
//     what allocates it;
//   - a class is a `tyclass` record whose `refoffs`/`nref` the conservative
//     collector walks -- a wrong offset there frees a live object;
//   - a try statement is a `tycatch` frame linked into the thread-local
//     `ty_cur_catch` and armed with setjmp, which is what ty_throw longjmps to;
//   - vtable slots 0, 1 and 2 are toString, hashCode and equals, which the
//     runtime itself calls through (`print_uncaught`, `ty_obj_hash`,
//     `ty_obj_equal`), so those three are never dropped from a class that can
//     have instances.
//
// The generated code pushes no GC roots: like the C back end's, its live
// references sit in its stack frame, which the collector scans conservatively. A
// loop body begins with a safepoint, the cooperative half of the stop-the-world
// protocol.
//
// Only what a program reaches is written. A class's table is emitted because
// something names it, a method's body because something calls it, and a vtable
// slot because a call site dispatches through it -- which is what keeps a
// program that says `System.out.println("hi")` from pinning the whole standard
// library into its executable.
//
// Everything outside the subset this back end implements is refused with a
// TY-INT-0100 diagnostic naming the missing piece. Nothing falls back to the C
// back end, and nothing is emitted that the back end cannot justify.

import (
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

// CodeLLVMUnsupported is the diagnostic the LLVM back end reports for a
// construct it cannot lower. The message names the construct, so a program that
// meets the boundary is told which feature it met rather than that the build
// failed.
const CodeLLVMUnsupported = "TY-INT-0100"

// Refusal is why the LLVM back end could not compile a program.
type Refusal struct {
	Code  string
	Pos   source.Pos
	Thing string // the construct, as the message names it
	Where string // the method whose emission met it
}

func (r *Refusal) Error() string { return r.Message() }

// Message is what the diagnostic says: the construct, and the method whose
// emission met it.
func (r *Refusal) Message() string {
	if r.Where != "" {
		return r.Thing + ", in " + r.Where
	}
	return r.Thing
}

// bail carries a refusal out of the emitter. Emission stops at the first
// construct it cannot lower, and one can be met at any depth -- a call inside an
// argument inside a loop -- so threading an error back through every expression
// and statement would put a check in every one of them for a path that ends the
// build. The value never crosses EmitLLVM's boundary: it is recovered there and
// turned into the diagnostic. It is the device the standard library's parsers
// use for the same reason.
type bail struct{ r *Refusal }

// EmitLLVM returns the LLVM IR module of a program, or the refusal that stopped
// it.
func EmitLLVM(p *sema.Program) (ir string, ref *Refusal) {
	e := newLLVMEmitter(p)
	defer func() {
		if r := recover(); r != nil {
			b, ok := r.(bail)
			if !ok {
				panic(r)
			}
			if os.Getenv("TEYRU_LLVM_TRACE") != "" {
				fmt.Fprintln(os.Stderr, string(debug.Stack()))
			}
			ir, ref = "", b.r
		}
	}()
	e.run()
	return e.module(), nil
}

// ---------------------------------------------------------------- runtime structs

// The runtime's own layouts, not this back end's invention: the module reads and
// writes fields of these structs, and the collector, the class loader and the
// exception machinery read what it writes. The offsets were taken from a probe
// program compiled against internal/runtime/src/tyrt.h (clang 22, x86-64 Linux):
//
//	jmp_buf 200          tycatch { buf 0, prev 200, ex 208 }              216
//	tystr  { obj 0, len 8, data 16 }                                      24
//	tyarr  { obj 0, len 8, data 16, esize 24, refs 28, elemcls 32 }       40
//	tymap  { sel 0, fn 8 }                                                16
//	tyclass { name 0, id 8, flags 12, super 16, niface 24, ifaces 32,
//	          nvt 40, vtable 48, clinit 56, isize 64, isel 68, imap 72,
//	          nsub 80, subs 88, nref 96, refoffs 104, mods 112, prim 116,
//	          fields 120, nfields 128, methods 136, nmethods 144,
//	          consts 152, nconsts 160, annos 168, nannos 176 }           184
//	tythread { bump 0, bump_end 8, alloc_since 16, chunk 24, roots 32, sp 40 }
type rtField struct {
	name string
	ty   string // an LLVM type
	off  int64
}

// sizeOfLLVM is the size of the LLVM types the runtime structs are built from.
// They are all primitives whose size is the same on every target this back end
// accepts.
func sizeOfLLVM(ty string) int64 {
	switch ty {
	case "i8":
		return 1
	case "i16":
		return 2
	case "i32", "float":
		return 4
	case "i64", "double", "ptr":
		return 8
	case "[200 x i8]":
		return 200
	}
	return 8
}

var tyclassLayout = []rtField{
	{"name", "ptr", 0},
	{"id", "i32", 8},
	{"flags", "i32", 12},
	{"super", "ptr", 16},
	{"niface", "i32", 24},
	{"ifaces", "ptr", 32},
	{"nvt", "i32", 40},
	{"vtable", "ptr", 48},
	{"clinit", "ptr", 56},
	{"isize", "i32", 64},
	{"isel", "i32", 68},
	{"imap", "ptr", 72},
	{"nsub", "i32", 80},
	{"subs", "ptr", 88},
	{"nref", "i32", 96},
	{"refoffs", "ptr", 104},
	{"mods", "i32", 112},
	{"prim", "i32", 116},
	{"fields", "ptr", 120},
	{"nfields", "i32", 128},
	{"methods", "ptr", 136},
	{"nmethods", "i32", 144},
	{"consts", "ptr", 152},
	{"nconsts", "i32", 160},
	{"annos", "ptr", 168},
	{"nannos", "i32", 176},
}

// tySB is StringBuilder's own struct: its state is the runtime's, not the
// generated program's, so the class is a typedef of it rather than a struct of
// instance fields.
var tySBLayout = []rtField{
	{"obj", "ptr", 0},
	{"len", "i64", 8},
	{"cap", "i64", 16},
	{"buf", "ptr", 24},
}

var tymapLayout = []rtField{
	{"sel", "i32", 0},
	{"fn", "ptr", 8},
}

var tystrLayout = []rtField{
	{"obj", "ptr", 0},
	{"len", "i64", 8},
	{"data", "ptr", 16},
}

var tyarrLayout = []rtField{
	{"obj", "ptr", 0},
	{"len", "i64", 8},
	{"data", "ptr", 16},
	{"esize", "i32", 24},
	{"refs", "i32", 28},
	{"elemcls", "ptr", 32},
}

var tycatchLayout = []rtField{
	{"buf", "[200 x i8]", 0},
	{"prev", "ptr", 200},
	{"ex", "ptr", 208},
}

var tythreadLayout = []rtField{
	{"bump", "ptr", 0},
	{"bump_end", "ptr", 8},
	{"alloc_since", "i64", 16},
	{"chunk", "ptr", 24},
	{"roots", "ptr", 32},
	{"sp", "i64", 40},
}

// structEntry is one field of an emitted struct type: a named field, or the
// padding that puts the next one where the C compiler put it.
type structEntry struct {
	name string // "" for padding
	ty   string
}

// structDecl renders an LLVM struct type with explicit padding, so that a field
// sits exactly where the C compiler put it, and answers with the entries it
// wrote -- the order a global initializer has to follow -- and the index of each
// named field in it (which is what a gep names).
func structDecl(name string, layout []rtField) (string, []structEntry, map[string]int) {
	var b strings.Builder
	fmt.Fprintf(&b, "%%%s = type {", name)
	idx := make(map[string]int, len(layout))
	var entries []structEntry
	cur := int64(0)
	emit := func(en structEntry) {
		if len(entries) > 0 {
			b.WriteString(", ")
		}
		b.WriteString(en.ty)
		entries = append(entries, en)
	}
	for _, f := range layout {
		a := sizeOfLLVM(f.ty)
		if start := util.Align(cur, a); start > f.off {
			// The layout this back end was given disagrees with the alignment
			// the type needs. That is a bug in the table above, and a silently
			// shifted field is a corrupt object, so it stops here.
			panic(fmt.Sprintf("codegen: %s.%s at %d is not aligned to %d", name, f.name, f.off, a))
		} else if start < f.off {
			emit(structEntry{ty: fmt.Sprintf("[%d x i8]", f.off-start)})
		}
		idx[f.name] = len(entries)
		emit(structEntry{name: f.name, ty: f.ty})
		cur = f.off + a
	}
	if pad := util.Align(cur, 8) - cur; pad > 0 {
		emit(structEntry{ty: fmt.Sprintf("[%d x i8]", pad)})
	}
	b.WriteString(" }\n")
	return b.String(), entries, idx
}

// recordInit renders a global struct initializer in the order the emitted type
// declares its fields: every slot is written, and a slot with nothing to say --
// the padding -- is filled with zeroinitializer, because LLVM counts elements
// and a struct with a hole in it is a struct with the wrong number of them.
func recordInit(entries []structEntry, vals map[string]string) string {
	parts := make([]string, len(entries))
	for i, en := range entries {
		v := ""
		if en.name != "" {
			v = vals[en.name]
		}
		if v == "" {
			// padding, and a field of a class the runtime never reads: LLVM
			// wants a typed element even when the element is all zeroes
			v = en.ty + " zeroinitializer"
		}
		parts[i] = v
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// vneed is one virtual call site: a dispatch through slot idx of a receiver
// whose static type is owner. Every instantiated class that is a subtype of
// owner has to answer that slot with a real method, because any of them can be
// the value the call site sees.
type vneed struct {
	owner *ast.Class
	idx   int
}

type llvmEmitter struct {
	p *sema.Program

	// curPos is the expression or statement being lowered. A refusal raised
	// deep inside a helper -- one that converts a value, or lays out an object
	// -- has no position of its own, and a limit that names the construct but
	// not the line is one the user has to bisect their own file to find.
	curPos source.Pos

	types    strings.Builder
	globals  strings.Builder
	defs     strings.Builder
	declares strings.Builder
	declared map[string]bool

	strs      map[string]int // Teyru string literals, by value
	strOrder  []string
	cstrs     map[string]int // C strings, by value
	cstrOrder []string

	classes    map[*ast.Class]bool
	classOrder []*ast.Class
	inst       map[*ast.Class]bool
	methods    map[*ast.Method]bool
	mqueue     []*ast.Method
	vdisp      []vneed

	// boxUsed holds the wrapper classes boxing has reached, and exnUsed the
	// exception classes the runtime helpers the module calls will throw. Both
	// are what the startup installs in TY_BOX and TY_NPE..TY_ARRAYSTORE, and
	// both bring their class in: the runtime builds the object itself, and it
	// reads that class's vtable slots 0..2 through print_uncaught, ty_obj_hash
	// and ty_obj_equal.
	boxUsed map[ast.PrimKind]bool
	exnUsed map[string]*ast.Class
	// arrays is set once an array is allocated: the runtime's ty_array_new
	// stamps TY_ARRAY on every array it makes, so the class has to be installed.
	arrays bool

	trapNames map[string]string // a vtable slot's signature -> its trap
	trapOrder []string

	// classWraps interns the object a class literal is built from, one per
	// class a literal names.
	classWraps map[string]string

	// primClasses are the classes a primitive class literal names. A primitive
	// is not one of the program's classes -- no value is ever an instance of
	// one -- so the table behind each literal is synthesized here, as the C
	// back end synthesizes it.
	primClasses map[ast.PrimKind]bool

	// ifaceNeeds are the interface call sites seen so far: a class that is
	// instantiated later still has to answer them, which is what makes the
	// closure over interface dispatch complete.
	ifaceNeeds []ifaceNeed

	// classIdx and structText hold each class's emitted struct type and where its
	// fields sit in it, so that the type and the field accesses cannot be walked
	// two different ways.
	classIdx   map[*ast.Class]map[*ast.Field]int
	structText map[*ast.Class]string
	capIdx     map[*ast.Class]map[string]int

	// tyclassEntries and tyclassIdx are the tyclass type's fields as emitted,
	// which is what a class record's initializer and a vtable lookup both walk.
	tyclassEntries []structEntry
	tyclassIdx     map[string]int

	// rtCtx names the runtime call whose arguments are being converted, so that
	// a conversion this back end cannot make says which call it was for.
	rtCtx string

	fb *fb // the function being emitted
}

func newLLVMEmitter(p *sema.Program) *llvmEmitter {
	e := &llvmEmitter{
		p:           p,
		declared:    map[string]bool{},
		strs:        map[string]int{},
		cstrs:       map[string]int{},
		classes:     map[*ast.Class]bool{},
		inst:        map[*ast.Class]bool{},
		methods:     map[*ast.Method]bool{},
		boxUsed:     map[ast.PrimKind]bool{},
		exnUsed:     map[string]*ast.Class{},
		trapNames:   map[string]string{},
		capIdx:      map[*ast.Class]map[string]int{},
		classWraps:  map[string]string{},
		primClasses: map[ast.PrimKind]bool{},
	}
	// The tyclass type's fields, computed once: the record initializer and the
	// vtable lookup both walk them, and they come from the same walk that writes
	// the type itself.
	_, e.tyclassEntries, e.tyclassIdx = structDecl("tyclass", tyclassLayout)
	return e
}

// run emits the whole module: the program's methods, then the classes they
// named, then the entry point.
func (e *llvmEmitter) run() {
	// Every program has Object and String in it: a string literal is an
	// instance of String, and Object is the root of the hierarchy the class
	// tables point at.
	e.needClass(e.p.Builtins.Object)
	e.needClass(e.p.Builtins.String)
	e.instantiate(e.p.Builtins.String)
	// The exception classes the runtime can build are instantiated whether or
	// not this program names one: ty_throw hands the object it made to whatever
	// catches it, and the catch reads its message through the virtual call its
	// class's vtable holds. A class the runtime can create without a call site
	// naming it is one whose vtable has to be real.
	for _, cl := range []*ast.Class{
		e.p.Builtins.NPE, e.p.Builtins.AIOOBE, e.p.Builtins.Arith, e.p.Builtins.CCE,
		e.p.Builtins.NegArr, e.p.Builtins.Assertion, e.p.Builtins.IllArg,
		e.p.Builtins.IllState, e.p.Builtins.NoSuchElem, e.p.Builtins.Unsup,
		e.p.Builtins.IllMon, e.p.Builtins.ArrayStore,
	} {
		if cl != nil {
			e.instantiate(cl)
		}
	}
	if e.p.Main != nil {
		e.needMethod(e.p.Main)
	}
	// A program whose own declarations carry a library annotation is read
	// through the annotation tables at run time, and this back end writes no
	// tables. The C back end decides the same thing with the same predicate.
	if ce := (&Emitter{prog: e.p}); ce.programUsesAnnotations() {
		e.refuse(noPos, "an annotation on the program's own declaration: the llvm back end does not write the annotation tables reflection reads")
	}
	// The work queue grows while it is drained: a method body names classes,
	// which bring their constructors, their static initializers and their
	// vtable's methods.
	for i := 0; i < len(e.mqueue); i++ {
		e.emitMethodBody(e.mqueue[i])
	}
	e.entry()
	e.emitClassTables()
	e.emitStatics()
	// The strings go last because the last of them is interned while a class
	// table is written: a trap stub names the method it stands in for, and that
	// name is a C string like any other.
	e.emitStrings()
}

// module assembles the text. The order matters only in that types precede their
// uses; a global may name one defined later, which is how a class table points
// at a class that has not been written yet.
func (e *llvmEmitter) module() string {
	e.emitTypes()
	var b strings.Builder
	b.WriteString("; generated by the Teyru compiler: the LLVM back end\n")
	b.WriteString("; the program itself is this module; only the runtime is C\n")
	b.WriteString("target triple = \"x86_64-unknown-linux-gnu\"\n\n")
	b.WriteString(e.types.String())
	b.WriteString(e.declares.String())
	b.WriteString(e.globals.String())
	b.WriteString(e.defs.String())
	return b.String()
}

// ---------------------------------------------------------------- needs

// needMethod queues a method's body for emission.
func (e *llvmEmitter) needMethod(m *ast.Method) {
	if m == nil || e.methods[m] {
		return
	}
	e.methods[m] = true
	e.mqueue = append(e.mqueue, m)
	// A method's own class comes with it. Its body may name the class's table
	// without naming the class anywhere else -- a static field read ends in
	// `ty_clinit(&cls_X)`, and X's table is only emitted if something asked for
	// the class -- and a descriptor that is referenced but not written is a
	// module clang rejects.
	e.needClass(m.Owner)
}

// needClass brings a class in: its table, its supertypes and its static
// initializer.
func (e *llvmEmitter) needClass(cl *ast.Class) {
	if cl == nil || e.classes[cl] {
		return
	}
	if cl.Kind == ast.KindAnnotation {
		// annotations need the tables reflection reads, which this back end
		// does not write; an enum or a record needs members, which it does
		e.refuse(noPos, "an annotation (%s): the llvm back end does not write the annotation tables its members are read from", cl.Full)
	}

	if cl.Inner || cl.OuterField != nil {
		// An inner class reaches its enclosing instance through a field the
		// compiler adds, and every unqualified name in it may have to walk that
		// chain. The offsets are known, but where the chain is entered is a
		// lowering of its own, and getting it wrong reads another object's
		// field -- so it is refused instead.
		e.refuse(noPos, "an inner class (%s): the llvm back end does not lower the enclosing-instance chain", cl.Full)
	}
	e.classes[cl] = true
	e.classOrder = append(e.classOrder, cl)
	if cl.Special == "array" {
		e.arrays = true
	}
	if cl.Super != nil {
		e.needClass(cl.Super.Class)
	}
	for _, i := range cl.Ifaces {
		e.needClass(i.Class)
	}
	if cl.ClInit != nil {
		e.needMethod(cl.ClInit)
	}
}

// instantiate marks a class as one that can have instances, which is what makes
// its vtable and the three Object-protocol slots the runtime calls by index
// reachable.
func (e *llvmEmitter) instantiate(cl *ast.Class) {
	if cl == nil {
		return
	}
	e.needClass(cl)
	if e.inst[cl] {
		return
	}
	if th := e.p.LookupClass("teyru.Thread"); th != nil && isSubclass(cl, th) {
		// A thread's run() is dispatched by the runtime's thread start, not by a
		// call site in the program: the slot it dispatches through is one no
		// call site names, and a slot this back end did not fill is a stub that
		// stops with a message. Threads are outside this back end's subset --
		// the C back end lowers them through ty_thread_start's selector -- so a
		// program that starts one is refused here rather than run into that stub.
		e.refuse(noPos, "a thread (%s): the llvm back end does not lower the runtime's thread start, which is what dispatches run()", cl.Full)
	}
	e.inst[cl] = true
	// Slots 0, 1 and 2 are toString, hashCode and equals. The collector does not
	// read the vtable, but print_uncaught, ty_obj_hash and ty_obj_equal do, on
	// whatever object they are handed, so these three answer for every class
	// that can have instances.
	for i := 0; i < 3 && i < len(cl.VTable); i++ {
		e.needMethod(cl.VTable[i])
	}
	// A call site seen before this class existed still has to reach it.
	for _, n := range e.vdisp {
		if n.idx < len(cl.VTable) && isSubclass(cl, n.owner) {
			e.needMethod(cl.VTable[n.idx])
		}
	}
	for _, n := range e.ifaceNeeds {
		if impl := e.p.Implements(cl, n.m); impl != nil {
			e.needMethod(impl)
		}
	}
}

// slot records a virtual call site, so that every class that can be the
// receiver answers it.
func (e *llvmEmitter) slot(owner *ast.Class, idx int) {
	e.vdisp = append(e.vdisp, vneed{owner, idx})
	for cl := range e.inst {
		if idx < len(cl.VTable) && isSubclass(cl, owner) {
			e.needMethod(cl.VTable[idx])
		}
	}
}

// ifaceNeed is one interface call site: the interface method, and the selector
// the dispatch uses.
type ifaceNeed struct {
	m   *ast.Method
	sel int
}

// ifaceSlot records an interface call site and answers with the implementation
// this class's interface map has to hold for it, or nil when the class does not
// answer the interface at all.
func (e *llvmEmitter) ifaceSlot(im *ast.Method, sel int) {
	e.ifaceNeeds = append(e.ifaceNeeds, ifaceNeed{im, sel})
	for cl := range e.inst {
		if impl := e.p.Implements(cl, im); impl != nil {
			e.needMethod(impl)
		}
	}
}

// isSubclass reports whether a is b or inherits from it, interfaces included.
// It is the walk the runtime's ty_class_is_sub makes, done at compile time so
// that a call site knows which classes have to answer it.
func isSubclass(a, b *ast.Class) bool {
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	if a.Super != nil && isSubclass(a.Super.Class, b) {
		return true
	}
	for _, i := range a.Ifaces {
		if isSubclass(i.Class, b) {
			return true
		}
	}
	return false
}

// markExn brings in the exception class a runtime helper throws.
func (e *llvmEmitter) markExn(global string, cl *ast.Class) {
	if cl == nil || e.exnUsed[global] == cl {
		return
	}
	e.exnUsed[global] = cl
	e.instantiate(cl)
}

// useArray brings in the one class every array value belongs to.
func (e *llvmEmitter) useArray() *ast.Class {
	cl := e.p.ArrayClass()
	if cl != nil {
		e.arrays = true
		e.needClass(cl)
		// The runtime builds every array, and it calls an array's own
		// toString/hashCode/equals through TY_ARRAY's vtable as freely as it
		// calls any object's: an array handed to println(Object) or compared
		// for identity goes through those slots.
		if !e.inst[cl] {
			e.instantiate(cl)
		}
	}
	return cl
}

// ---------------------------------------------------------------- refusals

// refuse stops emission with the diagnostic for a construct outside the subset.
func (e *llvmEmitter) refuse(pos source.Pos, format string, args ...any) {
	if pos.File == nil {
		pos = e.curPos
	}
	if pos.File == nil && e.fb != nil {
		pos = e.fb.pos
	}
	where := ""
	if e.fb != nil && e.fb.fn != nil {
		if e.fb.fn.Owner != nil {
			where = e.fb.fn.Owner.Full + "." + e.fb.fn.Name
		} else {
			where = e.fb.fn.Name
		}
	}
	panic(bail{&Refusal{Code: CodeLLVMUnsupported, Pos: pos, Thing: fmt.Sprintf(format, args...), Where: where}})
}

func kindName(k ast.ClassKind) string {
	switch k {
	case ast.KindEnum:
		return "enum"
	case ast.KindRecord:
		return "record"
	case ast.KindAnnotation:
		return "annotation"
	case ast.KindInterface:
		return "interface"
	}
	return "class"
}

// ---------------------------------------------------------------- types

// llvmType maps a Teyru type onto the LLVM type the runtime's ABI uses for it:
// a reference is a pointer whatever it points at, and the primitive widths are
// the widths of the C types the runtime's prototypes name.
func (e *llvmEmitter) llvmType(t ast.Type) string {
	switch v := e.p.Erased(t).(type) {
	case nil:
		return "void"
	case *ast.PrimType:
		switch v.Kind {
		case ast.Void:
			return "void"
		case ast.Boolean, ast.Int:
			return "i32"
		case ast.Byte:
			return "i8"
		case ast.Short, ast.Char:
			return "i16"
		case ast.Long:
			return "i64"
		case ast.Float:
			return "float"
		case ast.Double:
			return "double"
		}
		return "i32"
	}
	return "ptr"
}

func (e *llvmEmitter) isRef(t ast.Type) bool { return util.IsRef(e.p.Erased(t)) }

// stringType is the type of a String value, for the places a runtime helper's
// result is one.
func (e *llvmEmitter) stringType() ast.Type { return &ast.ClassType{Class: e.p.Builtins.String} }

// classType is the type of a value of a class, for the same reason.
func (e *llvmEmitter) classType(cl *ast.Class) ast.Type { return &ast.ClassType{Class: cl} }

// structType is the LLVM name of a class's instance layout. The special classes
// are the runtime's own types: a String *is* a tystr.
func (e *llvmEmitter) structType(cl *ast.Class) string {
	switch cl.Special {
	case "String":
		return "%tystr"
	case "sb":
		return "%tySB"
	}
	return "%C_" + util.Mangle(cl.Full)
}

// classSize is the size of an instance, computed the one way: with
// util.FieldOffsets, which is what the C back end allocates with and what the
// collector walks.
func (e *llvmEmitter) classSize(cl *ast.Class) int64 {
	if cl.Special == "sb" {
		// A builder's state is the runtime's own struct (tySB: the header, the
		// length, the capacity and the buffer), not a list of Teyru fields --
		// the class declares none, so the field walk would answer eight bytes
		// and the constructor would write its buffer pointer past the end of the
		// object.
		return rtStructSize(tySBLayout)
	}
	_, size := util.FieldLayout(cl.InstFields, e.capTypes(cl))
	return size
}

// rtStructSize is the size of one of the runtime's own structs, from the table
// that describes it: the end of its last field, rounded up the way the C
// compiler rounds a struct.
func rtStructSize(layout []rtField) int64 {
	cur := int64(0)
	for _, f := range layout {
		cur = util.Align(cur, sizeOfLLVM(f.ty))
		if f.off > cur {
			cur = f.off
		}
		cur += sizeOfLLVM(f.ty)
	}
	return util.Align(cur, 8)
}

// classStruct is the layout of a class's instances as an emitted struct type,
// and where each field sits in it.
//
// The struct type and the indices a field access uses come from this one walk,
// because they have to agree: LLVM pads a struct itself when the alignment
// already puts the next field where the layout says, and a padding entry the
// emitter counts but does not write -- or the other way round -- makes the
// eleventh field of a class index the twelfth slot, which clang reports as
// "invalid getelementptr indices" in a file the program's author never wrote.
func (e *llvmEmitter) classStruct(cl *ast.Class) (string, map[*ast.Field]int) {
	if idx, ok := e.classIdx[cl]; ok {
		return e.structText[cl], idx
	}
	layout := []rtField{{"obj", "ptr", 0}}
	caps := e.p.CapturedVars(cl)
	offsets, _ := util.FieldOffsets(cl.InstFields, e.capTypes(cl))
	for i, f := range cl.InstFields {
		// the field's position in the walk is its key: two fields of one class
		// cannot share a name, but a name is not what this walk is about
		layout = append(layout, rtField{fmt.Sprint(i), e.llvmType(f.Type), offsets[i]})
	}
	for i := range caps {
		fd := cl.CapFields[caps[i]]
		if fd == nil {
			continue
		}
		// the captures come after the instance fields, at the offsets
		// util.FieldLayout appended for them
		layout = append(layout, rtField{fmt.Sprintf("cap%d", i), e.llvmType(fd.Type),
			offsets[len(cl.InstFields)+i]})
	}
	decl, _, byKey := structDecl("C_"+util.Mangle(cl.Full), layout)
	idx := make(map[*ast.Field]int, len(cl.InstFields)+len(caps))
	for i, f := range cl.InstFields {
		idx[f] = byKey[fmt.Sprint(i)]
	}
	for i := range caps {
		if fd := cl.CapFields[caps[i]]; fd != nil {
			idx[fd] = byKey[fmt.Sprintf("cap%d", i)]
		}
	}
	if e.classIdx == nil {
		e.classIdx = map[*ast.Class]map[*ast.Field]int{}
		e.structText = map[*ast.Class]string{}
	}
	e.classIdx[cl] = idx
	e.structText[cl] = decl
	byName := map[string]int{}
	for i, f := range cl.InstFields {
		byName[f.Name] = byKey[fmt.Sprint(i)]
	}
	for i := range caps {
		fd := cl.CapFields[caps[i]]
		if fd == nil {
			continue
		}
		byName[fd.Name] = byKey[fmt.Sprintf("cap%d", i)]
	}
	e.capIdx[cl] = byName
	return decl, idx
}

// emitTypesDecl writes the struct a class's instances have, with explicit
// padding, so that the fields sit at the offsets the runtime and the collector
// were told about.
func (e *llvmEmitter) emitTypesDecl() {
	for _, cl := range e.classOrder {
		if cl.Special == "String" || cl.Special == "sb" {
			// the runtime owns these two layouts
			continue
		}
		decl, _ := e.classStruct(cl)
		e.types.WriteString(decl)
	}
}

// fieldStructIndex is where a field sits in the emitted struct type, from the
// same walk that wrote that type.
func (e *llvmEmitter) fieldStructIndex(cl *ast.Class, fd *ast.Field) (int, bool) {
	_, idx := e.classStruct(cl)
	if i, ok := idx[fd]; ok {
		return i, true
	}
	// A field reached by name may arrive as a field object the checker built for
	// that reference rather than the one the class holds -- a capture, or a
	// field of a generic class the checker substituted -- so the name is the
	// fallback: a name is what the body wrote, and one class cannot have two
	// fields of one name.
	if i, ok := e.capIdx[cl][fd.Name]; ok {
		return i, true
	}
	return 0, false
}

// ---------------------------------------------------------------- strings

// cstring interns a NUL-terminated C string, for the names the runtime reads.
func (e *llvmEmitter) cstring(s string) string {
	if id, ok := e.cstrs[s]; ok {
		return fmt.Sprintf("@.cs%d", id)
	}
	id := len(e.cstrOrder)
	e.cstrs[s] = id
	e.cstrOrder = append(e.cstrOrder, s)
	return fmt.Sprintf("@.cs%d", id)
}

// teyruString interns a Teyru string literal as a static tystr, the shape the C
// back end gives one: an object header naming teyru.String, the length, and a
// pointer to the bytes.
func (e *llvmEmitter) teyruString(s string) string {
	if id, ok := e.strs[s]; ok {
		return fmt.Sprintf("@S%d", id)
	}
	id := len(e.strOrder)
	e.strs[s] = id
	e.strOrder = append(e.strOrder, s)
	return fmt.Sprintf("@S%d", id)
}

func (e *llvmEmitter) emitStrings() {
	for i, s := range e.strOrder {
		// The literal carries a terminating NUL, exactly as the C back end's
		// string literal does (`static tystr S0 = {...}` points at "abc\0").
		// It is not a convenience: the runtime's string helpers read the
		// character after the last one -- ty_str_lastindexof_ch_from walks to
		// `data[len]` -- so a literal that stops at its length makes them read
		// whatever the linker put next, which is a wrong answer and, under
		// AddressSanitizer, a global-buffer-overflow.
		fmt.Fprintf(&e.globals, "@.sb%d = private unnamed_addr constant [%d x i8] c\"%s\", align 1\n",
			i, len(s)+1, llvmBytes(s, true))
		fmt.Fprintf(&e.globals, "@S%d = internal global %%tystr { ptr @cls_%s, i64 %d, ptr @.sb%d }, align 8\n",
			i, util.Mangle(e.p.Builtins.String.Full), len(s), i)
	}
	for i, s := range e.cstrOrder {
		fmt.Fprintf(&e.globals, "@.cs%d = private unnamed_addr constant [%d x i8] c\"%s\", align 1\n",
			i, len(s)+1, llvmBytes(s, true))
	}
}

// llvmBytes escapes a byte string for an LLVM c"..." literal.
func llvmBytes(s string, nul bool) string {
	var b strings.Builder
	write := func(c byte) {
		switch {
		case c == '"':
			b.WriteString("\\22")
		case c == '\\':
			b.WriteString("\\5C")
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\%02X", c)
		}
	}
	for i := 0; i < len(s); i++ {
		write(s[i])
	}
	if nul {
		write(0)
	}
	return b.String()
}

// ---------------------------------------------------------------- declarations

// decl records a runtime symbol's prototype once, at its first use. A runtime
// helper is declared with the types its header declares it with, because the
// runtime is compiled from that header and the two have to agree.
func (e *llvmEmitter) decl(name, ret string, params []string, attrs string) {
	if e.declared[name] {
		return
	}
	e.declared[name] = true
	fmt.Fprintf(&e.declares, "declare %s @%s(%s)%s\n", ret, name, strings.Join(params, ", "), attrs)
}

// declGlobal declares a global the runtime defines: a class handle, the
// thread's own state, the handler chain.
func (e *llvmEmitter) declGlobal(name, kind, ty, attrs string) {
	if e.declared[name] {
		return
	}
	e.declared[name] = true
	fmt.Fprintf(&e.declares, "@%s = external %sglobal %s%s\n", name, kind, ty, attrs)
}

// call emits a call to a runtime helper, declaring it on first use.
func (e *llvmEmitter) call(f *fb, name, ret string, params, args []string) string {
	e.decl(name, ret, params, "")
	return e.emitCall(f, name, ret, args)
}

func (e *llvmEmitter) emitCall(f *fb, name, ret string, args []string) string {
	if ret == "void" {
		f.ins(fmt.Sprintf("call void @%s(%s)", name, strings.Join(args, ", ")))
		return ""
	}
	r := f.reg()
	f.ins(fmt.Sprintf("%s = call %s @%s(%s)", r, ret, name, strings.Join(args, ", ")))
	return r
}

func (e *llvmEmitter) declMemSet() {
	if e.declared["llvm.memset.p0.i64"] {
		return
	}
	e.declared["llvm.memset.p0.i64"] = true
	e.declares.WriteString("declare void @llvm.memset.p0.i64(ptr, i8, i64, i1)\n")
}

// ---------------------------------------------------------------- symbol names

// methodSymbol is the name a generated method is defined under. It is the C back
// end's name, which matters for one thing only: a program's own native methods
// are declared with the symbol sema gave them, and a debugger looking at one
// build should see what it sees in the other.
func (e *llvmEmitter) methodSymbol(m *ast.Method) string {
	if m.External {
		return m.Native
	}
	if m.LLName == "" {
		m.LLName = fnName(m.Owner, m, methodIndex(m))
	}
	return m.LLName
}

// fnSig is the LLVM signature of a method: the receiver first for an instance
// method, then the parameters. A reference is a pointer, a primitive is the
// width the runtime's prototypes use.
func (e *llvmEmitter) fnSig(m *ast.Method) (string, []string) {
	ret := e.llvmType(m.Result)
	if m.IsCtor {
		ret = "void"
	}
	var params []string
	if !m.IsStatic() {
		params = append(params, "ptr")
	}
	for _, p := range m.Params {
		params = append(params, e.llvmType(p))
	}
	// A constructor of a class declared inside a method takes that method's
	// captured variables after the ones it declares: the object has to hold them
	// before any initializer of its own runs. They are the compiler's
	// parameters, so only the signature and the calls carry them -- a program's
	// own view of the constructor is the declared one, which is what `new`
	// matches.
	if m.IsCtor {
		for _, t := range e.capTypes(m.Owner) {
			params = append(params, e.llvmType(t))
		}
	}
	return ret, params
}

func (e *llvmEmitter) sigOf(ret string, params []string) string {
	if len(params) == 0 {
		return ret + " ()"
	}
	return ret + " (" + strings.Join(params, ", ") + ")"
}

// staticGlobal is the global a static field lives in.
func (e *llvmEmitter) staticGlobal(cl *ast.Class, f *ast.Field) string {
	return "@" + util.Mangle(staticName(cl, f))
}

// ---------------------------------------------------------------- method bodies

// emitMethodBody writes one method: its declaration if it is implemented
// outside the module, and its body otherwise.
func (e *llvmEmitter) emitMethodBody(m *ast.Method) {
	if m.External {
		ret, params := e.fnSig(m)
		e.decl(m.Native, ret, params, "")
		return
	}
	ret, params := e.fnSig(m)
	f := newFB(e, m, ret)
	e.fb = f
	names := []string{}
	if !m.IsStatic() {
		names = append(names, "%this")
	}
	for i := range m.Params {
		names = append(names, fmt.Sprintf("%%a%d", i))
	}
	if m.IsCtor {
		// the captures come after the declared parameters, so they need names in
		// the signature as well
		for i := range e.capTypes(m.Owner) {
			names = append(names, fmt.Sprintf("%%a%d", len(m.Params)+i))
		}
	}
	fmt.Fprintf(&e.defs, "define internal %s @%s(%s) {\n", ret, e.methodSymbol(m), joinParams(params, names))
	f.entry()
	if m.Lambda != nil {
		f.lam = m.Lambda
		f.bodyClass = e.enclosureOf(m.Lambda)
	}
	if m.Owner != nil && len(m.Owner.CapFields) > 0 {
		// a class declared inside a method reads its captures through `this`
		e.bindCaptures(f, m.Owner)
	}
	// The parameters are copied into stack slots, because a Teyru method may
	// assign to one and the value has to survive the assignment.
	for i, pv := range m.ParamVars {
		slot := f.localSlot(pv)
		e.store(f, slot, pv.Type, value(fmt.Sprintf("%%a%d", i), pv.Type))
	}
	e.emitBody(f, m)
	e.flushBody(f)
	e.fb = nil
}

// emitBody writes what a method's body is: its statements, the static
// initializer sema synthesized, a synthesized accessor, or a loud failure for a
// body nobody wrote.
func (e *llvmEmitter) emitBody(f *fb, m *ast.Method) {
	body := bodyOf(m)
	switch {
	case m.SynthKind == "lambda":
		e.lambdaBody(f, m, m.Lambda)
	case m.SynthKind == "lambda-ctor":
		// A closure is allocated and filled in at the site where the lambda is
		// written, not by a constructor, so this member is never called. It is
		// written as an empty body rather than left undefined, because a symbol
		// the module names has to exist.
		f.retVoid()
	case m.SynthKind == "clinit" || (m.Owner != nil && m.Owner.ClInit == m && body == nil):
		e.clinitBody(f, m.Owner)
	case m.IsCtor:
		e.ctorBody(f, m.Owner, m, body)
		e.finishBody(f)
	case body != nil:
		e.block(f, body)
		e.finishBody(f)
	case m.SynthKind != "" && e.synthBody(f, m, m.SynthKind):
		// the member is written above; a synthesized body returns for itself
	case m.Accessor != nil:
		e.accessorBody(f, m)
	case m.Mods.Has(ast.ModNative):
		e.nativeWrapper(f, m)
	case m.Mods.Has(ast.ModAbstract):
		// An abstract method something still called. The C back end fails at run
		// time here too (ty_unimplemented), and refusing to compile instead
		// would reject programs whose abstract methods are never reached.
		e.unimplemented(f, m.Owner.Full+"."+m.Name)
	default:
		e.refuse(noPos, "the synthesized member %s.%s: the llvm back end does not write this member", m.Owner.Full, m.Name)
	}
}

// finishBody ends a method that ran off the end of its body: a void method
// returns there, and a method with a result whose body can fall through is one
// the checker should not have accepted, so it is refused rather than emitted as
// a return of nothing.
func (e *llvmEmitter) finishBody(f *fb) {
	if f.term {
		return
	}
	if f.ret == "void" {
		f.retVoid()
		return
	}
	// The checker requires a result on every path, so a body that reaches its
	// end does so where the program says control cannot go -- the loop that
	// does not terminate above it. Saying so in the IR is what LLVM's
	// unreachable is for, and it is better than a return of a value nobody
	// wrote.
	f.unreachable()
}

// nativeWrapper writes the body of a native method of a built-in class: the
// runtime's helper is what implements it. A native with no binding has nothing
// behind it at all, and the C back end stops there too rather than returning
// whatever the register happened to hold.
func (e *llvmEmitter) nativeWrapper(f *fb, m *ast.Method) {
	if nativeKey(m) == forNameKey {
		// Class.forName searches a table this back end does not write
		e.refuse(noPos, "Class.forName: the llvm back end does not write the class table its search needs")
	}
	nf, ok := nativeTable[nativeKey(m)]
	if !ok {
		e.unimplemented(f, m.Owner.Full+"."+m.Name)
		return
	}
	vals := make([]lval, 0, len(m.Params))
	for i, p := range m.Params {
		vals = append(vals, value(fmt.Sprintf("%%a%d", i), p))
	}
	recv := "%this"
	res := e.nativeInvoke(f, m, nf, recv, vals, m.Pos)
	if m.Result == ast.TVoid {
		f.retVoid()
		return
	}
	e.retResult(f, res, m.Result)
}

// retResult returns a value from a method that answers with one.
func (e *llvmEmitter) retResult(f *fb, v lval, result ast.Type) {
	f.retVal(e.coerce(f, v, result).v)
}

// clinitBody writes a class's static initializer: the superclass's initializer
// first, then the static field initializers in declaration order, then the
// static init blocks.
func (e *llvmEmitter) clinitBody(f *fb, cl *ast.Class) {
	if cl.Super != nil {
		e.decl("ty_clinit", "void", []string{"ptr"}, "")
		f.ins(fmt.Sprintf("call void @ty_clinit(ptr @cls_%s)", util.Mangle(cl.Super.Class.Full)))
	}
	if cl.Decl != nil {
		for _, mem := range cl.Decl.Members {
			fd, ok := mem.(*ast.FieldDecl)
			if !ok {
				continue
			}
			for _, vd := range fd.Vars {
				fld := vd.Fld
				if fld == nil || !fld.Mods.Has(ast.ModStatic) || vd.Init == nil {
					continue
				}
				e.needClass(cl)
				ptr := e.staticGlobal(cl, fld)
				e.store(f, ptr, fld.Type, e.coerce(f, e.expr(f, vd.Init), fld.Type))
			}
		}
		for _, mem := range cl.Decl.Members {
			if ib, ok := mem.(*ast.InitBlock); ok && ib.Static {
				e.block(f, ib.Body)
			}
		}
	}
	e.enumInit(f, cl)
	// the initializers of synthesized fields
	for _, fld := range cl.Fields {
		if fld.InitExpr == nil || !fld.Mods.Has(ast.ModStatic) || (fld.Decl != nil && fld.Decl.Init != nil) {
			continue
		}
		e.store(f, e.staticGlobal(cl, fld), fld.Type, e.coerce(f, e.expr(f, fld.InitExpr), fld.Type))
	}
	f.retVoid()
}

// flushBody appends the finished function: its stack slots first, which is
// where they have to be for LLVM to promote them into registers.
func (e *llvmEmitter) flushBody(f *fb) {
	if !f.term {
		// A block without a terminator does not parse. Nothing should fall off
		// the end of a function this back end writes -- every path ends in a
		// return -- so this is the loud version of an internal mistake rather
		// than a return the program did not write.
		name := "main"
		if f.fn != nil {
			name = e.methodSymbol(f.fn)
		}
		e.refuse(noPos, "an internal lowerer error: %s ends with a block that has no terminator", name)
	}
	e.defs.WriteString(f.prolog.String())
	e.defs.WriteString(f.head.String())
	e.defs.WriteString(f.body.String())
	e.defs.WriteString("}\n\n")
}

// unimplemented emits the call the C back end emits for a method that has no
// implementation: it stops with the method's name rather than returning a value
// nobody wrote.
func (e *llvmEmitter) unimplemented(f *fb, what string) {
	e.decl("ty_unimplemented", "void", []string{"ptr"}, " noreturn")
	f.ins(fmt.Sprintf("call void @ty_unimplemented(ptr %s)", e.cstring(what)))
	f.unreachable()
}

func joinParams(types, names []string) string {
	parts := make([]string, 0, len(types))
	for i := range types {
		parts = append(parts, fmt.Sprintf("%s %s", types[i], names[i]))
	}
	return strings.Join(parts, ", ")
}

// alignOf is the natural alignment of a value of a type: the layout gives every
// primitive its own size, and a reference eight bytes.
func alignOf(t ast.Type, e *llvmEmitter) int64 {
	if _, ok := e.p.Erased(t).(*ast.PrimType); ok {
		return util.AlignOf(t)
	}
	return util.SizeRef
}

// ---------------------------------------------------------------- class tables

// emitClassTables writes, for every class the program named, the tyclass the
// runtime and the collector read.
func (e *llvmEmitter) emitClassTables() {
	for _, cl := range e.classOrder {
		e.emitVtable(cl)
		e.emitRefOffsets(cl)
		e.emitIfaces(cl)
		e.emitName(cl)
	}
	for _, cl := range e.classOrder {
		e.emitClassRecord(cl)
	}
	e.emitPrimClassRecords()
}

// needPrimClass brings in the table behind a primitive class literal: a name,
// the modifiers Java reports, and the primitive kind, so that `int.class`
// answers "int" and is not equal to `Integer.class`. Nothing walks it, and no
// value is ever an instance of it, so it has no vtable and no members.
func (e *llvmEmitter) needPrimClass(k ast.PrimKind) string {
	e.primClasses[k] = true
	name := primClassName(k)
	if !e.declared["prim."+name] {
		e.declared["prim."+name] = true
		fmt.Fprintf(&e.globals, "@.cn%s = private unnamed_addr constant [%d x i8] c\"%s\\00\", align 1\n",
			name, len((&ast.PrimType{Kind: k}).String())+1, (&ast.PrimType{Kind: k}).String())
	}
	return "@cls_" + name
}

// emitPrimClassRecords writes the tables for the primitive classes a program
// named. They are written after the program's own because a literal may be
// reached while any of them is being emitted.
func (e *llvmEmitter) emitPrimClassRecords() {
	var kinds []ast.PrimKind
	for k := range e.primClasses {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		name := primClassName(k)
		prim := int64(k)
		if k == ast.Void {
			prim = 0
		}
		vals := map[string]string{
			"name":    "ptr @.cn" + name,
			"id":      fmt.Sprintf("i32 %d", -1-int(k)),
			"flags":   "i32 0",
			"mods":    fmt.Sprintf("i32 %d", jPrimClassMods),
			"prim":    fmt.Sprintf("i32 %d", prim),
			"nref":    "i32 0",
			"refoffs": "ptr null",
		}
		fmt.Fprintf(&e.globals, "@cls_%s = internal global %%tyclass %s, align 8\n",
			name, recordInit(e.tyclassEntries, vals))
	}
}

func (e *llvmEmitter) emitName(cl *ast.Class) {
	fmt.Fprintf(&e.globals, "@.cn%s = private unnamed_addr constant [%d x i8] c\"%s\", align 1\n",
		util.Mangle(cl.Full), len(cl.Full)+1, llvmBytes(cl.Full, true))
}

// classFlags is the runtime's flag word: the class is an interface, the class
// every array value belongs to, or a wrapper.
func classFlags(cl *ast.Class) int64 {
	var flags int64
	if cl.IsInterface() {
		flags |= 1
	}
	if cl.Special == "array" {
		flags |= 2
	}
	if cl.Special == "box" {
		flags |= 4
	}
	return flags
}

// emitRefOffsets writes the offsets of the reference fields, which is what the
// collector walks. A missing offset frees a live object and an extra one
// dereferences garbage, so it comes from util.FieldLayout, the one place both
// back ends learn the layout from.
func (e *llvmEmitter) emitRefOffsets(cl *ast.Class) {
	refs, _ := util.FieldLayout(cl.InstFields, e.capTypes(cl))
	if len(refs) == 0 {
		// The C back end still writes a one-element array holding 0, and the
		// same shape here means a reader of one build can read the other's.
		fmt.Fprintf(&e.globals, "@refs_%s = internal global [1 x i32] [i32 0], align 4\n", util.Mangle(cl.Full))
		return
	}
	parts := make([]string, len(refs))
	for i, o := range refs {
		parts[i] = fmt.Sprintf("i32 %d", o)
	}
	fmt.Fprintf(&e.globals, "@refs_%s = internal global [%d x i32] [%s], align 4\n",
		util.Mangle(cl.Full), len(refs), strings.Join(parts, ", "))
}

func (e *llvmEmitter) emitIfaces(cl *ast.Class) {
	if len(cl.Ifaces) == 0 {
		fmt.Fprintf(&e.globals, "@if_%s = internal global [1 x ptr] [ptr null], align 8\n", util.Mangle(cl.Full))
		return
	}
	parts := make([]string, len(cl.Ifaces))
	for i, f := range cl.Ifaces {
		parts[i] = "ptr @cls_" + util.Mangle(f.Class.Full)
	}
	fmt.Fprintf(&e.globals, "@if_%s = internal global [%d x ptr] [%s], align 8\n",
		util.Mangle(cl.Full), len(parts), strings.Join(parts, ", "))
}

// emitVtable writes the dispatch table of a class that can have instances.
//
// A slot holds the method this program reaches through it. The rest hold a trap
// that names the method and stops: a slot is reached only by a call site, every
// call site is in a method that was compiled, and each one recorded the slot it
// dispatches through -- so a trap is a slot no call can reach, and it is a trap
// rather than null so that being wrong about that is a message and not a jump
// through a null pointer.
//
// A class that can have no instances carries no table: nothing dispatches
// through it, because dispatch is on an object.
func (e *llvmEmitter) emitVtable(cl *ast.Class) {
	if !e.inst[cl] || len(cl.VTable) == 0 {
		fmt.Fprintf(&e.globals, "@vt_%s = internal global [1 x ptr] [ptr null], align 8\n", util.Mangle(cl.Full))
		return
	}
	parts := make([]string, len(cl.VTable))
	for i, m := range cl.VTable {
		if e.methods[m] {
			parts[i] = "ptr @" + e.methodSymbol(m)
			continue
		}
		parts[i] = "ptr @" + e.trap(m)
	}
	fmt.Fprintf(&e.globals, "@vt_%s = internal global [%d x ptr] [%s], align 8\n",
		util.Mangle(cl.Full), len(parts), strings.Join(parts, ", "))
}

// trap names the stub that stands in for a method no call site reaches.
//
// One stub per method, not per signature: a stub serves a vtable slot, and the
// only thing a stub is for is to say which method is missing when something
// reaches it. Sharing stubs between methods with the same signature made the
// message name whichever method happened to ask first, which sent a reader to
// the wrong method -- the message has to be about the slot it stands in.
func (e *llvmEmitter) trap(m *ast.Method) string {
	ret, params := e.fnSig(m)
	sig := e.sigOf(ret, params) + "|" + m.Owner.Full + "." + m.Name
	if n, ok := e.trapNames[sig]; ok {
		return n
	}
	n := fmt.Sprintf("trap%d", len(e.trapOrder))
	e.trapNames[sig] = n
	e.trapOrder = append(e.trapOrder, n)
	e.defs.WriteString(e.trapStub(n, m, ret, params))
	return n
}

func (e *llvmEmitter) trapStub(name string, m *ast.Method, ret string, params []string) string {
	var b strings.Builder
	names := make([]string, len(params))
	for i := range params {
		names[i] = fmt.Sprintf("%%p%d", i)
	}
	fmt.Fprintf(&b, "define internal %s @%s(%s) {\n", ret, name, joinParams(params, names))
	b.WriteString("entry:\n")
	e.decl("ty_unimplemented", "void", []string{"ptr"}, " noreturn")
	fmt.Fprintf(&b, "  call void @ty_unimplemented(ptr %s)\n", e.cstring(m.Owner.Full+"."+m.Name+" (llvm vtable trap)"))
	b.WriteString("  unreachable\n}\n\n")
	return b.String()
}

// emitClassRecord writes the tyclass itself.
func (e *llvmEmitter) emitClassRecord(cl *ast.Class) {
	n := util.Mangle(cl.Full)
	size := e.classSize(cl)
	refs, _ := util.FieldLayout(cl.InstFields, e.capTypes(cl))
	super := "ptr null"
	if cl.Super != nil {
		super = "ptr @cls_" + util.Mangle(cl.Super.Class.Full)
	}
	clinit := "ptr null"
	if cl.ClInit != nil && e.methods[cl.ClInit] {
		clinit = "ptr @" + e.methodSymbol(cl.ClInit)
	}
	vtable, nvt := "ptr null", 0
	if e.inst[cl] && len(cl.VTable) > 0 {
		vtable, nvt = "ptr @vt_"+n, len(cl.VTable)
	}
	imap, nsel := e.emitImap(cl)
	// The reflection tables are not written: this back end refuses
	// java.lang.reflect, and a table nothing can reach is dead weight in every
	// executable.
	vals := map[string]string{
		"name":     "ptr @.cn" + n,
		"id":       fmt.Sprintf("i32 %d", cl.ID),
		"flags":    fmt.Sprintf("i32 %d", classFlags(cl)),
		"super":    super,
		"niface":   fmt.Sprintf("i32 %d", len(cl.Ifaces)),
		"ifaces":   "ptr @if_" + n,
		"nvt":      fmt.Sprintf("i32 %d", nvt),
		"vtable":   vtable,
		"clinit":   clinit,
		"isize":    fmt.Sprintf("i32 %d", size),
		"isel":     fmt.Sprintf("i32 %d", nsel),
		"imap":     imap,
		"nsub":     "i32 0",
		"subs":     "ptr null",
		"nref":     fmt.Sprintf("i32 %d", len(refs)),
		"refoffs":  "ptr @refs_" + n,
		"mods":     fmt.Sprintf("i32 %d", (&Emitter{prog: e.p}).classMods(cl)),
		"prim":     "i32 0",
		"fields":   "ptr null",
		"nfields":  "i32 0",
		"methods":  "ptr null",
		"nmethods": "i32 0",
		"consts":   "ptr null",
		"nconsts":  "i32 0",
		"annos":    "ptr null",
		"nannos":   "i32 0",
	}
	fmt.Fprintf(&e.globals, "@cls_%s = internal global %%tyclass %s, align 8\n",
		n, recordInit(e.tyclassEntries, vals))
}

// emitImap writes a class's interface map: one entry per selector the class
// answers for, sorted by selector, which is what the runtime's lookup scans. It
// is the C back end's table -- the same walk over the interfaces the class
// implements -- because a class has to answer the same calls in either build.
func (e *llvmEmitter) emitImap(cl *ast.Class) (string, int) {
	var sels []int
	impls := map[int]string{}
	for _, iface := range e.p.AllInterfaces(cl) {
		for _, im := range e.p.InterfaceMethods(iface) {
			if im.Selector < 0 || im.Selector >= e.p.Selectors {
				continue
			}
			impl := e.p.Implements(cl, im)
			if impl == nil {
				continue
			}
			if _, seen := impls[im.Selector]; !seen {
				sels = append(sels, im.Selector)
			}
			// An implementation no call site reaches is not written; the trap
			// it stands in for names the method if something reaches it anyway.
			slot := "ptr @" + e.trap(impl)
			if e.methods[impl] {
				slot = "ptr @" + e.methodSymbol(impl)
			}
			impls[im.Selector] = slot
		}
	}
	if len(sels) == 0 {
		return "ptr null", 0
	}
	sort.Ints(sels)
	parts := make([]string, len(sels))
	for i, s := range sels {
		// an array element is written with its type: LLVM wants one there and
		// there is no surrounding value to give it one
		parts[i] = fmt.Sprintf("%%tymap { i32 %d, %s }", s, impls[s])
	}
	fmt.Fprintf(&e.globals, "@imap_%s = internal global [%d x %%tymap] [%s], align 8\n",
		util.Mangle(cl.Full), len(parts), strings.Join(parts, ", "))
	return "ptr @imap_" + util.Mangle(cl.Full), len(sels)
}

// emitStatics writes the global holding each static field.
func (e *llvmEmitter) emitStatics() {
	for _, cl := range e.classOrder {
		for _, f := range cl.Fields {
			if !f.Mods.Has(ast.ModStatic) {
				continue
			}
			fmt.Fprintf(&e.globals, "%s = internal global %s zeroinitializer, align %d\n",
				e.staticGlobal(cl, f), e.llvmType(f.Type), alignOf(f.Type, e))
		}
	}
}

// sortedClasses is the class list in a stable order, for the startup sequence
// and anything else that has to be written the same way twice.
func (e *llvmEmitter) sortedClasses() []*ast.Class {
	out := append([]*ast.Class(nil), e.classOrder...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Full < out[j].Full })
	return out
}

// boxClassOf answers with the wrapper class of a primitive kind, when the
// program has one.
func (e *llvmEmitter) boxClassOf(k ast.PrimKind) *ast.Class {
	if c, ok := e.p.Builtins.Boxes[k]; ok {
		return c
	}
	return nil
}

// ---------------------------------------------------------------- entry

// entry writes main: the runtime comes up, the class handles the runtime reads
// through globals are installed, and the program's own main runs.
func (e *llvmEmitter) entry() {
	f := newFB(e, nil, "i32")
	e.fb = f
	e.decl("ty_init", "void", nil, "")
	f.def("define i32 @main(i32 %argc, ptr %argv) {")
	f.entry()
	f.ins("call void @ty_init()")
	// The collector reads a static reference through the address of the global
	// that holds it, so every reference-typed static is registered by address.
	for _, cl := range e.classOrder {
		for _, fd := range cl.Fields {
			if !fd.Mods.Has(ast.ModStatic) || !e.isRef(fd.Type) {
				continue
			}
			e.decl("ty_gc_register_static", "void", []string{"ptr"}, "")
			f.ins(fmt.Sprintf("call void @ty_gc_register_static(ptr %s)", e.staticGlobal(cl, fd)))
		}
	}
	e.installGlobal(f, "TY_STRING", e.p.Builtins.String)
	e.installGlobal(f, "TY_OBJECT", e.p.Builtins.Object)
	if e.arrays {
		e.installGlobal(f, "TY_ARRAY", e.p.ArrayClass())
	}
	for _, mn := range []struct {
		global string
		cl     *ast.Class
	}{
		{"TY_NPE", e.p.Builtins.NPE}, {"TY_AIOOBE", e.p.Builtins.AIOOBE},
		{"TY_ARITH", e.p.Builtins.Arith}, {"TY_CCE", e.p.Builtins.CCE},
		{"TY_NEGARR", e.p.Builtins.NegArr}, {"TY_ASSERT", e.p.Builtins.Assertion},
		{"TY_ILLARG", e.p.Builtins.IllArg}, {"TY_ILLSTATE", e.p.Builtins.IllState},
		{"TY_NOSUCHELEM", e.p.Builtins.NoSuchElem}, {"TY_UNSUP", e.p.Builtins.Unsup},
		{"TY_ILLMON", e.p.Builtins.IllMon}, {"TY_ARRAYSTORE", e.p.Builtins.ArrayStore},
	} {
		e.installGlobal(f, mn.global, mn.cl)
	}
	var kinds []ast.PrimKind
	for k := range e.boxUsed {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		cl := e.boxClassOf(k)
		if cl == nil {
			continue
		}
		e.declGlobal("TY_BOX", "", "[9 x ptr]", ", align 8")
		slot := f.reg()
		f.ins(fmt.Sprintf("%s = getelementptr inbounds [9 x ptr], ptr @TY_BOX, i64 0, i64 %d", slot, int(k)))
		f.ins(fmt.Sprintf("store ptr @cls_%s, ptr %s", util.Mangle(cl.Full), slot))
	}
	if obj := e.p.Builtins.Object; obj != nil && e.classes[obj] {
		e.decl("ty_clinit", "void", []string{"ptr"}, "")
		f.ins(fmt.Sprintf("call void @ty_clinit(ptr @cls_%s)", util.Mangle(obj.Full)))
	}
	// Only the classes that have an initializer are named here: ty_clinit on a
	// class whose whole hierarchy has none is a no-op, and naming it takes the
	// class's address for nothing.
	seen := map[string]bool{}
	for _, cl := range e.sortedClasses() {
		if seen[util.Mangle(cl.Full)] || !needsClinit(cl, map[*ast.Class]bool{}) {
			continue
		}
		seen[util.Mangle(cl.Full)] = true
		e.decl("ty_clinit", "void", []string{"ptr"}, "")
		f.ins(fmt.Sprintf("call void @ty_clinit(ptr @cls_%s)", util.Mangle(cl.Full)))
	}
	if main := e.p.Main; main != nil {
		// main is defined by this module like any other method, so it is queued
		// rather than declared: a declare and a define of one internal function
		// is a redefinition.
		e.needMethod(main)
		var vals []string
		if !main.IsStatic() {
			obj := e.allocInstance(f, main.Owner)
			f.ins(fmt.Sprintf("store ptr @cls_%s, ptr %s", util.Mangle(main.Owner.Full), obj))
			vals = append(vals, argOperand("ptr", obj))
		}
		if len(main.Params) == 1 {
			e.useArray()
			vals = append(vals, argOperand("ptr", e.entryArgs(f)))
		}
		e.emitCall(f, e.methodSymbol(main), "void", vals)
	}
	// The process ends with its last thread: a program that starts one and
	// returns must not lose what that thread printed.
	e.decl("ty_thread_join_all", "void", nil, "")
	f.ins("call void @ty_thread_join_all()")
	f.retVal("0")
	e.flushBody(f)
	e.fb = nil
}

// installGlobal stores a class into one of the runtime's class handles.
func (e *llvmEmitter) installGlobal(f *fb, global string, cl *ast.Class) {
	if cl == nil || !e.classes[cl] {
		return
	}
	e.declGlobal(global, "", "ptr", ", align 8")
	f.ins(fmt.Sprintf("store ptr @cls_%s, ptr @%s", util.Mangle(cl.Full), global))
}

// entryArgs builds the String[] a main that takes one is handed, out of argv.
// The array carries no element class, exactly as the C back end's does: the
// runtime built it, and a runtime-built array promises nothing about its
// elements.
func (e *llvmEmitter) entryArgs(f *fb) string {
	e.useArray()
	n32 := f.reg()
	f.ins(fmt.Sprintf("%s = sub i32 %%argc, 1", n32))
	pos := f.reg()
	f.ins(fmt.Sprintf("%s = icmp sgt i32 %s, 0", pos, n32))
	nc := f.reg()
	f.ins(fmt.Sprintf("%s = select i1 %s, i32 %s, i32 0", nc, pos, n32))
	n := f.reg()
	f.ins(fmt.Sprintf("%s = sext i32 %s to i64", n, nc))
	args := e.rtCall(f, "ty_array_new", e.refType(), []lval{
		value(n, ast.TLong), value(fmt.Sprint(util.SizeRef), ast.TLong)})
	f.ins(fmt.Sprintf("store i32 1, ptr %s", e.gepBytes(f, args.v, 28)))
	i := f.tempSlot(ast.TLong, "argi")
	f.ins(fmt.Sprintf("store i64 1, ptr %s", i))
	cond := f.nextLabel("args_cond")
	body := f.nextLabel("args_body")
	end := f.nextLabel("args_end")
	f.br(cond)
	f.label(cond)
	argc := f.reg()
	f.ins(fmt.Sprintf("%s = sext i32 %%argc to i64", argc))
	cur := e.loadRaw(f, "i64", i, 8)
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp slt i64 %s, %s", c, cur, argc))
	f.cbr(c, body, end)
	f.label(body)
	argp := e.gepIndex(f, "ptr", "%argv", cur)
	raw := e.loadRaw(f, "ptr", argp, 8)
	str := e.rtCall(f, "ty_str_intern", e.refType(), []lval{value(raw, e.refType())})
	zero := f.reg()
	f.ins(fmt.Sprintf("%s = sub i64 %s, 1", zero, cur))
	slot := e.elemAddrOfDyn(f, args.v, zero, util.SizeRef)
	f.ins(fmt.Sprintf("store ptr %s, ptr %s", str.v, slot))
	next := f.reg()
	f.ins(fmt.Sprintf("%s = add i64 %s, 1", next, cur))
	f.ins(fmt.Sprintf("store i64 %s, ptr %s", next, i))
	f.br(cond)
	f.label(end)
	return args.v
}

// ---------------------------------------------------------------- function builder

// fb writes one function: its stack slots, its blocks and its instructions.
type fb struct {
	e   *llvmEmitter
	fn  *ast.Method
	ret string

	head   strings.Builder
	body   strings.Builder
	prolog strings.Builder

	cur  string // the block being written
	term bool   // it already has its terminator
	n    int

	// pos is the statement being lowered, for the refusals that have no
	// position of their own.
	pos source.Pos

	locals map[*ast.Var]string
	// slots are the function's own stack slots. Once a try statement has opened
	// a frame in this function, every access to one of them is volatile: a
	// setjmp's second return can arrive with the memory of the frame but not
	// with whatever the optimiser kept in a register, and C's own rule for
	// locals live across setjmp is exactly this. Without it a local assigned
	// inside a try and read in its catch reads the value it had at the setjmp
	// -- which is what a wrong answer at -O1 and above looked like.
	slots    map[string]bool
	volatile bool
	// lam is the lambda whose body is being written, when it is one: inside such
	// a body `this` is the instance the lambda was created in, which is what
	// selfOperand reads. bodyClass is the class the body was written in, which
	// is where a bare name resolves -- not the closure class.
	lam       *ast.Lambda
	bodyClass *ast.Class

	swType        map[string]ast.Type
	retT          ast.Type
	loops         []loopFrame
	fins          []handlerFrame
	switches      []switchCtx
	pendingLabels []string
	labels        map[string]int
}

// loopFrame is one loop or switch body being written: where its break and
// continue go, and how deep the loop stack was when it was entered, which is
// what tells a finally action whether a jump leaves it.
type loopFrame struct {
	cont     string
	brk      string
	depth    int
	isSwitch bool
	labels   []string
}

// handlerFrame is a finally action an abrupt exit still has to run: a try with a
// finally keeps its tycatch on the handler chain, so leaving it has to take the
// frame off as well.
type handlerFrame struct {
	emit  func()
	depth int
	frame string // the tycatch slot, so the chain can be restored
}

func newFB(e *llvmEmitter, m *ast.Method, ret string) *fb {
	f := &fb{
		e:      e,
		fn:     m,
		ret:    ret,
		locals: map[*ast.Var]string{},
		slots:  map[string]bool{},
		swType: map[string]ast.Type{},
		labels: map[string]int{},
	}
	if m != nil {
		f.retT = m.Result
	}
	return f
}

func (f *fb) def(s string) { f.prolog.WriteString(s + "\n") }
func (f *fb) entry()       { f.cur = "entry"; f.prolog.WriteString("entry:\n") }

func (f *fb) reg() string {
	f.n++
	return fmt.Sprintf("%%t%d", f.n)
}

func (f *fb) ins(s string) {
	if f.term {
		// A statement that cannot be reached -- the tail of a block whose
		// return already ran -- still has to be written into a block, or the
		// module does not parse. LLVM drops what nothing branches to.
		f.newBlock()
	}
	fmt.Fprintf(&f.body, "  %s\n", s)
}

// newBlock opens a fresh block after a terminated one, for the dead code a
// statement sequence may still hold.
func (f *fb) newBlock() {
	f.n++
	l := fmt.Sprintf("dead%d", f.n)
	f.body.WriteString(l + ":\n")
	f.cur = l
	f.term = false
}

func (f *fb) label(l string) {
	if f.cur == "" {
		f.entry()
	}
	if !f.term {
		f.ins("br label %" + l)
	}
	f.body.WriteString(l + ":\n")
	f.cur = l
	f.term = false
}

// nextLabel answers with a label that is not used yet in this function.
func (f *fb) nextLabel(prefix string) string {
	n := f.labels[prefix]
	f.labels[prefix] = n + 1
	return fmt.Sprintf("%s%d", prefix, n)
}

func (f *fb) br(l string) { f.ins("br label %" + l); f.term = true }

// unreachable ends a block nothing branches to: the instructions after a call
// that never returns.
func (f *fb) unreachable()    { f.ins("unreachable"); f.term = true }
func (f *fb) retVoid()        { f.ins("ret void"); f.term = true }
func (f *fb) retVal(v string) { f.ins(fmt.Sprintf("ret %s %s", f.ret, v)); f.term = true }

func (f *fb) cbr(cond, yes, no string) {
	f.ins(fmt.Sprintf("br i1 %s, label %%%s, label %%%s", cond, yes, no))
	f.term = true
}

// localSlot is the stack slot of a local variable or parameter. One slot per
// variable, in the entry block, so that LLVM can promote it into registers.
func (f *fb) localSlot(v *ast.Var) string {
	if s, ok := f.locals[v]; ok {
		return s
	}
	f.n++
	slot := fmt.Sprintf("%%v%d_%s", v.ID, util.Mangle(v.Name))
	fmt.Fprintf(&f.head, "  %s = alloca %s, align %d\n", slot, f.e.llvmType(v.Type), alignOf(v.Type, f.e))
	f.locals[v] = slot
	f.slots[slot] = true
	return slot
}

// tempSlot is a stack slot for a value that has to survive a jump: the result of
// a switch expression, the loop counter of a colon loop, and the value a
// `return` computes before its finally actions run.
func (f *fb) tempSlot(t ast.Type, name string) string {
	f.n++
	slot := fmt.Sprintf("%%s%d_%s", f.n, util.Mangle(name))
	f.swType[slot] = t
	fmt.Fprintf(&f.head, "  %s = alloca %s, align %d\n", slot, f.e.llvmType(t), alignOf(t, f.e))
	f.slots[slot] = true
	return slot
}

// rawSlot is a stack slot of an LLVM type no Teyru type maps to, such as the
// catch frame.
func (f *fb) rawSlot(ty, name string, align int64) string {
	f.n++
	slot := fmt.Sprintf("%%r%d_%s", f.n, util.Mangle(name))
	fmt.Fprintf(&f.head, "  %s = alloca %s, align %d\n", slot, ty, align)
	return slot
}

func (f *fb) pushLoop(fr loopFrame) {
	fr.depth = len(f.loops)
	f.loops = append(f.loops, fr)
}

func (f *fb) popLoop() { f.loops = f.loops[:len(f.loops)-1] }

// breakFrame is the loop or switch a break leaves: the one a label names, or the
// innermost.
func (f *fb) breakFrame(label string) (loopFrame, bool) {
	for i := len(f.loops) - 1; i >= 0; i-- {
		if label == "" {
			return f.loops[i], true
		}
		for _, l := range f.loops[i].labels {
			if l == label {
				return f.loops[i], true
			}
		}
	}
	return loopFrame{}, false
}

// continueFrame is the loop a continue goes on with, skipping the switch case
// bodies between it and the statement.
func (f *fb) continueFrame(label string) (loopFrame, bool) {
	for i := len(f.loops) - 1; i >= 0; i-- {
		if f.loops[i].isSwitch {
			continue
		}
		if label == "" {
			return f.loops[i], true
		}
		for _, l := range f.loops[i].labels {
			if l == label {
				return f.loops[i], true
			}
		}
	}
	return loopFrame{}, false
}

func (f *fb) takeLabels() []string {
	l := f.pendingLabels
	f.pendingLabels = nil
	return l
}

func (f *fb) brkLabel(l string) string { return "brk_" + util.Mangle(l) }

func (f *fb) pushFin(fr handlerFrame) { f.fins = append(f.fins, fr) }
func (f *fb) popFin()                 { f.fins = f.fins[:len(f.fins)-1] }

func (f *fb) zero(t ast.Type) lval { return value(zeroOperand(f.e.llvmType(t)), t) }

// zeroValue is the zero of a Teyru type as an operand: `null` for a reference,
// because a pointer constant spelled `0` is not one -- `icmp eq ptr 0, null`
// does not parse -- and the number otherwise.
func (e *llvmEmitter) zeroValue(t ast.Type) lval {
	ty := e.llvmType(t)
	if ty == "ptr" {
		return value("null", t)
	}
	return value(zeroOperand(ty), t)
}

// zeroOperand is the zero value of an LLVM type, as an operand.
func zeroOperand(ty string) string {
	switch ty {
	case "i8", "i16", "i32", "i64":
		return "0"
	case "float", "double":
		return "0.0e+00"
	}
	return "null"
}

// lval is a lowered expression: an operand of a Teyru type. The operand may be
// an SSA register or a constant, which is what LLVM accepts wherever a value
// goes.
type lval struct {
	v string
	t ast.Type
}

func value(v string, t ast.Type) lval { return lval{v: v, t: t} }

// emitTypes writes the type declarations into the module's type section.
func (e *llvmEmitter) emitTypes() {
	for _, st := range []struct {
		name   string
		layout []rtField
	}{
		{"tyclass", tyclassLayout},
		{"tystr", tystrLayout},
		{"tyarr", tyarrLayout},
		{"tySB", tySBLayout},
		{"tymap", tymapLayout},
		{"tycatch", tycatchLayout},
		{"tythread", tythreadLayout},
	} {
		decl, _, _ := structDecl(st.name, st.layout)
		e.types.WriteString(decl)
	}

	e.emitTypesDecl()
}
