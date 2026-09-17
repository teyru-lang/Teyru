package codegen

/* Reflection metadata.
 *
 * The compiler writes one static table per class: its fields with their offsets
 * and flags, its methods and constructors with an invoker each, and its enum
 * constants. The runtime reads them (tyrt_reflect.c) and the prelude's
 * java.lang.reflect is written on top of that.
 *
 * The cost of it is data and code, never a longer object, a slower call or a
 * collector with more to walk: nothing here is built at run time, nothing is
 * reachable from an instance, and every pointer in the tables is to static
 * data, which the collector never follows.
 *
 * Java's modifier bits are the numbers java.lang.reflect.Modifier documents,
 * while Teyru's own Mods are a different numbering and a different set, so the
 * mapping is spelled out rather than shared. */

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

const (
	jModPublic       = 0x0001
	jModPrivate      = 0x0002
	jModProtected    = 0x0004
	jModStatic       = 0x0008
	jModFinal        = 0x0010
	jModSynchronized = 0x0020
	jModVolatile     = 0x0040
	jModTransient    = 0x0080
	jModNative       = 0x0100
	jModInterface    = 0x0200
	jModAbstract     = 0x0400
	jModAnnotation   = 0x2000
	jModEnum         = 0x4000

	// What every primitive class reports: public abstract final, as javac
	// answers for int.class and for void.class alike.
	jPrimClassMods = jModPublic | jModFinal | jModAbstract
)

// javaMods maps a Teyru modifier set onto Java's bits.
func javaMods(m ast.Mods) int32 {
	var out int32
	for _, p := range [...]struct {
		from ast.Mods
		to   int32
	}{
		{ast.ModPublic, jModPublic},
		{ast.ModPrivate, jModPrivate},
		{ast.ModProtected, jModProtected},
		{ast.ModStatic, jModStatic},
		{ast.ModFinal, jModFinal},
		{ast.ModSynchronized, jModSynchronized},
		{ast.ModTransient, jModTransient},
		{ast.ModVolatile, jModVolatile},
		{ast.ModNative, jModNative},
		{ast.ModAbstract, jModAbstract},
	} {
		if m.Has(p.from) {
			out |= p.to
		}
	}
	return out
}

// classMods is the modifier set a class reports. An interface is public,
// abstract and an interface; an annotation type is an interface that is also an
// annotation; an enum carries the enum bit. A record needs none of its own:
// its class-file flag is not one Modifier exposes, which is why Class.isRecord
// asks the runtime about the class kind instead.
func (e *Emitter) classMods(cl *ast.Class) int32 {
	out := javaMods(cl.Mods)
	if cl.IsInterface() {
		out |= jModInterface | jModAbstract
	}
	if cl.Decl != nil {
		switch cl.Decl.Kind {
		case ast.KindAnnotation:
			out |= jModAnnotation
		case ast.KindEnum:
			out |= jModEnum
		}
	}
	return out
}

// primKindOf answers with a type's primitive kind in the numbering the
// runtime's boxes use (1 boolean .. 8 double), or 0 for a reference. void has
// kind 0 as well: Java's void.class.isPrimitive() is false.
func primKindOf(t ast.Type) int32 {
	if p, ok := t.(*ast.PrimType); ok && p.Kind != ast.Void {
		return int32(p.Kind)
	}
	return 0
}

// isVoid reports whether a type is void, which a method's result may be and a
// parameter may not.
func isVoid(t ast.Type) bool {
	p, ok := t.(*ast.PrimType)
	return ok && p.Kind == ast.Void
}

// erasureOf is the type reflection reports for a member: Java erases a type
// variable to its leftmost bound, and a generic field is a field of that type
// to everything that reads it at run time.
func erasureOf(t ast.Type) ast.Type {
	if tv, ok := t.(*ast.TypeVarType); ok && tv.Var != nil && tv.Var.Bound != nil {
		return tv.Var.Bound
	}
	return t
}

// typeClassExpr names the class object of a type: the compiled class, the
// primitive's own class (which every program carries), or the single array
// class every array in a program belongs to.
func (e *Emitter) typeClassExpr(t ast.Type) string {
	switch tt := erasureOf(t).(type) {
	case *ast.PrimType:
		return "&cls_" + primClassName(tt.Kind)
	case *ast.ClassType:
		if tt.Class == nil {
			break
		}
		return "&cls_" + mangle(tt.Class.Full)
	case *ast.ArrayType:
		return "&cls_" + mangle(e.prog.ArrayClass().Full)
	}
	return "&cls_" + mangle(e.prog.Builtins.Object.Full)
}

