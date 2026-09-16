package codegen

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/util"
)

// cstr returns a C string literal for s.
func (e *Emitter) cstr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString("\\\"")
		case c == '\\':
			b.WriteString("\\\\")
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		case c < 32 || c > 126:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tmpRef materialises an expression into a statement-expression so it can be
// referenced more than once without double evaluation.
func (e *Emitter) tmpRef(x string) string {
	n := e.tmpName()
	return "({ __typeof__(" + x + ") " + n + " = " + x + "; " + n + "; })"
}

// cond renders a boolean condition.
// cond renders an expression in a boolean context: if, while, do, for, the
// ternary's test, and assert.
//
// A boxed Boolean has to be unboxed here. Everywhere else the target type is
// known and coerce() does it, but a condition has no declared type to convert
// to -- `Boolean b = false; if (b)` reached C as a pointer test, and a non-null
// Boolean is true whatever it holds, so the branch went the wrong way.
func (e *Emitter) cond(x ast.Expr) string {
	v := e.expr(x)
	ct, ok := x.GetType().(*ast.ClassType)
	if !ok {
		return v
	}
	if k, ok := e.prog.Builtins.Unbox[ct.Class]; ok && k == ast.Boolean {
		return e.unboxCall(v, ct, ast.TBoolean)
	}
	return v
}

// refExpr renders an expression that is being used as an object reference.
func (e *Emitter) refExpr(x ast.Expr) string {
	return e.expr(x)
}

// coerce converts an emitted value from src to dst type.
func (e *Emitter) coerce(v string, src, dst ast.Type) string {
	if src == nil || dst == nil || dst == ast.TVoid || ast.IsError(src) || ast.IsError(dst) {
		return v
	}
	// generics are erased at runtime
	if _, ok := dst.(*ast.TypeVarType); ok {
		dst = e.prog.Erased(dst)
	}
	if _, ok := src.(*ast.TypeVarType); ok {
		src = e.prog.Erased(src)
	}
	_, sp := src.(*ast.PrimType)
	_, dp := dst.(*ast.PrimType)
	switch {
	case sp && dp:
		// C handles the numeric conversions implicitly, and for every pair but
		// one that is also what Java means. The exception is a floating-point
		// value converted to an integral one: a value that fits is the same
		// truncation in both languages, but a value that does not is undefined
		// in C. Java only allows that conversion behind a cast, and the cast is
		// what this function is handed for one; a conversion the language
		// permits without one -- a compound assignment, a constant that fits --
		// arrives here, and it clamps for the same reason.
		if conv := e.saturatingConv(src.(*ast.PrimType), dst.(*ast.PrimType), v); conv != "" {
			return conv
		}
		return v // C handles the other numeric conversions implicitly
	case sp && !dp:
		return e.boxCall(v, src.(*ast.PrimType), dst)
	case !sp && dp:
		return e.unboxCall(v, src, dst.(*ast.PrimType))
	case !sp && !dp:
		if e.isRef(src) && e.isRef(dst) {
			return "(" + e.ctype(dst) + ")" + v
		}
	}
	return v
}

// classLiteralTarget is the C tyclass a class literal's Class object names.
func (e *Emitter) classLiteralTarget(v *ast.ClassLit) string {
	switch t := v.Type.Resolved.(type) {
	case *ast.ClassType:
		return "&cls_" + mangle(t.Class.Full)
	case *ast.PrimType:
		// A primitive type is not one of the program's classes and no value is
		// ever an instance of it, so its class object names a class emitted for
		// the literal itself (emitPrimClassMeta). Naming the wrapper class
		// instead would answer "teyru.Integer" for int.class and make it equal
		// to Integer.class, which Java keeps apart.
		e.primClasses[t.Kind] = true
		return "&cls_" + primClassName(t.Kind)
	case *ast.ArrayType:
		// Every array value is an instance of the one synthetic teyru.Array
		// class, so that is the class `A[].class` denotes; naming a class per
		// element type, as Java does, would make the literal a class no array
		// is an instance of.
		if a := e.prog.ArrayClass(); a != nil {
			return "&cls_" + mangle(a.Full)
		}
	}
	return "&cls_" + mangle(e.prog.Builtins.Object.Full)
}

// classObject renders a class literal as the Class object it denotes: the same
// kind of value Object.getClass() returns, built by the same runtime helper and
// equal to it, since the wrapper is cached per class.
//
// The literal used to render as the raw tyclass handle `(tyobj*)&cls_A`, which
// every other consumer reads as an object: the class's *name* sits exactly
// where an object keeps its class pointer, so println dispatched on the name
// and jumped through it.
//
// ty_class_of_cls wraps the class of the object it is handed, so the named
// tyclass rides in the header of a throwaway object -- a compound literal,
// whose address is good for the enclosing statement expression. The second
// argument is this program's Class, which the runtime needs to make the wrapper
// an instance of it; dispatch, instanceof and casts then work as for any other
// object.
func (e *Emitter) classObject(v *ast.ClassLit) string {
	cls := e.classLiteralTarget(v)
	wrap, ok := v.GetType().(*ast.ClassType)
	if !ok {
		// The checker types every class literal as the prelude's Class
		// (sema.classLiteralType); rendering the bare handle keeps an internal
		// invariant from becoming a panic, which the compiler path forbids.
		return "((tyobj*)" + cls + ")"
	}
	return "({ extern " + classOfProto + "; ty_class_of_cls(&(tyobj){(tyclass*)" + cls +
		"}, (void*)&cls_" + mangle(wrap.Class.Full) + "); })"
}

func (e *Emitter) boxCall(v string, p *ast.PrimType, dst ast.Type) string {
	call := e.boxedValue(p.Kind, v)
	if call == "" {
		return v
	}
	// The formal a boxed argument is passed to is the *erased* one: an
	// unbounded type variable erases to Object, a bounded one to its bound.
	// coerce has already erased dst, so what arrives here is that reference
	// type -- `Number` for `class Bag<T extends Number>` -- and not the box
	// class the call site substituted. Boxing to the argument's own wrapper
	// and casting to the erasure is what `M_Bag__init__0(C_Bag*,
	// C_teyru_Number*)` expects; without the cast `new Bag<Integer>(7)`
	// passes a bare `int` where the pointer goes.
	switch d := dst.(type) {
	case *ast.ClassType:
		return "(" + cname(d.Class) + "*)" + call
	case *ast.TypeVarType:
		return "(void*)" + call
	}
	return v
}

// boxedValue renders `v`, a primitive of kind k, as the wrapper object the
// prelude's Xxx.valueOf builds, and "" when the program has no wrapper for the
// kind. It is the C back end's whole boxing story: where the emitter used to
// call ty_box_int and its seven siblings in the runtime, it now calls the
// method the language spells out in lib/04_boxing.teyru, so the wrapper's
// value is the class's own field and nothing about it is a runtime secret.
//
// The class is initialized first, which is what a static call does anywhere
// else; a wrapper whose statics are all constants needs no guard and gets none
// (clinitCall answers "").
func (e *Emitter) boxedValue(k ast.PrimKind, v string) string {
	return e.wrapperCall(e.wrapperStatic(k, "valueOf"), v)
}

// wrapperStatic is the static method of a primitive's wrapper class that takes
// one parameter of that primitive: valueOf for boxing, hashCode for the
// synthesized record hash. nil when the program has no wrapper for the kind.
func (e *Emitter) wrapperStatic(k ast.PrimKind, name string) *ast.Method {
	cl := e.prog.Builtins.Boxes[k]
	if cl == nil {
		return nil
	}
	for _, m := range cl.Methods[name] {
		if !m.IsStatic() || len(m.Params) != 1 {
			continue
		}
		if p, ok := m.Params[0].(*ast.PrimType); ok && p.Kind == k {
			return m
		}
	}
	return nil
}

// wrapperCall renders a call to a prelude method the emitter itself makes,
// initializing the class first as any other static call site does, and "" for
// a method the program does not have.
func (e *Emitter) wrapperCall(m *ast.Method, args string) string {
	if m == nil {
		return ""
	}
	return "(" + e.clinitCall(m.Owner) + e.cfunc(m) + "(" + args + "))"
}

func (e *Emitter) unboxCall(v string, src ast.Type, p *ast.PrimType) string {
	if ct, ok := src.(*ast.ClassType); ok {
		kind, ok2 := e.prog.Builtins.Unbox[ct.Class]
		if ok2 {
			if m := e.unboxAccessor(ct.Class, kind); m != nil {
				// The wrapper's own accessor reads the box, which is what an
				// unboxing conversion means; a destination of another primitive
				// type is the conversion from that value, so `double d = anInt`
				// is (double) anInt.intValue() and not a look at the int's
				// bytes as a double.
				//
				// A null reference raises here, where Java raises it: the
				// generated call cannot test the receiver the way a call site
				// does (the accessor reads its own field, and `this` is null
				// only for a call the generator bound without a test), so the
				// test is written into the statement expression.
				n := e.tmpName()
				call := "({ " + cname(ct.Class) + "* " + n + " = (" + cname(ct.Class) + "*)" + v + "; " +
					n + " ? " + e.cfunc(m) + "(" + n + ") : (" + e.ctype(&ast.PrimType{Kind: kind}) +
					")(intptr_t)ty_npe(); })"
				if kind == p.Kind {
					return call
				}
				return "((" + e.ctype(p) + ")(" + call + "))"
			}
		}
	}
	return v
}

// unboxAccessor is the wrapper method that reads a box of its own kind: the
// intValue/longValue/... Java's Number declares and each wrapper implements.
func (e *Emitter) unboxAccessor(cl *ast.Class, k ast.PrimKind) *ast.Method {
	name := ""
	switch k {
	case ast.Boolean:
		name = "booleanValue"
	case ast.Byte:
		name = "byteValue"
	case ast.Short:
		name = "shortValue"
	case ast.Char:
		name = "charValue"
	case ast.Int:
		name = "intValue"
	case ast.Long:
		name = "longValue"
	case ast.Float:
		name = "floatValue"
	case ast.Double:
		name = "doubleValue"
	default:
		return nil
	}
	for _, m := range cl.Methods[name] {
		if !m.IsStatic() && len(m.Params) == 0 {
			return m
		}
	}
	return nil
}

// ---------------------------------------------------------------- expressions

