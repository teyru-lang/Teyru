// Package codegen lowers a checked Teyru program to portable C.
//
// The generated C is compiled to a native executable (optionally through LLVM)
// with no JVM involved: classes become C structs, virtual dispatch becomes a
// vtable, garbage collection is a conservative mark-and-sweep runtime.
package codegen

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/util"
)

// unit is the five sections of one C translation unit.
//
// The emitter writes into one of these at a time. A build of one translation
// unit (Emit) uses a single unit for everything; a build that compiles the
// standard library once and reuses it (EmitLibrary and EmitProgram) uses one
// per translation unit, and this is what makes that possible without every
// emission site knowing which unit it is writing into.
type unit struct {
	types strings.Builder // typedefs and struct declarations
	fns   strings.Builder // forward declarations
	data  strings.Builder // globals: strings, class metadata
	meta  strings.Builder // member tables, shipped only if reflection is used
	code  strings.Builder // function bodies
}

// Emitter produces C translation units for a whole program.
type Emitter struct {
	prog *sema.Program
	// The sections being written right now. They point into one of the units
	// below; the emitter switches them per class, which is what routes a
	// prelude class into the library unit and a program class into the
	// program's, without every emission site asking which is which.
	types *strings.Builder
	fns   *strings.Builder
	code  *strings.Builder
	data  *strings.Builder
	meta  *strings.Builder
	// own is the unit this emitter is filling, and other the one it is not.
	// In a single-unit build both are nil and the sections point at the
	// emitter's own storage.
	own      *unit
	other    *unit
	scratch  *unit    // where a class the unit does not own is computed into
	link     string   // "static " or "", so a unit can be a library
	decls    string   // the storage class of a declaration: see declLink
	split    bool     // whether this is the split (library + program) build
	sideLib  bool     // whether the class being emitted belongs to the prelude
	lib      bool     // whether this emitter is the library's unit
	metaInit []string // the assignments that attach them at startup
	libMeta  []string // the same, for the prelude classes, in the library unit
	// vts collects the vtable arrays of a split build (see emitClassMeta), and
	// forName the class table Class.forName searches (see forNameTable). Both
	// belong to the program's unit whatever class they name, so they are
	// written aside and appended to it.
	vts     *strings.Builder
	forName *strings.Builder
	// headerDecls are the declarations the program's unit needs that the
	// library's own sections do not carry: the string literals it defines
	// (see strLit) and the hook its Class.forName calls.
	headerDecls string
	// defined is the string literals this emitter has already named, litDefs
	// their definitions by name, and litPrelude which of them a standard library
	// body uses -- those are the library's to define (see strLit).
	defined     map[string]bool
	litDefs     map[string]string
	litPrelude  map[string]bool
	libLiterals []string
	reflectUsed bool // whether any call site asks for reflection
	fnTry       bool // whether the function being emitted contains a try statement
	strings     map[string]int
	strOrder    []string
	mainCls     *ast.Class
	tmp         int
	indent      int
	thrown      []string
	locals      map[*ast.Var]string
	enumOrdinal string
	patternVars map[*ast.InstanceOf]string
	// matches recorded by primitive type patterns, which test the value
	patternOK map[*ast.InstanceOf]string
	curClass  *ast.Class
	retType   ast.Type // declared result type of the method being emitted
	curLambda *ast.Lambda
	switchID  int
	// static type of the selector of the switch being emitted: a primitive type
	// pattern in a case asks about a box only when the selector is a reference
	switchSel ast.Type
	switchCur int
	// labels attached to the statement being emitted right now; a loop
	// consumes them and turns them into its continue and break targets
	pendingLabels []string
	// continue target of each enclosing loop; a basic `for` is lowered to a
	// while with its update at the end of the body, so an unlabelled
	// continue must jump over the rest of the body to reach that update
	loops []string
	// finally blocks of the try statements being emitted, innermost last; an
	// abrupt exit has to run them before it leaves
	finallys []finFrame
	// nesting depth of each labelled loop, for a labelled break or continue
	labelDepth map[string]int
	// objects that can live on the C stack in the method being emitted
	stackLocals map[*ast.Var]*ast.New
	// what is known about each method's receiver
	leakCache map[*ast.Method]leakState
	// primitive types named by a class literal, whose class objects the emitter
	// synthesizes; filled in while the function bodies are emitted and written
	// out with the rest of the metadata, before them
	primClasses map[ast.PrimKind]bool
	// frameMap is set while the body of a function whose frame the collector has
	// a map for is being emitted: it turns on the per-block kill stores (frames.go)
	// and the frame prologue emitMappedBody writes around the body.
	frameMap bool
	// useNow collects the variables the statement being emitted mentions, which
	// is what says where a reference's last use is. See emitBlockInner.
	useNow map[*ast.Var]bool
	// frameTemps are the frame-level words this function's body puts a value in
	// flight into -- one per expression that creates an object where another
	// expression may run before it is consumed. They are declared in the frame
	// prologue and named by its map. See frameTemp.
	frameTemps []string
	// frameBase is how many words of the frame's map come before them: one per
	// reference-typed parameter.
	frameBase int
	// frameUsed are the indices into frameTemps that the statement being emitted
	// has written a value into. Only those are given back when that statement
	// ends: a word that already holds NULL is back, and writing NULL into it once
	// per later statement is a store the program does not need (measured on a
	// reflection-heavy program: 98,489 of the registrations in the emitted C were
	// those repeats, 3.0 MB of the 6.1 MB the maps add). frameUsedStack holds the
	// enclosing statements' lists, because statements nest (see frameRelease).
	frameUsed      []int
	frameUsedStack [][]int
}

// frameRelease gives back the frame words for values in flight that the current
// statement wrote, and forgets them.
//
// A statement's end is where a value in flight stops being in flight: what the
// expression answered with has been read out of its word by then. A write that is
// never given back is a leak, not corruption -- the word stays a root, so the
// collector keeps what it last pointed at -- and
// TestEveryWrittenFrameWordIsReleased in frames_test.go is the check that no write
// is missed: it reads the emitted C and requires a release for every write.
// frameScope emits a piece of code that is not a statement of its own -- a
// constructor's field initializer is the one the emitter writes -- and gives back
// the frame words it wrote, the way a statement's end does.
func (e *Emitter) frameScope(f func()) {
	e.frameUsedStack = append(e.frameUsedStack, e.frameUsed)
	e.frameUsed = nil
	f()
	e.frameRelease()
	if n := len(e.frameUsedStack); n > 0 {
		e.frameUsed = e.frameUsedStack[n-1]
		e.frameUsedStack = e.frameUsedStack[:n-1]
	}
}

func (e *Emitter) frameRelease() {
	for _, k := range e.frameUsed {
		e.line("%s(&%s, %d, NULL);\n", framePut, frameVar, e.frameBase+k)
	}
	e.frameUsed = e.frameUsed[:0]
}

// finFrame is one try statement whose finally action must still run.
type finFrame struct {
	emit  func() // writes the finally action
	depth int    // len(loops) when the try was entered
	name  string // C name of the frame's exception handler
}

// Link is what a build has to link for a program besides the runtime every
// program links.
//
// It exists because the runtime has one part that is not self-contained: the
// TLS layer is written against OpenSSL, a library this compiler does not ship,
// and a program that cannot reach it must not be linked against it -- the point
// of the native runtime is a program that needs nothing but the C library.
type Link struct {
	// TLS is set when the program's reachable code can call one of the TLS
	// helpers. It is then that a build compiles internal/runtime/src/tyrt_tls.c
	// and passes -lssl -lcrypto, and that a target without OpenSSL refuses the
	// program by name (see internal/driver).
	TLS bool
}

