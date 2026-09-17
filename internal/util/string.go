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

// StrConcat joins two WTF-8 strings the way the runtime joins them, which is
// the way Java's `+` joins two strings: a high surrogate ending the first and
// its low one starting the second are one character, so they are written as the
// single four-byte sequence they are and not as the two three-byte sequences
// the halves take alone.
//
// A compile-time fold has to agree with the runtime about this, because byte
// equality is what equals, string switch and the hash built on it use:
// `("\uD83D" + "\uDE00").equals("😀")` is true in Java, and it is true here only
// when the folded literal is the same bytes as the literal it is compared with.
func StrConcat(a, b string) string {
	hi, ok := trailingHighSurrogate(a)
	if !ok {
		return a + b
	}
	lo, ok := leadingLowSurrogate(b)
	if !ok {
		return a + b
	}
	cp := 0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00)
	return a[:len(a)-3] + string(rune(cp)) + b[3:]
}

// trailingHighSurrogate reads the three-byte WTF-8 sequence that ends s, if it
// is one and if it spells a high surrogate.
func trailingHighSurrogate(s string) (rune, bool) {
	if len(s) < 3 {
		return 0, false
	}
	r, ok := wtf8Unit(s[len(s)-3:])
	return r, ok && r >= 0xD800 && r <= 0xDBFF
}

// leadingLowSurrogate reads the three-byte WTF-8 sequence that begins s, if it
// is one and if it spells a low surrogate.
func leadingLowSurrogate(s string) (rune, bool) {
	if len(s) < 3 {
		return 0, false
	}
	r, ok := wtf8Unit(s[:3])
	return r, ok && r >= 0xDC00 && r <= 0xDFFF
}

// wtf8Unit decodes one three-byte sequence of WTF-8, which is the only width a
// surrogate has: a canonical string never spells a pair this way.
func wtf8Unit(s string) (rune, bool) {
	if len(s) != 3 || s[0]&0xF0 != 0xE0 || s[1]&0xC0 != 0x80 || s[2]&0xC0 != 0x80 {
		return 0, false
	}
	return rune(s[0]&0x0F)<<12 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), true
}