func (e *Emitter) expr(x ast.Expr) string {
	if x == nil {
		return "0"
	}
	if v, ok := e.prog.Lowered(x); ok {
		return e.expr(v)
	}
	switch v := x.(type) {
	case *ast.Literal:
		return e.literal(v)
	case *ast.Ident:
		return e.ident(v)
	case *ast.Select:
		return e.selectExpr(v)
	case *ast.Index:
		return e.indexExpr(v)
	case *ast.Call:
		if v.ThisCtor {
			return ""
		}
		return e.callExpr(v)
	case *ast.New:
		return e.newExpr(v)
	case *ast.NewArray:
		return e.newArray(v)
	case *ast.ArrayInit:
		return e.arrayInit(v)
	case *ast.Unary:
		return e.unary(v)
	case *ast.Binary:
		return e.binary(v)
	case *ast.Assign:
		return e.assign(v)
	case *ast.Cond:
		return "(" + e.cond(v.C) + " ? " + e.coerce(e.expr(v.X), v.X.GetType(), v.GetType()) +
			" : " + e.coerce(e.expr(v.Y), v.Y.GetType(), v.GetType()) + ")"
	case *ast.Cast:
		return e.cast(v)
	case *ast.InstanceOf:
		return e.instanceOf(v)
	case *ast.Conv:
		return e.coerce(e.expr(v.X), v.X.GetType(), v.GetType())
	case *ast.This:
		if v.Qual == "" {
			// A synthesized node (the Lombok pass builds `this` by hand) can
			// reach here without a type. The cast narrows the instance to the
			// static type of this use; when there is none to narrow to, the
			// instance itself is the answer -- `((void)this)` is not.
			if t := v.GetType(); t == nil || t == ast.TVoid {
				return e.thisExpr()
			}
			return "((" + e.ctype(v.GetType()) + ")" + e.thisExpr() + ")"
		}
		if cl := e.prog.LookupClass(v.Qual); cl != nil {
			return "(" + cname(cl) + "*)" + e.outerAccess(cl)
		}
		return "((void*)" + e.thisExpr() + ")"
	case *ast.SuperExpr:
		return "((void*)" + e.thisExpr() + ")"
	case *ast.ClassLit:
		return e.classObject(v)
	case *ast.Lambda:
		return e.lambdaExpr(v)
	case *ast.MethodRef:
		if v.Lam != nil {
			return e.lambdaExpr(v.Lam)
		}
		return "NULL"
	case *ast.SwitchExpr:
		n := e.tmpName()
		ct := e.ctype(v.GetType())
		inner := e.capture(func() {
			e.line("%s %s = %s;\n", ct, n, zeroOf(ct))
			e.switchStmt(v.S, n)
		})
		return "({ " + inner + " " + n + "; })"
	}
	return "0"
}

func (e *Emitter) literal(v *ast.Literal) string {
	switch v.Kind {
	case ast.LitInt:
		i := int32(v.Int)
		if i == -2147483648 {
			return "(-2147483647 - 1)"
		}
		return fmt.Sprintf("%d", i)
	case ast.LitLong:
		return fmt.Sprintf("%dLL", int64(v.Int))
	case ast.LitFloat:
		return util.FloatLiteral(v.Flt, true)
	case ast.LitDouble:
		return util.FloatLiteral(v.Flt, false)
	case ast.LitChar:
		return fmt.Sprintf("((uint16_t)%d)", uint16(v.Int))
	case ast.LitString:
		return e.strLit(v.Str)
	case ast.LitBool:
		if v.Bool {
			return "1"
		}
		return "0"
	case ast.LitNull:
		return "NULL"
	}
	return "0"
}

// strLit interns a string literal into a static global.
func (e *Emitter) strLit(s string) string {
	if id, ok := e.strings[s]; ok {
		return fmt.Sprintf("((tystr*)&S%d)", id)
	}
	id := len(e.strOrder)
	e.strings[s] = id
	e.strOrder = append(e.strOrder, s)
	data := e.cstr(s)
	fmt.Fprintf(&e.data, "static tystr S%d = {{&cls_%s}, %d, %s};\n", id,
		mangle(e.prog.Builtins.String.Full), len(s), data)
	return fmt.Sprintf("((tystr*)&S%d)", id)
}

func (e *Emitter) ident(v *ast.Ident) string {
	switch r := v.Ref.(type) {
	case *ast.Field:
		if r == nil {
			return "0"
		}
		// a static final constant is inlined at every use site
		if r.Mods.Has(ast.ModStatic) && r.ConstVal != nil {
			if s, ok := e.prog.ConstantString(r); ok {
				return e.strLit(s)
			}
			if lit, ok := e.prog.ConstantLiteral(r); ok {
				return lit
			}
		}
		if e.curClass != nil && r.Owner != nil && r.Owner != e.curClass && !r.Mods.Has(ast.ModStatic) {
			return e.outerFieldAccess(r)
		}
		return e.fieldAccess(r, e.thisExpr())
	case *ast.Var:
		if r == nil {
			return "0"
		}
		if r.Field != nil {
			// the `field` of a property accessor: a static property's storage
			// is a global, and its accessor is a static method with no `this`
			// to reach it through
			return e.storageOf(r.Field)
		}
		return e.localName(r)
	}
	return "0"
}

// storageOf is the C expression naming a field's storage where it is directly
// reachable: through `this` for an instance field, by its global for a static
// one. The static case is what a static property's accessor needs, since it has
// no `this` -- and the class's own initializer has already run by then, so the
// global needs no ty_clinit call of its own.
func (e *Emitter) storageOf(f *ast.Field) string {
	if f.Mods.Has(ast.ModStatic) {
		return staticName(f.Owner, f)
	}
	return "this->f_" + mangle(f.Name)
}

// thisExpr is the C expression for the instance the code being emitted runs
// on. In a lambda body that is not the closure object: Java's `this` inside a
// lambda is the instance the lambda was created in (JLS 15.27.2), and the
// closure reaches that instance through the field it captured for it. An
// unqualified call, a bare field name and `this` itself all go through here,
// so none of them can land on the lambda object by accident.
func (e *Emitter) thisExpr() string {
	if f := e.capThisField(e.curLambda); f != nil {
		return "this->cap_" + mangle(f.Name)
	}
	return "this"
}

// capThisField is the field a lambda holds the instance its body calls `this`,
// or nil when the body does not need one. The checker adds it as a capture
// under the name `this` when the body uses the enclosing instance, and the
// capture mechanism gives it the struct slot `cap_this`.
func (e *Emitter) capThisField(lam *ast.Lambda) *ast.Field {
	if lam == nil || !lam.CapThis || lam.Class == nil {
		return nil
	}
	for _, f := range lam.Class.CapFields {
		if f.Name == "this" {
			return f
		}
	}
	return nil
}

// enclosureOf is the class of the instance a lambda body's `this` denotes, or
// nil when the body does not use the enclosing instance.
func (e *Emitter) enclosureOf(lam *ast.Lambda) *ast.Class {
	f := e.capThisField(lam)
	if f == nil {
		return nil
	}
	if ct, ok := f.Type.(*ast.ClassType); ok {
		return ct.Class
	}
	return nil
}

// outerAccess walks the enclosing-instance chain to the class that owns an
// outer object, for references from a nested or inner class.
func (e *Emitter) outerAccess(target *ast.Class) string {
	recv := e.thisExpr()
	for cl := e.curClass; cl != nil && cl != target; cl = cl.Outer {
		if cl.OuterField == nil || cl.Outer == nil {
			return "((void*)" + e.thisExpr() + ")"
		}
		recv = "((" + cname(cl.Outer) + "*)" + recv + "->f_" + mangle(cl.OuterField.Name) + ")"
	}
	return recv
}

// outerFieldAccess reads a field that belongs to an enclosing class.
func (e *Emitter) outerFieldAccess(f *ast.Field) string {
	return "((" + cname(f.Owner) + "*)" + e.outerAccess(f.Owner) + ")->f_" + mangle(f.Name)
}

// nativeIsFinal reports whether the runtime helper a native method maps to is
// the implementation that will actually run.
//
// Object.toString, hashCode and equals are native, but they are also the three
// methods a class is most likely to override. Calling the runtime helper
// directly for a receiver whose static type is Object would skip the override,
// so an overridable method of a class that has subclasses is dispatched through
// the vtable instead, and the vtable slot holds the runtime wrapper.
func (e *Emitter) nativeIsFinal(m *ast.Method) bool {
	if m == nil {
		return true
	}
	if m.IsStatic() || m.Mods.Has(ast.ModFinal) || m.Mods.Has(ast.ModPrivate) {
		return true
	}
	// a receiver typed as a class with no subclasses cannot dispatch anywhere else
	return m.Owner == nil || len(m.Owner.Subclasses) == 0
}

// clinitCall initializes a class before its static state is touched, matching
// Java's lazy class initialization. The initialized flag is tested inline, so
// the common case is a load and a branch instead of a call, and a class whose
// hierarchy has no initializer at all needs no code.
func (e *Emitter) clinitCall(cl *ast.Class) string {
	if cl == nil || cl == e.prog.Builtins.Object || !needsClinit(cl, map[*ast.Class]bool{}) {
		return ""
	}
	return "(void)((cls_" + mangle(cl.Full) + ".flags & TY_CLS_INIT) ? 0 : ty_clinit(&cls_" + mangle(cl.Full) + ")), "
}

// clinitStmt is clinitCall as a statement, for the places that initialise a
// class before allocating one of its instances.
func (e *Emitter) clinitStmt(cl *ast.Class) string {
	if cl == nil || !needsClinit(cl, map[*ast.Class]bool{}) {
		return ""
	}
	n := mangle(cl.Full)
	return "if (!(cls_" + n + ".flags & TY_CLS_INIT)) ty_clinit(&cls_" + n + ");\n"
}

// needsClinit reports whether a class or any class it extends starts with a
// static initializer that has to run.
func needsClinit(cl *ast.Class, seen map[*ast.Class]bool) bool {
	if cl == nil || seen[cl] {
		return false
	}
	seen[cl] = true
	if cl.ClInit != nil {
		return true
	}
	if cl.Super != nil && needsClinit(cl.Super.Class, seen) {
		return true
	}
	for _, i := range cl.Ifaces {
		if needsClinit(i.Class, seen) {
			return true
		}
	}
	return false
}

