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
