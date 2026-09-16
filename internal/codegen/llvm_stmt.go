package codegen

// Statement lowering for the LLVM back end: the same shape the C back end
// writes, block for block -- a loop body begins with a safepoint, a switch case
// body is a break level of its own, and a try statement is a tycatch frame
// armed with setjmp and linked into the thread-local handler chain.

import (
	"fmt"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

// noPos is the position of a diagnostic that belongs to no expression: an
// internal lowerer error, or a construct met while writing a class's table.
var noPos = source.Pos{}

// switchCtx is the switch a `yield` inside a case body belongs to.
type switchCtx struct {
	end   string
	slot  string
	depth int
}

func (e *llvmEmitter) stmt(f *fb, s ast.Stmt) {
	if p, ok := s.(interface{ GetPos() source.Pos }); ok {
		if q := p.GetPos(); q.File != nil {
			f.pos = q
			e.curPos = q
		}
	}
	switch v := s.(type) {
	case *ast.Block:
		e.block(f, v)
	case *ast.Empty:
	case *ast.LocalVar:
		e.localVar(f, v)
	case *ast.LocalClass:
		e.localClassStmt(f, v)
	case *ast.ExprStmt:
		e.exprStmt(f, v.X)
	case *ast.If:
		e.ifStmt(f, v)
	case *ast.While:
		e.whileStmt(f, v)
	case *ast.DoWhile:
		e.doWhile(f, v)
	case *ast.For:
		e.forStmt(f, v)
	case *ast.ForEach:
		e.forEach(f, v)
	case *ast.Return:
		e.retStmt(f, v)
	case *ast.Break:
		e.breakStmt(f, v)
	case *ast.Continue:
		e.continueStmt(f, v)
	case *ast.Throw:
		e.throwStmt(f, v)
	case *ast.Try:
		e.tryStmt(f, v)
	case *ast.Switch:
		e.switchStmt(f, v, "", nil)
	case *ast.Yield:
		e.yieldStmt(f, v)
	case *ast.Labeled:
		e.labeled(f, v)
	case *ast.Assert:
		e.assertStmt(f, v)
	case *ast.Sync:
		e.refuse(noPos, "synchronized: the llvm back end does not lower monitors yet")
	default:
		e.refuse(noPos, "the statement %T: the llvm back end does not lower it", s)
	}
}

func (e *llvmEmitter) block(f *fb, b *ast.Block) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		e.stmt(f, s)
	}
}

func (e *llvmEmitter) exprStmt(f *fb, x ast.Expr) {
	if c, ok := x.(*ast.Call); ok && c.ThisCtor {
		// A this(...) or super(...) call chains to a constructor on the instance
		// being constructed: it is a call like any other, and skipping it here
		// left every delegating constructor running its own body only.
		if c.Method != nil {
			e.directCall(f, c.Method, "%this", e.callArgs(f, c.Method, nil, c.Args))
		}
		return
	}
	e.expr(f, x)
}

// localClassStmt writes what a class declared inside a method leaves behind:
// the instance an @Helper local class is used through. The class itself is
// reached through the `new` that creates it, but that instance is a variable of
// the enclosing method and has to be initialized here -- leaving it to the stack
// is what made an unqualified call reach an object that was never built.
func (e *llvmEmitter) localClassStmt(f *fb, v *ast.LocalClass) {
	if v.Instance == nil {
		return
	}
	slot := f.localSlot(v.Instance)
	if v.InstanceInit == nil {
		e.store(f, slot, v.Instance.Type, f.zero(v.Instance.Type))
		return
	}
	e.store(f, slot, v.Instance.Type, e.coerce(f, e.expr(f, v.InstanceInit), v.Instance.Type))
}

func (e *llvmEmitter) localVar(f *fb, v *ast.LocalVar) {
	for _, vd := range v.Vars {
		if vd.Sym == nil {
			continue
		}
		slot := f.localSlot(vd.Sym)
		if vd.Init == nil {
			e.store(f, slot, vd.Sym.Type, f.zero(vd.Sym.Type))
			continue
		}
		e.store(f, slot, vd.Sym.Type, e.coerce(f, e.expr(f, vd.Init), vd.Sym.Type))
	}
}