// fieldRead reads an instance field through a receiver expression. Java reads
// the receiver before the field, so a null one has to answer with a
// NullPointerException instead of a load from address zero: the receiver is
// bound once, tested, and only then dereferenced. The test is a compare and a
// branch the compiler folds away when it can see the receiver is not null, and
// that the branch predictor answers the same way when it cannot.
//
// A read of an implicit `this.f` does not come through here — ident renders it
// directly, and `this` is null only for a method that was entered through a call
// the generator binds without a test, which is a gap of its own.
func (e *Emitter) fieldRead(f *ast.Field, recv ast.Expr) string {
	n := e.tmpName()
	ct := cname(f.Owner)
	return "({ " + ct + "* " + n + " = (" + ct + "*)" + e.expr(recv) + ";" +
		" if (!" + n + ") ty_npe(); " + n + "->f_" + mangle(f.Name) + "; })"
}

// fieldStore renders an assignable field through a receiver expression.
//
// A store through a null reference has to throw the way a load does. This used
// to be a bare `((Box*)p)->f_v`, so `Box b = null; b.v = 5` was a segmentation
// fault instead of a NullPointerException: the check was on the read path only.
// The statement expression's value is the address of the field, so what comes
// back is still an lvalue, which both the assignment and lvalueTemp's `&(...)`
// need.
func (e *Emitter) fieldStore(f *ast.Field, recv string) string {
	n := e.tmpName()
	ct := cname(f.Owner)
	return "(*({ " + ct + "* " + n + " = (" + ct + "*)" + recv + ";" +
		" if (!" + n + ") ty_npe(); &" + n + "->f_" + mangle(f.Name) + "; }))"
}

// fieldAccess renders a field read through a receiver expression.
func (e *Emitter) fieldAccess(f *ast.Field, recv string) string {
	if f.Owner == nil {
		return "0"
	}
	name := "f_" + mangle(f.Name)
	if f.Mods.Has(ast.ModStatic) {
		return "(" + e.clinitCall(f.Owner) + "G_" + mangle(f.Owner.Full) + "_" + mangle(f.Name) + ")"
	}
	ct := cname(f.Owner)
	return "((" + ct + "*)" + recv + ")->" + name
}

func (e *Emitter) selectExpr(v *ast.Select) string {
	if r, ok := v.Ref.(*ast.Class); ok {
		return "((tyobj*)&cls_" + mangle(r.Full) + ")"
	}
	if s, ok := v.Ref.(string); ok && s == "length" {
		// The operand's static type is an array, but the value that arrives may
		// be erased: `List<int[]> xs; xs.get(0).length` hands back the Object*
		// the interface signature was compiled with, and `__typeof__` of that
		// is a struct with no len. Every array's C type is tyarr*, so the cast
		// says the same thing whether anything was erased or not.
		arr := e.tmpRef("(tyarr*)" + e.expr(v.X))
		return "(" + arr + " ? " + arr + "->len : (int64_t)(intptr_t)ty_npe())"
	}
	if f, ok := v.Ref.(*ast.Field); ok {
		if f.Mods.Has(ast.ModStatic) {
			return e.fieldAccess(f, "")
		}
		return e.fieldRead(f, v.X)
	}
	return "0"
}

// boundCheck wraps an array access with the two tests Java applies, in Java's
// order: a null reference throws NullPointerException, and a non-null array has
// its index tested against the length, throwing ArrayIndexOutOfBoundsException.
// The index expression is evaluated before either test, as it is in Java.
//
// The result is the address of the checked element rather than its index. The
// tests have to run before the element is formed, which a plain subscript
// cannot promise: C may load the array's data pointer before it evaluates the
// subscript, dereferencing a null array instead of throwing.
func (e *Emitter) boundCheck(a, i, slot string) string {
	n := e.tmpName()
	k := e.tmpName()
	return "({ tyarr* " + n + " = (tyarr*)" + a + "; int64_t " + k + " = (int64_t)(" + i + ");" +
		" if (!" + n + ") ty_npe();" +
		" if (" + k + " < 0 || " + k + " >= " + n + "->len) ty_aioobe(" + k + ", " + n + "->len);" +
		" (" + slot + "*)" + n + "->data + " + k + "; })"
}

// elemSlot is the C type of the slot an array of elem holds: a reference is
// stored as an untyped pointer, a primitive as its own value.
func (e *Emitter) elemSlot(elem ast.Type) string {
	if e.isRef(elem) {
		return "void*"
	}
	return e.ctype(elem)
}

func (e *Emitter) indexExpr(v *ast.Index) string {
	elem := v.GetType()
	elemPtr := "(*" + e.boundCheck(e.expr(v.X), e.expr(v.Index), e.elemSlot(elem)) + ")"
	if e.isRef(elem) {
		return "(((" + e.ctype(elem) + ")" + elemPtr + "))"
	}
	return "(" + elemPtr + ")"
}

func (e *Emitter) cast(v *ast.Cast) string {
	src := v.X.GetType()
	dst := v.Type.Resolved
	_, sp := src.(*ast.PrimType)
	_, dp := dst.(*ast.PrimType)
	inner := e.expr(v.X)
	switch {
	case sp && dp:
		if conv := e.saturatingConv(src.(*ast.PrimType), dst.(*ast.PrimType), inner); conv != "" {
			return conv
		}
		return "((" + e.ctype(dst) + ")(" + inner + "))"
	case sp && !dp:
		return e.boxCall(inner, src.(*ast.PrimType), dst)
	case !sp && dp:
		return e.unboxCall(inner, src, dst.(*ast.PrimType))
	}
	if ct, ok := dst.(*ast.ClassType); ok {
		return "((" + cname(ct.Class) + "*)ty_checkcast((tyobj*)" + e.refExpr(v.X) + ", &cls_" + mangle(ct.Class.Full) + "))"
	}
	if _, ok := dst.(*ast.ArrayType); ok {
		return "((tyarr*)ty_checkcast((tyobj*)" + e.refExpr(v.X) + ", &cls_" + mangle(e.prog.ArrayClass().Full) + "))"
	}
	return "(" + e.ctype(dst) + ")(" + inner + ")"
}

// saturatingConv renders a conversion from a floating-point value to an
// integral one, and an empty string for every other pair.
//
// It is the one primitive conversion C does not already do the way Java defines
// it: a value that does not fit the destination is undefined in C -- the machine
// instruction answers 0x80000000 on x86-64, and a folded constant is whatever
// the optimiser decided -- while JLS 5.1.3 answers NaN, the infinities, and
// both ends of the range, all of which the runtime's helper does. A conversion
// to a type narrower than int is that int and then the truncation to the type,
// which is the two steps the conversion is defined in; the cast here is the
// second step. The in-range case is the same cast C would have emitted.
func (e *Emitter) saturatingConv(src, dst *ast.PrimType, inner string) string {
	switch src.Kind {
	case ast.Float, ast.Double:
	default:
		return ""
	}
	switch dst.Kind {
	case ast.Long:
		return "ty_d2l(" + inner + ")"
	case ast.Int:
		return "ty_d2i(" + inner + ")"
	case ast.Byte, ast.Short, ast.Char:
		return "((" + e.ctype(dst) + ")ty_d2i(" + inner + "))"
	}
	return ""
}

// narrowTarget is saturatingConv for a compound assignment, which converts the
// result of its operation back to the target's type (JLS 15.26.2). When the
// operation happens in a floating-point type and the target is integral, that
// conversion is the narrowing one -- the same one a cast performs, so the same
// clamp -- and `inner` is wrapped in it. An empty answer means there is nothing
// to clamp and the caller keeps whatever it would have emitted.
func (e *Emitter) narrowTarget(opType, target ast.Type, inner string) string {
	ot, ok := opType.(*ast.PrimType)
	if !ok {
		return inner
	}
	tt, ok := target.(*ast.PrimType)
	if !ok {
		return inner
	}
	if conv := e.saturatingConv(ot, tt, inner); conv != "" {
		return conv
	}
	return inner
}

func (e *Emitter) instanceOf(v *ast.InstanceOf) string {
	if e.patternVars != nil {
		if name, ok := e.patternVars[v]; ok {
			return e.assignPattern(v, name)
		}
	}
	if prim, isPrim := e.prog.Erased(v.Type.Resolved).(*ast.PrimType); isPrim {
		// a primitive pattern used as a value rather than as a condition: the
		// binding is not needed here, only the answer
		return e.primMatchExpr(v, prim)
	}
	dst := v.Type.Resolved
	target := ""
	if ct, ok := dst.(*ast.ClassType); ok {
		target = "&cls_" + mangle(ct.Class.Full)
	} else {
		target = "&cls_" + mangle(e.prog.ArrayClass().Full)
	}
	return "ty_instanceof((tyobj*)" + e.refExpr(v.X) + ", " + target + ")"
}

// assignPattern renders a pattern whose variables hoistPatterns declared before
// the loop. The test and the assignment travel together as one statement
// expression, so the variable is bound on every evaluation of the condition —
// which is what Java does — instead of once, when the loop was entered, which
// would leave the body reading the value of the first iteration forever.
//
// The source expression is read once into a temporary: it may have side effects
// (`xs[next()] instanceof String s`), and the test and the value that is bound
// have to agree on the one reading.
func (e *Emitter) assignPattern(v *ast.InstanceOf, name string) string {
	src := "(" + e.expr(v.X) + ")"
	if prim, isPrim := e.prog.Erased(v.Type.Resolved).(*ast.PrimType); isPrim {
		// a primitive pattern asks about the value, so the match is recorded in
		// the flag declareExprPattern left beside the value, and the value
		// itself is written only when it converted exactly
		okName := e.patternOK[v]
		return "({ " + name + " = 0; " + okName + " = ty_prim_match((void*)" + src + ", " +
			fmt.Sprint(prim.Kind) + ", &" + name + ", " + fmt.Sprint(e.primOperandBoxed(v)) + "); " +
			okName + " != 0; })"
	}
	ct := e.ctype(v.Type.Resolved)
	obj := e.tmpName()
	var b strings.Builder
	b.WriteString("({ void* " + obj + " = (void*)" + src + "; ")
	if len(v.Binding.Decomp) == 0 {
		// the variable holds the value only when the type test succeeds, which is
		// what `x instanceof T t` means as a condition
		fmt.Fprintf(&b, "%s = ty_instanceof(%s, %s) ? (%s)%s : NULL; ", name, obj, e.instTarget(v), ct, obj)
		fmt.Fprintf(&b, "%s != NULL; })", name)
		return b.String()
	}
	// The components are part of the binding, so they are read out of the record
	// again whenever it matches. A nested pattern's own components are read
	// under a test of their own, and the flag is what carries the outcome out of
	// the expression: a component that is not there, or is of the wrong type,
	// clears it, and the pattern as a whole does not match.
	ok := e.tmpName()
	fmt.Fprintf(&b, "int32_t %s = 0; ", ok)
	fmt.Fprintf(&b, "%s = ty_instanceof(%s, %s) ? (%s)%s : NULL; ", name, obj, e.instTarget(v), ct, obj)
	inner := e.capture(func() {
		e.line("%s = 1;\n", ok)
		e.emitComponentReads(e.planComponents(v.Binding, name), ok)
	})
	fmt.Fprintf(&b, "if (%s) { %s } ", name, inner)
	fmt.Fprintf(&b, "%s != 0; })", ok)
	return b.String()
}

