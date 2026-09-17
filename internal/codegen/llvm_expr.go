package codegen

// Expression lowering for the LLVM back end. Every function here answers with
// an operand and a Teyru type, and every rule is the C back end's: the same
// coercions, the same runtime helpers for the operations C would get wrong
// (integer division that throws, string concatenation, shifts masked to the bits
// Java leaves significant), and the same null and bounds tests in the same
// order, because they are what a program observes.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// cEmitter builds a C back end emitter over this program, for the handful of
// decisions the two back ends share: which native binding a method has, whether
// that binding is the implementation that runs, and how a PrintStream's print
// methods become the stream-aware twins. Those are data -- nativeTable and the
// tables beside it -- and a second copy of them is a second thing to keep true.
func (e *llvmEmitter) cEmitter() *Emitter { return &Emitter{prog: e.p} }

// expr lowers an expression.
func (e *llvmEmitter) expr(f *fb, x ast.Expr) lval {
	if x == nil {
		return value("0", nil)
	}
	if p := x.GetPos(); p.File != nil {
		e.curPos = p
	}
	if v, ok := e.p.Lowered(x); ok {
		return e.expr(f, v)
	}
	switch v := x.(type) {
	case *ast.Literal:
		return e.literal(f, v)
	case *ast.Ident:
		return e.ident(f, v)
	case *ast.Select:
		return e.selectExpr(f, v)
	case *ast.Index:
		return e.indexExpr(f, v)
	case *ast.Call:
		if v.ThisCtor {
			// a this()/super() call belongs to the constructor's body
			return value("0", nil)
		}
		return e.callExpr(f, v)
	case *ast.New:
		return e.newExpr(f, v)
	case *ast.NewArray:
		return e.newArray(f, v)
	case *ast.ArrayInit:
		return e.arrayInit(f, v, v.Elem)
	case *ast.Unary:
		return e.unary(f, v)
	case *ast.Binary:
		return e.binary(f, v)
	case *ast.Assign:
		return e.assign(f, v)
	case *ast.Cond:
		return e.condExpr(f, v)
	case *ast.Cast:
		return e.cast(f, v)
	case *ast.InstanceOf:
		return e.instanceOf(f, v)
	case *ast.Conv:
		return e.coerce(f, e.expr(f, v.X), v.GetType())
	case *ast.This:
		if v.Qual != "" {
			e.refuse(v.GetPos(), "an enclosing instance (%s.this): the llvm back end has no inner-class layout", v.Qual)
		}
		return value(e.selfOperand(f), v.GetType())
	case *ast.SuperExpr:
		return value("%this", v.GetType())
	case *ast.SwitchExpr:
		return e.switchExpr(f, v)
	case *ast.ClassLit:
		return e.classObject(f, v)
	case *ast.Lambda:
		return e.lambdaExpr(f, v)
	case *ast.MethodRef:
		if v.Lam == nil {
			e.refuse(v.GetPos(), "a method reference the checker did not desugar")
		}
		return e.lambdaExpr(f, v.Lam)
	}
	e.refuse(x.GetPos(), "the expression %T: the llvm back end does not lower it", x)
	return value("0", nil)
}

func (e *llvmEmitter) switchExpr(f *fb, v *ast.SwitchExpr) lval {
	t := v.GetType()
	slot := f.tempSlot(t, "swres")
	e.store(f, slot, t, f.zero(t))
	e.switchStmt(f, v.S, slot, t)
	return e.load(f, slot, t)
}

// ---------------------------------------------------------------- literals

func (e *llvmEmitter) literal(f *fb, l *ast.Literal) lval {
	switch l.Kind {
	case ast.LitInt:
		return value(fmt.Sprint(int32(l.Int)), ast.TInt)
	case ast.LitLong:
		return value(fmt.Sprint(int64(l.Int)), ast.TLong)
	case ast.LitFloat:
		return value(floatLit(l.Flt, true), ast.TFloat)
	case ast.LitDouble:
		return value(floatLit(l.Flt, false), ast.TDouble)
	case ast.LitChar:
		return value(fmt.Sprint(uint16(l.Int)), ast.TChar)
	case ast.LitString:
		return value(e.teyruString(l.Str), l.GetType())
	case ast.LitBool:
		if l.Bool {
			return value("1", ast.TBoolean)
		}
		return value("0", ast.TBoolean)
	case ast.LitNull:
		return value("null", l.GetType())
	}
	e.refuse(l.GetPos(), "the literal kind %d: the llvm back end does not lower it", l.Kind)
	return value("0", nil)
}

// floatLit renders a floating point constant as its bit pattern.
//
// The bit pattern rather than a decimal spelling, because LLVM parses a decimal
// floating point literal as a double and then requires it to be a value the
// operand's type can hold: `float 1.401298464e-45` -- the nine significant
// digits that round-trip a float32 -- is rejected as "floating point constant
// invalid for type", and only the seventeen-digit form of the double it came
// from is accepted. Nine digits look right and are wrong; the bits cannot be.
//
// A `single` constant is the float32 the program really has, widened for the
// literal, so that the constant and the value the program computes are the same
// number.
func floatLit(v float64, single bool) string {
	if single {
		v = float64(float32(v))
	}
	return fmt.Sprintf("0x%X", math.Float64bits(v))
}

// ---------------------------------------------------------------- names

func (e *llvmEmitter) ident(f *fb, v *ast.Ident) lval {
	switch r := v.Ref.(type) {
	case *ast.Field:
		if r == nil {
			return e.zeroValue(v.GetType())
		}
		r = e.canonicalField(r)
		if c, ok := e.constField(f, r); ok {
			return c
		}
		if r.Mods.Has(ast.ModStatic) {
			return e.staticField(f, r, v.GetType())
		}
		e.bareFieldCheck(f, r)
		return e.fieldRead(f, r, e.selfOperand(f), v.GetType())
	case *ast.Var:
		if r == nil {
			return e.zeroValue(v.GetType())
		}
		if r.Field != nil {
			return e.propertyStorageRead(f, r.Field, v.GetType())
		}
		return e.load(f, f.localSlot(r), v.GetType())
	case *ast.Class:
		e.refuse(v.GetPos(), "the class name %s used as a value", r.Full)
	}
	e.refuse(v.GetPos(), "the name %s: the llvm back end does not resolve it", v.Name)
	return value("0", nil)
}

// constField inlines a compile-time constant field, the way the C back end does:
// a static final String is the interned literal, and a static final numeric is
// the number.
func (e *llvmEmitter) constField(f *fb, fd *ast.Field) (lval, bool) {
	if !fd.Mods.Has(ast.ModStatic) || fd.ConstVal == nil {
		return lval{}, false
	}
	if s, ok := e.p.ConstantString(fd); ok {
		return value(e.teyruString(s), fd.Type), true
	}
	lit, ok := e.p.ConstantLiteral(fd)
	if !ok {
		return lval{}, false
	}
	switch t := e.p.Erased(fd.Type).(type) {
	case *ast.PrimType:
		switch t.Kind {
		case ast.Int:
			if n, err := strconv.ParseInt(strings.TrimSuffix(lit, "LL"), 10, 32); err == nil {
				return value(fmt.Sprint(n), fd.Type), true
			}
		case ast.Long:
			if n, err := strconv.ParseInt(strings.TrimSuffix(lit, "LL"), 10, 64); err == nil {
				return value(fmt.Sprint(n), fd.Type), true
			}
		case ast.Float, ast.Double:
			if v, err := strconv.ParseFloat(strings.TrimSuffix(lit, "f"), 64); err == nil {
				return value(floatLit(v, t.Kind == ast.Float), fd.Type), true
			}
		}
	}
	return lval{}, false
}

