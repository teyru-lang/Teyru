package main

// The two-level tables and the C file they are written into.
//
// A code point's general category and properties are looked up in two steps: a
// block index, one entry per 256 code points, says which block holds them, and
// the block holds one byte per code point. Blocks are deduplicated, so the
// unassigned plains and the long punctuation runs cost one block each rather
// than one block per 256 code points, and the numbers stay small enough to be
// emitted as C string literals -- which is what makes the generated file cheap
// to compile. An array initializer of forty thousand numbers is not: it is a
// forty-thousand-statement initializer in the program's own translation unit.
//
// The sparse tables (the simple case mappings, the digit and numeric values,
// the full case mappings) are sorted arrays searched by binary search. They are
// small enough to write as numbers, and a reader can check a row of one against
// the data file it came from.

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
)

// blockSize is the number of code points one block of the two-level table
// covers. 256 keeps the block count under the 255 the one-byte index can name
// (the largest table here has 156 blocks) and keeps the index small.
const blockSize = 256

// maxExpansion is TY_UC_MAX_EXPANSION in tyrt.h: the widest buffer the runtime
// has for one code point's full case mapping. The check above is what keeps the
// two in step; internal/tools/genunicode's test checks the number too.
const maxExpansion = 3

// The accessors. They are generated with the data because they name it: the two
// bytes of an index entry, the width of a block, and the layout of a special
// casing row are properties of how the tables were written.
const accessors = `
/* The general category of a code point, as one of the TY_UC_* constants above.
   A code point outside the range the table covers is not a code point, and the
   JDK answers UNASSIGNED for it. */
int32_t ty_uc_category(int32_t cp) {
  if (cp < 0 || cp > 0x10FFFF) return 0;
  return ty_uc_cat_block[((int32_t)ty_uc_cat_index[cp >> 8] << 8) | (cp & 0xFF)];
}

/* The property bits of a code point, as the TY_UC_* property constants. */
int32_t ty_uc_props(int32_t cp) {
  if (cp < 0 || cp > 0x10FFFF) return 0;
  return ty_uc_prop_block[((int32_t)ty_uc_prop_index[cp >> 8] << 8) | (cp & 0xFF)];
}

/* A code point's simple case mapping: which is 0 for toUpperCase, 1 for
   toLowerCase and 2 for toTitleCase. A code point with no mapping answers with
   itself, which is what the JDK's do. */
int32_t ty_uc_case(int32_t cp, int32_t which) {
  int32_t lo = 0, hi = (int32_t)(sizeof ty_uc_case_cp / sizeof ty_uc_case_cp[0]) - 1;
  if (cp < 0 || cp > 0x10FFFF) return cp;
  while (lo <= hi) {
    int32_t mid = lo + ((hi - lo) >> 1);
    int32_t at = ty_uc_case_cp[mid];
    if (at == cp) {
      if (which == 0) return ty_uc_case_upper[mid];
      if (which == 1) return ty_uc_case_lower[mid];
      return ty_uc_case_title[mid];
    }
    if (at < cp) lo = mid + 1;
    else hi = mid - 1;
  }
  return cp;
}

/* The digit value of a code point, or -1 when it has none. Which radix a digit
   is good for is the caller's question: the JDK's digit() answers -1 for a
   value that is not below the radix. */
int32_t ty_uc_digit_value(int32_t cp) {
  int32_t lo = 0, hi = (int32_t)(sizeof ty_uc_digit_cp / sizeof ty_uc_digit_cp[0]) - 1;
  while (lo <= hi) {
    int32_t mid = lo + ((hi - lo) >> 1);
    int32_t at = ty_uc_digit_cp[mid];
    if (at == cp) return ty_uc_digit_val[mid];
    if (at < cp) lo = mid + 1;
    else hi = mid - 1;
  }
  return -1;
}

/* Character.getNumericValue: the value of a digit or a numeral, -2 for a
   character whose numeric value is not a non-negative int (a fraction, or a
   magnitude the answer cannot hold), -1 for a character with no numeric value. */
int32_t ty_uc_numeric_value(int32_t cp) {
  int32_t lo = 0, hi = (int32_t)(sizeof ty_uc_num_cp / sizeof ty_uc_num_cp[0]) - 1;
  while (lo <= hi) {
    int32_t mid = lo + ((hi - lo) >> 1);
    int32_t at = ty_uc_num_cp[mid];
    if (at == cp) return ty_uc_num_val[mid];
    if (at < cp) lo = mid + 1;
    else hi = mid - 1;
  }
  return -1;
}

/* The unconditional full case mapping of a code point, when it has one: the
   result is written to out (at most cap code points, always fewer than
   TY_UC_MAX_EXPANSION) and its length returned, or 0 when the code point has no
   entry in SpecialCasing.txt. upper selects the uppercase mapping, anything
   else the lowercase one.

   The entries are the standard's own; the conditional ones (the languages, and
   the Final_Sigma rule about a string's word) are handled by the runtime, which
   is where the string is. */
int32_t ty_uc_special(int32_t cp, int32_t upper, int32_t *out, int32_t cap) {
  int32_t lo = 0, hi = (int32_t)(sizeof ty_uc_sc_cp / sizeof ty_uc_sc_cp[0]) - 1;
  while (lo <= hi) {
    int32_t mid = lo + ((hi - lo) >> 1);
    int32_t at = ty_uc_sc_cp[mid];
    if (at == cp) {
      int32_t off = upper ? ty_uc_sc_upper[mid] : ty_uc_sc_lower[mid];
      int32_t n = upper ? ty_uc_sc_upper_len[mid] : ty_uc_sc_lower_len[mid];
      int32_t i;
      if (n > cap) n = cap;
      for (i = 0; i < n; i++) out[i] = ty_uc_sc_units[ty_uc_sc_off[mid] + off + i];
      return n;
    }
    if (at < cp) lo = mid + 1;
    else hi = mid - 1;
  }
  return 0;
}
`