// primOperandBoxed reports whether the operand of a primitive type pattern is a
// reference and therefore carries a box, which JEP 507 matches against the
// pattern's type exactly. A primitive operand is boxed by the caller to reach
// the runtime, and for those only the conversion has to be exact, so the two
// cases travel as a flag (javac 25 --enable-preview agrees with both rules).
func (e *Emitter) primOperandBoxed(v *ast.InstanceOf) int {
	// Semantic analysis boxes a primitive operand so the runtime sees an object,
	// which turns the operand into a conversion whose source is primitive. That
	// conversion is the only trace of "the operand was a primitive", and it is
	// what decides between JEP 507's two rules; an operand that was already a
	// reference is boxed all the same but has no such conversion under it.
	for x := v.X; ; {
		c, ok := x.(*ast.Conv)
		if !ok {
			return operandIsBox(e.prog, x.GetType())
		}
		if _, isPrim := c.X.GetType().(*ast.PrimType); isPrim {
			return 0
		}
		x = c.X
	}
}

// operandIsBox reports whether a static type is a reference, and the operand
// therefore carries a box. Program.Erased cannot answer this: it erases a type
// variable, but it is not a test for primitiveness and it rewrites a primitive
// type rather than returning it unchanged.
func operandIsBox(p *sema.Program, t ast.Type) int {
	if tv, ok := t.(*ast.TypeVarType); ok {
		t = p.Erased(tv)
	}
	if _, isPrim := t.(*ast.PrimType); isPrim {
		return 0
	}
	return 1
}

// primMatchExpr renders the run time question a primitive type pattern asks,
// as a statement expression that yields a boolean.
func (e *Emitter) primMatchExpr(v *ast.InstanceOf, prim *ast.PrimType) string {
	src := e.expr(v.X)
	val := e.tmpName()
	ok := e.tmpName()
	inner := e.capture(func() {
		e.line("%s %s = 0;\n", e.ctype(prim), val)
		e.line("int32_t %s = ty_prim_match((void*)%s, %d, &%s, %d);\n", ok, src, prim.Kind, val, e.primOperandBoxed(v))
	})
	return "({ " + inner + " " + ok + " != 0; })"
}

func (e *Emitter) unary(v *ast.Unary) string {
	if v.Op == "++" || v.Op == "--" {
		if pre := e.targetClinit(v.X); pre != "" {
			return "(" + pre + e.unaryInner(v) + ")"
		}
	}
	return e.unaryInner(v)
}

func (e *Emitter) unaryInner(v *ast.Unary) string {
	x := e.expr(v.X)
	xt := v.X.GetType()
	switch v.Op {
	case "+":
		// A boxed operand is unboxed and promoted, so `+someByte` is an int:
		// the checker types every unary operand through unboxOrPrim and
		// promoteUnary, and the emitter has to answer in the same type.
		if k, ok := e.boxKindOf(xt); ok {
			return e.unboxCall(x, xt, promoteOf(k))
		}
		return "(" + x + ")"
	case "-":
		// Negating the most negative value is undefined in C and a compiler
		// may fold it away -- clang turns `-INT64_MIN` into 0 where Java says
		// it wraps to itself. Integer negation goes through an unsigned value
		// instead, where every input is defined.
		if k, ok := e.boxKindOf(xt); ok {
			return negate(e.unboxCall(x, xt, promoteOf(k)), promoteOf(k))
		}
		if p, ok := xt.(*ast.PrimType); ok && p.IsIntegral() {
			return negate(x, p)
		}
		return "(-(" + x + "))"
	case "!":
		if _, ok := xt.(*ast.PrimType); !ok {
			return "(!(" + e.unboxCall("("+x+")", xt, ast.TBoolean) + "))"
		}
		return "(!(" + x + "))"
	case "~":
		// No boxed case: the checker refuses `~` on anything but a primitive
		// (TY-TYP-0053), where Java unboxes. The other four operators accept a
		// wrapper through unboxOrPrim, which is why they are here.
		return "(~(" + x + "))"
	case "++", "--":
		// `++` on a wrapper replaces the wrapper: Java's update unboxes, does
		// the arithmetic in the promoted type and boxes the result back, and a
		// wrapper is immutable, so another variable holding the same one must
		// not see the change. Written the way C reads it, `(*_p) += 1` would be
		// pointer arithmetic on the box.
		if k, ok := e.boxKindOf(xt); ok {
			return e.boxedUpdate(v, k, xt)
		}
		op := "+ 1"
		if v.Op == "--" {
			op = "- 1"
		}
		decl, ref := e.lvalueTemp(v.X)
		if v.Postfix {
			// the value of a postfix update is the one the target held before it
			return "({ " + decl + ref + " += " + op + "; " + ref + " - (" + op + "); })"
		}
		return "({ " + decl + ref + " += " + op + "; })"
	}
	return x
}

// promoteOf is the type a unary operator works in: JLS 5.6 promotes byte,
// short and char to int and leaves the rest alone. It is sema's promoteUnary,
// restated for the emitter (which cannot import the checker).
func promoteOf(k ast.PrimKind) *ast.PrimType {
	switch k {
	case ast.Long:
		return ast.TLong
	case ast.Float:
		return ast.TFloat
	case ast.Double:
		return ast.TDouble
	}
	return ast.TInt
}

// negate renders -x, where x is an expression of type p. An integral negation
// goes through an unsigned value, because C leaves the negation of the most
// negative one undefined and clang folds it away where Java wraps; a floating
// one is the operator itself, which is what negates the zero and the infinities
// the way Java does.
func negate(x string, p *ast.PrimType) string {
	if !p.IsIntegral() {
		return "(-(" + x + "))"
	}
	if p.Kind == ast.Long {
		return "((int64_t)(0ull - (uint64_t)(" + x + ")))"
	}
	return "((int32_t)(0u - (uint32_t)(" + x + ")))"
}

// boxKindOf is the primitive an operand carries when its static type is one of
// the eight wrappers, boxed.
func (e *Emitter) boxKindOf(t ast.Type) (ast.PrimKind, bool) {
	ct, ok := e.prog.Erased(t).(*ast.ClassType)
	if !ok || ct.Class == nil {
		return ast.Void, false
	}
	k, ok := e.prog.Builtins.Unbox[ct.Class]
	return k, ok
}

// boxedUpdate renders `++` and `--` on a boxed variable. The value of the
// expression is the new wrapper for a prefix update and the old one for a
// postfix update, which is the wrapper the variable held -- so the old one is
// bound before the store.
func (e *Emitter) boxedUpdate(v *ast.Unary, k ast.PrimKind, xt ast.Type) string {
	decl, ref := e.lvalueTemp(v.X)
	promoted := promoteOf(k)
	// The read is unboxCall's: it tests the receiver, so a null wrapper raises
	// NullPointerException here rather than reading a field of address zero, and
	// it promotes byte, short and char to int the way the checker types them.
	read := e.unboxCall(ref, xt, promoted)
	step := " + 1"
	if v.Op == "--" {
		step = " - 1"
	}
	value := "(" + read + step + ")"
	if k != ast.Float && k != ast.Double {
		// Java wraps where C leaves signed overflow undefined, so the
		// arithmetic goes through an unsigned value of the promoted width.
		ut := "uint32_t"
		if k == ast.Long {
			ut = "uint64_t"
		}
		value = "(" + e.ctype(promoted) + ")((" + ut + ")(" + read + ")" + step + ")"
	}
	back := e.wrapperCall(e.wrapperStatic(k, "valueOf"), "("+e.ctype(&ast.PrimType{Kind: k})+")("+value+")")
	if v.Postfix {
		old := e.tmpName()
		return "({ " + decl + e.ctype(xt) + " " + old + " = " + ref + "; " +
			ref + " = " + back + "; " + old + "; })"
	}
	return "({ " + decl + ref + " = " + back + "; " + ref + "; })"
}

// lvalueTemp binds an lvalue to a temporary pointer. An update or a compound
// assignment needs the target twice, and repeating the lvalue would evaluate an
// index or a receiver with side effects a second time; dereferencing the shared
// temporary keeps that to one evaluation. The declaration is spliced into a
// statement expression by the caller.
func (e *Emitter) lvalueTemp(x ast.Expr) (string, string) {
	n := e.tmpName()
	t := e.ctype(x.GetType())
	return t + "* " + n + " = (" + t + "*)&(" + e.lvalue(x) + "); ", "(*" + n + ")"
}

// lvalue renders an assignable expression. Static targets are returned without
// the class-initialization wrapper because a comma expression is not assignable;
// assignClinit adds it around the whole assignment instead.
func (e *Emitter) lvalue(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		if f, ok := v.Ref.(*ast.Field); ok {
			if f.Mods.Has(ast.ModStatic) {
				return "G_" + mangle(f.Owner.Full) + "_" + mangle(f.Name)
			}
			// a bare name is a field of the instance the code runs on, which
			// inside a lambda body is the instance the lambda was created in
			if e.curClass != nil && f.Owner != nil && f.Owner != e.curClass {
				return e.outerFieldAccess(f)
			}
			return e.fieldAccess(f, e.thisExpr())
		}
		return e.ident(v)
	case *ast.Select:
		if f, ok := v.Ref.(*ast.Field); ok {
			if f.Mods.Has(ast.ModStatic) {
				return "G_" + mangle(f.Owner.Full) + "_" + mangle(f.Name)
			}
			return e.fieldStore(f, e.tmpRef(e.expr(v.X)))
		}
		return "0"
	case *ast.Index:
		return "(*" + e.boundCheck(e.expr(v.X), e.expr(v.Index), e.elemSlot(v.GetType())) + ")"
	}
	return e.expr(x)
}

