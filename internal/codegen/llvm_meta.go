package codegen

// The reflection tables of the LLVM back end: the annotations written on a
// class, its fields and its methods, and the field and method descriptors the
// runtime's java.lang.reflect reads.
//
// The shapes are the C back end's, field for field (internal/runtime/src/tyrt.h
// declares them, and reflect.go writes them): a table the runtime reads cannot
// depend on which back end built the program. What is different here is only
// where the data goes -- an LLVM global with the struct's padding spelled out,
// rather than a C initializer with designators -- and that a class the
// reachability scan left out keeps the null table its record was written with
// (reflectset.go), exactly as in the C build.
//
// A method's `fn` is the invoker Method.invoke would call. This back end
// refuses Method.invoke and Constructor.newInstance -- the call has no lowering
// here -- so the entry is a stub that stops with the method's name rather than
// a function nothing could have checked. It is the same device the vtable's
// unreached slots use: a trap says which method was missing instead of jumping
// through a null pointer.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// The C structs, as tyrt.h declares them. Every offset is the one the C
// compiler gives the field on x86-64; structDecl writes the same padding LLVM
// would, and these offsets say where the C side put each field.
var tyannoargLayout = []rtField{
	{"name", "ptr", 0},
	{"kind", "i32", 8},
	{"ival", "i64", 16},
	{"dval", "double", 24},
	{"sval", "ptr", 32},
	{"cval", "ptr", 40},
}

var tyannotationLayout = []rtField{
	{"type", "ptr", 0},
	{"args", "ptr", 8},
	{"nargs", "i32", 16},
}

var tyfieldLayout = []rtField{
	{"name", "ptr", 0},
	{"type", "ptr", 8},
	{"owner", "ptr", 16},
	{"off", "i32", 24},
	{"mods", "i32", 28},
	{"addr", "ptr", 32},
	{"prim", "i32", 40},
	{"annos", "ptr", 48},
	{"nannos", "i32", 56},
	{"elem", "ptr", 64},
}

var tymethodLayout = []rtField{
	{"name", "ptr", 0},
	{"fn", "ptr", 8},
	{"owner", "ptr", 16},
	{"ret", "ptr", 24},
	{"params", "ptr", 32},
	{"nparams", "i32", 40},
	{"mods", "i32", 44},
	{"kind", "i32", 48},
	{"primret", "i32", 52},
	{"annos", "ptr", 56},
	{"nannos", "i32", 64},
	{"pannos", "ptr", 72},
	{"pnannos", "ptr", 80},
	{"pnames", "ptr", 88},
}

// classMeta is what one class's tyclass record names: the tables emitted for
// it, and their lengths.
type classMeta struct {
	annos    string
	nannos   int
	fields   string
	nfields  int
	methods  string
	nmethods int
}

// covers reports whether a class's member tables ship, which is the C back
// end's question asked of the same answer (reflectset.go).
func (e *llvmEmitter) covers(cl *ast.Class) bool {
	return e.reflTables != nil && e.reflTables.covers(cl)
}

// emitClassMeta writes the tables a class's record points at: its own
// annotations, its fields and its methods (constructors included, marked as
// such, because that is how the runtime tells them apart).
func (e *llvmEmitter) emitClassMeta(cl *ast.Class) {
	if !e.covers(cl) {
		return
	}
	m := &classMeta{annos: "ptr null", fields: "ptr null", methods: "ptr null"}
	m.annos, m.nannos = e.emitAnnoTable(util.Mangle(cl.Full), annoListOf(cl))
	m.fields, m.nfields = e.emitFieldTable(cl)
	m.methods, m.nmethods = e.emitMethodTable(cl)
	e.meta[cl] = m
}

