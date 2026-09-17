package main

// The reader for the Unicode data files. Everything the tables hold is read
// from these files and nothing is hand-written: a property that is in a file is
// derived, and a property that is not is a rule with the reason written next to
// it (see javaWhiteSpace and digitValue).

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxCodePoint is the last code point a table covers. Everything above it is
// answered by the rules rather than by the table: the JDK treats an out-of-range
// argument as undefined, which is what the accessors do.
const maxCodePoint = 0x10FFFF

// category is one general category, as a 5-bit value. The numbers are the JDK's
// Character constants (Character.UNASSIGNED is 0, UPPERCASE_LETTER is 1, and so
// on), because Character.getType answers with them and a program compares its
// answer against those constants.
var category = map[string]uint8{
	"Cn": 0, "Lu": 1, "Ll": 2, "Lt": 3, "Lm": 4, "Lo": 5, "Mn": 6, "Me": 7,
	"Mc": 8, "Nd": 9, "Nl": 10, "No": 11, "Zs": 12, "Zl": 13, "Zp": 14,
	"Cc": 15, "Cf": 16, "Co": 18, "Cs": 19, "Pd": 20, "Ps": 21, "Pe": 22,
	"Pc": 23, "Po": 24, "Sm": 25, "Sc": 26, "Sk": 27, "So": 28, "Pi": 29,
	"Pf": 30,
}

// The property bits of the second table. They are the properties the JDK's
// Character answers with beyond the general category. There is no bit for
// Cased: Character's isCased (the Final_Sigma condition reads it) is
// UPPERCASE_LETTER, LOWERCASE_LETTER, TITLECASE_LETTER, Other_Uppercase or
// Other_Lowercase, which the category and these bits already say.
const (
	propWhiteSpace      = 1 << 0
	propOtherAlphabetic = 1 << 1
	propOtherUppercase  = 1 << 2
	propOtherLowercase  = 1 << 3
	propIdeographic     = 1 << 4
)

// specialMapping is one code point's entry in SpecialCasing.txt: the full case
// mapping, which may be more than one code point. The upper and lower slices
// are code points, and either may be empty (an entry that only maps one way).
type specialMapping struct {
	upper []int32
	lower []int32
}

type ucd struct {
	cat   []uint8 // general category, one byte per code point
	props []uint8 // the property bits above
	upper []int32 // simple upper mapping, or a negative number when none
	lower []int32 // simple lower mapping, or a negative number when none
	title []int32 // simple title mapping, or a negative number when none
	digit []int8  // digit value of a decimal digit, or -1
	// numericRaw is field 8 of the data as it is written ("1/2", "10000000000",
	// "" when the character has no numeric value). It is kept as text because
	// what Character.getNumericValue answers for it is a rule rather than a
	// number: see numericValue.
	numericRaw []string
	numeric    []int32 // Character.getNumericValue, or -1 / -2
	special    map[int32]specialMapping
}

func newUCD() *ucd {
	n := maxCodePoint + 1
	u := &ucd{
		cat:        make([]uint8, n),
		props:      make([]uint8, n),
		upper:      make([]int32, n),
		lower:      make([]int32, n),
		title:      make([]int32, n),
		digit:      make([]int8, n),
		numericRaw: make([]string, n),
		numeric:    make([]int32, n),
		special:    map[int32]specialMapping{},
	}
	for i := range u.upper {
		u.upper[i], u.lower[i], u.title[i] = -1, -1, -1
		u.digit[i], u.numeric[i] = -1, -1
	}
	return u
}

func readUCD(dir string) (*ucd, error) {
	u := newUCD()
	if err := u.readUnicodeData(filepath.Join(dir, "UnicodeData.txt")); err != nil {
		return nil, err
	}
	if err := u.readSpecialCasing(filepath.Join(dir, "SpecialCasing.txt")); err != nil {
		return nil, err
	}
	if err := u.readPropList(filepath.Join(dir, "PropList.txt")); err != nil {
		return nil, err
	}
	u.applyJavaRules()
	return u, nil
}