func (e *llvmEmitter) ifStmt(f *fb, v *ast.If) {
	then := f.nextLabel("if_then")
	els := f.nextLabel("if_else")
	end := f.nextLabel("if_end")
	f.cbr(e.toBool(f, v.Cond), then, els)
	f.label(then)
	e.stmtAsBlock(f, v.Then)
	if v.Else != nil {
		f.br(end)
		f.label(els)
		e.stmtAsBlock(f, v.Else)
		f.br(end)
	} else {
		f.br(end)
		f.label(els)
		f.br(end)
	}
	f.label(end)
}

func (e *llvmEmitter) stmtAsBlock(f *fb, s ast.Stmt) {
	if b, ok := s.(*ast.Block); ok {
		e.block(f, b)
		return
	}
	e.stmt(f, s)
}

func (e *llvmEmitter) whileStmt(f *fb, v *ast.While) {
	labels := f.takeLabels()
	cond := f.nextLabel("wh_cond")
	body := f.nextLabel("wh_body")
	end := f.nextLabel("wh_end")
	f.br(cond)
	f.label(cond)
	f.cbr(e.toBool(f, v.Cond), body, end)
	f.label(body)
	e.safepoint(f)
	f.pushLoop(loopFrame{cont: cond, brk: end, labels: labels})
	e.stmtAsBlock(f, v.Body)
	f.popLoop()
	f.br(cond)
	f.label(end)
}

func (e *llvmEmitter) doWhile(f *fb, v *ast.DoWhile) {
	labels := f.takeLabels()
	body := f.nextLabel("dw_body")
	cond := f.nextLabel("dw_cond")
	end := f.nextLabel("dw_end")
	f.br(body)
	f.label(body)
	e.safepoint(f)
	f.pushLoop(loopFrame{cont: cond, brk: end, labels: labels})
	e.stmtAsBlock(f, v.Body)
	f.popLoop()
	f.br(cond)
	f.label(cond)
	f.cbr(e.toBool(f, v.Cond), body, end)
	f.label(end)
}

// forStmt lowers the C-style for loop with its update at the end of the body,
// so that an unlabelled continue leaves through the update.
func (e *llvmEmitter) forStmt(f *fb, v *ast.For) {
	labels := f.takeLabels()
	for _, init := range v.Init {
		e.stmt(f, init)
	}
	cond := f.nextLabel("for_cond")
	body := f.nextLabel("for_body")
	upd := f.nextLabel("for_upd")
	end := f.nextLabel("for_end")
	f.br(cond)
	f.label(cond)
	if v.Cond != nil {
		f.cbr(e.toBool(f, v.Cond), body, end)
	} else {
		f.br(body)
	}
	f.label(body)
	e.safepoint(f)
	f.pushLoop(loopFrame{cont: upd, brk: end, labels: labels})
	e.stmtAsBlock(f, v.Body)
	f.popLoop()
	f.br(upd)
	f.label(upd)
	for _, u := range v.Update {
		e.expr(f, u)
	}
	f.br(cond)
	f.label(end)
}

