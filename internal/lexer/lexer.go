// Package lexer turns Teyru source text into tokens.
//
// Newlines are not emitted as tokens; instead every token records whether a
// logical line break precedes it, and the parser decides whether that break
// terminates the current construct.
package lexer

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/teyru-lang/Teyru/internal/source"
)

// Kind is a token kind.
type Kind int

const (
	EOF Kind = iota
	Ident
	IntLit
	LongLit
	FloatLit
	DoubleLit
	CharLit
	StringLit
	Keyword
	Op
)

// Token is a lexical token.
type Token struct {
	Kind Kind
	Text string // identifier, keyword or operator spelling; decoded literal value for strings
	Off  int
	End  int
	NL   bool // a line break precedes this token
	Int  uint64
	Flt  float64
}

var keywords = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true, "case": true,
	"catch": true, "char": true, "class": true, "const": true, "continue": true, "default": true,
	"do": true, "double": true, "else": true, "enum": true, "extends": true, "final": true,
	"finally": true, "float": true, "for": true, "goto": true, "if": true, "implements": true,
	"import": true, "instanceof": true, "int": true, "interface": true, "long": true, "native": true,
	"new": true, "package": true, "private": true, "protected": true, "public": true, "return": true,
	"short": true, "static": true, "strictfp": true, "super": true, "switch": true, "synchronized": true,
	"this": true, "throw": true, "throws": true, "transient": true, "try": true, "void": true,
	"volatile": true, "while": true, "true": true, "false": true, "null": true,
}

// operators sorted longest first.
var operators = []string{
	">>>=", "<<=", ">>=", "...", "->", "::", "++", "--", "&&", "||", "==", "!=", "<=", ">=",
	"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<",
	"(", ")", "{", "}", "[", "]", ",", ".", "@", "=", ">", "<", "!", "~", "?", ":",
	"+", "-", "*", "/", "&", "|", "^", "%", ";",
}

// Lex tokenizes a file. `>>` and `>>>` are produced as separate `>` tokens with
// adjacency preserved so generic closers work; the parser re-joins them.
func Lex(f *source.File, diags *source.Diagnostics) []Token {
	l := &lexer{f: f, src: f.Text, diags: diags}
	l.run()
	return l.toks
}

type lexer struct {
	f     *source.File
	src   string
	pos   int
	nl    bool
	toks  []Token
	diags *source.Diagnostics
	// offs, when set, maps an offset in src back to the offset in f.Text it came
	// from. The sub-lexer that decodes a text block runs on the de-indented
	// content, so without it a diagnostic there would point at the wrong place.
	offs []int
}

func (l *lexer) errf(off int, code, format string, args ...any) {
	if l.offs != nil && off < len(l.offs) {
		off = l.offs[off]
	}
	l.diags.Errorf(source.Pos{File: l.f, Off: off}, code, format, args...)
}

func (l *lexer) emit(t Token) {
	t.NL = l.nl
	l.nl = false
	l.toks = append(l.toks, t)
}