// fieldOffsets answers with the byte offset of every instance field, keyed by
// the field, following the layout the runtime uses.
func (e *Emitter) fieldOffsets(cl *ast.Class) map[*ast.Field]int64 {
	var capTypes []ast.Type
	for _, v := range e.prog.CapturedVars(cl) {
		capTypes = append(capTypes, v.Type)
	}
	offs, _ := util.FieldOffsets(cl.InstFields, capTypes)
	out := make(map[*ast.Field]int64, len(cl.InstFields))
	for i, f := range cl.InstFields {
		out[f] = offs[i]
	}
	return out
}

// elementOf answers the declared element type of a container type: the element
// of a Collection or an array, the value type of a Map, and nil when the type
// declares none. It is the one thing reflection erases and a binding of a
// container field needs (see elementTypeExpr).
func (e *Emitter) elementOf(t ast.Type) ast.Type {
	switch tt := erasureOf(t).(type) {
	case *ast.ArrayType:
		return declaredElem(tt.Elem)
	case *ast.ClassType:
		if tt.Class == nil || len(tt.Args) == 0 {
			return nil
		}
		if iface := e.prog.LookupClass("teyru.Map"); iface != nil &&
			e.prog.IsSubtype(tt, &ast.ClassType{Class: iface}) {
			// a Map's elements are its values, which is the second argument
			if len(tt.Args) < 2 {
				return nil
			}
			return declaredElem(tt.Args[1])
		}
		return declaredElem(tt.Args[0])
	}
	return nil
}

// declaredElem is what a type argument names once it is erased: a wildcard is
// its bound, a type variable its bound, and anything else that is not a class,
// a primitive or an array -- a bare `?` or a `? super X`, whose elements are
// whatever Object's are -- is nil, so that the descriptor says "no element
// type" rather than naming a class the declaration never named.
func declaredElem(t ast.Type) ast.Type {
	if t == nil {
		return nil
	}
	if w, ok := t.(*ast.WildcardType); ok {
		if w.Bound == nil || w.Super {
			return nil
		}
		t = w.Bound
	}
	t = erasureOf(t)
	switch t.(type) {
	case *ast.PrimType, *ast.ClassType, *ast.ArrayType:
		return t
	}
	return nil
}

// elementTypeExpr names the class object of a field's declared element type, or
// NULL when the field's declared type carries none.
//
// This is where the generic type the language erases is written down: a field
// declared `List<Person>` is a field of class teyru.List to everything that
// reads it at run time, so a reader that has only the erased class cannot tell
// its elements from the container. The compiler still knows -- the declared
// type is in the AST it is emitting from -- so the element is written into the
// field's descriptor next to the type, and java.lang.reflect answers it as
// Field.getElementType (lib/26_reflect.teyru). The cost is one pointer per
// field in a static table that is already emitted, and no work at run time.
//
// A nested container is one level deep: `List<List<Person>>` names teyru.List
// for its elements, and the inner element type is erased again.
func (e *Emitter) elementTypeExpr(t ast.Type) string {
	elem := e.elementOf(t)
	if elem == nil {
		return "NULL"
	}
	return e.typeClassExpr(elem)
}

// emitFieldTable writes the field descriptors of a class and answers with the
// table's C name, or NULL when the class declares no field.
func (e *Emitter) emitFieldTable(cl *ast.Class) string {
	if len(cl.Fields) == 0 {
		return "NULL"
	}
	offs := e.fieldOffsets(cl)
	// The entries are collected before the table is written: an annotation
	// table written into the middle of an initializer below would be a
	// declaration inside braces, which is not C.
	var entries []string
	for _, f := range cl.Fields {
		offset, addr := "-1", "NULL"
		if f.Mods.Has(ast.ModStatic) {
			addr = "(void*)&" + staticName(cl, f)
		} else {
			offset = fmt.Sprint(offs[f])
		}
		anns, nannos := e.emitAnnoTable(mangle(cl.Full)+"_f_"+mangle(f.Name), f.Annos)
		entries = append(entries, fmt.Sprintf(
			"  {.name = %q, .type = %s, .owner = &cls_%s, .off = %s, .mods = %d, .addr = %s, .prim = %d, .annos = %s, .nannos = %d, .elem = %s},\n",
			f.Name, e.typeClassExpr(f.Type), mangle(cl.Full), offset, javaMods(f.Mods), addr, primKindOf(f.Type), anns, nannos,
			e.elementTypeExpr(f.Type)))
	}
	name := "fds_" + mangle(cl.Full)
	fmt.Fprintf(&e.meta, "static const tyfield %s[] = {\n%s};\n", name, strings.Join(entries, ""))
	e.attach(cl, "fields", name, "tyfield", len(cl.Fields), "nfields")
	return name
}