// forEach lowers both forms of the colon loop. The array form walks the
// elements; the Iterable form is an interface dispatch and is refused.
func (e *llvmEmitter) forEach(f *fb, v *ast.ForEach) {
	labels := f.takeLabels()
	if v.Iterable {
		// `for (T x : collection)` is the Iterable protocol: one interface call
		// for the iterator, then hasNext/next per turn. The selector decides the
		// implementation and the receiver's class answers it, exactly as the C
		// back end's three ty_itab calls do.
		itM := e.ifaceMethod(e.p.Builtins.Iterable, "iterator", 0)
		hnM := e.ifaceMethod(e.p.Builtins.Iterator, "hasNext", 0)
		nxM := e.ifaceMethod(e.p.Builtins.Iterator, "next", 0)
		if itM == nil || hnM == nil || nxM == nil {
			e.refuse(noPos, "a colon loop over an Iterable: this program's prelude has no iterator protocol")
		}
		it := e.ifaceDispatch(f, itM, e.expr(f, v.X), nil)
		cond := f.nextLabel("fe_cond")
		body := f.nextLabel("fe_body")
		upd := f.nextLabel("fe_upd")
		end := f.nextLabel("fe_end")
		f.br(cond)
		f.label(cond)
		has := e.ifaceDispatch(f, hnM, it, nil)
		c := f.reg()
		f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, has.v))
		f.cbr(c, body, end)
		f.label(body)
		e.safepoint(f)
		f.pushLoop(loopFrame{cont: upd, brk: end, labels: labels})
		if v.Var.Sym != nil {
			raw := e.ifaceDispatch(f, nxM, it, nil)
			got := e.coerce(f, value(raw.v, nxM.Result), v.Elem)
			e.store(f, f.localSlot(v.Var.Sym), v.Var.Sym.Type, e.coerce(f, got, v.Var.Sym.Type))
		}
		e.stmtAsBlock(f, v.Body)
		f.popLoop()
		f.br(upd)
		f.label(upd)
		f.br(cond)
		f.label(end)
		return
	}
	elem := v.Elem
	arr := e.expr(f, v.X)
	e.nullCheck(f, arr.v)
	length := e.loadRaw(f, "i64", e.gepBytes(f, arr.v, 8), 8)
	i := f.tempSlot(ast.TLong, "fei")
	f.ins(fmt.Sprintf("store i64 0, ptr %s", i))
	cond := f.nextLabel("fe_cond")
	body := f.nextLabel("fe_body")
	upd := f.nextLabel("fe_upd")
	end := f.nextLabel("fe_end")
	f.br(cond)
	f.label(cond)
	cur := e.loadRaw(f, "i64", i, 8)
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp slt i64 %s, %s", c, cur, length))
	f.cbr(c, body, end)
	f.label(body)
	e.safepoint(f)
	f.pushLoop(loopFrame{cont: upd, brk: end, labels: labels})
	if v.Var.Sym != nil {
		slot := e.elemAddrOfDyn(f, arr.v, cur, util.SizeOf(elem))
		var got lval
		if e.isRef(elem) {
			got = value(e.loadRaw(f, "ptr", slot, 8), elem)
		} else {
			got = value(e.loadRaw(f, e.llvmType(elem), slot, util.SizeOf(elem)), elem)
		}
		e.store(f, f.localSlot(v.Var.Sym), v.Var.Sym.Type, e.coerce(f, got, v.Var.Sym.Type))
	}
	e.stmtAsBlock(f, v.Body)
	f.popLoop()
	f.br(upd)
	f.label(upd)
	next := f.reg()
	f.ins(fmt.Sprintf("%s = add i64 %s, 1", next, cur))
	f.ins(fmt.Sprintf("store i64 %s, ptr %s", next, i))
	f.br(cond)
	f.label(end)
}

func (e *llvmEmitter) retStmt(f *fb, v *ast.Return) {
	if v.X == nil {
		e.leaveFinallys(f, -1)
		f.retVoid()
		return
	}
	val := e.coerce(f, e.expr(f, v.X), f.retT)
	if len(f.fins) == 0 {
		f.retVal(val.v)
		return
	}
	// the value is computed first, then every pending finally action runs, and
	// only then does the method return
	slot := f.tempSlot(f.retT, "ret")
	e.store(f, slot, f.retT, val)
	e.leaveFinallys(f, -1)
	f.retVal(e.load(f, slot, f.retT).v)
}

func (e *llvmEmitter) throwStmt(f *fb, v *ast.Throw) {
	ex := e.expr(f, v.X)
	e.decl("ty_throw", "void", []string{"ptr"}, " noreturn")
	f.ins(fmt.Sprintf("call void @ty_throw(ptr %s)", ex.v))
	f.unreachable()
}

// breakStmt leaves the innermost loop or switch, running the finally actions of
// every try statement it abandons.
func (e *llvmEmitter) breakStmt(f *fb, v *ast.Break) {
	fr, ok := f.breakFrame(v.Label)
	if !ok {
		e.refuse(noPos, "a break with no loop or switch to leave")
	}
	e.leaveFinallys(f, fr.depth)
	f.br(fr.brk)
}

func (e *llvmEmitter) continueStmt(f *fb, v *ast.Continue) {
	fr, ok := f.continueFrame(v.Label)
	if !ok {
		e.refuse(noPos, "a continue with no loop to continue")
	}
	e.leaveFinallys(f, fr.depth)
	f.br(fr.cont)
}

func (e *llvmEmitter) labeled(f *fb, v *ast.Labeled) {
	if isLoop(unwrapLabels(v.Body)) {
		f.pendingLabels = append(f.pendingLabels, v.Label)
		e.stmt(f, v.Body)
		return
	}
	e.stmt(f, v.Body)
	f.label(f.brkLabel(v.Label))
}