func (e *llvmEmitter) selectExpr(f *fb, v *ast.Select) lval {
	switch r := v.Ref.(type) {
	case *ast.Field:
		if r == nil {
			return e.zeroValue(v.GetType())
		}
		r = e.canonicalField(r)
		if c, ok := e.constField(f, r); ok {
			return c
		}
		if r.Mods.Has(ast.ModStatic) {
			return e.staticField(f, r, v.GetType())
		}
		return e.fieldRead(f, r, e.expr(f, v.X).v, v.GetType())
	case string:
		if r == "length" {
			// an array's length, with Java's null test first
			arr := e.expr(f, v.X)
			e.useArray()
			e.nullCheck(f, arr.v)
			return value(e.loadRaw(f, "i64", e.gepBytes(f, arr.v, 8), 8), ast.TLong)
		}
	case *ast.Class:
		e.refuse(v.GetPos(), "the nested class name %s used as a value", r.Full)
	}
	e.refuse(v.GetPos(), "the selection .%s: the llvm back end does not resolve it", v.Name)
	return value("0", nil)
}

// staticField reads a static field through its global, initializing the class
// first, which is when its initializer runs.
func (e *llvmEmitter) staticField(f *fb, fd *ast.Field, t ast.Type) lval {
	e.needClass(fd.Owner)
	e.clinitIfNeeded(f, fd.Owner)
	return e.load(f, e.staticGlobal(fd.Owner, fd), t)
}

// fieldRead reads an instance field, testing the receiver first when it is not
// `this`: reading through a null reference is a NullPointerException in Java and
// a load from address zero in C.
func (e *llvmEmitter) fieldRead(f *fb, fd *ast.Field, recv string, t ast.Type) lval {
	if fd.Owner == nil {
		return e.zeroValue(t)
	}
	return e.load(f, e.fieldAddr(f, fd, recv, recv != "%this"), t)
}

// propertyStorageRead is the `field` a property accessor reads: a static
// property's storage is a global -- its accessor is a static method with no
// `this` to reach it through -- and an instance property's is a field of the
// object the body runs on.
func (e *llvmEmitter) propertyStorageRead(f *fb, fd *ast.Field, t ast.Type) lval {
	fd = e.canonicalField(fd)
	if fd.Mods.Has(ast.ModStatic) {
		return e.staticField(f, fd, t)
	}
	return e.fieldRead(f, fd, e.selfOperand(f), t)
}

// propertyStorageAddr is the same storage as an address, for an assignment to
// `field`.
func (e *llvmEmitter) propertyStorageAddr(f *fb, fd *ast.Field) string {
	fd = e.canonicalField(fd)
	if fd.Mods.Has(ast.ModStatic) {
		e.needClass(fd.Owner)
		e.clinitIfNeeded(f, fd.Owner)
		return e.staticGlobal(fd.Owner, fd)
	}
	return e.fieldAddr(f, fd, e.selfOperand(f), false)
}

// canonicalField is the field object the class itself holds, found by name.
//
// A reference in a body can arrive carrying a field object the checker built for
// that use rather than the one the class was laid out with, and the two must not
// disagree: whether a field is static decides between a global and an offset, and
// a copy that lost the flag reads the wrong storage entirely. The class's own
// field is the one the layout and the class table were built from.
func (e *llvmEmitter) canonicalField(fd *ast.Field) *ast.Field {
	if fd == nil || fd.Owner == nil {
		return fd
	}
	if canon := fd.Owner.FieldMap[fd.Name]; canon != nil {
		return canon
	}
	for _, f := range fd.Owner.Fields {
		if f.Name == fd.Name {
			return f
		}
	}
	return fd
}

// bareFieldCheck refuses a bare field name that belongs to neither the class
// being compiled nor one of its supertypes: an unqualified name is a field of
// the instance the code runs on, and when it belongs to an enclosing class the
// compiler has to walk the enclosing-instance chain to reach it. A field read
// through an explicit receiver is not this case -- `m.end` names another object,
// and its offset is the one the receiver's own table gives it.
func (e *llvmEmitter) bareFieldCheck(f *fb, fd *ast.Field) {
	body := f.fn
	owner := (*ast.Class)(nil)
	if body != nil {
		owner = body.Owner
	}
	if f.bodyClass != nil {
		// a lambda body is written in the enclosing class, and that is where its
		// bare names resolve -- the closure class has no fields but its captures
		owner = f.bodyClass
	}
	if owner == nil || fd.Owner == nil {
		return
	}
	if owner != fd.Owner && !isSubclass(owner, fd.Owner) {
		e.refuse(noPos, "the bare name %s, a field of another class (%s), read from %s: the llvm back end does not lower enclosing-instance access",
			fd.Name, fd.Owner.Full, owner.Full)
	}
}

// fieldAddr is the address of an instance field. It comes from the class's
// emitted struct type, whose fields were laid out with util.FieldOffsets, so the
// offset is the one the collector was told about.
func (e *llvmEmitter) fieldAddr(f *fb, fd *ast.Field, recv string, check bool) string {
	fd = e.canonicalField(fd)
	cl := fd.Owner
	e.needClass(cl)
	idx, ok := e.fieldStructIndex(cl, fd)
	if !ok {
		e.refuse(noPos, "the field %s of %s is not in the instance layout", fd.Name, cl.Full)
	}
	if check {
		e.nullCheck(f, recv)
	}
	r := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds %s, ptr %s, i32 0, i32 %d",
		r, e.structType(cl), recv, idx))
	return r
}

// ---------------------------------------------------------------- loads and stores

func (e *llvmEmitter) load(f *fb, ptr string, t ast.Type) lval {
	if t == nil || t == ast.TVoid {
		return value("0", t)
	}
	return value(e.loadRaw(f, e.llvmType(t), ptr, alignOf(t, e)), t)
}

func (e *llvmEmitter) loadRaw(f *fb, ty, ptr string, align int64) string {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = load %s%s, ptr %s, align %d", r, volOf(f, ptr), ty, ptr, align))
	return r
}

// volOf is `volatile ` for an access the optimiser must not keep in a register:
// a slot of a function that has opened a try frame.
func volOf(f *fb, ptr string) string {
	if f.volatile && f.slots[ptr] {
		return "volatile "
	}
	return ""
}

func (e *llvmEmitter) store(f *fb, ptr string, t ast.Type, v lval) {
	if t == nil || t == ast.TVoid {
		return
	}
	want := e.llvmType(t)
	if got := e.llvmType(v.t); got != want {
		// A store of the wrong shape is a bug in the lowering, not in the
		// program, and emitting it would be a module that does not verify.
		e.refuse(noPos, "an internal lowerer error: storing %s where %s goes", got, want)
	}
	f.ins(fmt.Sprintf("store %s%s %s, ptr %s, align %d", volOf(f, ptr), want, v.v, ptr, alignOf(t, e)))
}

// gepBytes is a byte-offset address, which is how this back end reaches the
// runtime's own fields: their offsets are the ones the header gave them.
func (e *llvmEmitter) gepBytes(f *fb, base string, off int64) string {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds i8, ptr %s, i64 %d", r, base, off))
	return r
}

// gepAt indexes a byte pointer by a computed offset.
func (e *llvmEmitter) gepAt(f *fb, base, off string) string {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds i8, ptr %s, i64 %s", r, base, off))
	return r
}

// gepIndex is a typed index into an array of `ty`.
func (e *llvmEmitter) gepIndex(f *fb, ty, base, idx string) string {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds %s, ptr %s, i64 %s", r, ty, base, idx))
	return r
}

// nullCheck throws the way Java's null test does, and falls through when the
// reference is there.
func (e *llvmEmitter) nullCheck(f *fb, p string) {
	if p == "0" {
		// a reference the lowering answered with the integer zero: the two are
		// the same object, and only one of them parses here
		p = "null"
	}
	e.markExn("TY_NPE", e.p.Builtins.NPE)
	e.decl("ty_npe", "ptr", nil, "")
	ok := f.nextLabel("npe_ok")
	bad := f.nextLabel("npe_bad")
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, null", c, p))
	f.cbr(c, bad, ok)
	f.label(bad)
	f.ins("call ptr @ty_npe()")
	f.br(ok)
	f.label(ok)
}