// isFloatingLiteral reports whether a literal is a floating-point value.
func isFloatingLiteral(l *ast.Literal) bool {
	return l.Kind == ast.LitDouble || l.Kind == ast.LitFloat
}

// foldBinary evaluates a binary expression over literals at compile time.
func (e *Emitter) foldBinary(v *ast.Binary) (string, bool) {
	xl, xok := v.X.(*ast.Literal)
	yl, yok := v.Y.(*ast.Literal)
	if !xok || !yok {
		return "", false
	}
	// string concatenation of two literals
	if v.Op == "+" && xl.Kind == ast.LitString && yl.Kind == ast.LitString {
		return e.strLit(xl.Str + yl.Str), true
	}
	pt, isPrim := v.OpType.(*ast.PrimType)
	if !isPrim || !pt.IsNumeric() {
		return "", false
	}
	wide := pt.Kind == ast.Long
	isFloat := isFloatingLiteral(xl) || isFloatingLiteral(yl)
	if isFloat {
		x, y := xl.Flt, yl.Flt
		if !isFloatingLiteral(xl) {
			x = float64(int64(xl.Int))
		}
		if !isFloatingLiteral(yl) {
			y = float64(int64(yl.Int))
		}
		switch v.Op {
		case "+":
			return e.literal(&ast.Literal{Kind: ast.LitDouble, Flt: x + y}), true
		case "-":
			return e.literal(&ast.Literal{Kind: ast.LitDouble, Flt: x - y}), true
		case "*":
			return e.literal(&ast.Literal{Kind: ast.LitDouble, Flt: x * y}), true
		case "/":
			if y != 0 {
				return e.literal(&ast.Literal{Kind: ast.LitDouble, Flt: x / y}), true
			}
		}
		return "", false
	}
	x, y := int64(xl.Int), int64(yl.Int)
	// JLS 15.19: only the low bits of the count are significant, and how many
	// depends on the type the shift is performed in -- not on the int64 used
	// here. Masking with the long width would fold an int `a << 32` to 0 where
	// Java gives 1.
	mask := uint((1 << shiftBitsInt) - 1)
	if wide {
		mask = (1 << shiftBitsLong) - 1
	}
	var r int64
	switch v.Op {
	case "+":
		r = x + y
	case "-":
		r = x - y
	case "*":
		r = x * y
	case "/":
		if y == 0 {
			return "", false
		}
		r = x / y
	case "%":
		if y == 0 {
			return "", false
		}
		r = x % y
	case "&":
		r = x & y
	case "|":
		r = x | y
	case "^":
		r = x ^ y
	case "<<":
		r = x << (uint(y) & mask)
	case ">>":
		r = x >> (uint(y) & mask)
	default:
		return "", false
	}
	if !wide {
		r = int64(int32(r))
	}
	kind := ast.LitInt
	if wide {
		kind = ast.LitLong
	}
	return e.literal(&ast.Literal{Kind: kind, Int: uint64(r)}), true
}

// flatOps are the operators whose left-deep runs are spelled as one flat chain.
// Each is left-associative in C with the same meaning as in Teyru, so dropping
// the parentheses cannot change the value. Division and remainder are absent
// because their grouping matters, and `>>>` has its own unsigned emission.
var flatOps = map[string]bool{
	"+": true, "-": true, "*": true, "&": true, "|": true, "^": true,
	"<<": true, ">>": true, "&&": true, "||": true,
}

// flattenable reports whether a binary node is a left-deep run of one operator
// that can be spelled flat. A chain such as `a + b + c + ...` otherwise hands
// the C compiler one parenthesis level per term, which overflows its parser on
// a chain of a few thousand terms; the flat spelling compiles.
func (e *Emitter) flattenable(v *ast.Binary) bool {
	if !flatOps[v.Op] {
		return false
	}
	// a string `+` concatenates rather than adds, and is emitted as a chain of
	// runtime calls already
	if _, ok := v.GetType().(*ast.PrimType); !ok {
		return false
	}
	b, ok := v.X.(*ast.Binary)
	return ok && e.flatSpine(v, b)
}

// flatSpine reports whether b continues a left-deep run of v's operator: the
// same operator, and not a constant that the nested spelling would have folded
// instead of computing in C.
func (e *Emitter) flatSpine(v, b *ast.Binary) bool {
	if b.Op != v.Op {
		return false
	}
	_, folds := e.foldBinary(b)
	return !folds
}

// flatChain renders a left-deep run of one operator as a single chain, with the
// operands in their original order and each rendered as the nested spelling
// rendered it. Only the left spine is flattened: a run on the right, as in
// `a - (b - c)`, keeps its parentheses because its grouping differs.
func (e *Emitter) flatChain(v *ast.Binary) string {
	if b, ok := v.X.(*ast.Binary); ok && e.flatSpine(v, b) {
		return e.flatChain(b) + " " + v.Op + " " + e.flatRight(v.Y, v)
	}
	return e.flatOperand(v.X, v) + " " + v.Op + " " + e.flatRight(v.Y, v)
}

// flatOperand renders one operand of a chain: a short-circuit operator tests
// its operands, any other coerces them.
func (e *Emitter) flatOperand(x ast.Expr, v *ast.Binary) string {
	if v.Op == "&&" || v.Op == "||" {
		return e.cond(x)
	}
	return e.operand(x, v.OpType)
}

// flatRight renders the right operand of a flattened link. For a shift that
// operand is the count, and a flattened chain has to mask it exactly like the
// nested spelling does, or the two spellings of one expression disagree.
func (e *Emitter) flatRight(y ast.Expr, v *ast.Binary) string {
	s := e.flatOperand(y, v)
	if v.Op == "<<" || v.Op == ">>" {
		return shiftCount(s, v.OpType)
	}
	return s
}

// shiftBitsInt and shiftBitsLong are the number of low bits of a shift count
// that JLS 15.19 leaves significant: 5 for the int family, 6 for long. Every
// other bit of the count is discarded, so an int `a << 33` shifts by 1. C
// instead leaves a shift whose count reaches the operand width undefined, and
// clang at -O1 and -O2 folds such a shift to an arbitrary value or lets a stack
// address through, so masking is what keeps the emitted program defined rather
// than an optimisation.
const (
	shiftBitsInt  = 5
	shiftBitsLong = 6
)

// shiftType is the type a shift whose left operand has type t is performed in:
// JLS 5.6 promotes every integral type but long to int.
func shiftType(t ast.Type) ast.Type {
	if ast.IsPrim(t, ast.Long) {
		return ast.TLong
	}
	return ast.TInt
}

// shiftCount renders an already-rendered shift count reduced to the bits of
// shiftBitsInt/shiftBitsLong. The count is masked as an unsigned value, so a
// negative one wraps the way Java's does (`a << -1` shifts by 31), and the
// masked count is cast back to the signed type the shift is performed in: a
// count of unsigned type would drag the shifted operand into unsigned
// arithmetic under the usual conversions and quietly turn `>>` into a logical
// shift. left is the type of the operand being shifted.
func shiftCount(count string, left ast.Type) string {
	ut, st, bits := "uint32_t", "int32_t", shiftBitsInt
	if ast.IsPrim(left, ast.Long) {
		ut, st, bits = "uint64_t", "int64_t", shiftBitsLong
	}
	return "(" + st + ")((" + ut + ")(" + count + ") & " + strconv.Itoa((1<<bits)-1) + ")"
}

func (e *Emitter) binary(v *ast.Binary) string {
	if s, ok := e.foldBinary(v); ok {
		return s
	}
	// `>>>` is an unsigned shift, which C spells with an unsigned operand
	if v.Op == ">>>" {
		ut := "uint32_t"
		if ast.IsPrim(v.OpType, ast.Long) {
			ut = "uint64_t"
		}
		if ast.IsPrim(v.OpType, ast.Byte) || ast.IsPrim(v.OpType, ast.Short) || ast.IsPrim(v.OpType, ast.Char) {
			ut = "uint32_t"
		}
		return "(" + e.ctype(v.GetType()) + ")((" + ut + ")(" + e.expr(v.X) + ") >> " +
			shiftCount(e.operand(v.Y, v.OpType), v.OpType) + ")"
	}
	if e.flattenable(v) {
		return "(" + e.flatChain(v) + ")"
	}
	lt, rt := v.X.GetType(), v.Y.GetType()
	switch v.Op {
	case "==", "!=":
		return e.equality(v, lt, rt)
	case "&&":
		return "(" + e.cond(v.X) + " && " + e.cond(v.Y) + ")"
	case "||":
		return "(" + e.cond(v.X) + " || " + e.cond(v.Y) + ")"
	case "+":
		if e.isStringType(v.GetType()) {
			return e.concat(v)
		}
	case "/":
		return e.divExpr(v)
	case "%":
		return e.remExpr(v)
	}
	x := e.operand(v.X, v.OpType)
	y := e.operand(v.Y, v.OpType)
	// C makes a shift whose count reaches the operand width undefined, so the
	// count of `<<` and `>>` is masked before it is emitted
	if v.Op == "<<" || v.Op == ">>" {
		y = shiftCount(y, v.OpType)
		// A shift takes its width from its left operand in C, and the operand
		// may have been written as a literal with no suffix of its own -- the
		// inlined value of a constant field, say. Naming the width here is what
		// keeps `MASK << 63` a long shift rather than an int one.
		if ast.IsPrim(v.OpType, ast.Long) {
			x = "(int64_t)" + x
		}
	}
	return "(" + x + " " + v.Op + " " + y + ")"
}

func (e *Emitter) isStringType(t ast.Type) bool {
	ct, ok := t.(*ast.ClassType)
	return ok && ct.Class.Special == "String"
}