// invokerName is the C name of the function Method.invoke calls a method
// through. It is derived from the method's own symbol, so the two cannot drift
// and two methods that share a name get one invoker each.
func (e *Emitter) invokerName(m *ast.Method) string {
	return "inv" + strings.TrimPrefix(e.cfunc(m), "M")
}

// emitInvoker writes the function reflection calls a method through.
//
// One signature serves the whole program -- the receiver, an array of boxed
// arguments, a boxed result -- because that is the only shape Method.invoke has
// to work with. Everything the compiler knows about the method's real
// signature lives here and nowhere else: the argument conversions, the
// receiver, the dispatch when the method has an override, and the boxing of
// what comes back.
func (e *Emitter) emitInvoker(cl *ast.Class, m *ast.Method) {
	name := e.invokerName(m)

	// An inner or local class cannot be built reflectively: its constructor
	// takes the enclosing instance or the captured locals, and a caller of
	// Constructor.newInstance has no way to name either. Java has the same
	// shape -- the enclosing instance is the constructor's first parameter
	// there -- and says so rather than constructing half an object.
	fmt.Fprintf(&e.fns, "static void* %s(void* self, void** a);\n", name)
	// A lambda or anonymous class is built by the call site that wrote it,
	// with the captured environment the checker handed it; it has no C
	// constructor function at all, so there is nothing to call.
	if m.IsCtor && (cl.Inner || len(e.prog.CapturedVars(cl)) > 0 || strings.HasPrefix(cl.Name, "$")) {
		fmt.Fprintf(&e.meta,
			"static void* %s(void* self, void** a) {\n  (void)self; (void)a;\n  ty_throw(ty_make_ex(TY_UNSUP, %s));\n  return NULL;\n}\n\n",
			name, e.cstr("an inner or local class has no constructor reflection can call"))
		return
	}

	args := make([]string, 0, len(m.Params))
	for i, p := range m.Params {
		args = append(args, e.argFromBox(p, i))
	}

	if m.IsCtor {
		fmt.Fprintf(&e.meta, "static void* %s(void* self, void** a) {\n  (void)self;\n", name)
		fmt.Fprintf(&e.meta, "  %s* o = (%s*)ty_alloc(sizeof(%s));\n", cname(cl), cname(cl), cname(cl))
		fmt.Fprintf(&e.meta, "  o->obj.cls = &cls_%s;\n", mangle(cl.Full))
		if cl.ClInit != nil {
			fmt.Fprintf(&e.meta, "  ty_clinit(&cls_%s);\n", mangle(cl.Full))
		}
		sep := ""
		if len(args) > 0 {
			sep = ", "
		}
		fmt.Fprintf(&e.meta, "  %s(o%s%s);\n  return (void*)o;\n}\n\n", e.cfunc(m), sep, strings.Join(args, ", "))
		return
	}

	// A virtual method is called the way a call site calls it: through the
	// receiver's own class, so that an override runs. Java's Method.invoke is a
	// virtual call for the same reason.
	//
	// The function pointer is cast to the method's own signature rather than to
	// a generic one: an indirect call through a mismatched prototype is
	// undefined even when the arguments happen to land in the right registers.
	recv := ""
	if !m.IsStatic() {
		recv = cname(m.Owner) + "*"
		// Method.invoke with a null receiver answers what Java answers, rather
		// than dereferencing the null the vtable lookup would read.
		fmt.Fprintf(&e.meta, "static void* %s(void* self, void** a) {\n", name)
		fmt.Fprintf(&e.meta, "  if (self == NULL) ty_throw(ty_make_ex(TY_NPE, %s));\n",
			e.cstr("a method was invoked on a null receiver"))
	} else {
		fmt.Fprintf(&e.meta, "static void* %s(void* self, void** a) {\n  (void)self;\n", name)
	}
	var ps []string
	if recv != "" {
		ps = append(ps, recv)
	}
	for _, p := range m.Params {
		ps = append(ps, e.ctype(p))
	}
	if len(ps) == 0 {
		ps = append(ps, "void")
	}
	ret := e.ctype(m.Result)
	callArgs := strings.Join(args, ", ")
	if recv != "" {
		if callArgs != "" {
			callArgs = "(" + recv + ")self, " + callArgs
		} else {
			callArgs = "(" + recv + ")self"
		}
	}
	var call string
	switch {
	case m.IsStatic():
		call = e.cfunc(m) + "(" + callArgs + ")"
	case m.VIndex >= 0:
		call = fmt.Sprintf("((%s(*)(%s))((tyobj*)self)->cls->vtable[%d])(%s)", ret, strings.Join(ps, ", "), m.VIndex, callArgs)
	case m.Selector >= 0:
		call = fmt.Sprintf("((%s(*)(%s))ty_itab(self, %d))(%s)", ret, strings.Join(ps, ", "), m.Selector, callArgs)
	default:
		// final, private or a static initializer: no override can run, so the
		// call is direct.
		call = e.cfunc(m) + "(" + callArgs + ")"
	}
	e.emitInvokerBody(name, m, call)
}