// clinitIfNeeded runs a class's static initializer on first use, which is Java's
// lazy initialization: the flag test is inline, so the common case is a load and
// a branch.
func (e *llvmEmitter) clinitIfNeeded(f *fb, cl *ast.Class) {
	if cl == nil || cl == e.p.Builtins.Object || !needsClinit(cl, map[*ast.Class]bool{}) {
		return
	}
	e.decl("ty_clinit", "void", []string{"ptr"}, "")
	done := f.nextLabel("clinit_done")
	call := f.nextLabel("clinit_call")
	cls := "@cls_" + util.Mangle(cl.Full)
	flags := e.loadRaw(f, "i32", e.gepBytes(f, cls, 12), 4)
	and := f.reg()
	f.ins(fmt.Sprintf("%s = and i32 %s, 8", and, flags))
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, and))
	f.cbr(c, done, call)
	f.label(call)
	f.ins(fmt.Sprintf("call void @ty_clinit(ptr %s)", cls))
	f.br(done)
	f.label(done)
}

// ---------------------------------------------------------------- conversions

// coerce converts a value from one Teyru type to another: the primitives C
// converts implicitly, the boxes a boxed/unboxed boundary needs, and nothing at
// all between two references, which are both pointers.
func (e *llvmEmitter) coerce(f *fb, v lval, dst ast.Type) lval {
	src := v.t
	if src == nil || dst == nil || dst == ast.TVoid || ast.IsError(src) || ast.IsError(dst) {
		return v
	}
	src = e.p.Erased(src)
	dst = e.p.Erased(dst)
	sp, sIsPrim := src.(*ast.PrimType)
	dp, dIsPrim := dst.(*ast.PrimType)
	switch {
	case sIsPrim && dIsPrim:
		return value(e.convertTo(f, value(v.v, src), e.llvmType(dst)), dst)
	case sIsPrim && !dIsPrim:
		return e.boxValue(f, value(v.v, src), sp, dst)
	case !sIsPrim && dIsPrim:
		return e.unboxValue(f, value(v.v, src), src, dp)
	}
	return value(v.v, dst)
}

// boxValue boxes a primitive into the wrapper class, which is what a primitive
// handed to an Object parameter is.
func (e *llvmEmitter) boxValue(f *fb, v lval, sp *ast.PrimType, dst ast.Type) lval {
	cl := e.boxClassOf(sp.Kind)
	m := e.cEmitter().wrapperStatic(sp.Kind, "valueOf")
	if cl == nil || m == nil {
		e.refuse(noPos, "boxing a %s: the program has no wrapper for it", sp.String())
	}
	e.boxUsed[sp.Kind] = true
	// The wrapper builds its own instances now (lib/04_boxing.teyru), so the
	// class, its vtable slots and the body of valueOf all come into the module:
	// the runtime helper the module used to call is gone.
	e.instantiate(cl)
	e.clinitIfNeeded(f, cl)
	return value(e.directCall(f, m, "", []lval{v}).v, dst)
}

// unboxValue opens a box. The helper is chosen by the type the program converts
// off the box, exactly as the C back end does it: the accessor of the wrapper's
// own kind reads the payload -- intValue, longValue, ... -- and a destination
// of another primitive type is the conversion from that value. Java's unboxing
// is that call and then that conversion, which is why `double d = someInteger`
// is 11.0 and not a look at the integer's four bytes as a double.
func (e *llvmEmitter) unboxValue(f *fb, v lval, src ast.Type, dp *ast.PrimType) lval {
	ct, ok := src.(*ast.ClassType)
	if !ok {
		e.refuse(noPos, "reading a %s out of a %s: the llvm back end does not lower this conversion", dp.String(), src.String())
	}
	kind, isBox := e.p.Builtins.Unbox[ct.Class]
	box := ct.Class
	if !isBox {
		// A reference that is not a wrapper is narrowed to the wrapper first,
		// which is the checkcast javac puts in front of the same conversion:
		// `(int) someObject` is `((Integer) someObject).intValue()`.
		box = e.boxClassOf(dp.Kind)
		if box == nil {
			e.refuse(noPos, "reading a %s out of a %s: this program has no wrapper for it", dp.String(), ct.Class.Full)
		}
		kind = dp.Kind
		e.needClass(box)
		e.markExn("TY_CCE", e.p.Builtins.CCE)
		narrowed := e.rtCall(f, "ty_checkcast", e.classType(box), []lval{
			value(v.v, e.refType()),
			value("@cls_"+util.Mangle(box.Full), e.refType()),
		})
		v = value(narrowed.v, e.classType(box))
	}
	m := e.cEmitter().unboxAccessor(box, kind)
	if m == nil {
		e.refuse(noPos, "reading a %s out of a %s: the wrapper has no accessor for its own value", dp.String(), box.Full)
	}
	e.boxUsed[kind] = true
	// The accessor reads its own field, so a null reference throws here, where
	// Java throws it and where ty_unbox_int used to.
	e.nullCheck(f, v.v)
	res := e.directCall(f, m, v.v, nil)
	if kind == dp.Kind {
		return value(res.v, dp)
	}
	return value(e.convertTo(f, res, e.llvmType(dp)), dp)
}

// convertTo converts a lowered value to the LLVM type a prototype declares,
// which is the conversion the C compiler would make at the same call.
func (e *llvmEmitter) convertTo(f *fb, v lval, want string) string {
	from := e.llvmType(v.t)
	if from == want {
		return v.v
	}
	if want == "void" || from == "void" {
		e.refuse(noPos, "an internal lowerer error: converting a %s value to %s", from, want)
	}
	if from == "ptr" || want == "ptr" {
		e.refuse(noPos, "an internal lowerer error: converting %s to %s", from, want)
	}
	switch {
	case isIntLLVM(from) && isIntLLVM(want):
		if llvmWidth(want) > llvmWidth(from) {
			r := f.reg()
			op := "sext"
			if isUnsigned(v.t) {
				op = "zext"
			}
			f.ins(fmt.Sprintf("%s = %s %s %s to %s", r, op, from, v.v, want))
			return r
		}
		if llvmWidth(want) < llvmWidth(from) {
			r := f.reg()
			f.ins(fmt.Sprintf("%s = trunc %s %s to %s", r, from, v.v, want))
			return r
		}
		r := f.reg()
		f.ins(fmt.Sprintf("%s = bitcast %s %s to %s", r, from, v.v, want))
		return r
	case isIntLLVM(from) && isFloatLLVM(want):
		r := f.reg()
		op := "sitofp"
		if isUnsigned(v.t) {
			op = "uitofp"
		}
		f.ins(fmt.Sprintf("%s = %s %s %s to %s", r, op, from, v.v, want))
		return r
	case isFloatLLVM(from) && isIntLLVM(want):
		return e.saturatingCast(f, v, from, want)
	case isFloatLLVM(from) && isFloatLLVM(want):
		r := f.reg()
		if want == "double" {
			f.ins(fmt.Sprintf("%s = fpext %s %s to double", r, from, v.v))
		} else {
			f.ins(fmt.Sprintf("%s = fptrunc %s %s to float", r, from, v.v))
		}
		return r
	}
	e.refuse(noPos, "an internal lowerer error: no conversion from %s to %s", from, want)
	return v.v
}

// saturatingCast lowers a conversion from a floating-point value to an integral
// one the way JLS 5.1.3 defines it, through the runtime's helper.
//
// An fptosi is the same conversion C leaves undefined -- it is poison for a
// value that does not fit -- and four builds of `(long) 1.0e20` answered 160, 0,
// -9223372036854775808 and 48 before this, one of them from this back end. The
// helper takes a double and answers an int32_t or an int64_t: a float source is
// widened on the way in, which is exact and is the order the conversion is
// defined in, and a destination narrower than the helper's own width is the
// truncation of that int, which is the second of the two steps JLS states. A
// destination that is not narrower is the helper's answer unchanged.
func (e *llvmEmitter) saturatingCast(f *fb, v lval, from, want string) string {
	helper := "ty_d2i"
	if llvmWidth(want) > 32 {
		helper = "ty_d2l"
	}
	proto, ok := rtProtoOf(helper)
	if !ok {
		e.refuse(noPos, "converting a floating-point value to an integer: internal/runtime/src/tyrt.h does not declare %s", helper)
	}
	got := e.rtCall(f, helper, nil, []lval{v})
	if proto.ret == want {
		return got.v
	}
	r := f.reg()
	f.ins(fmt.Sprintf("%s = trunc %s %s to %s", r, proto.ret, got.v, want))
	return r
}

