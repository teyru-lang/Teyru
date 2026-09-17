package codegen

// Calls, allocations and arrays for the LLVM back end.
//
// A call site makes the same choice the C back end's does -- a binding from
// nativeTable, a method this module defines, a slot of the receiver's vtable,
// or the symbol a program's own native method links against -- and the
// arguments are converted the way C would convert them, which is the whole
// reason the runtime's prototypes are read from its header.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

// argOperand renders a call argument. A call is the one place LLVM wants every
// operand's type written out -- a constant and a register alike -- because an
// argument has no surrounding instruction to give it one.
func argOperand(ty, v string) string { return ty + " " + v }

// refType is a reference type to lower a value of: every reference is a pointer,
// so which class it names only matters where the program's own types do.
func (e *llvmEmitter) refType() ast.Type {
	return &ast.ClassType{Class: e.p.Builtins.Object}
}

// rtCall calls a runtime helper, converting each argument to the type the
// helper's own prototype declares and the result back to the expression's type.
// The prototype comes from tyrt.h: a call the runtime would not agree with is
// not something to guess at, so a helper the header does not declare is refused.
func (e *llvmEmitter) rtCall(f *fb, name string, result ast.Type, args []lval) lval {
	proto, ok := rtProtoOf(name)
	if !ok {
		e.refuse(noPos, "the runtime helper %s: internal/runtime/src/tyrt.h does not declare it, so the module cannot call it with the right types", name)
	}
	if len(proto.params) != len(args) {
		e.refuse(noPos, "the runtime helper %s: the module would pass %d arguments to a helper that takes %d", name, len(proto.params), len(args))
	}
	vals := make([]string, len(args))
	saved := e.rtCtx
	e.rtCtx = name
	for i, a := range args {
		vals[i] = argOperand(proto.params[i], e.convertTo(f, a, proto.params[i]))
	}
	e.rtCtx = saved
	e.decl(name, proto.ret, proto.params, "")
	if proto.ret == "void" {
		e.emitCall(f, name, "void", vals)
		return value("0", result)
	}
	reg := e.emitCall(f, name, proto.ret, vals)
	return e.convertFrom(f, reg, proto.ret, result)
}

// rtVoid calls a helper whose result the expression does not want.
func (e *llvmEmitter) rtVoid(f *fb, name string, args []lval) {
	e.rtCall(f, name, nil, args)
}

// callExpr lowers a method call.
func (e *llvmEmitter) callExpr(f *fb, c *ast.Call) lval {
	m := c.Method
	if m == nil {
		return e.arrayMethod(f, c)
	}
	if e.needsReflection(f, m) {
		// The C back end writes the member tables when a call site like this
		// exists; this back end does not write them at all, so the program is
		// refused here rather than built into one whose reflection answers
		// NoSuchMethodException at run time -- which is a wrong answer, not a
		// missing feature.
		e.refuse(c.GetPos(), "a reflective call (%s.%s): the llvm back end does not write the member tables java.lang.reflect reads", m.Owner.Full, m.Name)
	}
	recv := ""
	if !m.IsStatic() {
		switch {
		case c.Recv == nil:
			// An unqualified call is a call on the instance the code runs on --
			// which inside a lambda body is the instance the lambda was created
			// in, not the closure. Dispatching on the closure would call the
			// lambda's own method and recurse.
			recv = e.selfOperand(f)
		case c.Super:
			recv = "%this"
		default:
			recv = e.expr(f, c.Recv).v
		}
	}
	if m.Selector >= 0 && !m.IsStatic() {
		// An interface call dispatches through the receiver's interface map.
		//
		// An unqualified one is a call on `this` -- the body of a default
		// method calling another method of its interface, or a class method
		// calling a default it inherits. `this` is the instance either way and
		// the interface map of that instance holds the implementation, so the
		// dispatch is the same one an explicit receiver makes. `super.m()` is
		// the one that is not: it has to reach that interface's own default and
		// skip the override the instance would otherwise answer with, which is
		// a direct call to a body this back end has not verified.
		if c.Super {
			e.refuse(c.GetPos(), "Iface.super.%s(): the llvm back end does not lower a call to a specific interface's default (it dispatches through the receiver, which would reach an override)", m.Name)
		}
		if c.Recv == nil {
			return e.ifaceDispatch(f, m, value(e.selfOperand(f), e.refType()), e.callArgs(f, m, c, c.Args))
		}
		// The receiver was evaluated above, before the arguments, which is the
		// order Java evaluates them in. Evaluating c.Recv again here would run
		// it a second time: for a receiver that is itself a call -- `it.next()`
		// as the receiver of `getKey()`, which is how the standard library's
		// key iterator is written -- that call would happen twice and the first
		// result be thrown away, so a walk over a collection would see every
		// other element.
		return e.ifaceDispatch(f, m, value(recv, c.Recv.GetType()), e.callArgs(f, m, c, c.Args))
	}
	// A native method that is not overridable is the runtime helper itself; one
	// that is goes through the receiver's vtable, because the override is what
	// has to run (nativeIsFinal is the C back end's rule, kept in one place).
	if nf, ok := nativeTable[nativeKey(m)]; ok && e.cEmitter().nativeIsFinal(m) {
		return e.nativeCall(f, m, nf, recv, c)
	}
	args := e.callArgs(f, m, c, c.Args)
	if m.External {
		return e.externalCall(f, m, recv, args)
	}
	if m.IsStatic() && !c.Super && c.Recv != nil {
		// naming a static member initializes its class first
		e.clinitIfNeeded(f, m.Owner)
	}
	switch {
	case c.Static || m.IsStatic() || c.Super:
		return e.directCall(f, m, recv, args)
	case c.Recv == nil && m.VIndex >= 0:
		if directBind(m) {
			// A class the program never extends has one implementation per
			// slot, so the slot can be skipped -- the C back end's own rule,
			// applied at this back end's call sites.
			return e.directCall(f, m, recv, args)
		}
		return e.virtualCall(f, m, value(recv, e.refType()), args)
	case c.Recv != nil && m.VIndex >= 0 && !m.Mods.Has(ast.ModPrivate):
		if directBind(m) {
			// The test the vtable lookup would have made: the callee reads its
			// own fields, so a null receiver has to raise NullPointerException
			// here rather than dereference it.
			e.nullCheck(f, recv)
			return e.directCall(f, m, recv, args)
		}
		return e.virtualCall(f, m, value(recv, c.Recv.GetType()), args)
	}
	return e.directCall(f, m, recv, args)
}