// Emit returns the C source for a program, and what linking it takes.
//
// This is the whole program in one translation unit: the standard library, the
// program's own classes and the entry point, with vtable pruning and the
// link-time optimisation that follows deciding what of it the executable
// actually needs. A split build (EmitLibrary and EmitProgram) trades that
// whole-program view for a standard library compiled once and shared.
func Emit(p *sema.Program) (string, Link) {
	e := newEmitter(p, &unit{}, false)
	e.runFull()
	var out strings.Builder
	out.WriteString(cHeader)
	out.WriteString(e.unitText(e.own, true))
	// The last step is the one piece of reachability this back end decides for
	// itself: a vtable holds the address of every method its class declares, and
	// an address is what link-time optimisation cannot drop (see prune.go).
	if p0 := os.Getenv("TEYRU_DUMP_C"); p0 != "" {
		os.WriteFile(p0, []byte(out.String()), 0644)
	}
	src, tls := pruneVtables(out.String())
	return src, Link{TLS: tls}
}

// Library is the half of a split build that every program of a given prelude,
// target and optimisation level shares: the standard library's declarations,
// its translation unit, and the literals it defines.
type Library struct {
	// Header declares everything the library's unit defines, for the program's
	// unit to include. It is written with external linkage: the two units are
	// compiled separately, so nothing the program may name can be file-local.
	Header string
	// Source is the library's own translation unit.
	Source string
	// Literals are the string literals the library defines, by the name the
	// program's unit refers to them by. The program's unit defines the literals
	// that are not here, which is what keeps one literal one object across the
	// two units: `a == "abc"` compares references, and two units that each
	// defined their own "abc" would make that comparison answer false.
	Literals []string
}

// LibraryOptions are what the prelude's translation unit depends on besides
// the prelude itself.
type LibraryOptions struct {
	// Reflect is whether the program can reach reflection: a program that can
	// needs the standard library's member tables, which are the bulk of a
	// reflecting build, and one that cannot must not pay for them.
	Reflect bool
}

// PreludeHeader is the name the library's header and unit are given.
const preludeHeader = "typrelude.h"

// EmitLibrary emits the standard library's own translation unit. The program p
// must be a program whose files are all standard library files (the driver
// checks the prelude alone; see internal/driver).
//
// What comes out depends on the prelude, the options, and nothing else: an
// executable's behaviour does not depend on the program's own classes except
// through the declarations the program's unit makes, and the point of this
// function is that the same bytes come out for every program that shares a
// prelude. That is what makes caching it sound, so it is a property to keep:
// nothing here may read anything the program contributed.
func EmitLibrary(p *sema.Program, o LibraryOptions) Library {
	e := newEmitter(p, &unit{}, true)
	e.lib = true
	e.reflectUsed = o.Reflect
	e.runFull()
	sort.Strings(e.libLiterals)
	return Library{
		Header:   cHeader + e.headerText(),
		Source:   "/* generated by the Teyru compiler: the standard library */\n#include \"" + preludeHeader + "\"\n\n" + e.unitText(e.own, false),
		Literals: e.libLiterals,
	}
}

// Prune decides, for the two units of a split build, which vtable slots stay
// filled and whether the program can reach the TLS layer. It returns the
// program's unit rewritten; the library's is returned unchanged by design.
//
// It is a separate call because the answer needs both units and the library may
// come from a cache: the program's unit is emitted first (it is what says
// whether the library must carry its member tables), and the scan runs once the
// program's own reachability is the question being asked.
func Prune(library, program string) (string, Link) {
	src, tls := pruneVtablesWith(library, program)
	return src, Link{TLS: tls}
}

// Program is a program's own half of a split build: its translation unit, and
// what the standard library's unit has to be built with for it.
type Program struct {
	Source string
	// Reflect is whether the program can reach reflection. A program that can
	// needs the standard library's member tables, which are the bulk of a
	// reflecting build, and one that cannot must not carry them.
	Reflect bool
	Link    Link
	// PreludeLiterals are the string literals the standard library's bodies use,
	// and so the ones its unit defines (see strLit). The program's unit refers
	// to them and defines the rest, which is what keeps one literal one object
	// across the two units.
	PreludeLiterals []string
}

// EmitProgram emits a program's own translation unit: the program's classes, the
// vtables of every class, the class table Class.forName searches, and the entry
// point. It is compiled against a library emitted by EmitLibrary for the same
// prelude.
//
// The unit includes the library's header, so everything the standard library
// defines is visible to it, and the standard library's own definitions are in
// the library's unit rather than here.
func EmitProgram(p *sema.Program) Program {
	e := newEmitter(p, &unit{}, true)
	e.vts = &strings.Builder{}
	e.forName = &strings.Builder{}
	e.runFull()
	var out strings.Builder
	out.WriteString("/* generated by the Teyru compiler: the program */\n#include \"" + preludeHeader + "\"\n\n")
	out.WriteString(e.unitText(e.own, true))
	sort.Strings(e.libLiterals)
	return Program{Source: out.String(), Reflect: e.reflectUsed, Link: e.linkForProgram(), PreludeLiterals: e.libLiterals}
}

// linkForProgram answers what linking the program needs, which is the question
// pruneVtables answers for a build of one unit. A split build has to read both
// units to answer it, so the driver calls Prune with the library's text once it
// has one; this is only the answer that needs no scan.
func (e *Emitter) linkForProgram() Link { return Link{} }

// cHeader is what every generated translation unit starts with.
const cHeader = "/* generated by the Teyru compiler */\n#include \"tyrt.h\"\n#include <math.h>\n#include <string.h>\n\n"

// newEmitter returns an emitter filling the unit u.
func newEmitter(p *sema.Program, u *unit, split bool) *Emitter {
	e := &Emitter{
		prog:        p,
		own:         u,
		split:       split,
		strings:     map[string]int{},
		defined:     map[string]bool{},
		litDefs:     map[string]string{},
		litPrelude:  map[string]bool{},
		locals:      map[*ast.Var]string{},
		leakCache:   map[*ast.Method]leakState{},
		primClasses: map[ast.PrimKind]bool{},
	}
	if split {
		e.other = &unit{}
		e.scratch = &unit{}
	} else {
		e.link = "static "
	}
	e.enter(nil)
	return e
}

// enter points the emitter's sections at the unit that owns cl. In a split
// build the unit that does not own a class writes it into a scratch unit
// instead: emitting a class is also what records where its static fields are
// registered and which member tables it is handed at startup, and those have
// to happen in the unit that owns the class either way.
func (e *Emitter) enter(cl *ast.Class) {
	if !e.split {
		e.types, e.fns, e.data, e.meta, e.code = &e.own.types, &e.own.fns, &e.own.data, &e.own.meta, &e.own.code
		e.link = "static "
		e.sideLib = false
		return
	}
	e.enterOwned(cl != nil && cl.Prelude())
}

// enterPrelude points the emitter at whatever unit owns the standard library.
func (e *Emitter) enterPrelude() {
	if !e.split {
		e.enter(nil)
		return
	}
	e.enterOwned(true)
}