func (l *lexer) run() {
	if strings.HasPrefix(l.src, "\uFEFF") {
		l.pos = 3
	}
	for {
		l.skipTrivia()
		if l.pos >= len(l.src) {
			l.emit(Token{Kind: EOF, Off: l.pos, End: l.pos})
			return
		}
		start := l.pos
		c := l.src[l.pos]
		switch {
		case c == '"':
			if strings.HasPrefix(l.src[l.pos:], `"""`) {
				l.textBlock()
			} else {
				l.stringLit()
			}
		case c == '\'':
			l.charLit()
		case c >= '0' && c <= '9' || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
			l.number()
		case isIdentStart(l.src[l.pos:]):
			for l.pos < len(l.src) {
				r, n := utf8.DecodeRuneInString(l.src[l.pos:])
				if !(r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
					break
				}
				l.pos += n
			}
			text := l.src[start:l.pos]
			k := Ident
			if keywords[text] {
				k = Keyword
			}
			l.emit(Token{Kind: k, Text: text, Off: start, End: l.pos})
		default:
			matched := false
			for _, op := range operators {
				if strings.HasPrefix(l.src[l.pos:], op) {
					l.pos += len(op)
					l.emit(Token{Kind: Op, Text: op, Off: start, End: l.pos})
					if op == ";" {
						// A semicolon ends a statement the way a newline does
						// (decision D6): the parser reads the ';' as the
						// terminator, and the next token carries the NL flag so
						// that everything which looks only at line breaks sees
						// the statement end as well.
						l.nl = true
					}
					matched = true
					break
				}
			}
			if !matched {
				_, n := utf8.DecodeRuneInString(l.src[l.pos:])
				l.errf(start, "TY-SYN-0002", "unexpected character %q", l.src[l.pos:l.pos+n])
				l.pos += n
			}
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// afterMinus reports whether the token just emitted is the operator '-', which
// is the only position where a literal may hold a value that does not fit.
func (l *lexer) afterMinus() bool {
	n := len(l.toks)
	return n > 0 && l.toks[n-1].Kind == Op && l.toks[n-1].Text == "-"
}

func isIdentStart(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '_' || r == '$' || unicode.IsLetter(r)
}

func (l *lexer) skipTrivia() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\n' || c == '\r':
			l.nl = true
			l.pos++
		case c == ' ' || c == '\t' || c == '\f':
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "//"):
			for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
				l.pos++
			}
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			end := strings.Index(l.src[l.pos+2:], "*/")
			if end < 0 {
				l.errf(l.pos, "TY-SYN-0004", "unterminated block comment")
				l.pos = len(l.src)
				return
			}
			if strings.ContainsAny(l.src[l.pos:l.pos+2+end], "\n\r") {
				l.nl = true
			}
			l.pos += end + 4
		default:
			return
		}
	}
}

// escape decodes one escape sequence starting after the backslash.
func (l *lexer) escape(sb *strings.Builder) {
	if l.pos >= len(l.src) {
		return
	}
	c := l.src[l.pos]
	l.pos++
	switch c {
	case 'n':
		sb.WriteByte('\n')
	case 't':
		sb.WriteByte('\t')
	case 'r':
		sb.WriteByte('\r')
	case 'b':
		sb.WriteByte('\b')
	case 'f':
		sb.WriteByte('\f')
	case 's':
		sb.WriteByte(' ')
	case '0', '1', '2', '3', '4', '5', '6', '7':
		v := int(c - '0')
		for i := 0; i < 2 && l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '7'; i++ {
			v = v*8 + int(l.src[l.pos]-'0')
			l.pos++
		}
		sb.WriteRune(rune(v))
	case 'u':
		for l.pos < len(l.src) && l.src[l.pos] == 'u' {
			l.pos++
		}
		if l.pos+4 <= len(l.src) {
			if v, err := strconv.ParseUint(l.src[l.pos:l.pos+4], 16, 32); err == nil {
				l.pos += 4
				r := rune(v)
				if utf16IsSurrogate(r) && strings.HasPrefix(l.src[l.pos:], `\u`) && l.pos+6 <= len(l.src) {
					if v2, err := strconv.ParseUint(l.src[l.pos+2:l.pos+6], 16, 32); err == nil {
						r = ((r - 0xD800) << 10) + (rune(v2) - 0xDC00) + 0x10000
						l.pos += 6
					}
				}
				sb.WriteRune(r)
				return
			}
		}
		l.errf(l.pos, "TY-SYN-0005", "invalid unicode escape")
	case '"', '\'', '\\':
		sb.WriteByte(c)
	case '\n':
		// line continuation inside text blocks
	default:
		// The backslash is dropped, so decoding it silently would turn "\q" into
		// "q" without any sign that the escape was not recognised.
		l.errf(l.pos-1, "TY-SYN-0011", "invalid escape sequence \\%c", c)
		sb.WriteByte(c)
	}
}

