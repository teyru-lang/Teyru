package sema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

// methodCtx carries the local scope of one method body.
type methodCtx struct {
	c      *Checker
	cl     *ast.Class
	m      *ast.Method
	env    *typeEnv
	scopes []map[string]*ast.Var
	loops  int
	sw     *ast.Switch
	lambda *ast.Lambda
	// envs is the type-environment stack that mirrors scopes: push swaps in a
	// block's own environment and pop puts the enclosing one back.
	envs []*typeEnv
	// noThis marks a context that inherits its staticness from the method a
	// lambda was written in: the lambda's own method is never static, so the
	// answer cannot be read off m alone.
	noThis          bool
	staticImports   []*ast.Field
	staticMethods   map[string][]*ast.Method
	props           map[ast.Expr]ast.Expr
	pendingTypeArgs []ast.Type
	// assignTarget is the expression an assignment is writing to, so that a
	// property resolved on it is judged by its setter: reading the target to
	// find the symbol is not a read of the property.
	assignTarget ast.Expr
	// helpers holds the @Helper local classes in scope, innermost last. An
	// unqualified call to one of their methods goes through the instance
	// Lombok declares for the class, which is what @Helper is for.
	helpers [][]helperRef
}

// helperRef is one @Helper local class in scope and the instance it is used
// through.
type helperRef struct {
	cl   *ast.Class
	inst *ast.Var
}

func (c *Checker) checkBodies(cl *ast.Class) {
	if cl.Decl == nil {
		return
	}
	for _, mem := range cl.Decl.Members {
		switch d := mem.(type) {
		case *ast.MethodDecl:
			if d.Sym == nil || d.Body == nil {
				continue
			}
			// a compact constructor already carries the record components as
			// its parameters, so its body binds those names directly
			ctx := c.newCtx(cl, d.Sym)
			ctx.checkBlock(d.Body, false)
			if d.Sym.Result != ast.TVoid && !d.Sym.IsCtor && canCompleteNormally(d.Body) {
				ctx.errf(d.Body.End, "TY-TYP-0020", "missing return statement")
			}
		case *ast.FieldDecl:
			for _, vd := range d.Vars {
				if vd.Init == nil {
					continue
				}
				ctx := c.newCtx(cl, nil)
				var want ast.Type
				if vd.Fld != nil {
					want = vd.Fld.Type
				}
				ctx.checkExpr(vd.Init, want)
				if want != nil {
					ctx.convertTo(vd.Init, want)
				}
				if f := vd.Fld; f != nil && !f.IsProp && f.Mods.Has(ast.ModFinal) {
					f.ConstVal = c.constFieldInit(isStaticField(f), vd.Init)
				}
			}
			for _, acc := range d.Accessor {
				if acc.Body == nil || acc.Sym == nil {
					continue
				}
				ctx := c.newCtx(cl, acc.Sym)
				if f := acc.Sym.Prop; f != nil && f.Storage {
					fv := ctx.declare("field", f.Type, acc.Pos)
					fv.Field = f
				}
				ctx.checkBlock(acc.Body, false)
			}
		case *ast.InitBlock:
			ctx := c.newCtx(cl, nil)
			ctx.checkBlock(d.Body, false)
		}
	}
	// compiler-synthesized bodies (annotation processing, records, enums)
	for _, m := range sortedSynthMethods(cl) {
		if m.Body == nil || m.Checked {
			continue
		}
		m.Checked = true
		ctx := c.newCtx(cl, m)
		ctx.checkBlock(m.Body, false)
		if m.Result != ast.TVoid && !m.IsCtor && canCompleteNormally(m.Body) {
			ctx.errf(m.Body.End, "TY-TYP-0020", "missing return statement")
		}
	}
	c.checkAbstracts(cl)
}

// constFieldInit records the compile-time value of a constant variable: only a
// final field with a constant initializer may be inlined at use sites (JLS 4.12.4).
func (c *Checker) constFieldInit(isStatic bool, e ast.Expr) any {
	if !isStatic {
		return nil
	}
	cv := c.constEval(e)
	if cv.ok {
		return cv
	}
	return nil
}

func isStaticField(f *ast.Field) bool { return f.Mods.Has(ast.ModStatic) }

// canCompleteNormally reports whether a statement can finish without leaving
// the method it belongs to: "can complete normally" as JLS 14.22 defines it,
// which is what decides whether a value-returning method that ends in the
// statement needs a return after it (JLS 8.4.7).
//
// The rules are javac's, and each was checked against it:
//
//   - a block finishes where its last statement does;
//   - an if with no else finishes, and one with an else finishes when either
//     branch does;
//   - a loop whose condition is the constant true finishes only through a
//     break (`while (true)`, `for (;;)` and `do ... while (true)` alike), and
//     an enhanced for may not run at all, so it always finishes;
//   - a try finishes when the body or a catch finishes, and not at all when
//     its finally cannot;
//   - a switch finishes per switchCanCompleteNormally below;
//   - a declaration, an expression statement, an assertion, a yield and the
//     empty statement finish, and a return, throw, break or continue does not.
func canCompleteNormally(s ast.Stmt) bool {
	switch v := s.(type) {
	case nil:
		return true
	case *ast.Return, *ast.Throw, *ast.Break, *ast.Continue:
		return false
	case *ast.Block:
		return len(v.Stmts) == 0 || canCompleteNormally(v.Stmts[len(v.Stmts)-1])
	case *ast.If:
		return v.Else == nil || canCompleteNormally(v.Then) || canCompleteNormally(v.Else)
	case *ast.While:
		return !constTrue(v.Cond) || hasBreak(v.Body)
	case *ast.DoWhile:
		return !constTrue(v.Cond) || hasBreak(v.Body)
	case *ast.For:
		return (v.Cond != nil && !constTrue(v.Cond)) || hasBreak(v.Body)
	case *ast.Try:
		if v.Finally != nil && !canCompleteNormally(v.Finally) {
			return false
		}
		if canCompleteNormally(v.Body) {
			return true
		}
		for _, cat := range v.Catches {
			if canCompleteNormally(cat.Body) {
				return true
			}
		}
		return false
	case *ast.Sync:
		return canCompleteNormally(v.Body)
	case *ast.Labeled:
		// A break inside the labeled statement leaves it, so it finishes even
		// when what it labels cannot (`L: while (true) { break L; }`).
		return canCompleteNormally(v.Body) || hasBreak(v.Body)
	case *ast.Switch:
		return switchCanCompleteNormally(v)
	}
	return true
}

// switchCanCompleteNormally is the switch rule of JLS 14.22, in the shape javac
// implements it, which is not quite the same shape for the two forms of switch:
//
//   - the block has to be total -- a default label, or a pattern switch that
//     covers every value of its selector (JLS 14.11.2). An enum switch with
//     every constant and no default is total to a reader and not to javac, so
//     it is not total here either;
//   - no break may leave the switch, and
//   - in the arrow form every arm has to leave the method (`case 1 -> { return
//     1; } default -> throw ...` ends the method; `case 1 -> { } default ->
//     throw ...` does not), while in the colon form the last statement group
//     decides, and a label with no statements after it falls out of the switch.
//
// A pattern switch whose coverage is not decidable here is the one case left to
// the reader: it is answered "cannot complete normally", the permissive reading,
// because a sealed type without a permits clause is undecidable for this
// checker (AGENTS.md §10) and rejecting a switch that does cover every value
// would be the worse error.
func switchCanCompleteNormally(s *ast.Switch) bool {
	total := false
	for _, cs := range s.Cases {
		if cs.Default {
			total = true
		}
	}
	if !total && (s.Kind != ast.SwitchType || !s.Exhaustive) {
		return true
	}
	if switchBreak(s) {
		return true
	}
	if s.Arrow {
		for _, cs := range s.Cases {
			if armCanCompleteNormally(cs) {
				return true
			}
		}
		return false
	}
	for i := len(s.Cases) - 1; i >= 0; i-- {
		body := s.Cases[i].Body
		if len(body) == 0 {
			return true
		}
		return canCompleteNormally(body[len(body)-1])
	}
	return true
}

// armCanCompleteNormally reports whether one arm of an arrow switch finishes
// without leaving the method. An arm written as an expression does finish: its
// value is the statement's and the switch moves on.
func armCanCompleteNormally(cs *ast.Case) bool {
	if cs.ArrowX != nil || len(cs.Body) == 0 {
		return true
	}
	return canCompleteNormally(cs.Body[len(cs.Body)-1])
}

// switchBreak reports whether a break inside the switch leaves the switch. A
// break belonging to a nested loop or switch is not this switch's, and the
// search stops descending at one.
func switchBreak(s *ast.Switch) bool {
	for _, cs := range s.Cases {
		for _, st := range cs.Body {
			if hasBreak(st) {
				return true
			}
		}
	}
	return false
}

// constTrue reports whether a condition is the constant true, which is what
// makes a loop's body the whole of its reachable control flow.
func constTrue(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	return ok && lit.Kind == ast.LitBool && lit.Bool
}

// hasBreak reports whether a statement can leave its loop through a break. A
// break inside a nested loop or switch belongs to that statement, so the search
// stops descending there; a labeled break may target a loop further out, and
// counting it is the conservative answer.
func hasBreak(s ast.Stmt) bool {
	switch v := s.(type) {
	case nil:
		return false
	case *ast.Break:
		return true
	case *ast.Block:
		for _, st := range v.Stmts {
			if hasBreak(st) {
				return true
			}
		}
	case *ast.If:
		return hasBreak(v.Then) || hasBreak(v.Else)
	case *ast.Labeled:
		return hasBreak(v.Body)
	case *ast.Sync:
		return hasBreak(v.Body)
	case *ast.Try:
		if hasBreak(v.Body) {
			return true
		}
		for _, cat := range v.Catches {
			if hasBreak(cat.Body) {
				return true
			}
		}
		if v.Finally != nil {
			return hasBreak(v.Finally)
		}
	}
	return false
}

func (c *Checker) newCtx(cl *ast.Class, m *ast.Method) *methodCtx {
	ctx := &methodCtx{c: c, cl: cl, m: m, env: c.classEnv(cl)}
	if m != nil && len(m.TypeParams) > 0 {
		// the body of a generic method sees its own type parameters
		env := &typeEnv{cls: cl, tvars: map[string]*ast.TypeVar{}, parent: ctx.env, file: cl.File}
		for _, tv := range m.TypeParams {
			env.tvars[tv.Name] = tv
		}
		ctx.env = env
	}
	if cl.File != nil {
		ctx.staticImports = cl.File.StaticImports
		ctx.staticMethods = cl.File.StaticMethods
	}
	ctx.props = c.Props
	ctx.push()
	if m != nil {
		for i, p := range m.ParamNames {
			v := ctx.declare(p, m.Params[i], m.Pos)
			m.ParamVars = append(m.ParamVars, v)
		}
	}
	return ctx
}

func (ctx *methodCtx) push() {
	ctx.scopes = append(ctx.scopes, map[string]*ast.Var{})
	ctx.helpers = append(ctx.helpers, nil)
	ctx.envs = append(ctx.envs, ctx.env)
	if ctx.env != nil {
		// a block gets its own type environment so that a local class declared
		// in it is visible for the rest of the block and nowhere else (JLS 6.3)
		ctx.env = &typeEnv{cls: ctx.env.cls, tvars: ctx.env.tvars, file: ctx.env.file,
			locals: map[string]*ast.Class{}, parent: ctx.env}
	}
}
func (ctx *methodCtx) pop() {
	ctx.scopes = ctx.scopes[:len(ctx.scopes)-1]
	if n := len(ctx.helpers); n > 0 {
		ctx.helpers = ctx.helpers[:n-1]
	}
	if n := len(ctx.envs); n > 0 {
		ctx.env = ctx.envs[n-1]
		ctx.envs = ctx.envs[:n-1]
	}
}

// declareLocalClass binds a local class name in the block that declares it.
func (ctx *methodCtx) declareLocalClass(name string, cl *ast.Class) {
	if ctx.env == nil {
		return
	}
	if ctx.env.locals == nil {
		ctx.env.locals = map[string]*ast.Class{}
	}
	ctx.env.locals[name] = cl
}

// visibleClasses is every local class name in scope at this point, for the
// body of a local class declared here to resolve against.
func (ctx *methodCtx) visibleClasses() map[string]*ast.Class {
	var out map[string]*ast.Class
	for e := ctx.env; e != nil; e = e.parent {
		for n, cl := range e.locals {
			if out == nil {
				out = map[string]*ast.Class{}
			}
			// the innermost binding of a name wins
			if _, seen := out[n]; !seen {
				out[n] = cl
			}
		}
	}
	return out
}

func (ctx *methodCtx) errf(pos source.Pos, code, format string, args ...any) {
	ctx.c.errf(pos, code, format, args...)
}

// declareUnnamed creates a binding for `_`, which is never referenced.
func (ctx *methodCtx) declareUnnamed(pos source.Pos) *ast.Var {
	return ctx.declare("_$"+fmt.Sprint(ctx.c.varID), ast.ErrorType{}, pos)
}

// declare adds a local variable to the innermost scope.
func (ctx *methodCtx) declare(name string, t ast.Type, pos source.Pos) *ast.Var {
	if name == "_" {
		// `_` is the unnamed variable (JEP 456): it never collides and is
		// never referenced, so give it a unique internal name.
		name = "_$" + fmt.Sprint(ctx.c.varID)
	}
	cur := ctx.scopes[len(ctx.scopes)-1]
	if prev := cur[name]; prev != nil {
		ctx.errf(pos, "TY-TYP-0021", "duplicate local variable %s", name)
	}
	id := ctx.c.varID
	ctx.c.varID++
	v := &ast.Var{Name: name, Type: t, Pos: pos, ID: id, Owner: ctx.m}
	if ctx.m != nil {
		ctx.m.Locals = append(ctx.m.Locals, v)
	}
	cur[name] = v
	return v
}

func (ctx *methodCtx) lookupLocal(name string) *ast.Var {
	for i := len(ctx.scopes) - 1; i >= 0; i-- {
		if v := ctx.scopes[i][name]; v != nil {
			return v
		}
	}
	return nil
}

// lookupField searches the class chain for a field, then static imports.
func (ctx *methodCtx) lookupField(name string) *ast.Field {
	for cl := ctx.cl; cl != nil; cl = cl.Outer {
		for k := cl; k != nil; {
			if f := k.FieldMap[name]; f != nil {
				return f
			}
			if !k.Resolved || k.Super == nil {
				break
			}
			k = k.Super.Class
		}
		if cl.Outer == nil {
			break
		}
	}
	return nil
}

func isStaticCtx(cl *ast.Class) bool {
	return cl.Decl != nil && cl.Decl.Implicit
}

func (ctx *methodCtx) inStatic() bool {
	return ctx.m == nil || ctx.m.IsStatic() || ctx.noThis
}

// hasThis reports whether `this` denotes an instance at the point being
// checked. A lambda body inherits the answer from where the lambda was
// written: JLS 15.27.2 gives `this` the same meaning inside the body as
// outside it, so a lambda in a static method has no instance to name, while a
// lambda in an instance method, constructor or field initializer does.
func (ctx *methodCtx) hasThis() bool {
	if ctx.noThis {
		return false
	}
	// a compact source file's implicit class has no instance at all
	if ctx.cl != nil && isStaticCtx(ctx.cl) {
		return false
	}
	if ctx.m != nil {
		return !ctx.m.IsStatic()
	}
	// no method in scope: a field initializer runs on the instance under
	// construction
	return true
}

// noteThis records that the lambda body being checked uses `this`, which is
// the instance the lambda was created in. Every lambda between the use and
// that instance has to carry the reference, so each one captures it in turn:
// the innermost needs the enclosing instance, the one around it needs it for
// the innermost, and so on.
func (ctx *methodCtx) noteThis() {
	for l := ctx.lambda; l != nil; l = l.Outer {
		l.CapThis = true
	}
}

// thisUse is the check every use of the enclosing instance inside a lambda has
// to pass: either there is an instance to capture, or the use is an error. In
// a static method there is no `this`, so Java rejects the use instead of
// silently binding it to the closure object.
func (ctx *methodCtx) thisUse(pos source.Pos, what string) bool {
	if ctx.lambda == nil {
		return true
	}
	if !ctx.hasThis() {
		ctx.errf(pos, "TY-TYP-0098", "non-static %s cannot be referenced from a static context", what)
		return false
	}
	ctx.noteThis()
	return true
}

// expectType gives the declared type of a field declarator.
// ---------------------------------------------------------------- statements

func (ctx *methodCtx) checkBlock(b *ast.Block, scoped bool) {
	if scoped {
		ctx.push()
		defer ctx.pop()
	}
	for i := 0; i < len(b.Stmts); i++ {
		// @Cleanup turns the rest of the block into a try-with-resources so the
		// resource is closed on every exit path.
		if lv, ok := b.Stmts[i].(*ast.LocalVar); ok && hasAnno(lv.Annos, "Cleanup") != nil {
			rest := append([]ast.Stmt(nil), b.Stmts[i+1:]...)
			t := &ast.Try{Pos: lv.Pos, Resources: []ast.Stmt{lv}, Body: &ast.Block{Stmts: rest}}
			// the transformation is reflected in the tree so code generation
			// emits the try-with-resources
			b.Stmts = append(b.Stmts[:i], t)
			ctx.checkTry(t)
			return
		}
		ctx.checkStmt(b.Stmts[i])
	}
}

// hasEffect reports whether an expression may stand alone as a statement,
// following JLS 14.8: an assignment, an increment or decrement, a method call,
// or an object creation. A parenthesised call is still a call, which is the one
// nesting Java allows.
func hasEffect(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Call, *ast.New, *ast.NewArray, *ast.Assign:
		return true
	case *ast.Unary:
		// an increment or a decrement is a statement; the other prefix and
		// postfix operators are not
		return strings.Contains(v.Op, "++") || strings.Contains(v.Op, "--")
	}
	return false
}

// describeExpr names an expression the way a reader wrote it, for the message
// that says it does nothing.
func describeExpr(e ast.Expr) string {
	switch e.(type) {
	case *ast.Unary, *ast.Binary:
		return "a value"
	case *ast.Select:
		return "a field read"
	case *ast.Literal:
		return "a literal"
	}
	return "reading a variable"
}