// enterOwned points the sections at the unit that owns a prelude or program
// piece of the output.
func (e *Emitter) enterOwned(prelude bool) {
	e.sideLib = prelude
	dest := e.own
	if prelude != e.lib {
		dest = e.scratch
	}
	e.types, e.fns, e.data, e.meta, e.code = &dest.types, &dest.fns, &dest.data, &dest.meta, &dest.code
	// A library is compiled separately from the program that links it, so
	// nothing it defines may be file-local: every method, table and class
	// object of the standard library is a symbol the program's unit may name.
	e.link = ""
}

// unitText assembles one unit. The member tables are written last, and only
// for a program that asks for them: they are the one part of a class's metadata
// that names other classes -- a field's type, a method's parameters -- so an
// array at file scope holding them would be a root, and it would pin every
// class they mention, and through those the whole standard library, into every
// executable, whether or not the program can reflect. A program that never asks
// pays nothing; the emitter's call sites are what decide. skipEntry is for the
// library's unit, which has no entry point of its own.
func (e *Emitter) unitText(u *unit, withEntry bool) string {
	var out strings.Builder
	if withDecls := u != e.own || !e.lib; withDecls {
		// The library's declarations are the header's, which both units
		// include; a program's are its own.
		out.WriteString(u.types.String())
		out.WriteString(u.fns.String())
	}
	out.WriteString(u.data.String())
	out.WriteString(e.literalDefs(u == e.own && e.lib))
	if e.reflectUsed {
		out.WriteString(u.meta.String())
	}
	out.WriteString(u.code.String())
	if withEntry {
		out.WriteString(e.entry())
	}
	return out.String()
}

// headerText is the library's header: the declarations both units need. The
// two are compiled apart, so what the other unit may name is declared here and
// defined there once.
func (e *Emitter) headerText() string {
	var out strings.Builder
	out.WriteString(e.own.types.String())
	out.WriteString(e.own.fns.String())
	out.WriteString(e.headerDecls)
	return out.String()
}

// declLink is the storage class of a declaration. One translation unit means
// every class object may be file-local; two mean the declarations are the only
// thing holding them together, and a declaration without `extern` is a
// definition in each unit that includes the header.
func (e *Emitter) declLink() string {
	if e.split {
		return "extern "
	}
	return "static "
}

// vtDst is where a vtable array is written: beside the class it belongs to in a
// build of one translation unit, and in the program's own unit in a split one
// (see emitClassMeta).
func (e *Emitter) vtDst() *strings.Builder {
	if e.vts != nil {
		return e.vts
	}
	return e.data
}

// LinkForIR is Link for the module the LLVM back end wrote. That back end
// resolves its own reachability -- a module holds the functions the program can
// run and nothing else -- so what is written there is what is linked, and a
// helper named in it is a helper the link needs.
func LinkForIR(ir string) Link {
	return Link{TLS: strings.Contains(ir, tlsPrefix)}
}

// runFull emits the program into the emitter's unit, routing each class to the
// unit that owns it (see enter).
func (e *Emitter) runFull() {
	for _, cl := range e.prog.Classes {
		e.enter(cl)
		e.declareClass(cl)
	}
	// A class's object is named by the tables of other classes -- a field's
	// type, a method's return type, the interface list of a subclass -- so
	// every one of them is declared before any of them is defined. Two
	// declarations of one static object, the first without an initializer,
	// are one definition.
	for _, cl := range e.prog.Classes {
		if cl == nil {
			continue
		}
		e.enter(cl)
		// The special classes are included: only their struct is a typedef of
		// a runtime type, their class object is emitted like any other's.
		fmt.Fprintf(e.types, "%styclass cls_%s;\n", e.declLink(), mangle(cl.Full))
	}
	// The primitive classes are the standard library's: a class literal for a
	// primitive is compiled into a class object like any other, and one
	// program's `int.class` is the same object as another's.
	e.enterPrelude()
	for k := ast.Void; k <= ast.Double; k++ {
		fmt.Fprintf(e.types, "%styclass cls_%s;\n", e.declLink(), primClassName(k))
	}
	for _, cl := range e.prog.Classes {
		e.enter(cl)
		e.emitClassMeta(cl)
	}
	for _, cl := range e.prog.Classes {
		e.enter(cl)
		e.emitClassCode(cl)
	}
	e.enterPrelude()
	// the metadata of the classes a class literal synthesized, now that the
	// bodies that may name them have been emitted
	e.emitPrimClassMeta()
	// A program that annotates its own classes needs the metadata whether or not
	// a call site names the reflection API: the frameworks read their
	// annotations through it.
	if e.programUsesAnnotations() {
		e.reflectUsed = true
	}
	if e.split {
		e.emitLibraryInstall()
		if !e.lib {
			// Before the entry point, not after it: the reachability scan reads
			// every line after main as part of main's body, and a table placed
			// there would make every class it names reachable from the start.
			if e.vts != nil {
				e.own.code.WriteString(e.vts.String())
			}
			if e.forName != nil {
				e.own.code.WriteString(e.forName.String())
			}
		}
	}
}