// needsReflection reports whether a call site can reach a member table. It is
// the C back end's own predicate -- the one that decides whether the tables
// ship -- asked at this back end's call sites, so that the two agree on which
// programs are reflection users. The emitter state it needs (the class the call
// is written in) is the one the C back end has at the same point.
func (e *llvmEmitter) needsReflection(f *fb, m *ast.Method) bool {
	ce := &Emitter{prog: e.p}
	if f != nil && f.fn != nil {
		ce.curClass = f.fn.Owner
	}
	return ce.reflectionCall(m)
}

// ifaceMethod is the interface method of a given name and arity, or nil when the
// program has no such interface method. The arity matters: an interface may
// declare two methods of one name with different parameters, and picking the
// wrong one dispatches a call to a method whose signature the caller does not
// have.
func (e *llvmEmitter) ifaceMethod(cl *ast.Class, name string, arity int) *ast.Method {
	if cl == nil {
		return nil
	}
	for _, m := range cl.Methods[name] {
		if m.Selector >= 0 && len(m.Params) == arity {
			return m
		}
	}
	return nil
}

// ifaceMethodFor is ifaceMethod for a method object rather than a name.
func (e *llvmEmitter) ifaceMethodFor(cl *ast.Class, name string, arity int) *ast.Method {
	return e.ifaceMethod(cl, name, arity)
}

// ifaceDispatch calls an interface method: the receiver's dynamic class decides
// which implementation runs, and that class's interface map is what says which.
func (e *llvmEmitter) ifaceDispatch(f *fb, im *ast.Method, recv lval, args []lval) lval {
	// The checker's selector is the one to use: it is what the interface map of
	// every class that answers the interface was built from. The lookup by name
	// is only a fallback for a method object that arrived without one, and it
	// matches on the name AND the parameter count -- matching on the name alone
	// picks the first overload of that name, which for `Writer.write` is
	// `write(int)` where the call site meant `write(String)`, and a String
	// receiver then arrives in an int parameter.
	sel := im.Selector
	if sel < 0 {
		if canon := e.ifaceMethodFor(im.Owner, im.Name, len(im.Params)); canon != nil {
			sel = canon.Selector
		}
	}
	e.ifaceSlot(im, sel)
	ret, _ := e.fnSig(im)
	vals := []string{argOperand("ptr", recv.v)}
	for i, a := range args {
		want := e.llvmType(a.t)
		if i < len(im.Params) {
			want = e.llvmType(im.Params[i])
		}
		vals = append(vals, argOperand(want, e.convertTo(f, a, want)))
	}
	e.nullCheck(f, recv.v)
	fn := e.itabLookup(f, recv.v, sel)
	if ret == "void" {
		f.ins(fmt.Sprintf("call void %s(%s)", fn, strings.Join(vals, ", ")))
		return value("0", im.Result)
	}
	r := f.reg()
	f.ins(fmt.Sprintf("%s = call %s %s(%s)", r, ret, fn, strings.Join(vals, ", ")))
	return e.convertFrom(f, r, ret, im.Result)
}

