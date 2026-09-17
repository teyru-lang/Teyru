package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
)

// localClass emits the instance Lombok makes for an @Helper local class. An
// ordinary local class declares nothing at run time.
func (e *Emitter) localClass(v *ast.LocalClass) {
	if v.Instance == nil || v.InstanceInit == nil {
		return
	}
	e.line("%s %s = %s;\n", e.ctype(v.Instance.Type), e.localName(v.Instance), e.expr(v.InstanceInit))
}

// localName returns the stable C name of a local variable or parameter.
func (e *Emitter) localName(v *ast.Var) string {
	if v.ID < 0 {
		return "this"
	}
	/* Every read and every write of a local comes through here, which is what
	   makes it the place that knows a reference was mentioned: the kill pass
	   (frames.go) never has to walk the tree and cannot miss a use the emitter
	   makes, because a use it did not record here would not be in the C. */
	if e.useNow != nil {
		e.useNow[v] = true
	}
	if n, ok := e.locals[v]; ok {
		return n
	}
	n := fmt.Sprintf("v%d_%s", v.ID, mangle(v.Name))
	e.locals[v] = n
	return n
}

func (e *Emitter) emitBlockInner(b *ast.Block) {
	if b == nil {
		return
	}
	if !e.frameMap {
		for _, s := range b.Stmts {
			e.stmt(s)
		}
		return
	}
	/* One statement's text at a time, so that the store that ends a reference's
	   life can be written after the last statement that mentions it rather than
	   after every one of them. The variables this block declares are the only
	   ones it decides for: a variable of the enclosing block is still live after
	   this one, which is the case a kill written inside a branch would get
	   wrong. */
	decls := e.declInBlock(b)
	texts := make([]string, len(b.Stmts))
	uses := make([]map[*ast.Var]bool, len(b.Stmts))
	for i, s := range b.Stmts {
		s := s
		outer := e.useNow
		e.useNow = map[*ast.Var]bool{}
		texts[i] = e.capture(func() { e.stmt(s) })
		uses[i] = e.useNow
		e.useNow = outer
		/* A use inside a nested statement is a use of this statement too: the
		   enclosing block decides for its own variables, and the loop or branch
		   it is looking at is one statement of its sequence. */
		for v := range uses[i] {
			if e.useNow != nil {
				e.useNow[v] = true
			}
		}
	}
	last := map[*ast.Var]int{}
	for i, u := range uses {
		for v := range u {
			if decls[v] {
				last[v] = i
			}
		}
	}
	kills := map[int][]*ast.Var{}
	for v, i := range last {
		kills[i] = append(kills[i], v)
	}
	for i := range texts {
		e.code.WriteString(texts[i])
		vs := kills[i]
		sort.Slice(vs, func(a, b int) bool { return e.localName(vs[a]) < e.localName(vs[b]) })
		for _, v := range vs {
			if killsOff {
				continue
			}
			e.line("%s = NULL;\n", e.localName(v))
		}
		/* The frame's own words for values that were in flight: a value cannot
		   still be waiting for the call that consumes it once the statement is
		   over, so the words they were held in go back. The word, not the
		   variable -- what the expression answered with has been read out of it
		   by now. */
		if n := len(e.frameTemps); n > 0 {
			for k := 0; k < n; k++ {
				e.line("%s(&%s, %d, NULL);\n", framePut, frameVar, e.frameBase+k)
			}
		}
		/* And where the statement ended, which is what tells the map's pass
		   (frames.go) to give the temporaries *it* registered their words back
		   in the same way. */
		e.line("%s\n", frameStmtEnd)
	}
}

func (e *Emitter) stmt(s ast.Stmt) {
	switch v := s.(type) {
	case *ast.Block:
		e.line("{\n")
		e.indent++
		e.emitBlockInner(v)
		e.indent--
		e.line("}\n")
	case *ast.Empty:
	case *ast.LocalVar:
		e.localVar(v)
	case *ast.LocalClass:
		e.localClass(v)
	case *ast.ExprStmt:
		e.exprStmt(v.X)
	case *ast.If:
		e.hoistPatterns(v.Cond)
		e.line("if (%s) {\n", e.cond(v.Cond))
		e.indent++
		e.stmtAsBlock(v.Then)
		e.indent--
		if v.Else != nil {
			e.line("} else {\n")
			e.indent++
			e.stmtAsBlock(v.Else)
			e.indent--
		}
		e.line("}\n")
		e.clearPatterns()
	case *ast.While:
		labels := e.takeLabels()
		inner := e.capture(func() {
			e.hoistPatterns(v.Cond)
			e.line("while (%s) {\n", e.cond(v.Cond))
			e.indent++
			e.safepoint()
			e.pushLoop("")
			e.stmtAsBlock(v.Body)
			e.popLoop()
			e.contLabels(labels)
			e.indent--
			e.line("}\n")
		})
		// the bindings are declared once, outside the loop
		e.code.WriteString(inner)
		e.brkLabels(labels)
		e.clearPatterns()
	case *ast.DoWhile:
		labels := e.takeLabels()
		// Java keeps the variable a pattern binds out of the body's scope, so
		// the declaration stands before the loop, where the condition that
		// follows `while` can still see it
		e.hoistPatterns(v.Cond)
		// the condition is rendered here and written after the body: emitting
		// the body would drop the record of which patterns this condition
		// declared, and the text must still name them
		cond := e.cond(v.Cond)
		e.line("do {\n")
		e.indent++
		e.safepoint()
		e.pushLoop("")
		e.stmtAsBlock(v.Body)
		e.popLoop()
		e.contLabels(labels)
		e.indent--
		e.line("} while (%s);\n", cond)
		e.brkLabels(labels)
		e.clearPatterns()
	case *ast.For:
		labels := e.takeLabels()
		e.line("{\n")
		e.indent++
		for _, init := range v.Init {
			e.stmt(init)
		}
		// the update and the body both run after the condition, so a variable
		// a pattern in the condition binds belongs to the whole loop
		e.hoistPatterns(v.Cond)
		cond := "1"
		if v.Cond != nil {
			cond = e.cond(v.Cond)
		}
		var up []string
		for _, u := range v.Update {
			up = append(up, e.expr(u))
		}
		// the update lives at the end of the body, so both an unlabelled
		// continue and a labelled one must jump over the rest of the body
		cont := e.tmpName()
		e.line("while (%s) {\n", cond)
		e.indent++
		e.safepoint()
		e.pushLoop(cont)
		e.stmtAsBlock(v.Body)
		e.popLoop()
		e.contLabels(labels)
		e.line("%s: ;\n", cont)
		if len(up) > 0 {
			e.line("%s;\n", strings.Join(up, ", "))
		}
		e.indent--
		e.line("}\n")
		e.indent--
		e.line("}\n")
		e.brkLabels(labels)
		e.clearPatterns()
	case *ast.ForEach:
		e.forEach(v)
	case *ast.Return:
		if v.X == nil {
			e.leaveFinallys(-1)
			e.line("return;\n")
		} else if len(e.finallys) == 0 {
			e.line("return %s;\n", e.coerce(e.expr(v.X), v.X.GetType(), e.retType))
		} else {
			// the value is computed first, then every pending finally action
			// runs, and only then does the method return
			tmp := e.tmpName()
			e.line("%s %s = %s;\n", e.ctype(e.retType), tmp, e.coerce(e.expr(v.X), v.X.GetType(), e.retType))
			e.leaveFinallys(-1)
			e.line("return %s;\n", tmp)
		}
	case *ast.Break:
		target := len(e.loops) - 1
		if v.Label != "" {
			target = e.labelDepth[v.Label]
		}
		e.leaveFinallys(target)
		switch {
		case v.Label != "":
			e.line("goto %s;\n", e.labelName(v.Label, true))
		case e.breakTarget() != "":
			e.line("goto %s;\n", e.breakTarget())
		default:
			e.line("break;\n")
		}
	case *ast.Continue:
		target := e.continueDepth()
		if v.Label != "" {
			target = e.labelDepth[v.Label]
		}
		e.leaveFinallys(target)
		switch {
		case v.Label != "":
			e.line("goto %s;\n", e.labelName(v.Label, false))
		case e.continueTarget() != "":
			e.line("goto %s;\n", e.continueTarget())
		default:
			e.line("continue;\n")
		}
	case *ast.Throw:
		e.line("ty_throw((tyobj*)%s);\n", e.refExpr(v.X))
	case *ast.Try:
		e.tryStmt(v)
	case *ast.Switch:
		e.switchStmt(v, "")
	case *ast.Yield:
		e.yieldStmt(v)
	case *ast.Labeled:
		// a loop turns the pending labels into its continue and break targets
		// (chained labels such as `a: b: for (...) {}` all apply to the loop);
		// any other statement only has the break target
		if isLoop(unwrapLabels(v.Body)) {
			e.pendingLabels = append(e.pendingLabels, v.Label)
			e.stmt(v.Body)
			return
		}
		e.stmt(v.Body)
		e.line("%s: ;\n", e.labelName(v.Label, true))
	case *ast.Assert:
		e.line("if (!(%s)) { ty_assertfail(%s); }\n", e.cond(v.Cond), e.assertMsg(v))
	case *ast.Sync:
		e.syncBlock("(void*)"+e.refExpr(v.Lock), true, func() {
			e.emitBlockInner(v.Body)
		})
	}
}