type tables struct {
	catIndex  []byte
	catBlock  []byte
	propIndex []byte
	propBlock []byte

	caseCP                   []int32
	caseUpper, caseLower     []int32
	caseTitle                []int32
	digitCP                  []int32
	digitVal                 []int32
	numCP                    []int32
	numVal                   []int32
	scCP                     []int32
	scOff, scUpper, scLower  []int32
	scUpperLen, scLowerLen   []int32
	scUnits                  []int32
	maxSpecialExpansionUnits int
}

// twoLevel packs one value per code point into a block index and a list of
// deduplicated blocks, both one byte per entry.
func twoLevel(vals []uint8) (index, block []byte, err error) {
	blocks := map[string]byte{}
	nb := (len(vals) + blockSize - 1) / blockSize
	index = make([]byte, 0, nb)
	for b := 0; b < nb; b++ {
		chunk := make([]byte, blockSize)
		lo := b * blockSize
		hi := lo + blockSize
		if hi > len(vals) {
			hi = len(vals)
		}
		copy(chunk, vals[lo:hi])
		id, ok := blocks[string(chunk)]
		if !ok {
			if len(blocks) > 255 {
				return nil, nil, fmt.Errorf("more than 255 blocks with blockSize %d", blockSize)
			}
			id = byte(len(blocks))
			blocks[string(chunk)] = id
			block = append(block, chunk...)
		}
		index = append(index, id)
	}
	return index, block, nil
}