// itabLookup inlines the runtime's interface lookup: walk the receiver's class
// chain, and each class's interface map, for the selector. It is the walk
// ty_itab makes in C -- the header defines that one inline for the C back end's
// call sites, and a module cannot call a function defined in a header, so the
// walk is written out here.
//
// A selector no class answers is what ty_itab_slow reports: it throws a catchable
// error rather than jumping through nothing.
func (e *llvmEmitter) itabLookup(f *fb, recv string, sel int) string {
	e.decl("ty_itab_slow", "void", nil, " noreturn")
	cls := e.loadRaw(f, "ptr", recv, 8)
	kInit := f.nextLabel("itab_k")
	kBody := f.nextLabel("itab_kb")
	iInit := f.nextLabel("itab_i")
	iBody := f.nextLabel("itab_ib")
	iNext := f.nextLabel("itab_in")
	found := f.nextLabel("itab_hit")
	kNext := f.nextLabel("itab_kn")
	slow := f.nextLabel("itab_slow")
	done := f.nextLabel("itab_done")
	// The two loop-carried values are named before the phis that carry them: a
	// phi names a value defined in the block that branches back, which is
	// emitted further down.
	ksupReg := f.reg()
	inextReg := f.reg()
	from := f.cur
	f.br(kInit)
	// The class walk, then the map of each class: both need a phi, because the
	// back edge of one is the body of the other.
	f.label(kInit)
	k := f.reg()
	f.ins(fmt.Sprintf("%s = phi ptr [ %s, %%%s ], [ %s, %%%s ]", k, cls, from, ksupReg, kNext))
	kn := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq ptr %s, null", kn, k))
	f.cbr(kn, slow, kBody)
	f.label(kBody)
	imap := e.loadRaw(f, "ptr", e.gepBytes(f, k, 72), 8)
	isel := e.loadRaw(f, "i32", e.gepBytes(f, k, 68), 4)
	n := f.reg()
	f.ins(fmt.Sprintf("%s = sext i32 %s to i64", n, isel))
	f.br(iInit)
	f.label(iInit)
	i := f.reg()
	f.ins(fmt.Sprintf("%s = phi i64 [ 0, %%%s ], [ %s, %%%s ]", i, kBody, inextReg, iNext))
	over := f.reg()
	f.ins(fmt.Sprintf("%s = icmp sge i64 %s, %s", over, i, n))
	f.cbr(over, kNext, iBody)
	f.label(iBody)
	selp := e.gepAt(f, imap, e.mulAdd(f, i, 16, 0))
	got := e.loadRaw(f, "i32", selp, 4)
	hit := f.reg()
	f.ins(fmt.Sprintf("%s = icmp eq i32 %s, %d", hit, got, sel))
	f.cbr(hit, found, iNext)
	f.label(iNext)
	f.ins(fmt.Sprintf("%s = add i64 %s, 1", inextReg, i))
	f.br(iInit)
	f.label(found)
	fnp := e.gepAt(f, imap, e.mulAdd(f, i, 16, 8))
	fn := e.loadRaw(f, "ptr", fnp, 8)
	f.br(done)
	f.label(kNext)
	f.ins(fmt.Sprintf("%s = load ptr, ptr %s, align 8", ksupReg, e.gepBytes(f, k, 16)))
	f.br(kInit)
	f.label(slow)
	f.ins("call void @ty_itab_slow()")
	f.unreachable()
	f.label(done)
	out := f.reg()
	f.ins(fmt.Sprintf("%s = phi ptr [ %s, %%%s ]", out, fn, found))
	return out
}

// mulAdd is `i * scale + base`, for walking a runtime table by byte offset.
func (e *llvmEmitter) mulAdd(f *fb, i string, scale, base int64) string {
	r := f.reg()
	f.ins(fmt.Sprintf("%s = mul i64 %s, %d", r, i, scale))
	if base == 0 {
		return r
	}
	r2 := f.reg()
	f.ins(fmt.Sprintf("%s = add i64 %s, %d", r2, r, base))
	return r2
}

// arrayMethod lowers the Object-like methods an array has without a method
// symbol of its own.
func (e *llvmEmitter) arrayMethod(f *fb, c *ast.Call) lval {
	at, ok := e.p.Erased(c.Recv.GetType()).(*ast.ArrayType)
	if !ok {
		e.refuse(c.GetPos(), "the call %s: the llvm back end does not resolve it", c.Name)
	}
	recv := e.expr(f, c.Recv)
	e.nullCheck(f, recv.v)
	switch c.Name {
	case "clone":
		cl := e.useArray()
		e.needClass(cl)
		sz := strconv.FormatInt(util.SizeOf(at.Elem), 10)
		return value(e.rtCall(f, "ty_array_clone", c.GetType(), []lval{recv, value(sz, ast.TLong)}).v, c.GetType())
	case "toString":
		return value(e.rtCall(f, "ty_str_intern", e.stringType(), []lval{value(e.cstring("[array]"), e.refType())}).v, c.GetType())
	case "hashCode":
		return value(e.rtCall(f, "ty_object_hash", ast.TInt, []lval{recv}).v, c.GetType())
	case "equals":
		var other = value("null", e.refType())
		if len(c.Args) > 0 {
			other = e.expr(f, c.Args[0])
		}
		r := f.reg()
		f.ins(fmt.Sprintf("%s = icmp eq ptr %s, %s", r, recv.v, other.v))
		return e.boolVal(f, r)
	}
	e.refuse(c.GetPos(), "the array method %s: the llvm back end does not lower it", c.Name)
	return value("0", nil)
}