func (e *Emitter) assertMsg(v *ast.Assert) string {
	if v.Msg == nil {
		return "NULL"
	}
	if lit, ok := v.Msg.(*ast.Literal); ok && lit.Kind == ast.LitString {
		return e.cstr(lit.Str)
	}
	return e.tmpRef(e.expr(v.Msg))
}

func isLoop(s ast.Stmt) bool {
	switch s.(type) {
	case *ast.While, *ast.DoWhile, *ast.For, *ast.ForEach:
		return true
	}
	return false
}

// unwrapLabels removes the labels stacked in front of a statement, so that
// `a: b: for (...) {}` is recognised as a labelled loop.
func unwrapLabels(s ast.Stmt) ast.Stmt {
	for {
		l, ok := s.(*ast.Labeled)
		if !ok {
			return s
		}
		s = l.Body
	}
}

func (e *Emitter) labelName(l string, brk bool) string {
	if brk {
		return "brk_" + mangle(l)
	}
	return "lbl_" + mangle(l)
}

// takeLabels removes the labels pending for the current statement, so that
// statements nested inside it do not inherit them.
func (e *Emitter) takeLabels() []string {
	l := e.pendingLabels
	e.pendingLabels = nil
	return l
}

// contLabels emits the targets of `continue label`.
func (e *Emitter) contLabels(labels []string) {
	for _, l := range labels {
		e.line("%s: ;\n", e.labelName(l, false))
	}
}

// brkLabels emits the targets of `break label`.
func (e *Emitter) brkLabels(labels []string) {
	for _, l := range labels {
		e.line("%s: ;\n", e.labelName(l, true))
	}
}

// leaveFinallys runs the finally actions of every try statement that a jump out
// of the loop at loopDepth abandons, innermost first, and restores the handler
// chain of every frame it passes. A depth of -1 means the whole method is being
// left.
//
// A frame may have nothing to run: a try with a catch and no finally registers
// a frame of its own, because leaving it still has to take ty_cur_catch off the
// frame. Only restoring it where the body falls through — which was the only
// restore there was — left a return, a break, a continue or a yield out of the
// body with the chain pointing at a frame of a call that had already returned:
// the next throw longjmped into freed stack, which crashed or, if the frame was
// still intact, resumed an older iteration and re-ran its catch forever.
func (e *Emitter) leaveFinallys(loopDepth int) {
	if len(e.finallys) == 0 {
		return
	}
	saved := e.finallys
	for i := len(saved) - 1; i >= 0; i-- {
		f := saved[i]
		if f.depth <= loopDepth {
			continue
		}
		// the block itself must not see its own frame, or a return inside it
		// would run the same finally twice
		e.finallys = saved[:i]
		e.line("ty_cur_catch = %s.prev;\n", f.name)
		if f.emit == nil {
			continue
		}
		e.line("{\n")
		e.indent++
		f.emit()
		e.indent--
		e.line("}\n")
	}
	e.finallys = saved
}

// continueTarget returns the label an unlabelled continue must jump to in the
// innermost enclosing loop, or "" when C's continue statement already means
// the right thing.
func (e *Emitter) continueTarget() string {
	i := e.continueDepth()
	if i < 0 {
		return ""
	}
	return e.loops[i]
}

// continueDepth returns the position in the loop stack of the innermost loop an
// unlabelled continue can reach, or -1 when no loop encloses it. A switch case
// body sits on the same stack but is not a loop: a continue never leaves the
// switch it is in, so it keeps looking further out for its loop.
func (e *Emitter) continueDepth() int {
	for i := len(e.loops) - 1; i >= 0; i-- {
		if !isCaseEnd(e.loops[i]) {
			return i
		}
	}
	return -1
}

// breakTarget returns the label an unlabelled break must jump to, or "" when
// C's break statement already leaves the innermost breakable statement. The top
// of the loop stack is that statement: a case body pushes the label that ends
// its switch, and a C break would leave the loop around the switch instead,
// while a loop pushes its continue target, which C's break already handles.
func (e *Emitter) breakTarget() string {
	if n := len(e.loops); n > 0 && isCaseEnd(e.loops[n-1]) {
		return e.loops[n-1]
	}
	return ""
}

// caseDepth returns the position in the loop stack of the case body the
// statement being emitted sits in, or -1 when it sits in none. The innermost
// case body is the one whose switch a yield belongs to: a switch nested in a
// case body pushes an entry of its own above.
func (e *Emitter) caseDepth() int {
	for i := len(e.loops) - 1; i >= 0; i-- {
		if isCaseEnd(e.loops[i]) {
			return i
		}
	}
	return -1
}

// yieldStmt lowers a yield inside a case body.
//
// Java gives the value to the switch expression the case body belongs to,
// whatever statement sits between the two — an if, a loop or a try does not
// change where the value goes — so the switch being emitted is read out of the
// emitter instead of being looked for among the statements of the body. Only a
// yield written directly in the body could be found that way, and the rest were
// turned into a comment: the value was dropped and the control flow fell
// through into the next case body, which overwrote the result.
//
// e.switchCur holds the id of the switch in progress, and carries whether it
// has a result in its sign: a switch expression stores the id, a switch
// statement stores it negated because there is no value to hand back.
func (e *Emitter) yieldStmt(v *ast.Yield) {
	depth := e.caseDepth()
	if depth < 0 {
		// The checker accepts a yield outside a switch expression, so one can
		// still reach the emitter; jumping to a label that was never emitted
		// would not compile at all.
		e.line("/* yield outside of a switch expression */;\n")
		return
	}
	id, result := e.switchCur, e.switchCur >= 0
	if !result {
		id = -e.switchCur - 1
	}
	if result {
		// the value is read before any finally action runs, as it is for a
		// return: the actions are part of leaving, not of computing the value
		e.line("*_res%d = %s;\n", id, e.expr(v.X))
	}
	// a yield abandons every loop, try and catch frame between the yield and
	// its case body, and their finally actions run before the jump
	e.leaveFinallys(depth)
	e.line("goto _end%d;\n", id)
}

func (e *Emitter) pushLoop(target string) { e.loops = append(e.loops, target) }

func (e *Emitter) popLoop() { e.loops = e.loops[:len(e.loops)-1] }

func (e *Emitter) stmtAsBlock(s ast.Stmt) {
	if b, ok := s.(*ast.Block); ok {
		e.emitBlockInner(b)
		return
	}
	e.stmt(s)
}

func (e *Emitter) localVar(v *ast.LocalVar) {
	for _, vd := range v.Vars {
		if vd.Sym == nil {
			continue
		}
		ct := e.ctype(vd.Sym.Type)
		name := e.localName(vd.Sym)
		if e.fnTry {
			// See the comment where fnTry is set: a value that has to survive a
			// longjmp is indeterminate unless it is volatile, and a stack
			// allocation cannot be qualified at all, so in a function that
			// catches, locals are ordinary volatile C variables.
			if vd.Init == nil {
				e.line("%s volatile %s = %s;\n", ct, name, zeroOf(ct))
				continue
			}
			e.hoistPatterns(vd.Init)
			e.line("%s volatile %s = %s;\n", ct, name, e.coerce(e.expr(vd.Init), vd.Init.GetType(), vd.Sym.Type))
			e.clearPatterns()
			continue
		}
		if vd.Init == nil {
			e.line("%s %s = %s;\n", ct, name, zeroOf(ct))
			continue
		}
		if nw, ok := e.stackLocals[vd.Sym]; ok && nw == vd.Init {
			e.stackNew(vd, nw, ct, name)
			continue
		}
		// An `instanceof` pattern binds a variable whose scope is the rest of
		// the expression, so `boolean b = o instanceof String s && s.length() > 0`
		// is legal Java. Declare it here, before the statement that reads it:
		// without this the emitted C named a variable that was never declared,
		// and the compiler reported it as a C error in generated code.
		e.hoistPatterns(vd.Init)
		e.line("%s %s = %s;\n", ct, name, e.coerce(e.expr(vd.Init), vd.Init.GetType(), vd.Sym.Type))
		e.clearPatterns()
	}
}

