package main

// The generator's own test: that the file in the tree is the one this data
// produces, and that the three facts the runtime's C relies on still hold.
//
// It is what makes "re-running it changes nothing" a property of the repository
// rather than a promise: `go test ./...` fails when the checked-in file and the
// data have drifted apart, so a change that regenerates the tables without
// committing them, or edits them by hand, cannot land quietly.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestGeneratedTablesAreCurrent(t *testing.T) {
	want, err := generate(repoPath(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	path := repoPath(t, outPath)
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		t.Fatalf("%s is not what the data produce: run `make unicode-tables` and commit the result", path)
	}
}

// repoPath resolves a repository-relative path, the way the generator does.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, rel)
}

// TestRuntimeAssumptions holds the runtime's C to the facts it is written
// against. Each of these is a place where the C takes a shortcut that is only
// right because of something about the data, and a Unicode release that changed
// it would break the C silently.
func TestRuntimeAssumptions(t *testing.T) {
	u, err := readUCD(repoPath(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}

	// str_case maps an ASCII string with the a..z / A..Z arithmetic, which is
	// the whole answer only if no code point below U+0080 has a full case
	// mapping. (U+00DF is the lowest code point that has one.)
	for cp, m := range u.special {
		if cp < 0x80 {
			t.Errorf("U+%04X has a full case mapping; the ASCII fast path in str_case is not exact", cp)
			_ = m
		}
	}

	// ty_str_cmp_ic advances by the code point's length rather than by
	// Character.charCount of the folded value as the JDK's own loop does, which
	// is the same thing only while no simple mapping crosses the BMP boundary.
	for cp := int32(0); cp <= maxCodePoint; cp++ {
		for _, v := range []int32{u.upper[cp], u.lower[cp], u.title[cp]} {
			if v >= 0 && (cp < 0x10000) != (v < 0x10000) {
				t.Errorf("U+%04X maps to U+%04X, across the BMP boundary", cp, v)
			}
		}
	}

	// The runtime's buffer for one code point's full case mapping is
	// TY_UC_MAX_EXPANSION code points; tyrt.h says how wide, and the generator
	// refuses data that does not fit.
	header, err := os.ReadFile(repoPath(t, "internal/runtime/src/tyrt.h"))
	if err != nil {
		t.Fatal(err)
	}
	max, ok := defineInt(string(header), "TY_UC_MAX_EXPANSION")
	if !ok {
		t.Fatal("tyrt.h does not define TY_UC_MAX_EXPANSION")
	}
	if max != maxExpansion {
		t.Errorf("tyrt.h says TY_UC_MAX_EXPANSION is %d, this generator is written for %d", max, maxExpansion)
	}
	tb, err := u.tables()
	if err != nil {
		t.Fatal(err)
	}
	if tb.maxSpecialExpansionUnits > max {
		t.Errorf("the data needs a %d-code-point buffer, tyrt.h has %d", tb.maxSpecialExpansionUnits, max)
	}

	// The category numbers are Character's constants, which a program reads
	// from lib/04_boxing.teyru and compares against what getType answers. They
	// are written by hand in tyrt.h and by this generator in its own table of
	// them, so the two are checked against each other rather than assumed equal.
	for name, v := range category {
		have, ok := defineInt(string(header), "TY_UC_"+categoryName(name))
		if !ok {
			t.Errorf("tyrt.h does not define TY_UC_%s", categoryName(name))
			continue
		}
		if have != int(v) {
			t.Errorf("tyrt.h says TY_UC_%s is %d, the generator numbers it %d", categoryName(name), have, v)
		}
	}
	// ... and the constants a program reads have the same numbers.
	boxing, err := os.ReadFile(repoPath(t, "lib/04_boxing.teyru"))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range categoryConstants {
		if !strings.Contains(string(boxing), fmt.Sprintf("public static final int %s = %d\n", name, v)) {
			t.Errorf("lib/04_boxing.teyru does not declare %s = %d", name, v)
		}
	}
}

// categoryName is the tyrt.h spelling of a general category abbreviation.
func categoryName(abbr string) string {
	switch abbr {
	case "Lu":
		return "UPPERCASE_LETTER"
	case "Ll":
		return "LOWERCASE_LETTER"
	case "Lt":
		return "TITLECASE_LETTER"
	case "Lm":
		return "MODIFIER_LETTER"
	case "Lo":
		return "OTHER_LETTER"
	case "Mn":
		return "NON_SPACING_MARK"
	case "Me":
		return "ENCLOSING_MARK"
	case "Mc":
		return "COMBINING_SPACING_MARK"
	case "Nd":
		return "DECIMAL_DIGIT_NUMBER"
	case "Nl":
		return "LETTER_NUMBER"
	case "No":
		return "OTHER_NUMBER"
	case "Zs":
		return "SPACE_SEPARATOR"
	case "Zl":
		return "LINE_SEPARATOR"
	case "Zp":
		return "PARAGRAPH_SEPARATOR"
	case "Cc":
		return "CONTROL"
	case "Cf":
		return "FORMAT"
	case "Co":
		return "PRIVATE_USE"
	case "Cs":
		return "SURROGATE"
	case "Pd":
		return "DASH_PUNCTUATION"
	case "Ps":
		return "START_PUNCTUATION"
	case "Pe":
		return "END_PUNCTUATION"
	case "Pc":
		return "CONNECTOR_PUNCTUATION"
	case "Po":
		return "OTHER_PUNCTUATION"
	case "Sm":
		return "MATH_SYMBOL"
	case "Sc":
		return "CURRENCY_SYMBOL"
	case "Sk":
		return "MODIFIER_SYMBOL"
	case "So":
		return "OTHER_SYMBOL"
	case "Pi":
		return "INITIAL_QUOTE_PUNCTUATION"
	case "Pf":
		return "FINAL_QUOTE_PUNCTUATION"
	case "Cn":
		return "UNASSIGNED"
	}
	return abbr
}

// categoryConstants maps the names a program reads to the numbers it compares
// against: lib/04_boxing.teyru's Character constants, in Java's numbering.
var categoryConstants = map[string]int{
	"UNASSIGNED":                0,
	"UPPERCASE_LETTER":          1,
	"LOWERCASE_LETTER":          2,
	"TITLECASE_LETTER":          3,
	"MODIFIER_LETTER":           4,
	"OTHER_LETTER":              5,
	"NON_SPACING_MARK":          6,
	"ENCLOSING_MARK":            7,
	"COMBINING_SPACING_MARK":    8,
	"DECIMAL_DIGIT_NUMBER":      9,
	"LETTER_NUMBER":             10,
	"OTHER_NUMBER":              11,
	"SPACE_SEPARATOR":           12,
	"LINE_SEPARATOR":            13,
	"PARAGRAPH_SEPARATOR":       14,
	"CONTROL":                   15,
	"FORMAT":                    16,
	"PRIVATE_USE":               18,
	"SURROGATE":                 19,
	"DASH_PUNCTUATION":          20,
	"START_PUNCTUATION":         21,
	"END_PUNCTUATION":           22,
	"CONNECTOR_PUNCTUATION":     23,
	"OTHER_PUNCTUATION":         24,
	"MATH_SYMBOL":               25,
	"CURRENCY_SYMBOL":           26,
	"MODIFIER_SYMBOL":           27,
	"OTHER_SYMBOL":              28,
	"INITIAL_QUOTE_PUNCTUATION": 29,
	"FINAL_QUOTE_PUNCTUATION":   30,
}

// defineInt reads `#define NAME <n>` out of a C header.
func defineInt(src, name string) (int, bool) {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "#define "+name)
		if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