func utf16IsSurrogate(r rune) bool { return r >= 0xD800 && r < 0xDC00 }

func (l *lexer) stringLit() {
	start := l.pos
	l.pos++
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) || l.src[l.pos] == '\n' || l.src[l.pos] == '\r' {
			l.errf(start, "TY-SYN-0006", "unterminated string literal")
			break
		}
		c := l.src[l.pos]
		if c == '"' {
			l.pos++
			break
		}
		if c == '\\' {
			l.pos++
			l.escape(&sb)
			continue
		}
		sb.WriteByte(c)
		l.pos++
	}
	l.emit(Token{Kind: StringLit, Text: sb.String(), Off: start, End: l.pos})
}

func (l *lexer) textBlock() {
	start := l.pos
	l.pos += 3
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '\r' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '\n' {
		l.pos++
	} else {
		l.errf(start, "TY-SYN-0007", "text block must start with a line break after \"\"\"")
	}
	end := strings.Index(l.src[l.pos:], `"""`)
	if end < 0 {
		l.errf(start, "TY-SYN-0006", "unterminated text block")
		l.pos = len(l.src)
		l.emit(Token{Kind: StringLit, Off: start, End: l.pos})
		return
	}
	contentStart, contentEnd := l.pos, l.pos+end
	raw := strings.ReplaceAll(l.src[contentStart:contentEnd], "\r\n", "\n")
	l.pos += end + 3
	lines := strings.Split(raw, "\n")
	// lineOff[i] is where lines[i] starts in the source. Escapes are decoded on
	// the de-indented text, so a diagnostics has to be mapped back through it.
	lineOff := make([]int, len(lines))
	lineOff[0] = contentStart
	for i, off := 1, contentStart; i < len(lines); i++ {
		for off < contentEnd && l.src[off] != '\n' {
			off++
		}
		off++
		lineOff[i] = off
	}
	minIndent := -1
	for i, ln := range lines {
		last := i == len(lines)-1
		if strings.TrimSpace(ln) == "" && !last {
			continue
		}
		ind := len(ln) - len(strings.TrimLeft(ln, " \t"))
		if minIndent < 0 || ind < minIndent {
			minIndent = ind
		}
	}
	var out strings.Builder
	// offs[i] is the source offset of out[i], which is what the escape decoder
	// below reports against.
	offs := make([]int, 0, len(raw))
	for i, ln := range lines {
		last := i == len(lines)-1
		base := lineOff[i]
		if len(ln) >= minIndent && minIndent > 0 {
			ln = ln[minIndent:]
			base += minIndent
		} else if strings.TrimSpace(ln) == "" {
			ln = ""
		}
		ln = strings.TrimRight(ln, " \t")
		for j := 0; j < len(ln); j++ {
			offs = append(offs, base+j)
		}
		out.WriteString(ln)
		if !last {
			out.WriteByte('\n')
			offs = append(offs, lineOff[i+1]-1)
		}
	}
	// decode escapes
	s := out.String()
	sub := &lexer{f: l.f, src: s, diags: l.diags, offs: offs}
	var sb strings.Builder
	for sub.pos < len(s) {
		if s[sub.pos] == '\\' {
			sub.pos++
			sub.escape(&sb)
			continue
		}
		sb.WriteByte(s[sub.pos])
		sub.pos++
	}
	l.emit(Token{Kind: StringLit, Text: sb.String(), Off: start, End: l.pos})
}