// stackNew allocates an object that provably does not leave the method as a C
// local, so LLVM can promote its fields and drop the allocation altogether.
func (e *Emitter) stackNew(vd *ast.VarDeclarator, nw *ast.New, ct, name string) {
	cty, _ := e.prog.Erased(vd.Sym.Type).(*ast.ClassType)
	cl := cty.Class
	slot := "_stack_" + name
	e.line("%s %s;\n", cname(cl), slot)
	e.line("memset(&%s, 0, sizeof(%s));\n", slot, slot)
	e.line("%s.obj.cls = &cls_%s;\n", slot, mangle(cl.Full))
	if cl.Inner && cl.OuterField != nil {
		e.line("%s.f_%s = (%s*)%s;\n", slot, mangle(cl.OuterField.Name), cname(cl.Outer), e.outerArg(nw, cl.Outer))
	}
	e.line("%s", e.clinitStmt(cl))
	e.line("%s(%s);\n", e.cfunc(nw.Ctor), e.argsWithCaptures("&"+slot, nw, cl))
	e.line("%s %s = &%s;\n", ct, name, slot)
}

// exprStmt emits an expression as a statement.
func (e *Emitter) exprStmt(x ast.Expr) {
	switch v := x.(type) {
	case *ast.Call:
		if v.ThisCtor {
			recv := "this"
			if v.Super {
				recv = "((void*)this)"
			}
			e.line("%s(%s);\n", e.cfunc(v.Method), e.args(recv, v.Args, v.Method))
			return
		}
		e.line("%s;\n", e.callExpr(v))
	case *ast.Assign, *ast.Unary:
		e.line("%s;\n", e.expr(x))
	case *ast.New:
		e.line("%s;\n", e.expr(x))
	case *ast.SwitchExpr:
		e.line("%s;\n", e.expr(x))
	default:
		e.line("(void)(%s);\n", e.expr(x))
	}
}

// args renders the C argument list of a call, inserting conversions.
func (e *Emitter) args(recv string, list []ast.Expr, m *ast.Method) string {
	return e.argsFor(recv, list, m, nil)
}