func (e *Emitter) emitInvokerBody(name string, m *ast.Method, call string) {
	if isVoid(m.Result) {
		fmt.Fprintf(&e.meta, "  %s;\n  return NULL;\n}\n\n", call)
		return
	}
	if kind := primKindOf(m.Result); kind != 0 {
		fmt.Fprintf(&e.meta, "  %s r = %s;\n", e.ctype(m.Result), call)
		// The invoker hands the runtime an object, so a primitive result is
		// boxed here -- through the wrapper's valueOf, the same call an
		// autoboxing site in the program makes.
		fmt.Fprintf(&e.meta, "  return (void*)(%s);\n}\n\n", e.boxedValue(ast.PrimKind(kind), "r"))
		return
	}
	fmt.Fprintf(&e.meta, "  return (void*)(%s);\n}\n\n", call)
}

// argFn is the runtime's checked conversion for a primitive kind.
func argFn(kind int32) string {
	switch kind {
	case 1:
		return "ty_rv_bool"
	case 2:
		return "ty_rv_byte"
	case 3:
		return "ty_rv_short"
	case 4:
		return "ty_rv_char"
	case 5:
		return "ty_rv_int"
	case 6:
		return "ty_rv_long"
	case 7:
		return "ty_rv_float"
	}
	return "ty_rv_double"
}

// argFromBox converts one boxed argument into the type the method declares, the
// way Java converts it: a null or a foreign box for a primitive is an
// IllegalArgumentException, and so is a reference of the wrong class.
func (e *Emitter) argFromBox(p ast.Type, i int) string {
	idx := fmt.Sprintf("a[%d]", i)
	if k := primKindOf(p); k != 0 {
		return fmt.Sprintf("%s(%s)", argFn(k), idx)
	}
	return fmt.Sprintf("(%s)ty_rv_ref(%s, %s)", e.ctype(p), idx, e.typeClassExpr(p))
}

