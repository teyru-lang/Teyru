package ast

import (
	"strings"
)

// Type is a resolved Teyru type.
type Type interface {
	String() string
	typeNode()
}

// PrimKind enumerates primitive types.
type PrimKind int

const (
	Void PrimKind = iota
	Boolean
	Byte
	Short
	Char
	Int
	Long
	Float
	Double
)

// PrimType is a primitive (or void) type.
type PrimType struct{ Kind PrimKind }

// Canonical primitive instances.
var (
	TVoid    = &PrimType{Void}
	TBoolean = &PrimType{Boolean}
	TByte    = &PrimType{Byte}
	TShort   = &PrimType{Short}
	TChar    = &PrimType{Char}
	TInt     = &PrimType{Int}
	TLong    = &PrimType{Long}
	TFloat   = &PrimType{Float}
	TDouble  = &PrimType{Double}
)

// PrimByName maps keyword to primitive type.
var PrimByName = map[string]*PrimType{
	"void": TVoid, "boolean": TBoolean, "byte": TByte, "short": TShort, "char": TChar,
	"int": TInt, "long": TLong, "float": TFloat, "double": TDouble,
}

func (p *PrimType) String() string {
	return [...]string{"void", "boolean", "byte", "short", "char", "int", "long", "float", "double"}[p.Kind]
}

// IsNumeric reports whether p participates in arithmetic.
func (p *PrimType) IsNumeric() bool { return p.Kind >= Byte }

// IsIntegral reports whether p is an integral type.
func (p *PrimType) IsIntegral() bool { return p.Kind >= Byte && p.Kind <= Long }

// ClassType is a reference to a class with type arguments.
type ClassType struct {
	Class *Class
	Args  []Type
	// Raw is a class written without type arguments where it declares some:
	// `List x`. The checker fills Args with the bounds so the rest of the
	// compiler has a type to work with, and reflection needs the difference:
	// Java answers the raw Class for a raw declaration and a ParameterizedType
	// for an explicit one, even when the arguments erase to the same bounds.
	Raw bool
}

// A type half-built by inference can reach String() with a nil Class or nil
// argument -- a diagnostic must print something rather than crash the compiler.
// The placeholders below are what those print as.
const (
	unknownClass = "?"
	unknownArg   = "?"
	unknownVar   = "?"
)

func (c *ClassType) String() string {
	name := unknownClass
	if c.Class != nil {
		name = c.Class.Name
	}
	if len(c.Args) == 0 {
		return name
	}
	parts := make([]string, len(c.Args))
	for i, a := range c.Args {
		parts[i] = unknownArg
		if a != nil {
			parts[i] = a.String()
		}
	}
	return name + "<" + strings.Join(parts, ", ") + ">"
}

// ArrayType is T[].
type ArrayType struct{ Elem Type }

func (a *ArrayType) String() string {
	if a.Elem == nil {
		return "?[]"
	}
	return a.Elem.String() + "[]"
}

// TypeVarType references a generic parameter.
type TypeVarType struct{ Var *TypeVar }

func (t *TypeVarType) String() string {
	if t.Var == nil {
		return unknownVar
	}
	return t.Var.Name
}

// NullType is the type of `null`.
type NullType struct{}

func (NullType) String() string { return "null" }

// WildcardType is `?`, `? extends T` or `? super T`.
type WildcardType struct {
	Bound Type // may be nil
	Super bool
}

func (w *WildcardType) String() string {
	if w.Bound == nil {
		return "?"
	}
	if w.Super {
		return "? super " + w.Bound.String()
	}
	return "? extends " + w.Bound.String()
}

// ErrorType marks an unresolved type; it is compatible with everything to avoid cascades.
type ErrorType struct{}

func (ErrorType) String() string { return "<error>" }

func (*PrimType) typeNode()     {}
func (*ClassType) typeNode()    {}
func (*ArrayType) typeNode()    {}
func (*TypeVarType) typeNode()  {}
func (NullType) typeNode()      {}
func (*WildcardType) typeNode() {}
func (ErrorType) typeNode()     {}

// IsRef reports whether t is a reference type.
func IsRef(t Type) bool {
	switch t.(type) {
	case *ClassType, *ArrayType, *TypeVarType, NullType, *WildcardType:
		return true
	}
	return false
}

// IsPrimType reports whether t is a primitive type of any kind. Together with
// IsRef this is the single definition of which types are references: util
// delegates here, because the dependency only runs util -> ast, and a second
// list of reference kinds in util would be free to drift from this one.
func IsPrimType(t Type) bool {
	_, ok := t.(*PrimType)
	return ok
}

// IsPrim reports whether t is a primitive of kind k.
func IsPrim(t Type, k PrimKind) bool {
	p, ok := t.(*PrimType)
	return ok && p.Kind == k
}

// IsError reports whether t is the error type.
func IsError(t Type) bool {
	_, ok := t.(ErrorType)
	return ok
}