// directCall calls a method this module defines, with the C back end's
// signature: the receiver first for an instance method, then the parameters.
func (e *llvmEmitter) directCall(f *fb, m *ast.Method, recv string, args []lval) lval {
	e.needMethod(m)
	ret, params := e.fnSig(m)
	vals := make([]string, 0, len(args)+1)
	if !m.IsStatic() {
		if recv == "" {
			recv = "%this"
		}
		vals = append(vals, argOperand("ptr", recv))
	}
	saved := e.rtCtx
	e.rtCtx = e.methodSymbol(m)
	for i, a := range args {
		want := e.llvmType(a.t)
		if i < len(m.Params) {
			want = e.llvmType(m.Params[i])
		}
		vals = append(vals, argOperand(want, e.convertTo(f, a, want)))
	}
	e.rtCtx = saved
	// The method is defined by this module -- needMethod queued it, and a call
	// site is always inside a function that itself came from the queue, so the
	// definition is on its way. Declaring it here as well would be a declare and
	// a define of one internal function, which is a redefinition.
	_ = params
	r := e.emitCall(f, e.methodSymbol(m), ret, vals)
	return e.convertFrom(f, r, ret, m.Result)
}

// externalCall calls a native method the program itself implements, whose
// symbol and signature sema fixed (docs/native.md is the contract).
func (e *llvmEmitter) externalCall(f *fb, m *ast.Method, recv string, args []lval) lval {
	ret, params := e.fnSig(m)
	vals := make([]string, 0, len(args)+1)
	if !m.IsStatic() {
		if recv == "" {
			recv = "%this"
		}
		vals = append(vals, argOperand("ptr", recv))
	}
	saved := e.rtCtx
	e.rtCtx = e.methodSymbol(m)
	for i, a := range args {
		want := e.llvmType(a.t)
		if i < len(m.Params) {
			want = e.llvmType(m.Params[i])
		}
		vals = append(vals, argOperand(want, e.convertTo(f, a, want)))
	}
	e.rtCtx = saved
	e.decl(m.Native, ret, params, "")
	r := e.emitCall(f, m.Native, ret, vals)
	return e.convertFrom(f, r, ret, m.Result)
}

// virtualCall dispatches through the receiver's vtable. The slot index is the
// one the C back end uses, and the slot is recorded so that every class that
// can be the receiver answers it.
func (e *llvmEmitter) virtualCall(f *fb, m *ast.Method, recv lval, args []lval) lval {
	if m.Owner == nil || m.Owner.IsInterface() {
		e.refuse(noPos, "the interface method %s: the llvm back end does not lower interface dispatch", m.Name)
	}
	e.slot(m.Owner, m.VIndex)
	_, params := e.fnSig(m)
	_ = params
	ret := e.llvmType(m.Result)
	vals := make([]string, 0, len(args)+1)
	vals = append(vals, argOperand("ptr", recv.v))
	saved := e.rtCtx
	e.rtCtx = e.methodSymbol(m)
	for i, a := range args {
		want := e.llvmType(a.t)
		if i < len(m.Params) {
			want = e.llvmType(m.Params[i])
		}
		vals = append(vals, argOperand(want, e.convertTo(f, a, want)))
	}
	e.rtCtx = saved
	e.nullCheck(f, recv.v)
	cls := e.loadRaw(f, "ptr", recv.v, 8)
	vtp := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds %%tyclass, ptr %s, i32 0, i32 %d", vtp, cls, e.tyclassIdx["vtable"]))
	vt := e.loadRaw(f, "ptr", vtp, 8)
	slotp := e.gepIndex(f, "ptr", vt, strconv.Itoa(m.VIndex))
	fn := e.loadRaw(f, "ptr", slotp, 8)
	call := f.reg()
	callArgs := strings.Join(vals, ", ")
	if ret == "void" {
		f.ins(fmt.Sprintf("call void %s(%s)", fn, callArgs))
		return value("0", m.Result)
	}
	f.ins(fmt.Sprintf("%s = call %s %s(%s)", call, ret, fn, callArgs))
	return e.convertFrom(f, call, ret, m.Result)
}