// readUnicodeData reads the field layout of UnicodeData.txt: the general
// category, the simple case mappings, the decimal digit value and the numeric
// value. A range ("<CJK Ideograph, First>" followed by its Last) carries a
// category for every code point in it and nothing else.
func (u *ucd) readUnicodeData(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	var pending int32 = -1
	var pendingCat uint8
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		field := strings.Split(line, ";")
		if len(field) < 15 {
			return fmt.Errorf("%s: %d fields on a line, want 15", path, len(field))
		}
		cp, err := strconv.ParseInt(field[0], 16, 32)
		if err != nil {
			return fmt.Errorf("%s: %q is not a code point: %w", path, field[0], err)
		}
		cat, ok := category[field[2]]
		if !ok {
			return fmt.Errorf("%s: unknown general category %q", path, field[2])
		}
		if strings.HasSuffix(field[1], ", First>") {
			pending, pendingCat = int32(cp), cat
			continue
		}
		if strings.HasSuffix(field[1], ", Last>") {
			if pending < 0 {
				return fmt.Errorf("%s: %q has no First", path, field[1])
			}
			for c := pending; c <= int32(cp); c++ {
				u.cat[c] = pendingCat
			}
			pending = -1
			continue
		}
		u.cat[cp] = cat
		// Field 5 is an ordinal for the characters that carry a numeric value
		// without being digits; it is not read, because field 8 is what
		// Character.getNumericValue is defined against.
		u.numericRaw[cp] = field[8]
		if cat == 9 { // Nd: the decimal digit value is what digit() reads
			if field[6] == "" {
				return fmt.Errorf("%s: %s is a decimal digit with no digit value", path, field[1])
			}
			d, err := strconv.Atoi(field[6])
			if err != nil || d < 0 || d > 9 {
				return fmt.Errorf("%s: %q is not a digit value", path, field[6])
			}
			u.digit[cp] = int8(d)
		}
		if field[12] != "" {
			u.upper[cp] = int32(mustHex(path, field[12]))
		}
		if field[13] != "" {
			u.lower[cp] = int32(mustHex(path, field[13]))
		}
		if field[14] != "" {
			u.title[cp] = int32(mustHex(path, field[14]))
		}
	}
	return sc.Err()
}

// readSpecialCasing keeps the mappings SpecialCasing.txt gives that are not
// conditioned on a language: those are the ones "ß is two characters upper"
// needs. The conditional entries (tr, az, lt, Final_Sigma) are deliberately not
// read: the two language-tagged ones are not implementable without a locale,
// and Final_Sigma is a rule about the string, which the runtime applies.
func (u *ucd) readSpecialCasing(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field := strings.Split(line, ";")
		if len(field) < 5 {
			return fmt.Errorf("%s: %q has %d fields, want 5", path, line, len(field))
		}
		for i := range field {
			field[i] = strings.TrimSpace(field[i])
		}
		if field[4] != "" {
			continue // a conditional mapping; see the comment above
		}
		cp := int32(mustHex(path, field[0]))
		m := specialMapping{upper: seq(path, field[3]), lower: seq(path, field[1])}
		u.special[cp] = m
	}
	return sc.Err()
}

// readPropList reads the properties PropList.txt carries that the tables need.
// White_Space is read but then adjusted (see applyJavaRules): the JDK's
// Character.isWhitespace is not exactly the property.
func (u *ucd) readPropList(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field := strings.Split(line, ";")
		if len(field) < 2 {
			continue
		}
		lo, hi := mustRange(path, strings.TrimSpace(field[0]))
		var bit uint8
		switch strings.TrimSpace(field[1]) {
		case "White_Space":
			bit = propWhiteSpace
		case "Other_Alphabetic":
			bit = propOtherAlphabetic
		case "Other_Uppercase":
			bit = propOtherUppercase
		case "Other_Lowercase":
			bit = propOtherLowercase
		case "Ideographic":
			bit = propIdeographic
		default:
			continue
		}
		for c := lo; c <= hi; c++ {
			u.props[c] |= bit
		}
	}
	return sc.Err()
}

