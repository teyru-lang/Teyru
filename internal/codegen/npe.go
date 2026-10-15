package codegen

import (
	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// The helpful NullPointerException (JEP 358, JDK 15 and later).
//
// A NullPointerException that says only "null" tells a reader nothing they did
// not already know: the null was somewhere, and this line. The JDK's message
// names two things -- what was attempted, and what the null was -- and both
// halves are compile-time constants, so the emitter builds the two strings and
// the runtime only formats them (ty_npe_msg). The shapes and the naming rules
// below were measured against JDK 21 rather than recalled;
// tests/programs/t283 is the JDK's own output for the fifteen of them.

// npeCause is the "because ..." half: what the null was, in the JDK's own
// vocabulary. It is a closed set of shapes, and npeCauseOf picks one by looking
// at the expression that produced the null:
//
//	"<localN>"          a local, numbered as javac's bytecode numbers it
//	"<parameterN>"      a parameter, likewise
//	"this"              the receiver of an instance method
//	"<localN>.f"        a field of a local
//	"<localN>[i]"       an element of a local
//	the return value of "Foo.bar()"
//	the null literal
//
// The type is a string rather than an enum because the runtime formats whatever
// arrives, and a shape this emitter does not know is not a wrong sentence: it
// is the "null" the runtime has always printed.
type npeCause string

// npeInvokeSig renders the `invoke "M"` action: the method as the JDK writes it
// in this message, which is `Owner.name(ParamTypes)` with the parameter types
// simple and the owner qualified -- except that the JDK prints java.lang.Object
// and java.lang.String by their simple names and no other class by a simple one.
// That is not a rule about java.lang: measured on JDK 21, java.lang.Integer,
// java.lang.Comparable, java.lang.Runnable, java.lang.Thread and
// java.lang.StringBuilder all print qualified, and only those two print simple.
// It is the one thing the JDK's own text does to a class name.
func (e *Emitter) npeInvokeSig(m *ast.Method) string {
	if m == nil {
		return "invoke \"<unknown>\""
	}
	s := "invoke \"" + npeOwnerName(m.Owner) + "." + m.Name + "("
	for i, p := range m.Params {
		if i > 0 {
			s += ", "
		}
		s += npeTypeName(p)
	}
	return s + ")\""
}

// npeCall is the C expression that throws the helpful NullPointerException:
// `ty_npe_msg(action, cause)`, with both halves as string literals. A site with
// no cause to name passes NULL, and the runtime falls back to the bare "null"
// message that is what it has always printed -- so a shape this emitter has not
// learned to describe is never a wrong sentence.
func (e *Emitter) npeCall(action string, cause npeCause) string {
	a := "NULL"
	if action != "" {
		a = e.cstr(action)
	}
	c := "NULL"
	if cause != "null" && cause != "" {
		c = e.cstr(string(cause))
	}
	return "ty_npe_msg(" + a + ", " + c + ")"
}

// npeField is the action for a field read or write, with the field's own name
// quoted the way the JDK quotes it.
func npeField(verb, name string) string { return verb + " field \"" + name + "\"" }

func npeOwnerName(cl *ast.Class) string {
	if cl == nil {
		return "<unknown>"
	}
	full := util.JavaName(cl.Full)
	if full == "java.lang.Object" || full == "java.lang.String" {
		return simpleNameOf(full)
	}
	return full
}

// npeTypeName is a parameter type as that text writes it. The JDK prints the
// simple name of every parameter type -- `append(String)`, `put(Object, Object)`,
// `compareTo(Object)` -- so a parameter type is a different rule from the owner
// above, and the two short forms above do not apply to it.
func npeTypeName(t ast.Type) string {
	switch v := t.(type) {
	case *ast.ClassType:
		if v == nil || v.Class == nil {
			return "?"
		}
		return simpleNameOf(util.JavaName(v.Class.Full))
	case *ast.ArrayType:
		return npeTypeName(v.Elem) + "[]"
	case *ast.PrimType:
		return primNpeName(v.Kind)
	}
	return "?"
}

func simpleNameOf(fqn string) string {
	if i := lastDot(fqn); i >= 0 {
		return fqn[i+1:]
	}
	return fqn
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// primNpeName is a primitive as the array messages name it. The JDK's wording
// is "int array", and every reference element is "object array" -- the same word
// ArrayStoreException and arraycopy use.
func primNpeName(k ast.PrimKind) string {
	switch k {
	case ast.Boolean:
		return "boolean"
	case ast.Byte:
		return "byte"
	case ast.Char:
		return "char"
	case ast.Short:
		return "short"
	case ast.Int:
		return "int"
	case ast.Long:
		return "long"
	case ast.Float:
		return "float"
	case ast.Double:
		return "double"
	}
	return "object"
}

// npeElemName is what the load and store messages call the array's element.
func npeElemName(elem ast.Type) string {
	if p, ok := elem.(*ast.PrimType); ok {
		return primNpeName(p.Kind)
	}
	return "object"
}

// npeCauseOf renders the null operand of a check as the JDK's text names it.
// A shape the emitter cannot decide falls back to the bare "null" message, so
// an unrecognised site is never a wrong sentence -- it is the one this runtime
// has always printed.
func (e *Emitter) npeCauseOf(x ast.Expr) npeCause {
	if x == nil {
		return "null"
	}
	switch v := x.(type) {
	case *ast.Literal:
		if v.Kind == ast.LitNull {
			return "the null literal"
		}
	case *ast.This:
		return "this"
	case *ast.Ident:
		if r, ok := v.Ref.(*ast.Var); ok {
			return e.npeVarCause(r)
		}
	case *ast.Select:
		// `x.f` -- the read that failed names the field, and the thing it was
		// read from is described in its own turn. This is the shape t283 calls
		// `Cannot read field "s" because "<local0>" is null`: the *inner* read
		// of `n.s` fails, and the cause is the local, not the field.
		if f, ok := v.Ref.(*ast.Field); ok {
			return npeCause(string(e.npeCauseOf(v.X)) + "." + f.Name)
		}
	case *ast.Index:
		// The JDK's own shape puts the quotes around the whole subscripted name
		// -- `<local1>[0]` -- rather than around the local alone, so the inner
		// shape is unquoted first and the outer pair added here.
		inner := string(e.npeCauseOf(v.X))
		if len(inner) > 1 && inner[0] == '"' && inner[len(inner)-1] == '"' {
			inner = inner[1 : len(inner)-1]
		}
		return npeCause("\"" + inner + "[" + indexText(v.Index) + "]\"")
	case *ast.Call:
		return npeCause("the return value of \"" + e.npeCalleeName(v) + "()\"")
	}
	return "null"
}

// npeCalleeName is the `"Foo.bar"` inside a return-value cause. It is the
// *callee*, not the receiver: measured on JDK 21, `retnull().length()` reads
// `the return value of "Npe.retnull()"`, naming the call that produced the null
// and not the one that dereferenced it.
func (e *Emitter) npeCalleeName(c *ast.Call) string {
	if c == nil {
		return "<unknown>"
	}
	if c.Method != nil && c.Method.Owner != nil {
		return npeOwnerName(c.Method.Owner) + "." + c.Name
	}
	return c.Name
}

// indexText is the index an element access names: the constant the JDK saw, or
// a placeholder when the index is computed.
func indexText(x ast.Expr) string {
	if l, ok := x.(*ast.Literal); ok && l.Kind == ast.LitInt {
		return itoa(int(l.Int))
	}
	return "i"
}

// npeVarCause is a local or parameter as the message names it: the bytecode
// slot, prefixed by which of the two it is. The numbering is javac's, measured:
//
//   - a static method numbers its parameters from 0, an instance method's from
//     1 (slot 0 is the receiver), and a local continues from there;
//   - a long or a double takes two slots, so `static void f(long a, long b)`
//     has a at 0 and b at 2 -- measured, not inferred;
//   - a slot is released when the block that declared it ends, so a local in a
//     later block reuses an earlier one's number. That is why two locals in two
//     sibling blocks are both `<local1>` in the JDK's own output.
//
// The layout is computed once per method by npeSlots and cached on the emitter
// by the method's pointer; see npeVarCause's caller.
func (e *Emitter) npeVarCause(r *ast.Var) npeCause {
	if r == nil {
		return "null"
	}
	if r.ID < 0 {
		return "this"
	}
	if r.Owner == nil {
		return "null"
	}
	if s, ok := e.npeSlotFor(r.Owner)[r]; ok {
		if s.param {
			// javac's slot for the first parameter is 0, but the message counts
			// parameters from 1 (measured: the first parameter reads
			// <parameter1>), so the stored slot is not the printed number.
			return npeCause("\"<parameter" + itoa(int(s.n)+1) + ">\"")
		}
		return npeCause("\"<local" + itoa(int(s.n)) + ">\"")
	}
	return "null"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// npeSlot is one variable's javac slot, and whether it is a parameter (which
// the message names differently).
type npeSlot struct {
	n     int32
	param bool
}

// npeSlotFor answers the layout of one method, computing it the first time a
// variable of that method is named. The layouts are kept per method, so a
// method that is emitted later cannot overwrite the one being emitted now.
func (e *Emitter) npeSlotFor(m *ast.Method) map[*ast.Var]npeSlot {
	if e.npeSlots == nil {
		e.npeSlots = map[*ast.Method]map[*ast.Var]npeSlot{}
	}
	if got, ok := e.npeSlots[m]; ok {
		return got
	}
	slots := map[*ast.Var]npeSlot{}
	e.npeSlots[m] = slots
	n := 0
	if !m.IsStatic() {
		// slot 0 is the receiver, which the emitter never has to name because a
		// receiver that is null is not expressible in Teyru.
		n = 1
	}
	for _, p := range m.ParamVars {
		slots[p] = npeSlot{n: int32(n), param: true}
		n += typeSlots(p.Type)
	}
	assignBodySlots(bodyOf(m), n, func(r *ast.Var, slot int) {
		slots[r] = npeSlot{n: int32(slot)}
	})
	return slots
}

// typeSlots is how many bytecode slots a type takes: two for the 64-bit
// primitives, one for everything else.
func typeSlots(t ast.Type) int {
	if p, ok := t.(*ast.PrimType); ok {
		if p.Kind == ast.Long || p.Kind == ast.Double {
			return 2
		}
	}
	return 1
}

// assignBodySlots walks a method body the way javac allocates: a local takes the
// next free slot, and leaving the block that declared it frees that slot for a
// later block. `next` is the first free slot at this point.
func assignBodySlots(b *ast.Block, next int, put func(*ast.Var, int)) int {
	if b == nil {
		return next
	}
	return assignStmts(b.Stmts, next, put)
}

func assignStmts(stmts []ast.Stmt, next int, put func(*ast.Var, int)) int {
	for _, s := range stmts {
		next = assignSlot(s, next, put)
	}
	return next
}

// assignSlot numbers the variables one statement declares and answers the next
// free slot *in the enclosing scope* -- which is the slot it was given for every
// statement that opens a scope, because everything such a statement declares is
// out of scope by the time it ends.
//
// That release is what makes a later block reuse an earlier one's number, and
// it is the rule the JDK's own output shows: in a method whose only parameter
// is `args` at slot 0, every local in every one of a run of sibling try blocks
// reads `<local1>`, because each block's slot is freed when that block ends.
func assignSlot(s ast.Stmt, next int, put func(*ast.Var, int)) int {
	switch v := s.(type) {
	case *ast.LocalVar:
		// The one statement whose variables live in the *enclosing* scope, so
		// this is the only one that advances the running slot.
		n := next
		for _, d := range v.Vars {
			if d == nil || d.Sym == nil {
				continue
			}
			put(d.Sym, n)
			n += typeSlots(d.Sym.Type)
		}
		return n
	case *ast.Block:
		assignBodySlots(v, next, put)
	case *ast.If:
		assignSlot(v.Then, next, put)
		assignSlot(v.Else, next, put)
	case *ast.While:
		assignSlot(v.Body, next, put)
	case *ast.DoWhile:
		assignSlot(v.Body, next, put)
	case *ast.For:
		// The init and the body are separate scopes: a variable the init
		// declares is not visible in the body, and neither outlives the loop.
		assignStmts(v.Init, next, put)
		assignSlot(v.Body, next, put)
	case *ast.ForEach:
		// The loop variable is declared by the loop itself, is in scope for the
		// body, and is released with it.
		if v.Var != nil && v.Var.Sym != nil {
			put(v.Var.Sym, next)
			assignSlot(v.Body, next+typeSlots(v.Var.Sym.Type), put)
			return next
		}
		assignSlot(v.Body, next, put)
	case *ast.Switch:
		assignSwitch(v, next, put)
	case *ast.Sync:
		assignSlot(v.Body, next, put)
	case *ast.Try:
		// Four scopes, each starting from the same slot: the resources, the
		// body, each catch (whose parameter takes the first), and the finally.
		// The JDK frees a resource's slot when the try statement ends, so a name
		// in the body or a catch can take the same number.
		assignStmts(v.Resources, next, put)
		assignBodySlots(v.Body, next, put)
		for _, c := range v.Catches {
			n := next
			if c.Sym != nil {
				put(c.Sym, n)
				n += typeSlots(c.Sym.Type)
			}
			assignBodySlots(c.Body, n, put)
		}
		assignBodySlots(v.Finally, next, put)
	case *ast.ExprStmt:
		assignNested(v.X, next, put)
	case *ast.Return:
		assignNested(v.X, next, put)
	case *ast.Throw:
		assignNested(v.X, next, put)
	case *ast.LocalClass:
		assignLocalClass(v, next, put)
	}
	return next
}

// assignLocalClass numbers the locals of a class declared in a method. Its
// fields are not slots -- they live in the struct -- but the bodies of its
// methods declare locals, and those are the enclosing method's to number.
func assignLocalClass(v *ast.LocalClass, next int, put func(*ast.Var, int)) {
	if v == nil || v.Decl == nil {
		return
	}
	for _, mem := range v.Decl.Members {
		if md, ok := mem.(*ast.MethodDecl); ok {
			assignBodySlots(md.Body, next, put)
		}
	}
}

// assignSlotsInCase numbers the variables a switch introduces. The selector is
// bound by the switch in javac, so the cases continue from after it; Teyru's
// checker keeps the selector in a generated variable, so there is nothing to
// number for it here.
func assignSwitch(v *ast.Switch, next int, put func(*ast.Var, int)) int {
	for _, c := range v.Cases {
		if c == nil {
			continue
		}
		if c.Pattern != nil && c.Pattern.Sym != nil {
			put(c.Pattern.Sym, next)
			next += typeSlots(c.Pattern.Sym.Type)
		}
		next = assignStmts(c.Body, next, put)
		next = assignNested(c.Guard, next, put)
		next = assignNested(c.ArrowX, next, put)
	}
	return next
}

// assignNested walks into an expression for the lambdas and local classes it
// holds: a lambda body is a method of its own, so its variables belong to it
// and are not numbered here, but a local class's fields are the enclosing
// method's to allocate.
func assignNested(x ast.Expr, next int, put func(*ast.Var, int)) int {
	if x == nil {
		return next
	}
	switch v := x.(type) {
	case *ast.Lambda:
		// A lambda body is a method of its own: its variables are that method's,
		// and they were already numbered when the emitter reached the lambda's
		// own body. Nothing is numbered here.
	case *ast.New:
		if v.Body != nil {
			for _, mem := range v.Body.Members {
				if md, ok := mem.(*ast.MethodDecl); ok {
					assignBodySlots(md.Body, next, put)
				}
			}
		}
	}
	return next
}

// npeCauseOfX renders a null operand without needing an Emitter. The slot
// layout is the only part that needs one, and it is memoised on the method's
// owner, so a fresh Emitter answers it: a layout is a pure function of a method
// (see npeSlotFor), which is what makes this safe to call from a throwaway.
func npeCauseOfX(x ast.Expr) npeCause {
	return (&Emitter{}).npeCauseOf(x)
}

// npeInvokeOf renders a method's invoke action without needing an Emitter.
func npeInvokeOf(m *ast.Method) string { return (&Emitter{}).npeInvokeSig(m) }

// arrayMethodAction is the action for a call to one of the methods an array has
// without a method symbol of its own (clone, toString, hashCode, equals). The
// receiver is an array and inherits those from Object, which is where the JDK
// names them from.
func arrayMethodAction(c *ast.Call) string {
	if c == nil {
		return ""
	}
	return npeInvokeOf(&ast.Method{Name: c.Name, Owner: &ast.Class{Full: "java.lang.Object", Name: "Object"}})
}

// callRecvCause is what a call's null receiver was: the receiver the program
// wrote, or nothing for an unqualified call, whose receiver is the `this` the
// method was entered with and cannot be null in Teyru.
func callRecvCause(c *ast.Call) npeCause {
	if c == nil || c.Recv == nil {
		return ""
	}
	return npeCauseOfX(c.Recv)
}