// argsFor renders a call's argument list; call is used to consult the varargs
// decision made during overload resolution and may be nil.
func (e *Emitter) argsFor(recv string, list []ast.Expr, m *ast.Method, call *ast.Call) string {
	if call != nil && m != nil && m.Varargs && e.prog.VarargsDirect(call) {
		var parts []string
		if recv != "" && !m.IsStatic() {
			parts = append(parts, "("+cname(m.Owner)+"*)"+recv)
		}
		for i, a := range list {
			var want ast.Type
			if i < len(m.Params) {
				want = m.Params[i]
			}
			parts = append(parts, e.coerce(e.expr(a), a.GetType(), want))
		}
		return strings.Join(parts, ", ")
	}
	var parts []string
	if recv != "" && m != nil && !m.IsStatic() {
		parts = append(parts, "("+cname(m.Owner)+"*)"+recv)
	} else if recv != "" && m == nil {
		parts = append(parts, recv)
	}
	for i, a := range list {
		var want ast.Type
		if m != nil {
			// A variable-arity call gives every argument from the array
			// parameter's position on the *element* type, not the array's:
			// the argument at index len(Params)-1 is the first vararg, and
			// treating it as the array is what left `String.format("%d", 7)`
			// passing a bare 7 where an Object* goes -- the value was never
			// boxed, and the callee read address 7.
			if m.Varargs && len(m.Params) > 0 && i >= len(m.Params)-1 {
				last := m.Params[len(m.Params)-1]
				if arr, ok := last.(*ast.ArrayType); ok {
					want = arr.Elem
					// An argument that is already the array is passed through
					// as one: `printf("%d", new Object[]{n})` hands over the
					// array, and coercing it to the element type would wrap it
					// in a second array whose single element is the first.
					if _, isArr := a.GetType().(*ast.ArrayType); isArr {
						want = last
					}
				}
			} else if i < len(m.Params) {
				want = m.Params[i]
			}
		}
		parts = append(parts, e.coerce(e.expr(a), a.GetType(), want))
	}
	// varargs packing, unless overload resolution chose to pass the array itself
	if m != nil && m.Varargs {
		direct := call != nil && e.prog.VarargsDirect(call)
		if !direct && call != nil {
			e.packVarargs(&parts, list, m)
		} else if call == nil && len(list) != len(m.Params) {
			e.packVarargs(&parts, list, m)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func (e *Emitter) packVarargs(parts *[]string, list []ast.Expr, m *ast.Method) {
	// the last parameter is an array; gather the remaining arguments into one
	n := len(m.Params) - 1
	off := boolToInt(!m.IsStatic())
	head := []string{}
	tail := *parts
	if len(*parts) >= n+off {
		head = (*parts)[:n+off]
		tail = (*parts)[n+off:]
	}
	elem := m.Params[len(m.Params)-1].(*ast.ArrayType).Elem
	es := e.elemSize(elem)
	var b strings.Builder
	b.WriteString("({ tyarr* _va = ty_array_new(" + fmt.Sprint(len(tail)) + ", " + es + ");")
	if e.isRef(elem) {
		b.WriteString(" _va->refs = 1;")
	}
	for i, t := range tail {
		if e.isRef(elem) {
			fmt.Fprintf(&b, " ((void**)_va->data)[%d] = (void*)%s;", i, t)
		} else {
			fmt.Fprintf(&b, " ((%s*)_va->data)[%d] = %s;", e.ctype(elem), i, t)
		}
	}
	b.WriteString(" _va; })")
	*parts = append(head, b.String())
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (e *Emitter) forEach(v *ast.ForEach) {
	labels := e.takeLabels()
	e.line("{\n")
	e.indent++
	name := e.localName(v.Var.Sym)
	x := e.expr(v.X)
	if !v.Iterable {
		ix := e.tmpName()
		e.line("tyarr* %s = (tyarr*)%s;\n", ix, x)
		if v.Var.Sym != nil {
			e.line("int64_t %s = 0;\n", ix+"_i")
			e.line("for (%s = 0; %s < %s->len; %s++) {\n", ix+"_i", ix+"_i", ix, ix+"_i")
			e.indent++
			e.safepoint()
			raw := e.tmpName()
			if e.isRefElem(v.Elem) {
				e.line("%s %s = (%s)((void**)%s->data)[%s];\n", e.ctype(v.Elem), raw, e.ctype(v.Elem), ix, ix+"_i")
			} else {
				e.line("%s %s = ((%s*)%s->data)[%s];\n", e.ctype(v.Elem), raw, e.ctype(v.Elem), ix, ix+"_i")
			}
			e.line("%s %s = %s;\n", e.ctype(v.Var.Sym.Type), name, e.coerce(raw, v.Elem, v.Var.Sym.Type))
		}
		e.pushLoop("")
		e.stmtAsBlock(v.Body)
		e.popLoop()
		e.contLabels(labels)
		e.indent--
		e.line("}\n")
		e.indent--
		e.line("}\n")
		e.brkLabels(labels)
		return
	}
	// Iterable protocol
	itSel := e.selectorOf(e.prog.Builtins.Iterable, "iterator")
	hnSel := e.selectorOf(e.prog.Builtins.Iterator, "hasNext")
	nxSel := e.selectorOf(e.prog.Builtins.Iterator, "next")
	it := e.tmpName()
	e.line("void* %s = ((void*(*)(void*))ty_itab((tyobj*)%s, %d))((tyobj*)%s);\n", it, x, itSel, x)
	e.line("while (((int32_t(*)(void*))ty_itab((tyobj*)%s, %d))(%s)) {\n", it, hnSel, it)
	e.indent++
	e.safepoint()
	if v.Var.Sym != nil {
		// `for (int v : list)` unboxes the element the iterator hands back
		raw := e.tmpName()
		e.line("%s %s = (%s)((void*(*)(void*))ty_itab((tyobj*)%s, %d))(%s);\n",
			e.ctype(v.Elem), raw, e.ctype(v.Elem), it, nxSel, it)
		e.line("%s %s = %s;\n", e.ctype(v.Var.Sym.Type), name, e.coerce(raw, v.Elem, v.Var.Sym.Type))
	}
	e.pushLoop("")
	e.stmtAsBlock(v.Body)
	e.popLoop()
	e.contLabels(labels)
	e.indent--
	e.line("}\n")
	e.indent--
	e.line("}\n")
	e.brkLabels(labels)
}

// syncBlock emits a body that runs holding a monitor: the enter, the body, and
// the release on every way out. The lock expression is evaluated once, before
// the enter, so that a `synchronized (obj.field)` releases the object it locked
// rather than whatever the field holds by then.
//
// The release is a finally action -- the same machinery try/finally uses -- and
// that is the point: the shape this replaced released the monitor only on the
// path that ran off the end of the body. A `return` inside a synchronized block
// returned still holding the monitor, and an exception thrown inside one left it
// held for the rest of the program's life, because the throw longjmps past the
// release. With real (and now thread-visible) monitors, either one hangs every
// other thread that asks for the object.
//
// nullTest is Java's `synchronized (null)` check (JLS 14.19), which only a block
// needs: a synchronized method's monitor is `this` or the class, and neither is
// ever null.
func (e *Emitter) syncBlock(lock string, nullTest bool, body func()) {
	e.line("{ void* _lock = %s;\n", lock)
	if nullTest {
		e.line("if (!_lock) ty_npe();\n")
	}
	e.line("ty_sync_enter(_lock);\n")
	e.indent++
	frame := finFrame{depth: len(e.loops), name: e.tmpName()}
	frame.emit = func() { e.line("ty_sync_exit(_lock);\n") }
	e.line("{ tycatch %s; tyobj* %s_ex = NULL;\n", frame.name, frame.name)
	e.indent++
	/* The frame chain as of this point, which is where ty_throw rewinds it to
	   before it jumps: a longjmp drops every frame between the throw and this
	   setjmp, and the collector must not walk the maps of frames whose storage
	   the jump just freed. See tycatch in internal/runtime/src/tyrt.h. */
	e.line("%s.prev = ty_cur_catch; %s.ex = NULL; %s.frames = ty_frames; ty_cur_catch = &%s;\n",
		frame.name, frame.name, frame.name, frame.name)
	e.line("if (setjmp(%s.buf) == 0) {\n", frame.name)
	e.indent++
	e.finallys = append(e.finallys, frame)
	body()
	e.finallys = e.finallys[:len(e.finallys)-1]
	e.indent--
	e.line("} else { %s_ex = %s.ex; }\n", frame.name, frame.name)
	e.line("ty_cur_catch = %s.prev;\n", frame.name)
	frame.emit()
	e.line("if (%s_ex) ty_throw(%s_ex);\n", frame.name, frame.name)
	e.indent--
	e.line("}\n")
	e.indent--
	e.line("}\n")
}

// safepoint writes the statement a loop body begins with, so that a thread in a
// loop is always about to pass one: the collector's stop-the-world protocol
// (internal/runtime/src/tyrt_thread.c) is cooperative, and a loop that neither
// allocates nor calls anything is otherwise a thread no collection can stop.
// A loop is where a long-running thread spends its time, so a loop is where the
// check has to be, and it is one load and one predicted branch where no
// collection is running.
func (e *Emitter) safepoint() {
	e.line("ty_safepoint();\n")
}

func (e *Emitter) selectorOf(cl *ast.Class, name string) int {
	for _, m := range cl.Methods[name] {
		if m.Selector >= 0 {
			return m.Selector
		}
	}
	return -1
}

// ---------------------------------------------------------------- try

func (e *Emitter) tryStmt(v *ast.Try) {
	if len(v.Resources) > 0 {
		e.tryWithResources(v)
		return
	}
	e.emitTryCore(v, nil)
}

// tryWithResources opens the resources in the enclosing block and closes them
// in reverse order on every exit, including a return or a throw.
func (e *Emitter) tryWithResources(v *ast.Try) {
	e.line("{\n")
	e.indent++
	// a resource written as an expression is evaluated once, into a temporary
	// the close calls read back: re-evaluating it at every exit would run its
	// side effects again and could close a different object than it opened
	names := make([]string, len(v.Resources))
	for i, r := range v.Resources {
		switch t := r.(type) {
		case *ast.LocalVar:
			e.stmt(r)
			names[i] = e.localName(t.Vars[0].Sym)
		case *ast.ExprStmt:
			names[i] = e.tmpName()
			e.line("tyobj* %s = (tyobj*)%s;\n", names[i], e.expr(t.X))
		}
	}
	closeFn := func() {
		for i := len(v.Resources) - 1; i >= 0; i-- {
			name := names[i]
			if name != "" {
				e.line("if (%s) ((void(*)(void*))ty_itab((tyobj*)%s, %d))((tyobj*)%s);\n",
					name, name, e.selectorOf(e.prog.Builtins.AutoCloseable, "close"), name)
			}
		}
	}
	e.emitTryCore(v, closeFn)
	e.indent--
	e.line("}\n")
}

// emitTryCore lowers a try statement. closeFn, when set, is the implicit
// finally action of a try-with-resources: it runs before the explicit finally
// block (JLS 14.20.3).
func (e *Emitter) emitTryCore(v *ast.Try, closeFn func()) {
	frame := finFrame{depth: len(e.loops)}
	if v.Finally != nil || closeFn != nil {
		frame.emit = func() {
			if closeFn != nil {
				closeFn()
			}
			if v.Finally != nil {
				e.emitBlockInner(v.Finally)
			}
		}
	}
	if frame.emit != nil {
		frame.name = e.tmpName()
		e.line("{ tycatch %s; tyobj* %s_ex = NULL;\n", frame.name, frame.name)
		e.indent++
		/* The frame chain as of this point, which is where ty_throw rewinds it to
	   before it jumps: a longjmp drops every frame between the throw and this
	   setjmp, and the collector must not walk the maps of frames whose storage
	   the jump just freed. See tycatch in internal/runtime/src/tyrt.h. */
	e.line("%s.prev = ty_cur_catch; %s.ex = NULL; %s.frames = ty_frames; ty_cur_catch = &%s;\n",
		frame.name, frame.name, frame.name, frame.name)
		e.line("if (setjmp(%s.buf) == 0) {\n", frame.name)
		e.indent++
		e.finallys = append(e.finallys, frame)
	}
	if len(v.Catches) > 0 {
		c := e.tmpName()
		e.line("{ tycatch %s; %s.prev = ty_cur_catch; %s.ex = NULL; %s.frames = ty_frames; ty_cur_catch = &%s;\n",
			c, c, c, c, c)
		e.indent++
		e.line("if (setjmp(%s.buf) == 0) {\n", c)
		e.indent++
		// The catch frame is a handler the body can leave without unwinding at
		// all, so it is registered like a finally frame: the exits that do not
		// jump — a return, a break, a continue, a yield — have to take
		// ty_cur_catch off it before anything else can throw. The frame is
		// dropped again once the body is emitted: the catch bodies below run
		// with the chain already restored, exactly as the generated code does
		// at the top of the else branch.
		e.finallys = append(e.finallys, finFrame{depth: frame.depth, name: c})
		e.emitBlockInner(v.Body)
		e.finallys = e.finallys[:len(e.finallys)-1]
		e.indent--
		e.line("} else {\n")
		e.indent++
		e.line("tyobj* _ex = %s.ex;\n", c)
		e.line("ty_cur_catch = %s.prev;\n", c)
		first := true
		for _, cat := range v.Catches {
			cond := e.catchCond(cat)
			if first {
				e.line("if (%s) {\n", cond)
				first = false
			} else {
				e.line("else if (%s) {\n", cond)
			}
			e.indent++
			e.line("%s %s = (%s)(void*)_ex;\n", e.ctype(cat.Sym.Type), e.localName(cat.Sym), e.ctype(cat.Sym.Type))
			e.emitBlockInner(cat.Body)
			e.indent--
			e.line("}\n")
		}
		e.line("else { ty_cur_catch = %s.prev; ty_throw(_ex); }\n", c)
		e.indent--
		e.line("}\n")
		e.line("ty_cur_catch = %s.prev;\n", c)
		e.indent--
		e.line("}\n")
	} else {
		e.emitBlockInner(v.Body)
	}
	if frame.emit != nil {
		e.finallys = e.finallys[:len(e.finallys)-1]
		e.indent--
		e.line("} else { %s_ex = %s.ex; }\n", frame.name, frame.name)
		e.line("ty_cur_catch = %s.prev;\n", frame.name)
		frame.emit()
		e.line("if (%s_ex) ty_throw(%s_ex);\n", frame.name, frame.name)
		e.indent--
		e.line("}\n")
	}
}

func (e *Emitter) catchCond(cat *ast.Catch) string {
	var parts []string
	for _, te := range cat.Types {
		t := te.Resolved
		if ct, ok := t.(*ast.ClassType); ok {
			parts = append(parts, fmt.Sprintf("ty_instanceof(_ex, &cls_%s)", mangle(ct.Class.Full)))
		}
	}
	if len(parts) == 0 {
		return "1"
	}
	return strings.Join(parts, " || ")
}

// ---------------------------------------------------------------- switch

// caseEndPrefix starts the label every switch case body breaks out to. The loop
// stack carries those labels as well, so the prefix is what tells them apart
// from the entries of real loops, which are "" or a temporary such as _t12.
const caseEndPrefix = "_end"

// caseEndLabel is the C label that ends the switch with the given id.
func caseEndLabel(id int) string { return fmt.Sprintf("%s%d", caseEndPrefix, id) }

// isCaseEnd reports whether a loop stack entry is the end of a switch case body
// rather than a loop.
func isCaseEnd(t string) bool { return strings.HasPrefix(t, caseEndPrefix) }

// resultSlot declares the pointer a nested yield writes the value of a switch
// expression through. The yield knows the switch it belongs to but not the
// temporary its value ends up in, which the caller of switchStmt declares, so
// the value is reached through a pointer named after the switch id — the same
// id that names the label the yield jumps to.
func (e *Emitter) resultSlot(resultTmp string, id int) {
	if resultTmp == "" {
		return
	}
	e.line("__typeof__(%s)* _res%d = &%s;\n", resultTmp, id, resultTmp)
}

// hoistPatterns declares the variables bound by `instanceof` patterns that
// appear in a controlling expression, so the body and the update of a loop can
// refer to them. Java binds such a variable afresh on every evaluation of the
// condition rather than once when the loop is entered, so only the declaration
// is hoisted: the assignment is part of the condition, and instanceOf writes it
// there (see assignPattern in emit_expr.go).
func (e *Emitter) hoistPatterns(cond ast.Expr) {
	if cond == nil {
		return
	}
	e.patternVars = map[*ast.InstanceOf]string{}
	e.patternOK = map[*ast.InstanceOf]string{}
	var walk func(ast.Expr)
	walk = func(x ast.Expr) {
		switch v := x.(type) {
		case *ast.InstanceOf:
			if v.Binding != nil {
				name := e.declareExprPattern(v)
				if name != "" {
					e.patternVars[v] = name
				}
			}
			walk(v.X)
		case *ast.Binary:
			walk(v.X)
			walk(v.Y)
		case *ast.Unary:
			walk(v.X)
		case *ast.Cond:
			walk(v.C)
			walk(v.X)
			walk(v.Y)
		case *ast.Conv:
			walk(v.X)
		}
	}
	walk(cond)
}

// instTarget renders the class object an instanceof test compares against.
func (e *Emitter) instTarget(v *ast.InstanceOf) string {
	if ct, ok := v.Type.Resolved.(*ast.ClassType); ok {
		return "&cls_" + mangle(ct.Class.Full)
	}
	return "&cls_" + mangle(e.prog.ArrayClass().Full)
}

// declareExprPattern declares the variables an `instanceof` pattern in a
// controlling expression binds, with their zero values: the one that holds the
// whole match and, for a record pattern, one per component. It returns the
// variable holding the match, or "" when there is none.
//
// Only the declaration is hoisted; the values arrive with every evaluation of
// the condition, which instanceOf emits as a statement expression. Java gives
// the variable the scope of the loop body and binds it anew each time the
// condition is evaluated, so `xs[i] instanceof String s` must see the element
// of the current iteration, not of the one that entered the loop.
func (e *Emitter) declareExprPattern(v *ast.InstanceOf) string {
	pat := v.Binding
	if pat == nil {
		return ""
	}
	if prim, ok := e.prog.Erased(v.Type.Resolved).(*ast.PrimType); ok {
		// a primitive pattern is a question about the value, so the match is
		// recorded in a flag next to the value itself
		n := e.tmpName()
		okName := e.tmpName()
		e.line("%s %s = 0;\n", e.ctype(prim), n)
		e.line("int32_t %s = 0;\n", okName)
		e.patternOK[v] = okName
		if pat.Sym != nil && !pat.Unnamed {
			e.locals[pat.Sym] = n
		}
		return n
	}
	n := e.tmpName()
	e.line("%s %s = NULL;\n", e.ctype(v.Type.Resolved), n)
	if len(pat.Decomp) > 0 {
		// the components of a record pattern are part of the binding, so they
		// are declared here and read out of the match in the condition
		e.declareBinds(e.planComponents(pat, n))
		return n
	}
	if pat.Sym != nil && !pat.Unnamed {
		e.locals[pat.Sym] = n
	}
	return n
}

// compBind is one variable of a record pattern: the expression that reads its
// value out of the enclosing record, the test the component has to pass before
// the variables nested inside it may be read, and those nested binds.
type compBind struct {
	ct       string
	name     string
	accessor string
	// test guards the reads nested in this component, and is empty when the
	// enclosing record already settles what they need. A component read out of
	// the matched value itself needs no test; a component a nested pattern
	// destructures is only reachable when it holds a value, and one the pattern
	// names a narrower type for has to be of that type.
	test string
	subs []compBind
}

// planComponents walks a record pattern and returns the variables it binds,
// with the accessor and the test for each. Nested patterns get a temporary for
// the inner record, declared before the variables read out of it.
//
// The name of that temporary is derived from the receiver rather than taken
// from the counter, so that planning one pattern twice — once to declare its
// variables, once to assign them — yields the same names. A pattern in the
// condition of a loop is planned both ways.
func (e *Emitter) planComponents(p *ast.Param, recv string) []compBind {
	var walk func(p *ast.Param, recv string) []compBind
	walk = func(p *ast.Param, recv string) []compBind {
		var out []compBind
		for i, sub := range p.Decomp {
			if i >= len(p.Comps) {
				break
			}
			comp := p.Comps[i]
			accessor := fmt.Sprintf("(%s)->f_%s", recv, mangle(comp.Name))
			if len(sub.Decomp) > 0 {
				inner := fmt.Sprintf("%s_n%d", recv, i)
				out = append(out, compBind{
					ct:       e.ctype(comp.Type),
					name:     inner,
					accessor: accessor,
					test:     e.compTest(sub, comp, inner),
					subs:     walk(sub, inner),
				})
				continue
			}
			if sub.Sym == nil || sub.Unnamed {
				// a pattern that binds nothing asks nothing of the component:
				// the unnamed pattern matches every value, null included
				continue
			}
			name := e.localName(sub.Sym)
			out = append(out, compBind{
				ct:       e.ctype(comp.Type),
				name:     name,
				accessor: accessor,
				test:     e.compTest(sub, comp, name),
			})
		}
		return out
	}
	return walk(p, recv)
}

// nestedClass returns the class a component pattern names for its component, or
// nil when the pattern is total: a pattern written without a type (`var`, or a
// bare name) and the unnamed pattern ask nothing about the type of the value.
// The checker resolves the type of the pattern a case or a condition tests, but
// a component pattern keeps its type only as written, so the name is resolved
// here. A name that resolves to nothing leaves the pattern total rather than
// being guessed at, which reads the component as though the pattern had asked
// for nothing.
func (e *Emitter) nestedClass(sub *ast.Param, comp *ast.Field) *ast.Class {
	if sub == nil || sub.Unnamed || sub.Type == nil || sub.Type.Dims > 0 {
		return nil
	}
	if ct, ok := sub.Type.Resolved.(*ast.ClassType); ok {
		return ct.Class
	}
	if comp != nil && comp.Owner != nil {
		// a type parameter of the record shadows a class of the same name, and
		// the checker binds the component to that type rather than to the class
		for _, tv := range comp.Owner.TypeParams {
			if tv.Name == sub.Type.Name {
				return nil
			}
		}
	}
	return e.resolveClass(sub.Type.Name)
}

// resolveClass resolves a type name as written in a pattern. A class nested in
// another is registered under the class that encloses it and under no name of
// its own, so the walk goes segment by segment, the way the checker resolves a
// qualified name. A package name resolves to nothing here, and the pattern
// stays total.
func (e *Emitter) resolveClass(name string) *ast.Class {
	parts := strings.Split(name, ".")
	cl := e.prog.LookupClass(parts[0])
	for _, seg := range parts[1:] {
		if cl == nil {
			return nil
		}
		cl = cl.Nested[seg]
	}
	return cl
}

// compTest renders the test a component pattern places on its component before
// the variables nested inside that component may be read, or "" when there is
// none. The name is the variable the component is read into; the test names it,
// so an empty one only asks whether a test exists.
//
// A record pattern asks whether the component holds a value: matching a record
// means being an instance of it, so a null component fails the pattern. A type
// pattern that names a class narrower than the component's own type asks the
// same question of that class, and a null component fails it as well. A type
// pattern that names the component's own type is total — it reads the component
// as it is, null included, which is what javac does — and a pattern written
// without a type is total too, so both leave the component to the enclosing
// test.
func (e *Emitter) compTest(sub *ast.Param, comp *ast.Field, name string) string {
	cls := e.nestedClass(sub, comp)
	narrower := cls != nil && e.ctype(&ast.ClassType{Class: cls}) != e.ctype(comp.Type)
	switch {
	case len(sub.Decomp) > 0:
		if narrower {
			return fmt.Sprintf("%s != NULL && ty_instanceof((void*)%s, &cls_%s)", name, name, mangle(cls.Full))
		}
		return fmt.Sprintf("%s != NULL", name)
	case narrower:
		return fmt.Sprintf("ty_instanceof((void*)%s, &cls_%s)", name, mangle(cls.Full))
	}
	return ""
}

// patternTestsComponents reports whether a pattern asks about its components as
// well as the record itself, which is what makes its condition more than the
// type test on the value.
func (e *Emitter) patternTestsComponents(p *ast.Param) bool {
	for i, sub := range p.Decomp {
		if i >= len(p.Comps) {
			break
		}
		if e.compTest(sub, p.Comps[i], "") != "" || e.patternTestsComponents(sub) {
			return true
		}
	}
	return false
}

// declareBinds declares the variables of a record pattern with their zero
// values, each before the variables read out of it.
func (e *Emitter) declareBinds(binds []compBind) {
	for _, b := range binds {
		e.line("%s %s = %s;\n", b.ct, b.name, zeroOf(b.ct))
		e.declareBinds(b.subs)
	}
}

// emitComponentReads fills the variables of a record pattern, outermost first.
// okName names the int32_t variable holding the result of the pattern so far: a
// component a nested pattern rejects clears it, so the test that reads these
// variables reports that the pattern did not match. An empty okName is for a
// caller that has already made sure the pattern matched — the reads a failed
// test would have guarded are left out there. Either way a component nested in
// another is only ever read under the test that makes it safe: a value that is
// not of the tested type, or is not there at all, is never dereferenced.
func (e *Emitter) emitComponentReads(binds []compBind, okName string) {
	for _, b := range binds {
		e.line("%s = %s;\n", b.name, b.accessor)
		if b.test == "" {
			continue
		}
		if len(b.subs) == 0 {
			// nothing nested to read, so the test is the whole of it
			if okName != "" {
				e.line("if (!(%s)) { %s = 0; }\n", b.test, okName)
			}
			continue
		}
		e.line("if (%s) {\n", b.test)
		e.indent++
		e.emitComponentReads(b.subs, okName)
		e.indent--
		if okName == "" {
			e.line("}\n")
		} else {
			e.line("} else { %s = 0; }\n", okName)
		}
	}
}

// bindComponents declares the variables of a record pattern and reads the
// components into them. The receiver has already matched, so reading the
// components themselves is safe; a component a nested pattern rejects is left
// at its zero value instead of being read through.
func (e *Emitter) bindComponents(p *ast.Param, recv string) {
	binds := e.planComponents(p, recv)
	e.declareBinds(binds)
	e.emitComponentReads(binds, "")
}

// patternBind declares the variables of a case pattern with zero values, reads
// them inside the type test, and returns the condition that says the pattern
// matched. The caller must only use the variables under that condition: a value
// that is not of the tested type is never read, and neither is a component that
// a nested pattern rejects — that component is read only under a test of its
// own, which also clears the condition, so a nested pattern matches only when
// every component it destructures is there and of the type it names.
// switchSelectorValue renders the selector of the switch being emitted as the
// runtime sees it. A primitive selector has to be boxed like any other
// primitive operand of a primitive type pattern: handing ty_prim_match the
// number where it expects an object made it dereference the value as a class
// pointer (a switch on a long with `case int i` segfaulted).
func (e *Emitter) switchSelectorValue(id int) string {
	if p, ok := e.switchSel.(*ast.PrimType); ok && p.Kind != ast.Void {
		if call := e.boxedValue(p.Kind, fmt.Sprintf("_s%d", id)); call != "" {
			return call
		}
	}
	return fmt.Sprintf("_s%d", id)
}

// switchOperandBoxed is primOperandBoxed for the selector of the switch being
// emitted: a reference selector carries a box, a primitive one does not.
func (e *Emitter) switchOperandBoxed() int {
	return operandIsBox(e.prog, e.switchSel)
}

func (e *Emitter) patternBind(cs *ast.Case, id int) string {
	pat := cs.Pattern
	src := e.switchSelectorValue(id)
	if prim, isPrim := e.prog.Erased(pat.Type.Resolved).(*ast.PrimType); isPrim {
		val := e.tmpName()
		ok := e.tmpName()
		e.line("%s %s = 0;\n", e.ctype(prim), val)
		e.line("int32_t %s = ty_prim_match((void*)%s, %d, &%s, %d);\n", ok, src, prim.Kind, val,
			e.switchOperandBoxed())
		if pat.Sym != nil && !pat.Unnamed {
			e.locals[pat.Sym] = val
		}
		return fmt.Sprintf("(%s != 0)", ok)
	}
	pred := fmt.Sprintf("ty_instanceof((void*)_s%d, %s)", id, e.classOf(pat.Type.Resolved))
	ct := e.ctype(pat.Type.Resolved)
	recv := e.tmpName()
	e.line("%s %s = NULL;\n", ct, recv)
	var binds []compBind
	if len(pat.Decomp) > 0 {
		binds = e.planComponents(pat, recv)
	}
	varName := ""
	if len(pat.Decomp) == 0 && pat.Sym != nil && !pat.Unnamed {
		varName = e.localName(pat.Sym)
		e.line("%s %s = NULL;\n", ct, varName)
	}
	e.declareBinds(binds)
	if len(binds) == 0 {
		// nothing is read out of the match, so the type test is all of it
		e.line("if (%s) {\n", pred)
		e.indent++
		e.line("%s = (%s)%s;\n", recv, ct, src)
		if varName != "" {
			e.line("%s = %s;\n", varName, recv)
		}
		e.indent--
		e.line("}\n")
		return fmt.Sprintf("(%s != NULL)", recv)
	}
	ok := e.tmpName()
	e.line("int32_t %s = 0;\n", ok)
	e.line("if (%s) {\n", pred)
	e.indent++
	e.line("%s = (%s)%s;\n", recv, ct, src)
	// passing the type test is the whole of the match for a pattern with no
	// nested components, and every one that fails a test of its own clears the
	// flag again
	e.line("%s = 1;\n", ok)
	e.emitComponentReads(binds, ok)
	e.indent--
	e.line("}\n")
	return fmt.Sprintf("(%s != 0)", ok)
}

// clearPatterns drops the substitutions recorded for one controlling expression.
func (e *Emitter) clearPatterns() { e.patternVars = nil }

// switchNeedsChain reports whether the switch uses patterns, guards or a null
// case, which cannot be expressed as a plain C switch and are lowered as an
// if/else chain.
//
// `case null` belongs here with them: the checker routes a reference selector
// with a null case to a pattern switch, and nothing about a C case label can
// ask whether the selector is null. Lowered as a constant switch, the label of
// every constant case collapsed to the integer 0 — the comparison the chain
// makes is about the value, not about the pointer — so all of them landed on
// the null case at once (F2: "DDA" where javac prints "ADN"), and two of them
// emitted the same C case twice, which does not compile at all.
func switchNeedsChain(s *ast.Switch) bool {
	for _, cs := range s.Cases {
		if cs.Pattern != nil || cs.Guard != nil || cs.Null {
			return true
		}
	}
	return false
}

// emitSwitchNullCheck writes the null test a reference selector needs.
//
// A switch over a String, an enum, a box or any other reference throws
// NullPointerException when the selector is null and there is no `case null`
// to take it (JLS 14.11.2) -- a type pattern does not catch null either. The
// lowering read the null pointer instead: an enum answered with whatever
// ordinal lay at address zero and fell through to default, so an exhaustive
// switch expression printed null where javac throws.
func (e *Emitter) emitSwitchNullCheck(s *ast.Switch, id int) {
	switch s.Kind {
	case ast.SwitchEnum, ast.SwitchString:
		// the selector is a reference by construction
	default:
		if !e.isRef(s.X.GetType()) {
			return
		}
		for _, cs := range s.Cases {
			if cs.Null {
				return
			}
		}
	}
	e.line("if (!_s%d) ty_npe();\n", id)
}

// switchStmt lowers a switch statement or expression. Cases are emitted as
// labels so that colon-form cases keep Java's fall-through semantics; resultTmp
// is non-empty for switch expressions and receives the yielded value.
func (e *Emitter) switchStmt(s *ast.Switch, resultTmp string) {
	if len(s.Cases) == 0 {
		return
	}
	id := e.switchID
	e.switchID++
	// the selector's static type decides how a primitive type pattern in a case
	// is matched: a reference selector carries a box and JEP 507 then requires
	// the box to be exactly the pattern's type, while a primitive selector is
	// only asked for an exact conversion
	selType := e.switchSel
	e.switchSel = s.X.GetType()
	defer func() { e.switchSel = selType }()
	// a yield in a case body reaches its switch through the emitter, however
	// deeply the body nests it; see yieldStmt
	curType := e.switchCur
	if resultTmp != "" {
		e.switchCur = id
	} else {
		e.switchCur = -id - 1
	}
	defer func() { e.switchCur = curType }()
	if switchNeedsChain(s) {
		e.switchChain(s, resultTmp, id)
		return
	}
	e.line("{\n")
	e.indent++
	e.resultSlot(resultTmp, id)
	selT := e.ctype(s.X.GetType())
	switch s.Kind {
	case ast.SwitchString:
		e.line("tystr* _s%d = (tystr*)%s;\n", id, e.expr(s.X))
		e.emitSwitchNullCheck(s, id)
		e.line("int _k%d = -1;\n", id)
		for i, cs := range s.Cases {
			if isDefaultCase(cs) {
				continue
			}
			for _, l := range cs.Labels {
				e.line("if (_k%d < 0 && ty_str_eq(_s%d, %s)) _k%d = %d;\n", id, id, e.tmpRef(e.expr(l)), id, i)
			}
		}
		e.line("switch (_k%d) {\n", id)
	case ast.SwitchEnum:
		e.line("%s _s%d = %s;\n", selT, id, e.expr(s.X))
		e.emitSwitchNullCheck(s, id)
		e.line("int32_t _e%d = ty_enum_ordinal((void*)_s%d);\n", id, id)
		e.line("switch (_e%d) {\n", id)
	default:
		e.line("%s _s%d = %s;\n", selT, id, e.expr(s.X))
		e.line("switch ((int64_t)_s%d) {\n", id)
	}
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		if s.Kind == ast.SwitchString {
			// the selector holds the index of the matching case, because
			// strings cannot be compared in a C case label
			e.line("case %d: ", i)
		} else {
			for _, l := range cs.Labels {
				e.line("case %s: ", e.constInt(l))
			}
		}
		if len(cs.Labels) > 0 {
			e.line("goto _c%d_%d;\n", id, i)
		}
	}
	e.line("default: goto _cd%d;\n}\n", id)
	// A break anywhere in a case body leaves the switch, so the body is a level
	// of its own on the loop stack: without it the break would leave the loop
	// around the switch, and the finally actions of a try inside the case body
	// would not run. A nested loop pushes its own entry above, which restores
	// the loop meaning of a break inside it.
	e.pushLoop(caseEndLabel(id))
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		e.line("_c%d_%d: ;\n", id, i)
		e.indent++
		e.switchCaseBody(cs, resultTmp, id)
		e.indent--
	}
	e.line("_cd%d: ;\n", id)
	e.indent++
	for _, cs := range s.Cases {
		if isDefaultCase(cs) {
			e.switchCaseBody(cs, resultTmp, id)
		}
	}
	e.indent--
	e.popLoop()
	e.line("_end%d: ;\n", id)
	e.indent--
	e.line("}\n")
}

// isDefaultCase reports whether a case is the default branch.
func isDefaultCase(cs *ast.Case) bool {
	return cs.Default || (len(cs.Labels) == 0 && cs.Pattern == nil && !cs.Null)
}

// switchChain lowers a switch with type patterns or guards into an if/else
// chain that computes the case index, then reuses the labelled-case scheme.
func (e *Emitter) switchChain(s *ast.Switch, resultTmp string, id int) {
	e.line("{\n")
	e.indent++
	e.resultSlot(resultTmp, id)
	selT := e.ctype(s.X.GetType())
	e.line("%s _s%d = %s;\n", selT, id, e.expr(s.X))
	e.emitSwitchNullCheck(s, id)
	e.line("int _k%d = -1;\n", id)
	def := -1
	n := 0
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			def = i
			continue
		}
		kw := "if"
		if n > 0 {
			kw = "else if"
		}
		n++
		e.line("%s (%s) { _k%d = %d; }\n", kw, e.caseCond(s, cs, id), id, i)
	}
	if def >= 0 {
		if n > 0 {
			e.line("else { _k%d = %d; }\n", id, def)
		} else {
			e.line("_k%d = %d;\n", id, def)
		}
	} else if n > 0 && resultTmp != "" {
		// A switch expression with no matching case and no default has no value.
		e.line("if (_k%d < 0) { ty_throw((tyobj*)ty_illegal_state(%s)); }\n", id, e.cstr("no matching switch case"))
	}
	e.line("switch (_k%d) {\n", id)
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		e.line("case %d: goto _c%d_%d;\n", i, id, i)
	}
	e.line("default: goto _cd%d;\n}\n", id)
	// see switchStmt: the case bodies are a break level of their own
	e.pushLoop(caseEndLabel(id))
	for i, cs := range s.Cases {
		if isDefaultCase(cs) {
			continue
		}
		e.line("_c%d_%d: ;\n", id, i)
		e.indent++
		e.emitPatternBinding(cs, id)
		e.switchCaseBody(cs, resultTmp, id)
		e.indent--
	}
	e.line("_cd%d: ;\n", id)
	e.indent++
	for _, cs := range s.Cases {
		if isDefaultCase(cs) {
			e.emitPatternBinding(cs, id)
			e.switchCaseBody(cs, resultTmp, id)
		}
	}
	e.indent--
	e.popLoop()
	e.line("_end%d: ;\n", id)
	e.indent--
	e.line("}\n")
}