// convertFrom converts a value the runtime answered with into the Teyru type the
// expression has.
func (e *llvmEmitter) convertFrom(f *fb, reg, from string, to ast.Type) lval {
	if to == nil || to == ast.TVoid {
		return value(reg, to)
	}
	want := e.llvmType(to)
	if from == want {
		return value(reg, to)
	}
	if from == "ptr" || want == "ptr" {
		e.refuse(noPos, "the runtime answered with %s where the program expects %s", from, want)
	}
	return value(e.convertTo(f, value(reg, llvmPseudoType(from)), want), to)
}

// llvmPseudoType is a Teyru type that maps back to an LLVM type, for a value
// that came out of the runtime: the conversions between the primitive widths are
// the same either way, and only the signedness of a widening matters, which is
// the runtime's own (`int32_t` is signed).
func llvmPseudoType(ty string) ast.Type {
	switch ty {
	case "i8":
		return ast.TByte
	case "i16":
		return ast.TShort
	case "i32":
		return ast.TInt
	case "i64":
		return ast.TLong
	case "float":
		return ast.TFloat
	case "double":
		return ast.TDouble
	}
	return ast.TLong
}

func isIntLLVM(ty string) bool {
	return ty == "i8" || ty == "i16" || ty == "i32" || ty == "i64"
}

func isFloatLLVM(ty string) bool { return ty == "float" || ty == "double" }

func llvmWidth(ty string) int {
	switch ty {
	case "i8":
		return 8
	case "i16":
		return 16
	case "i32", "float":
		return 32
	}
	return 64
}

// isUnsigned reports whether a primitive is unsigned, which decides between a
// sign and a zero extension: a `char` widened to an int is zero-extended, a
// `byte` is sign-extended.
func isUnsigned(t ast.Type) bool {
	if p, ok := t.(*ast.PrimType); ok {
		return p.Kind == ast.Char || p.Kind == ast.Boolean
	}
	return false
}

// ---------------------------------------------------------------- operators

// operand coerces an operand of a numeric or bitwise operation to the type the
// operation is performed in, unboxing it first when it is a box.
func (e *llvmEmitter) operand(f *fb, x ast.Expr, op ast.Type) lval {
	v := e.expr(f, x)
	if op == nil {
		return v
	}
	return e.coerce(f, v, op)
}

// toBool lowers a controlling expression to an i1.
func (e *llvmEmitter) toBool(f *fb, x ast.Expr) string {
	v := e.expr(f, x)
	t := e.p.Erased(x.GetType())
	if p, ok := t.(*ast.PrimType); ok {
		if p.Kind != ast.Boolean {
			e.refuse(x.GetPos(), "a %s used as a condition: Teyru conditions are boolean", p.String())
		}
		r := f.reg()
		f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", r, v.v))
		return r
	}
	// a boxed Boolean is unboxed here: a condition has no declared type to
	// convert to, and a non-null box is true whatever it holds
	b := e.unboxValue(f, v, t, ast.TBoolean)
	r := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", r, b.v))
	return r
}

func (e *llvmEmitter) boolVal(f *fb, cond string) lval {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = zext i1 %s to i32", r, cond))
	return value(r, ast.TBoolean)
}

func (e *llvmEmitter) condExpr(f *fb, v *ast.Cond) lval {
	then := f.nextLabel("cond_then")
	els := f.nextLabel("cond_else")
	join := f.nextLabel("cond_end")
	f.cbr(e.toBool(f, v.C), then, els)
	f.label(then)
	x := e.coerce(f, e.expr(f, v.X), v.GetType())
	// The arm may have split its own block -- a cast inside it emits a check --
	// and the phi has to name the block that branches to the join, not the one
	// the arm started in.
	xFrom := f.cur
	f.br(join)
	f.label(els)
	y := e.coerce(f, e.expr(f, v.Y), v.GetType())
	yFrom := f.cur
	f.br(join)
	f.label(join)
	ty := e.llvmType(v.GetType())
	r := f.reg()
	f.ins(fmt.Sprintf("%s = phi %s [ %s, %%%s ], [ %s, %%%s ]", r, ty, x.v, xFrom, y.v, yFrom))
	return value(r, v.GetType())
}

func (e *llvmEmitter) unary(f *fb, v *ast.Unary) lval {
	// The operation is performed in the expression's own type, which is the
	// promoted one: negating a byte is an int negation in Java, and negating it
	// as a byte wraps where Java gives 128.
	t := e.p.Erased(v.GetType())
	if t == nil {
		t = e.p.Erased(v.X.GetType())
	}
	switch v.Op {
	case "+":
		x := e.expr(f, v.X)
		if p, ok := t.(*ast.PrimType); ok {
			if ct, isBox := e.asBox(x.t); isBox {
				// a boxed operand is unboxed and promoted, which is the type
				// the checker gave the expression
				return e.unboxValue(f, x, ct, p)
			}
		}
		return x
	case "-":
		x := e.expr(f, v.X)
		if ct, isBox := e.asBox(x.t); isBox {
			// As in the C back end: the operand is read through the wrapper's
			// own accessor and negated in the promoted type, so `-someByte` is
			// an int and `-someLong` a long.
			if p, ok := t.(*ast.PrimType); ok {
				x = e.unboxValue(f, x, ct, p)
			}
		}
		if p, ok := t.(*ast.PrimType); ok && p.IsIntegral() {
			// Negating the most negative value is undefined in C and defined in
			// Java, so it is a subtraction from zero, which wraps the way Java
			// says it does.
			x = e.coerce(f, x, t)
			r := f.reg()
			f.ins(fmt.Sprintf("%s = sub %s 0, %s", r, e.llvmType(t), x.v))
			return value(r, t)
		}
		if _, ok := t.(*ast.PrimType); !ok {
			e.refuse(v.GetPos(), "negating a %s: the llvm back end does not lower this operand", t.String())
		}
		x = e.coerce(f, x, t)
		r := f.reg()
		f.ins(fmt.Sprintf("%s = fneg %s %s", r, e.llvmType(t), x.v))
		return value(r, t)
	case "!":
		b := e.toBool(f, v.X)
		r := f.reg()
		f.ins(fmt.Sprintf("%s = xor i1 %s, true", r, b))
		return e.boolVal(f, r)
	case "~":
		// The complement is taken in the expression's own type, which is the
		// promoted one: complementing a long as an int throws away its upper
		// half, and that is not a wrong shift or a wrong mask somewhere later
		// -- it is a wrong value that every later round inherits.
		ct := t
		if _, ok := ct.(*ast.PrimType); !ok {
			ct = ast.TInt
		}
		x := e.coerce(f, e.expr(f, v.X), ct)
		r := f.reg()
		f.ins(fmt.Sprintf("%s = xor %s %s, -1", r, e.llvmType(ct), x.v))
		return value(r, ct)
	case "++", "--":
		return e.incDec(f, v)
	}
	e.refuse(v.GetPos(), "the operator %s: the llvm back end does not lower it", v.Op)
	return value("0", nil)
}