// emitLibraryInstall writes the function that wires the standard library into a
// split build's startup: the collector roots its static fields hold, and the
// member tables its classes are handed. The program's entry point calls it,
// which is what makes the library's unit independent of the program it is
// linked into: nothing here names anything the program contributed.
func (e *Emitter) emitLibraryInstall() {
	if !e.lib {
		return
	}
	var b strings.Builder
	b.WriteString("\nvoid ty_prelude_install(void) {\n")
	b.WriteString(e.clinitRefs(true))
	if e.reflectUsed {
		for _, line := range e.libMeta {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("}\n")
	e.own.code.WriteString(b.String())
	e.headerDecls += "\nvoid ty_prelude_install(void);\n"
}

// primClassName is the C name of the class a primitive class literal names.
func primClassName(k ast.PrimKind) string {
	return mangle("teyru.prim." + (&ast.PrimType{Kind: k}).String())
}

// emitPrimClassMeta writes the class behind a primitive class literal, such as
// `int.class`. A primitive is not a class in Teyru: no value is ever an
// instance of one, since an int boxes to teyru.Integer, and the program has no
// metadata for it. The literal still has to answer with what javac answers --
// "int", "boolean", "void" -- so the class is synthesized here rather than
// reusing the wrapper class, which would answer "teyru.Integer" and compare
// equal to Integer.class, where Java keeps the two apart.
//
// The struct carries only a name: nothing walks it. Its id is negative, since
// it is not one of the program's classes (that counter starts at zero and the
// runtime never reads the field), and it has no superclass or members, as a
// primitive type has none.
func (e *Emitter) emitPrimClassMeta() {
	// All nine are written whether or not the program names one, because
	// reflection can reach them without a literal: a field of type int reports
	// int.class, and Array.get asks the element's class which box to build.
	// The cost is a hundred bytes of .data each.
	for k := ast.Void; k <= ast.Double; k++ {
		kind := 0
		if k != ast.Void {
			kind = int(k)
		}
		fmt.Fprintf(e.data, "%styclass cls_%s = {\n  .name = %q, .id = %d, .mods = %d, .prim = %d,\n};\n", e.link,
			primClassName(k), (&ast.PrimType{Kind: k}).String(), -1-int(k), jPrimClassMods, kind)
	}
}

// ---------------------------------------------------------------- naming

// cname is the C type name of a class.
func cname(cl *ast.Class) string {
	return "C_" + util.Mangle(cl.Full)
}

// mangle is the shared identifier mangler.
func mangle(s string) string { return util.Mangle(s) }

func fnName(cl *ast.Class, m *ast.Method, idx int) string {
	owner := ""
	if cl != nil {
		owner = mangle(cl.Full)
	}
	name := m.Name
	if m.IsCtor {
		// "<init>" mangles to "_init_", which cannot collide with a user method
		name = "<init>"
	}
	if m.Accessor != nil && m.Prop != nil {
		if m.Accessor.IsSet {
			name = "set_" + m.Prop.Name
		} else {
			name = "get_" + m.Prop.Name
		}
	}
	return "M_" + owner + "_" + mangle(name) + "_" + fmt.Sprint(idx)
}

// methodIndex finds the position of m among the same-named methods of its owner.
func methodIndex(m *ast.Method) int {
	if m.Owner == nil {
		return 0
	}
	for i, x := range m.Owner.Methods[m.Name] {
		if x == m {
			return i
		}
	}
	for i, x := range m.Owner.Ctors {
		if x == m {
			return i
		}
	}
	return 0
}

func (e *Emitter) cfunc(m *ast.Method) string {
	if m.External {
		// implemented outside the generated program
		return m.Native
	}
	if m.LLName == "" {
		m.LLName = fnName(m.Owner, m, methodIndex(m))
	}
	return m.LLName
}

// ---------------------------------------------------------------- C types

func (e *Emitter) ctype(t ast.Type) string {
	switch v := t.(type) {
	case nil:
		return "void"
	case *ast.PrimType:
		switch v.Kind {
		case ast.Void:
			return "void"
		case ast.Boolean:
			return "int32_t"
		case ast.Byte:
			return "int8_t"
		case ast.Short:
			return "int16_t"
		case ast.Char:
			return "uint16_t"
		case ast.Int:
			return "int32_t"
		case ast.Long:
			return "int64_t"
		case ast.Float:
			return "float"
		case ast.Double:
			return "double"
		}
		return "int32_t"
	case *ast.ClassType:
		return cname(v.Class) + "*"
	case *ast.ArrayType:
		return "tyarr*"
	case *ast.TypeVarType:
		return e.ctype(e.prog.Erased(v))
	case *ast.WildcardType:
		return "void*"
	case ast.NullType:
		return "void*"
	case ast.ErrorType:
		return "void*"
	}
	return "void*"
}

func (e *Emitter) isRef(t ast.Type) bool { return util.IsRef(t) }

// zero renders the zero value of a C type.
func zeroOf(c string) string {
	switch c {
	case "int32_t", "int16_t", "uint16_t", "int8_t", "int64_t", "float", "double":
		return "0"
	}
	return "NULL"
}

// elemSize returns the byte size of an array element type as a C constant.
func (e *Emitter) elemSize(t ast.Type) string {
	return fmt.Sprint(util.SizeOf(t))
}

func (e *Emitter) isRefElem(t ast.Type) bool { return e.isRef(t) }

// ---------------------------------------------------------------- class decls

func (e *Emitter) declareClass(cl *ast.Class) {
	// the special classes are typedefs of runtime types, and they must be
	// visible before any struct that has a field of that type
	switch cl.Special {
	case "String", "sb":
		e.types.WriteString(e.structOf(cl))
		return
	}
	fmt.Fprintf(e.types, "typedef struct %s %s;\n", cname(cl), cname(cl))
}

func (e *Emitter) structOf(cl *ast.Class) string {
	switch cl.Special {
	case "String":
		return "typedef tystr " + cname(cl) + ";\n"
	case "sb":
		return "typedef tySB " + cname(cl) + ";\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "struct %s {\n  tyobj obj;\n", cname(cl))
	for _, f := range cl.InstFields {
		fmt.Fprintf(&b, "  %s f_%s;\n", e.ctype(f.Type), mangle(f.Name))
	}
	for _, v := range e.prog.CapturedVars(cl) {
		fmt.Fprintf(&b, "  %s cap_%s;\n", e.ctype(v.Type), mangle(v.Name))
	}
	b.WriteString("};\n")
	// A primitive wrapper is an ordinary class with one field holding its value
	// (lib/04_boxing.teyru), and its struct is written from that field list like
	// any other. The runtime keeps a second name for the same bytes -- tyintbox,
	// tylongbox, and so on -- because the reflection path opens a box through a
	// raw pointer, and a field read that lands one byte off reads another
	// object rather than failing. So the two views of one layout are tied
	// together here, where neither can drift without the build saying so.
	if cl.Special == "box" && len(cl.InstFields) == 1 {
		if rt := boxStruct(cl); rt != "" {
			fmt.Fprintf(&b, "_Static_assert(sizeof(%s) == sizeof(%s), \"%s is the size of the runtime's %s\");\n",
				cname(cl), rt, cl.Full, rt)
			fmt.Fprintf(&b, "_Static_assert(offsetof(%s, f_%s) == offsetof(%s, v), \"%s keeps its value where the runtime's %s does\");\n",
				cname(cl), mangle(cl.InstFields[0].Name), rt, cl.Full, rt)
		}
	}
	return b.String()
}

func (e *Emitter) emitClassMeta(cl *ast.Class) {
	if cl.Special != "String" && cl.Special != "sb" {
		// special classes were declared with their typedef in the first pass
		e.types.WriteString(e.structOf(cl))
	}
	e.emitStaticFields(cl)
	isIface := cl.IsInterface()
	flags := 0
	if isIface {
		flags |= 1
	}
	if cl.Special == "array" {
		// The class every array value belongs to is marked as one, which is
		// what Class.isArray and the reflective array accessors read.
		flags |= 2
	}
	if cl.Special == "box" {
		flags |= 4
	}
	sup := "NULL"
	if cl.Super != nil {
		sup = "&cls_" + mangle(cl.Super.Class.Full)
	}
	// vtable
	var vt []string
	for _, m := range cl.VTable {
		vt = append(vt, "(void*)"+e.cfunc(m))
	}
	if len(vt) == 0 {
		vt = []string{"NULL"}
	}
	// The vtable arrays are the program's own whatever class they belong to,
	// in a split build: prune.go decides which slots stay filled from the
	// program's dispatch sites -- a program that never dispatches to a method
	// is a program that does not need its address -- and a standard library
	// unit cached once could not carry a decision only the program can make.
	if e.split && e.lib {
		// The library's class records name the vtables, which the program's
		// unit defines; only the declaration is the library's.
		e.headerDecls += fmt.Sprintf("extern void* vt_%s[];\n", mangle(cl.Full))
	} else {
		if e.split {
			// A program's own class needs its vtable declared before the record
			// that names it, as it is in a build of one unit.
			fmt.Fprintf(e.types, "%svoid* vt_%s[];\n", e.link, mangle(cl.Full))
		}
		fmt.Fprintf(e.vtDst(), "%svoid* vt_%s[%d] = {%s};\n", e.link, mangle(cl.Full), len(vt), strings.Join(vt, ", "))
	}
	// interface table
	//
	// Sparse, sorted by selector, and holding only what the class actually
	// implements. It used to be one slot per selector the whole program
	// declares -- one pointer per slot -- so a class paid 4.4 KB of .data no
	// matter how many of them it answered for. Every class the program
	// mentions carries one of these, so a hello world pinned 180 of them,
	// 792 KB, for methods it could not reach. ty_itab scans the few entries
	// a class really has instead of indexing into the whole set.
	sel := e.prog.Selectors
	sels := make([]int, 0, len(cl.Ifaces))
	impls := map[int]string{}
	for _, iface := range e.prog.AllInterfaces(cl) {
		for _, m := range e.prog.InterfaceMethods(iface) {
			if m.Selector < 0 || m.Selector >= sel {
				continue
			}
			if impl := e.prog.Implements(cl, m); impl != nil {
				// later interfaces overwrite earlier ones, as they did when the
				// entry was written into a slot the whole table shared
				if _, seen := impls[m.Selector]; !seen {
					sels = append(sels, m.Selector)
				}
				impls[m.Selector] = "(void*)" + e.cfunc(impl)
			}
		}
	}
	sort.Ints(sels)
	imapName, imapLen := "NULL", 0
	if len(sels) > 0 {
		parts := make([]string, len(sels))
		for i, s := range sels {
			parts[i] = fmt.Sprintf("{%d, %s}", s, impls[s])
		}
		imapName, imapLen = "imap_"+mangle(cl.Full), len(sels)
		fmt.Fprintf(e.data, "%stymap %s[%d] = {%s};\n", e.link, imapName, imapLen, strings.Join(parts, ", "))
	}
	// ifaces list
	var ifs []string
	for _, i := range cl.Ifaces {
		ifs = append(ifs, "&cls_"+mangle(i.Class.Full))
	}
	if len(ifs) == 0 {
		ifs = []string{"NULL"}
	}
	fmt.Fprintf(e.data, "%styclass* if_%s[] = {%s};\n", e.link, mangle(cl.Full), strings.Join(ifs, ", "))
	// reference field offsets, following the C struct layout with alignment
	var capTypes []ast.Type
	for _, v := range e.prog.CapturedVars(cl) {
		capTypes = append(capTypes, v.Type)
	}
	refOffsets, isize := util.FieldLayout(cl.InstFields, capTypes)
	offs := make([]string, 0, len(refOffsets))
	for _, o := range refOffsets {
		offs = append(offs, fmt.Sprint(o))
	}
	off := isize
	if len(offs) == 0 {
		offs = []string{"0"}
	}
	// The offsets above are computed here, by util.FieldLayout; the layout they
	// describe is computed by the C compiler, from the struct written a few
	// hundred lines up. Two calculations, one layout, and a disagreement between
	// them is not a wrong answer a test would catch -- the collector walks these
	// offsets and would mark the wrong word, which frees a live object. So the
	// two are tied together where neither can drift without the build saying so,
	// the same way the primitive wrappers' layout is tied to the runtime's
	// struct. It costs nothing at run time: the assert is compiled away.
	// The two classes that are typedefs of runtime types have no struct here;
	// their layout is the runtime's, and the collector's offsets for them come
	// from the same place the runtime's own code gets them.
	if cl.Special != "String" && cl.Special != "sb" {
		i := 0
		for _, f := range cl.InstFields {
			if !ast.IsRef(f.Type) {
				continue
			}
			if i >= len(refOffsets) {
				break
			}
			fmt.Fprintf(e.data, "_Static_assert(offsetof(struct %s, f_%s) == %d, \"%s.%s is not where the collector looks for it\");\n",
				cname(cl), mangle(f.Name), refOffsets[i], cl.Full, f.Name)
			i++
		}
		for _, v := range e.prog.CapturedVars(cl) {
			if !ast.IsRef(v.Type) {
				continue
			}
			if i >= len(refOffsets) {
				break
			}
			fmt.Fprintf(e.data, "_Static_assert(offsetof(struct %s, cap_%s) == %d, \"%s captures %s somewhere else than the collector looks\");\n",
				cname(cl), mangle(v.Name), refOffsets[i], cl.Full, v.Name)
			i++
		}
		fmt.Fprintf(e.data, "_Static_assert(sizeof(struct %s) == %d, \"%s is not the size the allocator gives it\");\n",
			cname(cl), isize, cl.Full)
	}
	fmt.Fprintf(e.data, "%sint32_t refs_%s[] = {%s};\n", e.link, mangle(cl.Full), strings.Join(offs, ", "))
	clinit := "NULL"
	if cl.ClInit != nil {
		clinit = "(void*)" + e.cfunc(cl.ClInit)
	}
	// The reflection tables, which is what java.lang.reflect reads. They are
	// written here, with the class, because a reader of the generated C should
	// find a class's metadata in one place.
	// The class's own annotations go to the startup-attached group with the
	// member tables: an annotation names its type, so naming one in the class
	// object would pin that type's object into every program.
	annsTbl, nannos := e.emitAnnoTable(mangle(cl.Full), annoListOf(cl))
	if annsTbl != "NULL" {
		e.attach(cl, "annos", annsTbl, "tyannotation", nannos, "nannos")
	}
	fieldsTbl := e.emitFieldTable(cl)
	methodsTbl := e.emitMethodTable(cl)
	constsTbl := e.emitConstTable(cl)
	flagsExpr := fmt.Sprint(flags)
	if cl.Decl != nil && cl.Decl.Kind == ast.KindRecord {
		flagsExpr = fmt.Sprintf("(%d | TY_CLS_RECORD)", flags)
	}
	// Written with designators rather than in order: the struct grows as
	// reflection learns more about a class, and position would put a new fact
	// in an old field the first time someone forgets to update this line.
	fmt.Fprintf(e.data, "%styclass cls_%s = {\n", e.link, mangle(cl.Full))
	fmt.Fprintf(e.data, "  .name = %q, .id = %d, .flags = %s,\n", cl.Full, cl.ID, flagsExpr)
	// The name the class reports where a program is shown one: for a
	// standard-library class, the JDK class it stands in for (decision D8), and
	// nothing at all when the two names are the same -- the runtime reads a
	// null as "named as it is" (ty_class_jname).
	if j := util.JavaName(cl.Full); j != cl.Full {
		fmt.Fprintf(e.data, "  .jname = %q,\n", j)
	}
	fmt.Fprintf(e.data, "  .super = %s, .niface = %d, .ifaces = if_%s,\n", sup, len(cl.Ifaces), mangle(cl.Full))
	fmt.Fprintf(e.data, "  .nvt = %d, .vtable = vt_%s, .clinit = %s,\n", len(cl.VTable), mangle(cl.Full), clinit)
	fmt.Fprintf(e.data, "  .isize = %d, .isel = %d, .imap = %s,\n", int(off), imapLen, imapName)
	fmt.Fprintf(e.data, "  .nref = %d, .refoffs = refs_%s,\n", len(offs), mangle(cl.Full))
	fmt.Fprintf(e.data, "  .mods = %d, .prim = 0,\n", e.classMods(cl))
	// The member tables are not named here: whether they ship is a decision the
	// program's call sites make later, so the startup attaches them (see meta).
	_ = fieldsTbl
	_ = methodsTbl
	fmt.Fprintf(e.data, "  .consts = %s, .nconsts = %d,\n", constsTbl, len(cl.EnumConsts))
	e.data.WriteString("};\n")
}

// boxStruct is the runtime's C type for a wrapper's bytes, which is what the
// generated struct of that wrapper is asserted against. The runtime's own view
// of the layout is stated once, in internal/runtime/src/tyrt.h.
func boxStruct(cl *ast.Class) string {
	switch cl.Name {
	case "Integer":
		return "tyintbox"
	case "Long":
		return "tylongbox"
	case "Double":
		return "tydoublebox"
	case "Float":
		return "tyfloatbox"
	case "Character":
		return "tycharbox"
	case "Boolean":
		return "tyboolbox"
	case "Byte":
		return "tybytebox"
	case "Short":
		return "tyshortbox"
	}
	return ""
}

// bindCaptures points the captured variables of an anonymous class at the
// fields its constructor filled, and returns a function restoring the previous
// bindings.
func (e *Emitter) bindCaptures(cl *ast.Class) func() {
	var saved []*ast.Var
	var prev []string
	for _, v := range e.prog.CapturedVars(cl) {
		saved = append(saved, v)
		prev = append(prev, e.locals[v])
		e.locals[v] = "this->cap_" + mangle(v.Name)
	}
	return func() {
		for i, v := range saved {
			e.locals[v] = prev[i]
		}
	}
}

func (e *Emitter) sizeOf(t ast.Type) int64 { return util.SizeOf(t) }

// ---------------------------------------------------------------- methods

func (e *Emitter) emitClassCode(cl *ast.Class) {
	e.emitClInit(cl)
	for _, name := range mangleOrder(cl) {
		for i, m := range cl.Methods[name] {
			e.emitMethod(cl, m, i)
		}
	}
	for i, m := range cl.Ctors {
		e.emitMethod(cl, m, i)
	}
}

func mangleOrder(cl *ast.Class) []string {
	var names []string
	for n := range cl.Methods {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// bodyOf returns the syntax body of a method, if any.
func bodyOf(m *ast.Method) *ast.Block {
	if m.Body != nil {
		return m.Body
	}
	if m.Decl != nil && m.Decl.Body != nil {
		return m.Decl.Body
	}
	if m.Accessor != nil {
		return m.Accessor.Body
	}
	return nil
}

// blockHasTry reports whether a statement block contains a try, at any depth.
//
// It walks statements and never expressions, which is what keeps a lambda inside
// one from counting: a lambda's body is a function of its own, emitted
// separately, and its try statements say nothing about where this function's
// locals have to live.
func blockHasTry(b *ast.Block) bool {
	if b == nil {
		return false
	}
	var stmt func(ast.Stmt) bool
	stmt = func(s ast.Stmt) bool {
		switch v := s.(type) {
		case *ast.Try:
			return true
		case *ast.Block:
			for _, s2 := range v.Stmts {
				if stmt(s2) {
					return true
				}
			}
		case *ast.If:
			return stmt(v.Then) || stmt(v.Else)
		case *ast.While:
			return stmt(v.Body)
		case *ast.DoWhile:
			return stmt(v.Body)
		case *ast.For:
			return stmt(v.Body)
		case *ast.ForEach:
			return stmt(v.Body)
		case *ast.Switch:
			for _, c := range v.Cases {
				for _, s2 := range c.Body {
					if stmt(s2) {
						return true
					}
				}
			}
		}
		return false
	}
	for _, s := range b.Stmts {
		if stmt(s) {
			return true
		}
	}
	return false
}

// signature renders the C declaration of a method.
func (e *Emitter) signature(m *ast.Method) string {
	ret := e.ctype(m.Result)
	if m.IsCtor {
		ret = "void"
	}
	var params []string
	if !m.IsStatic() {
		params = append(params, cname(m.Owner)+"* this")
	}
	for i, p := range m.Params {
		// A parameter is a C parameter here, so it is an automatic object of the
		// function that contains the setjmp -- and if the program assigns to it
		// inside a try and reads it in the catch, the C standard makes its value
		// after the longjmp indeterminate. It has to be volatile, and the
		// qualifier has to sit after the type: `volatile T*` is a pointer to a
		// volatile T, which would qualify the pointee and not the variable, and
		// it also disagrees with the forward declaration. Top-level qualifiers on
		// a parameter are ignored by the C compiler, so the prototype stays
		// plain and the two still agree.
		if e.fnTry {
			params = append(params, fmt.Sprintf("%s volatile a%d", e.ctype(p), i))
			continue
		}
		params = append(params, fmt.Sprintf("%s a%d", e.ctype(p), i))
	}
	// A constructor of a class that captures the enclosing method's variables
	// takes them after the ones it declares, because the object has to hold
	// them before any initializer of its own runs. They are the compiler's
	// parameters rather than the program's -- the checker's view of the
	// signature is the declared one, which is what a `new` has to match -- so
	// only the C declaration and the calls carry them.
	if m.IsCtor {
		for i, v := range e.prog.CapturedVars(m.Owner) {
			params = append(params, fmt.Sprintf("%s a%d", e.ctype(v.Type), len(m.Params)+i))
		}
	}
	if len(params) == 0 {
		params = append(params, "void")
	}
	return fmt.Sprintf("%s %s(%s)", ret, e.cfunc(m), strings.Join(params, ", "))
}

func (e *Emitter) emitMethod(cl *ast.Class, m *ast.Method, idx int) {
	if m.SynthKind == "lambda" || m.SynthKind == "lambda-ctor" {
		e.emitLambdaMethod(cl, m)
		return
	}
	if m.External {
		// a native method is implemented outside this translation unit
		fmt.Fprintf(e.fns, "%s;\n", e.nativeSignature(m))
		return
	}
	fmt.Fprintf(e.fns, "%s%s;\n", e.link, e.signature(m))
	body := bodyOf(m)
	if body == nil && !m.IsCtor && m.SynthKind != "" {
		e.emitSynthetic(cl, m)
		return
	}
	if body == nil && !m.IsCtor && m.Decl != nil {
		e.emitSynthetic(cl, m)
		return
	}
	// A default accessor -- `get` with no body -- is exactly the method that
	// has no syntax to emit: its body is the backing field, which emitSynthetic
	// writes. Without this it fell through to the unimplemented trap and the
	// program stopped the first time the property was used.
	if body == nil && !m.IsCtor && m.Accessor != nil {
		e.emitSynthetic(cl, m)
		return
	}
	e.indent = 0
	// A local assigned inside a try and read in its catch or finally has to
	// survive a longjmp, and the C standard calls such a value indeterminate
	// unless it is volatile. So every local (and parameter) of a function that
	// contains a try at all is emitted volatile: the rule is conservative in the
	// safe direction -- a local cannot be proved safe by its position, because
	// the emitter's own scope walks are many -- and it costs only the functions
	// that catch, which the hot loops are not.
	prevTry := e.fnTry
	e.fnTry = blockHasTry(body)
	fmt.Fprintf(e.code, "%s%s {\n", e.link, e.signature(m))
	e.indent++
	e.stackCheck()
	prevClass, prevRet := e.curClass, e.retType
	prevStack := e.stackLocals
	e.curClass = cl
	e.retType = m.Result
	e.stackLocals = e.findStackLocals(body)
	restore := e.bindCaptures(cl)
	defer func() {
		e.curClass, e.retType = prevClass, prevRet
		e.stackLocals = prevStack
		e.fnTry = prevTry
		restore()
	}()
	var copies []string
	for i, pv := range m.ParamVars {
		name := fmt.Sprintf("a%d", i)
		if e.fnTry {
			// gcc does not honour a volatile qualifier on a parameter object:
			// in a function that catches, it reads back the value the parameter
			// had on entry where clang reads the one assigned inside the try
			// (measured on gcc 16 at -O2, with and without LTO, against the
			// same program under clang). A volatile *local* it does honour, so
			// the parameter is copied into one at entry and every access goes
			// through the copy. The qualifier on the C parameter is left in
			// place for the compilers that do honour it; this copy is what
			// makes the answer the same on both.
			vol := fmt.Sprintf("v%d_%s", pv.ID, mangle(pv.Name))
			copies = append(copies, fmt.Sprintf("%s volatile %s = %s;",
				e.ctype(pv.Type), vol, name))
			name = vol
		}
		e.locals[pv] = name
	}
	// The frame map is written around the body: the collector's precise view of
	// this frame is what the prologue there declares and what the body fills in
	// (frames.go). The stack check stays the first statement, before it.
	e.frameTemps = nil
	e.frameBase = len(e.funcParams(m))
	e.emitMappedBody(e.cfunc(m), e.funcParams(m), func() {
		// The parameter copies are the body's own first declarations, and that
		// is where they have to be: what tells the collector about a word is
		// the registration the pass writes, and the pass writes it by reading
		// the emitted body (frames.go). A declaration emitted before the
		// prologue is a declaration the pass never sees, and a copy of a
		// reference parameter that no word names is retention the collector
		// does not have -- the object the copy holds is freed while the
		// program still points at it.
		for _, c := range copies {
			e.line("%s\n", c)
		}
		if m.IsCtor {
			e.emitCtorBody(cl, m)
		} else if body != nil {
			if m.Mods.Has(ast.ModSynchronized) {
				// A synchronized method holds its monitor for the whole body, the
				// way Java's does. The lock is the instance, or the class for a
				// static method -- the same object Java locks, since a Class value
				// in Teyru is that class's tyclass and there is exactly one per
				// class. Until the runtime's monitors became real this modifier was
				// parsed, kept for reflection and emitted as nothing at all.
				e.syncBlock(syncMethodLock(cl, m), false, func() {
					e.emitBlockInner(body)
				})
			} else {
				e.emitBlockInner(body)
			}
		} else {
			// Only reachable for abstract or native methods with no implementation;
			// fail loudly instead of returning an undefined value.
			e.line("ty_unimplemented(%s);\n", e.cstr(cl.Full+"."+m.Name))
		}
	})
	e.indent--
	e.code.WriteString("}\n\n")
}

// syncMethodLock is the monitor of a synchronized method: the receiver for an
// instance method, and the class itself for a static one. A tyclass address is
// stable for the life of the program and unique per class, which is what a
// monitor needs of a key; Java locks the Class object, and this is that class's.
func syncMethodLock(cl *ast.Class, m *ast.Method) string {
	if m.IsStatic() {
		return "(void*)&cls_" + mangle(cl.Full)
	}
	return "(void*)this"
}

// ctorCallIndex returns the position of the this()/super() call in a
// constructor body, or -1 when there is none.
func ctorCallIndex(b *ast.Block) int {
	for i, st := range b.Stmts {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if call, ok := es.X.(*ast.Call); ok && call.ThisCtor {
			return i
		}
	}
	return -1
}

// emitCtorBody writes the chained constructor call, field initializers and the
// body. Teyru supports flexible constructor bodies (JEP 513): statements may
// appear before the super()/this() call, and they run first.
func (e *Emitter) emitCtorBody(cl *ast.Class, m *ast.Method) {
	e.line("\n")
	body := bodyOf(m)
	idx := -1
	if body != nil {
		idx = ctorCallIndex(body)
	}
	// 1. prologue: statements before the explicit constructor call
	if idx > 0 {
		for _, st := range body.Stmts[:idx] {
			e.stmt(st)
		}
	}
	// 2. chain to the superclass or the sibling constructor
	switch {
	case m.SynthKind == "anon-ctor" && m.Forward != nil:
		recv := "this"
		if o := m.Forward.Owner; o != nil {
			recv = "(" + cname(o) + "*)this"
		}
		var args []string
		for i := range m.Params {
			args = append(args, fmt.Sprintf("a%d", i))
		}
		call := e.cfunc(m.Forward) + "(" + recv
		if len(args) > 0 {
			call += ", " + strings.Join(args, ", ")
		}
		e.line("%s);\n", call)
	case idx >= 0:
		// the explicit call is emitted with the rest of the body
	case cl.Super == nil || cl.Super.Class == e.prog.Builtins.Object:
		// nothing to chain to
	default:
		if sup := e.findSuperCtor(cl); sup != nil {
			e.line("%s;\n", e.callTo(e.args(seqOperand{text: "this"}, nil, sup), e.cfunc(sup)+"(", ")"))
		}
	}
	// captured locals arrive as parameters trailing the declared ones, which is
	// what the signature added; a local class captures them the same way an
	// anonymous one does
	for i, cv := range e.prog.CapturedVars(cl) {
		e.line("this->cap_%s = a%d;\n", mangle(cv.Name), len(m.Params)+i)
	}
	// 3. instance field initializers run after the superclass constructor
	e.emitFieldInits(cl, m)
	// 4. the body itself (including the explicit constructor call, if any)
	if body != nil {
		stmts := body.Stmts
		if idx > 0 {
			stmts = stmts[idx:]
		}
		for _, st := range stmts {
			e.stmt(st)
		}
	}
	if m.Decl != nil && m.Decl.Compact {
		e.emitRecordAssign(cl, m)
		return
	}
	if m.SynthKind == "record-ctor" {
		e.emitRecordAssign(cl, m)
	}
}

func (e *Emitter) emitRecordAssign(cl *ast.Class, m *ast.Method) {
	for _, rc := range cl.Decl.RecordComps {
		f := cl.FieldMap[rc.Name]
		if f == nil {
			continue
		}
		e.line("this->f_%s = a%d;\n", mangle(f.Name), indexOfName(m.ParamNames, rc.Name))
	}
}

func indexOfName(names []string, n string) int {
	for i, x := range names {
		if x == n {
			return i
		}
	}
	return 0
}

// emitFieldInits runs instance field initializers in declaration order.
func (e *Emitter) emitFieldInits(cl *ast.Class, ctor *ast.Method) {
	if cl.Decl == nil {
		return
	}
	// superclass fields are initialized by the super constructor
	for _, mem := range cl.Decl.Members {
		fd, ok := mem.(*ast.FieldDecl)
		if !ok {
			continue
		}
		for _, vd := range fd.Vars {
			if vd.Init == nil || vd.Fld == nil || vd.Fld.Mods.Has(ast.ModStatic) {
				continue
			}
			target := "this->f_" + mangle(vd.Fld.Name)
			e.frameScope(func() {
				e.line("%s = %s;\n", target, e.coerce(e.expr(vd.Init), vd.Init.GetType(), vd.Fld.Type))
			})
		}
	}
	for _, mem := range cl.Decl.Members {
		ib, ok := mem.(*ast.InitBlock)
		if !ok || ib.Static {
			continue
		}
		e.emitBlockInner(ib.Body)
	}
}

func (e *Emitter) findSuperCtor(cl *ast.Class) *ast.Method {
	sup := cl.Super.Class
	for _, c := range sup.Ctors {
		if len(c.Params) == 0 {
			return c
		}
	}
	if len(sup.Ctors) > 0 {
		return sup.Ctors[0]
	}
	return nil
}

// line writes an indented line of generated C.
func (e *Emitter) line(format string, args ...any) {
	for i := 0; i < e.indent; i++ {
		e.code.WriteString("  ")
	}
	fmt.Fprintf(e.code, format, args...)
}

// stackCheck writes the first statement of a generated function body: the
// address of the frame this function is about to run, against the thread's
// stack limit. A function that finds itself past the limit throws a
// StackOverflowError instead of running, which is what makes a recursion that
// went too deep a Java error the program can catch rather than a fault that
// ends the process. The check is ty_stack_check in
// internal/runtime/src/tyrt.h, inlined into the function it guards so that
// there is no frame between the frame that is measured and the limit.
func (e *Emitter) stackCheck() { e.line("ty_stack_check();\n") }

func (e *Emitter) tmpName() string {
	e.tmp++
	return fmt.Sprintf("_t%d", e.tmp)
}

// threadFinish is what main does last: wait for every other thread, so that a
// thread's output is not cut off by main returning. It is a call and not a
// scheduling trick because the runtime owns the thread registry (tyrt_thread.c).
const threadFinish = "ty_thread_join_all();\n"

// entry emits main() and the startup sequence.
func (e *Emitter) entry() string {
	var b strings.Builder
	main := e.prog.Main
	b.WriteString("\nint main(int argc, char** argv) {\n  (void)argc; (void)argv;\n  ty_init();\n")
	// main's own check, like every other generated function's: it can never
	// answer for the frame it is in, and being the same shape as the rest is
	// worth more than the line it would save to leave it out.
	b.WriteString("  ty_stack_check();\n")
	if e.split {
		// The standard library's unit is linked in rather than written here, so
		// what it has to do at startup -- the collector roots its static fields
		// hold and the member tables its classes are handed -- is a call.
		b.WriteString("  ty_prelude_install();\n")
	}
	b.WriteString(e.clinitRefs(false))
	b.WriteString("  TY_STRING = &cls_" + mangle(e.prog.Builtins.String.Full) + ";\n")
	b.WriteString("  TY_OBJECT = &cls_" + mangle(e.prog.Builtins.Object.Full) + ";\n")
	// the arrays the compiler creates get this class, so `a instanceof Object`,
	// `(Object) a` and `"" + a` behave, and the collector can see that an array's
	// elements are references and trace them
	b.WriteString("  TY_ARRAY = &cls_" + mangle(e.prog.ArrayClass().Full) + ";\n")
	// the member tables, attached here rather than in the class object's
	// initializer so that a program which never reflects does not carry them
	if e.reflectUsed {
		for _, line := range e.metaInit {
			b.WriteString("  " + line + "\n")
		}
	}
	// sorted: iterating the map directly would emit TY_BOX assignments in a
	// different order every run, so two builds of one program would not produce
	// the same C and the output could not be diffed
	boxKinds := make([]int, 0, len(e.prog.Builtins.Boxes))
	for k := range e.prog.Builtins.Boxes {
		boxKinds = append(boxKinds, int(k))
	}
	sort.Ints(boxKinds)
	for _, k := range boxKinds {
		fmt.Fprintf(&b, "  TY_BOX[%d] = &cls_%s;\n", k, mangle(e.prog.Builtins.Boxes[ast.PrimKind(k)].Full))
	}
	for _, pair := range [][2]any{
		{"TY_NPE", e.prog.Builtins.NPE}, {"TY_AIOOBE", e.prog.Builtins.AIOOBE},
		{"TY_SIOOBE", e.prog.Builtins.SIOOBE},
		{"TY_SOE", e.prog.Builtins.SOE},
		{"TY_ARITH", e.prog.Builtins.Arith}, {"TY_CCE", e.prog.Builtins.CCE},
		{"TY_NEGARR", e.prog.Builtins.NegArr}, {"TY_ASSERT", e.prog.Builtins.Assertion},
		{"TY_ILLARG", e.prog.Builtins.IllArg}, {"TY_ILLSTATE", e.prog.Builtins.IllState},
		{"TY_NOSUCHELEM", e.prog.Builtins.NoSuchElem}, {"TY_UNSUP", e.prog.Builtins.Unsup},
		{"TY_ILLMON", e.prog.Builtins.IllMon},
		{"TY_ARRAYSTORE", e.prog.Builtins.ArrayStore},
		{"TY_CNF", e.prog.Builtins.ClassNotFound}, {"TY_NSFE", e.prog.Builtins.NoSuchField},
		{"TY_NSME", e.prog.Builtins.NoSuchMethod}, {"TY_ILLACCESS", e.prog.Builtins.IllAccess},
		{"TY_INVOCATION", e.prog.Builtins.Invocation}, {"TY_INSTANTIATION", e.prog.Builtins.Instantiation},
	} {
		if cl, ok := pair[1].(*ast.Class); ok && cl != nil {
			fmt.Fprintf(&b, "  %s = &cls_%s;\n", pair[0], mangle(cl.Full))
		}
	}
	// The thread's StackOverflowError, before any generated function can run:
	// the frame that throws it has the margin left and nothing else, so the
	// object it throws has to exist already.
	b.WriteString("  ty_stack_overflow_reserve();\n")
	b.WriteString("  ty_clinit(&cls_" + mangle(e.prog.Builtins.Object.Full) + ");\n")
	// Only the classes that actually have a static initializer are named here.
	// ty_clinit on a class whose whole hierarchy has none is a no-op, but
	// naming it takes the class's address in main, and that single reference
	// keeps the class -- its vtable, its interface table, its methods, and
	// everything it in turn mentions -- alive through link-time optimisation.
	// Taking one address per class pinned the entire standard library into
	// every executable: a hello world built from the same sources was 1.8 MB
	// with 1.4 MB of it interface tables no program could ever reach.
	//
	// The lazy guards the call sites already carry (clinitCall, clinitStmt)
	// are what actually runs an initializer, in Java's on-first-use order.
	seenInit := map[string]bool{}
	for _, cl := range e.prog.Classes {
		if cl.Builtin || seenInit[cname(cl)] || !needsClinit(cl, map[*ast.Class]bool{}) {
			continue
		}
		seenInit[cname(cl)] = true
		fmt.Fprintf(&b, "  ty_clinit(&cls_%s);\n", mangle(cl.Full))
	}
	if main == nil {
		b.WriteString("  " + threadFinish + "  return 0;\n}\n")
		return b.String()
	}
	recv := ""
	if !main.IsStatic() {
		// the class is a C struct, so the instance an instance main runs on is
		// its pointer; spelling these without the `*` emitted C that does not
		// compile for a compact file with `void main(String[] args)`
		fmt.Fprintf(&b, "  %s* _main_obj = (%s*)ty_alloc(sizeof(%s));\n", cname(main.Owner), cname(main.Owner), cname(main.Owner))
		fmt.Fprintf(&b, "  _main_obj->obj.cls = &cls_%s;\n", mangle(main.Owner.Full))
		recv = "_main_obj, "
	}
	if len(main.Params) == 1 {
		b.WriteString("  tyarr* _args = ty_array_new(argc > 0 ? argc - 1 : 0, 8);\n")
		b.WriteString("  _args->refs = 1;\n")
		b.WriteString("  for (int _i = 1; _i < argc; _i++) ((void**)_args->data)[_i - 1] = (void*)ty_str_intern(argv[_i]);\n")
		fmt.Fprintf(&b, "  %s(%s_args);\n", e.cfunc(main), recv)
	} else {
		fmt.Fprintf(&b, "  %s(%s);\n", e.cfunc(main), strings.TrimSuffix(recv, ", "))
	}
	// The process ends with its last thread, not with main: a program that
	// starts a thread and returns must not lose what that thread printed, which
	// is what java.lang.Thread's "the virtual machine exits when every
	// non-daemon thread has finished" means here. It is a walk of the thread
	// registry that a program without threads pays a call for.
	b.WriteString("  " + threadFinish + "  return 0;\n}\n")
	return b.String()
}

// capture runs fn with a fresh output buffer and returns what it wrote.
func (e *Emitter) capture(fn func()) string {
	old := e.code
	e.code = &strings.Builder{}
	fn()
	s := e.code.String()
	e.code = old
	return s
}