func (l *lexer) charLit() {
	start := l.pos
	l.pos++
	var sb strings.Builder
	if l.pos < len(l.src) && l.src[l.pos] == '\\' {
		l.pos++
		l.escape(&sb)
	} else if l.pos < len(l.src) {
		r, n := utf8.DecodeRuneInString(l.src[l.pos:])
		sb.WriteRune(r)
		l.pos += n
	}
	if l.pos >= len(l.src) || l.src[l.pos] != '\'' {
		l.errf(start, "TY-SYN-0008", "unterminated character literal")
	} else {
		l.pos++
	}
	r, _ := utf8.DecodeRuneInString(sb.String())
	if r > 0xFFFF {
		l.errf(start, "TY-SYN-0008", "character literal does not fit in a char")
	}
	l.emit(Token{Kind: CharLit, Text: sb.String(), Int: uint64(r), Off: start, End: l.pos})
}

func (l *lexer) number() {
	start := l.pos
	s := l.src
	isHex, isBin := false, false
	if strings.HasPrefix(s[l.pos:], "0x") || strings.HasPrefix(s[l.pos:], "0X") {
		isHex = true
		l.pos += 2
	} else if strings.HasPrefix(s[l.pos:], "0b") || strings.HasPrefix(s[l.pos:], "0B") {
		isBin = true
		l.pos += 2
	}
	isFloat := false
	for l.pos < len(s) {
		c := s[l.pos]
		switch {
		case isDigit(c) || c == '_':
		case isHex && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
		case !isHex && !isBin && c == '.' && l.pos+1 < len(s) && isDigit(s[l.pos+1]):
			isFloat = true
		case !isHex && !isBin && c == '.' && !(l.pos+1 < len(s) && (isIdentStart(s[l.pos+1:]) || s[l.pos+1] == '.')):
			isFloat = true
		case !isHex && !isBin && (c == 'e' || c == 'E'):
			isFloat = true
			if l.pos+1 < len(s) && (s[l.pos+1] == '+' || s[l.pos+1] == '-') {
				l.pos++
			}
		default:
			goto done
		}
		l.pos++
	}
done:
	body := strings.ReplaceAll(s[start:l.pos], "_", "")
	kind := IntLit
	if l.pos < len(s) {
		switch s[l.pos] {
		case 'l', 'L':
			kind = LongLit
			l.pos++
		case 'f', 'F':
			kind = FloatLit
			l.pos++
		case 'd', 'D':
			kind = DoubleLit
			l.pos++
		}
	}
	if isFloat && kind != FloatLit {
		kind = DoubleLit
	}
	t := Token{Kind: kind, Text: s[start:l.pos], Off: start, End: l.pos}
	switch kind {
	case FloatLit, DoubleLit:
		v, err := strconv.ParseFloat(body, 64)
		if err != nil {
			l.errf(start, "TY-SYN-0009", "malformed floating-point literal")
		}
		t.Flt = v
	default:
		var v uint64
		var err error
		switch {
		case isHex:
			v, err = strconv.ParseUint(body[2:], 16, 64)
		case isBin:
			v, err = strconv.ParseUint(body[2:], 2, 64)
		case len(body) > 1 && body[0] == '0':
			v, err = strconv.ParseUint(body[1:], 8, 64)
		default:
			v, err = strconv.ParseUint(body, 10, 64)
		}
		if err != nil {
			l.errf(start, "TY-SYN-0009", "malformed integer literal")
		}
		limit := uint64(1 << 31)
		if kind == LongLit {
			limit = 1 << 63
		}
		if isHex || isBin || (len(body) > 1 && body[0] == '0') {
			if kind == IntLit && v > 0xFFFFFFFF {
				l.errf(start, "TY-SYN-0010", "integer literal out of range")
			}
		} else if v > limit || (v == limit && !l.afterMinus()) {
			// limit itself is the smallest value that overflows: the parser folds
			// a minus in front of a literal, which is the only way -2147483648
			// and -9223372036854775808L can be written (parser.go parseUnary).
			// Anywhere else the value would wrap around to the negative boundary.
			l.errf(start, "TY-SYN-0010", "integer literal out of range")
		}
		t.Int = v
	}
	l.emit(t)
}