func (e *llvmEmitter) incDec(f *fb, v *ast.Unary) lval {
	ptr, t := e.addr(f, v.X)
	old := e.load(f, ptr, t)
	if ct, isBox := e.asBox(t); isBox {
		return e.boxedIncDec(f, v, ptr, ct, old)
	}
	if !e.isRef(t) {
		if _, ok := e.p.Erased(t).(*ast.PrimType); !ok {
			e.refuse(v.GetPos(), "an update of a %s", t.String())
		}
	}
	one := value("1", ast.TInt)
	if ast.IsPrim(e.p.Erased(t), ast.Long) {
		one = value("1", ast.TLong)
	} else if ast.IsPrim(e.p.Erased(t), ast.Float) {
		one = value("1.0e+00", ast.TFloat)
	} else if ast.IsPrim(e.p.Erased(t), ast.Double) {
		one = value("1.0e+00", ast.TDouble)
	}
	y := e.coerce(f, one, t)
	r := f.reg()
	if isFloatLLVM(e.llvmType(t)) {
		op := "fadd"
		if v.Op == "--" {
			op = "fsub"
		}
		f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, op, e.llvmType(t), old.v, y.v))
	} else {
		op := "add"
		if v.Op == "--" {
			op = "sub"
		}
		f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, op, e.llvmType(t), old.v, y.v))
	}
	next := value(r, t)
	e.store(f, ptr, t, next)
	if v.Postfix {
		return old
	}
	return next
}

// asBox is the wrapper class of a value's static type, when it is one of the
// eight wrappers: the operand of a unary operator, or the target of an update.
func (e *llvmEmitter) asBox(t ast.Type) (*ast.ClassType, bool) {
	ct, ok := e.p.Erased(t).(*ast.ClassType)
	if !ok || ct.Class == nil {
		return nil, false
	}
	if _, isBox := e.p.Builtins.Unbox[ct.Class]; !isBox {
		return nil, false
	}
	return ct, true
}

// boxedIncDec is `++` and `--` on a wrapper. Java unboxes the variable's
// wrapper, does the arithmetic in the promoted type and boxes the result into a
// *new* wrapper -- a wrapper is immutable, so another variable holding the old
// one must not see the change -- and stores that back. The value of the
// expression is the new wrapper for a prefix update and the old one for a
// postfix update.
func (e *llvmEmitter) boxedIncDec(f *fb, v *ast.Unary, ptr string, ct *ast.ClassType, old lval) lval {
	k, ok := e.p.Builtins.Unbox[ct.Class]
	if !ok {
		e.refuse(v.GetPos(), "an update of a %s", ct.Class.Full)
	}
	m := e.cEmitter().wrapperStatic(k, "valueOf")
	if m == nil {
		e.refuse(v.GetPos(), "an update of a %s: the program has no valueOf for it", ct.Class.Full)
	}
	promoted := promoteOf(k)
	// The read is unboxValue's: it tests the wrapper, so a null one raises
	// NullPointerException here rather than being dereferenced.
	val := e.unboxValue(f, old, ct, promoted)
	one := value("1", ast.TInt)
	switch promoted.Kind {
	case ast.Long:
		one = value("1", ast.TLong)
	case ast.Float:
		one = value("1.0e+00", ast.TFloat)
	case ast.Double:
		one = value("1.0e+00", ast.TDouble)
	}
	y := e.coerce(f, one, promoted)
	r := f.reg()
	op := "add"
	if v.Op == "--" {
		op = "sub"
	}
	if isFloatLLVM(e.llvmType(promoted)) {
		op = "f" + op
	}
	f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, op, e.llvmType(promoted), val.v, y.v))
	sum := value(r, promoted)
	own := &ast.PrimType{Kind: k}
	narrowed := value(e.convertTo(f, sum, e.llvmType(own)), own)
	e.clinitIfNeeded(f, ct.Class)
	next := e.directCall(f, m, "", []lval{narrowed})
	e.store(f, ptr, ct, next)
	if v.Postfix {
		return old
	}
	return next
}

// ---------------------------------------------------------------- binary

func (e *llvmEmitter) binary(f *fb, b *ast.Binary) lval {
	switch b.Op {
	case "&&", "||":
		return e.shortCircuit(f, b)
	case "==", "!=", "<", ">", "<=", ">=":
		return e.compare(f, b)
	case "+":
		if e.isStringType(b.GetType()) {
			return e.concat(f, b)
		}
	case "/":
		return e.divide(f, b)
	case "%":
		return e.remainder(f, b)
	case "<<", ">>", ">>>":
		return e.shift(f, b)
	}
	ot := b.OpType
	if ot == nil {
		ot = b.GetType()
	}
	x := e.operand(f, b.X, ot)
	y := e.operand(f, b.Y, ot)
	op := map[string]string{"+": "add", "-": "sub", "*": "mul", "&": "and", "|": "or", "^": "xor"}[b.Op]
	if op == "" {
		e.refuse(b.GetPos(), "the operator %s: the llvm back end does not lower it", b.Op)
	}
	r := f.reg()
	f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, arithOp(op, e.llvmType(ot)), e.llvmType(ot), x.v, y.v))
	return value(r, b.GetType())
}

func (e *llvmEmitter) shortCircuit(f *fb, b *ast.Binary) lval {
	// The right operand runs only when the left does not decide the answer, so
	// it lives in its own block and the value is the phi of the two paths.
	rhs := f.nextLabel("sc_rhs")
	join := f.nextLabel("sc_end")
	l := e.toBool(f, b.X)
	// the block the left operand decided in, which is the one the phi names --
	// reading it before the test would name a block the test may have split
	from := f.cur
	if b.Op == "&&" {
		f.cbr(l, rhs, join)
	} else {
		f.cbr(l, join, rhs)
	}
	f.label(rhs)
	rw := e.boolVal(f, e.toBool(f, b.Y))
	// the right operand may have split its own block, and the phi names the one
	// that branches
	to := f.cur
	f.br(join)
	f.label(join)
	res := f.reg()
	f.ins(fmt.Sprintf("%s = phi i32 [ %d, %%%s ], [ %s, %%%s ]",
		res, boolInt(b.Op == "||"), from, rw.v, to))
	return value(res, ast.TBoolean)
}

