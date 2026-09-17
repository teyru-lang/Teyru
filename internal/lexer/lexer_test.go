package lexer

import (
	"testing"

	"github.com/teyru-lang/Teyru/internal/source"
)

func lex(src string) ([]Token, string) {
	d := &source.Diagnostics{}
	toks := Lex(source.NewFile("t.teyru", src), d)
	return toks, d.String()
}

func kinds(toks []Token) []Kind {
	out := make([]Kind, 0, len(toks))
	for _, t := range toks {
		out = append(out, t.Kind)
	}
	return out
}

func TestBasicTokens(t *testing.T) {
	toks, errs := lex("class A {}\n")
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	want := []string{"class", "A", "{", "}", ""}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d", len(toks), len(want))
	}
	for i, w := range want {
		if toks[i].Text != w {
			t.Errorf("token %d = %q, want %q", i, toks[i].Text, w)
		}
	}
}

func TestNewlineFlag(t *testing.T) {
	toks, _ := lex("a\nb c\n")
	if !toks[1].NL {
		t.Error("token after a newline must carry the NL flag")
	}
	if toks[2].NL {
		t.Error("token on the same line must not carry the NL flag")
	}
}

// A semicolon ends a statement the way a newline does (decision D6): it is an
// ordinary token, and the token after it starts a new logical line.
func TestSemicolonEndsTheStatement(t *testing.T) {
	toks, errs := lex("int x = 1; int y = 2;")
	if errs != "" {
		t.Fatalf("a semicolon must be accepted: %s", errs)
	}
	semis, after := 0, 0
	for i, tk := range toks {
		if tk.Kind != Op || tk.Text != ";" {
			continue
		}
		semis++
		if i+1 < len(toks) && toks[i+1].NL {
			after++
		}
	}
	if semis != 2 {
		t.Errorf("expected 2 semicolon tokens, got %d", semis)
	}
	if after != 2 {
		t.Errorf("the token after a semicolon must carry the NL flag: %d of %d", after, semis)
	}
}

// A semicolon terminates the statement it follows, so it is the last token on
// its line as far as the flag is concerned.
func TestSemicolonOnItsOwnLine(t *testing.T) {
	toks, errs := lex(";;")
	if errs != "" {
		t.Fatalf("empty statements are legal: %s", errs)
	}
	if got := kinds(toks); len(got) != 3 || got[0] != Op || got[1] != Op || got[2] != EOF {
		t.Errorf("expected two semicolons and EOF, got %v", got)
	}
}