// callArgs lowers a call's arguments, converting each to the parameter it goes
// to and packing a variable-arity tail into the array the callee takes.
func (e *llvmEmitter) callArgs(f *fb, m *ast.Method, call *ast.Call, list []ast.Expr) []lval {
	out := make([]lval, 0, len(list))
	n := len(m.Params)
	for i, a := range list {
		var want ast.Type
		switch {
		case m.Varargs && n > 0 && i >= n-1:
			last := m.Params[n-1]
			if at, ok := e.p.Erased(last).(*ast.ArrayType); ok {
				want = at.Elem
				// an argument that is already the array is passed as one
				if _, isArr := e.p.Erased(a.GetType()).(*ast.ArrayType); isArr {
					want = last
				}
			}
		case i < n:
			want = m.Params[i]
		}
		out = append(out, e.coerce(f, e.expr(f, a), want))
	}
	if !m.Varargs {
		return out
	}
	direct := call != nil && e.p.VarargsDirect(call)
	if call == nil && len(list) == len(m.Params) {
		return out
	}
	if direct {
		return out
	}
	return e.packVarargs(f, out, m)
}

// packVarargs gathers a variable-arity call's tail into the array the callee's
// last parameter is.
func (e *llvmEmitter) packVarargs(f *fb, parts []lval, m *ast.Method) []lval {
	last, ok := e.p.Erased(m.Params[len(m.Params)-1]).(*ast.ArrayType)
	if !ok {
		e.refuse(noPos, "the variable-arity method %s.%s: its last parameter is not an array", m.Owner.Full, m.Name)
	}
	n := len(m.Params) - 1
	// The receiver is not in `parts`: callArgs lowers the declared parameters
	// only, and directCall puts the receiver in front of them afterwards. The C
	// back end's packVarargs walks a list that already has the receiver in it,
	// and reserving a slot for one here dropped the last argument of every
	// variable-arity method call -- `printf("%s%n", "x")` packed an empty array.
	head := []lval{}
	tail := parts
	if len(parts) >= n {
		head = parts[:n]
		tail = parts[n:]
	}
	arr := e.allocArray(f, last.Elem, int64(len(tail)))
	for i, t := range tail {
		slot := e.elemAddrOf(f, arr, int64(i), last.Elem)
		e.store(f, slot, last.Elem, e.coerce(f, t, last.Elem))
	}
	return append(head, value(arr, last))
}

// ---------------------------------------------------------------- native calls

// nativeCall lowers a call to one of the runtime's bound helpers. The bindings
// and the stream-aware twins are the C back end's tables; what is done here is
// the same call with the types the helper's own prototype declares.
func (e *llvmEmitter) nativeCall(f *fb, m *ast.Method, nf nativeFn, recv string, c *ast.Call) lval {
	return e.nativeInvoke(f, m, nf, recv, e.callArgs(f, m, c, c.Args), c.GetPos())
}

// nativeInvoke renders a call to a bound helper. The bindings and the
// stream-aware twins are the C back end's tables; what is done here is the same
// call with the types the helper's own prototype declares. A call site and a
// synthesized wrapper for a prelude native both come through here, so neither
// can honour clsArg, classHint or the twin and the other forget to.
func (e *llvmEmitter) nativeInvoke(f *fb, m *ast.Method, nf nativeFn, recv string, vals []lval, pos source.Pos) lval {
	if twin := e.cEmitter().psTwin(m, nf); twin != nil {
		args := append([]lval{value(recv, e.refType())}, vals...)
		return e.rtNamed(f, twin.name, m, args)
	}
	switch {
	case nf.clsArg != "":
		cl := e.p.LookupClass(nf.clsArg)
		if cl == nil {
			e.refuse(pos, "the native binding of %s.%s names the class %s, which this program does not have", m.Owner.Full, m.Name, nf.clsArg)
		}
		// The runtime hands back an instance of this class (ty_class_make builds
		// the Class object the literal names), so it is a class that can have
		// instances: its vtable has to be real, because the object's own
		// toString/hashCode/equals are reached through it.
		e.instantiate(cl)
		return e.rtNamed(f, nf.fn, m, append(vals, value("@cls_"+util.Mangle(cl.Full), nil)))
	case nf.selClass != "":
		cl := e.p.LookupClass(nf.selClass)
		sel := -1
		if cl != nil {
			for _, cand := range cl.Methods[nf.selMethod] {
				if cand.Selector >= 0 {
					sel = cand.Selector
				}
			}
		}
		if cl == nil || sel < 0 {
			e.refuse(pos, "the native binding of %s.%s needs the interface selector of %s.%s, which this program does not have",
				m.Owner.Full, m.Name, nf.selClass, nf.selMethod)
		}
		return e.rtNamed(f, nf.fn, m, append(vals, value(strconv.Itoa(sel), ast.TInt)))
	case nf.classHint != "":
		cl := e.p.LookupClass(nf.classHint)
		if cl == nil {
			e.refuse(pos, "the native binding of %s.%s names the class %s, which this program does not have", m.Owner.Full, m.Name, nf.classHint)
		}
		e.instantiate(cl)
		args := []lval{value(recv, e.refType())}
		args = append(args, vals...)
		args = append(args, value("@cls_"+util.Mangle(cl.Full), nil))
		return e.rtNamed(f, "ty_class_of_cls", m, args)
	}
	args := vals
	if !m.IsStatic() {
		args = append([]lval{value(recv, e.refType())}, vals...)
	}
	return e.rtNamed(f, nf.fn, m, args)
}