func (u *ucd) tables() (*tables, error) {
	t := &tables{}
	var err error
	if t.catIndex, t.catBlock, err = twoLevel(u.cat); err != nil {
		return nil, fmt.Errorf("general category: %w", err)
	}
	if t.propIndex, t.propBlock, err = twoLevel(u.props); err != nil {
		return nil, fmt.Errorf("properties: %w", err)
	}
	for cp := int32(0); cp <= maxCodePoint; cp++ {
		if u.upper[cp] < 0 && u.lower[cp] < 0 && u.title[cp] < 0 {
			continue
		}
		t.caseCP = append(t.caseCP, cp)
		t.caseUpper = append(t.caseUpper, orSelf(u.upper[cp], cp))
		t.caseLower = append(t.caseLower, orSelf(u.lower[cp], cp))
		t.caseTitle = append(t.caseTitle, orSelf(u.title[cp], cp))
	}
	// The digit and numeric tables are collected in loops of their own: a code
	// point with a value may have no case mapping (the fullwidth digits do not)
	// and one with a case mapping may have no value, so neither loop may skip
	// the other's entries.
	for cp := int32(0); cp <= maxCodePoint; cp++ {
		if u.digit[cp] >= 0 {
			t.digitCP = append(t.digitCP, cp)
			t.digitVal = append(t.digitVal, int32(u.digit[cp]))
		}
		if u.numeric[cp] != -1 {
			t.numCP = append(t.numCP, cp)
			t.numVal = append(t.numVal, u.numeric[cp])
		}
	}
	if len(t.caseCP) == 0 || len(t.digitCP) == 0 || len(t.numCP) == 0 {
		return nil, fmt.Errorf("a table came out empty")
	}
	// The runtime's buffer for one code point's full case mapping is
	// TY_UC_MAX_EXPANSION code points wide (see tyrt.h). This data has never
	// needed more than three, and a release that needs more would silently
	// truncate every mapping if this did not stop it.
	if t.maxSpecialExpansionUnits > maxExpansion {
		return nil, fmt.Errorf("a full case mapping expands to %d code points, more than TY_UC_MAX_EXPANSION (%d)",
			t.maxSpecialExpansionUnits, maxExpansion)
	}
	cps := make([]int32, 0, len(u.special))
	for cp := range u.special {
		cps = append(cps, cp)
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i] < cps[j] })
	for _, cp := range cps {
		m := u.special[cp]
		t.scCP = append(t.scCP, cp)
		// The upper and lowercase expansions share one unit pool; the lowercase
		// one is written first so that an entry with no lowercase mapping is a
		// length of zero rather than a second index. The offsets are relative to
		// the entry, and the entry's own offset is where its units start.
		t.scOff = append(t.scOff, int32(len(t.scUnits)))
		t.scUnits = append(t.scUnits, m.lower...)
		t.scLower = append(t.scLower, 0)
		t.scLowerLen = append(t.scLowerLen, int32(len(m.lower)))
		t.scUpper = append(t.scUpper, int32(len(m.lower)))
		t.scUnits = append(t.scUnits, m.upper...)
		t.scUpperLen = append(t.scUpperLen, int32(len(m.upper)))
		if len(m.upper) > t.maxSpecialExpansionUnits {
			t.maxSpecialExpansionUnits = len(m.upper)
		}
		if len(m.lower) > t.maxSpecialExpansionUnits {
			t.maxSpecialExpansionUnits = len(m.lower)
		}
	}
	return t, nil
}

func orSelf(v, cp int32) int32 {
	if v < 0 {
		return cp
	}
	return v
}