func (e *llvmEmitter) assertStmt(f *fb, v *ast.Assert) {
	ok := f.nextLabel("assert_ok")
	bad := f.nextLabel("assert_fail")
	f.cbr(e.toBool(f, v.Cond), ok, bad)
	f.label(bad)
	msg := e.cstring("assertion failed")
	if v.Msg != nil {
		if lit, isLit := v.Msg.(*ast.Literal); isLit && lit.Kind == ast.LitString {
			msg = e.cstring(lit.Str)
		} else {
			// the runtime takes a C string, so the message's own bytes are what
			// it gets -- the object's data pointer, not the object
			s := e.expr(f, v.Msg)
			msg = e.loadRaw(f, "ptr", e.gepBytes(f, s.v, 16), 8)
		}
	}
	e.markExn("TY_ASSERT", e.p.Builtins.Assertion)
	e.decl("ty_assertfail", "ptr", []string{"ptr"}, "")
	f.ins(fmt.Sprintf("call ptr @ty_assertfail(ptr %s)", msg))
	f.br(ok)
	f.label(ok)
}

// safepoint is the cooperative half of the collector's stop-the-world protocol:
// one acquire load and a predicted branch, at the top of every loop body, so a
// thread that only loops is still a thread a collection can stop.
func (e *llvmEmitter) safepoint(f *fb) {
	e.declGlobal("ty_stw_request", "", "i32", ", align 4")
	e.decl("ty_safepoint_slow", "void", nil, "")
	v := f.reg()
	f.ins(fmt.Sprintf("%s = load atomic i32, ptr @ty_stw_request acquire, align 4", v))
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, v))
	slow := f.nextLabel("sp_slow")
	ok := f.nextLabel("sp_ok")
	f.cbr(c, slow, ok)
	f.label(slow)
	f.ins("call void @ty_safepoint_slow()")
	f.br(ok)
	f.label(ok)
}

// ---------------------------------------------------------------- switch

// switchStmt lowers a switch statement or a switch expression. Case bodies are
// labels, so a colon case falls through into the next one the way Java's does,
// and a `break` leaves through the end label, which is a break level of its own.
func (e *llvmEmitter) switchStmt(f *fb, s *ast.Switch, slot string, resT ast.Type) {
	if len(s.Cases) == 0 {
		return
	}
	for _, cs := range s.Cases {
		if cs.Pattern != nil || cs.Guard != nil || cs.Null {
			e.refuse(noPos, "a switch case with a type pattern, a guard or null: the llvm back end lowers only constant cases")
		}
	}
	end := f.nextLabel("sw_end")
	def := -1
	caseLabels := make([]string, len(s.Cases))
	for i := range s.Cases {
		caseLabels[i] = f.nextLabel("sw_case")
	}
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			def = i
		}
	}
	f.switches = append(f.switches, switchCtx{end: end, slot: slot, depth: len(f.loops)})
	defer func() { f.switches = f.switches[:len(f.switches)-1] }()
	// A break anywhere in a case body leaves the switch, so the body is a level
	// of its own: without it the break would leave the loop around the switch,
	// and the finally actions of a try inside the case body would not run. A
	// nested loop pushes its own entry above, which restores the loop meaning of
	// a break inside it.
	f.pushLoop(loopFrame{brk: end, isSwitch: true})
	defer f.popLoop()

	sel := e.switchSelector(f, s, caseLabels)
	defLabel := f.nextLabel("sw_def")
	f.ins(fmt.Sprintf("switch %s, label %%%s [\n%s\n  ]", sel.ty, defLabel, sel.cases))
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		if cs.Arrow {
			f.label(caseLabels[i])
			e.switchCaseBody(f, cs, slot, resT, end)
			continue
		}
		f.label(caseLabels[i])
		e.switchCaseBody(f, cs, slot, resT, end)
	}
	f.label(defLabel)
	if def >= 0 {
		e.switchCaseBody(f, s.Cases[def], slot, resT, end)
	} else if slot != "" {
		// A switch expression with no matching case and no default has no value
		// to hand back.
		e.markExn("TY_ILLSTATE", e.p.Builtins.IllState)
		e.decl("ty_illegal_state", "ptr", []string{"ptr"}, "")
		f.ins(fmt.Sprintf("call ptr @ty_illegal_state(ptr %s)", e.cstring("no matching switch case")))
		f.unreachable()
	}
	f.br(end)
	f.label(end)
}