// emitPatternBinding declares the variables of a type or record pattern case.
func (e *Emitter) emitPatternBinding(cs *ast.Case, id int) {
	if cs.Pattern == nil {
		return
	}
	src := e.switchSelectorValue(id)
	if prim, isPrim := e.prog.Erased(cs.Pattern.Type.Resolved).(*ast.PrimType); isPrim {
		// the body only runs when the pattern matched, so the value is simply
		// read again
		if cs.Pattern.Sym == nil || cs.Pattern.Unnamed {
			return
		}
		n := e.localName(cs.Pattern.Sym)
		e.line("%s %s = 0;\n", e.ctype(prim), n)
		e.line("ty_prim_match((void*)%s, %d, &%s, %d);\n", src, prim.Kind, n,
			e.switchOperandBoxed())
		return
	}
	if len(cs.Pattern.Decomp) > 0 {
		n := e.tmpName()
		ct := e.ctype(cs.Pattern.Type.Resolved)
		e.line("%s %s = (%s)%s;\n", ct, n, ct, src)
		e.bindComponents(cs.Pattern, n)
		return
	}
	if cs.Pattern.Sym == nil || cs.Pattern.Unnamed {
		return
	}
	ct := e.ctype(cs.Pattern.Sym.Type)
	e.line("%s %s = (%s)%s;\n", ct, e.localName(cs.Pattern.Sym), ct, src)
}

