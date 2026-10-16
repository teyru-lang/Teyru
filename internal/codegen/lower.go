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

import (
	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/sema"
)

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

// divFold is what a division or a remainder by a *constant* divisor is, which
// Java answers without a call and which C and LLVM each get wrong in one case.
// The two back ends render the answer in their own terms -- a C expression, an
// instruction -- but the answer itself is one piece of code, because it is a
// rule about Java and not about either back end.
//
// The cases that matter, all of them about the most negative value:
//
//   - `x % 1` and `x % -1` are 0. In C `x % -1` is undefined for the most
//     negative x and in LLVM `srem` with -1 is poison, so folding is what keeps
//     the program defined rather than an optimisation.
//   - `x / 1` is x.
//   - `x / -1` is the negation of x *through an unsigned value*, because Java
//     answers MIN / -1 with MIN and a plain negation of MIN is undefined in C
//     and poison in LLVM.
//   - any other constant is a bare divide or remainder: the divisor is not zero,
//     so neither the helper call nor its exception mark is needed.
//   - a constant zero is *not* a fold. It is the case that throws, and the
//     helper is the only thing that can.
type divFold uint8

const (
	divHelper   divFold = iota // not a constant, or a constant zero
	divIdentity                // x / 1 and x % 1
	divNegate                  // x / -1 and x % -1
	divPlain                   // another constant: a bare divide or remainder
)

// foldDiv classifies a divisor the checker folded.
func foldDiv(c int64, ok bool) divFold {
	switch {
	case !ok, c == 0:
		return divHelper
	case c == 1:
		return divIdentity
	case c == -1:
		return divNegate
	}
	return divPlain
}

// intConstOf reports the value of an integer expression the checker folded: a
// literal, a `-` in front of one, or a constant field. Everything else is not
// one, and saying so is what keeps an emitter from guessing. Both back ends ask
// it, because "what counts as a constant" is a rule about Java.
func intConstOf(p *sema.Program, x ast.Expr) (int64, bool) {
	switch v := x.(type) {
	case *ast.Literal:
		switch v.Kind {
		case ast.LitChar, ast.LitInt, ast.LitLong:
			return int64(v.Int), true
		}
	case *ast.Unary:
		if v.Op == "-" {
			if c, ok := intConstOf(p, v.X); ok {
				return -c, true
			}
		}
	}
	if cv := p.ConstInt(x); cv != nil {
		return *cv, true
	}
	return 0, false
}