// switchSel is the computed selector of a switch: the LLVM comparison type and
// the case lines that map a label's value onto its case body.
type switchSel struct {
	ty    string
	cases string
}

// switchSelector builds the comparison a switch makes. A String selector is
// compared by value and becomes an index first, because a case label cannot be
// a string.
func (e *llvmEmitter) switchSelector(f *fb, s *ast.Switch, caseLabels []string) switchSel {
	sel := e.expr(f, s.X)
	if _, isStr := e.p.Erased(s.X.GetType()).(*ast.ClassType); isStr && e.isStringType(s.X.GetType()) {
		e.nullCheck(f, sel.v)
		idx := f.tempSlot(ast.TInt, "swk")
		f.ins(fmt.Sprintf("store i32 -1, ptr %s", idx))
		for i, cs := range s.Cases {
			if isDefaultCase(cs) {
				continue
			}
			for _, l := range cs.Labels {
				lit := e.expr(f, l)
				eq := e.rtCall(f, "ty_str_eq", ast.TBoolean, []lval{
					value(sel.v, e.stringType()), value(lit.v, e.stringType())})
				set := f.nextLabel("sw_set")
				next := f.nextLabel("sw_next")
				c := f.reg()
				f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, eq.v))
				f.cbr(c, set, next)
				f.label(set)
				f.ins(fmt.Sprintf("store i32 %d, ptr %s", i, idx))
				f.br(next)
				f.label(next)
			}
		}
		lines := make([]string, 0, len(s.Cases))
		for i, cs := range s.Cases {
			if isDefaultCase(cs) {
				continue
			}
			lines = append(lines, fmt.Sprintf("    i32 %d, label %%%s", i, caseLabels[i]))
		}
		cur := e.loadRaw(f, "i32", idx, 4)
		return switchSel{ty: "i32 " + cur, cases: joinLines(lines)}
	}
	// An enum selector is its ordinal: the runtime reads the ordinal out of the
	// enum base the prelude declares, and every case label is the ordinal of the
	// constant it names, which is what the checker folds an enum constant to.
	selType := s.X.GetType()
	if ct, ok := e.p.Erased(selType).(*ast.ClassType); ok && ct.Class != nil && ct.Class.Kind == ast.KindEnum {
		e.instantiate(ct.Class)
		sel = e.rtCall(f, "ty_enum_ordinal", ast.TInt, []lval{value(sel.v, selType)})
		selType = ast.TInt
	}
	ty := e.llvmType(selType)
	if !isIntLLVM(ty) {
		e.refuse(noPos, "a switch on a %s: the llvm back end lowers integral, String and enum selectors", selType.String())
	}
	op := "sext"
	if isUnsigned(selType) {
		op = "zext"
	}
	wide := f.reg()
	f.ins(fmt.Sprintf("%s = %s %s %s to i64", wide, op, ty, sel.v))
	var lines []string
	seen := map[int64]bool{}
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		for _, l := range cs.Labels {
			n, ok := e.constLabel(l)
			if !ok {
				e.refuse(l.GetPos(), "the case label: the llvm back end lowers only constant labels")
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			lines = append(lines, fmt.Sprintf("    i64 %d, label %%%s", n, caseLabels[i]))
		}
	}
	return switchSel{ty: "i64 " + wide, cases: joinLines(lines)}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// constLabel is the integer a case label stands for.
func (e *llvmEmitter) constLabel(l ast.Expr) (int64, bool) {
	if lit, ok := l.(*ast.Literal); ok {
		switch lit.Kind {
		case ast.LitChar, ast.LitInt, ast.LitLong:
			return int64(lit.Int), true
		}
	}
	if cv := e.p.ConstInt(l); cv != nil {
		return *cv, true
	}
	return 0, false
}

