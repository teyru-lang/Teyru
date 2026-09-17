// Package util holds helpers shared by the front end and the back end.
//
// The front end and the code generator must agree on how a Teyru symbol turns
// into a C identifier and on how a method signature is keyed, so both live
// here rather than being duplicated in each package.
package util

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/teyru-lang/Teyru/internal/ast"
)

// Mangle turns an arbitrary qualified name into a C identifier.
//
// Dots and the `$` of nested and synthetic classes become underscores; every
// other character that is not valid in a C identifier is replaced too, so the
// result is always safe to paste into generated code.
func Mangle(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			b.WriteByte(c)
		case c >= '0' && c <= '9' && i > 0:
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Capitalize applies the JavaBeans rule used for property accessor names: the
// first code point is upper-cased unless the first two are already upper-case
// (so `URL` stays `URL` rather than becoming `URL` via `Url`).
func Capitalize(name string) string {
	if name == "" {
		return name
	}
	r, n := utf8.DecodeRuneInString(name)
	if n < len(name) {
		r2, _ := utf8.DecodeRuneInString(name[n:])
		if unicode.IsUpper(r) && unicode.IsUpper(r2) {
			return name
		}
	}
	return string(unicode.ToUpper(r)) + name[n:]
}

// Descriptor encodes a type as a single character, matching the JVM-ish
// convention the runtime uses for native method names.
func Descriptor(t ast.Type) string {
	switch v := t.(type) {
	case *ast.PrimType:
		return [...]string{"V", "Z", "B", "S", "C", "I", "J", "F", "D"}[v.Kind]
	case *ast.ArrayType:
		return "A"
	case *ast.ClassType:
		return v.Class.Name
	case *ast.TypeVarType:
		return "O"
	}
	return "O"
}

// Signature renders a parameter list as a stable lookup key, for example
// `println(I)`.
func Signature(name string, params []ast.Type) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(Descriptor(p))
	}
	b.WriteByte(')')
	return b.String()
}

// ParamDescriptor is one parameter's component of a key: a primitive keeps its
// single letter and a class its simple name, but an array carries its element's
// descriptor after the `A`.
//
// A bare `A` for every array is not injective, and the compiler builds method
// keys from these -- the native method table's keys and the C symbol of a
// user-declared native method both. With one letter for every array,
// String(char[]) and String(byte[]) share a key and one of the two constructors
// runs the other's helper; `size(int[])` and `size(String[])` were the same
// symbol. `int[]` is `AI` and `int[][]` is `AAI`.
//
// This is the sema package's old nativeParam, moved here because the codegen
// table key and the native symbol have to be the same rule.
func ParamDescriptor(t ast.Type) string {
	if at, ok := t.(*ast.ArrayType); ok {
		return "A" + ParamDescriptor(at.Elem)
	}
	return Descriptor(t)
}

// KeySignature renders the parameter list a method is looked up by: the same as
// Signature, except that an array parameter names its element.
func KeySignature(name string, params []ast.Type) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(ParamDescriptor(p))
	}
	b.WriteByte(')')
	return b.String()
}

// FloatLiteral renders a floating point value as a C literal of the right
// width. The value must keep a fractional or exponent part, because an
// integral spelling such as 10 would turn the surrounding expression into
// integer arithmetic.
func FloatLiteral(f float64, single bool) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if single {
		s = strconv.FormatFloat(f, 'g', -1, 32)
	}
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	if single {
		s += "f"
	}
	return s
}