// needReflectionClasses brings in every class the member tables of the classes
// seen so far name: a field's type and element type, a method's result and
// parameters, and the annotation types and class literals of the annotations on
// all of them. It answers whether the class set grew, which is what makes the
// caller drain the method queue again.
func (e *llvmEmitter) needReflectionClasses() bool {
	before := len(e.classOrder)
	queued := len(e.mqueue)
	// A reflective program may read a primitive field through Field.get, and
	// the runtime builds the wrapper TY_BOX names. The startup installs the
	// boxes the program's own code boxes into; reflection can name one no code
	// does, so once a member table ships every wrapper is installed and
	// instantiated -- the runtime calls its toString/hashCode/equals through
	// the vtable, so a null slot there is a crash rather than a wrong value.
	if !e.reflBoxes {
		for _, cl := range e.classOrder {
			if e.covers(cl) {
				e.reflBoxes = true
				for k, box := range e.p.Builtins.Boxes {
					e.boxUsed[k] = true
					e.instantiate(box)
				}
				break
			}
		}
	}
	// The slice grows while it is walked: a class brought in here has a table
	// of its own to walk if the scan covers it.
	for i := 0; i < len(e.classOrder); i++ {
		cl := e.classOrder[i]
		if !e.covers(cl) {
			continue
		}
		e.needAnnoClasses(annoListOf(cl))
		for _, f := range cl.Fields {
			e.needTypeClass(f.Type)
			if elem := e.cEmitter().elementOf(f.Type); elem != nil {
				e.needTypeClass(elem)
			}
			e.needAnnoClasses(f.Annos)
		}
		for _, name := range mangleOrder(cl) {
			for _, m := range cl.Methods[name] {
				e.needMethodTypes(m)
			}
		}
		for _, m := range cl.Ctors {
			e.needMethodTypes(m)
		}
	}
	return len(e.classOrder) != before || len(e.mqueue) != queued
}

// needMethodTypes brings in the classes a method descriptor names.
func (e *llvmEmitter) needMethodTypes(m *ast.Method) {
	e.needTypeClass(m.Result)
	for _, p := range m.Params {
		e.needTypeClass(p)
	}
	e.needAnnoClasses(methodAnnos(m))
	for _, list := range m.ParamAnnos {
		e.needAnnoClasses(list)
	}
}

// needAnnoClasses brings in the classes an annotation table names.
func (e *llvmEmitter) needAnnoClasses(annos []*ast.Annotation) {
	for _, a := range annos {
		cl := e.cEmitter().annoClass(a)
		if cl == nil {
			continue
		}
		e.needClass(cl)
		for _, arg := range e.annoArgs(a) {
			e.needAnnoValueClass(arg.Value)
		}
	}
}

// needAnnoValueClass brings in the class an annotation element's value names:
// a class literal, or an enum constant's class.
func (e *llvmEmitter) needAnnoValueClass(v ast.Expr) {
	switch x := v.(type) {
	case *ast.ClassLit:
		if cl := e.cEmitter().classLitClass(x); cl != nil {
			e.needClass(cl)
		}
	case *ast.Select:
		if f, ok := x.Ref.(*ast.Field); ok {
			if f.Owner != nil {
				e.needClass(f.Owner)
			}
		} else if id, isIdent := x.X.(*ast.Ident); isIdent {
			if ec := e.p.ProgramClass(id.Name); ec != nil && ec.Kind == ast.KindEnum {
				e.needClass(ec)
			}
		}
	case *ast.Ident:
		if f, ok := x.Ref.(*ast.Field); ok && f.Owner != nil {
			e.needClass(f.Owner)
		}
	}
}

// needTypeClass brings in the class a type names, the way typeClassOperand
// will name it.
func (e *llvmEmitter) needTypeClass(t ast.Type) {
	switch tt := erasureOf(t).(type) {
	case *ast.PrimType:
		e.needPrimClass(tt.Kind)
	case *ast.ClassType:
		if tt.Class != nil {
			e.needClass(tt.Class)
		}
	case *ast.ArrayType:
		e.useArray()
	}
}

// classOperand names a class's record: the pointer a table slot holds.
func (e *llvmEmitter) classOperand(cl *ast.Class) string {
	e.needClass(cl)
	return "@cls_" + util.Mangle(cl.Full)
}

// typeClassOperand names the class object of a type, which is what a member
// descriptor records: the primitive's own class, the class the type names, or
// the one class every array belongs to.
func (e *llvmEmitter) typeClassOperand(t ast.Type) string {
	switch tt := erasureOf(t).(type) {
	case *ast.PrimType:
		return e.needPrimClass(tt.Kind)
	case *ast.ClassType:
		if tt.Class != nil {
			return e.classOperand(tt.Class)
		}
	case *ast.ArrayType:
		if cl := e.useArray(); cl != nil {
			return "@cls_" + util.Mangle(cl.Full)
		}
	}
	if o := e.p.Builtins.Object; o != nil {
		return e.classOperand(o)
	}
	return "ptr null"
}