func (ctx *methodCtx) checkStmt(s ast.Stmt) {
	c := ctx.c
	switch v := s.(type) {
	case *ast.Block:
		ctx.checkBlock(v, true)
	case *ast.Empty:
	case *ast.LocalVar:
		ctx.checkLocalVar(v)
	case *ast.LocalClass:
		cd := v.Decl
		if cd.Sym == nil {
			cl := c.declareClass(ctx.cl.File, cd, ctx.cl)
			cl.LocalOwner = ctx.m
			// The body sees the variables that are in scope where the class is
			// declared (JLS 6.3). Without this the scopes are empty, so a
			// reference to a local of the enclosing method found nothing and
			// was reported as a missing symbol -- an anonymous class got them
			// and a local class did not.
			cl.LocalScopes = ctx.scopes
			// A local class declared in a static context has no enclosing
			// instance to point at (JLS 8.1.3): `class Local {...}` inside a
			// static method is instantiated with `new Local()`, exactly like a
			// static nested class. declareClass marks every class without a
			// `static` modifier as inner, which is right for a nested type and
			// wrong here, so the local case corrects it before the outer field
			// is laid out.
			cl.Inner = !ctx.inStatic()
			// A local class belongs to the block that declares it (JLS 6.3),
			// so two methods may each declare `class Local` and they are
			// distinct types. declareClass put it in the enclosing type's
			// scope, which is a single namespace and the wrong one; move it to
			// this block, and give it a name of its own so the two do not
			// collide as C symbols either.
			if ctx.cl.Nested[cd.Name] == cl {
				delete(ctx.cl.Nested, cd.Name)
			}
			c.localN++
			cl.Full = fmt.Sprintf("%s$%s$%d", ctx.cl.Full, cd.Name, c.localN)
			cl.LocalClasses = ctx.visibleClasses()
			ctx.declareLocalClass(cd.Name, cl)
			c.resolveHeader(cl)
			c.resolveMembers(cl)
			c.layout(cl)
			c.checkBodies(cl)
			// @Helper: Lombok declares the instance right here and lets the
			// statements below call the class's methods unqualified
			if hasAnno(cd.Annos, "Helper") != nil {
				inst := ctx.declare("$"+cd.Name, &ast.ClassType{Class: cl}, cd.Pos)
				init := newObj(cl)
				ctx.checkExpr(init, nil)
				v.Instance, v.InstanceInit = inst, init
				if n := len(ctx.helpers); n > 0 {
					ctx.helpers[n-1] = append(ctx.helpers[n-1], helperRef{cl: cl, inst: inst})
				}
			}
		}
	case *ast.ExprStmt:
		ctx.checkExpr(v.X, nil)
		// Java allows only the forms whose evaluation does something: an
		// assignment, an increment or decrement, a method call, an object
		// creation (JLS 14.8). Anything else is a statement with no effect,
		// and rejecting it is what turns a real trap into a diagnostic -- this
		// language ends an expression at the newline, so
		//     long x = 100L + a
		//              + b
		// is two statements, and the second one used to be a silent unary plus
		// that quietly left x one term short.
		if !hasEffect(v.X) {
			ctx.errf(v.X.GetPos(), "TY-TYP-0114",
				"not a statement: %s has no effect", describeExpr(v.X))
		}
	case *ast.If:
		ctx.checkCond(v.Cond)
		ctx.checkStmt(v.Then)
		if v.Else != nil {
			ctx.checkStmt(v.Else)
		}
	case *ast.While:
		ctx.checkCond(v.Cond)
		ctx.loops++
		ctx.checkStmt(v.Body)
		ctx.loops--
	case *ast.DoWhile:
		ctx.loops++
		ctx.checkStmt(v.Body)
		ctx.loops--
		ctx.checkCond(v.Cond)
	case *ast.For:
		ctx.push()
		for _, s := range v.Init {
			ctx.checkStmt(s)
		}
		if v.Cond != nil {
			ctx.checkCond(v.Cond)
		}
		for _, u := range v.Update {
			ctx.checkExpr(u, nil)
		}
		ctx.loops++
		ctx.checkStmt(v.Body)
		ctx.loops--
		ctx.pop()
	case *ast.ForEach:
		ctx.checkForEach(v)
	case *ast.Return:
		ctx.checkReturn(v)
	case *ast.Break:
		if ctx.loops == 0 && ctx.sw == nil || v.Label != "" {
			if v.Label == "" {
				ctx.errf(v.Pos, "TY-TYP-0022", "break outside of loop or switch")
			}
		}
	case *ast.Continue:
		if ctx.loops == 0 {
			ctx.errf(v.Pos, "TY-TYP-0023", "continue outside of loop")
		}
	case *ast.Throw:
		ctx.checkExpr(v.X, nil)
		if t := v.X.GetType(); t != nil && !c.isSubtype(t, &ast.ClassType{Class: c.b.Throwable}) && !ast.IsError(t) {
			if _, isNull := t.(ast.NullType); !isNull {
				ctx.errf(v.Pos, "TY-TYP-0024", "thrown value must be a Throwable, found %s", t)
			}
		}
	case *ast.Try:
		ctx.checkTry(v)
	case *ast.Switch:
		ctx.checkSwitch(v, false)
	case *ast.Yield:
		ctx.checkExpr(v.X, nil)
	case *ast.Labeled:
		ctx.checkStmt(v.Body)
	case *ast.Assert:
		ctx.checkCond(v.Cond)
		if v.Msg != nil {
			ctx.checkExpr(v.Msg, nil)
		}
	case *ast.Sync:
		ctx.checkExpr(v.Lock, nil)
		if lt := v.Lock.GetType(); lt != nil && ast.IsPrim(lt, ast.Void) {
			ctx.errf(v.Pos, "TY-TYP-0025", "cannot synchronize on void")
		}
		ctx.checkBlock(v.Body, true)
	}
}

func (ctx *methodCtx) checkLocalVar(v *ast.LocalVar) {
	c := ctx.c
	explicit := v.Type.Name != "var" && v.Type.Name != "val"
	var base ast.Type
	if explicit {
		base = c.resolveType(ctx.env, v.Type)
	}
	if !explicit && v.Mods.Has(ast.ModFinal) {
		ctx.errf(v.Pos, "TY-TYP-0026", "local variables cannot be declared final; use 'val'")
	}
	for i, vd := range v.Vars {
		t := base
		for k := 0; k < vd.Dims; k++ {
			t = &ast.ArrayType{Elem: t}
		}
		if !explicit {
			if vd.Init == nil {
				ctx.errf(vd.Pos, "TY-TYP-0027", "'%s' requires an initializer", v.Type.Name)
				vd.Sym = ctx.declare(vd.Name, ast.ErrorType{}, vd.Pos)
				continue
			}
			ctx.checkExpr(vd.Init, nil)
			it := vd.Init.GetType()
			if isNullType(it) {
				ctx.errf(vd.Pos, "TY-TYP-0028", "'%s' cannot infer a type from null", v.Type.Name)
				it = ast.ErrorType{}
			}
			if it != nil {
				if _, isLam := vd.Init.(*ast.Lambda); isLam {
					ctx.errf(vd.Pos, "TY-TYP-0029", "'%s' cannot infer a functional interface type; declare it explicitly", v.Type.Name)
					it = ast.ErrorType{}
				}
			}
			t = it
		} else if vd.Init != nil {
			ctx.checkExpr(vd.Init, t)
			ctx.convertTo(vd.Init, t)
		}
		v2 := ctx.declare(vd.Name, t, vd.Pos)
		if v.Type.Name == "val" || v.Mods.Has(ast.ModFinal) {
			v2.Final = true
		}
		// A final variable declared with an initializer has spent its one
		// assignment. Without this the counter stayed at zero until the second
		// assignment, so the first reassignment of a `val` went through -- the
		// declaration was not counted as the assignment it is. A `final int x`
		// with no initializer still gets its one assignment, which is Java's
		// rule (JLS 16); `val` cannot be written that way at all.
		if v2.Final && vd.Init != nil {
			v2.Assigns = 1
		}
		vd.Sym = v2
		_ = i
	}
}

func (ctx *methodCtx) checkForEach(v *ast.ForEach) {
	c := ctx.c
	ctx.push()
	defer ctx.pop()
	ctx.checkExpr(v.X, nil)
	xt := v.X.GetType()
	var elem ast.Type
	if arr, ok := xt.(*ast.ArrayType); ok {
		elem = arr.Elem
		v.Iterable = false
	} else {
		iter := &ast.ClassType{Class: c.b.Iterable}
		if xt != nil && c.isSubtype(xt, iter) {
			v.Iterable = true
			if ct, ok := xt.(*ast.ClassType); ok {
				if sup := c.asSuper(ct, c.b.Iterable); sup != nil && len(sup.Args) == 1 {
					elem = sup.Args[0]
				}
			}
		} else if xt != nil && !ast.IsError(xt) {
			ctx.errf(v.Var.Pos, "TY-TYP-0030", "for-each requires an array or Iterable, found %s", xt)
			elem = ast.ErrorType{}
		} else {
			elem = ast.ErrorType{}
		}
	}
	declared := v.Var.Type.Name != "var" && v.Var.Type.Name != "val"
	if declared {
		dt := c.resolveType(ctx.env, v.Var.Type)
		// `for (int v : listOfInteger)` unboxes, like any other assignment
		if !ast.IsError(elem) && !c.assignableTo(elem, dt) {
			ctx.errf(v.Var.Pos, "TY-TYP-0031", "incompatible types: %s is not assignable to %s", elem, dt)
		}
		// the element type stays what the sequence yields; code generation
		// converts it to the declared type of the loop variable
		v.Var.Sym = ctx.declare(v.Var.Name, dt, v.Var.Pos)
		v.Elem = elem
	} else {
		v.Elem = elem
		v.Var.Sym = ctx.declare(v.Var.Name, elem, v.Var.Pos)
	}
	if v.Var.Name == "_" {
		v.Var.Unnamed = true
	}
	if v.Var.Type != nil && v.Var.Type.Name == "val" {
		v.Var.Sym.Final = true
	}
	// The body is a loop body: `break` and `continue` belong to this statement
	// and not to whatever encloses it, so the counter the checker reads when it
	// sees one has to cover the enhanced form too. Without this every
	// `continue` in a for-each was reported as being outside a loop.
	ctx.loops++
	ctx.checkStmt(v.Body)
	ctx.loops--
}

func (ctx *methodCtx) checkReturn(v *ast.Return) {
	m := ctx.m
	if m == nil {
		return
	}
	want := m.Result
	if m.IsCtor {
		want = ast.TVoid
	}
	if v.X == nil {
		if want != nil && !ast.IsPrim(want, ast.Void) {
			ctx.errf(v.Pos, "TY-TYP-0032", "return value required for %s", m.Name)
		}
		return
	}
	// the declared result type is the target type of a lambda or method
	// reference in the return expression
	target := want
	if target != nil && !ast.IsRef(target) {
		target = nil
	}
	ctx.checkExpr(v.X, target)
	if want == nil || ast.IsPrim(want, ast.Void) {
		if !m.IsCtor {
			ctx.errf(v.Pos, "TY-TYP-0033", "cannot return a value from a void method")
		}
		return
	}
	ctx.convertTo(v.X, want)
}

func (ctx *methodCtx) checkTry(v *ast.Try) {
	c := ctx.c
	ctx.push()
	defer ctx.pop()
	for _, r := range v.Resources {
		// `try (held)` is Java 9's form: an existing variable (or any
		// expression) named as the resource. Its expression is not a statement,
		// so the no-effect rule does not apply to it.
		if es, ok := r.(*ast.ExprStmt); ok {
			ctx.checkExpr(es.X, nil)
			continue
		}
		ctx.checkStmt(r)
		if lv, ok := r.(*ast.LocalVar); ok {
			for _, vd := range lv.Vars {
				if vd.Sym != nil {
					vd.Sym.Final = true
				}
				// Java requires the resource's type to be a subtype of
				// AutoCloseable, and this is where that stops being a
				// formality: the implicit close is an interface call, so a
				// class that merely happens to have a close() method compiles
				// into a dispatch the object has no entry for -- a runtime
				// failure with nothing in the source to point at.
				if vd.Sym != nil && vd.Sym.Type != nil && !ast.IsError(vd.Sym.Type) &&
					!c.isSubtype(vd.Sym.Type, &ast.ClassType{Class: c.b.AutoCloseable}) {
					ctx.errf(vd.Pos, "TY-TYP-0113",
						"resource type %s is not a subtype of AutoCloseable", vd.Sym.Type)
				}
			}
		}
	}
	ctx.checkBlock(v.Body, true)
	for _, cat := range v.Catches {
		ctx.push()
		for _, te := range cat.Types {
			t := c.resolveType(ctx.env, te)
			if !c.isSubtype(t, &ast.ClassType{Class: c.b.Throwable}) && !ast.IsError(t) {
				ctx.errf(te.Pos, "TY-TYP-0034", "catch type must be a Throwable, found %s", t)
			}
		}
		ct := c.resolveType(ctx.env, cat.Types[0])
		cat.Sym = ctx.declare(cat.Name, ct, cat.Pos)
		if cat.Name == "_" {
			cat.Unnamed = true
		}
		ctx.checkBlock(cat.Body, true)
		ctx.pop()
	}
	if v.Finally != nil {
		ctx.checkBlock(v.Finally, true)
	}
}