// rtNamed calls a helper by name, taking the prototype from the header. A
// helper with a prototype the C back end repeats at the call site (the socket
// and file primitives) is not in this subset: their bindings are reached
// through classes this back end refuses, and a call made with guessed types
// would be wrong output rather than a failed build.
func (e *llvmEmitter) rtNamed(f *fb, name string, m *ast.Method, args []lval) lval {
	if _, ok := rtProtoOf(name); !ok {
		e.refuse(noPos, "the native helper %s (for %s.%s): internal/runtime/src/tyrt.h does not declare it", name, m.Owner.Full, m.Name)
	}
	return e.rtCall(f, name, m.Result, args)
}

// ---------------------------------------------------------------- allocation

// allocInstance allocates an instance of a class with the runtime's own fast
// path: a bump of the thread's slab, with ty_alloc_slow when the slab is full or
// the collection budget is spent. It is the same two-branch shape the C back end
// gets from the header's inlined ty_alloc, so the allocation cost is the same.
func (e *llvmEmitter) allocInstance(f *fb, cl *ast.Class) string {
	size := e.classSize(cl)
	total := (size + tyHdr + tyAlign - 1) &^ (tyAlign - 1)
	return e.bumpAlloc(f, total, size)
}

// tyHdr and tyAlign are TY_HDR and TY_ALIGN from the runtime's header: the
// space before an object's payload, and the alignment every block is rounded to.
const (
	tyHdr   = 16
	tyAlign = 16
)

func (e *llvmEmitter) bumpAlloc(f *fb, total, size int64) string {
	e.declGlobal("ty_self", "thread_local ", "ptr", ", align 8")
	e.declGlobal("ty_gc_threshold", "", "i64", ", align 8")
	e.decl("ty_alloc_slow", "ptr", []string{"i64"}, "")
	e.declMemSet()
	me := e.loadRaw(f, "ptr", "@ty_self", 8)
	bump := e.loadRaw(f, "ptr", me, 8)
	bend := e.loadRaw(f, "ptr", e.gepBytes(f, me, 8), 8)
	end := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds i8, ptr %s, i64 %d", end, bump, total))
	over := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ugt ptr %s, %s", over, end, bend))
	since := e.loadRaw(f, "i64", e.gepBytes(f, me, 16), 8)
	thr := e.loadRaw(f, "i64", "@ty_gc_threshold", 8)
	budget := f.reg()
	f.ins(fmt.Sprintf("%s = icmp sgt i64 %s, %s", budget, since, thr))
	slowp := f.reg()
	f.ins(fmt.Sprintf("%s = or i1 %s, %s", slowp, over, budget))
	slow := f.nextLabel("alloc_slow")
	fast := f.nextLabel("alloc_fast")
	join := f.nextLabel("alloc_join")
	f.cbr(slowp, slow, fast)
	f.label(slow)
	slowObj := f.reg()
	f.ins(fmt.Sprintf("%s = call ptr @ty_alloc_slow(i64 %d)", slowObj, total))
	f.br(join)
	f.label(fast)
	f.ins(fmt.Sprintf("store ptr %s, ptr %s", end, me))
	ns := f.reg()
	f.ins(fmt.Sprintf("%s = add i64 %s, %d", ns, since, total))
	f.ins(fmt.Sprintf("store i64 %s, ptr %s", ns, e.gepBytes(f, me, 16)))
	f.ins(fmt.Sprintf("store i64 %d, ptr %s", total, bump))
	f.ins(fmt.Sprintf("store i64 0, ptr %s", e.gepBytes(f, bump, 8)))
	obj := f.reg()
	f.ins(fmt.Sprintf("%s = getelementptr inbounds i8, ptr %s, i64 %d", obj, bump, tyHdr))
	f.ins(fmt.Sprintf("call void @llvm.memset.p0.i64(ptr %s, i8 0, i64 %d, i1 false)", obj, size))
	f.br(join)
	f.label(join)
	r := f.reg()
	f.ins(fmt.Sprintf("%s = phi ptr [ %s, %%%s ], [ %s, %%%s ]", r, slowObj, slow, obj, fast))
	return r
}