// operand coerces an operand of a numeric/bitwise operation.
func (e *Emitter) operand(x ast.Expr, op ast.Type) string {
	v := e.expr(x)
	if op == nil {
		return v
	}
	if _, ok := x.GetType().(*ast.PrimType); !ok {
		return e.unboxCall(v, x.GetType(), op.(*ast.PrimType))
	}
	return v
}

func (e *Emitter) equality(v *ast.Binary, lt, rt ast.Type) string {
	ls, lsOk := lt.(*ast.PrimType)
	rs, rsOk := rt.(*ast.PrimType)
	switch {
	case lsOk && rsOk:
		return "(" + e.expr(v.X) + " " + v.Op + " " + e.expr(v.Y) + ")"
	case lsOk != rsOk:
		// boxed/unboxed comparison
		x, y := e.expr(v.X), e.expr(v.Y)
		if lsOk {
			y = e.unboxCall(y, rt, ls)
		} else {
			x = e.unboxCall(x, lt, rs)
		}
		return "(" + x + " " + v.Op + " " + y + ")"
	}
	// `==` on two String references is a reference comparison, as in Java: two
	// strings built separately are equal only under equals(). Literals are
	// interned per program by strLit, so `a == "abc"` is still true, and so is a
	// comparison against a concatenation of literals folded at compile time.
	return "(" + e.expr(v.X) + " " + v.Op + " " + e.expr(v.Y) + ")"
}

// divExpr uses a runtime helper for integral division, which throws on a zero
// divisor; floating point division follows IEEE 754 and yields an infinity or
// NaN instead.
func (e *Emitter) divExpr(v *ast.Binary) string {
	x, y := e.operand(v.X, v.OpType), e.operand(v.Y, v.OpType)
	switch {
	case ast.IsPrim(v.OpType, ast.Long):
		return "ty_div_long(" + x + ", " + y + ")"
	case isFloating(v.OpType):
		return "((" + x + ") / (" + y + "))"
	}
	return "ty_div_int(" + x + ", " + y + ")"
}

func (e *Emitter) remExpr(v *ast.Binary) string {
	x, y := e.operand(v.X, v.OpType), e.operand(v.Y, v.OpType)
	switch {
	case ast.IsPrim(v.OpType, ast.Long):
		return "ty_rem_long(" + x + ", " + y + ")"
	case isFloating(v.OpType):
		return "fmod(" + x + ", " + y + ")"
	}
	return "ty_rem_int(" + x + ", " + y + ")"
}

// isFloating reports whether a type is float or double.
func isFloating(t ast.Type) bool {
	return ast.IsPrim(t, ast.Double) || ast.IsPrim(t, ast.Float)
}

// concat renders Java string concatenation.
func (e *Emitter) concat(v *ast.Binary) string {
	return e.concatFrom(e.stringOperand(v.X), v.Y)
}

// concatFrom builds the concatenation of an already-rendered left operand with
// the string parts of x. It is used when the left operand is a value read
// through a bound temporary rather than an expression of its own.
func (e *Emitter) concatFrom(first string, x ast.Expr) string {
	parts := []string{first}
	var collect func(ast.Expr)
	collect = func(y ast.Expr) {
		if b, ok := y.(*ast.Binary); ok && b.Op == "+" && e.isStringType(b.GetType()) {
			collect(b.X)
			collect(b.Y)
			return
		}
		parts = append(parts, e.stringOperand(y))
	}
	collect(x)
	out := parts[0]
	for _, p := range parts[1:] {
		out = "ty_str_concat(" + out + ", " + p + ")"
	}
	return out
}

func (e *Emitter) stringOperand(x ast.Expr) string {
	t := x.GetType()
	if e.isStringType(t) {
		return "(tystr*)" + e.expr(x)
	}
	if p, ok := t.(*ast.PrimType); ok {
		switch p.Kind {
		case ast.Boolean:
			return "ty_str_of_bool(" + e.expr(x) + ")"
		case ast.Char:
			return "ty_str_of_char(" + e.expr(x) + ")"
		case ast.Float:
			return "ty_str_of_float(" + e.expr(x) + ")"
		case ast.Double:
			return "ty_str_of_double(" + e.expr(x) + ")"
		case ast.Byte, ast.Short, ast.Int, ast.Long:
			return "ty_str_of_long(" + e.expr(x) + ")"
		}
	}
	return "ty_str_of_obj((tyobj*)" + e.expr(x) + ")"
}

// targetClinit returns the class-initialization prefix for a static target.
func (e *Emitter) targetClinit(x ast.Expr) string {
	var ref any
	switch t := x.(type) {
	case *ast.Ident:
		ref = t.Ref
	case *ast.Select:
		ref = t.Ref
	}
	if f, ok := ref.(*ast.Field); ok && f.Mods.Has(ast.ModStatic) && f.Owner != nil {
		return e.clinitCall(f.Owner)
	}
	return ""
}

func (e *Emitter) assign(v *ast.Assign) string {
	pre := e.targetClinit(v.X)
	// A plain store of a reference into an array element goes through the
	// runtime's checked store, which is what makes an array reject a value its
	// element class never promised. targetClinit never wraps an index target, so
	// the check for one costs nothing here.
	if ix, ok := v.X.(*ast.Index); ok && v.Op == "=" && pre == "" {
		if selem := e.refElemClass(ix.GetType()); selem != "" {
			return e.refElemStore(ix, selem, e.coerce(e.expr(v.Y), v.Y.GetType(), v.X.GetType()))
		}
	}
	if v.Op == "=" {
		lv := e.lvalue(v.X)
		if pre != "" {
			return "(" + pre + e.assignInner(v, lv) + ")"
		}
		return e.assignInner(v, lv)
	}
	// a compound assignment reads the target as well as writing it, so the
	// target is bound once and both uses go through the temporary
	decl, ref := e.lvalueTemp(v.X)
	expr := "({ " + decl + e.assignInner(v, ref) + "; })"
	if pre != "" {
		return "(" + pre + expr + ")"
	}
	return expr
}

func (e *Emitter) assignInner(v *ast.Assign, lv string) string {
	if v.Op == "=" {
		return "(" + lv + " = " + e.coerce(e.expr(v.Y), v.Y.GetType(), v.X.GetType()) + ")"
	}
	if v.Op == "+=" && e.isStringType(v.X.GetType()) {
		// string += is concatenation; the left operand is the value the target
		// holds before the store
		return "(" + lv + " = (tystr*)" + e.concatFrom("(tystr*)"+lv, v.Y) + ")"
	}
	op := v.Op[:len(v.Op)-1]
	// a compound shift masks its count exactly like the binary form; the target
	// also gives the type the count is unboxed to, since sema leaves the
	// operation type of a compound assignment unset
	if op == "<<" || op == ">>" || op == ">>>" {
		st := shiftType(v.X.GetType())
		count := shiftCount(e.operand(v.Y, st), st)
		if op == ">>>" {
			ut := "uint32_t"
			if ast.IsPrim(st, ast.Long) {
				ut = "uint64_t"
			}
			return "(" + lv + " = (" + e.ctype(v.X.GetType()) + ")((" + ut + ")" + lv + " >> " + count + "))"
		}
		return "(" + lv + " " + op + "= " + count + ")"
	}
	if op == "/" || op == "%" {
		xt := v.X.GetType()
		if isFloating(v.OpType) {
			// floating point division and remainder never throw
			call := "((" + lv + ") " + op + " (" + e.expr(v.Y) + "))"
			if op == "%" {
				call = "fmod(" + lv + ", " + e.expr(v.Y) + ")"
			}
			// the result is converted back to the target (JLS 15.26.2), and for
			// an integral target that is the narrowing conversion a cast does --
			// which C's own conversion of a double to an int does not define
			return "(" + lv + " = (" + e.ctype(xt) + ")" + e.narrowTarget(v.OpType, xt, call) + ")"
		}
		fn := "ty_div_int"
		if op == "%" {
			fn = "ty_rem_int"
		}
		if ast.IsPrim(xt, ast.Long) {
			if op == "%" {
				fn = "ty_rem_long"
			} else {
				fn = "ty_div_long"
			}
		}
		return "(" + lv + " = (" + e.ctype(xt) + ")" + fn + "(" + lv + ", " + e.expr(v.Y) + "))"
	}
	// The result of the operation is converted back to the target's type, and
	// when the operation happens in a floating-point type and the target is
	// integral that conversion is a narrowing one: Java runs it through the same
	// conversion a cast performs (JLS 15.26.2), while C's `x op= y` converts it
	// with a plain cast, which is undefined for a value that does not fit. The
	// statement C already emitted is kept wherever no clamping is needed.
	call := "(" + lv + " " + op + " " + e.operand(v.Y, v.OpType) + ")"
	if conv := e.narrowTarget(v.OpType, v.X.GetType(), call); conv != call {
		return "(" + lv + " = " + conv + ")"
	}
	return "(" + lv + " " + op + "= " + e.operand(v.Y, v.OpType) + ")"
}

// refElemStore renders the store of val into ix, an element of an array of
// references, as one statement expression. selem is the element class at the
// store site, which the runtime store check compares the array's own element
// class against (see ty_array_store_ref in tyrt.h).
//
// The order is Java's for an array assignment: the array, then the index, then
// the value, and only then the tests, which is why the value is bound to a
// temporary first. The result of the assignment is the stored value, so it is
// the last expression of the statement expression.
func (e *Emitter) refElemStore(ix *ast.Index, selem, val string) string {
	n := e.tmpName()
	k := e.tmpName()
	v := e.tmpName()
	t := e.ctype(ix.GetType())
	return "({ tyarr* " + n + " = (tyarr*)" + e.expr(ix.X) + "; int64_t " + k + " = (int64_t)(" +
		e.expr(ix.Index) + "); " + t + " " + v + " = (" + t + ")(" + val + ");" +
		" ty_array_store_ref(" + n + ", " + selem + ", " + k + ", (void*)" + v + "); " + v + "; })"
}

// ---------------------------------------------------------------- calls