// emitMethodTable writes the method descriptors of a class, constructors
// included and marked as such, and answers with the table's C name.
//
// Java does not call a constructor a method: getDeclaredMethods leaves them out
// and getDeclaredConstructors returns nothing else. The compiler keeps one
// table with a kind on each entry and the runtime's accessors filter, because
// two tables would have to answer the same questions twice.
func (e *Emitter) emitMethodTable(cl *ast.Class) string {
	if len(cl.Methods) == 0 && len(cl.Ctors) == 0 {
		return "NULL"
	}
	var ms []*ast.Method
	for _, name := range mangleOrder(cl) {
		ms = append(ms, cl.Methods[name]...)
	}
	ms = append(ms, cl.Ctors...)

	// The parameter tables are written first: a table written into the middle
	// of the initializer below would be a declaration inside braces, which is
	// not C.
	// The tables of one method are keyed by its position in the list rather than
	// by its signature: the signature omits what an array's element type is, so
	// copyOf(int[], int) and copyOf(long[], int) have the same one.
	var entries []string
	for mi, m := range ms {
		e.emitInvoker(cl, m)
		params := "NULL"
		if len(m.Params) > 0 {
			params = "ps" + strings.TrimPrefix(e.invokerName(m), "inv")
			var types []string
			for _, p := range m.Params {
				types = append(types, e.typeClassExpr(p))
			}
			fmt.Fprintf(&e.meta, "static const tyclass* %s[] = {%s};\n", params, strings.Join(types, ", "))
		}
		kind := "TY_METH_INSTANCE"
		switch {
		case m.IsCtor:
			kind = "TY_METH_CTOR"
		case m.IsStatic():
			kind = "TY_METH_STATIC"
		}
		if m.Varargs {
			kind = "(int32_t)(" + kind + " | TY_METH_VARARGS)"
		}
		ret := e.typeClassExpr(m.Result)
		if m.IsCtor {
			ret = "&cls_" + mangle(m.Owner.Full)
		}
		suffix := fmt.Sprintf("%s_m%d", mangle(cl.Full), mi)
		anns, nannos := e.emitAnnoTable(suffix, methodAnnos(m))
		// a parameter's own annotations, which is where @Value and @Autowired
		// on a constructor or method parameter are written
		pannos, pnannos, pnames := "NULL", "NULL", "NULL"
		if len(m.ParamNames) > 0 {
			var names []string
			for _, n := range m.ParamNames {
				names = append(names, e.cstr(n))
			}
			pnames = "pnames_" + suffix
			fmt.Fprintf(&e.meta, "static const char *const %s[] = {%s};\n", pnames, strings.Join(names, ", "))
		}
		if len(m.ParamAnnos) > 0 {
			var lists, counts []string
			for i := range m.Params {
				name, n := "NULL", 0
				if i < len(m.ParamAnnos) {
					name, n = e.emitAnnoTable(fmt.Sprintf("%s_p%d", suffix, i), m.ParamAnnos[i])
				}
				lists = append(lists, name)
				counts = append(counts, fmt.Sprint(n))
			}
			pannos = "pannos_" + suffix
			pnannos = "pnannos_" + suffix
			fmt.Fprintf(&e.meta, "static const tyannotation *const %s[] = {%s};\n", pannos, strings.Join(lists, ", "))
			fmt.Fprintf(&e.meta, "static const int32_t %s[] = {%s};\n", pnannos, strings.Join(counts, ", "))
		}
		entries = append(entries, fmt.Sprintf(
			"  {.name = %q, .fn = (void*)%s, .owner = &cls_%s, .ret = %s, .params = %s, .nparams = %d, .mods = %d, .kind = %s, .primret = %d, .annos = %s, .nannos = %d, .pannos = %s, .pnannos = %s, .pnames = %s},\n",
			m.Name, e.invokerName(m), mangle(m.Owner.Full), ret, params, len(m.Params),
			javaMods(m.Mods), kind, primKindOf(m.Result), anns, nannos, pannos, pnannos, pnames))
	}
	name := "mds_" + mangle(cl.Full)
	fmt.Fprintf(&e.meta, "static const tymethod %s[] = {\n%s};\n", name, strings.Join(entries, ""))
	e.attach(cl, "methods", name, "tymethod", len(entries), "nmethods")
	return name
}

// emitConstTable writes the enum constants of a class, which is what
// Class.getEnumConstants answers with.
func (e *Emitter) emitConstTable(cl *ast.Class) string {
	if len(cl.EnumConsts) == 0 {
		return "NULL"
	}
	// Unlike the member tables this one ships unconditionally, and it can: it
	// is filled by the class's own static initializer, which is the only place
	// the constants' addresses are known, and it names nothing but the class's
	// own constants -- so it pulls in no class a program was not already
	// holding.
	name := "cts_" + mangle(cl.Full)
	fmt.Fprintf(&e.data, "static void* %s[%d];\n", name, len(cl.EnumConsts))
	return name
}

// emitClassRegistry writes the table Class.forName searches. It holds every
// class of the program: the set of names a program can be asked for is known
// when it is compiled, and the table costs one pointer per class.
// forNameTable writes the program's class table inside the function that
// searches it, and returns the C name and the count.
//
// It is a function-local static rather than a global on purpose. An array at
// file scope that names every class is a root: it pins every class, its
// vtable, its interface table and its member tables into every executable,
// including one that never loads a class by name. Written here, link-time
// optimisation drops the table along with the function -- and the function is
// reachable only from a call site that asked for forName.
func (e *Emitter) forNameTable() (string, int) {
	names := make([]string, 0, len(e.prog.Classes))
	for _, cl := range e.prog.Classes {
		if cl == nil {
			continue
		}
		// Every class, including the ones whose struct is a runtime typedef: a
		// program loads teyru.String by name as readily as its own types.
		names = append(names, "&cls_"+mangle(cl.Full))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "NULL", 0
	}
	name := "forname_all"
	// The table is marked reflectOnly for the same reason an attachment is: it
	// names every class of the program, and it does so for the reflective call
	// that has not happened yet, not because the program's own code names them.
	fmt.Fprintf(e.code, "static tyclass* %s[%d] = {%s}; %s\n",
		name, len(names), strings.Join(names, ", "), reflectOnly)
	return name, len(names)
}