// newExpr lowers `new T(...)`: an allocation, the class stamped on it, the
// class initialized if its initializer has not run, and the constructor.
func (e *llvmEmitter) newExpr(f *fb, v *ast.New) lval {
	ct, ok := e.p.Erased(v.GetType()).(*ast.ClassType)
	if !ok {
		e.refuse(v.GetPos(), "`new` of a type that is not a class")
	}
	cl := ct.Class
	if fn, ok := specialNew[cl.Name]; ok && len(v.Args) == 0 {
		// The runtime allocates this class itself: a builder's buffer is the
		// runtime's, and ty_sb_new is what gives it one.
		e.instantiate(cl)
		obj := e.rtCall(f, fn, e.refType(), nil).v
		f.ins(fmt.Sprintf("store ptr @cls_%s, ptr %s", util.Mangle(cl.Full), obj))
		return value(obj, v.GetType())
	}
	e.instantiate(cl)
	if v.Ctor != nil {
		if nf, ok := nativeTable[nativeKey(v.Ctor)]; ok && nf.fn != "" {
			args := make([]lval, 0, len(v.Args))
			for i, a := range v.Args {
				var want ast.Type
				if i < len(v.Ctor.Params) {
					want = v.Ctor.Params[i]
				}
				args = append(args, e.coerce(f, e.expr(f, a), want))
			}
			if nf.recv != "" && len(args) > 0 {
				args[0] = e.coerce(f, args[0], e.refType())
			}
			return e.rtNamed(f, nf.fn, v.Ctor, args)
		}
	}
	obj := e.allocInstance(f, cl)
	f.ins(fmt.Sprintf("store ptr @cls_%s, ptr %s", util.Mangle(cl.Full), obj))
	e.clinitIfNeeded(f, cl)
	if v.Ctor != nil {
		args := e.callArgs(f, v.Ctor, nil, v.Args)
		args = append(args, e.capturesForNew(f, cl)...)
		e.directCall(f, v.Ctor, obj, args)
	}
	return value(obj, v.GetType())
}

// allocArray allocates an array of n elements and records what it promises
// about them, which is what a reference store into it is checked against.
func (e *llvmEmitter) allocArray(f *fb, elem ast.Type, n int64) string {
	cl := e.useArray()
	e.needClass(cl)
	refs := 0
	elemCls := ""
	switch t := e.p.Erased(elem).(type) {
	case *ast.ClassType:
		e.needClass(t.Class)
		elemCls = "@cls_" + util.Mangle(t.Class.Full)
		refs = 1
	case *ast.ArrayType:
		refs = 1
	case *ast.PrimType:
		// A primitive array records the class of its element, as the C backend's
		// elemClass does and for the same two reasons: it is what
		// java.lang.reflect.Array reads to know which box to build, and it is
		// what lets the runtime name the array when System.arraycopy refuses a
		// copy -- "can not copy long[] into byte[]" is arr_elem_name, which has
		// a->elemcls and nothing else to go on for a primitive. It is not a
		// store check: a primitive array holds no references, so refs above
		// stays 0 and a store into it is unchecked, exactly as in C.
		//
		// needPrimClass and not a bare symbol: the record has to be marked for
		// emission as well as named, and these nine are synthesized rather than
		// declared by the program (see emitPrimClassRecords).
		elemCls = e.needPrimClass(t.Kind)
	}
	arr := e.rtCall(f, "ty_array_new", e.refType(), []lval{
		value(strconv.FormatInt(n, 10), ast.TLong),
		value(strconv.FormatInt(util.SizeOf(elem), 10), ast.TLong),
	}).v
	f.ins(fmt.Sprintf("store i32 %d, ptr %s", refs, e.gepBytes(f, arr, 28)))
	if elemCls != "" {
		f.ins(fmt.Sprintf("store ptr %s, ptr %s", elemCls, e.gepBytes(f, arr, 32)))
	}
	return arr
}

// elemAddrOf is the address of element i of an allocated array, with no tests:
// the index is one this back end computed.
func (e *llvmEmitter) elemAddrOf(f *fb, arr string, i int64, elem ast.Type) string {
	data := e.loadRaw(f, "ptr", e.gepBytes(f, arr, 16), 8)
	return e.gepIndex(f, e.llvmType(elem), data, strconv.FormatInt(i, 10))
}

func (e *llvmEmitter) newArray(f *fb, v *ast.NewArray) lval {
	// The element type is one dimension inside the type the expression has: for
	// `new int[2][2]` the elements of the array being created are int[].
	elem := v.Elem.Resolved
	if at, ok := e.p.Erased(v.GetType()).(*ast.ArrayType); ok {
		elem = at.Elem
	}
	if v.Init != nil {
		return e.arrayInit(f, v.Init, elem)
	}
	if len(v.Dims) == 0 {
		e.refuse(v.GetPos(), "an array with no dimension whose length is written")
	}
	e.useArray()
	return e.newArrayDims(f, elem, v.Dims, v.GetType())
}