// caseCond renders the condition that selects a case.
func (e *Emitter) caseCond(s *ast.Switch, cs *ast.Case, id int) string {
	var parts []string
	if cs.Null {
		parts = append(parts, fmt.Sprintf("(_s%d == NULL)", id))
	}
	for _, l := range cs.Labels {
		parts = append(parts, e.labelCond(s.X.GetType(), l, id))
	}
	primitive := false
	if cs.Pattern != nil {
		if _, isPrim := e.prog.Erased(cs.Pattern.Type.Resolved).(*ast.PrimType); isPrim {
			// a primitive pattern is answered by the value test in patternBind,
			// there is no class to compare against
			primitive = true
			parts = nil
		} else {
			parts = append(parts, fmt.Sprintf("ty_instanceof((void*)_s%d, %s)", id, e.classOf(cs.Pattern.Type.Resolved)))
		}
	}
	cond := "1"
	if len(parts) > 0 {
		cond = "(" + strings.Join(parts, " || ") + ")"
	}
	if primitive {
		var guard string
		var bound string
		inner := e.capture(func() {
			bound = e.patternBind(cs, id)
			if cs.Guard != nil {
				guard = e.expr(cs.Guard)
			}
		})
		if cs.Guard == nil {
			return fmt.Sprintf("({ %s %s; })", inner, bound)
		}
		return fmt.Sprintf("({ %s (%s) && (%s); })", inner, bound, guard)
	}
	if cs.Guard == nil && (cs.Pattern == nil || !e.patternTestsComponents(cs.Pattern)) {
		return cond
	}
	if cs.Pattern != nil {
		// The guard reads the pattern variables, so they have to exist before
		// it runs. They are read inside the type test and the whole thing
		// becomes the condition, which keeps a value of the wrong type from
		// ever being dereferenced. A nested pattern is bound the same way and
		// for a second reason: what it reads is only there under the tests of
		// its own components, and those tests have to be part of the condition
		// that picks the case, or the body would run for a value the pattern
		// rejects.
		var guard string
		var bound string
		inner := e.capture(func() {
			bound = e.patternBind(cs, id)
			if cs.Guard != nil {
				guard = e.expr(cs.Guard)
			}
		})
		if cs.Guard == nil {
			return fmt.Sprintf("({ %s %s; })", inner, bound)
		}
		return fmt.Sprintf("({ %s (%s) && (%s); })", inner, bound, guard)
	}
	return "(" + cond + " && " + e.expr(cs.Guard) + ")"
}