// arithOp is the LLVM instruction for an arithmetic operator: a floating point
// operation is the f-prefixed form of the integer one, and the bitwise ones have
// no floating point form at all.
func arithOp(op, ty string) string {
	if !isFloatLLVM(ty) {
		return op
	}
	switch op {
	case "add":
		return "fadd"
	case "sub":
		return "fsub"
	case "mul":
		return "fmul"
	}
	return op
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (e *llvmEmitter) isStringType(t ast.Type) bool {
	ct, ok := e.p.Erased(t).(*ast.ClassType)
	return ok && ct.Class != nil && ct.Class.Special == "String"
}

func (e *llvmEmitter) compare(f *fb, b *ast.Binary) lval {
	lt := e.p.Erased(b.X.GetType())
	rt := e.p.Erased(b.Y.GetType())
	_, lp := lt.(*ast.PrimType)
	_, rp := rt.(*ast.PrimType)
	switch {
	case lp && rp:
		ot := b.OpType
		if ot == nil {
			ot = lt
		}
		x := e.coerce(f, e.expr(f, b.X), ot)
		y := e.coerce(f, e.expr(f, b.Y), ot)
		return e.compareValues(f, b.Op, x, y, ot)
	case lp != rp:
		// one side is a box: the comparison is between the values
		prim := rt
		if lp {
			prim = lt
		}
		x := e.coerce(f, e.expr(f, b.X), prim)
		y := e.coerce(f, e.expr(f, b.Y), prim)
		return e.compareValues(f, b.Op, x, y, prim)
	}
	// two references: identity, which is what Java's == means for them
	x := e.expr(f, b.X)
	y := e.expr(f, b.Y)
	r := f.reg()
	f.ins(fmt.Sprintf("%s = icmp %s ptr %s, %s", r, eqCmp(b.Op), x.v, y.v))
	return e.boolVal(f, r)
}

// eqCmp is the integer comparison operator for a Java comparison, which is
// `eq`/`ne` whenever the operands are not ordered.
func eqCmp(op string) string {
	if op == "!=" {
		return "ne"
	}
	return "eq"
}

func (e *llvmEmitter) compareValues(f *fb, op string, x, y lval, ot ast.Type) lval {
	cmp := map[string]string{"==": "eq", "!=": "ne", "<": "lt", ">": "gt", "<=": "le", ">=": "ge"}[op]
	ty := e.llvmType(ot)
	r := f.reg()
	if isFloatLLVM(ty) {
		// Java's ordered comparisons are false for a NaN operand, `!=` included,
		// which is what the `o` says
		f.ins(fmt.Sprintf("%s = fcmp o%s %s %s, %s", r, cmp, ty, x.v, y.v))
	} else {
		f.ins(fmt.Sprintf("%s = icmp %s%s %s %s, %s", r, unsignedCmpPrefix(cmp, ot), cmp, ty, x.v, y.v))
	}
	return e.boolVal(f, r)
}

// unsignedCmpPrefix makes an *ordered* integer comparison unsigned when the
// operand is a char or a boolean, which is what C's usual arithmetic conversions
// do with a uint16_t. Equality has no signed form, so it takes no prefix: an
// "icmp seq" is not a predicate.
func unsignedCmpPrefix(op string, t ast.Type) string {
	if op == "eq" || op == "ne" {
		return ""
	}
	if isUnsigned(t) {
		return "u"
	}
	return "s"
}

func (e *llvmEmitter) divide(f *fb, b *ast.Binary) lval {
	ot := b.OpType
	x := e.operand(f, b.X, ot)
	y := e.operand(f, b.Y, ot)
	if ast.IsPrim(ot, ast.Long) {
		e.markExn("TY_ARITH", e.p.Builtins.Arith)
		return e.rtCall(f, "ty_div_long", b.GetType(), []lval{x, y})
	}
	if isFloating(ot) {
		r := f.reg()
		f.ins(fmt.Sprintf("%s = fdiv %s %s, %s", r, e.llvmType(ot), x.v, y.v))
		return value(r, b.GetType())
	}
	e.markExn("TY_ARITH", e.p.Builtins.Arith)
	return e.rtCall(f, "ty_div_int", b.GetType(), []lval{x, y})
}

func (e *llvmEmitter) remainder(f *fb, b *ast.Binary) lval {
	ot := b.OpType
	x := e.operand(f, b.X, ot)
	y := e.operand(f, b.Y, ot)
	if ast.IsPrim(ot, ast.Long) {
		e.markExn("TY_ARITH", e.p.Builtins.Arith)
		return e.rtCall(f, "ty_rem_long", b.GetType(), []lval{x, y})
	}
	if isFloating(ot) {
		r := f.reg()
		f.ins(fmt.Sprintf("%s = frem %s %s, %s", r, e.llvmType(ot), x.v, y.v))
		return value(r, b.GetType())
	}
	e.markExn("TY_ARITH", e.p.Builtins.Arith)
	return e.rtCall(f, "ty_rem_int", b.GetType(), []lval{x, y})
}

// shift lowers <<, >> and >>>. JLS 15.19 leaves only the low five bits of an
// int's count (six for a long) significant, and C leaves a shift whose count
// reaches the operand width undefined -- which LLVM makes poison -- so the count
// is masked here, exactly as the C back end masks it.
func (e *llvmEmitter) shift(f *fb, b *ast.Binary) lval {
	ot := b.OpType
	if ot == nil {
		ot = shiftType(b.GetType())
	}
	ot = e.p.Erased(ot)
	width := llvmWidth(e.llvmType(ot))
	x := e.coerce(f, e.expr(f, b.X), ot)
	y := e.coerce(f, e.expr(f, b.Y), ot)
	mask := f.reg()
	f.ins(fmt.Sprintf("%s = and %s %s, %d", mask, e.llvmType(ot), y.v, width-1))
	r := f.reg()
	switch b.Op {
	case "<<":
		f.ins(fmt.Sprintf("%s = shl %s %s, %s", r, e.llvmType(ot), x.v, mask))
	case ">>":
		f.ins(fmt.Sprintf("%s = ashr %s %s, %s", r, e.llvmType(ot), x.v, mask))
	default:
		f.ins(fmt.Sprintf("%s = lshr %s %s, %s", r, e.llvmType(ot), x.v, mask))
	}
	return value(r, b.GetType())
}

// concat lowers Java's string concatenation: the left operand as a string, then
// every part of the right spine, folded with ty_str_concat.
func (e *llvmEmitter) concat(f *fb, b *ast.Binary) lval {
	parts := []string{e.stringOperand(f, b.X)}
	collect := func(y ast.Expr) {}
	collect = func(y ast.Expr) {
		if nb, ok := y.(*ast.Binary); ok && nb.Op == "+" && e.isStringType(nb.GetType()) {
			collect(nb.X)
			collect(nb.Y)
			return
		}
		parts = append(parts, e.stringOperand(f, y))
	}
	collect(b.Y)
	return value(e.concatParts(f, parts), b.GetType())
}

// concatParts folds already-lowered string operands with the runtime's
// concatenation.
func (e *llvmEmitter) concatParts(f *fb, parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out = e.rtCall(f, "ty_str_concat", e.stringType(), []lval{
			value(out, e.stringType()),
			value(p, e.stringType()),
		}).v
	}
	return out
}

// stringOperand renders one part of a concatenation as a String, converting a
// primitive with the runtime's own formatting so that `1 + ""` reads the way
// Java's reads.
func (e *llvmEmitter) stringOperand(f *fb, x ast.Expr) string {
	v := e.expr(f, x)
	t := e.p.Erased(x.GetType())
	if e.isStringType(t) {
		return v.v
	}
	if p, ok := t.(*ast.PrimType); ok {
		switch p.Kind {
		case ast.Boolean:
			return e.rtCall(f, "ty_str_of_bool", e.stringType(), []lval{v}).v
		case ast.Char:
			return e.rtCall(f, "ty_str_of_char", e.stringType(), []lval{v}).v
		case ast.Float:
			return e.rtCall(f, "ty_str_of_float", e.stringType(), []lval{v}).v
		case ast.Double:
			return e.rtCall(f, "ty_str_of_double", e.stringType(), []lval{v}).v
		case ast.Byte, ast.Short, ast.Int, ast.Long:
			return e.rtCall(f, "ty_str_of_long", e.stringType(), []lval{e.coerce(f, v, ast.TLong)}).v
		}
	}
	return e.rtCall(f, "ty_str_of_obj", e.stringType(), []lval{v}).v
}

// classObject lowers a class literal to the Class object it denotes.
//
// The value is built by the runtime's ty_class_of_cls, which reads the class the
// object names out of the header of the object it is handed: what a literal
// passes is a pointer to a wrapper whose first word is that class. The wrapper
// is static data, so a literal costs one word in the executable and no
// allocation, and the wrappers the runtime caches per class make two literals
// for one class the same object.
func (e *llvmEmitter) classObject(f *fb, v *ast.ClassLit) lval {
	clsTy, ok := e.p.Erased(v.GetType()).(*ast.ClassType)
	if !ok {
		e.refuse(v.GetPos(), "a class literal whose type is not a Class value")
	}
	target := ""
	switch t := e.p.Erased(v.Type.Resolved).(type) {
	case *ast.ClassType:
		e.needClass(t.Class)
		target = "@cls_" + util.Mangle(t.Class.Full)
	case *ast.ArrayType:
		cl := e.useArray()
		e.needClass(cl)
		target = "@cls_" + util.Mangle(cl.Full)
	case *ast.PrimType:
		// a primitive is not one of the program's classes: no value is ever an
		// instance of one, so its class is a table of its own
		target = e.needPrimClass(t.Kind)
		e.instantiate(clsTy.Class)
		return e.rtCall(f, "ty_class_of_cls", v.GetType(), []lval{
			value(e.classWrapper(target), e.refType()),
			value("@cls_"+util.Mangle(clsTy.Class.Full), e.refType()),
		})
	default:
		e.refuse(v.GetPos(), "a class literal for a %s", v.Type.Resolved.String())
	}
	e.instantiate(clsTy.Class)
	return e.rtCall(f, "ty_class_of_cls", v.GetType(), []lval{
		value(e.classWrapper(target), e.refType()),
		value("@cls_"+util.Mangle(clsTy.Class.Full), e.refType()),
	})
}

