package util

import "testing"

func TestStringUnits(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		units int
		ascii bool
	}{
		{"empty", "", 0, true},
		{"ascii", "hello", 5, true},
		{"ascii control", "a\nb", 3, true},
		{"latin1 code point", "\u00e9", 1, false},
		{"two byte sequence", "\u00e9\u00e9", 2, false},
		{"han", "\u4e2d", 1, false},
		{"han twice", "\u4e2d\u6587", 2, false},
		// U+1F600 is one code point and two UTF-16 code units, and the lexer
		// writes it as the four-byte sequence rather than as two surrogates.
		{"astral", "\U0001F600", 2, false},
		{"astral with ascii", "a\U0001F600b", 4, false},
		// A lone surrogate is a code unit Java can hold; the lexer writes it in
		// the three-byte form, which is not two units and not astral.
		{"lone high surrogate", "\xed\xa0\x80", 1, false},
		{"lone low surrogate", "\xed\xb0\x80", 1, false},
		{"lone surrogate pair as units", "\xed\xa0\x80\xed\xb8\x80", 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			units, ascii := StringUnits(c.in)
			if units != c.units || ascii != c.ascii {
				t.Fatalf("StringUnits(%q) = %d, %v; want %d, %v", c.in, units, ascii, c.units, c.ascii)
			}
		})
	}
}

func TestStrBreadcrumbs(t *testing.T) {
	// An ASCII string needs no table; anything else needs one entry per 64 code
	// units plus the entry for unit zero, which is what TY_STR_NBC computes in
	// the runtime.
	cases := []struct {
		units int
		ascii bool
		want  int
	}{
		{0, true, 0},
		{5, true, 0},
		{1, false, 1},
		{64, false, 2},
		{65, false, 2},
		{128, false, 3},
	}
	for _, c := range cases {
		if got := StrBreadcrumbs(c.units, c.ascii); got != c.want {
			t.Errorf("StrBreadcrumbs(%d, %v) = %d; want %d", c.units, c.ascii, got, c.want)
		}
	}
}

func TestStrConcat(t *testing.T) {
	// A compile-time fold has to produce the same bytes as the runtime's own
	// join, because byte equality is what equals, string switch and HashMap
	// lookups rest on. The case that decides it is a high surrogate ending the
	// left half and a low one starting the right: Java holds the one character
	// they are, and the four-byte sequence is how this runtime spells it.
	const loneHigh = "\xed\xa0\xbd" // U+D83D, the high half of U+1F600
	const loneLow = "\xed\xb8\x80"  // U+DE00, its low half
	const astral = "\U0001F600"     // 😀
	cases := []struct {
		name string
		a, b string
		want string
	}{
		{"ascii", "ab", "cd", "abcd"},
		{"empty left", "", "ab", "ab"},
		{"empty right", "ab", "", "ab"},
		{"han", "\u4e2d", "\u6587", "\u4e2d\u6587"},
		{"halves join", loneHigh, loneLow, astral},
		{"halves join with text", "a" + loneHigh, loneLow + "b", "a" + astral + "b"},
		// Two halves that are not a pair stay two units: a high followed by a
		// high is not a character, and neither is a low followed by a low.
		{"high then high", loneHigh, loneHigh, loneHigh + loneHigh},
		{"low then high", loneLow, loneHigh, loneLow + loneHigh},
		{"astral then low", astral, loneLow, astral + loneLow},
		{"lone then astral", loneHigh, astral, loneHigh + astral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StrConcat(c.a, c.b)
			if got != c.want {
				t.Fatalf("StrConcat(%q, %q) = %q; want %q", c.a, c.b, got, c.want)
			}
			// Joining two halves into one sequence changes the bytes and not
			// the code units, so the measure a literal is emitted with has to
			// add up the same way the runtime's ulen does.
			want := unitsOf(c.a) + unitsOf(c.b)
			if units, _ := StringUnits(got); units != want {
				t.Fatalf("StringUnits(%q) = %d; want %d", got, units, want)
			}
		})
	}
}

// unitsOf answers the code unit count of a WTF-8 test string, which is what a
// concatenation's units have to add up to however the bytes are laid out.
func unitsOf(s string) int {
	n, _ := StringUnits(s)
	return n
}