// annoArgs is the element list an annotation carries at run time: what the use
// wrote, then the declared default of every element it left out, which is what
// Java requires -- a reader asks without knowing whether it was written.
func (e *llvmEmitter) annoArgs(a *ast.Annotation) []*ast.AnnoArg {
	ce := e.cEmitter()
	cl := ce.annoClass(a)
	if cl == nil {
		return nil
	}
	out := make([]*ast.AnnoArg, 0, len(a.Args))
	written := map[string]bool{}
	for _, arg := range a.Args {
		out = append(out, arg)
		written[annoArgName(arg)] = true
	}
	for _, el := range annoElements(cl) {
		if written[el.Name] || el.Decl == nil || el.Decl.Default == nil {
			continue
		}
		out = append(out, &ast.AnnoArg{Name: el.Name, Value: el.Decl.Default})
	}
	return out
}

// emitAnnoTable writes the annotations of one element and answers with the
// pointer a record holds and how many entries it points at, or a null table
// when the element carries none.
func (e *llvmEmitter) emitAnnoTable(suffix string, annos []*ast.Annotation) (string, int) {
	if len(annos) == 0 {
		return "ptr null", 0
	}
	ce := e.cEmitter()
	var entries []string
	for i, a := range annos {
		cl := ce.annoClass(a)
		if cl == nil {
			continue
		}
		args := e.annoArgs(a)
		argsOp := "ptr null"
		if len(args) > 0 {
			inits := make([]string, len(args))
			for j, arg := range args {
				inits[j] = e.annoArgInit(arg)
			}
			argsOp = fmt.Sprintf("ptr @annarg_%s_%d", suffix, i)
			fmt.Fprintf(&e.globals, "@annarg_%s_%d = internal global [%d x %%tyannoarg] [%s], align 8\n",
				suffix, i, len(inits), strings.Join(inits, ", "))
		}
		vals := map[string]string{
			"type":  "ptr " + e.classOperand(cl),
			"args":  argsOp,
			"nargs": fmt.Sprintf("i32 %d", len(args)),
		}
		entries = append(entries, "%tyannotation "+recordInit(e.tyannotationEntries, vals))
	}
	if len(entries) == 0 {
		return "ptr null", 0
	}
	name := "anns_" + suffix
	fmt.Fprintf(&e.globals, "@%s = internal global [%d x %%tyannotation] [%s], align 8\n",
		name, len(entries), strings.Join(entries, ", "))
	return "ptr @" + name, len(entries)
}

// annoArgInit renders one annotation element as a tyannoarg initializer. The
// kinds are the runtime's TY_ANN_* macros; a value this cannot carry is
// recorded as unsupported rather than dropped, so reading it says so where it
// is read.
func (e *llvmEmitter) annoArgInit(arg *ast.AnnoArg) string {
	name := annoArgName(arg)
	kind := int32(annUnsupported)
	ival, dval, sval, cval := "i64 0", "double 0.0", "ptr null", "ptr null"
	switch v := arg.Value.(type) {
	case *ast.Literal:
		switch v.Kind {
		case ast.LitString:
			kind, sval = annString, "ptr "+e.cstring(v.Str)
		case ast.LitInt:
			kind, ival = annInt, fmt.Sprintf("i64 %d", int64(v.Int))
		case ast.LitLong:
			kind, ival = annLong, fmt.Sprintf("i64 %d", int64(v.Int))
		case ast.LitFloat, ast.LitDouble:
			kind, dval = annDouble, "double "+llvmFloat(v.Flt, v.Kind == ast.LitFloat)
		case ast.LitBool:
			kind = annBool
			if v.Bool {
				ival = "i64 1"
			}
		case ast.LitChar:
			kind, ival = annInt, fmt.Sprintf("i64 %d", int64(v.Int))
		}
	case *ast.ClassLit:
		// An annotation argument is read as text while checking, so the class
		// literal is usually unresolved and the type is found by the name that
		// was written -- the same rule the C back end applies.
		if cl := e.cEmitter().classLitClass(v); cl != nil {
			kind, cval = annClass, "ptr "+e.classOperand(cl)
		}
	case *ast.Select:
		if f, ok := v.Ref.(*ast.Field); ok {
			kind, sval, cval = e.enumArg(f)
		} else if id, isIdent := v.X.(*ast.Ident); isIdent {
			if ec := e.p.ProgramClass(id.Name); ec != nil && ec.Kind == ast.KindEnum {
				kind, sval, cval = annEnum, "ptr "+e.cstring(v.Name), "ptr "+e.classOperand(ec)
			}
		}
	case *ast.Ident:
		if f, ok := v.Ref.(*ast.Field); ok {
			kind, sval, cval = e.enumArg(f)
		}
	}
	return "%tyannoarg " + recordInit(e.tyannoargEntries, map[string]string{
		"name": "ptr " + e.cstring(name),
		"kind": fmt.Sprintf("i32 %d", kind),
		"ival": ival,
		"dval": dval,
		"sval": sval,
		"cval": cval,
	})
}