func (e *llvmEmitter) switchCaseBody(f *fb, cs *ast.Case, slot string, resT ast.Type, end string) {
	if cs.ArrowX != nil {
		if slot != "" {
			v := e.coerce(f, e.expr(f, cs.ArrowX), resT)
			e.store(f, slot, resT, v)
		} else {
			e.expr(f, cs.ArrowX)
		}
		f.br(end)
		return
	}
	for _, st := range cs.Body {
		e.stmt(f, st)
	}
	if cs.Arrow {
		f.br(end)
	}
}

func (e *llvmEmitter) yieldStmt(f *fb, v *ast.Yield) {
	if len(f.switches) == 0 {
		e.refuse(noPos, "a yield outside a switch expression")
	}
	sw := f.switches[len(f.switches)-1]
	if sw.slot != "" {
		resT := f.switchResult(sw)
		val := e.coerce(f, e.expr(f, v.X), resT)
		e.store(f, sw.slot, resT, val)
	}
	e.leaveFinallys(f, sw.depth)
	f.br(sw.end)
}

// switchResult is a switch expression's result type, which is the type of the
// slot the enclosing SwitchExpr allocated for it.
func (f *fb) switchResult(sw switchCtx) ast.Type { return f.swType[sw.slot] }

// leaveFinallys runs the finally actions of every try statement a jump out of
// the loop at loopDepth abandons, innermost first, and restores the handler
// chain of every frame it passes.
//
// A frame may have nothing to run: a try with a catch and no finally registers
// one of its own, because leaving it still has to take ty_cur_catch off it.
// Only restoring the chain where the body falls through would leave a return, a
// break or a continue pointing at a frame of a call that had already returned,
// and the next throw would longjmp into freed stack.
func (e *llvmEmitter) leaveFinallys(f *fb, loopDepth int) {
	if len(f.fins) == 0 {
		return
	}
	saved := f.fins
	for i := len(saved) - 1; i >= 0; i-- {
		fr := saved[i]
		if fr.depth <= loopDepth {
			continue
		}
		// the block itself must not see its own frame, or a return inside it
		// would run the same finally twice
		f.fins = saved[:i]
		if fr.frame != "" {
			e.restoreCatch(f, fr.frame)
		}
		if fr.emit != nil {
			fr.emit()
		}
	}
	f.fins = saved
}

// ---------------------------------------------------------------- try

func (e *llvmEmitter) tryStmt(f *fb, v *ast.Try) {
	// From here on every access to this function's own slots is volatile: the
	// catch runs on the path setjmp's second return takes, and only memory is
	// guaranteed to be there. See the fb.slots comment for why this is the same
	// rule C puts on a local live across a setjmp.
	f.volatile = true
	if len(v.Resources) > 0 {
		e.refuse(noPos, "try-with-resources: the llvm back end does not lower interface dispatch (AutoCloseable.close)")
	}
	hasFinally := v.Finally != nil
	depth := len(f.loops)
	var finSlot, finEx string
	finBody := f.nextLabel("fin_body")
	finLand := f.nextLabel("fin_land")
	finDone := f.nextLabel("fin_done")
	if hasFinally {
		finSlot = f.rawSlot("%tycatch", "fin", 8)
		finEx = f.tempSlot(e.refType(), "finex")
		e.store(f, finEx, e.refType(), value("null", e.refType()))
		e.armCatch(f, finSlot)
		f.cbr(e.setjmp(f, finSlot), finLand, finBody)
		f.label(finBody)
		// The frame is registered around the body *and* the catch clauses: a
		// return from either one still runs the finally, which is what Java
		// says a finally is for.
		f.pushFin(handlerFrame{depth: depth, frame: finSlot, emit: func() {
			e.block(f, v.Finally)
		}})
	}
	if len(v.Catches) > 0 {
		cSlot := f.rawSlot("%tycatch", "catch", 8)
		body := f.nextLabel("try_body")
		land := f.nextLabel("try_land")
		done := f.nextLabel("try_done")
		e.armCatch(f, cSlot)
		f.cbr(e.setjmp(f, cSlot), land, body)
		f.label(body)
		f.pushFin(handlerFrame{depth: depth, frame: cSlot})
		e.block(f, v.Body)
		f.popFin()
		f.br(done)
		f.label(land)
		ex := e.loadRaw(f, "ptr", e.gepBytes(f, cSlot, 208), 8)
		e.restoreCatch(f, cSlot)
		for _, cat := range v.Catches {
			cond := e.catchCond(f, cat, ex)
			body := f.nextLabel("cat_body")
			next := f.nextLabel("cat_next")
			f.cbr(cond, body, next)
			f.label(body)
			if cat.Sym != nil {
				e.store(f, f.localSlot(cat.Sym), cat.Sym.Type, value(ex, cat.Sym.Type))
			}
			e.block(f, cat.Body)
			f.br(done)
			f.label(next)
		}
		// nothing matched: the handler chain is already restored, so the
		// exception goes on to the enclosing handler
		e.rethrow(f, ex)
		f.label(done)
		e.restoreCatch(f, cSlot)
	} else {
		e.block(f, v.Body)
	}
	if hasFinally {
		f.popFin()
		f.br(finDone)
		f.label(finLand)
		ex := e.loadRaw(f, "ptr", e.gepBytes(f, finSlot, 208), 8)
		e.store(f, finEx, e.refType(), value(ex, e.refType()))
		f.br(finDone)
		f.label(finDone)
		e.restoreCatch(f, finSlot)
		e.block(f, v.Finally)
		exNow := e.load(f, finEx, e.refType())
		none := f.reg()
		f.ins(fmt.Sprintf("%s = icmp eq ptr %s, null", none, exNow.v))
		ret := f.nextLabel("fin_ret")
		ok := f.nextLabel("fin_ok")
		f.cbr(none, ok, ret)
		f.label(ret)
		e.rethrow(f, exNow.v)
		f.label(ok)
	}
}