// labelCond renders the test of one constant case label against the selector.
//
// The comparison follows the selector's static type rather than the switch kind
// the checker resolved. A `case null` turns a switch on a String or an enum
// into a pattern switch, and its labels are still String constants or enum
// constants: compared as plain integers they all came out as 0, so every one of
// them matched the null case as well, and a switch that also held a type
// pattern compared a pointer against an ordinal and never matched its constants
// at all.
func (e *Emitter) labelCond(sel ast.Type, l ast.Expr, id int) string {
	switch {
	case e.isStringSelector(sel):
		return fmt.Sprintf("ty_str_eq((tystr*)_s%d, (tystr*)%s)", id, e.expr(l))
	case e.isEnumSelector(sel):
		// an enum constant is only reachable through its ordinal, the same
		// value the constant dispatch switches on
		return fmt.Sprintf("(ty_enum_ordinal((void*)_s%d) == %s)", id, e.constInt(l))
	}
	if c, ok := e.boxedLabelCond(sel, l, id); ok {
		return c
	}
	return fmt.Sprintf("((int64_t)_s%d == %s)", id, e.constInt(l))
}

// boxedLabelCond renders the test of one constant label against a selector that
// holds a box, and reports whether it applies.
//
// A `case null` is also what lets a switch be written on a box at all: the
// checker rejects a box as a switch selector unless a null case makes it a
// pattern switch, and the chain that lowers such a switch holds the box, not
// the value in it. Comparing the box as an integer never matched, so every
// constant label of such a switch silently answered with the default; Java
// compares the value the box carries.
//
// A null selector carries no value to compare and belongs to the null case, as
// Java says, so the test asks for the box before it opens it: an unboxing call
// on null is a NullPointerException, and a case label must not throw it.
func (e *Emitter) boxedLabelCond(sel ast.Type, l ast.Expr, id int) (string, bool) {
	ct, ok := e.prog.Erased(sel).(*ast.ClassType)
	if !ok || ct.Class == nil {
		return "", false
	}
	kind, ok := e.prog.Builtins.Unbox[ct.Class]
	if !ok {
		return "", false
	}
	m := e.unboxAccessor(ct.Class, kind)
	if m == nil {
		return "", false
	}
	return fmt.Sprintf("(_s%d != NULL && %s((%s*)_s%d) == %s)", id, e.cfunc(m), cname(ct.Class), id, e.constInt(l)), true
}