func TestStringEscapes(t *testing.T) {
	toks, errs := lex(`"a\tb\nA\101"`)
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	got := toks[0].Text
	want := "a\tb\nAA"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEscapeThatStandsForItself(t *testing.T) {
	toks, errs := lex(`"a\sb\"c\\d\'e"`)
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	got := toks[0].Text
	want := `a b"c\d'e`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUnicodeEscapes(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"A"`, "A"},
		{`"\uuu0041"`, "A"},
		{`"😀"`, "\U0001F600"},
	}
	for _, tc := range cases {
		toks, errs := lex(tc.src)
		if errs != "" {
			t.Errorf("%s: unexpected diagnostics: %s", tc.src, errs)
			continue
		}
		if toks[0].Text != tc.want {
			t.Errorf("%s: got %q, want %q", tc.src, toks[0].Text, tc.want)
		}
	}
}

func TestUnknownEscapeIsRejected(t *testing.T) {
	for _, src := range []string{`"\q"`, `'\q'`, `"\8"`} {
		if _, errs := lex(src); !contains(errs, "TY-SYN-0011") {
			t.Errorf("%s: want TY-SYN-0011, got %q", src, errs)
		}
	}
}

// A text block is de-indented before its escapes are decoded, so the diagnostic
// has to be mapped back to the source it came from.
func TestTextBlockEscapePosition(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
		line int
		col  int
	}{
		{"unknown", "class A {\n  int x = \"\"\"\n    a\\qb\n    \"\"\"\n}\n", "TY-SYN-0011", 3, 7},
		{"unicode", "class A {\n  int x = \"\"\"\n    a\\uZZZZb\n    \"\"\"\n}\n", "TY-SYN-0005", 3, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &source.Diagnostics{}
			Lex(source.NewFile("t.teyru", tc.src), d)
			if len(d.List) != 1 {
				t.Fatalf("got %d diagnostics, want 1: %s", len(d.List), d.String())
			}
			got := d.List[0]
			if got.Code != tc.code {
				t.Errorf("code = %s, want %s", got.Code, tc.code)
			}
			line, col := got.Pos.File.Position(got.Pos.Off)
			if line != tc.line || col != tc.col {
				t.Errorf("position = %d:%d, want %d:%d", line, col, tc.line, tc.col)
			}
		})
	}
}

func TestTextBlock(t *testing.T) {
	toks, errs := lex("\"\"\"\n  hello\n  world\n  \"\"\"\n")
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	if toks[0].Kind != StringLit {
		t.Fatalf("expected a string literal, got %v", kinds(toks))
	}
	// JLS 3.10.6: the content ends just before the closing delimiter, so the
	// line terminator after the last line of text is part of it.
	if toks[0].Text != "hello\nworld\n" {
		t.Errorf("text block = %q", toks[0].Text)
	}
}

func TestTextBlockClosingOnTextLine(t *testing.T) {
	toks, errs := lex("\"\"\"\n  hello \"\"\"\n")
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	// ... and trailing white space is stripped from every line, so the
	// content starts at the first non-blank character.
	if toks[0].Text != "hello" {
		t.Errorf("text block = %q", toks[0].Text)
	}
}

func TestNumbers(t *testing.T) {
	toks, errs := lex("0 1_000 0x1F 0b1010 017 1.5 2e3 4L 5.0f 'x'")
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	wantInts := []uint64{0, 1000, 31, 10, 15}
	for i, w := range wantInts {
		if toks[i].Int != w {
			t.Errorf("literal %d = %d, want %d", i, toks[i].Int, w)
		}
	}
	if toks[7].Kind != LongLit {
		t.Errorf("4L should be a long literal, got %v", toks[7].Kind)
	}
	if toks[9].Kind != CharLit || toks[9].Int != 'x' {
		t.Errorf("char literal wrong: %+v", toks[9])
	}
}

func TestIntegerRange(t *testing.T) {
	ok := []string{
		"2147483647", "-2147483648", "0", "9223372036854775807L", "-9223372036854775808L",
		"0x80000000", "0xFFFFFFFF", "037777777777", "0b11111111111111111111111111111111",
	}
	for _, src := range ok {
		if _, errs := lex(src); errs != "" {
			t.Errorf("%s: unexpected diagnostics: %s", src, errs)
		}
	}
	bad := []string{
		"2147483648", "-2147483649", "9223372036854775808L", "-9223372036854775809L",
		"0x100000000", "040000000000", "0b100000000000000000000000000000000",
	}
	for _, src := range bad {
		if _, errs := lex(src); !contains(errs, "TY-SYN-0010") {
			t.Errorf("%s: want TY-SYN-0010, got %q", src, errs)
		}
	}
	// limit is the smallest value that overflows, so the check has to distinguish
	// "-" before the literal from a "-" that ends a preceding expression.
	if _, errs := lex("a - 2147483647"); errs != "" {
		t.Errorf("2147483647 after a binary minus: unexpected diagnostics: %s", errs)
	}
}

func TestTextBlockBlankFirstLine(t *testing.T) {
	toks, errs := lex("\"\"\"\n  \n    hello\n    \"\"\"\n")
	if errs != "" {
		t.Fatalf("unexpected diagnostics: %s", errs)
	}
	// The blank first line is shorter than the common indent, so it must be
	// emptied rather than sliced past its own length.
	if toks[0].Text != "\nhello\n" {
		t.Errorf("text block = %q", toks[0].Text)
	}
}

func TestUnterminatedString(t *testing.T) {
	_, errs := lex(`"abc`)
	if !contains(errs, "TY-SYN-0006") {
		t.Errorf("expected unterminated string diagnostic, got %s", errs)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
