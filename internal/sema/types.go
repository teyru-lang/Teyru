package sema

import (
	"sort"

	"github.com/teyru-lang/Teyru/internal/ast"
)

func sortStrings(s []string) { sort.Strings(s) }

func stableSortBy(s []string, less func(a, b string) bool) {
	sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
}

// bindings maps a class's type variables to the arguments of a parameterized supertype.
func bindings(cl *ast.Class, args []ast.Type) map[*ast.TypeVar]ast.Type {
	m := map[*ast.TypeVar]ast.Type{}
	if len(args) != len(cl.TypeParams) {
		return m
	}
	for i, tv := range cl.TypeParams {
		m[tv] = args[i]
	}
	return m
}

// subst replaces type variables using b.
func (c *Checker) subst(t ast.Type, b map[*ast.TypeVar]ast.Type) ast.Type {
	if len(b) == 0 || t == nil {
		return t
	}
	switch v := t.(type) {
	case *ast.TypeVarType:
		// A nil entry is how inference records "still open", not "replace me
		// with nothing": substituting one in erases the variable, and a
		// wildcard whose bound was a variable becomes `? extends nil`, which
		// no inference can read back. Only a settled binding substitutes.
		if r, ok := b[v.Var]; ok && r != nil {
			return r
		}
		return t
	case *ast.ClassType:
		if len(v.Args) == 0 {
			return v
		}
		nt := &ast.ClassType{Class: v.Class}
		changed := false
		for _, a := range v.Args {
			na := c.subst(a, b)
			if na != a {
				changed = true
			}
			nt.Args = append(nt.Args, na)
		}
		if !changed {
			return v
		}
		return nt
	case *ast.ArrayType:
		e := c.subst(v.Elem, b)
		if e == v.Elem {
			return v
		}
		return &ast.ArrayType{Elem: e}
	case *ast.WildcardType:
		if v.Bound == nil {
			return v
		}
		nb := c.subst(v.Bound, b)
		if nb == v.Bound {
			return v
		}
		return &ast.WildcardType{Bound: nb, Super: v.Super}
	}
	return t
}

// wildToBound replaces every wildcard inside t with its bound: `? extends B`
// and `? super B` both become B, and a bare `?` becomes Object, leaving the
// rest of the type alone.
//
// Java gets to the same place through capture conversion. `List<? extends
// Number> xs; xs.get(0)` has type CAP#1, and CAP#1's upper bound is Number, so
// `.intValue()` on it resolves. Without the capture step a wildcard reaches
// member lookup as itself and nothing resolves -- `Fn<? super String, R>` was
// unusable as a lambda target because its parameter type came out as `? super
// String` and `.length()` was looked for on that.
func (c *Checker) wildToBound(t ast.Type) ast.Type {
	switch v := t.(type) {
	case *ast.WildcardType:
		if v.Bound == nil {
			return c.objType
		}
		return c.wildToBound(v.Bound)
	case *ast.ClassType:
		if len(v.Args) == 0 {
			return v
		}
		changed := false
		args := make([]ast.Type, len(v.Args))
		for i, a := range v.Args {
			args[i] = c.wildToBound(a)
			if args[i] != a {
				changed = true
			}
		}
		if !changed {
			return v
		}
		return &ast.ClassType{Class: v.Class, Args: args}
	case *ast.ArrayType:
		e := c.wildToBound(v.Elem)
		if e == v.Elem {
			return v
		}
		return &ast.ArrayType{Elem: e}
	}
	return t
}

// erasure removes generic arguments, as C has no generics.
func (c *Checker) erasure(t ast.Type) ast.Type {
	switch v := t.(type) {
	case nil:
		return ast.ErrorType{}
	case *ast.TypeVarType:
		if v.Var.Bound != nil {
			return c.erasure(v.Var.Bound)
		}
		return c.objType
	case *ast.ClassType:
		if len(v.Args) == 0 {
			return v
		}
		return &ast.ClassType{Class: v.Class}
	case *ast.ArrayType:
		return &ast.ArrayType{Elem: c.erasure(v.Elem)}
	case *ast.WildcardType:
		if v.Bound != nil {
			return c.erasure(v.Bound)
		}
		return c.objType
	}
	return t
}