// newArrayDims allocates only the dimensions whose length was written: `new
// int[2][]` is two null int[]s and builds no inner array at all.
func (e *llvmEmitter) newArrayDims(f *fb, elem ast.Type, dims []ast.Expr, result ast.Type) lval {
	// Every dimension whose length was written is evaluated once, in the order
	// it was written, before anything is allocated (JLS 15.10.2). Lowering each
	// one where the array it describes is allocated put the ones after the
	// first inside the loop that fills the level above, so `new int[f()][g()]`
	// called g() once per element of the first dimension where the JDK calls it
	// once.
	lens := make([]string, len(dims))
	for i, d := range dims {
		dim := e.coerce(f, e.expr(f, d), ast.TInt)
		n := f.reg()
		f.ins(fmt.Sprintf("%s = sext i32 %s to i64", n, dim.v))
		lens[i] = n
	}
	return e.newArrayLevel(f, elem, lens, result)
}

// newArrayLevel allocates one dimension of an array, given the lengths of the
// dimensions that were written, outermost first.
func (e *llvmEmitter) newArrayLevel(f *fb, elem ast.Type, lens []string, result ast.Type) lval {
	e.markExn("TY_NEGARR", e.p.Builtins.NegArr)
	arr := e.allocArrayN(f, elem, lens[0])
	if len(lens) == 1 {
		return value(arr, result)
	}
	inner := elem
	if at, ok := e.p.Erased(elem).(*ast.ArrayType); ok {
		inner = at.Elem
	}
	// This level holds arrays, so its slots are references and each one is a
	// freshly allocated array of the element type.
	length := e.loadRaw(f, "i64", e.gepBytes(f, arr, 8), 8)
	i := f.tempSlot(ast.TLong, "awi")
	f.ins(fmt.Sprintf("store i64 0, ptr %s", i))
	cond := f.nextLabel("aw_cond")
	body := f.nextLabel("aw_body")
	end := f.nextLabel("aw_end")
	f.br(cond)
	f.label(cond)
	cur := e.loadRaw(f, "i64", i, 8)
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp slt i64 %s, %s", c, cur, length))
	f.cbr(c, body, end)
	f.label(body)
	sub := e.newArrayLevel(f, inner, lens[1:], result)
	slot := e.elemAddrOfDyn(f, arr, cur, 8)
	f.ins(fmt.Sprintf("store ptr %s, ptr %s", sub.v, slot))
	next := f.reg()
	f.ins(fmt.Sprintf("%s = add i64 %s, 1", next, cur))
	f.ins(fmt.Sprintf("store i64 %s, ptr %s", next, i))
	f.br(cond)
	f.label(end)
	return value(arr, result)
}

// allocArrayN is allocArray with a computed length.
func (e *llvmEmitter) allocArrayN(f *fb, elem ast.Type, n string) string {
	cl := e.useArray()
	e.needClass(cl)
	refs := 0
	elemCls := ""
	switch t := e.p.Erased(elem).(type) {
	case *ast.ClassType:
		e.needClass(t.Class)
		elemCls = "@cls_" + util.Mangle(t.Class.Full)
		refs = 1
	case *ast.ArrayType:
		refs = 1
	case *ast.PrimType:
		// The promise of a primitive array is its element's class, as in
		// allocArray above: the runtime names the array with it when
		// System.arraycopy refuses a copy, and reflection reads it to know which
		// box to build.
		elemCls = e.needPrimClass(t.Kind)
	}
	arr := e.rtCall(f, "ty_array_new", e.refType(), []lval{
		value(n, ast.TLong),
		value(strconv.FormatInt(util.SizeOf(elem), 10), ast.TLong),
	}).v
	f.ins(fmt.Sprintf("store i32 %d, ptr %s", refs, e.gepBytes(f, arr, 28)))
	if elemCls != "" {
		f.ins(fmt.Sprintf("store ptr %s, ptr %s", elemCls, e.gepBytes(f, arr, 32)))
	}
	return arr
}

// elemAddrOfDyn is the address of element i (a computed index) of an array.
func (e *llvmEmitter) elemAddrOfDyn(f *fb, arr string, i string, size int64) string {
	data := e.loadRaw(f, "ptr", e.gepBytes(f, arr, 16), 8)
	off := f.reg()
	f.ins(fmt.Sprintf("%s = mul i64 %s, %d", off, i, size))
	return e.gepAt(f, data, off)
}

func (e *llvmEmitter) arrayInit(f *fb, v *ast.ArrayInit, elem ast.Type) lval {
	if elem == nil {
		elem = v.Elem
	}
	e.useArray()
	arr := e.allocArray(f, elem, int64(len(v.Elems)))
	for i, el := range v.Elems {
		slot := e.elemAddrOf(f, arr, int64(i), elem)
		if sub, ok := el.(*ast.ArrayInit); ok {
			inner := elem
			if at, ok := e.p.Erased(elem).(*ast.ArrayType); ok {
				inner = at.Elem
			}
			e.store(f, slot, elem, e.arrayInit(f, sub, inner))
			continue
		}
		want := elem
		e.store(f, slot, want, e.coerce(f, e.expr(f, el), want))
	}
	return value(arr, v.GetType())
}