// classWrapper interns the object a class literal is built from: a tyobj whose
// class pointer is the class the literal names.
func (e *llvmEmitter) classWrapper(target string) string {
	if n, ok := e.classWraps[target]; ok {
		return n
	}
	name := fmt.Sprintf("@.cw%d", len(e.classWraps))
	e.classWraps[target] = name
	fmt.Fprintf(&e.globals, "%s = internal global ptr %s, align 8\n", name, target)
	return name
}

// ---------------------------------------------------------------- casts

func (e *llvmEmitter) cast(f *fb, v *ast.Cast) lval {
	src := e.p.Erased(v.X.GetType())
	dst := e.p.Erased(v.Type.Resolved)
	if dst == nil {
		return e.expr(f, v.X)
	}
	_, sp := src.(*ast.PrimType)
	_, dp := dst.(*ast.PrimType)
	if sp || dp {
		return e.coerce(f, e.expr(f, v.X), dst)
	}
	target := ""
	if ct, ok := dst.(*ast.ClassType); ok {
		e.needClass(ct.Class)
		target = "@cls_" + util.Mangle(ct.Class.Full)
	} else if _, ok := dst.(*ast.ArrayType); ok {
		cl := e.useArray()
		e.needClass(cl)
		target = "@cls_" + util.Mangle(cl.Full)
	} else {
		return e.coerce(f, e.expr(f, v.X), dst)
	}
	e.markExn("TY_CCE", e.p.Builtins.CCE)
	x := e.expr(f, v.X)
	return value(e.rtCall(f, "ty_checkcast", dst, []lval{
		value(x.v, e.refType()), value(target, e.refType()),
	}).v, dst)
}

func (e *llvmEmitter) instanceOf(f *fb, v *ast.InstanceOf) lval {
	if v.Binding != nil {
		e.refuse(v.GetPos(), "an instanceof pattern that binds a variable (%s): the llvm back end does not lower patterns", v.Binding.Name)
	}
	if _, ok := e.p.Erased(v.Type.Resolved).(*ast.PrimType); ok {
		e.refuse(v.GetPos(), "a primitive type pattern: the llvm back end does not lower patterns")
	}
	x := e.expr(f, v.X)
	target := ""
	if ct, ok := e.p.Erased(v.Type.Resolved).(*ast.ClassType); ok {
		e.needClass(ct.Class)
		target = "@cls_" + util.Mangle(ct.Class.Full)
	} else {
		cl := e.useArray()
		e.needClass(cl)
		target = "@cls_" + util.Mangle(cl.Full)
	}
	return e.rtCall(f, "ty_instanceof", ast.TBoolean, []lval{
		value(x.v, e.refType()), value(target, e.refType())})
}

// ---------------------------------------------------------------- assignment

// addr answers with the address of an assignable expression and its type, or
// refuses when the expression is not one this back end can store through.
func (e *llvmEmitter) addr(f *fb, x ast.Expr) (string, ast.Type) {
	switch v := x.(type) {
	case *ast.Ident:
		switch r := v.Ref.(type) {
		case *ast.Field:
			if r == nil {
				break
			}
			r = e.canonicalField(r)
			if r.Mods.Has(ast.ModStatic) {
				e.needClass(r.Owner)
				e.clinitIfNeeded(f, r.Owner)
				return e.staticGlobal(r.Owner, r), v.GetType()
			}
			e.bareFieldCheck(f, r)
			return e.fieldAddr(f, r, e.selfOperand(f), false), v.GetType()
		case *ast.Var:
			if r == nil {
				break
			}
			if r.Field != nil {
				return e.propertyStorageAddr(f, r.Field), v.GetType()
			}
			return f.localSlot(r), r.Type
		}
	case *ast.Select:
		if r, ok := v.Ref.(*ast.Field); ok && r != nil {
			r = e.canonicalField(r)
			if r.Mods.Has(ast.ModStatic) {
				e.needClass(r.Owner)
				e.clinitIfNeeded(f, r.Owner)
				return e.staticGlobal(r.Owner, r), v.GetType()
			}
			recv := e.expr(f, v.X)
			return e.fieldAddr(f, r, recv.v, true), v.GetType()
		}
	case *ast.Index:
		return e.elemAddr(f, v), v.GetType()
	}
	e.refuse(x.GetPos(), "the assignment target: the llvm back end cannot store through this expression")
	return "", nil
}

// elemAddr is the address of an array element, with Java's two tests in Java's
// order: the null test on the array, then the bounds test on the index.
func (e *llvmEmitter) elemAddr(f *fb, ix *ast.Index) string {
	arr := e.expr(f, ix.X)
	idx := e.coerce(f, e.expr(f, ix.Index), ast.TInt)
	return e.elemAddrOfIndex(f, e.p.Erased(ix.GetType()), arr.v, idx.v)
}

// elemAddrOfIndex is elemAddr with the array reference and the index already
// lowered, for a caller that has to evaluate something between the index and
// these tests -- an assignment's value, which Java evaluates before them
// (JLS 15.26.1).
func (e *llvmEmitter) elemAddrOfIndex(f *fb, elem ast.Type, arr, idx string) string {
	e.nullCheck(f, arr)
	n := f.reg()
	f.ins(fmt.Sprintf("%s = sext i32 %s to i64", n, idx))
	length := e.loadRaw(f, "i64", e.gepBytes(f, arr, 8), 8)
	e.boundsCheck(f, n, length)
	return e.elemAddrOfDyn(f, arr, n, util.SizeOf(elem))
}

// boundsCheck throws when the index is outside the array, the way Java does.
func (e *llvmEmitter) boundsCheck(f *fb, idx, length string) {
	e.markExn("TY_AIOOBE", e.p.Builtins.AIOOBE)
	e.decl("ty_aioobe", "ptr", []string{"i64", "i64"}, "")
	ok := f.nextLabel("inb_ok")
	bad := f.nextLabel("inb_bad")
	lo := f.reg()
	f.ins(fmt.Sprintf("%s = icmp slt i64 %s, 0", lo, idx))
	hi := f.reg()
	f.ins(fmt.Sprintf("%s = icmp sge i64 %s, %s", hi, idx, length))
	both := f.reg()
	f.ins(fmt.Sprintf("%s = or i1 %s, %s", both, lo, hi))
	f.cbr(both, bad, ok)
	f.label(bad)
	f.ins(fmt.Sprintf("call ptr @ty_aioobe(i64 %s, i64 %s)", idx, length))
	f.br(ok)
	f.label(ok)
}

func (e *llvmEmitter) indexExpr(f *fb, ix *ast.Index) lval {
	elem := e.p.Erased(ix.GetType())
	p := e.elemAddr(f, ix)
	if e.isRef(elem) {
		return value(e.loadRaw(f, "ptr", p, 8), ix.GetType())
	}
	return value(e.loadRaw(f, e.llvmType(elem), p, util.SizeOf(elem)), ix.GetType())
}