// attach records the assignment that hands a class its table at startup. The
// class object itself is written with a null table, because whether the table
// ships is not known until the program's call sites have been emitted.
//
// The line is marked reflectOnly: it is the one place a class is named because
// reflection may ask for it, and what it names -- the class, and the member
// table that names every method's invoker -- is what a *reflective* call can
// reach rather than what the program's own code calls. codegen/prune.go reads
// the mark when it decides whether the program uses TLS.
func (e *Emitter) attach(cl *ast.Class, field, table, elem string, n int, count string) {
	e.metaInit = append(e.metaInit, fmt.Sprintf("cls_%s.%s = (%s*)%s; cls_%s.%s = %d; %s",
		mangle(cl.Full), field, elem, table, mangle(cl.Full), count, n, reflectOnly))
}

// reflectMethodCount is how many entries a class's method table holds: every
// method by name, then its constructors, in the order emitMethodTable writes
// them.
func (e *Emitter) reflectMethodCount(cl *ast.Class) int {
	n := 0
	for _, name := range mangleOrder(cl) {
		n += len(cl.Methods[name])
	}
	return n + len(cl.Ctors)
}

// reflectClassMembers are the Class methods whose answers come from a class's
// member tables. Asking a class its name, its modifiers, its superclass or its
// interfaces reads fields that every class object already carries, so those
// calls do not make a program a reflection user.
var reflectClassMembers = map[string]bool{
	"getDeclaredFields": true, "getFields": true,
	"getDeclaredField": true, "getField": true,
	"getDeclaredMethods": true, "getMethods": true,
	"getDeclaredMethod": true, "getMethod": true,
	"getDeclaredConstructors": true, "getConstructors": true,
	"getDeclaredConstructor": true, "getConstructor": true,
	"newInstance": true, "getEnumConstants": true,
	"forName": true, "forNameOf": true,
}

// reflectionCall reports whether a call to m can reach a member table, which is
// what decides whether the tables ship. The member classes are reflection's own,
// so any call to one of them is a reflection user; Class is not, so it is asked
// by name.
// A call written inside the standard library does not count: the library's own
// Class, Field and Array methods call each other, so counting them would make
// every program a reflection user. What the gate asks is whether a program the
// user wrote can reach a member table.
func (e *Emitter) reflectionCall(m *ast.Method) bool {
	if m == nil {
		return false
	}
	if e.curClass != nil && strings.HasPrefix(e.curClass.Full, "teyru.") {
		return false
	}
	switch m.Owner.Name {
	case "Field", "Method", "Constructor", "Array":
		return true
	case "Class":
		return reflectClassMembers[m.Name]
	case "Gson", "Application", "ApplicationContext", "BeanRegistry":
		// The library reads its own annotations and its classes' members through
		// reflection now, so a program that calls into it is a program that
		// needs the metadata -- the calls inside the library do not count, since
		// they are prelude-internal.
		return true
	}
	return false
}

// programUsesAnnotations reports whether the program annotates its own
// declarations with a library annotation. Such a program's annotations are
// read at run time by the library, so the metadata has to be there even when no
// call site in the program names the reflection API.
//
// Lombok's annotations are excluded: the compiler consumes those while
// checking, and nothing reads them afterwards.
func (e *Emitter) programUsesAnnotations() bool {
	fromLib := func(annos []*ast.Annotation) bool {
		for _, a := range annos {
			cl := e.annoClass(a)
			if cl == nil || cl.Kind != ast.KindAnnotation {
				continue
			}
			if strings.HasPrefix(cl.Full, "teyru.") && !lombokAnnotations[cl.Name] {
				return true
			}
		}
		return false
	}
	for _, cl := range e.prog.Classes {
		if cl == nil || strings.HasPrefix(cl.Full, "teyru.") {
			continue
		}
		if fromLib(annoListOf(cl)) {
			return true
		}
		for _, f := range cl.Fields {
			if fromLib(f.Annos) {
				return true
			}
		}
		for _, ms := range cl.Methods {
			for _, m := range ms {
				if fromLib(methodAnnos(m)) {
					return true
				}
			}
		}
		for _, m := range cl.Ctors {
			if fromLib(methodAnnos(m)) {
				return true
			}
		}
	}
	return false
}