func (e *Emitter) callExpr(v *ast.Call) string {
	m := v.Method
	if e.reflectionCall(m) {
		// The member tables -- the field and method tables, and the invokers
		// beside them -- are the one part of a class's metadata that names other
		// classes, so they are only put in the output when a call site can reach
		// them. This is that call site: a program that asks a Class for its
		// members, invokes through a Method, or loads one by name.
		e.reflectUsed = true
	}
	if m == nil {
		// arrays have Object-like methods but no method symbol
		if arr, ok := v.Recv.GetType().(ast.Type); ok {
			if at, isArr := arr.(*ast.ArrayType); isArr {
				switch v.Name {
				case "clone":
					return "((tyarr*)ty_array_clone((tyarr*)" + e.expr(v.Recv) + ", " + e.elemSize(at.Elem) + "))"
				case "toString":
					return "((tystr*)ty_str_intern(\"[array]\"))"
				case "hashCode":
					return "ty_object_hash((void*)" + e.expr(v.Recv) + ")"
				case "equals":
					a := e.tmpRef(e.expr(v.Recv))
					var other string
					if len(v.Args) > 0 {
						other = e.expr(v.Args[0])
					} else {
						other = "NULL"
					}
					return "((void*)" + a + " == (void*)" + other + ")"
				}
			}
		}
		return "0"
	}
	recv := ""
	if !m.IsStatic() {
		switch {
		case v.Recv == nil:
			// an unqualified call is a call on the instance the code runs on.
			// The checker names the enclosing instance for a lambda body, so a
			// call that still arrives without a receiver is a method of the
			// class the body was written in: inside a lambda `this` is that
			// instance, not the closure object, or the call would dispatch back
			// into the lambda's own method and recurse until the stack ran out.
			recv = e.thisExpr()
		case v.Super:
			recv = "((void*)" + e.thisExpr() + ")"
		default:
			recv = e.expr(v.Recv)
		}
	}
	if _, ok := nativeTable[nativeKey(m)]; ok && e.nativeIsFinal(m) {
		return e.nativeCall(m, recv, v.Args)
	}
	name := e.cfunc(m)
	a := e.argsFor(recv, v.Args, m, v)
	if m.External {
		return m.Native + "(" + a + ")"
	}
	if m.IsStatic() && !v.Super && v.Recv != nil {
		// touching a static member initializes its class first
		return "(" + e.clinitCall(m.Owner) + name + "(" + a + "))"
	}
	if v.Static || m.IsStatic() || v.Super {
		// super calls bind statically to the superclass implementation
		return name + "(" + a + ")"
	}
	if v.Recv == nil {
		if (m.Selector >= 0 || m.VIndex >= 0) && !directBind(m) {
			return e.virtCall(m, cname(m.Owner)+"*", e.thisExpr(), v.Args, v)
		}
		return name + "(" + a + ")"
	}
	if (m.Selector >= 0 || m.VIndex >= 0) && !m.Mods.Has(ast.ModPrivate) {
		if directBind(m) {
			return e.bindCall(m, cname(m.Owner)+"*", v.Recv, v.Args, v)
		}
		return e.virtCallTemp(v.Recv, m, v.Args, v)
	}
	return name + "(" + a + ")"
}

// directBind reports whether a virtual call can be bound at compile time
// instead of read out of the receiver's vtable.
//
// A method that cannot be overridden -- because it is final, or because its
// class is -- has one implementation per slot, so the lookup can only answer
// with the method resolved here. That is the reasoning nativeIsFinal applies to
// a native helper, and it is what keeps a call to a wrapper's intValue() as
// cheap as the runtime helper it replaced: the wrapper classes are declared
// final (lib/04_boxing.teyru), as they are in Java.
//
// An interface method is never bound this way: a class implements an interface
// by being listed in it rather than by extending it, so Subclasses records
// nothing about the implementations an interface call can reach.
//
// "Nothing in this program extends it" would be a wider rule, and a sound one,
// but it is deliberately not used here: it also binds the methods of a class
// that merely happens not to have a subclass yet, which lets the optimiser drop
// the allocation behind a call like `new Cell(i).value()` -- measured on
// examples/bench_alloc, the program then allocates nothing at all and the
// benchmark stops measuring allocation (0.1325s -> 0.0234s for a hundred
// million iterations). What a program may rely on is the declaration it wrote.
func directBind(m *ast.Method) bool {
	return m.Selector < 0 && m.VIndex >= 0 && (m.Mods.Has(ast.ModFinal) ||
		(m.Owner != nil && m.Owner.Mods.Has(ast.ModFinal)))
}

// bindCall calls a method that cannot dispatch anywhere else. The receiver is
// bound once and tested, exactly as the vtable lookup would have tested it: a
// call on a null receiver is a NullPointerException in Java, and the generated
// callee reads its own fields without a test of its own -- the gap virtCall's
// comment describes for a call the generator never entered through.
func (e *Emitter) bindCall(m *ast.Method, recvT string, recv ast.Expr, args []ast.Expr, call *ast.Call) string {
	if recvT == "" {
		recvT = "void*"
	}
	n := e.tmpName()
	inner := e.cfunc(m) + "(" + e.argsFor(n, args, m, call) + ")"
	if ret := e.ctype(m.Result); ret != "void" {
		return "({ " + recvT + " " + n + " = (" + recvT + ")" + e.expr(recv) + "; " +
			"((" + n + ") ? " + inner + " : (" + ret + ")((intptr_t)ty_npe())); })"
	}
	return "({ " + recvT + " " + n + " = (" + recvT + ")" + e.expr(recv) + "; " +
		"((" + n + ") ? (void)" + inner + " : (void)ty_npe()); })"
}

// virtCallTemp dispatches a virtual call whose receiver may be an expression
// rather than a variable. The receiver is both dereferenced for the vtable
// lookup and passed as the first argument, so it is evaluated once into a
// temporary that every use shares; otherwise a receiver such as make() would
// run again for each use.
func (e *Emitter) virtCallTemp(recv ast.Expr, m *ast.Method, args []ast.Expr, call *ast.Call) string {
	rt := cname(m.Owner) + "*"
	n := e.tmpName()
	return "({ " + rt + " " + n + " = (" + rt + ")" + e.expr(recv) + "; " +
		e.virtCall(m, rt, n, args, call) + "; })"
}

// virtCall dispatches through the vtable or, for interface receivers, the itable.
//
// Both lookups read the receiver, so a null one has to be answered the way Java
// answers it: a NullPointerException, not a segmentation fault. The receiver is
// a variable or a temporary by the time it gets here (virtCallTemp binds
// anything else), so the test evaluates nothing twice and costs one branch,
// which the conditional operator keeps out of the call itself.
func (e *Emitter) virtCall(m *ast.Method, recvT, recv string, args []ast.Expr, call *ast.Call) string {
	if recvT == "" {
		recvT = "void*"
	}
	var body string
	if m.Selector >= 0 {
		fn := "((void*)ty_itab((tyobj*)" + recv + ", " + fmt.Sprint(m.Selector) + "))"
		body = e.indirect(m, fn, "void*", recv, args, call)
	} else {
		fn := "((" + recv + ")->obj.cls->vtable[" + fmt.Sprint(m.VIndex) + "])"
		body = e.indirect(m, fn, recvT, recv, args, call)
	}
	if ret := e.ctype(m.Result); ret != "void" {
		return "((" + recv + ") ? (" + body + ") : (" + ret + ")((intptr_t)ty_npe()))"
	}
	return "((" + recv + ") ? (void)(" + body + ") : (void)ty_npe())"
}

// indirect builds a call through a runtime-resolved function pointer.
func (e *Emitter) indirect(m *ast.Method, fn, recvT, recv string, args []ast.Expr, call *ast.Call) string {
	ret := e.ctype(m.Result)
	var ps []string
	if !m.IsStatic() {
		ps = append(ps, recvT)
	}
	for _, p := range m.Params {
		ps = append(ps, e.ctype(p))
	}
	if len(ps) == 0 {
		ps = append(ps, "void")
	}
	// The call is threaded through rather than guessed at: argsFor needs it to
	// know whether a variable-arity argument list was written as the array
	// itself or as its elements, and a heuristic cannot tell
	// `printf("%s", x)` from `printf("%s", new Object[]{x})`.
	a := e.argsFor(recv, args, m, call)
	sig := "(( " + ret + "(*)(" + strings.Join(ps, ", ") + "))" + fn + ")"
	if ret == "void" {
		if a == "" {
			return sig + "()"
		}
		return sig + "(" + a + ")"
	}
	return sig + "(" + a + ")"
}

// ---------------------------------------------------------------- new

func (e *Emitter) newExpr(v *ast.New) string {
	ct, ok := v.GetType().(*ast.ClassType)
	if !ok {
		if ct2, ok2 := e.prog.Erased(v.GetType()).(*ast.ClassType); ok2 {
			ct, ok = ct2, true
		} else {
			return "NULL"
		}
	}
	cl := ct.Class
	// a native constructor is implemented by a runtime helper
	if v.Ctor != nil {
		if nf, ok := nativeTable[nativeKey(v.Ctor)]; ok && nf.fn != "" {
			args := make([]string, 0, len(v.Args))
			for i, a := range v.Args {
				var want ast.Type
				if i < len(v.Ctor.Params) {
					want = v.Ctor.Params[i]
				}
				args = append(args, e.coerce(e.expr(a), a.GetType(), want))
			}
			if nf.recv != "" && len(args) > 0 {
				args[0] = "(" + nf.recv + ")" + args[0]
			}
			return "(" + e.ctype(ct) + ")" + nf.fn + "(" + strings.Join(args, ", ") + ")"
		}
	}
	if fn, ok := specialNew[cl.Name]; ok && len(v.Args) == 0 {
		p := cname(cl) + "*"
		return "({ " + p + " _o = (" + p + ")" + fn + "(); _o->obj.cls = &cls_" + mangle(cl.Full) + "; _o; })"
	}
	n := e.tmpName()
	var b strings.Builder
	fmt.Fprintf(&b, "({ %s %s = (%s)ty_alloc(sizeof(%s)); %s->obj.cls = &cls_%s;",
		cname(cl)+"*", n, cname(cl)+"*", cname(cl), n, mangle(cl.Full))
	if cl.Inner && cl.OuterField != nil {
		fmt.Fprintf(&b, " %s->f_%s = (%s*)%s;", n, mangle(cl.OuterField.Name), cname(cl.Outer), e.outerArg(v, cl.Outer))
	}
	if init := e.clinitStmt(cl); init != "" {
		fmt.Fprintf(&b, " %s", strings.TrimSuffix(init, "\n"))
	}
	if v.Ctor != nil {
		fmt.Fprintf(&b, " %s(%s);", e.cfunc(v.Ctor), e.argsWithCaptures(n, v, cl))
	}
	fmt.Fprintf(&b, " %s; })", n)
	return b.String()
}