// applyJavaRules turns the properties that are not what the JDK answers with
// into the ones that are, and fills in the two derived answers.
//
// Character.isWhitespace is the White_Space property *except* for U+0085 (NEXT
// LINE), U+00A0 (NO-BREAK SPACE), U+2007 (FIGURE SPACE) and U+202F (NARROW
// NO-BREAK SPACE), which Java excludes, and *plus* the four information
// separators U+001C..U+001F, which Java includes and Unicode does not call
// whitespace. Both differences are the JDK's own and are stated in its
// documentation of isWhitespace; the table below is what the JDK answers for
// every code point, measured rather than read.
//
// digit() takes the decimal digit value of a decimal digit number and the
// letters A-Z, a-z and their fullwidth forms, and nothing else: the JDK answers
// -1 for U+00B2 SUPERSCRIPT TWO and U+216B ROMAN NUMERAL TWELVE, both of which
// carry a numeric value, and for every OSMANYA digit above the BMP. The letters
// are filled in here because they are not in any data file: what makes them
// digits in a radix is the arithmetic a..z is 10..35.
//
// getNumericValue answers the digit value of a digit, the numeric value of a
// numeral, -2 for a value that is not a non-negative int, and -1 for everything
// else. The int limit is the JDK's: 10^10 (U+16B60) is -2 rather than a value
// that does not fit the return type.
func (u *ucd) applyJavaRules() {
	notWhitespace := map[int32]bool{0x85: true, 0xA0: true, 0x2007: true, 0x202F: true}
	for c := int32(0); c <= maxCodePoint; c++ {
		ws := u.props[c]&propWhiteSpace != 0 && !notWhitespace[c]
		if (c >= 0x1C && c <= 0x1F) || ws {
			u.props[c] |= propWhiteSpace
		} else {
			u.props[c] &^= propWhiteSpace
		}
		if v := letterDigit(c); v >= 0 {
			u.digit[c] = int8(v)
		}
		switch {
		case letterDigit(c) >= 0:
			u.numeric[c] = int32(letterDigit(c))
		case u.digit[c] >= 0:
			u.numeric[c] = int32(u.digit[c])
		default:
			u.numeric[c] = numericValue(u.numericRaw[c])
		}
	}
}

// letterDigit is the value Character.digit and Character.getNumericValue give
// the letters that are digits: a..z, A..Z and the fullwidth forms of both.
func letterDigit(cp int32) int {
	switch {
	case cp >= 'a' && cp <= 'z':
		return int(cp-'a') + 10
	case cp >= 'A' && cp <= 'Z':
		return int(cp-'A') + 10
	case cp >= 0xFF41 && cp <= 0xFF5A:
		return int(cp-0xFF41) + 10
	case cp >= 0xFF21 && cp <= 0xFF3A:
		return int(cp-0xFF21) + 10
	}
	return -1
}

// numericValue is Character.getNumericValue's answer for a raw UnicodeData
// numeric field: -1 when the character has no numeric value, -2 when it has one
// that is not a non-negative int -- a fraction ("1/2"), or a magnitude the
// answer type cannot hold (10^10).
func numericValue(raw string) int32 {
	if raw == "" {
		return -1
	}
	if strings.ContainsRune(raw, '/') {
		return -2
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > 0x7FFFFFFF {
		return -2
	}
	return int32(n)
}

func mustHex(path, s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 16, 32)
	if err != nil {
		panic(fmt.Sprintf("%s: %q is not a code point: %v", path, s, err))
	}
	return v
}

// seq parses a SpecialCasing column: code points separated by spaces.
func seq(path, s string) []int32 {
	if s == "" {
		return nil
	}
	parts := strings.Fields(s)
	out := make([]int32, 0, len(parts))
	for _, p := range parts {
		out = append(out, int32(mustHex(path, p)))
	}
	return out
}

func mustRange(path, s string) (int32, int32) {
	if i := strings.Index(s, ".."); i >= 0 {
		return int32(mustHex(path, s[:i])), int32(mustHex(path, s[i+2:]))
	}
	v := int32(mustHex(path, s))
	return v, v
}