// lombokAnnotations are the annotations the compiler consumes while checking, so
// nothing reads them at run time and a program that uses only these needs no
// annotation metadata.
var lombokAnnotations = map[string]bool{
	"Data": true, "Value": true, "Getter": true, "Setter": true, "Builder": true,
	"AllArgsConstructor": true, "NoArgsConstructor": true, "RequiredArgsConstructor": true,
	"ToString": true, "EqualsAndHashCode": true, "NonNull": true, "SneakyThrows": true,
	"Cleanup": true, "Synchronized": true, "Log": true, "Log4j2": true, "Slf4j": true,
	"CommonsLog": true, "Flogger": true, "JBossLog": true, "XSlf4j": true, "CustomLog": true,
	"UtilityClass": true, "FieldDefaults": true, "FieldNameConstants": true, "Accessors": true,
	"Wither": true, "Experimental": true, "StandardException": true, "Helper": true,
	"PackagePrivate": true, "NonFinal": true, "Tolerate": true, "Singular": true,
	"Jacksonized": true, "SuperBuilder": true, "ExtensionMethod": true, "FieldNamingStrategy": true,
	"EqualsAndHashCodeExclude": true, "With": true, "WithBy": true, "ValueExclude": true,
}

// ---------------------------------------------------------------- annotations

// The kinds an annotation element's value is carried as. They are the TY_ANN_*
// macros tyrt.h defines.
const (
	annUnsupported = 0
	annString      = 1
	annInt         = 2
	annLong        = 3
	annDouble      = 4
	annBool        = 5
	annClass       = 6
	annEnum        = 7
)

// annoClass resolves the annotation type a written annotation names. A program
// writes @Component, whose class the checker has already found; the name may be
// simple or qualified, and an annotation from another package is written with
// its package.
func (e *Emitter) annoClass(a *ast.Annotation) *ast.Class {
	if cl := e.prog.LookupClass(a.Name); cl != nil {
		return cl
	}
	return e.prog.ProgramClass(a.Name)
}

// annoListOf answers the annotations of a class or method declaration, which is
// where they are written.
func annoListOf(cl *ast.Class) []*ast.Annotation {
	if cl == nil || cl.Decl == nil {
		return nil
	}
	return cl.Decl.Annos
}

func methodAnnos(m *ast.Method) []*ast.Annotation {
	if m == nil || m.Decl == nil {
		return nil
	}
	return m.Decl.Annos
}

// emitAnnoTable writes the annotations written on one element and answers with
// the table's C name, or NULL when the element carries none.
//
// An annotation is its type and its elements, and an element value is a string,
// a primitive, a class, an enum constant or an array of those. Everything but
// the array is carried; an element this cannot carry is recorded as unsupported
// rather than dropped, so reading it fails where it is read instead of
// answering nothing.
func (e *Emitter) emitAnnoTable(suffix string, annos []*ast.Annotation) (string, int) {
	var entries []string
	for i, a := range annos {
		cl := e.annoClass(a)
		if cl == nil {
			continue
		}
		args := make([]string, 0, len(a.Args))
		written := map[string]bool{}
		for _, arg := range a.Args {
			args = append(args, e.annoArgC(arg))
			written[annoArgName(arg)] = true
		}
		// An element a use did not write answers its declared default, which is
		// what Java requires: the annotation interface declares them and a
		// reader asks without knowing whether it was written.
		for _, el := range annoElements(cl) {
			if written[el.Name] || el.Decl == nil || el.Decl.Default == nil {
				continue
			}
			args = append(args, e.annoArgC(&ast.AnnoArg{Name: el.Name, Value: el.Decl.Default}))
		}
		argsName := "NULL"
		if len(args) > 0 {
			argsName = fmt.Sprintf("annarg_%s_%d", suffix, i)
			fmt.Fprintf(&e.meta, "static const tyannoarg %s[] = {\n%s};\n", argsName, strings.Join(args, ""))
		}
		entries = append(entries, fmt.Sprintf("  {.type = &cls_%s, .args = %s, .nargs = %d},\n",
			mangle(cl.Full), argsName, len(args)))
	}
	if len(entries) == 0 {
		return "NULL", 0
	}
	name := "anns_" + suffix
	fmt.Fprintf(&e.meta, "static const tyannotation %s[] = {\n%s};\n", name, strings.Join(entries, ""))
	return name, len(entries)
}