// isStringSelector reports whether a selector has the String type, the only
// reference type whose case labels are compared by value.
func (e *Emitter) isStringSelector(t ast.Type) bool {
	ct, ok := e.prog.Erased(t).(*ast.ClassType)
	return ok && ct.Class == e.prog.Builtins.String
}

// isEnumSelector reports whether a selector is an enum, whose case labels name
// its constants.
func (e *Emitter) isEnumSelector(t ast.Type) bool {
	ct, ok := e.prog.Erased(t).(*ast.ClassType)
	return ok && ct.Class != nil && ct.Class.Kind == ast.KindEnum
}

// classOf renders the class descriptor of a resolved type.
func (e *Emitter) classOf(t ast.Type) string {
	if ct, ok := t.(*ast.ClassType); ok {
		return "&cls_" + mangle(ct.Class.Full)
	}
	return "&cls_" + mangle(e.prog.ArrayClass().Full)
}

// constInt renders a case label as a C integer constant.
func (e *Emitter) constInt(l ast.Expr) string {
	if lit, ok := l.(*ast.Literal); ok {
		switch lit.Kind {
		case ast.LitChar, ast.LitInt, ast.LitLong:
			return fmt.Sprint(int64(lit.Int))
		}
	}
	if cv := e.prog.ConstInt(l); cv != nil {
		return fmt.Sprint(*cv)
	}
	return "0"
}

// switchCaseBody emits one case body; Java colon cases fall through.
//
// A yield in the body is left to e.stmt: it is the switch being emitted, not
// the shape of the body, that gives a yield its value and its end label, so
// every statement of the body — whatever depth a yield hides at — is emitted
// the same way. Looking for yields in the body's own statements found only the
// ones written directly in it.
func (e *Emitter) switchCaseBody(cs *ast.Case, resultTmp string, id int) {
	if cs.ArrowX != nil {
		if resultTmp != "" {
			e.line("%s = %s;\n", resultTmp, e.expr(cs.ArrowX))
		} else {
			e.line("(void)(%s);\n", e.expr(cs.ArrowX))
		}
		e.line("goto _end%d;\n", id)
		return
	}
	for _, st := range cs.Body {
		e.stmt(st)
	}
	// `case N -> { ... }` is a whole body that stops there, like the expression
	// form above, and `case N -> throw ...` is one too -- a throw inside a try
	// in the block can be caught, and Java still does not continue into the
	// next case. Both are a Block or a statement in Body, which is what a colon
	// case holds as well, so the case has to say which form it came from.
	if cs.Arrow {
		e.line("goto _end%d;\n", id)
	}
}