// armCatch installs a tycatch frame at the head of the thread's handler chain,
// which is what ty_throw longjmps to.
func (e *llvmEmitter) armCatch(f *fb, slot string) {
	e.declGlobal("ty_cur_catch", "thread_local ", "ptr", ", align 8")
	prev := e.loadRaw(f, "ptr", "@ty_cur_catch", 8)
	f.ins(fmt.Sprintf("store ptr %s, ptr %s", prev, e.gepBytes(f, slot, 200)))
	f.ins(fmt.Sprintf("store ptr null, ptr %s", e.gepBytes(f, slot, 208)))
	f.ins(fmt.Sprintf("store ptr %s, ptr @ty_cur_catch", slot))
}

// restoreCatch takes a frame back off the chain.
func (e *llvmEmitter) restoreCatch(f *fb, slot string) {
	prev := e.loadRaw(f, "ptr", e.gepBytes(f, slot, 200), 8)
	f.ins(fmt.Sprintf("store ptr %s, ptr @ty_cur_catch", prev))
}

// setjmp arms the frame's buffer and answers with the i1 that says whether the
// jump came back: a nonzero result is a throw that landed here.
func (e *llvmEmitter) setjmp(f *fb, slot string) string {
	e.decl("setjmp", "i32", []string{"ptr"}, " returns_twice")
	r := f.reg()
	f.ins(fmt.Sprintf("%s = call i32 @setjmp(ptr %s)", r, slot))
	c := f.reg()
	f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, r))
	return c
}

func (e *llvmEmitter) rethrow(f *fb, ex string) {
	e.decl("ty_throw", "void", []string{"ptr"}, " noreturn")
	f.ins(fmt.Sprintf("call void @ty_throw(ptr %s)", ex))
	f.unreachable()
}

// catchCond is whether a catch clause takes an exception: a type test per type
// it names, which is Java's multi-catch.
func (e *llvmEmitter) catchCond(f *fb, cat *ast.Catch, ex string) string {
	var parts []string
	for _, te := range cat.Types {
		ct, ok := e.p.Erased(te.Resolved).(*ast.ClassType)
		if !ok {
			e.refuse(noPos, "a catch clause whose type is not a class")
		}
		e.needClass(ct.Class)
		r := e.rtCall(f, "ty_instanceof", ast.TBoolean, []lval{
			value(ex, e.refType()),
			value("@cls_"+util.Mangle(ct.Class.Full), nil),
		})
		c := f.reg()
		f.ins(fmt.Sprintf("%s = icmp ne i32 %s, 0", c, r.v))
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return "true"
	}
	cur := parts[0]
	for _, p := range parts[1:] {
		r := f.reg()
		f.ins(fmt.Sprintf("%s = or i1 %s, %s", r, cur, p))
		cur = r
	}
	return cur
}

// ---------------------------------------------------------------- constructors

