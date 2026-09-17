package util

// StringUnits answers the length of a string literal in UTF-16 code units and
// whether all of its bytes are ASCII.
//
// A Teyru string is stored as WTF-8 and measured in code units: length() and
// every index in the Java API count those, while the bytes are what the object
// holds. A string the runtime builds measures itself; a literal is a static
// object, so the compiler measures it here and emits the header already filled
// in -- tystr.blen, tystr.ulen and the ASCII bit (tyrt.h).
//
// The bytes are canonical WTF-8 by construction: the lexer builds a literal
// from code units and writes a surrogate pair as the one four-byte sequence it
// is, so one sequence head is one code unit except for the four-byte form,
// which is two.
func StringUnits(s string) (units int, ascii bool) {
	ascii = true
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < 0x80:
			i++
		case c&0xE0 == 0xC0:
			i += 2
			ascii = false
		case c&0xF0 == 0xE0:
			i += 3
			ascii = false
		default:
			i += 4
			units++
			ascii = false
		}
		units++
	}
	return units, ascii
}

// StrFlagASCII is tystr.flags' "every byte is below 0x80" bit (tyrt.h
// TY_SF_ASCII). A literal is emitted with it set or clear and never with the
// breadcrumb bit, which the first index operation sets at run time.
const StrFlagASCII = 1

// StrBreadcrumbs answers how many entries the breadcrumb table of a literal
// with these bytes needs. The runtime computes the same number from the code
// unit count (TY_STR_NBC); an ASCII string needs none, because a code unit
// index and a byte offset are the same number there.
func StrBreadcrumbs(units int, ascii bool) int {
	if ascii {
		return 0
	}
	return units/64 + 1
}