// emit writes the whole generated file.
func (t *tables) emit(w io.Writer) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, `/* The Unicode %s tables the runtime classifies and case maps with.
 *
 * Generated by internal/tools/genunicode from the Unicode data files in
 * internal/tools/genunicode/data (UnicodeData.txt, SpecialCasing.txt and
 * PropList.txt of that release), which are distributed under the Unicode
 * License v3; see THIRD-PARTY-NOTICES.md. Do not edit this file: run
 * `+"`make unicode-tables`"+` and commit the result.
 *
 * The reference implementation is OpenJDK 21, which is Unicode %s, so the answers here
 * are the JDK's answers and not an interpretation of them: every property is
 * read from the data above, and the two places where the JDK's answer is not
 * exactly a property in a file (Character.isWhitespace, Character.digit) are
 * stated and implemented in genunicode.
 */
#include "tyrt.h"

/* The names the tables are read by -- the general categories, the property bits
   and the longest expansion -- are in tyrt.h, which this file includes: they are
   the runtime's interface rather than an encoding, so they are written by hand
   and checked against this generator by its test. */

/* Block size of the two-level tables: %d code points per block. The index
   names the block, one byte per %d code points, and the block holds one byte
   per code point. */
`, unicodeVersion, unicodeVersion, blockSize, blockSize)

	writeBytes(&b, "ty_uc_cat_index", t.catIndex, "the general category block of each 256 code points")
	writeBytes(&b, "ty_uc_cat_block", t.catBlock, "the general category of each code point in a block, one block after another")
	writeBytes(&b, "ty_uc_prop_index", t.propIndex, "the property block of each 256 code points")
	writeBytes(&b, "ty_uc_prop_block", t.propBlock, "the property bits of each code point in a block")

	writeInts(&b, "ty_uc_case_cp", t.caseCP, "the code points with a simple case mapping, in order")
	writeInts(&b, "ty_uc_case_upper", t.caseUpper, "their uppercase mappings")
	writeInts(&b, "ty_uc_case_lower", t.caseLower, "their lowercase mappings")
	writeInts(&b, "ty_uc_case_title", t.caseTitle, "their titlecase mappings")
	writeInts(&b, "ty_uc_digit_cp", t.digitCP, "the code points Character.digit accepts as digits")
	writeInts(&b, "ty_uc_digit_val", t.digitVal, "their digit values")
	writeInts(&b, "ty_uc_num_cp", t.numCP, "the code points Character.getNumericValue has a value for")
	writeInts(&b, "ty_uc_num_val", t.numVal, "the values: -2 is a numeric value that is not a non-negative int")
	writeInts(&b, "ty_uc_sc_cp", t.scCP, "the code points with an unconditional full case mapping")
	writeInts(&b, "ty_uc_sc_off", t.scOff, "where each entry's units start")
	writeInts(&b, "ty_uc_sc_upper", t.scUpper, "where its uppercase expansion starts, from the entry's units")
	writeInts(&b, "ty_uc_sc_upper_len", t.scUpperLen, "how many code points that expansion is")
	writeInts(&b, "ty_uc_sc_lower", t.scLower, "where its lowercase expansion starts")
	writeInts(&b, "ty_uc_sc_lower_len", t.scLowerLen, "how many code points that expansion is")
	writeInts(&b, "ty_uc_sc_units", t.scUnits, "the unit pool the expansions are slices of")

	fmt.Fprintf(&b, "\n/* The longest expansion any code point has, which is what a caller's buffer has to hold. */\n#define TY_UC_MAX_EXPANSION %d\n", t.maxSpecialExpansionUnits)
	b.WriteString(accessors)
	if _, err := w.Write(b.Bytes()); err != nil {
		return err
	}
	return nil
}

// writeBytes writes a byte table as a C string literal. The data is one byte
// per character and the literal is written in chunks: a string constant longer
// than the C standard's minimum maximum is a diagnostic in some compilers, and
// adjacent literals are concatenated by the compiler into the one array.
func writeBytes(b *bytes.Buffer, name string, data []byte, doc string) {
	fmt.Fprintf(b, "\n/* %s: %d bytes. */\nstatic const unsigned char %s[%d] =\n", doc, len(data), name, len(data))
	const chunk = 512
	for i := 0; i < len(data); i += chunk {
		hi := i + chunk
		if hi > len(data) {
			hi = len(data)
		}
		b.WriteString("  \"")
		for _, c := range data[i:hi] {
			switch {
			case c == '"' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case c >= 0x20 && c < 0x7F:
				b.WriteByte(c)
			default:
				fmt.Fprintf(b, "\\%03o", c)
			}
		}
		b.WriteString("\"\n")
	}
	b.WriteString(";\n")
}

// writeInts writes a table of numbers, wrapped so that a reader can scan it and
// a diff of a regenerated file is one line per row rather than one line for a
// megabyte.
func writeInts(b *bytes.Buffer, name string, data []int32, doc string) {
	fmt.Fprintf(b, "\n/* %s: %d entries, in increasing code point order. */\nstatic const int32_t %s[] = {\n", doc, len(data), name)
	const per = 12
	for i := 0; i < len(data); i += per {
		hi := i + per
		if hi > len(data) {
			hi = len(data)
		}
		row := make([]string, 0, per)
		for _, v := range data[i:hi] {
			row = append(row, fmt.Sprint(v))
		}
		b.WriteString("  " + strings.Join(row, ",") + ",\n")
	}
	b.WriteString("};\n")
}