func (ctx *methodCtx) checkSwitch(s *ast.Switch, expr bool) {
	c := ctx.c
	ctx.checkExpr(s.X, nil)
	xt := s.X.GetType()
	hasPattern := false
	for _, cs := range s.Cases {
		if cs.Pattern != nil || cs.Null {
			hasPattern = true
		}
		if cs.Null && !ast.IsRef(xt) {
			ctx.errf(cs.Pos, "TY-TYP-0089", "'case null' requires a reference selector")
		}
	}
	// Java allows a switch on a boxed integral type as well as on the primitive,
	// and unboxes the selector (JLS 14.11). A pattern or a `case null` keeps the
	// box instead: those cases ask about the box itself, and only the constant
	// form below is rewritten.
	boxed := c.boxedSwitchPrim(xt)
	switch {
	case hasPattern && ast.IsRef(xt):
		s.Kind = ast.SwitchType
	case hasPattern && isIntegralType(xt):
		// A primitive selector under a pattern switch (JEP 507): each case asks
		// whether the value converts exactly to the pattern's type, so a long
		// selector is legal here even though no constant switch accepts one
		// (t72_primitive_switch).
		s.Kind = ast.SwitchInt
	case boxed != nil:
		// `switch (Character c)` is a switch on the char the box holds, and a
		// null box throws NullPointerException when it is opened, exactly as in
		// Java. Rewriting the selector as the primitive keeps one lowering: the
		// constant C switch the backend writes for a char selector, whose labels
		// are already the values the box carries.
		s.X = ctx.convertWith(s.X, boxed, xt)
		xt = boxed
		s.Kind = ast.SwitchInt
	case isSwitchSelectorType(xt):
		// byte, short, char and int are the primitives a constant switch may
		// select on. long is integral but is not one of them, and float and
		// double are not integral at all: all three fall through to the
		// diagnostic below rather than being truncated to int.
		s.Kind = ast.SwitchInt
	case c.isSubtype(xt, c.strType):
		s.Kind = ast.SwitchString
	case isEnumType(xt):
		s.Kind = ast.SwitchEnum
	default:
		if xt != nil && !ast.IsError(xt) {
			ctx.errf(s.Pos, "TY-TYP-0035", "switch selector must be a char, byte, short, int, Character, Byte, Short, Integer, String or enum type, found %s", xt)
		}
		s.Kind = ast.SwitchInt
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	hasDefault := false
	for _, cs := range s.Cases {
		if cs.Default {
			if hasDefault {
				ctx.errf(cs.Pos, "TY-TYP-0036", "duplicate default label")
			}
			hasDefault = true
		}
		if cs.Pattern != nil {
			ctx.push()
			t := c.resolveType(ctx.env, cs.Pattern.Type)
			if prim, isPrim := t.(*ast.PrimType); isPrim {
				ctx.checkPrimitivePattern(cs.Pattern.Pos, prim, xt, cs.Pattern)
			} else if !c.isSubtype(t, xt) && !c.isSubtype(xt, t) {
				ctx.errf(cs.Pattern.Pos, "TY-TYP-0037", "incompatible pattern type %s for switch on %s", t, xt)
			}
			ctx.declarePattern(cs.Pattern, t)
		}
		if isEnumType(xt) {
			if et, ok := xt.(*ast.ClassType); ok {
				for _, l := range cs.Labels {
					if id, ok2 := l.(*ast.Ident); ok2 && id.Ref == nil {
						if f := et.Class.FieldMap[id.Name]; f != nil && f.Mods.Has(ast.ModStatic) {
							id.Ref = f
							id.SetType(f.Type)
						}
					}
				}
			}
		}
		for _, l := range cs.Labels {
			ctx.checkExpr(l, nil)
			ctx.convertTo(l, xt)
			// what the label names, for the exhaustiveness check below: an
			// enum constant is written bare or qualified (`case RED` and
			// `case Color.RED` are the same constant)
			switch lbl := l.(type) {
			case *ast.Ident:
				covered[lbl.Name] = true
			case *ast.Select:
				covered[lbl.Name] = true
			}
			cv := c.constEval(l)
			if !cv.ok {
				ctx.errf(l.GetPos(), "TY-TYP-0038", "case label must be a constant expression")
				continue
			}
			key := cv.s
			if cv.kind != ast.LitString {
				key = fmt.Sprintf("%d", cv.i)
				if cv.kind == ast.LitDouble || cv.kind == ast.LitFloat {
					key = fmt.Sprintf("%v", cv.f)
				}
			}
			if seen[key] {
				ctx.errf(l.GetPos(), "TY-TYP-0039", "duplicate case label")
			}
			seen[key] = true
		}
		if cs.Guard != nil {
			ctx.checkCond(cs.Guard)
		}
		prev := ctx.sw
		ctx.sw = s
		ctx.push()
		if cs.ArrowX != nil {
			ctx.checkExpr(cs.ArrowX, nil)
		}
		for _, st := range cs.Body {
			ctx.checkStmt(st)
		}
		ctx.pop()
		ctx.sw = prev
		if cs.Pattern != nil {
			ctx.pop()
		}
	}
	// A switch expression must be exhaustive (JLS 14.11.2): with no default and
	// no arms that cover every value it would evaluate to the type's zero
	// value, a silent wrong answer. An enum selector and a sealed one are
	// decidable here, so those are checked rather than refused outright like
	// the others.
	// A pattern switch records the answer either way: it is what decides
	// whether a method may end in the switch without a return after it
	// (canCompleteNormally, JLS 14.22).
	if !hasDefault && s.Kind == ast.SwitchType {
		s.Exhaustive = ctx.patternsExhaustive(s, xt)
	}
	if expr && !hasDefault {
		switch s.Kind {
		case ast.SwitchInt, ast.SwitchString:
			ctx.errf(s.Pos, "TY-TYP-0096", "switch expression does not cover all possible input values")
		case ast.SwitchEnum:
			et, ok := xt.(*ast.ClassType)
			if ok {
				for _, f := range et.Class.EnumConsts {
					if !covered[f.Name] {
						ctx.errf(s.Pos, "TY-TYP-0096", "switch expression does not cover all possible input values")
						break
					}
				}
			}
		case ast.SwitchType:
			if !s.Exhaustive {
				ctx.errf(s.Pos, "TY-TYP-0096", "switch expression does not cover all possible input values")
			}
		}
	}
}

// patternsExhaustive reports whether the type patterns of a switch expression
// cover every value of its selector type.
//
// A pattern whose type is a supertype of the selector's is total -- every value
// of the selector matches it -- and covers the whole switch on its own. Failing
// that the question is answerable only for a sealed selector: the values it can
// take are its permitted subtypes, and it has to be exhaustive for each of them
// (JLS 14.11.2). A guarded pattern is not counted, since `when` can fail and so
// proves nothing about the value it binds: javac rejects the same switch.
//
// Constant labels are not counted either. A sealed interface whose permitted
// subtype is an enum can be switched with constant labels in Java, but a label
// against an interface selector compares a pointer with a constant in this
// backend, so such a switch is left to the `default` it should have written.
func (ctx *methodCtx) patternsExhaustive(s *ast.Switch, xt ast.Type) bool {
	var pats []ast.Type
	for _, cs := range s.Cases {
		if cs.Pattern == nil || cs.Guard != nil || cs.Pattern.Type == nil {
			continue
		}
		pats = append(pats, cs.Pattern.Type.Resolved)
	}
	return ctx.patternsCover(pats, xt, map[*ast.Class]bool{})
}

// patternsCover reports whether pats cover every value of t: some pattern is a
// supertype of t, or t is sealed and every one of its permitted subtypes is
// covered in turn. seen breaks a `permits` cycle, which is illegal Java but is
// not something the sealed-type checks this compiler lacks would have caught.
func (ctx *methodCtx) patternsCover(pats []ast.Type, t ast.Type, seen map[*ast.Class]bool) bool {
	c := ctx.c
	for _, p := range pats {
		if c.isSubtype(t, p) {
			return true
		}
	}
	cl, ok := t.(*ast.ClassType)
	if !ok || cl.Class == nil || !cl.Class.Mods.Has(ast.ModSealed) || seen[cl.Class] {
		// Nothing is known about the values of a type that is not sealed: only a
		// default or a total pattern can make such a switch exhaustive.
		return false
	}
	seen[cl.Class] = true
	permits := c.permittedSubtypes(ctx.env, cl.Class)
	if len(permits) == 0 {
		// A sealed type that names no permitted subclass says nothing about the
		// values it can take, so exhaustiveness cannot be shown for it.
		return false
	}
	for _, pc := range permits {
		if !ctx.patternsCover(pats, &ast.ClassType{Class: pc}, seen) {
			return false
		}
	}
	return true
}

// permittedSubtypes returns the classes a sealed type's permits clause names.
// A name that does not resolve is left out: the caller can then only answer
// conservatively, and reporting a bad name belongs to the sealed-type checks
// this compiler does not have yet (AGENTS.md §10).
func (c *Checker) permittedSubtypes(env *typeEnv, cl *ast.Class) []*ast.Class {
	if cl.Decl == nil {
		return nil
	}
	var out []*ast.Class
	for _, te := range cl.Decl.Permits {
		if pc := c.lookupClassName(env, te.Name); pc != nil {
			out = append(out, pc)
		}
	}
	return out
}

func isEnumType(t ast.Type) bool {
	ct, ok := t.(*ast.ClassType)
	return ok && ct.Class.Kind == ast.KindEnum
}

// isIntegralType reports whether t is a primitive integral type, char included.
// float and double are deliberately excluded: a switch selector must be
// integral (docs/language.md §6.4).
func isIntegralType(t ast.Type) bool {
	p, ok := t.(*ast.PrimType)
	return ok && p.IsIntegral()
}

// isSwitchSelectorType reports whether t is a primitive a switch with constant
// labels may select on: char, byte, short and int (JLS 14.11). long is integral
// but is not one of them -- Java rejects `switch` on a long selector -- and
// float and double are not integral at all.
func isSwitchSelectorType(t ast.Type) bool {
	p, ok := t.(*ast.PrimType)
	return ok && p.IsIntegral() && p.Kind != ast.Long
}

// boxedSwitchPrim returns the primitive a boxed switch selector is unboxed to,
// or nil when the selector is not one of the four boxes Java allows: Character,
// Byte, Short and Integer. Long, Float, Double and Boolean are not among them.
func (c *Checker) boxedSwitchPrim(t ast.Type) *ast.PrimType {
	p, ok := c.unboxed(t)
	if !ok {
		return nil
	}
	switch p.Kind {
	case ast.Byte, ast.Short, ast.Char, ast.Int:
		return p
	}
	return nil
}

// ---------------------------------------------------------------- expressions

func (ctx *methodCtx) checkCond(e ast.Expr) {
	ctx.checkExpr(e, ast.TBoolean)
	ctx.convertTo(e, ast.TBoolean)
}

func (ctx *methodCtx) checkExpr(e ast.Expr, want ast.Type) {
	c := ctx.c
	switch v := e.(type) {
	case *ast.Literal:
		switch v.Kind {
		case ast.LitInt:
			v.SetType(ast.TInt)
		case ast.LitLong:
			v.SetType(ast.TLong)
		case ast.LitFloat:
			v.SetType(ast.TFloat)
		case ast.LitDouble:
			v.SetType(ast.TDouble)
		case ast.LitChar:
			v.SetType(ast.TChar)
		case ast.LitString:
			v.SetType(c.strType)
		case ast.LitBool:
			v.SetType(ast.TBoolean)
		case ast.LitNull:
			v.SetType(ast.NullType{})
		}
	case *ast.Ident:
		ctx.checkIdent(v, want)
	case *ast.Select:
		ctx.checkSelect(v, want)
	case *ast.Index:
		ctx.checkExpr(v.X, nil)
		ctx.checkExpr(v.Index, ast.TInt)
		ctx.convertTo(v.Index, ast.TInt)
		xt := v.X.GetType()
		if arr, ok := xt.(*ast.ArrayType); ok {
			v.SetType(arr.Elem)
		} else if ast.IsError(xt) || xt == nil {
			v.SetType(ast.ErrorType{})
		} else {
			ctx.errf(v.Pos, "TY-TYP-0040", "array required, found %s", xt)
			v.SetType(ast.ErrorType{})
		}
	case *ast.Call:
		ctx.checkCall(v, want)
	case *ast.New:
		ctx.checkNew(v, want)
	case *ast.NewArray:
		ctx.checkNewArray(v)
	case *ast.ArrayInit:
		ctx.checkArrayInit(v, want)
	case *ast.Unary:
		ctx.checkUnary(v)
	case *ast.Binary:
		ctx.checkBinary(v, want)
	case *ast.Assign:
		ctx.checkAssign(v)
	case *ast.Cond:
		ctx.checkCond2(v, want)
	case *ast.Cast:
		ctx.checkExpr(v.X, nil)
		t := c.resolveType(ctx.env, v.Type)
		for i := 0; i < v.Type.Dims; i++ {
		}
		xt := v.X.GetType()
		if xt != nil && !c.isCastable(xt, t) {
			ctx.errf(v.Pos, "TY-TYP-0041", "inconvertible types: %s cannot be cast to %s", xt, t)
		}
		v.SetType(t)
	case *ast.InstanceOf:
		ctx.checkExpr(v.X, nil)
		t := c.resolveType(ctx.env, v.Type)
		if _, isPrim := t.(*ast.PrimType); isPrim && v.Binding == nil {
			// JEP 507: there is nothing to test without a binding
			ctx.errf(v.Pos, "TY-TYP-0092", "a primitive pattern needs a name to bind the value to")
		}
		if v.Binding != nil {
			xt := v.X.GetType()
			if prim, isPrim := t.(*ast.PrimType); isPrim {
				// JEP 507: a primitive type pattern asks whether the value
				// survives the conversion, not whether it has the type
				ctx.checkPrimitivePattern(v.Pos, prim, xt, v.Binding)
				if sv, ok := xt.(*ast.PrimType); ok {
					// the test reads a boxed value, so a primitive selector is
					// boxed first
					if box := c.b.Boxes[sv.Kind]; box != nil {
						v.X = ctx.convertWith(v.X, &ast.ClassType{Class: box}, xt)
					}
				}
			} else if !c.isSubtype(t, xt) && !c.isSubtype(xt, t) && !ast.IsError(xt) {
				ctx.errf(v.Pos, "TY-TYP-0042", "incompatible pattern type %s for %s", t, xt)
			}
			ctx.push()
			ctx.declarePattern(v.Binding, t)
		}
		v.SetType(ast.TBoolean)
	case *ast.Lambda:
		ctx.checkLambda(v, want)
	case *ast.MethodRef:
		ctx.checkMethodRef(v, want)
	case *ast.This:
		if v.Qual != "" {
			oc := ctx.lookupOuter(v.Qual)
			if oc == nil {
				ctx.errf(v.Pos, "TY-TYP-0043", "not an enclosing class: %s", v.Qual)
				v.SetType(ast.ErrorType{})
				return
			}
			v.Qual = oc.Full
			v.SetType(&ast.ClassType{Class: oc, Args: typeVarArgs(oc)})
			return
		}
		if ctx.lambda != nil {
			ctx.thisUse(v.Pos, "variable this")
		}
		v.SetType(&ast.ClassType{Class: ctx.cl, Args: typeVarArgs(ctx.cl)})
	case *ast.SuperExpr:
		if ctx.cl.Super == nil {
			ctx.errf(v.Pos, "TY-TYP-0044", "no superclass")
			v.SetType(ast.ErrorType{})
			return
		}
		// super.x reaches the enclosing instance too, so a lambda body has to
		// carry it exactly as it carries `this`
		if ctx.lambda != nil {
			ctx.thisUse(v.Pos, "variable super")
		}
		v.SetType(ctx.cl.Super)
	case *ast.SwitchExpr:
		ctx.checkSwitch(v.S, true)
		var rt ast.Type
		for _, cs := range v.S.Cases {
			var t ast.Type
			switch {
			case cs.ArrowX != nil:
				t = cs.ArrowX.GetType()
			case len(cs.Body) == 1:
				if y, ok := cs.Body[0].(*ast.Yield); ok {
					t = y.X.GetType()
				} else if th, ok := cs.Body[0].(*ast.Throw); ok {
					_ = th
					continue
				} else if b, ok := cs.Body[0].(*ast.Block); ok {
					t = blockYieldType(b)
					if t == nil {
						continue
					}
				} else {
					continue
				}
			default:
				continue
			}
			if rt == nil {
				rt = t
			} else if !sameType(rt, t) {
				if c.isSubtype(t, rt) {
				} else if c.isSubtype(rt, t) {
					rt = t
				} else {
					rt = c.lub(rt, t)
				}
			}
		}
		if rt == nil {
			rt = ast.ErrorType{}
		}
		v.SetType(rt)
	case *ast.ClassLit:
		t := c.resolveType(ctx.env, v.Type)
		// A class literal denotes a Class object -- the same kind of value
		// Object.getClass() returns -- so that is its static type. It used to
		// be Object, which made `A.class.getName()` unresolvable.
		v.SetType(c.classLiteralType())
		v.Type.Resolved = t
	default:
		ctx.errf(e.GetPos(), "TY-INT-0002", "unsupported expression %T", e)
		e.SetType(ast.ErrorType{})
	}
}

// preludeClassFull is the prelude class a class literal's value is an instance
// of, teyru.Class (lib/01_core.teyru). Object.getClass() returns the same kind
// of value, and code generation hands the runtime this same class.
const preludeClassFull = "teyru.Class"

// classLiteralType is the static type of a class literal: the prelude's Class.
func (c *Checker) classLiteralType() ast.Type {
	if cl := c.global[preludeClassFull]; cl != nil {
		return &ast.ClassType{Class: cl}
	}
	// The prelude always declares Class; a prelude without it has already been
	// reported by initBuiltins, and Object keeps the checker going.
	return &ast.ClassType{Class: c.b.Object}
}

func blockYieldType(b *ast.Block) ast.Type {
	for _, s := range b.Stmts {
		if y, ok := s.(*ast.Yield); ok {
			return y.X.GetType()
		}
	}
	return nil
}

// lub returns a common supertype.
func (c *Checker) lub(a, b ast.Type) ast.Type {
	if p, ok := a.(*ast.PrimType); ok {
		if q, ok := b.(*ast.PrimType); ok {
			return numericPromote(p, q)
		}
		return c.objType
	}
	ca, ok1 := a.(*ast.ClassType)
	cb, ok2 := b.(*ast.ClassType)
	if !ok1 || !ok2 {
		if ast.IsRef(a) && ast.IsRef(b) {
			return c.objType
		}
		return ast.ErrorType{}
	}
	sup := c.asSuper(cb, ca.Class)
	if sup != nil {
		if len(ca.Args) == 0 || len(sup.Args) == 0 {
			return &ast.ClassType{Class: ca.Class}
		}
		return ca
	}
	sup = c.asSuper(ca, cb.Class)
	if sup != nil {
		if len(cb.Args) == 0 || len(sup.Args) == 0 {
			return &ast.ClassType{Class: cb.Class}
		}
		return cb
	}
	// Neither is a supertype of the other, so the answer is their nearest
	// common supertype -- Base for two subclasses of Base, not Object. Walking
	// one chain and asking whether the other is a subtype of each step finds
	// the first shared ancestor, and the walk goes up the superclasses before
	// the interfaces because a class beats an interface when both fit, which is
	// what Java picks.
	for k := ca.Class; k != nil; k = superOf(k) {
		if k != ca.Class && c.isSubtype(cb, &ast.ClassType{Class: k}) {
			return &ast.ClassType{Class: k}
		}
	}
	for k := ca.Class; k != nil; k = superOf(k) {
		for _, i := range k.Ifaces {
			if i.Class != nil && c.isSubtype(cb, &ast.ClassType{Class: i.Class}) {
				return &ast.ClassType{Class: i.Class}
			}
		}
	}
	return c.objType
}

// superOf is the superclass of a class, or nil at the root of the hierarchy.
func superOf(cl *ast.Class) *ast.Class {
	if cl.Super == nil {
		return nil
	}
	return cl.Super.Class
}

func (ctx *methodCtx) lookupOuter(name string) *ast.Class {
	for cl := ctx.cl.Outer; cl != nil; cl = cl.Outer {
		if cl.Name == name || cl.Full == name {
			return cl
		}
	}
	return nil
}

func (ctx *methodCtx) checkIdent(v *ast.Ident, want ast.Type) {
	c := ctx.c
	if v.Ref != nil {
		ctx.setRefType(v, v.Ref)
		// the reference was resolved in an enclosing scope: when the lambda
		// body reuses it, the variable still has to be captured
		if lv, ok := v.Ref.(*ast.Var); ok {
			ctx.noteCapture(lv)
		}
		return
	}
	if lv := ctx.lookupLocal(v.Name); lv != nil {
		v.Ref = lv
		v.SetType(lv.Type)
		ctx.noteCapture(lv)
		return
	}
	// enclosing method locals (local/anonymous classes)
	for cl := ctx.cl; cl != nil; cl = cl.Outer {
		if cl.LocalOwner == nil {
			continue
		}
		// The scopes captured where the class was declared are the enclosing
		// method's own, so looking in them finds the variable itself. Building
		// a fresh context for the enclosing method instead declared a second
		// copy of each of its parameters: the copy was what the body referred
		// to, and it was appended to the method's parameter list a second time,
		// so the C parameter it was given was one past the real ones.
		for i := len(cl.LocalScopes) - 1; i >= 0; i-- {
			lv := cl.LocalScopes[i][v.Name]
			if lv == nil {
				continue
			}
			v.Ref = lv
			v.SetType(lv.Type)
			ctx.captureOuter(cl, lv)
			ctx.noteCapture(lv)
			return
		}
	}
	if f := ctx.lookupField(v.Name); f != nil {
		if ctx.inStatic() && !f.Mods.Has(ast.ModStatic) && ctx.m != nil {
			ctx.errf(v.Pos, "TY-TYP-0045", "cannot access instance field %s from a static context", v.Name)
		}
		if f.Mods.Has(ast.ModPrivate) && !sameNest(f.Owner, ctx.cl) {
			ctx.errf(v.Pos, "TY-TYP-0046", "%s has private access in %s", f.Name, f.Owner.Name)
		}
		// A bare field name reads through the enclosing instance, so a lambda
		// body has to carry that instance the same way an explicit `this` makes
		// it. The static case is already reported above, by TY-TYP-0045.
		if !f.Mods.Has(ast.ModStatic) && ctx.lambda != nil && ctx.hasThis() {
			ctx.noteThis()
		}
		v.Ref = f
		v.SetType(f.Type)
		ctx.rewriteProp(v, f)
		return
	}
	// static field of enclosing/imported types
	if f := ctx.lookupStaticField(v.Name); f != nil {
		v.Ref = f
		v.SetType(f.Type)
		ctx.rewriteProp(v, f)
		return
	}
	// a type name used as a qualifier for a static member
	if cl := c.lookupClassName(ctx.env, v.Name); cl != nil {
		v.Ref = cl
		v.SetType(&ast.ClassType{Class: cl})
		return
	}
	ctx.errf(v.Pos, "TY-TYP-0048", "cannot find symbol %s", v.Name)
	v.SetType(ast.ErrorType{})
}

// rewriteProp turns a field read into a getter call for native properties.
func (ctx *methodCtx) rewriteProp(v ast.Expr, f *ast.Field) {
	if !f.IsProp {
		return
	}
	if id, ok := v.(*ast.Ident); ok {
		if id.Name == "field" {
			return
		}
	}
	if f.Getter == nil {
		ctx.errf(v.GetPos(), "TY-PROP-0005", "property %s has no getter; use 'field' inside an accessor", f.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	recv := ast.Expr(&ast.This{ExprBase: ast.ExprBase{Pos: v.GetPos(), T: &ast.ClassType{Class: ctx.cl, Args: typeVarArgs(ctx.cl)}}})
	if f.Mods.Has(ast.ModStatic) {
		recv = nil
	}
	ctx.props[v] = &ast.Call{ExprBase: ast.ExprBase{Pos: v.GetPos(), T: f.Type}, Recv: recv, Name: f.Getter.Name, Args: []ast.Expr{}, Method: f.Getter}
}

// checkPropAccess rejects a property whose accessor is private somewhere else.
//
// The storage field behind a property is private whatever the property is, so
// the accessor's own modifiers are the ones that say who may use it -- the
// declaration's visibility is the default the accessor takes when it declares
// none of its own.
func (ctx *methodCtx) checkPropAccess(pos source.Pos, f *ast.Field, m *ast.Method) {
	if m == nil || !m.Mods.Has(ast.ModPrivate) {
		return
	}
	if sameNest(m.Owner, ctx.cl) {
		return
	}
	ctx.errf(pos, "TY-TYP-0046", "%s has private access in %s", f.Name, m.Owner.Name)
}

func (ctx *methodCtx) lookupStaticField(name string) *ast.Field {
	for _, f := range ctx.staticImports {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// capturedVars lists the variables a class declared inside a method captures,
// in a stable order shared by the checker and code generation.
func capturedVars(cl *ast.Class) []*ast.Var {
	var out []*ast.Var
	for v := range cl.CapFields {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// noteCapture marks a local as captured when referenced from a lambda or inner class.
func (ctx *methodCtx) noteCapture(v *ast.Var) {
	if ctx.lambda == nil {
		return
	}
	// A variable of an enclosing method is copied into the lambda object; the
	// lambda's own parameters and locals stay on the C stack. Every lambda
	// between this one and the variable's owner carries it, because a capture
	// is one hop: a lambda written inside another lambda has to hold the value
	// in its own object before it can copy it into a third. Recording only the
	// innermost one left the inner closure reading a local of a method it is
	// not compiled into, and the C name it emitted was the outer method's.
	for lam := ctx.lambda; lam != nil; lam = lam.Outer {
		if v.Owner == lambdaMethod(lam) {
			return
		}
		seen := false
		for _, c := range lam.Captures {
			if c == v {
				seen = true
				break
			}
		}
		if !seen {
			lam.Captures = append(lam.Captures, v)
		}
	}
	v.Captured = true
}

// lambdaMethod is the synthesized method a lambda's body is checked in, which
// is what decides whether a variable belongs to the lambda itself.
func lambdaMethod(lam *ast.Lambda) *ast.Method {
	if lam.Class == nil {
		return nil
	}
	for _, ms := range lam.Class.Methods {
		for _, m := range ms {
			if m.Lambda == lam {
				return m
			}
		}
	}
	return nil
}

func (ctx *methodCtx) captureOuter(target *ast.Class, v *ast.Var) {
	// mark every class between ctx.cl and target as capturing v
	for cl := ctx.cl; cl != nil; cl = cl.Outer {
		if cl.CapFields[v] == nil {
			f := &ast.Field{Name: "_cap$" + v.Name, Type: v.Type, Mods: ast.ModPrivate | ast.ModFinal, Pos: v.Pos, Storage: true, Owner: cl}
			cl.CapFields[v] = f
		}
		if cl == target {
			break
		}
	}
}

func (ctx *methodCtx) convertWith(e ast.Expr, target, src ast.Type) ast.Expr {
	c := ctx.c
	if target == nil || src == nil || ast.IsError(target) || ast.IsError(src) {
		return e
	}
	if sameType(target, src) {
		return e
	}
	if _, ok := src.(ast.NullType); ok && ast.IsRef(target) {
		return e
	}
	if sp, isPrim := src.(*ast.PrimType); isPrim && ast.IsRef(target) {
		// boxing, and boxing followed by a widening reference conversion: both
		// are a Conv node, which code generation lowers to the box call plus a
		// cast to the target
		if _, ok := c.unboxed(target); ok {
			return &ast.Conv{ExprBase: ast.ExprBase{Pos: e.GetPos(), T: target}, X: e}
		}
		if box := c.b.Boxes[sp.Kind]; box != nil &&
			c.isSubtype(&ast.ClassType{Class: box}, target) {
			return &ast.Conv{ExprBase: ast.ExprBase{Pos: e.GetPos(), T: target}, X: e}
		}
	}
	if ast.IsRef(src) && isPrimType(target) {
		if _, ok := c.unboxed(src); ok {
			return &ast.Conv{ExprBase: ast.ExprBase{Pos: e.GetPos(), T: target}, X: e}
		}
	}
	return e
}

func isPrimType(t ast.Type) bool {
	_, ok := t.(*ast.PrimType)
	return ok
}

// convertTo checks assignability and records conversions.
func (ctx *methodCtx) convertTo(e ast.Expr, target ast.Type) {
	c := ctx.c
	src := e.GetType()
	if target == nil || src == nil || ast.IsError(target) || ast.IsError(src) {
		return
	}
	if sameType(src, target) {
		return
	}
	if _, ok := src.(ast.NullType); ok {
		if isPrimType(target) {
			ctx.errf(e.GetPos(), "TY-TYP-0049", "null is not assignable to %s", target)
		}
		return
	}
	if isPrimType(src) && isPrimType(target) {
		sp := src.(*ast.PrimType)
		tp := target.(*ast.PrimType)
		if sp.IsNumeric() && tp.IsNumeric() {
			// narrowing needs a cast, except when the source is a constant in range
			if widening(sp, tp) {
				return
			}
			if cv := c.constEval(e); cv.ok {
				if fitsConstant(cv, tp) {
					return
				}
			}
			ctx.errf(e.GetPos(), "TY-TYP-0050", "possible lossy conversion from %s to %s", sp, tp)
			return
		}
		ctx.errf(e.GetPos(), "TY-TYP-0051", "incompatible types: %s cannot be converted to %s", src, target)
		return
	}
	if sp, ok := src.(*ast.PrimType); ok && ast.IsRef(target) {
		if _, ok := c.unboxed(target); ok {
			return
		}
		// boxing followed by a widening reference conversion (JLS 5.3): an int
		// fits a Number parameter, because it boxes to Integer and Integer is a
		// Number. The Object case is the same rule with Object at the top, and
		// is covered by the subtype test.
		if box := c.b.Boxes[sp.Kind]; box != nil &&
			c.isSubtype(&ast.ClassType{Class: box}, target) {
			return
		}
		ctx.errf(e.GetPos(), "TY-TYP-0051", "incompatible types: %s cannot be converted to %s", src, target)
		return
	}
	if ast.IsRef(src) && isPrimType(target) {
		if bp, ok := c.unboxed(src); ok {
			if widening(bp, target.(*ast.PrimType)) {
				return
			}
		}
		ctx.errf(e.GetPos(), "TY-TYP-0051", "incompatible types: %s cannot be converted to %s", src, target)
		return
	}
	if ast.IsRef(src) && ast.IsRef(target) {
		if c.isSubtype(src, target) {
			return
		}
		// unchecked: erasure match (List<String> -> List)
		if ct, ok := src.(*ast.ClassType); ok {
			if tt, ok2 := target.(*ast.ClassType); ok2 && tt.Class.Special != "box" {
				if c.asSuper(ct, tt.Class) != nil {
					return
				}
			}
		}
		ctx.errf(e.GetPos(), "TY-TYP-0051", "incompatible types: %s cannot be converted to %s", src, target)
	}
}

// widening reports whether a value of type `from` converts to `to` with no cast.
//
// The pairs are the ones JLS 5.1.2 lists, written out rather than read off an
// ordering of the kinds. An ordering has to put byte below char -- they are both
// one byte wide and both widen to int -- and putting it there says byte widens
// to char, and short (beside char) widens to char, which the language does not
// say: char is unsigned, and Java converts a byte or a short to a char only with
// a cast. The difference is not academic. With the ordering, `String.valueOf(b)`
// for a byte `b` resolved to valueOf(char) instead of valueOf(int) and printed
// the character the byte encodes rather than its number, which is how the
// standard library's own wrapper code had to be written around it.
func widening(from, to *ast.PrimType) bool {
	if from.Kind == to.Kind {
		return true
	}
	switch from.Kind {
	case ast.Byte:
		return to.Kind == ast.Short || to.Kind == ast.Int || to.Kind == ast.Long ||
			to.Kind == ast.Float || to.Kind == ast.Double
	case ast.Short:
		return to.Kind == ast.Int || to.Kind == ast.Long ||
			to.Kind == ast.Float || to.Kind == ast.Double
	case ast.Char:
		return to.Kind == ast.Int || to.Kind == ast.Long ||
			to.Kind == ast.Float || to.Kind == ast.Double
	case ast.Int:
		return to.Kind == ast.Long || to.Kind == ast.Float || to.Kind == ast.Double
	case ast.Long:
		return to.Kind == ast.Float || to.Kind == ast.Double
	case ast.Float:
		return to.Kind == ast.Double
	}
	return false
}

func fitsConstant(cv constValue, to *ast.PrimType) bool {
	if cv.kind == ast.LitDouble || cv.kind == ast.LitFloat {
		return to.Kind == ast.Float || to.Kind == ast.Double
	}
	switch to.Kind {
	case ast.Byte:
		return cv.i >= -128 && cv.i <= 127
	case ast.Char:
		return cv.i >= 0 && cv.i <= 0xFFFF
	case ast.Short:
		return cv.i >= -32768 && cv.i <= 32767
	case ast.Int:
		return cv.i >= -2147483648 && cv.i <= 2147483647
	case ast.Long:
		return true
	case ast.Float, ast.Double:
		return true
	}
	return false
}

func isNullType(t ast.Type) bool { _, ok := t.(ast.NullType); return ok }

func (ctx *methodCtx) checkUnary(v *ast.Unary) {
	c := ctx.c
	ctx.checkExpr(v.X, nil)
	t := v.X.GetType()
	switch v.Op {
	case "!", "~":
		if v.Op == "!" {
			if t != nil && !isBooleanType(t) && !ast.IsError(t) {
				if _, ok := c.unboxed(t); !ok {
					ctx.errf(v.Pos, "TY-TYP-0052", "operator '!' cannot be applied to %s", t)
					v.SetType(ast.ErrorType{})
					return
				}
			}
			v.SetType(ast.TBoolean)
			return
		}
		if _, ok := t.(*ast.PrimType); !ok {
			ctx.errf(v.Pos, "TY-TYP-0053", "operator '~' requires an integral operand")
			v.SetType(ast.ErrorType{})
			return
		}
		p := unboxOrPrim(c, t)
		if p == nil || !p.IsIntegral() {
			ctx.errf(v.Pos, "TY-TYP-0053", "operator '~' requires an integral operand")
			v.SetType(ast.ErrorType{})
			return
		}
		v.SetType(promoteUnary(p))
	case "+", "-":
		p := unboxOrPrim(c, t)
		if p == nil || !p.IsNumeric() {
			ctx.errf(v.Pos, "TY-TYP-0054", "operator '%s' requires a numeric operand", v.Op)
			v.SetType(ast.ErrorType{})
			return
		}
		v.SetType(promoteUnary(p))
	case "++", "--":
		p := unboxOrPrim(c, t)
		if p == nil || !p.IsNumeric() {
			ctx.errf(v.Pos, "TY-TYP-0055", "operator '%s' requires a numeric operand", v.Op)
			v.SetType(ast.ErrorType{})
			return
		}
		if !ctx.assignable(v.X) {
			ctx.errf(v.Pos, "TY-TYP-0056", "cannot apply '%s' to a non-assignable expression", v.Op)
		}
		v.SetType(t)
		ctx.checkFinalAssign(v.X)
	}
}

func promoteUnary(p *ast.PrimType) ast.Type {
	switch p.Kind {
	case ast.Byte, ast.Short, ast.Char:
		return ast.TInt
	case ast.Long:
		return ast.TLong
	case ast.Float:
		return ast.TFloat
	case ast.Double:
		return ast.TDouble
	}
	return ast.TInt
}

func unboxOrPrim(c *Checker, t ast.Type) *ast.PrimType {
	if p, ok := t.(*ast.PrimType); ok {
		return p
	}
	if p, ok := c.unboxed(t); ok {
		return p
	}
	return nil
}

func (ctx *methodCtx) assignable(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident, *ast.Index, *ast.This:
		return true
	case *ast.Select:
		if _, ok := v.Ref.(*ast.Field); ok {
			return true
		}
		if v.Ref == "length" {
			return false
		}
		return false
	}
	return false
}

func (ctx *methodCtx) checkFinalAssign(target ast.Expr) {
	if id, ok := target.(*ast.Ident); ok {
		if lv, ok := id.Ref.(*ast.Var); ok && lv.Final && lv.Assigns > 0 {
			ctx.errf(target.GetPos(), "TY-TYP-0057", "cannot assign a value to final variable %s", lv.Name)
		}
		ctx.checkFinalField(target, id.Ref)
		return
	}
	// `o.k = 9` is a field of another object, and only the bare name was
	// examined: the check stopped at any target that was not an identifier, so
	// a `final` field was writable from anywhere and the modifier meant
	// nothing outside the class that declared it.
	if sel, ok := target.(*ast.Select); ok {
		ctx.checkFinalField(target, sel.Ref)
	}
}

// checkFinalField rejects an assignment to a final field of another class.
func (ctx *methodCtx) checkFinalField(target ast.Expr, ref any) {
	f, ok := ref.(*ast.Field)
	if !ok || !f.Mods.Has(ast.ModFinal) || f.Owner == ctx.cl {
		return
	}
	ctx.errf(target.GetPos(), "TY-TYP-0058", "cannot assign a value to final field %s", f.Name)
}

func (ctx *methodCtx) checkBinary(v *ast.Binary, want ast.Type) {
	c := ctx.c
	ctx.checkExpr(v.X, nil)
	ctx.checkExpr(v.Y, nil)
	xt, yt := v.X.GetType(), v.Y.GetType()
	switch v.Op {
	case "&&", "||":
		ctx.convertTo(v.X, ast.TBoolean)
		ctx.convertTo(v.Y, ast.TBoolean)
		v.SetType(ast.TBoolean)
		return
	case "==", "!=":
		if isPrimType(xt) && isPrimType(yt) {
			xp, yp := xt.(*ast.PrimType), yt.(*ast.PrimType)
			if xp.Kind == ast.Boolean || yp.Kind == ast.Boolean {
				if xp.Kind != yp.Kind {
					ctx.errf(v.Pos, "TY-TYP-0059", "cannot compare %s and %s", xt, yt)
				}
				v.SetType(ast.TBoolean)
				return
			}
			v.OpType = numericPromote(xp, yp)
			v.SetType(ast.TBoolean)
			return
		}
		if isPrimType(xt) != isPrimType(yt) {
			// one primitive, one reference: unbox the reference
			ctx.convertTo(v.Y, xt)
			ctx.convertTo(v.X, yt)
			if unboxOrPrim(c, xt) != nil && unboxOrPrim(c, yt) != nil {
				v.SetType(ast.TBoolean)
				return
			}
			ctx.errf(v.Pos, "TY-TYP-0059", "incompatible operand types %s and %s", xt, yt)
			v.SetType(ast.TBoolean)
			return
		}
		if !ast.IsRef(xt) || !ast.IsRef(yt) {
			ctx.errf(v.Pos, "TY-TYP-0059", "incompatible operand types %s and %s", xt, yt)
			v.SetType(ast.TBoolean)
			return
		}
		if !c.isCastable(xt, yt) && !c.isCastable(yt, xt) && !ast.IsError(xt) && !ast.IsError(yt) {
			ctx.errf(v.Pos, "TY-TYP-0060", "incomparable types: %s and %s", xt, yt)
		}
		v.SetType(ast.TBoolean)
		return
	case "<", ">", "<=", ">=":
		if isRefType(xt) {
			if bt := unboxOrPrim(c, xt); bt != nil {
				xt = bt
			}
		}
		if isRefType(yt) {
			if bt := unboxOrPrim(c, yt); bt != nil {
				yt = bt
			}
		}
		xp, ok1 := xt.(*ast.PrimType)
		yp, ok2 := yt.(*ast.PrimType)
		if !ok1 || !ok2 || !xp.IsNumeric() || !yp.IsNumeric() {
			ctx.errf(v.Pos, "TY-TYP-0061", "operator '%s' cannot be applied to %s and %s", v.Op, xt, yt)
			v.SetType(ast.ErrorType{})
			return
		}
		v.OpType = numericPromote(xp, yp)
		v.SetType(ast.TBoolean)
		return
	case "&", "|", "^":
		xp := unboxOrPrim(c, xt)
		yp := unboxOrPrim(c, yt)
		if xp == nil || yp == nil {
			ctx.errf(v.Pos, "TY-TYP-0062", "operator '%s' requires integral or boolean operands", v.Op)
			v.SetType(ast.ErrorType{})
			return
		}
		if xp.Kind == ast.Boolean || yp.Kind == ast.Boolean {
			if xp.Kind != yp.Kind {
				ctx.errf(v.Pos, "TY-TYP-0062", "operator '%s' requires both operands to be boolean", v.Op)
				v.SetType(ast.ErrorType{})
				return
			}
			v.SetType(ast.TBoolean)
			return
		}
		if !xp.IsIntegral() || !yp.IsIntegral() {
			ctx.errf(v.Pos, "TY-TYP-0062", "operator '%s' requires integral operands", v.Op)
			v.SetType(ast.ErrorType{})
			return
		}
		v.OpType = numericPromote(xp, yp)
		v.SetType(v.OpType)
		return
	case "<<", ">>", ">>>":
		xp := unboxOrPrim(c, xt)
		yp := unboxOrPrim(c, yt)
		if xp == nil || !xp.IsIntegral() || yp == nil || !yp.IsIntegral() {
			ctx.errf(v.Pos, "TY-TYP-0063", "operator '%s' requires integral operands", v.Op)
			v.SetType(ast.ErrorType{})
			return
		}
		v.OpType = promoteUnary(xp)
		v.SetType(v.OpType)
		return
	case "+":
		if c.isSubtype(xt, c.strType) || c.isSubtype(yt, c.strType) || isNullType(xt) && c.isSubtype(yt, c.strType) || isNullType(yt) && c.isSubtype(xt, c.strType) {
			v.SetType(c.strType)
			return
		}
	}
	// arithmetic
	xp := unboxOrPrim(c, xt)
	yp := unboxOrPrim(c, yt)
	if xp == nil || yp == nil || !xp.IsNumeric() || !yp.IsNumeric() {
		if ast.IsError(xt) || ast.IsError(yt) {
			v.SetType(ast.ErrorType{})
			return
		}
		ctx.errf(v.Pos, "TY-TYP-0064", "operator '%s' cannot be applied to %s and %s", v.Op, xt, yt)
		v.SetType(ast.ErrorType{})
		return
	}
	v.OpType = numericPromote(xp, yp)
	v.SetType(v.OpType)
}

func isRefType(t ast.Type) bool { return ast.IsRef(t) }

func (ctx *methodCtx) checkAssign(v *ast.Assign) {
	c := ctx.c
	// The left-hand side is a write wherever it lands, so a property on it is
	// judged by its setter rather than its getter: the read of the target that
	// resolves the name must not report an access the assignment does not make.
	prevTarget := ctx.assignTarget
	ctx.assignTarget = v.X
	ctx.checkExpr(v.X, nil)
	ctx.assignTarget = prevTarget
	if f := ctx.propertyOf(v.X); f != nil {
		ctx.lowerPropAssign(v, f)
		return
	}
	if !ctx.assignable(v.X) {
		ctx.errf(v.Pos, "TY-TYP-0065", "left-hand side of an assignment must be a variable")
	}
	ctx.checkFinalAssign(v.X)
	if lv, ok := v.X.(*ast.Ident); ok {
		if vr, ok := lv.Ref.(*ast.Var); ok {
			vr.Assigns++
		}
	}
	xt := v.X.GetType()
	ctx.checkExpr(v.Y, xt)
	if v.Op == "=" {
		ctx.convertTo(v.Y, xt)
		v.SetType(xt)
		return
	}
	// compound: implicit cast back to the target type
	yt := v.Y.GetType()
	xp := unboxOrPrim(c, xt)
	yp := unboxOrPrim(c, yt)
	if v.Op == "+=" && (c.isSubtype(xt, c.strType) || isNullType(xt)) {
		v.SetType(xt)
		return
	}
	if xp == nil || yp == nil || ast.IsError(xt) || ast.IsError(yt) {
		v.SetType(ast.ErrorType{})
		return
	}
	switch {
	case xp.Kind == ast.Boolean:
		if v.Op != "&=" && v.Op != "|=" && v.Op != "^=" {
			ctx.errf(v.Pos, "TY-TYP-0066", "operator '%s' cannot be applied to boolean", v.Op)
		}
		v.SetType(xt)
	case v.Op == "<<=" || v.Op == ">>=" || v.Op == ">>>=":
		if !xp.IsIntegral() || !yp.IsIntegral() {
			ctx.errf(v.Pos, "TY-TYP-0063", "operator '%s' requires integral operands", v.Op)
		}
		v.SetType(xt)
	case v.Op == "&=" || v.Op == "|=" || v.Op == "^=":
		if !xp.IsIntegral() || !yp.IsIntegral() {
			ctx.errf(v.Pos, "TY-TYP-0062", "operator '%s' requires integral operands", v.Op)
		}
		v.SetType(xt)
	default:
		if !xp.IsNumeric() || !yp.IsNumeric() {
			ctx.errf(v.Pos, "TY-TYP-0064", "operator '%s' cannot be applied to %s and %s", v.Op, xt, yt)
			v.SetType(ast.ErrorType{})
			return
		}
		v.OpType = numericPromote(xp, yp)
		v.SetType(xt)
	}
}

// declarePattern binds the variables of a (possibly record) pattern.
func (ctx *methodCtx) declarePattern(p *ast.Param, t ast.Type) {
	if len(p.Decomp) > 0 {
		cl := recordOf(t)
		if cl == nil {
			ctx.errf(p.Pos, "TY-TYP-0087", "record pattern requires a record type, found %s", t)
			return
		}
		comps := cl.RecordComps()
		if len(comps) != len(p.Decomp) {
			ctx.errf(p.Pos, "TY-TYP-0088", "record pattern for %s needs %d components, found %d", cl.Name, len(comps), len(p.Decomp))
			return
		}
		p.Comps = comps
		for i, sub := range p.Decomp {
			ctx.declarePattern(sub, comps[i].Type)
		}
		return
	}
	if p.Unnamed {
		p.Sym = ctx.declareUnnamed(p.Pos)
		return
	}
	if p.Name != "" {
		p.Sym = ctx.declare(p.Name, t, p.Pos)
	}
}

// checkPrimitivePattern validates a primitive type pattern (JEP 507). The
// selector may be a reference type, in which case the test is a run time
// question, or a primitive whose value might convert exactly.
func (ctx *methodCtx) checkPrimitivePattern(pos source.Pos, prim *ast.PrimType, xt ast.Type, p *ast.Param) {
	if p.Name == "_" {
		ctx.errf(pos, "TY-TYP-0092", "a primitive pattern needs a name to bind the value to")
	}
	if xt == nil || ast.IsError(xt) {
		return
	}
	if sv, ok := xt.(*ast.PrimType); ok {
		if sv.Kind == ast.Boolean || prim.Kind == ast.Boolean {
			if sv.Kind != prim.Kind {
				ctx.errf(pos, "TY-TYP-0093", "boolean cannot be converted to %s", prim)
			}
		}
		return
	}
	if !ast.IsRef(xt) {
		ctx.errf(pos, "TY-TYP-0094", "primitive pattern %s needs a boxed value, found %s", prim, xt)
	}
}

// recordOf returns the record class behind a type, if any.
func recordOf(t ast.Type) *ast.Class {
	ct, ok := t.(*ast.ClassType)
	if !ok || ct.Class.Kind != ast.KindRecord {
		return nil
	}
	return ct.Class
}

// propertyOf returns the property behind an assignable expression.
func (ctx *methodCtx) propertyOf(x ast.Expr) *ast.Field {
	var ref any
	switch v := x.(type) {
	case *ast.Ident:
		ref = v.Ref
	case *ast.Select:
		ref = v.Ref
	}
	if f, ok := ref.(*ast.Field); ok && f.IsProp {
		return f
	}
	return nil
}

// lowerPropAssign rewrites `x.p = v` (and compound forms) into setter calls.
func (ctx *methodCtx) lowerPropAssign(v *ast.Assign, f *ast.Field) {
	if f.Setter == nil {
		ctx.errf(v.Pos, "TY-PROP-0007", "property %s has no setter", f.Name)
		v.SetType(f.Type)
		return
	}
	ctx.checkPropAccess(v.Pos, f, f.Setter)
	if v.Op != "=" {
		// a compound assignment reads the property as well as writing it
		ctx.checkPropAccess(v.Pos, f, f.Getter)
	}
	ctx.checkExpr(v.Y, f.Type)
	if v.Op != "=" {
		if f.Getter == nil {
			ctx.errf(v.Pos, "TY-PROP-0008", "property %s needs a getter for compound assignment", f.Name)
			v.SetType(f.Type)
			return
		}
		getCall := ctx.getterCall(v.X, f)
		bin := &ast.Binary{ExprBase: ast.ExprBase{Pos: v.Pos}, Op: v.Op[:len(v.Op)-1], X: getCall, Y: v.Y}
		bin.SetType(f.Type)
		v.Y = bin
	}
	recv := ctx.propReceiver(v.X, f)
	ctx.props[v] = &ast.Call{ExprBase: ast.ExprBase{Pos: v.Pos, T: f.Type}, Recv: recv, Name: f.Setter.Name, Args: []ast.Expr{v.Y}, Method: f.Setter}
	v.SetType(f.Type)
}

func (ctx *methodCtx) propReceiver(x ast.Expr, f *ast.Field) ast.Expr {
	if !f.Mods.Has(ast.ModStatic) {
		if sel, ok := x.(*ast.Select); ok {
			return sel.X
		}
	}
	return nil
}

func (ctx *methodCtx) getterCall(x ast.Expr, f *ast.Field) ast.Expr {
	return &ast.Call{ExprBase: ast.ExprBase{Pos: x.GetPos(), T: f.Type}, Recv: ctx.propReceiver(x, f), Name: f.Getter.Name, Args: []ast.Expr{}, Method: f.Getter}
}

func (ctx *methodCtx) checkCond2(v *ast.Cond, want ast.Type) {
	ctx.checkExpr(v.C, ast.TBoolean)
	ctx.convertTo(v.C, ast.TBoolean)
	ctx.checkExpr(v.X, want)
	ctx.checkExpr(v.Y, want)
	xt, yt := v.X.GetType(), v.Y.GetType()
	if xt == nil || yt == nil {
		v.SetType(ast.ErrorType{})
		return
	}
	// A conditional expression in an assignment or argument position is a
	// poly expression: when both branches fit the target type, that is its type.
	if want != nil && !ast.IsError(want) && !ast.IsPrim(want, ast.Void) {
		if ctx.c.assignableTo(xt, want) && ctx.c.assignableTo(yt, want) {
			v.SetType(want)
			return
		}
	}
	if sameType(xt, yt) {
		v.SetType(xt)
		return
	}
	if isNullType(xt) {
		v.SetType(yt)
		return
	}
	if isNullType(yt) {
		v.SetType(xt)
		return
	}
	// Both branches are converted to the result type, which is their common
	// supertype -- not to each other. Converting each to the other's type makes
	// one of the two directions a downcast, and a conditional whose branches are
	// two subclasses of one class (`f ? null : element`, or `f ? a : new B()`)
	// would be reported as an error for a branch that is perfectly assignable to
	// the result.
	t := ctx.c.lub(xt, yt)
	ctx.convertTo(v.X, t)
	ctx.convertTo(v.Y, t)
	v.SetType(t)
}

func (ctx *methodCtx) checkNewArray(v *ast.NewArray) {
	c := ctx.c
	elem := c.resolveType(ctx.env, v.Elem)
	for i, d := range v.Dims {
		ctx.checkExpr(d, ast.TInt)
		ctx.convertTo(d, ast.TInt)
		if cv := c.constEval(d); cv.ok && cv.i < 0 {
			ctx.errf(d.GetPos(), "TY-TYP-0067", "array dimension must be non-negative")
		}
		_ = i
	}
	levels := len(v.Dims) + v.Extra
	if v.Init != nil {
		// The braces are one array's worth of elements, so the initializer's
		// element type is the created type with one dimension taken off:
		// `new int[][]{{1, 2}}` makes an int[][], and its one element is an
		// int[]. Handing the initializer the whole type instead made every
		// element of a multi-dimensional literal answer to the wrong type --
		// `new int[][]{ new int[]{1, 2} }` was asked for an int.
		init := elem
		for i := 0; i < levels-1; i++ {
			init = &ast.ArrayType{Elem: init}
		}
		v.Init.Elem = init
		ctx.checkArrayInit(v.Init, nil)
	}
	t := elem
	for i := 0; i < levels; i++ {
		t = &ast.ArrayType{Elem: t}
	}
	v.SetType(t)
}

func (ctx *methodCtx) checkArrayInit(v *ast.ArrayInit, want ast.Type) {
	var elem ast.Type
	if want != nil {
		if arr, ok := want.(*ast.ArrayType); ok {
			elem = arr.Elem
		}
	}
	if elem == nil {
		elem = v.Elem
	}
	if elem == nil {
		// infer from first element
		for _, e := range v.Elems {
			ctx.checkExpr(e, nil)
			elem = e.GetType()
			break
		}
	}
	for _, e := range v.Elems {
		if ai, ok := e.(*ast.ArrayInit); ok {
			ai.Elem = elem
			ctx.checkArrayInit(ai, elem)
			continue
		}
		ctx.checkExpr(e, elem)
		if elem != nil {
			ctx.convertTo(e, elem)
		}
	}
	v.Elem = elem
	v.SetType(&ast.ArrayType{Elem: elem})
}

func (ctx *methodCtx) checkNew(v *ast.New, want ast.Type) {
	c := ctx.c
	if v.Outer != nil && v.Type != nil && !strings.Contains(v.Type.Name, ".") {
		// `outer.new Inner()`: the simple name is a member type of the
		// qualifier's class, not a name in the lexical scope
		ctx.checkExpr(v.Outer, nil)
		if qct, ok := c.erasure(v.Outer.GetType()).(*ast.ClassType); ok {
			if nested := qct.Class.Nested[v.Type.Name]; nested != nil {
				v.Type.Name = qct.Class.Full + "." + v.Type.Name
			}
		}
	}
	t := c.resolveType(ctx.env, v.Type)
	if v.Type.Args != nil && len(v.Type.Args) == 0 {
		if dt, ok := t.(*diamondType); ok {
			if ct, ok2 := want.(*ast.ClassType); ok2 && ct.Class == dt.Class {
				t = ct
				v.Type.Resolved = ct
			} else {
				// fall back to erasure
				t = &ast.ClassType{Class: dt.Class}
				for _, tv := range dt.Class.TypeParams {
					t.(*ast.ClassType).Args = append(t.(*ast.ClassType).Args, c.erasure(tv.Bound))
				}
				v.Type.Resolved = t
			}
		}
	}
	ct, ok := c.erasure(t).(*ast.ClassType)
	if !ok {
		ctx.errf(v.Pos, "TY-TYP-0068", "cannot instantiate %s", t)
		v.SetType(ast.ErrorType{})
		return
	}
	cl := ct.Class
	if v.Body == nil {
		if cl.Mods.Has(ast.ModAbstract) || cl.IsInterface() {
			ctx.errf(v.Pos, "TY-TYP-0069", "%s is abstract; cannot be instantiated", cl.Name)
		}
	}
	if v.Body != nil {
		if !cl.IsInterface() && !cl.Mods.Has(ast.ModAbstract) && cl.Mods.Has(ast.ModFinal) {
			ctx.errf(v.Pos, "TY-TYP-0070", "cannot extend final class %s", cl.Name)
		}
		if cl.Kind == ast.KindEnum {
			ctx.errf(v.Pos, "TY-TYP-0070", "cannot subclass enum %s", cl.Name)
		}
	}
	// resolve the constructor against the parameterised type so that type
	// arguments substitute into the constructor signature
	ctorType := ct
	if pt, ok := t.(*ast.ClassType); ok && len(pt.Args) > 0 {
		ctorType = pt
	}
	ctor := c.resolveCtor(ctx, ctorType, v, cl, want)
	_ = ctor
	// outer instance for inner classes
	if cl.Inner && !ctx.inScopeOf(cl) {
		if v.Outer != nil {
			ctx.checkExpr(v.Outer, nil)
		} else if !ctx.inStatic() {
			if findEnclosing(ctx.cl, cl) == nil {
				ctx.errf(v.Pos, "TY-TYP-0071", "an enclosing instance of %s is required", cl.Name)
			}
		} else {
			ctx.errf(v.Pos, "TY-TYP-0071", "an enclosing instance of %s is required", cl.Name)
		}
	} else if cl.Inner && v.Outer != nil {
		ctx.checkExpr(v.Outer, nil)
	}
	if v.Body != nil {
		body := v.Body
		body.Name = cl.Name + "$" + fmt.Sprint(c.anonN[cl])
		c.anonN[cl]++
		sub := c.declareClass(ctx.cl.File, body, ctx.cl)
		sub.Anon = true
		delete(ctx.cl.Nested, body.Name)
		sub.Mods |= ast.ModFinal
		// An anonymous class only carries an enclosing instance when it is
		// created in an instance context (JLS 15.9.5).
		sub.Inner = !ctx.inStatic()
		if cl.IsInterface() {
			sub.Ifaces = append(sub.Ifaces, ct)
		} else {
			sub.Super = ct
			cl.Subclasses = append(cl.Subclasses, sub)
		}
		sub.Resolved = true
		sub.LocalOwner = ctx.m
		// the body of an anonymous class sees the locals in scope where it is
		// written; the ones it actually uses become captured fields
		sub.LocalScopes = ctx.scopes
		c.resolveMembers(sub)
		// The constructor of the anonymous class mirrors the target's
		// signature and forwards to it; drop the synthesized default ctor.
		var kept []*ast.Method
		for _, k := range sub.Ctors {
			if k.SynthKind != "default-ctor" {
				kept = append(kept, k)
			}
		}
		sub.Ctors = kept
		anonCtor := &ast.Method{
			Name: "<init>", Owner: sub, IsCtor: true, Mods: ast.ModPublic,
			Result: ast.TVoid, Pos: v.Pos, SynthKind: "anon-ctor", Forward: ctor,
		}
		if ctor != nil {
			anonCtor.Params = ctor.Params
			anonCtor.ParamNames = ctor.ParamNames
			anonCtor.Varargs = ctor.Varargs
		}
		c.addCtor(sub, anonCtor)
		c.layout(sub)
		c.checkBodies(sub)
		t = &ast.ClassType{Class: sub}
		v.Body = body
		v.Ctor = anonCtor
	} else {
		v.Ctor = ctor
	}
	v.SetType(t)
}

// sameNest reports whether two classes belong to the same nest, that is, they
// are declared within the same top level class. Java grants access to private
// members across a whole nest (JLS 8.8.10).
func sameNest(a, b *ast.Class) bool {
	if a == nil || b == nil {
		return false
	}
	return nestHost(a) == nestHost(b)
}

// nestHost walks up to the outermost enclosing class.
func nestHost(cl *ast.Class) *ast.Class {
	for cl.Outer != nil {
		cl = cl.Outer
	}
	return cl
}

// inScopeOf reports whether cl is a local class whose declaring block we are
// lexically inside. Its enclosing instance is then the current `this`, so
// `new Local()` needs no qualifier -- findClass cannot see it, because a local
// class is deliberately not a member of the enclosing type (JLS 6.3).
func (ctx *methodCtx) inScopeOf(cl *ast.Class) bool {
	for e := ctx.env; e != nil; e = e.parent {
		for _, l := range e.locals {
			if l == cl {
				return true
			}
		}
	}
	return false
}

func findEnclosing(from, target *ast.Class) *ast.Class {
	for cl := from; cl != nil; cl = cl.Outer {
		if c := findClass(cl, target); c != nil {
			return c
		}
	}
	return nil
}

func findClass(scope, target *ast.Class) *ast.Class {
	for cl := scope; cl != nil; cl = cl.Outer {
		if cl == target {
			return cl
		}
	}
	// nested classes of scope
	var found *ast.Class
	var walk func(cl *ast.Class)
	walk = func(cl *ast.Class) {
		if cl == nil || found != nil {
			return
		}
		for _, n := range cl.Nested {
			if n == target {
				found = n
				return
			}
			walk(n)
		}
	}
	walk(scope)
	return found
}

// resolveCtor picks a constructor and checks arguments.
func (c *Checker) resolveCtor(ctx *methodCtx, ct *ast.ClassType, v *ast.New, cl *ast.Class, want ast.Type) *ast.Method {
	if cl.IsInterface() {
		// interfaces have no constructors; an anonymous class implements one
		for _, a := range v.Args {
			ctx.checkExpr(a, nil)
		}
		return nil
	}
	var cands []*ast.Method
	cands = append(cands, cl.Ctors...)
	if len(cands) == 0 {
		cands = append(cands, &ast.Method{Name: "<init>", IsCtor: true, Owner: cl, Mods: ast.ModPublic, Result: ast.TVoid})
	}
	best, score := ctx.pickOverload(ct, cands, v.Args, want)
	if best == nil {
		ctx.errf(v.Pos, "TY-TYP-0072", "no suitable constructor found for %s(%s)", cl.Name, argTypes(v.Args))
		for _, e := range v.Args {
			ctx.checkExpr(e, nil)
		}
		return nil
	}
	ctx.bindArgs(best, score, v.Args, ct)
	return best
}

func argTypes(args []ast.Expr) string {
	var parts []string
	for _, a := range args {
		t := a.GetType()
		if t == nil {
			parts = append(parts, "?")
			continue
		}
		parts = append(parts, t.String())
	}
	return strings.Join(parts, ", ")
}

type ovScore struct {
	total    int
	method   *ast.Method
	instArgs []ast.Type
	targs    map[*ast.TypeVar]ast.Type // inferred method type arguments
	// phase is the applicability phase the candidate was found in
	// (JLS 15.12.2): 1 without boxing or varargs, 2 with boxing, 3 with a
	// variable-arity call. An earlier phase always wins over a later one.
	phase int
	// directVarargs records that a varargs method was called with the array
	// itself rather than with individual arguments
	directVarargs bool
}

// onlyByArity returns the one candidate that can be called with n arguments,
// or nil when the name is still ambiguous at that point. It is what gives an
// argument that cannot type itself -- a lambda, a nested generic call -- a
// target to be checked against before the overload is chosen.
func onlyByArity(cands []*ast.Method, n int) *ast.Method {
	var found *ast.Method
	for _, m := range cands {
		fixed := len(m.Params)
		if m.Varargs {
			if n < fixed-1 {
				continue
			}
		} else if n != fixed {
			continue
		}
		switch {
		case found == nil:
			found = m
		case found == m || sameParams(found, m):
			// the same declaration, reached twice through the type graph
		default:
			return nil
		}
	}
	return found
}

// sameParams reports whether two methods declare the same parameter types. A
// method is reachable by more than one path -- `Stream.collect` through the
// interface and through its implementation -- and a candidate list holding it
// twice is not an overload.
func sameParams(a, b *ast.Method) bool {
	if len(a.Params) != len(b.Params) || a.Varargs != b.Varargs {
		return false
	}
	for i := range a.Params {
		if !sameType(a.Params[i], b.Params[i]) {
			return false
		}
	}
	return true
}

// pickOverload chooses the method to call following the phases of JLS 15.12.2:
// the candidates applicable without boxing or varargs are considered first, then
// those that need boxing, and only if none applies the variable-arity ones. A
// varargs call whose elements all match exactly used to tie with a fixed-arity
// method and win or lose on declaration order alone.
func (ctx *methodCtx) pickOverload(recv *ast.ClassType, cands []*ast.Method, args []ast.Expr, want ast.Type) (*ast.Method, ovScore) {
	// Check arguments once to obtain their types. Lambdas and method references
	// need a target type, so they are checked after the overload is chosen, in
	// bindArgs.
	//
	// A nested generic call needs one too, for the same reason: `id(chained())`
	// where chained() has a type variable of its own leaves it open when the
	// argument is checked with no target, and an open variable is a hole the
	// inner call is then rejected for. That is where the JDK's collectors are
	// used from: `collect(Collectors.toList())` is a `List<T>` because the
	// parameter it is passed to says so, and checking `toList()` with no target
	// settles its T on Object and then rejects the call. The target is taken
	// from the one candidate whose parameter count fits the call, which is the
	// first thing applicability asks anyway -- `Stream.collect` has a
	// one-argument and a three-argument overload, and only one of them can be
	// the target of a one-argument call.
	only := onlyByArity(cands, len(args))
	for i, a := range args {
		if a.GetType() != nil || isLambdaLike(a) {
			continue
		}
		var want ast.Type
		if only != nil && i < len(only.Params) && !only.Varargs {
			want = ctx.c.subst(only.Params[i], ctx.c.recvBind(recv, only))
		}
		ctx.checkExpr(a, want)
	}
	best := ovScore{total: 1 << 30}
	for phase := phaseStrict; phase <= phaseVarargs; phase++ {
		for _, m := range cands {
			if !ctx.accessible(m) {
				continue
			}
			s, ok := ctx.applicable(recv, m, args, want)
			if !ok || s.phase != phase {
				continue
			}
			// Two candidates can fit an argument list equally well and only
			// their parameter types tell them apart, which is the question
			// Java's most-specific rule answers (JLS 15.12.2.5). Declaration
			// order decided it instead, and the answer it gave was wrong
			// wherever the more general method came first: `Stream.of(array)`
			// took of(T) -- one element, the array itself -- because of(T) is
			// declared above of(T...).
			if best.method == nil || s.total < best.total ||
				(s.total == best.total && ctx.c.moreSpecific(s.method, best.method)) {
				best = s
			}
		}
		if best.method != nil {
			ctx.checkInferred(best.method, best, args)
			return best.method, best
		}
	}
	return nil, best
}

// moreSpecific reports whether m1's parameters are strictly more specific than
// m2's, which is what decides between two candidates that fit an argument list
// equally well.
//
// The comparison is over the declared parameter types rather than over what
// they were inferred to be, and that is the whole of it: `of(T...)` and `of(T)`
// both end up taking the argument as a String[], and only their declarations --
// an array of the element type, a single element -- say which of them Java
// picks. A variable-arity method is compared as the array it declares, since
// that is the parameter the array argument binds to.
func (c *Checker) moreSpecific(m1, m2 *ast.Method) bool {
	if len(m1.Params) != len(m2.Params) {
		return false
	}
	strict := false
	for i := range m1.Params {
		a := c.erasure(m1.Params[i])
		b := c.erasure(m2.Params[i])
		if !c.isSubtype(a, b) {
			return false
		}
		if !sameType(a, b) {
			strict = true
		}
	}
	return strict
}

// checkInferred rejects a call whose method type arguments stayed unknown: the
// signature would collapse to void or to a missing type at lowering time, and
// handing that to the C backend ends in an error about generated code rather
// than about the program. An argument that is not a lambda always reports its
// own type, so only a lambda (or method reference) can leave a hole.
func (ctx *methodCtx) checkInferred(m *ast.Method, s ovScore, args []ast.Expr) {
	if len(m.TypeParams) == 0 {
		return
	}
	targs := s.targs
	// A lambda is the one argument that cannot say what it is until its body
	// has been checked, and the overload is picked before that happens: `pick(s
	// -> s.length() * 2)` against `R pick(Fn<? super String, ? extends R>)`
	// still has R open at this point. Checking the lambda against the parameter
	// type now is what lets the body answer -- the functional interface it
	// turns out to be is `Fn<String, Integer>`, and the variable is the
	// wildcard's bound inside that.
	for i, a := range args {
		lam, ok := a.(*ast.Lambda)
		if !ok || lam.GetType() != nil || i >= len(s.instArgs) || s.instArgs[i] == nil {
			continue
		}
		ctx.checkExpr(lam, s.instArgs[i])
		if act := ctx.lambdaActual(lam); act != nil {
			ctx.c.inferTypeArg(s.instArgs[i], act, targs)
		}
	}
	// A call with no arguments and no target says nothing about the method's
	// type variables, and Java answers Object for one that nothing else
	// constrains: `List.of()` is a `List<Object>`, not a mistake. (The JDK
	// spells that one out as its own overload; a variable-arity method reaches
	// the same place with an empty argument list.)
	if len(args) == 0 {
		for _, tv := range m.TypeParams {
			if targs[tv] == nil && mentionsTypeVar(m, tv) {
				targs[tv] = ctx.c.objType
			}
		}
		return
	}
	missing := false
	for _, tv := range m.TypeParams {
		if targs[tv] == nil && mentionsTypeVar(m, tv) {
			missing = true
			break
		}
	}
	if !missing {
		return
	}
	// The call site is what a reader has to look at, and it is not where the
	// method was declared: this used to point at the method's own header, so a
	// failure inside a library call sent the reader to the library instead of
	// to their own line. An argument is on the call's line; failing that, the
	// declaration is all there is.
	pos := m.Pos
	for _, a := range args {
		if a.GetPos().File != nil {
			pos = a.GetPos()
			break
		}
	}
	for _, a := range args {
		if isLambdaLike(a) && a.GetType() == nil {
			pos = a.GetPos()
			break
		}
	}
	ctx.errf(pos, "TY-TYP-0095", "cannot infer the type arguments of %s(%s)", m.Name, argTypes(args))
}

// mentionsTypeVar reports whether tv occurs in the signature of m.
func mentionsTypeVar(m *ast.Method, tv *ast.TypeVar) bool {
	for _, t := range m.Params {
		if mentionsType(t, tv) {
			return true
		}
	}
	return mentionsType(m.Result, tv)
}

func mentionsType(t ast.Type, tv *ast.TypeVar) bool {
	switch v := t.(type) {
	case *ast.TypeVarType:
		return v.Var == tv
	case *ast.ClassType:
		for _, a := range v.Args {
			if mentionsType(a, tv) {
				return true
			}
		}
	case *ast.ArrayType:
		return mentionsType(v.Elem, tv)
	}
	return false
}

// The applicability phases of JLS 15.12.2, in the order they are tried.
const (
	phaseStrict  = 1 // no boxing, no varargs
	phaseBoxing  = 2 // boxing allowed, no varargs
	phaseVarargs = 3 // variable arity
)

func (ctx *methodCtx) accessible(m *ast.Method) bool {
	if m.Owner == nil || m.Owner.Builtin {
		return true
	}
	if m.Mods.Has(ast.ModPublic) {
		return true
	}
	if m.Mods.Has(ast.ModPrivate) {
		return sameNest(m.Owner, ctx.cl)
	}
	if m.Mods.Has(ast.ModProtected) {
		if ctx.cl == m.Owner {
			return true
		}
		return ctx.c.isSubclass(ctx.cl, m.Owner) || samePackage(ctx.cl.File, m.Owner.File)
	}
	// package private
	return samePackage(ctx.cl.File, m.Owner.File)
}

func samePackage(a, b *ast.File) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Package == b.Package
}

// recvBind is the receiver's contribution to a method's type arguments: the
// declaring class's own type parameters bound to the type arguments of the
// receiver, or of the supertype of it that declares the method. `List<String>`
// as the receiver of `stream()` is what makes T of `Stream<T>` be String before
// a single argument is looked at.
func (c *Checker) recvBind(recv *ast.ClassType, m *ast.Method) map[*ast.TypeVar]ast.Type {
	bind := map[*ast.TypeVar]ast.Type{}
	if m.Owner == nil || len(m.Owner.TypeParams) == 0 || recv == nil {
		return bind
	}
	if sup := c.asSuper(recv, m.Owner); sup != nil {
		return bindings(m.Owner, sup.Args)
	}
	if recv.Class == m.Owner {
		return bindings(m.Owner, recv.Args)
	}
	return bind
}

// applicable reports whether args can be passed to m together with a cost.
// want is the type the call is expected to produce, which is what determines a
// method type argument that no argument can pin down (a lambda has no type of
// its own until it is checked against its target interface).
func (ctx *methodCtx) applicable(recv *ast.ClassType, m *ast.Method, args []ast.Expr, want ast.Type) (ovScore, bool) {
	c := ctx.c
	// substitute type variables from the receiver
	bind := c.recvBind(recv, m)
	params := make([]ast.Type, len(m.Params))
	for i, p := range m.Params {
		params[i] = c.subst(p, bind)
	}
	// method-level type variables: explicit arguments win, otherwise infer
	mbind := map[*ast.TypeVar]ast.Type{}
	for _, tv := range m.TypeParams {
		mbind[tv] = nil
	}
	if len(ctx.pendingTypeArgs) == len(m.TypeParams) && len(m.TypeParams) > 0 {
		for i, tv := range m.TypeParams {
			mbind[tv] = ctx.pendingTypeArgs[i]
		}
	}
	// An argument that is a lambda cannot say which type argument it stands
	// for, so the result the call must produce is matched against the declared
	// result type first: `Box<Integer> c = b.map(s -> len(s))` infers the
	// method's U from the Box<Integer> the caller asked for.
	if want != nil {
		c.inferTypeArg(m.Result, want, mbind)
	}
	n := len(params)
	if m.Varargs {
		n--
	}
	if len(args) < n {
		return ovScore{}, false
	}
	if !m.Varargs && len(args) != len(params) {
		return ovScore{}, false
	}
	// A varargs method also accepts the array itself in place of the arguments.
	// That invocation passes the array explicitly, so it is an ordinary
	// fixed-arity one and takes part in the phases above.
	direct := false
	if m.Varargs && len(args) == len(params) {
		direct, boxed, total := true, false, 0
		for i, a := range args {
			// The array that stands in for the variable arguments is an
			// ordinary argument here, and it is the only thing that can say
			// what the method's type variable is: `asList(String[])` settles
			// T = String, and without this the call was rejected for a type
			// argument that was sitting in the argument list all along.
			if len(mbind) > 0 {
				params[i] = c.inferTypeArg(params[i], a.GetType(), mbind)
			}
			cost, ok := ctx.convCost(a.GetType(), params[i])
			if !ok {
				direct = false
				break
			}
			if cost >= convBoxingCost {
				boxed = true
			}
			total += cost
		}
		if direct {
			return ovScore{method: m, instArgs: params, targs: mbind, total: total,
				phase: phase(boxed), directVarargs: true}, true
		}
	}
	s := ovScore{method: m, instArgs: params}
	boxed := false
	for i, a := range args {
		var pt ast.Type
		switch {
		case i < n:
			pt = params[i]
		case m.Varargs:
			elem := params[len(params)-1]
			if arr, ok := elem.(*ast.ArrayType); ok {
				pt = arr.Elem
			} else {
				pt = elem
			}
		}
		if isLambdaLike(a) && a.GetType() == nil {
			// any functional interface will do; the argument is checked once the
			// overload is known
			if pt != nil {
				// ... but not one whose single abstract method takes a different
				// number of parameters: `Comparator.thenComparing` has a
				// Comparator overload and a Function overload, both of which
				// accept a lambda, and only one of them can take this one.
				target := pt
				if i < n {
					target = c.subst(pt, mbind)
					params[i] = target
				}
				switch arg := a.(type) {
				case *ast.Lambda:
					if !c.lambdaArityFits(arg, target) {
						return ovScore{}, false
					}
				case *ast.MethodRef:
					if !ctx.refArityFits(arg, target) {
						return ovScore{}, false
					}
				}
				s.total += 1
				continue
			}
		}
		if len(mbind) > 0 {
			pt = c.inferTypeArg(pt, a.GetType(), mbind)
			// Only a fixed parameter may be written back. A variable-arity
			// element is not a parameter: `asList(w, w)` has one parameter,
			// `T[]`, and putting the inferred `String[]` into params[0] turned
			// the next element's type into String -- so a call whose every
			// argument is an array was rejected for a method that is right
			// there.
			if i < n {
				params[i] = pt
			}
		}
		cost, ok := ctx.convCost(a.GetType(), pt)
		if !ok {
			if m.Varargs && i >= n && cost == 0 {
				return ovScore{}, false
			}
			return ovScore{}, false
		}
		if cost >= convBoxingCost {
			boxed = true
		}
		s.total += cost
	}
	// the variable-arity phase: the arguments are collected into a fresh array
	if m.Varargs && !direct {
		s.phase = phaseVarargs
	} else {
		s.phase = phase(boxed)
	}
	s.targs = mbind
	return s, true
}

// phase maps a candidate that avoids varargs to its applicability phase.
func phase(boxed bool) int {
	if boxed {
		return phaseBoxing
	}
	return phaseStrict
}

// assignableTo reports whether a value of type t may be used where target is
// expected, without reporting diagnostics.
func (c *Checker) assignableTo(t, target ast.Type) bool {
	if t == nil || target == nil || ast.IsError(t) || ast.IsError(target) {
		return true
	}
	if sameType(t, target) {
		return true
	}
	if _, ok := t.(ast.NullType); ok {
		return ast.IsRef(target)
	}
	if p, ok := t.(*ast.PrimType); ok {
		if q, ok2 := target.(*ast.PrimType); ok2 {
			return widening(p, q)
		}
		if _, ok2 := c.unboxed(target); ok2 {
			return true
		}
		// Boxing followed by a widening reference conversion (JLS 5.3): an int
		// fits a Number parameter, because it boxes to Integer and Integer is a
		// Number. The Object case is the same rule with Object at the top.
		if box := c.b.Boxes[p.Kind]; box != nil {
			return c.isSubtype(&ast.ClassType{Class: box}, target)
		}
		return false
	}
	if q, ok := target.(*ast.PrimType); ok {
		if bp, ok2 := c.unboxed(t); ok2 {
			return widening(bp, q)
		}
		return false
	}
	if _, ok := t.(*ast.ArrayType); ok {
		if ct, ok2 := target.(*ast.ClassType); ok2 {
			switch ct.Class.Special {
			case "Object", "Cloneable", "Serializable":
				return true
			}
			return false
		}
	}
	return c.isSubtype(t, target)
}

// isLambdaLike reports whether an expression needs a target type.
func isLambdaLike(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Lambda, *ast.MethodRef:
		return true
	}
	return false
}

// lambdaArityFits reports whether a lambda's parameter count matches the single
// abstract method of the functional interface it is being passed as, which is
// part of what makes a lambda congruent with that interface (JLS 15.27.3).
// Nothing is decided when the target is not a class type or has no single
// abstract method: the argument check reports those.
func (c *Checker) lambdaArityFits(lam *ast.Lambda, want ast.Type) bool {
	ct, ok := want.(*ast.ClassType)
	if !ok || ct.Class == nil || !ct.Class.IsInterface() {
		return true
	}
	sam := c.singleAbstract(ct)
	if sam == nil {
		return true
	}
	return len(lam.Params) == len(sam.Params)
}

// refArityFits reports whether a method reference can stand for the functional
// interface in want: the one abstract method's parameter count has to be the
// number the referenced method takes, plus one when the reference is unbound
// (`String::length` takes the receiver as the first parameter, `s::length` does
// not). JLS 15.28.2 asks the same question when it decides whether a method
// reference is congruent with a function type, and it is what tells
// `thenComparing(Comparator)` from `thenComparing(Function)` when the argument
// is a reference rather than a lambda.
//
// Nothing is decided when the qualifier is not a type this checker can name:
// the reference check reports that on its own.
func (ctx *methodCtx) refArityFits(mr *ast.MethodRef, want ast.Type) bool {
	c := ctx.c
	ct, ok := want.(*ast.ClassType)
	if !ok || ct.Class == nil || !ct.Class.IsInterface() {
		return true
	}
	sam := c.singleAbstract(ct)
	if sam == nil {
		return true
	}
	n := len(sam.Params)
	var recvType ast.Type
	switch {
	case mr.TypeX != nil:
		recvType = c.resolveType(ctx.env, mr.TypeX)
	case mr.X != nil:
		if mr.X.GetType() == nil {
			ctx.checkExpr(mr.X, nil)
		}
		recvType = mr.X.GetType()
	}
	rt := ctx.recvClassForRef(recvType)
	if rt == nil {
		return true
	}
	if mr.Name == "new" {
		for _, ctor := range rt.Class.Ctors {
			if len(ctor.Params) == n {
				return true
			}
		}
		return false
	}
	all := c.methodsFor(rt, mr.Name)
	if len(all) == 0 {
		return true
	}
	for _, m := range all {
		if len(m.Params) == n || (!m.IsStatic() && len(m.Params)+1 == n) {
			return true
		}
	}
	return false
}

func (c *Checker) inferTypeArg(param, arg ast.Type, bind map[*ast.TypeVar]ast.Type) ast.Type {
	if param == nil || arg == nil {
		return param
	}
	switch p := param.(type) {
	case *ast.TypeVarType:
		if _, ok := bind[p.Var]; ok {
			if bind[p.Var] == nil {
				// The argument may be a wildcard rather than a type: the
				// target `Comparator<? super String>` says the same thing about
				// `Comparator.naturalOrder()`'s T as `Comparator<String>` does,
				// and taking the bound is what keeps T a String instead of a
				// wildcard that every later use has to see through.
				if w, isWild := arg.(*ast.WildcardType); isWild && w.Bound != nil {
					bind[p.Var] = c.subst(w.Bound, bind)
					return bind[p.Var]
				}
				// a primitive argument boxes when it becomes a type argument
				if util.IsPrim(arg) {
					bind[p.Var] = c.boxed(arg)
				} else {
					// The whole type, not its erasure: a bound that keeps its
					// arguments is what lets the argument list refine it later
					// (`collect` sees `Set<T>` from the target first, and the
					// collector it is given says which Set it really is).
					// Erasing here threw that away and left the outer call with
					// a variable nothing could settle.
					bind[p.Var] = arg
				}
			}
			return bind[p.Var]
		}
		return param
	case *ast.ClassType:
		if len(p.Args) == 0 {
			return param
		}
		act, ok := arg.(*ast.ClassType)
		if !ok {
			if a, isArr := arg.(*ast.ArrayType); isArr {
				_ = a
				return p
			}
			return p
		}
		sup := c.asSuper(act, p.Class)
		if sup == nil {
			return p
		}
		nt := &ast.ClassType{Class: p.Class}
		for i, a := range p.Args {
			if i < len(sup.Args) {
				nt.Args = append(nt.Args, c.inferTypeArg(a, sup.Args[i], bind))
			} else {
				nt.Args = append(nt.Args, a)
			}
		}
		return nt
	case *ast.ArrayType:
		if at, ok := arg.(*ast.ArrayType); ok {
			return &ast.ArrayType{Elem: c.inferTypeArg(p.Elem, at.Elem, bind)}
		}
		return param
	case *ast.WildcardType:
		if p.Bound == nil {
			return param
		}
		if p.Super {
			// `Comparator<? super T>` against a `Comparator<String>` settles T =
			// String: the argument's type is what the lower bound is asking
			// about, and leaving T open makes the containment check ask whether
			// String is a subtype of a bare variable -- false for every
			// argument. This is the contravariant direction of the same rule
			// the `? extends` case above follows.
			if tv, isVar := p.Bound.(*ast.TypeVarType); isVar && arg != nil {
				if v, bound := bind[tv.Var]; bound && v == nil {
					bind[tv.Var] = arg
					return &ast.WildcardType{Bound: arg, Super: true}
				}
			}
			// T may also already have a value -- from the receiver, or from an
			// explicit witness -- and the check has to see it: `Function<? super
			// T, U>` with T = String must accept a Function<String, Integer>.
			nb := c.subst(p.Bound, bind)
			if nb == p.Bound {
				return param
			}
			return &ast.WildcardType{Bound: nb, Super: true}
		}
		// The free variable is the wildcard's bound: `Fn<? super String,
		// ? extends R>` against `Fn<String, Integer>` settles R = Integer.
		return c.inferTypeArg(p.Bound, arg, bind)
	}
	return param
}

// convBoxingCost is the cheapest conversion cost that boxes or unboxes: a
// candidate that needs one is applicable only from the boxing phase on.
const convBoxingCost = 2

// convCost returns 0 for identity, 1 for widening/upcast, 2 for boxing, 3 for unboxing.
func (ctx *methodCtx) convCost(src, target ast.Type) (int, bool) {
	c := ctx.c
	if src == nil || target == nil {
		return 0, true
	}
	if ast.IsError(src) || ast.IsError(target) {
		return 0, true
	}
	if _, ok := src.(ast.NullType); ok {
		if ast.IsRef(target) {
			return 1, true
		}
		return 0, false
	}
	if sameType(src, target) {
		return 0, true
	}
	if sp, ok := src.(*ast.PrimType); ok {
		if tp, ok2 := target.(*ast.PrimType); ok2 {
			if widening(sp, tp) {
				if sp.Kind == tp.Kind {
					return 0, true
				}
				return 1, true
			}
			return 0, false
		}
		if _, ok2 := c.unboxed(target); ok2 {
			return 2, true
		}
		// Boxing followed by a widening reference conversion (JLS 5.3): an int
		// argument fits a Number parameter, because it boxes to Integer and
		// Integer is a Number. Without this step `new Box(1)` could not find
		// `Box(Number)` -- and an overload set with both `Box(Character)` and
		// `Box(Number)` could only ever match the box class itself, so calls
		// fell to whichever overload happened to be declared first.
		if box := c.b.Boxes[sp.Kind]; box != nil &&
			c.isSubtype(&ast.ClassType{Class: box}, target) {
			return 3, true
		}
		return 0, false
	}
	if tp, ok := target.(*ast.PrimType); ok {
		if bp, ok2 := c.unboxed(src); ok2 {
			if widening(bp, tp) {
				return 3, true
			}
			return 0, false
		}
		return 0, false
	}
	if tv, ok := target.(*ast.TypeVarType); ok {
		if tv.Var.Bound == nil || tv.Var.Bound == c.objType {
			if ast.IsRef(src) {
				return 1, true
			}
			return 0, false
		}
		return ctx.convCost(src, tv.Var.Bound)
	}
	if ast.IsRef(src) && ast.IsRef(target) {
		if c.isSubtype(src, target) {
			return 1, true
		}
		return 0, false
	}
	return 0, false
}

// bindArgs records conversions for the chosen overload.
func (ctx *methodCtx) bindArgs(m *ast.Method, s ovScore, args []ast.Expr, recv *ast.ClassType) {
	if s.directVarargs {
		for i, a := range args {
			if i < len(s.instArgs) {
				ctx.convertTo(a, s.instArgs[i])
			}
		}
		return
	}
	fixed := len(m.Params)
	if m.Varargs {
		fixed--
	}
	for i, a := range args {
		var pt ast.Type
		if i < fixed {
			if i < len(s.instArgs) {
				pt = s.instArgs[i]
			} else if i < len(m.Params) {
				pt = m.Params[i]
			}
		} else if m.Varargs && len(m.Params) > 0 {
			// trailing arguments are elements of the varargs array
			last := len(s.instArgs) - 1
			if last >= 0 && last < len(s.instArgs) {
				if arr, ok := s.instArgs[last].(*ast.ArrayType); ok {
					pt = arr.Elem
				}
			}
			if pt == nil {
				if arr, ok := m.Params[len(m.Params)-1].(*ast.ArrayType); ok {
					pt = arr.Elem
				}
			}
		}
		if pt != nil {
			if isLambdaLike(a) && a.GetType() == nil {
				ctx.checkExpr(a, pt)
			}
			ctx.convertTo(a, pt)
		}
	}
}

// lambdaActual is the functional interface a lambda actually turned out to be.
// Its parameter types are the ones the target handed it, but its result comes
// from its own body -- which is the only place a type variable the target left
// open can be read off. A body that is a block says nothing here, so such a
// lambda reports nothing and leaves the variable to be settled elsewhere.
func (ctx *methodCtx) lambdaActual(lam *ast.Lambda) ast.Type {
	ct, ok := lam.GetType().(*ast.ClassType)
	if !ok || len(ct.Args) != len(lam.Params)+1 {
		return nil
	}
	body, ok := lam.Body.(ast.Expr)
	if !ok || body.GetType() == nil || ast.IsError(body.GetType()) {
		return nil
	}
	res := body.GetType()
	if util.IsPrim(res) {
		res = ctx.c.boxed(res)
	}
	actual := &ast.ClassType{Class: ct.Class}
	for _, p := range lam.Params {
		if p.Sym == nil || p.Sym.Type == nil {
			return nil
		}
		actual.Args = append(actual.Args, p.Sym.Type)
	}
	actual.Args = append(actual.Args, res)
	return actual
}

func (ctx *methodCtx) checkCall(v *ast.Call, want ast.Type) {
	if len(v.TypeArgs) > 0 {
		var ts []ast.Type
		for _, te := range v.TypeArgs {
			ts = append(ts, ctx.c.resolveType(ctx.env, te))
		}
		// Saved and put back rather than cleared: an argument is checked while
		// this call's witness is in hand, and an argument that is itself a call
		// with a witness of its own would otherwise clear the outer one --
		// `pair(f, Builder.<Integer>make())` then bound the lambda's parameter
		// to Object, because by the time the overload was picked the outer
		// witness was gone.
		prev := ctx.pendingTypeArgs
		ctx.pendingTypeArgs = ts
		defer func() { ctx.pendingTypeArgs = prev }()
	}
	if v.ThisCtor {
		ctx.checkThisCtor(v)
		return
	}
	if v.Qual != "" {
		ctx.checkQualifiedSuper(v)
		return
	}
	var rt ast.Type
	if v.Recv != nil {
		// The receiver of a chained call is checked with the call's own target:
		// the type a method returns is often the receiver's own type parameter,
		// so `Comparator.comparing(f).thenComparing(g)` standing where a
		// `Comparator<? super String>` is wanted is what tells `comparing` that
		// its T is String (JLS 18.5.2 reads the same constraint off the whole
		// expression). A receiver whose type does not share a type variable
		// with the result is unaffected: nothing matches and nothing binds.
		var recvWant ast.Type
		if _, isCall := v.Recv.(*ast.Call); isCall {
			recvWant = want
		}
		ctx.checkExpr(v.Recv, recvWant)
		rt = v.Recv.GetType()
	}
	for _, a := range v.Args {
		// An argument that is itself a call is left to the overload choice
		// below, which checks it with the parameter type as its target: that is
		// what settles a type variable only the parameter can determine, as in
		// `collect(Collectors.toList())`.
		_, nested := a.(*ast.Call)
		if a.GetType() == nil && !isLambdaLike(a) && !nested {
			ctx.checkExpr(a, nil)
		}
	}
	v.RecvType = rt
	if rt != nil {
		if _, ok := rt.(*ast.ArrayType); ok {
			if ctx.checkArrayCall(v, rt) {
				return
			}
			// not an array specific method: fall through, checkArrayObjCall
			// resolves the Object methods an array inherits
		}
	}
	if rt == nil {
		ctx.checkUnqualifiedCall(v, want)
		return
	}
	ctx.checkMethodCall(v, rt, want)
}

// helperHasMethod reports whether a helper class declares a method of that
// name, which is what decides that an unqualified call belongs to it.
func helperHasMethod(ctx *methodCtx, cl *ast.Class, name string) bool {
	for _, m := range ctx.methodsOf(cl, name) {
		if !m.Mods.Has(ast.ModPrivate) {
			return true
		}
	}
	return false
}

func (ctx *methodCtx) checkArrayCall(v *ast.Call, rt ast.Type) bool {
	switch v.Name {
	case "clone":
		if len(v.Args) != 0 {
			ctx.errf(v.Pos, "TY-TYP-0073", "array clone takes no arguments")
		}
		v.SetType(rt)
		return true
	case "toString", "hashCode", "equals":
		// handled as the Object methods an array inherits; leave the type to
		// the caller instead of failing here
		return false
	}
	return false
}

// checkQualifiedSuper resolves `Interface.super.method(...)`, which binds
// statically to that interface's default implementation.
func (ctx *methodCtx) checkQualifiedSuper(v *ast.Call) {
	c := ctx.c
	iface := c.lookupClassName(ctx.env, v.Qual)
	if iface == nil || !iface.IsInterface() {
		ctx.errf(v.Pos, "TY-TYP-0090", "%s does not name a super interface", v.Qual)
		v.SetType(ast.ErrorType{})
		return
	}
	implements := false
	c.eachInterface(ctx.cl, func(i *ast.Class) bool {
		if i == iface {
			implements = true
			return false
		}
		return true
	})
	if !implements {
		ctx.errf(v.Pos, "TY-TYP-0091", "%s is not a super interface of %s", iface.Name, ctx.cl.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	recv := &ast.ClassType{Class: iface, Args: typeVarArgs(iface)}
	m, sc := ctx.pickOverload(recv, ctx.methodsOf(iface, v.Name), v.Args, nil)
	if m == nil {
		ctx.errf(v.Pos, "TY-TYP-0076", "cannot find method %s(%s) in %s", v.Name, argTypes(v.Args), iface.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	ctx.bindArgs(m, sc, v.Args, recv)
	if sc.directVarargs {
		ctx.c.Direct[v] = true
	}
	v.Method = m
	v.SetType(m.Result)
}

func (ctx *methodCtx) checkThisCtor(v *ast.Call) {
	if ctx.m == nil || !ctx.m.IsCtor {
		// Not "must be the first statement": statements may precede it, which is
		// JEP 513 and what the emitter implements. This fires when there is no
		// constructor around the call at all.
		ctx.errf(v.Pos, "TY-TYP-0074", "this(...) and super(...) may only be called from a constructor")
	}
	if v.Super {
		if ctx.cl.Super == nil {
			ctx.errf(v.Pos, "TY-TYP-0044", "no superclass to call")
			v.SetType(ast.TVoid)
			return
		}
		m, s := ctx.pickOverload(ctx.cl.Super, ctx.cl.Super.Class.Ctors, v.Args, nil)
		if m == nil {
			ctx.errf(v.Pos, "TY-TYP-0072", "no suitable constructor found for %s(%s)", ctx.cl.Super.Class.Name, argTypes(v.Args))
		} else {
			ctx.bindArgs(m, s, v.Args, ctx.cl.Super)
			v.Method = m
		}
	} else {
		m, s := ctx.pickOverload(&ast.ClassType{Class: ctx.cl}, ctx.cl.Ctors, v.Args, nil)
		if m == nil {
			ctx.errf(v.Pos, "TY-TYP-0072", "no suitable constructor found for %s(%s)", ctx.cl.Name, argTypes(v.Args))
		} else {
			// JLS 8.8.7.1: delegating to another constructor is legal, but a
			// cycle of this(...) calls never terminates. The diagnostic fires
			// on whichever delegation closes the cycle.
			if ctx.delegationCloses(m) {
				ctx.errf(v.Pos, "TY-TYP-0075", "recursive constructor invocation")
			}
			ctx.bindArgs(m, s, v.Args, &ast.ClassType{Class: ctx.cl})
			v.Method = m
		}
	}
	v.SetType(ast.TVoid)
}

// delegationCloses reports whether the chain of this(...) calls reachable from
// m comes back to the constructor being checked, either directly or through a
// cycle that never leaves it. Bodies are checked in declaration order, so a
// cycle is first visible from whichever of its constructors is checked last:
// the walk follows the delegation targets resolved so far and stops at the
// first constructor whose own this(...) call has not been checked yet.
func (ctx *methodCtx) delegationCloses(m *ast.Method) bool {
	seen := map[*ast.Method]bool{}
	for m != nil {
		if m == ctx.m || seen[m] {
			return true
		}
		seen[m] = true
		m = thisCtorTarget(m)
	}
	return false
}

// thisCtorTarget returns the constructor that a leading this(...) call in m's
// body delegates to, or nil when m's body does not delegate. A super(...) call
// chains to a different class and is never part of a cycle.
func thisCtorTarget(m *ast.Method) *ast.Method {
	if m == nil {
		return nil
	}
	// a source constructor keeps its body on the declaration, a synthesized
	// one carries it on the method itself
	body := m.Body
	if body == nil && m.Decl != nil {
		body = m.Decl.Body
	}
	if body == nil || len(body.Stmts) == 0 {
		return nil
	}
	es, ok := body.Stmts[0].(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := es.X.(*ast.Call)
	if !ok || !call.ThisCtor || call.Super {
		return nil
	}
	return call.Method
}

func (ctx *methodCtx) checkUnqualifiedCall(v *ast.Call, want ast.Type) {
	// @Helper local classes come first: Lombok's rule is that *any* unqualified
	// call below the declaration whose name matches one of the helper's methods
	// is a call to the helper, and an argument list that does not fit is an
	// error rather than a fall back to some other method of that name.
	for i := len(ctx.helpers) - 1; i >= 0; i-- {
		for _, h := range ctx.helpers[i] {
			if !helperHasMethod(ctx, h.cl, v.Name) {
				continue
			}
			ct := &ast.ClassType{Class: h.cl, Args: typeVarArgs(h.cl)}
			v.Recv = &ast.Ident{ExprBase: ast.ExprBase{Pos: v.Pos, T: ct}, Name: h.inst.Name, Ref: h.inst}
			v.RecvType = ct
			ctx.checkMethodCall(v, ct, want)
			return
		}
	}
	// methods of the enclosing class chain
	recv := &ast.ClassType{Class: ctx.cl, Args: typeVarArgs(ctx.cl)}
	if m, s := ctx.pickOverload(recv, ctx.methodsOf(ctx.cl, v.Name), v.Args, want); m != nil {
		ctx.bindArgs(m, s, v.Args, recv)
		if s.directVarargs {
			ctx.c.Direct[v] = true
		}
		v.Method = m
		v.Static = m.IsStatic()
		// An unqualified instance call runs on the enclosing instance, which
		// inside a lambda is the instance the lambda was created in and not the
		// closure object the body is compiled into. Naming that instance as the
		// receiver is what makes the closure carry it and what keeps the call
		// from dispatching back into the lambda's own method.
		if !m.IsStatic() && ctx.lambda != nil {
			if ctx.thisUse(v.Pos, "method "+callSignature(m)) {
				v.Recv = ctx.implicitThis(v.Pos)
			}
		}
		v.SetType(ctx.c.wildToBound(ctx.c.subst(m.Result, s.targs)))
		return
	}
	// static imports
	for _, m := range ctx.staticMethods[v.Name] {
		if s, ok := ctx.applicable(recv, m, v.Args, want); ok {
			ctx.bindArgs(m, s, v.Args, nil)
			if s.directVarargs {
				ctx.c.Direct[v] = true
			}
			v.Method = m
			v.Static = true
			v.SetType(ctx.c.wildToBound(ctx.c.subst(m.Result, s.targs)))
			return
		}
	}
	// enclosing class (inner class calling outer method)
	for cl := ctx.cl.Outer; cl != nil; cl = cl.Outer {
		orecv := &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}
		if m, s := ctx.pickOverload(orecv, ctx.methodsOf(cl, v.Name), v.Args, want); m != nil {
			ctx.bindArgs(m, s, v.Args, orecv)
			if s.directVarargs {
				ctx.c.Direct[v] = true
			}
			v.Method = m
			v.Static = m.IsStatic()
			v.Recv = &ast.This{ExprBase: ast.ExprBase{Pos: v.Pos, T: orecv}, Qual: cl.Full, Var: ctx.thisVar(cl)}
			v.SetType(m.Result)
			return
		}
	}
	// Compact source files implicitly import java.io.IO, so bare
	// print/println/readln calls resolve there (JEP 512).
	if ctx.cl != nil && ctx.cl.Decl != nil && ctx.cl.Decl.Implicit {
		if io := ctx.c.programClass("IO"); io != nil {
			recv := &ast.ClassType{Class: io}
			if m, s := ctx.pickOverload(recv, ctx.methodsOf(io, v.Name), v.Args, want); m != nil {
				ctx.bindArgs(m, s, v.Args, recv)
				v.Method = m
				v.Static = true
				v.SetType(m.Result)
				return
			}
		}
	}
	ctx.errf(v.Pos, "TY-TYP-0076", "cannot find method %s(%s)", v.Name, argTypes(v.Args))
	v.SetType(ast.ErrorType{})
}

// implicitThis is the receiver an unqualified instance member access runs on:
// the instance the enclosing method was called on, or, from inside a lambda,
// the instance the lambda was created in (JLS 15.27.2).
func (ctx *methodCtx) implicitThis(pos source.Pos) ast.Expr {
	return &ast.This{ExprBase: ast.ExprBase{Pos: pos, T: &ast.ClassType{Class: ctx.cl, Args: typeVarArgs(ctx.cl)}},
		Var: ctx.thisVar(ctx.cl)}
}

// callSignature renders a method the way a diagnostic names it: "apply(int)".
func callSignature(m *ast.Method) string {
	var parts []string
	for _, p := range m.Params {
		if p == nil {
			parts = append(parts, "?")
			continue
		}
		parts = append(parts, p.String())
	}
	return m.Name + "(" + strings.Join(parts, ", ") + ")"
}

func (ctx *methodCtx) thisVar(cl *ast.Class) *ast.Var {
	if ctx.m != nil && ctx.m.ThisVar == nil {
		ctx.m.ThisVar = &ast.Var{Name: "this", Type: &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, ID: -1}
	}
	if ctx.m != nil {
		return ctx.m.ThisVar
	}
	return nil
}

func (ctx *methodCtx) methodsOf(cl *ast.Class, name string) []*ast.Method {
	var out []*ast.Method
	for k := cl; k != nil; {
		out = append(out, k.Methods[name]...)
		if !k.Resolved || k.Super == nil {
			break
		}
		k = k.Super.Class
	}
	ctx.c.eachInterface(cl, func(i *ast.Class) bool {
		out = append(out, i.Methods[name]...)
		return true
	})
	return out
}

func (ctx *methodCtx) checkMethodCall(v *ast.Call, rt ast.Type, want ast.Type) {
	// A type name receiver: static call or nested class field
	if cl := ctx.typeOf(v.Recv); cl != nil {
		if m, s := ctx.pickOverload(&ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, ctx.methodsOf(cl, v.Name), v.Args, want); m != nil {
			if !m.IsStatic() {
				ctx.errf(v.Pos, "TY-TYP-0077", "non-static method %s cannot be referenced from a type name", v.Name)
			}
			ctx.bindArgs(m, s, v.Args, &ast.ClassType{Class: cl})
			if s.directVarargs {
				ctx.c.Direct[v] = true
			}
			v.Method = m
			v.Static = true
			v.SetType(ctx.c.wildToBound(ctx.c.subst(m.Result, s.targs)))
			return
		}
		ctx.errf(v.Pos, "TY-TYP-0076", "cannot find method %s(%s) in %s", v.Name, argTypes(v.Args), cl.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	// auto-dereference: obj.field.m()
	ctx.derefFields(v)
	rt = v.Recv.GetType()
	recvCT := ctx.recvClass(rt)
	if recvCT == nil {
		if arr, ok := rt.(*ast.ArrayType); ok {
			if ctx.checkArrayObjCall(v, arr) {
				return
			}
		}
		if !ast.IsError(rt) && rt != nil {
			ctx.errf(v.Pos, "TY-TYP-0078", "cannot invoke %s on %s", v.Name, rt)
		}
		v.SetType(ast.ErrorType{})
		return
	}
	cands := ctx.c.methodsFor(recvCT, v.Name)
	if len(cands) == 0 {
		// an intersection bound reaches past the erasure: try the other bounds
		// before giving up
		for _, alt := range ctx.recvClasses(rt) {
			if alt.Class == recvCT.Class {
				continue
			}
			if more := ctx.c.methodsFor(alt, v.Name); len(more) > 0 {
				cands, recvCT = more, alt
				break
			}
		}
	}
	if len(cands) == 0 {
		if ctx.tryExtensionMethod(v, rt) {
			return
		}
		ctx.errf(v.Pos, "TY-TYP-0076", "cannot find method %s(%s) in %s", v.Name, argTypes(v.Args), recvCT.Class.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	m, s := ctx.pickOverload(recvCT, cands, v.Args, want)
	if m == nil {
		ctx.errf(v.Pos, "TY-TYP-0076", "cannot find method %s(%s) in %s", v.Name, argTypes(v.Args), recvCT.Class.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	ctx.bindArgs(m, s, v.Args, recvCT)
	v.Method = m
	v.Static = m.IsStatic()
	if s.directVarargs {
		ctx.c.Direct[v] = true
	}
	res := ctx.c.subst(m.Result, s.targs)
	if len(m.Owner.TypeParams) > 0 {
		if sup := ctx.c.asSuper(recvCT, m.Owner); sup != nil {
			res = ctx.c.subst(res, bindings(m.Owner, sup.Args))
		}
	}
	// after the receiver's arguments are in, not before: `E get(int)` on a
	// `List<? extends Number>` is only a wildcard once E has been replaced by
	// what the receiver said, and a wildcard is not a type anything can be
	// done with.
	v.SetType(ctx.c.wildToBound(res))
	if !v.Static && !ctx.accessibleInstance(m, rt) {
		ctx.errf(v.Pos, "TY-TYP-0079", "%s has %s access in %s", m.Name, visName(m.Mods), m.Owner.Name)
	}
}

// tryExtensionMethod rewrites `a.foo(b)` into `Extensions.foo(a, b)` for
// classes named in @ExtensionMethod on the enclosing class.
func (ctx *methodCtx) tryExtensionMethod(v *ast.Call, rt ast.Type) bool {
	c := ctx.c
	exts := c.extensions[ctx.cl]
	if len(exts) == 0 || rt == nil {
		return false
	}
	for _, ext := range exts {
		recv := &ast.ClassType{Class: ext}
		args := append([]ast.Expr{v.Recv}, v.Args...)
		if m, s := ctx.pickOverload(recv, ctx.methodsOf(ext, v.Name), args, nil); m != nil {
			ctx.bindArgs(m, s, args, recv)
			v.Static = true
			v.Method = m
			v.Args = args
			v.Recv = nil
			v.SetType(m.Result)
			return true
		}
	}
	return false
}

func (ctx *methodCtx) accessibleInstance(m *ast.Method, rt ast.Type) bool {
	if m.Mods.Has(ast.ModPublic) || m.Owner == nil || m.Owner.Builtin {
		return true
	}
	if m.Mods.Has(ast.ModPrivate) {
		return sameNest(m.Owner, ctx.cl)
	}
	if m.Mods.Has(ast.ModProtected) {
		return ctx.c.isSubclass(ctx.cl, m.Owner) || samePackage(ctx.cl.File, m.Owner.File) || ctx.cl == m.Owner
	}
	return samePackage(ctx.cl.File, m.Owner.File)
}

func visName(m ast.Mods) string {
	switch {
	case m.Has(ast.ModPrivate):
		return "private"
	case m.Has(ast.ModProtected):
		return "protected"
	}
	return "package"
}

// recvClasses lists every reference type a receiver expression can be looked
// up in. For an intersection bound (JLS 4.4: `<T extends A & B>`) the variable
// has the members of all of its bounds, not only the leftmost one -- that one
// is the erasure, not the whole type. Every other receiver answers with the
// single class recvClass gives.
func (ctx *methodCtx) recvClasses(t ast.Type) []*ast.ClassType {
	if tv, ok := t.(*ast.TypeVarType); ok && len(tv.Var.Bounds) > 1 {
		var out []*ast.ClassType
		seen := map[*ast.Class]bool{}
		for _, b := range tv.Var.Bounds {
			ct, ok := ctx.c.erasure(b).(*ast.ClassType)
			if !ok || ct.Class == nil || seen[ct.Class] {
				continue
			}
			seen[ct.Class] = true
			out = append(out, ct)
		}
		if len(out) > 0 {
			return out
		}
	}
	if ct := ctx.recvClass(t); ct != nil {
		return []*ast.ClassType{ct}
	}
	return nil
}

// recvClass unwraps a type into a receiver class type (boxing primitives).
func (ctx *methodCtx) recvClass(t ast.Type) *ast.ClassType {
	c := ctx.c
	switch v := t.(type) {
	case *ast.ClassType:
		return v
	case *ast.TypeVarType:
		if v.Var.Bound != nil {
			if ct, ok := c.erasure(v.Var.Bound).(*ast.ClassType); ok {
				return ct
			}
		}
		return c.objType
	case *ast.PrimType:
		if cl := c.b.Boxes[v.Kind]; cl != nil {
			return &ast.ClassType{Class: cl}
		}
	case *ast.NullType:
		return c.objType
	case ast.ErrorType:
		return nil
	case *ast.ArrayType:
		return nil
	}
	return nil
}

func (ctx *methodCtx) checkArrayObjCall(v *ast.Call, arr *ast.ArrayType) bool {
	obj := &ast.ClassType{Class: ctx.c.b.Object}
	switch v.Name {
	case "equals":
		if len(v.Args) == 1 {
			ctx.checkExpr(v.Args[0], obj)
			v.SetType(ast.TBoolean)
			return true
		}
	case "hashCode":
		if len(v.Args) == 0 {
			v.SetType(ast.TInt)
			return true
		}
	case "toString":
		if len(v.Args) == 0 {
			v.SetType(ctx.c.strType)
			return true
		}
	case "clone":
		if len(v.Args) == 0 {
			v.SetType(arr)
			return true
		}
	}
	return false
}

// derefFields auto-dereferences property/field receivers: `a.b.c()` where b is a field.
func (ctx *methodCtx) derefFields(v *ast.Call) {
	for {
		sel, ok := v.Recv.(*ast.Select)
		if !ok {
			return
		}
		if sel.Ref == nil {
			return
		}
		return
	}
}

// typeOf returns the class denoted by an expression used as a type name.
func (ctx *methodCtx) typeOf(e ast.Expr) *ast.Class {
	switch v := e.(type) {
	case *ast.Ident:
		if _, isVar := v.Ref.(*ast.Var); isVar {
			return nil
		}
		if _, isVar := v.Ref.(*ast.Field); isVar {
			return nil
		}
		return ctx.c.lookupClassName(ctx.env, v.Name)
	case *ast.Select:
		if v.Ref != nil {
			return nil
		}
		return ctx.c.lookupClassName(ctx.env, exprTypeName(v))
	}
	return nil
}

func exprTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.Select:
		return exprTypeName(v.X) + "." + v.Name
	}
	return ""
}

func (ctx *methodCtx) checkSelect(v *ast.Select, want ast.Type) {
	if v.Ref != nil {
		ctx.setRefType(v, v.Ref)
		return
	}
	// type name?
	if cl := ctx.typeOfName(v); cl != nil {
		return
	}
	ctx.checkExpr(v.X, nil)
	if q := typeQualifier(v.X); q != nil {
		f := ctx.c.findStaticField(q, v.Name)
		if f == nil {
			ctx.errf(v.Pos, "TY-TYP-0080", "cannot find static symbol %s in %s", v.Name, q.Name)
			v.SetType(ast.ErrorType{})
			return
		}
		v.Ref = f
		v.SetType(f.Type)
		ctx.rewriteSelectProp(v, f)
		return
	}
	xt := v.X.GetType()
	if ast.IsError(xt) {
		v.SetType(ast.ErrorType{})
		return
	}
	// array length
	if _, ok := xt.(*ast.ArrayType); ok {
		if v.Name == "length" {
			v.Ref = "length"
			v.SetType(ast.TInt)
			return
		}
		ctx.errf(v.Pos, "TY-TYP-0080", "cannot find symbol %s on array", v.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	ct := ctx.recvClass(xt)
	if ct == nil {
		ctx.errf(v.Pos, "TY-TYP-0080", "cannot find symbol %s on %s", v.Name, xt)
		v.SetType(ast.ErrorType{})
		return
	}
	f := ctx.c.findField(ct, v.Name)
	if f == nil {
		// a field may come from any bound of an intersection, not only the
		// leftmost one
		for _, alt := range ctx.recvClasses(xt) {
			if alt.Class == ct.Class {
				continue
			}
			if f = ctx.c.findField(alt, v.Name); f != nil {
				ct = alt
				break
			}
		}
	}
	if f == nil {
		ctx.errf(v.Pos, "TY-TYP-0080", "cannot find symbol %s in %s", v.Name, ct.Class.Name)
		v.SetType(ast.ErrorType{})
		return
	}
	switch {
	case f.IsProp:
		// A property's storage field is private for every property, so the
		// accessor's own modifiers are what says who may use it. Skipping the
		// check for properties left a private one readable from anywhere: the
		// only modifier on it was the storage's, and that one was exempted. A
		// property on the left of an assignment is checked against its setter
		// instead, where the assignment is lowered.
		if ctx.assignTarget != v {
			ctx.checkPropAccess(v.Pos, f, f.Getter)
		}
	case f.Mods.Has(ast.ModPrivate) && !sameNest(f.Owner, ctx.cl):
		ctx.errf(v.Pos, "TY-TYP-0046", "%s has private access in %s", f.Name, f.Owner.Name)
	}
	if !f.Mods.Has(ast.ModStatic) && ctx.inStatic() && ctx.m != nil && !isTypeReceiver(v.X) {
		// instance access via an expression is fine even in static context
	}
	v.Ref = f
	v.SetType(f.Type)
	ctx.rewriteSelectProp(v, f)
}

// setRefType records the type of an expression whose symbol was resolved
// before the checker reached it (synthesized bodies carry resolved references).
func (ctx *methodCtx) setRefType(e ast.Expr, ref any) {
	switch r := ref.(type) {
	case *ast.Var:
		e.SetType(r.Type)
	case *ast.Field:
		e.SetType(r.Type)
	case *ast.Class:
		e.SetType(&ast.ClassType{Class: r, Args: typeVarArgs(r)})
	case string:
		e.SetType(ast.TInt) // array length
	}
}

// typeQualifier returns the class when e denotes a type name.
func typeQualifier(e ast.Expr) *ast.Class {
	switch v := e.(type) {
	case *ast.Ident:
		if cl, ok := v.Ref.(*ast.Class); ok {
			return cl
		}
	case *ast.Select:
		if cl, ok := v.Ref.(*ast.Class); ok {
			return cl
		}
	}
	return nil
}

// findStaticField looks up a static field, walking the superclass chain.
func (c *Checker) findStaticField(cl *ast.Class, name string) *ast.Field {
	for k := cl; k != nil; {
		if f := k.FieldMap[name]; f != nil && f.Mods.Has(ast.ModStatic) {
			return f
		}
		if k.Super == nil {
			break
		}
		k = k.Super.Class
	}
	return nil
}

func isTypeReceiver(e ast.Expr) bool {
	_, ok := e.(*ast.Ident)
	return ok
}

func (ctx *methodCtx) rewriteSelectProp(v *ast.Select, f *ast.Field) {
	if !f.IsProp {
		return
	}
	if f.Getter == nil {
		ctx.errf(v.Pos, "TY-PROP-0005", "property %s has no getter", f.Name)
		if f.Setter != nil {
			v.SetType(f.Type)
			return
		}
		v.SetType(ast.ErrorType{})
		return
	}
	recv := v.X
	if f.Mods.Has(ast.ModStatic) {
		recv = nil
	}
	ctx.props[v] = &ast.Call{ExprBase: ast.ExprBase{Pos: v.Pos, T: f.Type}, Recv: recv, Name: f.Getter.Name, Args: []ast.Expr{}, Method: f.Getter}
}

func (ctx *methodCtx) typeOfName(v *ast.Select) *ast.Class {
	if v.Ref != nil {
		return nil
	}
	if _, isVar := v.Ref.(*ast.Var); isVar {
		return nil
	}
	cl := ctx.c.lookupClassName(ctx.env, exprTypeName(v))
	if cl == nil {
		return nil
	}
	v.Ref = cl
	v.SetType(&ast.ClassType{Class: cl})
	return cl
}

// ---------------------------------------------------------------- lambdas

func (ctx *methodCtx) checkLambda(lam *ast.Lambda, want ast.Type) {
	c := ctx.c
	if want == nil || ast.IsError(want) {
		ctx.errf(lam.Pos, "TY-TYP-0081", "cannot infer the functional interface for this lambda; declare the target type")
		lam.SetType(ast.ErrorType{})
		return
	}
	ct, ok := want.(*ast.ClassType)
	if !ok || !ct.Class.IsInterface() {
		ctx.errf(lam.Pos, "TY-TYP-0082", "lambda target type must be a functional interface, found %s", want)
		lam.SetType(ast.ErrorType{})
		return
	}
	sam := c.singleAbstract(ct)
	if sam == nil {
		ctx.errf(lam.Pos, "TY-TYP-0083", "%s is not a functional interface", ct.Class.Name)
		lam.SetType(ast.ErrorType{})
		return
	}
	bind := c.samBindings(ct, sam)
	params := make([]ast.Type, len(sam.Params))
	for i, p := range sam.Params {
		params[i] = c.wildToBound(c.subst(p, bind))
	}
	if len(lam.Params) != len(params) {
		ctx.errf(lam.Pos, "TY-TYP-0084", "lambda has %d parameters but %s requires %d", len(lam.Params), sam.Name, len(params))
	}
	lam.Iface = sam
	lam.SetType(ct)
	// synthesize a class implementing ct
	lamID := c.anonN[nil]
	cl := c.newClass("$Lambda"+fmt.Sprint(lamID), "teyru.Lambda$"+fmt.Sprint(lamID), ast.KindClass)
	c.anonN[nil]++
	cl.Anon = true
	cl.Mods = ast.ModFinal
	cl.File = ctx.cl.File
	cl.Resolved = true
	cl.Laidout = false
	cl.Ifaces = []*ast.ClassType{ct}
	cl.LocalOwner = ctx.m
	cl.Lambda = lam
	lam.Class = cl
	m := &ast.Method{Name: sam.Name, Owner: cl, Mods: ast.ModPublic, Result: c.wildToBound(c.subst(sam.Result, bind)), Params: params, Lambda: lam, SynthKind: "lambda"}
	cl.Methods[m.Name] = append(cl.Methods[m.Name], m)
	c.addCtor(cl, &ast.Method{Name: "<init>", IsCtor: true, Owner: cl, Mods: ast.ModPublic, Result: ast.TVoid, SynthKind: "lambda-ctor"})
	// check the body in the lambda's scope
	lam.Outer = ctx.lambda
	lctx := &methodCtx{c: c, cl: ctx.cl, m: m, env: ctx.env, lambda: lam, noThis: !ctx.hasThis()}
	lctx.push()
	outerLocals := ctx.scopes
	lctx.scopes = append(append([]map[string]*ast.Var{}, outerLocals...), map[string]*ast.Var{})
	lam.Captures = lam.PreCaptures
	for i, p := range lam.Params {
		name := p.Name
		var t ast.Type
		if i < len(params) {
			t = params[i]
		}
		if p.Type != nil && p.Type.Name != "var" {
			t = c.resolveType(ctx.env, p.Type)
		}
		if p.Name == "_" {
			p.Unnamed = true
		}
		p.Sym = lctx.declare(name, t, p.Pos)
		m.ParamVars = append(m.ParamVars, p.Sym)
	}
	switch b := lam.Body.(type) {
	case ast.Expr:
		if ast.IsPrim(m.Result, ast.Void) {
			// a void compatible body is a statement expression (JLS 15.27.2)
			lctx.checkExpr(b, nil)
			lam.ExprStmt = true
		} else if _, open := m.Result.(*ast.TypeVarType); open {
			// The target's result type may still be an open variable:
			// `Comparator<String> c = comparing(s -> s)` against
			// `Comparator<T> comparing(Fn<? super T, ? extends U> key)` has U
			// unsettled when the body is checked, and checking a String against
			// a bare `U` rejects it for not being one. The body's own type is
			// what settles U, so it is checked with no target and the caller
			// reads the answer back.
			lctx.checkExpr(b, nil)
		} else {
			lctx.checkExpr(b, m.Result)
			lctx.convertTo(b, m.Result)
		}
	case *ast.Block:
		lctx.checkBlock(b, false)
		if m.Result != ast.TVoid && canCompleteNormally(b) {
			lctx.errf(b.End, "TY-TYP-0020", "missing return statement")
		}
	}
	if lam.CapThis {
		// The instance the lambda was created in is captured like any other
		// variable, under the name code generation reads for it: cap_this. It
		// is registered as a capture rather than as an instance field of the
		// synthetic class so that it travels with the rest of them -- the
		// struct slot, the reference layout the collector walks, and the store
		// the closure does when it is created.
		cv := &ast.Var{Name: "this", Type: &ast.ClassType{Class: ctx.cl, Args: typeVarArgs(ctx.cl)}, ID: -1}
		cl.CapFields[cv] = &ast.Field{Name: "this", Type: cv.Type,
			Mods: ast.ModPrivate | ast.ModFinal, Pos: lam.Pos, Storage: true, Owner: cl}
	}
	// captured variables become fields of the synthetic class
	for _, v := range lam.Captures {
		f := &ast.Field{Name: v.Name, Type: v.Type, Mods: ast.ModPrivate | ast.ModFinal, Pos: v.Pos, Storage: true, Owner: cl}
		cl.Fields = append(cl.Fields, f)
		cl.FieldMap[f.Name] = f
		cl.CapFields[v] = f
	}
	c.addInstanceFields(cl)
	c.layout(cl)
}

// samBindings maps the type variables of the interface that *declares* the
// single abstract method onto what they are at the use site.
//
// `interface Un2<T> extends Function<T, T>` declares nothing itself: its one
// abstract method is Function's apply, and the T in apply's signature belongs
// to Function. Binding only ct's own variables leaves that T unsubstituted, so
// an inferred lambda parameter comes out as the bare variable and `n ->
// n.intValue()` is looked up on Object. Walking the inheritance path first and
// then through ct's own arguments is what makes it Integer.
func (c *Checker) samBindings(ct *ast.ClassType, sam *ast.Method) map[*ast.TypeVar]ast.Type {
	bind := bindings(ct.Class, ct.Args)
	if sam.Owner == nil || sam.Owner == ct.Class {
		return bind
	}
	sup := c.asSuper(ct, sam.Owner)
	if sup == nil {
		return bind
	}
	for k, v := range bindings(sam.Owner, sup.Args) {
		bind[k] = c.subst(v, bind)
	}
	return bind
}

// singleAbstract finds the functional interface method.
func (c *Checker) singleAbstract(ct *ast.ClassType) *ast.Method {
	var found *ast.Method
	c.eachInterface(ct.Class, func(i *ast.Class) bool {
		for _, name := range sortedMethodNames(i) {
			for _, m := range i.Methods[name] {
				if m.IsStatic() || m.Mods.Has(ast.ModPrivate) || !m.Mods.Has(ast.ModAbstract) {
					continue
				}
				if c.objectHas(m) && i.Name != ct.Class.Name {
					continue
				}
				if found != nil && found.Name != m.Name {
					return false
				}
				if found == nil || !sameType(c.erasure(found.Result), c.erasure(m.Result)) {
					found = m
				}
			}
		}
		return true
	})
	return found
}

func (ctx *methodCtx) checkMethodRef(mr *ast.MethodRef, want ast.Type) {
	c := ctx.c
	if want == nil {
		ctx.errf(mr.Pos, "TY-TYP-0081", "cannot infer the functional interface for this method reference")
		mr.SetType(ast.ErrorType{})
		return
	}
	ct, ok := want.(*ast.ClassType)
	if !ok || !ct.Class.IsInterface() {
		ctx.errf(mr.Pos, "TY-TYP-0082", "method reference target type must be a functional interface")
		mr.SetType(ast.ErrorType{})
		return
	}
	sam := c.singleAbstract(ct)
	if sam == nil {
		ctx.errf(mr.Pos, "TY-TYP-0083", "%s is not a functional interface", ct.Class.Name)
		mr.SetType(ast.ErrorType{})
		return
	}
	bind := c.samBindings(ct, sam)
	params := make([]ast.Type, len(sam.Params))
	for i, p := range sam.Params {
		params[i] = c.wildToBound(c.subst(p, bind))
	}
	res := c.wildToBound(c.subst(sam.Result, bind))
	// build an equivalent lambda
	lam := &ast.Lambda{ExprBase: ast.ExprBase{Pos: mr.Pos}, Iface: sam}
	for i := range params {
		lam.Params = append(lam.Params, &ast.Param{Pos: mr.Pos, Name: fmt.Sprintf("p%d", i)})
	}
	callee := mr.Name
	recv := mr.X
	if mr.Name == "new" {
		callee = "<new>"
	}
	// resolve the target method
	var target *ast.Method
	var recvType ast.Type
	if mr.TypeX != nil {
		recvType = c.resolveType(ctx.env, mr.TypeX)
	} else if recv != nil {
		ctx.checkExpr(recv, nil)
		recvType = recv.GetType()
	}
	if callee == "<new>" {
		ct2, ok := c.erasure(recvType).(*ast.ClassType)
		if !ok {
			ctx.errf(mr.Pos, "TY-TYP-0085", "cannot construct %s", recvType)
			mr.SetType(ast.ErrorType{})
			return
		}
		var cands []*ast.Method
		cands = append(cands, ct2.Class.Ctors...)
		m, s := ctx.matchRefParams(ct2, cands, params)
		if m == nil {
			ctx.errf(mr.Pos, "TY-TYP-0072", "no suitable constructor for %s", ct2.Class.Name)
			mr.SetType(ast.ErrorType{})
			return
		}
		_ = s
		target = m
		te := mr.TypeX
		if te == nil {
			te = &ast.TypeExpr{Pos: mr.Pos, Name: ct2.Class.Name, Resolved: ct2}
		}
		te.Resolved = ct2
		lam.Body = &ast.New{ExprBase: ast.ExprBase{Pos: mr.Pos, T: ct2}, Type: te, Ctor: target}
		mr.Lam = lam
		ctx.checkLambda(lam, want)
		mr.SetType(ct)
		return
	}
	// instance method on the receiver type, or a static method / unbound instance method
	if rt := ctx.recvClassForRef(recvType); rt != nil {
		all := c.methodsFor(rt, callee)
		// try bound form first (params as-is)
		if m, _ := ctx.matchRefParams(rt, all, params); m != nil && !m.IsStatic() {
			target = m
		}
		// an unbound reference takes its receiver from the first parameter, so it
		// only exists when the functional interface has at least one parameter
		if target == nil && len(params) > 0 {
			// unbound: first parameter is the receiver
			if m, _ := ctx.matchRefParams(rt, all, params[1:]); m != nil && !m.IsStatic() {
				target = m
			}
		}
		if target == nil {
			if m, _ := ctx.matchRefParams(rt, all, params); m != nil && m.IsStatic() {
				target = m
			}
		}
	}
	if target == nil {
		ctx.errf(mr.Pos, "TY-TYP-0076", "cannot find method %s for this functional interface", callee)
		mr.SetType(ast.ErrorType{})
		return
	}
	// build body: call
	args := make([]ast.Expr, len(lam.Params))
	for i, p := range lam.Params {
		args[i] = &ast.Ident{ExprBase: ast.ExprBase{Pos: mr.Pos}, Name: p.Name}
	}
	var callRecv ast.Expr
	if target.IsStatic() {
		// qualify the call with the declaring class so it resolves from
		// anywhere, not just from the enclosing class
		if o := target.Owner; o != nil {
			callRecv = &ast.Ident{
				ExprBase: ast.ExprBase{Pos: mr.Pos, T: &ast.ClassType{Class: o, Args: typeVarArgs(o)}},
				Name:     o.Name, Ref: o,
			}
		}
	} else if len(args) == len(target.Params) {
		callRecv = ctx.bindRefReceiver(lam, mr, recv, recvType)
	} else {
		callRecv = args[0]
		args = args[1:]
	}
	lam.Body = &ast.Call{ExprBase: ast.ExprBase{Pos: mr.Pos, T: res}, Recv: callRecv, Name: target.Name, Args: args}
	mr.Lam = lam
	ctx.checkLambda(lam, want)
	mr.SetType(ct)
}

// bindRefReceiver returns the receiver an instance method reference calls. A
// simple name is reused directly; any other expression is evaluated once, when
// the method reference is created, and captured by the closure (JLS 15.13.3).
func (ctx *methodCtx) bindRefReceiver(lam *ast.Lambda, mr *ast.MethodRef, recv ast.Expr, recvType ast.Type) ast.Expr {
	if recv == nil {
		return recv
	}
	switch r := recv.(type) {
	case *ast.Ident:
		if _, ok := r.Ref.(*ast.Var); ok {
			return recv
		}
	case *ast.This:
		return recv
	}
	v := &ast.Var{Name: fmt.Sprintf("recv%d", ctx.c.varID), Type: recvType, Owner: ctx.m}
	ctx.c.varID++
	lam.PreCaptures = append(lam.PreCaptures, v)
	lam.RecvVar = v
	lam.RecvExpr = recv
	return &ast.Ident{ExprBase: ast.ExprBase{Pos: mr.Pos, T: recvType}, Name: v.Name, Ref: v}
}

func (ctx *methodCtx) recvClassForRef(t ast.Type) *ast.ClassType {
	if t == nil {
		return nil
	}
	if arr, ok := t.(*ast.ArrayType); ok {
		_ = arr
		return ctx.c.objType
	}
	return ctx.recvClass(t)
}

func (ctx *methodCtx) matchRefParams(recv *ast.ClassType, cands []*ast.Method, params []ast.Type) (*ast.Method, ovScore) {
	best := ovScore{total: 1 << 30}
	var bestM *ast.Method
	for _, m := range cands {
		if !ctx.accessible(m) {
			continue
		}
		n := len(m.Params)
		varargs := m.Varargs
		if varargs {
			n--
			if len(params) < n {
				continue
			}
		} else if len(params) != len(m.Params) {
			continue
		}
		s := ovScore{method: m}
		ok := true
		for i, p := range params {
			var pt ast.Type
			if i < len(m.Params) {
				pt = m.Params[i]
			} else if varargs && len(m.Params) > 0 {
				if arr, isArr := m.Params[len(m.Params)-1].(*ast.ArrayType); isArr {
					pt = arr.Elem
				}
			}
			cost, good := ctx.convCost(p, pt)
			if !good {
				ok = false
				break
			}
			s.total += cost
		}
		if ok && (bestM == nil || s.total < best.total) {
			best = s
			bestM = m
		}
	}
	return bestM, best
}

// ---------------------------------------------------------------- helpers

var _ = source.Pos{}

// methodsFor returns all methods with the given name visible on ct.
func (c *Checker) methodsFor(ct *ast.ClassType, name string) []*ast.Method {
	var out []*ast.Method
	seen := map[*ast.Class]bool{}
	for k := ct.Class; k != nil; {
		out = append(out, k.Methods[name]...)
		if k.Super == nil {
			break
		}
		k = k.Super.Class
	}
	c.eachInterface(ct.Class, func(i *ast.Class) bool {
		if seen[i] {
			return true
		}
		seen[i] = true
		for _, m := range i.Methods[name] {
			if m.Mods.Has(ast.ModAbstract) && c.Implementation(ct.Class, m) != nil {
				continue
			}
			out = append(out, m)
		}
		return true
	})
	return out
}

// findField looks up a field on ct (including inherited).
func (c *Checker) findField(ct *ast.ClassType, name string) *ast.Field {
	bind := bindings(ct.Class, ct.Args)
	for k := ct.Class; k != nil; {
		if f := k.FieldMap[name]; f != nil {
			if len(bind) > 0 {
				nf := *f
				nf.Type = c.subst(f.Type, bind)
				return &nf
			}
			return f
		}
		if !k.Resolved {
			break
		}
		if k.Super == nil {
			break
		}
		bind = compose(bind, bindings(k.Super.Class, k.Super.Args))
		k = k.Super.Class
	}
	return nil
}

func compose(inner, outer map[*ast.TypeVar]ast.Type) map[*ast.TypeVar]ast.Type {
	if len(inner) == 0 {
		return outer
	}
	out := map[*ast.TypeVar]ast.Type{}
	for k, v := range outer {
		out[k] = v
	}
	for k, v := range inner {
		if tv, ok := v.(*ast.TypeVarType); ok {
			if r, ok2 := outer[tv.Var]; ok2 {
				out[k] = r
				continue
			}
		}
		out[k] = v
	}
	return out
}

func walkBlock(b *ast.Block, fn func(ast.Expr)) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		walkStmt(s, fn)
	}
}

func walkStmt(s ast.Stmt, fn func(ast.Expr)) {
	switch v := s.(type) {
	case *ast.Block:
		walkBlock(v, fn)
	case *ast.ExprStmt:
		walkExpr(v.X, fn)
	case *ast.LocalVar:
		for _, d := range v.Vars {
			if d.Init != nil {
				walkExpr(d.Init, fn)
			}
		}
	case *ast.If:
		walkExpr(v.Cond, fn)
		walkStmt(v.Then, fn)
		if v.Else != nil {
			walkStmt(v.Else, fn)
		}
	case *ast.While:
		walkExpr(v.Cond, fn)
		walkStmt(v.Body, fn)
	case *ast.DoWhile:
		walkStmt(v.Body, fn)
		walkExpr(v.Cond, fn)
	case *ast.For:
		for _, i := range v.Init {
			walkStmt(i, fn)
		}
		if v.Cond != nil {
			walkExpr(v.Cond, fn)
		}
		for _, u := range v.Update {
			walkExpr(u, fn)
		}
		walkStmt(v.Body, fn)
	case *ast.ForEach:
		walkExpr(v.X, fn)
		walkStmt(v.Body, fn)
	case *ast.Return:
		if v.X != nil {
			walkExpr(v.X, fn)
		}
	case *ast.Throw:
		walkExpr(v.X, fn)
	case *ast.Try:
		for _, r := range v.Resources {
			walkStmt(r, fn)
		}
		walkBlock(v.Body, fn)
		for _, c := range v.Catches {
			walkBlock(c.Body, fn)
		}
		if v.Finally != nil {
			walkBlock(v.Finally, fn)
		}
	case *ast.Switch:
		walkExpr(v.X, fn)
		for _, cs := range v.Cases {
			for _, l := range cs.Labels {
				walkExpr(l, fn)
			}
			if cs.Guard != nil {
				walkExpr(cs.Guard, fn)
			}
			if cs.ArrowX != nil {
				walkExpr(cs.ArrowX, fn)
			}
			for _, st := range cs.Body {
				walkStmt(st, fn)
			}
		}
	case *ast.Yield:
		walkExpr(v.X, fn)
	case *ast.Labeled:
		walkStmt(v.Body, fn)
	case *ast.Assert:
		walkExpr(v.Cond, fn)
		if v.Msg != nil {
			walkExpr(v.Msg, fn)
		}
	case *ast.Sync:
		walkExpr(v.Lock, fn)
		walkBlock(v.Body, fn)
	}
}

func walkExpr(e ast.Expr, fn func(ast.Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch v := e.(type) {
	case *ast.Unary:
		walkExpr(v.X, fn)
	case *ast.Binary:
		walkExpr(v.X, fn)
		walkExpr(v.Y, fn)
	case *ast.Assign:
		walkExpr(v.X, fn)
		walkExpr(v.Y, fn)
	case *ast.Cond:
		walkExpr(v.C, fn)
		walkExpr(v.X, fn)
		walkExpr(v.Y, fn)
	case *ast.Cast:
		walkExpr(v.X, fn)
	case *ast.Conv:
		walkExpr(v.X, fn)
	case *ast.Call:
		if v.Recv != nil {
			walkExpr(v.Recv, fn)
		}
		for _, a := range v.Args {
			walkExpr(a, fn)
		}
	case *ast.New:
		if v.Outer != nil {
			walkExpr(v.Outer, fn)
		}
		for _, a := range v.Args {
			walkExpr(a, fn)
		}
	case *ast.NewArray:
		for _, d := range v.Dims {
			walkExpr(d, fn)
		}
	case *ast.ArrayInit:
		for _, el := range v.Elems {
			walkExpr(el, fn)
		}
	case *ast.Index:
		walkExpr(v.X, fn)
		walkExpr(v.Index, fn)
	case *ast.Select:
		walkExpr(v.X, fn)
	case *ast.InstanceOf:
		walkExpr(v.X, fn)
	case *ast.Lambda:
		if b, ok := v.Body.(*ast.Block); ok {
			walkBlock(b, fn)
		} else if x, ok := v.Body.(ast.Expr); ok {
			walkExpr(x, fn)
		}
	case *ast.SwitchExpr:
		if v.S != nil {
			walkStmt(v.S, fn)
		}
	}
}