// annoArgName is the element name an argument stands for: an argument written
// without a name is the element `value`, which is Java's rule for a lone one.
func annoArgName(arg *ast.AnnoArg) string {
	if arg.Name == "" {
		return "value"
	}
	return arg.Name
}

// annoElements answers an annotation type's elements, which are the methods its
// interface declares.
func annoElements(cl *ast.Class) []*ast.Method {
	var out []*ast.Method
	for _, name := range mangleOrder(cl) {
		out = append(out, cl.Methods[name]...)
	}
	return out
}

// classLitClass resolves the class a written class literal names.
func (e *Emitter) classLitClass(lit *ast.ClassLit) *ast.Class {
	if lit.Type == nil {
		return nil
	}
	if ct, ok := lit.Type.Resolved.(*ast.ClassType); ok && ct.Class != nil {
		return ct.Class
	}
	if cl := e.prog.LookupClass(lit.Type.Name); cl != nil {
		return cl
	}
	return e.prog.ProgramClass(lit.Type.Name)
}

// enumArg renders an enum constant as an annotation element value: its name, and
// the class to compare it against. An element that is not an enum constant, or
// an array, is unsupported rather than dropped, so reading it says so.
func (e *Emitter) enumArg(f *ast.Field) (int, string, string) {
	if f == nil || f.Owner == nil || f.Owner.Kind != ast.KindEnum {
		return annUnsupported, "NULL", "NULL"
	}
	return annEnum, e.cstr(f.Name), "&cls_" + mangle(f.Owner.Full)
}

// annoArgC renders one annotation element as a tyannoarg initializer.
func (e *Emitter) annoArgC(arg *ast.AnnoArg) string {
	name := annoArgName(arg)
	kind := annUnsupported
	ival, dval, sval, cval := "0", "0", "NULL", "NULL"
	switch v := arg.Value.(type) {
	case *ast.Literal:
		switch v.Kind {
		case ast.LitString:
			kind, sval = annString, e.cstr(v.Str)
		case ast.LitInt:
			kind, ival = annInt, fmt.Sprint(int64(v.Int))
		case ast.LitLong:
			kind, ival = annLong, fmt.Sprintf("%dLL", int64(v.Int))
		case ast.LitFloat, ast.LitDouble:
			kind, dval = annDouble, util.FloatLiteral(v.Flt, v.Kind == ast.LitFloat)
		case ast.LitBool:
			kind = annBool
			if v.Bool {
				ival = "1"
			}
		case ast.LitChar:
			kind, ival = annInt, fmt.Sprint(int64(v.Int))
		}
	case *ast.ClassLit:
		// An annotation argument is read as text while checking, so the class
		// literal in it is usually unresolved and the type is found by the name
		// that was written -- the same reason the enum case below resolves by
		// name. An unresolved literal would otherwise carry the annotation's own
		// default (Object.class) and a handler would match every exception.
		if cl := e.classLitClass(v); cl != nil {
			kind, cval = annClass, "(tyclass*)&cls_"+mangle(cl.Full)
		}
	case *ast.Select:
		// An annotation argument is read as text while checking -- the passes
		// that consume annotations want the names, not the types -- so the
		// resolved field is usually not there and the enum is found by the name
		// that was written instead.
		if f, ok := v.Ref.(*ast.Field); ok {
			kind, sval, cval = e.enumArg(f)
		} else if id, isIdent := v.X.(*ast.Ident); isIdent {
			if ec := e.prog.ProgramClass(id.Name); ec != nil && ec.Kind == ast.KindEnum {
				kind, sval, cval = annEnum, e.cstr(v.Name), "&cls_"+mangle(ec.Full)
			}
		}
	case *ast.Ident:
		// An enum constant of an enum in the same file may be written without
		// its type name, and the checker resolves it to the same field.
		if f, ok := v.Ref.(*ast.Field); ok {
			kind, sval, cval = e.enumArg(f)
		}
	}
	return fmt.Sprintf("  {.name = %q, .kind = %d, .ival = %s, .dval = %s, .sval = %s, .cval = %s},\n",
		name, kind, ival, dval, sval, cval)
}