// ctorBody writes a constructor: the statements before an explicit this() or
// super() call, the chained call, the instance field initializers, and the body.
func (e *llvmEmitter) ctorBody(f *fb, cl *ast.Class, m *ast.Method, body *ast.Block) {
	idx := -1
	if body != nil {
		idx = ctorCallIndex(body)
	}
	if idx > 0 {
		for _, st := range body.Stmts[:idx] {
			e.stmt(f, st)
		}
	}
	switch {
	case m.SynthKind == "anon-ctor" && m.Forward != nil:
		// An anonymous or enum-constant subclass's constructor forwards its own
		// parameters to the constructor it was declared against -- for an enum
		// constant that is the enum's own constructor, which is where the
		// constant's arguments (ADD(1)'s 1) are used. Chaining to a no-argument
		// constructor here instead was what dropped them.
		args := make([]lval, 0, len(m.Params))
		for i, p := range m.Params {
			args = append(args, value(fmt.Sprintf("%%a%d", i), p))
		}
		e.directCall(f, m.Forward, "%this", args)
	case idx >= 0:
		// the explicit call is emitted with the rest of the body
	case cl.Super == nil || cl.Super.Class == e.p.Builtins.Object:
	default:
		if sup := e.cEmitter().findSuperCtor(cl); sup != nil {
			e.needMethod(sup)
			e.directCall(f, sup, "%this", nil)
		}
	}
	// the captures arrive as trailing parameters, before the field initializers
	// run -- an initializer may read one
	if len(cl.CapFields) > 0 {
		e.bindCapturesFromParams(f, cl, m)
	}
	e.fieldInits(f, cl)
	if body != nil {
		stmts := body.Stmts
		if idx > 0 {
			stmts = stmts[idx:]
		}
		for _, st := range stmts {
			e.stmt(f, st)
		}
	}
	// A record's components are assigned where the C back end assigns them:
	// after a compact constructor's body, which is the by-hand validation Java
	// gives a record, and after the body of the canonical one.
	if m.Decl != nil && m.Decl.Compact {
		e.recordAssign(f, cl, m)
		return
	}
	if m.SynthKind == "record-ctor" {
		e.recordAssign(f, cl, m)
	}
}

// fieldInits runs the instance field initializers in declaration order, after
// the superclass constructor has run.
func (e *llvmEmitter) fieldInits(f *fb, cl *ast.Class) {
	if cl.Decl == nil {
		return
	}
	for _, mem := range cl.Decl.Members {
		fd, ok := mem.(*ast.FieldDecl)
		if !ok {
			continue
		}
		for _, vd := range fd.Vars {
			if vd.Init == nil || vd.Fld == nil || vd.Fld.Mods.Has(ast.ModStatic) {
				continue
			}
			ptr := e.fieldAddr(f, vd.Fld, "%this", false)
			e.store(f, ptr, vd.Fld.Type, e.coerce(f, e.expr(f, vd.Init), vd.Fld.Type))
		}
	}
	for _, mem := range cl.Decl.Members {
		if ib, ok := mem.(*ast.InitBlock); ok && !ib.Static {
			e.block(f, ib.Body)
		}
	}
}

// accessorBody writes a default property accessor, whose body is the backing
// field.
func (e *llvmEmitter) accessorBody(f *fb, m *ast.Method) {
	fd := e.canonicalField(m.Prop)
	if fd == nil {
		e.refuse(noPos, "the synthesized member %s.%s", m.Owner.Full, m.Name)
	}
	if fd.Mods.Has(ast.ModStatic) {
		e.needClass(fd.Owner)
		e.clinitIfNeeded(f, fd.Owner)
		g := "@" + util.Mangle(staticName(fd.Owner, fd))
		if m.Accessor != nil && m.Accessor.IsSet {
			e.store(f, g, fd.Type, e.coerce(f, value("%a0", m.Params[0]), fd.Type))
			f.retVoid()
			return
		}
		f.retVal(e.load(f, g, fd.Type).v)
		return
	}
	ptr := e.fieldAddr(f, fd, "%this", false)
	if m.Accessor != nil && m.Accessor.IsSet {
		e.store(f, ptr, fd.Type, e.coerce(f, value("%a0", m.Params[0]), fd.Type))
		f.retVoid()
		return
	}
	f.retVal(e.load(f, ptr, fd.Type).v)
}