// argsWithCaptures builds the argument list of a constructor call. A class
// declared inside a method captures the locals of it that its body uses -- an
// anonymous class and a local class both -- and receives them as trailing
// parameters after the ones it declares.
func (e *Emitter) argsWithCaptures(n string, v *ast.New, cl *ast.Class) string {
	a := e.args(n, v.Args, v.Ctor)
	for _, cv := range e.prog.CapturedVars(cl) {
		if a != "" {
			a += ", "
		}
		a += e.coerce(e.localName(cv), cv.Type, cv.Type)
	}
	return a
}

// outerArg is the enclosing instance an inner class's new object is created
// with, as seen from the code doing the creating. It is not always `this`: a
// local class declared inside another local class reaches the outer instance
// through the chain of captured references, and `this` there is the innermost
// object -- storing it as the outer instance of a class further out reads
// whatever happens to lie at that offset.
func (e *Emitter) outerArg(v *ast.New, outer *ast.Class) string {
	if v.Outer != nil {
		return e.expr(v.Outer)
	}
	if outer == nil {
		return e.thisExpr()
	}
	return e.outerAccess(outer)
}

// elemClass is the C expression for the one class an array of elem promises its
// elements are, or "" when the element type is not a single class. A primitive
// array holds no references, and Teyru has one runtime class for every array
// type, so an array of arrays cannot name the class its elements have either.
func (e *Emitter) elemClass(elem ast.Type) string {
	switch t := e.prog.Erased(elem).(type) {
	case *ast.ClassType:
		return "&cls_" + mangle(t.Class.Full)
	case *ast.PrimType:
		// A primitive element records its class too, and not for the store
		// check -- a primitive array is stored into directly (refElemClass is
		// that one). It is what java.lang.reflect.Array reads to know which box
		// to build: without it int[] and Object[] are indistinguishable at run
		// time.
		return "&cls_" + primClassName(t.Kind)
	}
	return ""
}

// refElemClass is elemClass for the store check, which only has anything to
// compare for an array of references: a value stored into an int[] is not an
// object and has no class to test against the promise.
func (e *Emitter) refElemClass(elem ast.Type) string {
	if _, prim := e.prog.Erased(elem).(*ast.PrimType); prim {
		return ""
	}
	return e.elemClass(elem)
}

// elemPromise renders the statement that records what an array promises for its
// elements, which is what the store check in tyrt.h compares a value against,
// or "" when the element type names no single class (see elemClass). An array
// created without one is stored into unchecked, as every array was before the
// promise was recorded.
func (e *Emitter) elemPromise(name string, elem ast.Type) string {
	if c := e.elemClass(elem); c != "" {
		return " " + name + "->elemcls = " + c + ";"
	}
	return ""
}

func (e *Emitter) newArray(v *ast.NewArray) string {
	// The element type is one dimension inside the type the expression has, not
	// the bare name the parser kept: `v.Elem.Resolved` for `new int[2][2]` is
	// `int`, while the array being created is an int[][] whose elements are
	// int[]. Compiling the outer array as an array of ints wrote four-byte
	// integers where the collector expects pointers.
	elem := v.Elem.Resolved
	if at, ok := v.GetType().(*ast.ArrayType); ok {
		elem = at.Elem
	}
	if v.Init != nil {
		return e.arrayInitOf(v.Init, elem)
	}
	return e.newArrayDims(elem, v.Dims)
}

// newArrayDims allocates an array whose elements are elem, with dims the
// dimensions whose length was written. Only the written dimensions allocate
// (JLS 15.10.1): `new int[2][]` is two null int[]s and builds no inner arrays
// at all, because the empty brackets are part of the type rather than a level
// to construct.
func (e *Emitter) newArrayDims(elem ast.Type, dims []ast.Expr) string {
	dim := "0"
	if len(dims) > 0 {
		dim = e.expr(dims[0])
	}
	if len(dims) <= 1 {
		es := e.elemSize(elem)
		refs := "0"
		if e.isRefElem(elem) {
			refs = "1"
		}
		promise := e.elemPromise("_a", elem)
		return "({ tyarr* _a = ty_array_new(" + dim + ", " + es + "); _a->refs = " + refs + ";" + promise + " _a; })"
	}
	// Another dimension follows, so this level holds arrays: its slots are
	// references, each one a freshly allocated array of the element type. Only
	// the innermost array carries a promise about its elements.
	inner := elem
	if at, ok := elem.(*ast.ArrayType); ok {
		inner = at.Elem
	}
	alloc := e.newArrayDims(inner, dims[1:])
	n := e.tmpName()
	i := e.tmpName()
	var b strings.Builder
	fmt.Fprintf(&b, "({ tyarr* %s = ty_array_new(%s, 8); %s->refs = 1;", n, dim, n)
	fmt.Fprintf(&b, " for (int64_t %s = 0; %s < %s->len; %s++) ((void**)%s->data)[%s] = (void*)(%s);", i, i, n, i, n, i, alloc)
	fmt.Fprintf(&b, " %s; })", n)
	return b.String()
}

func (e *Emitter) arrayInit(v *ast.ArrayInit) string {
	return e.arrayInitOf(v, v.Elem)
}

func (e *Emitter) arrayInitOf(v *ast.ArrayInit, elem ast.Type) string {
	es := e.elemSize(elem)
	refs := "0"
	if e.isRefElem(elem) {
		refs = "1"
	}
	var b strings.Builder
	n := e.tmpName()
	fmt.Fprintf(&b, "({ tyarr* %s = ty_array_new(%d, %s); %s->refs = %s;%s", n, len(v.Elems), es, n, refs, e.elemPromise(n, elem))
	for i, el := range v.Elems {
		val := e.arrayElemValue(el, elem)
		if e.isRefElem(elem) {
			fmt.Fprintf(&b, " ((void**)%s->data)[%d] = (void*)%s;", n, i, val)
		} else {
			fmt.Fprintf(&b, " ((%s*)%s->data)[%d] = %s;", e.ctype(elem), n, i, val)
		}
	}
	fmt.Fprintf(&b, " %s; })", n)
	return b.String()
}

func (e *Emitter) arrayElemValue(el ast.Expr, elem ast.Type) string {
	if ai, ok := el.(*ast.ArrayInit); ok {
		// a nested initializer builds an array of the element type one level
		// deeper, which is the type of the initializer itself
		if at, ok2 := el.GetType().(*ast.ArrayType); ok2 {
			return e.arrayInitOf(ai, at.Elem)
		}
		return e.arrayInitOf(ai, elem)
	}
	return e.coerce(e.expr(el), el.GetType(), elem)
}

// ---------------------------------------------------------------- lambdas

func (e *Emitter) lambdaExpr(lam *ast.Lambda) string {
	cl := lam.Class
	if cl == nil {
		return "NULL"
	}
	n := e.tmpName()
	var b strings.Builder
	fmt.Fprintf(&b, "({ %s %s = (%s)ty_alloc(sizeof(%s)); %s->obj.cls = &cls_%s;",
		cname(cl)+"*", n, cname(cl)+"*", cname(cl), n, mangle(cl.Full))
	for _, v := range e.prog.CapturedVars(cl) {
		if lam.RecvVar == v && lam.RecvExpr != nil {
			// the receiver of a bound method reference is evaluated here and
			// once only, so the closure sees a stable object
			fmt.Fprintf(&b, " %s->cap_%s = %s;", n, mangle(v.Name), e.refExpr(lam.RecvExpr))
			continue
		}
		if f := e.capThisField(lam); f != nil && f.Name == v.Name {
			// the enclosing instance comes from the same place a use of `this`
			// inside this body does: the closure around it, or the method the
			// lambda was created in
			fmt.Fprintf(&b, " %s->cap_%s = %s;", n, mangle(v.Name), e.thisExpr())
			continue
		}
		fmt.Fprintf(&b, " %s->cap_%s = %s;", n, mangle(v.Name), e.localName(v))
	}
	fmt.Fprintf(&b, " %s; })", n)
	return b.String()
}

func (e *Emitter) emitLambdaMethod(cl *ast.Class, m *ast.Method) {
	lam := m.Lambda
	if lam == nil || m.IsCtor {
		return
	}
	fmt.Fprintf(&e.fns, "static %s;\n", e.signature(m))
	e.indent = 0
	fmt.Fprintf(e.code, "static %s {\n", e.signature(m))
	e.indent++
	for i, pv := range m.ParamVars {
		e.locals[pv] = fmt.Sprintf("a%d", i)
	}
	// Captured locals live in fields of the synthetic lambda class. From the
	// body's point of view `this` is the instance the lambda was created in
	// (JLS 15.27.2), so the class in scope is the one the body was written in,
	// not the closure class: a bare field name and an enclosing-class reference
	// have to resolve the way they do in that class.
	prevLambda, prevClass, prevRet := e.curLambda, e.curClass, e.retType
	e.curLambda = lam
	// A return inside a block-bodied lambda returns from the lambda, so the
	// copy it has to match is the functional method's result -- not whatever
	// method the lambda happens to be written in. Without this the emitted
	// `return` coerced to the enclosing method's type and a lambda answering
	// Object returned a C_teyru_HttpResponse*, which does not compile.
	e.retType = m.Result
	if enc := e.enclosureOf(lam); enc != nil {
		e.curClass = enc
	}
	defer func() { e.curLambda, e.curClass, e.retType = prevLambda, prevClass, prevRet }()
	for v, f := range cl.CapFields {
		e.locals[v] = "this->cap_" + mangle(f.Name)
	}
	switch b := lam.Body.(type) {
	case ast.Expr:
		if lam.ExprStmt {
			e.line("%s;\n", e.expr(b))
		} else {
			e.line("return %s;\n", e.coerce(e.expr(b), b.GetType(), m.Result))
		}
	case *ast.Block:
		e.emitBlockInner(b)
	}
	e.indent--
	e.code.WriteString("}\n\n")
}
