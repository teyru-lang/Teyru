package codegen

/* The lowering rules both back ends share.
 *
 * A rule is what decides *what* a construct means -- which type an operation is
 * performed in, how far a shift count is masked -- and it has to be one piece of
 * code, because the two back ends disagreeing about a rule is a program that
 * answers differently depending on which one compiled it. That is what
 * scripts/backend-matrix.sh exists to catch, and what W8.2 is about.
 *
 * What lives here is only the rule. How a back end *renders* it stays in that
 * back end: the C back end's shift also has to cast the masked count back to a
 * signed type (a count of unsigned type would drag the shifted operand into
 * unsigned arithmetic under the usual conversions and turn `>>` into a logical
 * shift), and that is a fact about C, not about Java.
 *
 * The rules that are still written twice are listed in scripts/w8-inventory.md
 * with the risk each one carries; this file is where they are going. */

import "github.com/teyru-lang/Teyru/internal/ast"

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

// shiftBits is how many low bits of a shift count Java keeps: JLS 15.19 masks
// the count with 0x1f for an int and 0x3f for a long, so a count that reaches
// the operand's width wraps instead of being undefined (which is what C leaves
// it) or poison (which is what LLVM calls it). Both back ends mask with this one
// number.
//
// It answers for the type the shift is *performed in*, which is the operand type
// after promotion -- an int shift is five bits even when the count was written
// as a byte or a char.
func shiftBits(t ast.Type) int {
	if ast.IsPrim(t, ast.Long) {
		return 6
	}
	return 5
}