// enumArg renders an enum constant as an annotation element value: its name,
// and the class to compare it against. An element that is not an enum constant
// is unsupported rather than dropped.
func (e *llvmEmitter) enumArg(f *ast.Field) (int32, string, string) {
	if f == nil || f.Owner == nil || f.Owner.Kind != ast.KindEnum {
		return annUnsupported, "ptr null", "ptr null"
	}
	return annEnum, "ptr " + e.cstring(f.Name), "ptr " + e.classOperand(f.Owner)
}

// llvmFloat renders a floating-point constant for LLVM assembly, which wants a
// decimal point or an exponent.
func llvmFloat(f float64, single bool) string {
	bits := 64
	if single {
		bits = 32
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// emitFieldTable writes a class's field descriptors, which is what
// Class.getDeclaredField, Field.get and the annotations on a field are read
// from.
func (e *llvmEmitter) emitFieldTable(cl *ast.Class) (string, int) {
	if len(cl.Fields) == 0 {
		return "ptr null", 0
	}
	offs := e.cEmitter().fieldOffsets(cl)
	owner := "ptr " + e.classOperand(cl)
	entries := make([]string, 0, len(cl.Fields))
	for _, f := range cl.Fields {
		offset, addr := "-1", "ptr null"
		if f.Mods.Has(ast.ModStatic) {
			addr = "ptr " + e.staticGlobal(cl, f)
		} else {
			offset = fmt.Sprint(offs[f])
		}
		annos, nannos := e.emitAnnoTable(util.Mangle(cl.Full)+"_f_"+util.Mangle(f.Name), f.Annos)
		elem := "ptr null"
		if et := e.cEmitter().elementOf(f.Type); et != nil {
			elem = "ptr " + e.typeClassOperand(et)
		}
		entries = append(entries, "%tyfield "+recordInit(e.tyfieldEntries, map[string]string{
			"name":   "ptr " + e.cstring(f.Name),
			"type":   "ptr " + e.typeClassOperand(f.Type),
			"owner":  owner,
			"off":    fmt.Sprintf("i32 %s", offset),
			"mods":   fmt.Sprintf("i32 %d", javaMods(f.Mods)),
			"addr":   addr,
			"prim":   fmt.Sprintf("i32 %d", primKindOf(f.Type)),
			"annos":  annos,
			"nannos": fmt.Sprintf("i32 %d", nannos),
			"elem":   elem,
		}))
	}
	name := "fds_" + util.Mangle(cl.Full)
	fmt.Fprintf(&e.globals, "@%s = internal global [%d x %%tyfield] [%s], align 8\n",
		name, len(entries), strings.Join(entries, ", "))
	return "ptr @" + name, len(entries)
}

// emitMethodTable writes a class's method descriptors, constructors included
// and marked as such. The runtime filters on the kind, because Java does not
// call a constructor a method.
func (e *llvmEmitter) emitMethodTable(cl *ast.Class) (string, int) {
	if len(cl.Methods) == 0 && len(cl.Ctors) == 0 {
		return "ptr null", 0
	}
	var ms []*ast.Method
	for _, name := range mangleOrder(cl) {
		ms = append(ms, cl.Methods[name]...)
	}
	ms = append(ms, cl.Ctors...)
	owner := "ptr " + e.classOperand(cl)
	entries := make([]string, 0, len(ms))
	for mi, m := range ms {
		suffix := fmt.Sprintf("%s_m%d", util.Mangle(cl.Full), mi)
		params := "ptr null"
		if len(m.Params) > 0 {
			types := make([]string, len(m.Params))
			for i, p := range m.Params {
				types[i] = "ptr " + e.typeClassOperand(p)
			}
			name := "ps_" + suffix
			fmt.Fprintf(&e.globals, "@%s = internal global [%d x ptr] [%s], align 8\n",
				name, len(types), strings.Join(types, ", "))
			params = "ptr @" + name
		}
		kind := 0 // TY_METH_INSTANCE
		switch {
		case m.IsCtor:
			kind = 2 // TY_METH_CTOR
		case m.IsStatic():
			kind = 1 // TY_METH_STATIC
		}
		if m.Varargs {
			kind |= 4 // TY_METH_VARARGS
		}
		ret := "ptr " + e.typeClassOperand(m.Result)
		if m.IsCtor {
			ret = owner
		}
		annos, nannos := e.emitAnnoTable(suffix, methodAnnos(m))
		pannos, pnannos, pnames := "ptr null", "ptr null", "ptr null"
		if len(m.ParamNames) > 0 {
			names := make([]string, len(m.ParamNames))
			for i, n := range m.ParamNames {
				names[i] = "ptr " + e.cstring(n)
			}
			namesOp := "pnames_" + suffix
			fmt.Fprintf(&e.globals, "@%s = internal global [%d x ptr] [%s], align 8\n",
				namesOp, len(names), strings.Join(names, ", "))
			pnames = "ptr @" + namesOp
		}
		if len(m.ParamAnnos) > 0 {
			lists := make([]string, len(m.Params))
			counts := make([]string, len(m.Params))
			for i := range m.Params {
				name, n := "ptr null", 0
				if i < len(m.ParamAnnos) {
					name, n = e.emitAnnoTable(fmt.Sprintf("%s_p%d", suffix, i), m.ParamAnnos[i])
				}
				lists[i], counts[i] = name, fmt.Sprintf("i32 %d", n)
			}
			fmt.Fprintf(&e.globals, "@pannos_%s = internal global [%d x ptr] [%s], align 8\n",
				suffix, len(lists), strings.Join(lists, ", "))
			fmt.Fprintf(&e.globals, "@pnannos_%s = internal global [%d x i32] [%s], align 4\n",
				suffix, len(counts), strings.Join(counts, ", "))
			pannos, pnannos = "ptr @pannos_"+suffix, "ptr @pnannos_"+suffix
		}
		entries = append(entries, "%tymethod "+recordInit(e.tymethodEntries, map[string]string{
			"name":    "ptr " + e.cstring(m.Name),
			"fn":      e.invokerTrap(m),
			"owner":   owner,
			"ret":     ret,
			"params":  params,
			"nparams": fmt.Sprintf("i32 %d", len(m.Params)),
			"mods":    fmt.Sprintf("i32 %d", javaMods(m.Mods)),
			"kind":    fmt.Sprintf("i32 %d", kind),
			"primret": fmt.Sprintf("i32 %d", primKindOf(m.Result)),
			"annos":   annos,
			"nannos":  fmt.Sprintf("i32 %d", nannos),
			"pannos":  pannos,
			"pnannos": pnannos,
			"pnames":  pnames,
		}))
	}
	name := "mds_" + util.Mangle(cl.Full)
	fmt.Fprintf(&e.globals, "@%s = internal global [%d x %%tymethod] [%s], align 8\n",
		name, len(entries), strings.Join(entries, ", "))
	return "ptr @" + name, len(entries)
}

// invokerTrap writes the function a method descriptor's `fn` points at. A
// program this back end compiles cannot call it -- Method.invoke and
// Constructor.newInstance are refused, because the boxed-argument convention
// has no lowering here -- so it stops with the method's name rather than being
// a null pointer in a table the runtime would otherwise trust.
func (e *llvmEmitter) invokerTrap(m *ast.Method) string {
	key := e.methodSymbol(m)
	if n, ok := e.invNames[key]; ok {
		return "ptr @" + n
	}
	n := fmt.Sprintf("inv%d", len(e.invOrder))
	e.invNames[key] = n
	e.invOrder = append(e.invOrder, n)
	e.decl("ty_unimplemented", "void", []string{"ptr"}, " noreturn")
	fmt.Fprintf(&e.defs, "define internal ptr @%s(ptr %%self, ptr %%args) {\nentry:\n  call void @ty_unimplemented(ptr %s)\n  unreachable\n}\n\n",
		n, e.cstring(m.Owner.Full+"."+m.Name+" (llvm reflective invoke)"))
	return "ptr @" + n
}