func sameType(a, b ast.Type) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch x := a.(type) {
	case *ast.PrimType:
		y, ok := b.(*ast.PrimType)
		return ok && x.Kind == y.Kind
	case *ast.ClassType:
		y, ok := b.(*ast.ClassType)
		if !ok || x.Class != y.Class || len(x.Args) != len(y.Args) {
			return false
		}
		for i := range x.Args {
			if !sameType(x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	case *ast.ArrayType:
		y, ok := b.(*ast.ArrayType)
		return ok && sameType(x.Elem, y.Elem)
	case *ast.TypeVarType:
		y, ok := b.(*ast.TypeVarType)
		return ok && x.Var == y.Var
	case ast.NullType:
		_, ok := b.(ast.NullType)
		return ok
	case *ast.WildcardType:
		y, ok := b.(*ast.WildcardType)
		if !ok || x.Super != y.Super {
			return false
		}
		if x.Bound == nil || y.Bound == nil {
			return x.Bound == nil && y.Bound == nil
		}
		return sameType(x.Bound, y.Bound)
	case ast.ErrorType:
		_, ok := b.(ast.ErrorType)
		return ok
	}
	return false
}

// asSuper returns the parameterization of target as seen from ct, or nil.
func (c *Checker) asSuper(ct *ast.ClassType, target *ast.Class) *ast.ClassType {
	if ct.Class == target {
		return ct
	}
	seen := map[*ast.Class]bool{}
	var walk func(cl *ast.Class, bind map[*ast.TypeVar]ast.Type) *ast.ClassType
	walk = func(cl *ast.Class, bind map[*ast.TypeVar]ast.Type) *ast.ClassType {
		if cl == nil || seen[cl] {
			return nil
		}
		seen[cl] = true
		if ct.Class == cl && target == cl {
			return ct
		}
		var supers []*ast.ClassType
		if cl.Super != nil {
			supers = append(supers, c.substCT(cl.Super, bind))
		}
		for _, i := range cl.Ifaces {
			supers = append(supers, c.substCT(i, bind))
		}
		for _, s := range supers {
			if s.Class == target {
				return s
			}
			if r := walk(s.Class, bindings(s.Class, s.Args)); r != nil {
				return r
			}
		}
		return nil
	}
	return walk(ct.Class, bindings(ct.Class, ct.Args))
}

func (c *Checker) substCT(ct *ast.ClassType, b map[*ast.TypeVar]ast.Type) *ast.ClassType {
	if len(b) == 0 || len(ct.Args) == 0 {
		return ct
	}
	nt := &ast.ClassType{Class: ct.Class}
	for _, a := range ct.Args {
		nt.Args = append(nt.Args, c.subst(a, b))
	}
	return nt
}

// subPair is one comparison of the isSubtype path.
type subPair struct{ a, b ast.Type }

// isSubtype reports whether a is a subtype of b (ignoring unchecked warnings).
//
// A comparison that comes back to one already on the path is taken as holding:
// the walk has reached a pair it is in the middle of deciding, so the type
// structure is cyclic, and nothing further along the walk can settle it. The
// shape that produces one is a type variable bound to a class that mentions it
// -- `T extends C<T>` -- where `isSubtype(C<T>, T)` asks for
// `isSubtype(T, C<T>)`, whose argument `typeArgOK` walks back to the pair it
// started from. Reading that as false would reject every use of the bound,
// which is what the cycle is made of; reading it as true is what the variable's
// own declaration says.
func (c *Checker) isSubtype(a, b ast.Type) bool {
	if ast.IsError(a) || ast.IsError(b) {
		return true
	}
	for _, p := range c.subPath {
		if p.a == a && p.b == b {
			return true
		}
	}
	c.subPath = append(c.subPath, subPair{a, b})
	ok := c.isSubtypeWalk(a, b)
	c.subPath = c.subPath[:len(c.subPath)-1]
	return ok
}

func (c *Checker) isSubtypeWalk(a, b ast.Type) bool {
	switch y := b.(type) {
	case *ast.PrimType:
		x, ok := a.(*ast.PrimType)
		return ok && x.Kind == y.Kind
	case *ast.ClassType:
		if y.Class.Special == "Object" {
			return ast.IsRef(a)
		}
		if x, ok := a.(*ast.ClassType); ok {
			if y.Class.Special == "String" && x.Class.Special == "String" {
				return true
			}
			sup := c.asSuper(x, y.Class)
			if sup == nil {
				return false
			}
			if len(y.Args) == 0 || len(sup.Args) == 0 {
				// raw or partially parameterised: unchecked but allowed
				return true
			}
			if len(sup.Args) != len(y.Args) {
				return false
			}
			for i := range y.Args {
				if !c.typeArgOK(sup.Args[i], y.Args[i]) {
					return false
				}
			}
			return true
		}
		if x, ok := a.(*ast.ArrayType); ok {
			switch y.Class.Special {
			case "Object", "Cloneable", "Serializable":
				return true
			}
			if y.Class.Special == "box" {
				return false
			}
			_ = x
			return false
		}
		if tv, ok := a.(*ast.TypeVarType); ok {
			if tv.Var.Bound != nil {
				return c.isSubtype(tv.Var.Bound, b)
			}
			return y.Class == c.b.Object
		}
		if _, ok := a.(ast.NullType); ok {
			return true
		}
		if _, ok := a.(*ast.WildcardType); ok {
			return c.isSubtype(c.erasure(a), b)
		}
		return false
	case *ast.ArrayType:
		switch x := a.(type) {
		case *ast.ArrayType:
			if x.Elem == nil {
				return true
			}
			if _, ok := x.Elem.(*ast.PrimType); ok {
				return sameType(x.Elem, y.Elem)
			}
			if _, ok := y.Elem.(*ast.PrimType); ok {
				return false
			}
			// Arrays of the same class are compatible regardless of type
			// arguments (unchecked, exactly like Java).
			if xe, ok := x.Elem.(*ast.ClassType); ok {
				if ye, ok2 := y.Elem.(*ast.ClassType); ok2 && xe.Class == ye.Class {
					return true
				}
			}
			return c.isSubtype(x.Elem, y.Elem)
		case *ast.TypeVarType:
			if x.Var.Bound != nil {
				return c.isSubtype(x.Var.Bound, b)
			}
			return false
		case ast.NullType:
			return true
		}
		return false
	case *ast.TypeVarType:
		if x, ok := a.(*ast.TypeVarType); ok {
			return x.Var == y.Var || (x.Var.Bound != nil && c.isSubtype(x.Var.Bound, b))
		}
		if y.Var.Bound != nil && y.Var.Bound != c.objType {
			return c.isSubtype(a, y.Var.Bound)
		}
		return y.Var.Bound == nil || y.Var.Bound.String() == "Object"
	case *ast.WildcardType:
		if y.Bound == nil {
			return true
		}
		if y.Super {
			return c.isSubtype(y.Bound, a)
		}
		return c.isSubtype(a, y.Bound)
	}
	return false
}

func (c *Checker) typeArgOK(a, b ast.Type) bool {
	if w, ok := b.(*ast.WildcardType); ok {
		if w.Bound == nil {
			return true
		}
		if w.Super {
			return c.isSubtype(w.Bound, a)
		}
		return c.isSubtype(a, w.Bound)
	}
	return sameType(a, b) || c.isSubtype(a, b)
}

// isCastable reports whether a cast from a to b is permitted.
func (c *Checker) isCastable(a, b ast.Type) bool {
	if ast.IsError(a) || ast.IsError(b) {
		return true
	}
	if c.isSubtype(a, b) || c.isSubtype(b, a) {
		return true
	}
	if ast.IsRef(a) && ast.IsRef(b) {
		return true // both reference types: runtime check
	}
	if _, aok := a.(*ast.PrimType); aok {
		if _, bok := b.(*ast.PrimType); bok {
			return true // numeric conversion
		}
	}
	return false
}

// unboxed returns the primitive type of a boxed class, if any.
func (c *Checker) unboxed(t ast.Type) (*ast.PrimType, bool) {
	ct, ok := t.(*ast.ClassType)
	if !ok {
		return nil, false
	}
	k, ok := c.b.Unbox[ct.Class]
	if !ok {
		return nil, false
	}
	return &ast.PrimType{Kind: k}, true
}

func (c *Checker) boxed(t ast.Type) ast.Type {
	if p, ok := t.(*ast.PrimType); ok {
		if cl := c.b.Boxes[p.Kind]; cl != nil {
			return &ast.ClassType{Class: cl}
		}
	}
	return t
}

// numericPromote returns the binary numeric promotion result of two primitives.
func numericPromote(a, b *ast.PrimType) *ast.PrimType {
	if a.Kind == ast.Double || b.Kind == ast.Double {
		return ast.TDouble
	}
	if a.Kind == ast.Float || b.Kind == ast.Float {
		return ast.TFloat
	}
	if a.Kind == ast.Long || b.Kind == ast.Long {
		return ast.TLong
	}
	return ast.TInt
}

func isBooleanType(t ast.Type) bool { return ast.IsPrim(t, ast.Boolean) }

func isNumericType(t ast.Type) bool {
	p, ok := t.(*ast.PrimType)
	return ok && p.IsNumeric()
}

// constValue evaluates compile-time constants for case labels and array sizes.
type constValue struct {
	kind ast.LitKind
	i    int64
	f    float64
	s    string
	ok   bool
}

func (c *Checker) constEval(e ast.Expr) constValue {
	switch v := e.(type) {
	case *ast.Literal:
		switch v.Kind {
		case ast.LitInt, ast.LitLong, ast.LitChar:
			return constValue{i: int64(v.Int), kind: v.Kind, ok: true}
		case ast.LitFloat, ast.LitDouble:
			return constValue{f: v.Flt, kind: v.Kind, ok: true}
		case ast.LitString:
			return constValue{s: v.Str, kind: ast.LitString, ok: true}
		}
	case *ast.Unary:
		x := c.constEval(v.X)
		if !x.ok {
			return constValue{}
		}
		switch v.Op {
		case "-":
			return constValue{i: -x.i, f: -x.f, kind: x.kind, ok: true}
		case "+":
			return x
		case "~":
			return constValue{i: ^x.i, kind: x.kind, ok: true}
		case "!":
			return constValue{}
		}
	case *ast.Binary:
		a := c.constEval(v.X)
		b := c.constEval(v.Y)
		if !a.ok || !b.ok {
			return constValue{}
		}
		if v.Op == "+" && a.kind == ast.LitString && b.kind == ast.LitString {
			return constValue{s: a.s + b.s, kind: ast.LitString, ok: true}
		}
		if a.kind == ast.LitDouble || a.kind == ast.LitFloat || b.kind == ast.LitDouble || b.kind == ast.LitFloat {
			af, bf := a.f, b.f
			if a.kind != ast.LitDouble && a.kind != ast.LitFloat {
				af = float64(a.i)
			}
			if b.kind != ast.LitDouble && b.kind != ast.LitFloat {
				bf = float64(b.i)
			}
			switch v.Op {
			case "+":
				return constValue{f: af + bf, kind: ast.LitDouble, ok: true}
			case "-":
				return constValue{f: af - bf, kind: ast.LitDouble, ok: true}
			case "*":
				return constValue{f: af * bf, kind: ast.LitDouble, ok: true}
			case "/":
				if bf != 0 {
					return constValue{f: af / bf, kind: ast.LitDouble, ok: true}
				}
			}
			return constValue{}
		}
		// The width of the promoted operand decides everything here. An int
		// result wraps to 32 bits, a shift uses the low 5 bits of its count (the
		// low 6 for a long, JLS 15.19), and a long result keeps the long kind so
		// that the literal is spelled as one. Folding with a fixed 63-bit mask
		// and a fixed int kind gave `1 << 32` as 4294967296 and `-8 >> 33` as -1
		// where Java has 1 and -4, and it reached both static final inlining and
		// switch case labels.
		wide := a.kind == ast.LitLong || b.kind == ast.LitLong
		narrow := func(x int64) constValue {
			if wide {
				return constValue{i: x, kind: ast.LitLong, ok: true}
			}
			return constValue{i: int64(int32(x)), kind: ast.LitInt, ok: true}
		}
		mask := uint(31) /* the low 5 bits of the count address an int */
		if wide {
			mask = 63 /* the low 6 address a long */
		}
		switch v.Op {
		case "+":
			return narrow(a.i + b.i)
		case "-":
			return narrow(a.i - b.i)
		case "*":
			return narrow(a.i * b.i)
		case "/":
			if b.i != 0 {
				return narrow(a.i / b.i)
			}
		case "%":
			if b.i != 0 {
				return narrow(a.i % b.i)
			}
		case "<<":
			return narrow(a.i << (uint(b.i) & mask))
		case ">>":
			return narrow(a.i >> (uint(b.i) & mask))
		case ">>>":
			if wide {
				return narrow(int64(uint64(a.i) >> (uint(b.i) & mask)))
			}
			return narrow(int64(uint32(a.i) >> (uint(b.i) & mask)))
		}
	case *ast.Ident:
		if f, ok := v.Ref.(*ast.Field); ok {
			if f.EnumOrd >= 0 && f.Owner != nil && f.Owner.Kind == ast.KindEnum {
				return constValue{i: int64(f.EnumOrd), kind: ast.LitInt, ok: true}
			}
			if cv, ok := f.ConstVal.(constValue); ok {
				return cv
			}
		}
	case *ast.Select:
		if f, ok := v.Ref.(*ast.Field); ok {
			if f.EnumOrd >= 0 && f.Owner != nil && f.Owner.Kind == ast.KindEnum {
				return constValue{i: int64(f.EnumOrd), kind: ast.LitInt, ok: true}
			}
			if cv, ok := f.ConstVal.(constValue); ok {
				return cv
			}
		}
	}
	return constValue{}
}