func (e *llvmEmitter) assign(f *fb, a *ast.Assign) lval {
	if a.Op == "=" {
		if ix, ok := a.X.(*ast.Index); ok {
			if selem := e.elemClassForStore(ix.GetType()); selem != "" {
				return e.storeRefElem(f, ix, selem, a.Y)
			}
			// Java's order for an array assignment: the array reference, then
			// the index, then the value, and only then the null and bounds
			// tests (JLS 15.26.1). elemAddr's tests have to wait for the value,
			// or `a[9] = f()` throws without calling f() where the JDK calls
			// it and throws afterwards.
			arr := e.expr(f, ix.X)
			idx := e.coerce(f, e.expr(f, ix.Index), ast.TInt)
			v := e.coerce(f, e.expr(f, a.Y), a.X.GetType())
			ptr := e.elemAddrOfIndex(f, e.p.Erased(ix.GetType()), arr.v, idx.v)
			e.store(f, ptr, a.X.GetType(), v)
			return v
		}
		ptr, t := e.addr(f, a.X)
		v := e.coerce(f, e.expr(f, a.Y), t)
		e.store(f, ptr, t, v)
		return v
	}
	ptr, t := e.addr(f, a.X)
	cur := e.load(f, ptr, t)
	var out lval
	switch {
	case a.Op == "+=" && e.isStringType(t):
		parts := []string{cur.v}
		collect := func(y ast.Expr) {}
		collect = func(y ast.Expr) {
			if nb, ok := y.(*ast.Binary); ok && nb.Op == "+" && e.isStringType(nb.GetType()) {
				collect(nb.X)
				collect(nb.Y)
				return
			}
			parts = append(parts, e.stringOperand(f, y))
		}
		collect(a.Y)
		out = value(e.concatParts(f, parts), t)
	case a.Op == "/=" || a.Op == "%=":
		out = e.compoundDiv(f, a, strings.TrimSuffix(a.Op, "="), cur, t)
	case a.Op == "<<=" || a.Op == ">>=" || a.Op == ">>>=":
		op := strings.TrimSuffix(a.Op, "=")
		// The width and the mask are the operation's, which the checker
		// records: for a boxed target that is the unboxed target's own
		// promoted type, so `Long l; l <<= 33` shifts a long by 33 and not an
		// int by 33 & 31. coerce then unboxes the target and boxes the result.
		ot := opTypeOf(a, t)
		x := e.coerce(f, cur, ot)
		y := e.coerce(f, e.expr(f, a.Y), ot)
		width := llvmWidth(e.llvmType(ot))
		mask := f.reg()
		f.ins(fmt.Sprintf("%s = and %s %s, %d", mask, e.llvmType(ot), y.v, width-1))
		r := f.reg()
		f.ins(fmt.Sprintf("%s = %s %s %s, %s", r,
			map[string]string{"<<": "shl", ">>": "ashr", ">>>": "lshr"}[op],
			e.llvmType(ot), x.v, mask))
		out = e.coerce(f, value(r, ot), t)
	default:
		op := strings.TrimSuffix(a.Op, "=")
		ot := opTypeOf(a, t)
		x := e.coerce(f, cur, ot)
		y := e.operand(f, a.Y, ot)
		opc := map[string]string{"+": "add", "-": "sub", "*": "mul", "&": "and", "|": "or", "^": "xor"}[op]
		if opc == "" {
			e.refuse(a.GetPos(), "the operator %s: the llvm back end does not lower it", a.Op)
		}
		r := f.reg()
		f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, arithOp(opc, e.llvmType(ot)), e.llvmType(ot), x.v, y.v))
		out = e.coerce(f, value(r, ot), t)
	}
	e.store(f, ptr, t, out)
	return out
}

// opTypeOf is the type a compound assignment's operation happens in, which the
// checker records for every compound form: the promoted type of the unboxed
// operands (JLS 15.26.2). The target's own type is the fallback for an
// assignment the checker reported an error for, which does not reach the back
// end; it is what C's `x +=` means.
func opTypeOf(a *ast.Assign, t ast.Type) ast.Type {
	if a.OpType != nil {
		return a.OpType
	}
	return t
}

// compoundDiv lowers `x /= y` and `x %= y`.
//
// The operation happens in the type binary numeric promotion gives the two
// operands, and only its result is converted back to the target (JLS 15.26.2).
// Dividing in the target's type instead narrows the right operand first: for
// `int a = 7; a /= 2.5` that is 7/2, which is 3, where Java's promotion makes it
// 7.0/2.5 = 2.8 and the conversion back gives 2. The same mistake turns
// `a %= 2.5` into 1 instead of 2, and makes `int m = 2147483647; m /= 0.5`
// divide by zero where Java divides in double and saturates on the way back.
//
// OpType is the checker's answer to that promotion, the same one the other
// compound operators use.
func (e *llvmEmitter) compoundDiv(f *fb, a *ast.Assign, op string, cur lval, t ast.Type) lval {
	ot := opTypeOf(a, t)
	if isFloating(ot) {
		x := e.coerce(f, cur, ot)
		y := e.coerce(f, e.expr(f, a.Y), ot)
		r := f.reg()
		ins := "fdiv"
		if op == "%" {
			ins = "frem"
		}
		f.ins(fmt.Sprintf("%s = %s %s %s, %s", r, ins, e.llvmType(ot), x.v, y.v))
		return e.coerce(f, value(r, ot), t)
	}
	e.markExn("TY_ARITH", e.p.Builtins.Arith)
	fn := "ty_div_int"
	if ast.IsPrim(ot, ast.Long) {
		fn = "ty_div_long"
		if op == "%" {
			fn = "ty_rem_long"
		}
	} else if op == "%" {
		fn = "ty_rem_int"
	}
	x := e.coerce(f, cur, ot)
	y := e.coerce(f, e.expr(f, a.Y), ot)
	return e.coerce(f, e.rtCall(f, fn, ot, []lval{x, y}), t)
}

// elemClassForStore is the class an array store at this site promises, or ""
// when the element type names no single class: a primitive array holds no
// references, and an array of arrays has one class for every array rather than a
// class per element type.
func (e *llvmEmitter) elemClassForStore(t ast.Type) string {
	if ct, ok := e.p.Erased(t).(*ast.ClassType); ok {
		e.needClass(ct.Class)
		return "@cls_" + util.Mangle(ct.Class.Full)
	}
	return ""
}

// storeRefElem stores a reference into an array element through the same check
// the runtime's ty_array_store_ref inlines: a value the array's own element
// class never promised is an ArrayStoreException, not a silent write.
func (e *llvmEmitter) storeRefElem(f *fb, ix *ast.Index, selem string, val ast.Expr) lval {
	arr := e.expr(f, ix.X)
	idx := e.coerce(f, e.expr(f, ix.Index), ast.TInt)
	v := e.coerce(f, e.expr(f, val), ix.GetType())
	e.nullCheck(f, arr.v)
	n := f.reg()
	f.ins(fmt.Sprintf("%s = sext i32 %s to i64", n, idx.v))
	length := e.loadRaw(f, "i64", e.gepBytes(f, arr.v, 8), 8)
	e.boundsCheck(f, n, length)
	elemcls := e.loadRaw(f, "ptr", e.gepBytes(f, arr.v, 32), 8)
	slot := e.elemAddrOfDyn(f, arr.v, n, util.SizeRef)
	store := f.nextLabel("as_store")
	chk1 := f.nextLabel("as_chk1")
	chk2 := f.nextLabel("as_chk2")
	chk3 := f.nextLabel("as_chk3")
	chk4 := f.nextLabel("as_chk4")
	fail := f.nextLabel("as_fail")
	same := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, %s", same, elemcls, selem))
	f.cbr(same, store, chk1)
	f.label(chk1)
	noPromise := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, null", noPromise, elemcls))
	f.cbr(noPromise, store, chk2)
	f.label(chk2)
	isNull := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, null", isNull, v.v))
	f.cbr(isNull, store, chk3)
	f.label(chk3)
	vc := e.loadRaw(f, "ptr", v.v, 8)
	sameCls := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, %s", sameCls, vc, elemcls))
	f.cbr(sameCls, store, chk4)
	f.label(chk4)
	e.markExn("TY_ARRAYSTORE", e.p.Builtins.ArrayStore)
	e.decl("ty_arraystore", "ptr", nil, "")
	inst := e.rtCall(f, "ty_instanceof", ast.TBoolean, []lval{
		value(v.v, e.refType()), value(elemcls, e.refType())})
	ok := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", ok, inst.v))
	f.cbr(ok, store, fail)
	f.label(fail)
	f.ins("call ptr @ty_arraystore()")
	f.br(store)
	f.label(store)
	e.store(f, slot, ix.GetType(), v)
	return v
}
