// Package format prints Teyru source in one canonical style.
//
// The formatter works on the token stream rather than the syntax tree: the
// parser drops trivia, and a comment is trivia, so a tree-shaped printer would
// have to re-attach every comment to a node it was never attached to. Tokens
// and comments are instead merged back into source order, and the printer
// decides the whitespace between them. Line breaks are kept where the author
// put them; what changes is the indentation, the spacing inside a line, and
// the placement of braces and semicolons.
//
// Comments are copied byte for byte, including the interior of a block
// comment, so formatting never rewrites what a comment says.
package format

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/lexer"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Ext is the source extension the formatter looks for when it walks a
// directory.
const Ext = ".teyru"

// Source formats one source file. The path is used only in diagnostics.
func Source(path, text string) (string, error) {
	items, err := items(path, text)
	if err != nil {
		return "", err
	}
	classifyOps(items)
	markWildcards(items)
	p := &printer{}
	for i, it := range items {
		if it.comment {
			p.comment(it)
			continue
		}
		p.token(it, nextItem(items, i))
	}
	return p.result(), nil
}

// Collect resolves the paths of `teyru fmt` into the files to format. A
// directory stands for every .teyru file underneath it; `.git` and `tests` are
// skipped while walking, because the first is tool state and the second is a
// checkout of another repository. No paths means the current directory.
func Collect(paths []string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	var out []string
	seen := map[string]bool{}
	add := func(path string) {
		clean := filepath.Clean(path)
		if seen[clean] {
			return
		}
		seen[clean] = true
		out = append(out, clean)
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			add(p)
			continue
		}
		root := filepath.Clean(p)
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == root {
					return nil
				}
				switch d.Name() {
				case ".git", "tests":
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), Ext) {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// item is one token or one comment, in source order.
type item struct {
	text    string
	tok     lexer.Token
	comment bool
	// nl reports a line break between the previous item and this one; blank
	// reports that there was an empty line as well.
	nl    bool
	blank bool
	// lineComment distinguishes `// ...` from a block comment.
	lineComment bool
	// off is the item's offset in the source and col its column there, which
	// the printer needs to move a block comment's continuation lines with it.
	off int
	col int
	// op is how `<` and `>` were classified: a generic delimiter, an angle
	// that is a comparison or a shift, or neither.
	op opKind
	// wildcard marks the `*` of `import a.b.*`, which is not multiplication.
	wildcard bool
}

type opKind int

const (
	opNone opKind = iota
	opGenericOpen
	opGenericClose
	opAngle
)

// items merges the lexer's tokens with the comments in the gaps between them.
func items(path, text string) ([]*item, error) {
	f := source.NewFile(path, text)
	diags := &source.Diagnostics{}
	toks := lexer.Lex(f, diags)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%s", strings.TrimSpace(diags.String()))
	}
	var out []*item
	pos := 0
	for _, t := range toks {
		gap := text[pos:t.Off]
		comments, nl, blank := scanGap(text, gap, pos)
		out = append(out, comments...)
		if t.Kind == lexer.EOF {
			break
		}
		out = append(out, &item{
			text:  text[t.Off:t.End],
			tok:   t,
			off:   t.Off,
			col:   column(text, t.Off),
			nl:    nl > 0,
			blank: blank,
		})
		pos = t.End
	}
	return out, nil
}

// scanGap returns the comments in the trivia between two tokens, then the
// number of line breaks that follow the last of them. The gap holds only
// trivia, so every `//` and `/*` in it starts a comment.
func scanGap(text, gap string, base int) ([]*item, int, bool) {
	var out []*item
	i, nl := 0, 0
	for i < len(gap) {
		switch {
		case gap[i] == '\n':
			nl++
			i++
		case gap[i] == '\r':
			nl++
			i++
			if i < len(gap) && gap[i] == '\n' {
				i++
			}
		case strings.HasPrefix(gap[i:], "//"):
			j := i + 2
			for j < len(gap) && gap[j] != '\n' && gap[j] != '\r' {
				j++
			}
			out = append(out, &item{
				text:        gap[i:j],
				comment:     true,
				lineComment: true,
				off:         base + i,
				col:         column(text, base+i),
				nl:          nl > 0,
				blank:       nl >= 2,
			})
			nl = 0
			i = j
		case strings.HasPrefix(gap[i:], "/*"):
			k := strings.Index(gap[i+2:], "*/")
			if k < 0 {
				// The lexer already refused an unterminated comment, so this
				// cannot happen for a source that got here; stop rather than
				// inventing an end.
				return out, nl, nl >= 2
			}
			end := i + 2 + k + 2
			out = append(out, &item{
				text:    gap[i:end],
				comment: true,
				off:     base + i,
				col:     column(text, base+i),
				nl:      nl > 0,
				blank:   nl >= 2,
			})
			nl = 0
			i = end
		default:
			i++
		}
	}
	return out, nl, nl >= 2
}

func nextItem(items []*item, i int) *item {
	if i+1 < len(items) {
		return items[i+1]
	}
	return nil
}

// column is the zero-based column of off in text.
func column(text string, off int) int {
	if off > len(text) {
		off = len(text)
	}
	i := strings.LastIndexByte(text[:off], '\n')
	return off - i - 1
}

// classifyOps separates the two meanings of `<` and `>`. A `<` that opens a
// balanced, type-shaped argument list and a `>` that closes it are generic
// delimiters; every other `<` or `>` is a comparison or a shift and is spaced
// as an operator. Without this, either `List<String>` gains spaces or `a < b`
// loses them.
func classifyOps(items []*item) {
	var toks []*item
	for _, it := range items {
		if !it.comment {
			toks = append(toks, it)
		}
	}
	for i := 0; i < len(toks); i++ {
		if toks[i].text != "<" || !genericStart(toks, i) {
			continue
		}
		j, ok := matchGeneric(toks, i)
		if !ok {
			continue
		}
		for k := i; k <= j; k++ {
			switch toks[k].text {
			case "<":
				toks[k].op = opGenericOpen
			case ">":
				toks[k].op = opGenericClose
			}
		}
		i = j
	}
	// Every remaining `<` or `>` is an operator. Adjacent `>`s spell `>>` and
	// `>>>`, which the lexer deliberately splits; they form one operator.
	for i := 0; i < len(toks); i++ {
		if (toks[i].text != "<" && toks[i].text != ">") || toks[i].op != opNone {
			continue
		}
		toks[i].op = opAngle
		if toks[i].text != ">" {
			continue
		}
		for j := i + 1; j < len(toks); j++ {
			if toks[j].text != ">" || toks[j].op != opNone || toks[j-1].tok.End != toks[j].tok.Off {
				break
			}
			toks[j].op = opAngle
			i = j
		}
	}
}

// markWildcards marks the `*` in an import's star, which is the one place `*`
// is not multiplication.
func markWildcards(items []*item) {
	var prev *item
	for _, it := range items {
		if it.comment {
			continue
		}
		if it.text == "*" && prev != nil && prev.text == "." {
			it.wildcard = true
		}
		prev = it
	}
}

func genericStart(toks []*item, i int) bool {
	if i == 0 || i+1 >= len(toks) {
		return false
	}
	prev := toks[i-1]
	switch prev.tok.Kind {
	case lexer.Ident:
		return true
	}
	switch prev.text {
	case ".", "]", ">":
		return true
	}
	return false
}

// matchGeneric returns the index of the `>` that closes the `<` at i, if the
// tokens between them can spell type arguments and what follows the closer is
// a position a type can occupy.
func matchGeneric(toks []*item, i int) (int, bool) {
	depth := 1
	j := i + 1
	for ; j < len(toks); j++ {
		t := toks[j]
		switch t.text {
		case "<":
			depth++
			continue
		case ">":
			depth--
			if depth == 0 {
				if !typeFollowers(toks, j+1) {
					return 0, false
				}
				return j, j > i+1
			}
			continue
		case ",", ".", "?", "[", "]", "@", "&", "...":
			continue
		}
		switch t.tok.Kind {
		case lexer.Ident:
			continue
		case lexer.Keyword:
			switch t.text {
			case "extends", "super":
				continue
			}
			return 0, false
		}
		return 0, false
	}
	return 0, false
}

// typeFollowers reports whether the token after a generic closer can follow a
// type: a name, a call, a declaration, a punctuation mark that ends one.
func typeFollowers(toks []*item, i int) bool {
	if i >= len(toks) {
		return true
	}
	t := toks[i]
	switch t.tok.Kind {
	case lexer.Ident, lexer.EOF:
		return true
	case lexer.Keyword:
		switch t.text {
		case "extends", "implements", "throws", "permits", "instanceof":
			return true
		}
		return false
	}
	switch t.text {
	case "(", ")", ",", ".", ";", "{", "}", "[", "]", ">", "::", "=", ":", "->", "&", "?", "...":
		return true
	}
	return false
}

type braceKind int

const (
	braceBlock braceKind = iota
	braceArray
)

type frame struct {
	kind      braceKind
	multiline bool // an array initializer that was written over several lines
	isDo      bool
	isSwitch  bool
}

// printer writes one line at a time and decides where the lines end.
type printer struct {
	lines []string
	cur   string
	// lineStarted says cur holds a line, so that a pending break is emitted
	// before the next piece of text rather than at the end of the old one.
	lineStarted bool
	// pendingIndent is the indentation of the next line, in levels.
	pendingIndent int
	blankPending  bool
	indent        int
	paren         int
	frames        []frame
	prev          *item
	// lastUnary says the previous operator was unary, so `-` in `-x` does not
	// take a space after it. lastPostfix does the same for `i++`, which does
	// take one before whatever follows it.
	lastUnary   bool
	lastPostfix bool
	// caseLabel is set between `case`/`default` and its colon, which ends the
	// label's line. caseExtra is one while a case body is being written, so
	// the statements under a label are indented under it.
	caseLabel bool
	caseExtra int
	// pendingDo and pendingSwitch mark a keyword whose body block has not
	// opened yet; the block they belong to is marked as it opens.
	pendingDo     bool
	pendingSwitch bool
	// afterDo is set when a do-while body's brace closed, so `while` stays on
	// the closing brace's line.
	afterDo bool
	// parenBrace records, for each open paren, whether a brace after its
	// closing `)` belongs to the same declaration or control statement. It
	// decides whether a line break before that brace may be removed:
	// `if (x)\n{` and `void f()\n{` are joined, `call()\n{` (a block
	// statement) is not.
	parenBrace     []bool
	lastParenBrace bool
	// lastTok and prevTok are the last two tokens, comments excluded.
	lastTok *item
	prevTok *item
}

func (p *printer) write(s string) {
	if !p.lineStarted {
		if len(p.lines) > 0 && p.blankPending {
			p.lines = append(p.lines, "")
		}
		p.blankPending = false
		p.cur = strings.Repeat("  ", p.pendingIndent)
		p.lineStarted = true
	}
	p.cur += s
}

func (p *printer) space() {
	if p.lineStarted && !strings.HasSuffix(p.cur, " ") {
		p.cur += " "
	}
}

func (p *printer) flushLine() {
	if p.lineStarted {
		p.lines = append(p.lines, strings.TrimRight(p.cur, " \t"))
		p.cur = ""
		p.lineStarted = false
	}
}

func (p *printer) newline(extra int, blank bool) {
	p.flushLine()
	p.pendingIndent = p.indent + p.caseExtra + extra
	if blank {
		p.blankPending = true
	}
}

func (p *printer) result() string {
	p.flushLine()
	for len(p.lines) > 0 && strings.TrimSpace(p.lines[len(p.lines)-1]) == "" {
		p.lines = p.lines[:len(p.lines)-1]
	}
	if len(p.lines) == 0 {
		return ""
	}
	return strings.Join(p.lines, "\n") + "\n"
}

func (p *printer) comment(it *item) {
	if it.nl {
		p.newline(0, it.blank)
	} else if p.lineStarted {
		p.space()
	}
	text := it.text
	if it.nl && !it.lineComment && strings.ContainsAny(text, "\r\n") {
		// The comment moves to its own line, so its continuation lines move
		// with it: the interior is copied as written, shifted by the
		// difference between the column it had and the one it gets.
		text = shiftComment(text, p.pendingIndent*2-it.col)
	}
	p.write(text)
	p.prev = it
	if it.lineComment {
		p.newline(0, false)
		return
	}
	if strings.ContainsAny(it.text, "\r\n") {
		// A block comment that spans lines ends the line; what follows starts
		// on a new one. Its interior is copied as written.
		p.newline(0, false)
		return
	}
	// An inline block comment is separated from the token after it.
	p.space()
}

func (p *printer) token(it, next *item) {
	txt := it.text
	prev := p.prev
	if (txt == "case" || txt == "default") && p.inSwitch() {
		p.caseExtra = 0
	}

	if txt == "}" {
		// Close the frame first: the line a `}` sits on is indented for the
		// block it closes, not the one it closes.
		block := false
		if f := p.top(); f != nil {
			block = f.kind == braceBlock || f.multiline
		}
		if f := p.pop(); f != nil {
			if p.indent > 0 {
				p.indent--
			}
			if f.isDo {
				p.afterDo = true
			}
			if f.isSwitch {
				p.caseExtra = 0
			}
		}
		if p.lineStarted && prev != nil && prev.text != "{" && block {
			p.newline(0, false)
		}
		if !p.lineStarted {
			p.pendingIndent = p.indent + p.caseExtra
		}
	}
	if it.nl && p.lineStarted && p.allowBreakBefore(txt, prev) && p.allowBreakAfter(prev) {
		p.newline(p.continuation(prev), it.blank)
	}
	if p.lineStarted && p.needSpace(prev, it) {
		p.space()
	}
	p.write(txt)
	p.lastUnary = isUnary(txt, prev)
	p.lastPostfix = (txt == "++" || txt == "--") && operandEnd(prev)
	p.prev = it

	switch txt {
	case "(":
		p.paren++
		p.parenBrace = append(p.parenBrace, isControlKeyword(prev) || isDeclarationParen(p.lastTok, p.prevTok))
	case ")":
		if p.paren > 0 {
			p.paren--
		}
		if n := len(p.parenBrace); n > 0 {
			p.lastParenBrace = p.parenBrace[n-1]
			p.parenBrace = p.parenBrace[:n-1]
		}
	case "[":
		p.paren++
	case "]":
		if p.paren > 0 {
			p.paren--
		}
	case "{":
		kind := p.classifyBrace(prev)
		f := frame{kind: kind, isDo: p.pendingDo, isSwitch: p.pendingSwitch}
		p.pendingDo = false
		p.pendingSwitch = false
		if kind == braceArray {
			f.multiline = next != nil && next.nl
		}
		p.frames = append(p.frames, f)
		p.indent++
		if kind == braceBlock {
			if next == nil || next.text != "}" {
				p.newline(0, false)
			}
		} else if f.multiline {
			p.newline(0, false)
		}
	case ";":
		if p.paren == 0 {
			p.newline(0, false)
		}
	case ":":
		if p.caseLabel {
			p.caseLabel = false
			p.caseExtra = 1
			p.newline(0, false)
		}
	case "case":
		if p.inSwitch() {
			p.caseLabel = true
		}
	case "default":
		if p.inSwitch() {
			p.caseLabel = true
		}
	case "do":
		p.pendingDo = true
	case "switch":
		p.pendingSwitch = true
	case "while":
		p.pendingDo = false
		p.afterDo = false
	}
	p.prevTok = p.lastTok
	p.lastTok = it
}

func (p *printer) top() *frame {
	if len(p.frames) == 0 {
		return nil
	}
	return &p.frames[len(p.frames)-1]
}

func (p *printer) pop() *frame {
	if len(p.frames) == 0 {
		return nil
	}
	f := p.frames[len(p.frames)-1]
	p.frames = p.frames[:len(p.frames)-1]
	return &f
}

func (p *printer) inSwitch() bool {
	f := p.top()
	return f != nil && f.isSwitch
}

// classifyBrace decides whether `{` opens a body or an array initializer.
// A body follows a declaration, a control keyword or `->`; an initializer
// follows `=`, a comma, an opening bracket, or `new T[]`.
func (p *printer) classifyBrace(prev *item) braceKind {
	if prev == nil || prev.comment {
		return braceBlock
	}
	switch prev.text {
	case "=", ",", "(", "[", "]", "return":
		return braceArray
	case "{":
		if f := p.top(); f != nil && f.kind == braceArray {
			return braceArray
		}
		return braceBlock
	default:
		return braceBlock
	}
}

func (p *printer) allowBreakBefore(txt string, prev *item) bool {
	if prev == nil {
		return false
	}
	switch txt {
	case ")", "]", "}", ",", ";", ".", "::":
		return false
	case "{":
		if prev.text == ")" {
			return !p.lastParenBrace
		}
		return prev.text == ";" || prev.text == "}" || prev.text == "{" || endsStatement(prev)
	case "else", "catch", "finally":
		return prev.text != "}"
	case "while":
		return !p.afterDo
	case "++", "--":
		return !operandEnd(prev)
	}
	if binaryOp(txt) || txt == "=" || txt == "?" || txt == ":" || txt == "->" || txt == "<" || txt == ">" {
		return false
	}
	return true
}

func (p *printer) allowBreakAfter(prev *item) bool {
	if prev == nil {
		return false
	}
	if prev.comment {
		return true
	}
	switch prev.text {
	case "(", "[", ".", "::", "@", "{", "!", "~":
		return false
	case "++", "--":
		// A postfix operator ends its statement; a prefix one is attached to
		// the operand that follows it.
		return p.lastPostfix
	}
	return true
}

func (p *printer) continuation(prev *item) int {
	if prev == nil {
		return 0
	}
	if p.paren > 0 {
		return 1
	}
	if (binaryOp(prev.text) && !prev.wildcard) || prev.op == opAngle || prev.text == "=" || prev.text == "?" || prev.text == ":" || prev.text == "->" {
		// An array initializer already contributes a level of its own.
		if prev.text == "," {
			if f := p.top(); f != nil && f.kind == braceArray {
				return 0
			}
		}
		return 1
	}
	if prev.text == "," {
		if f := p.top(); f != nil && f.kind == braceArray {
			return 0
		}
		return 1
	}
	return 0
}

func (p *printer) needSpace(prev, cur *item) bool {
	if prev == nil {
		return false
	}
	if prev.comment {
		return true
	}
	pt, ct := prev.text, cur.text

	switch ct {
	case ")", "]", ",", ";", ".", "::", "[":
		return false
	case "(":
		switch pt {
		case "if", "for", "while", "switch", "catch", "synchronized", "assert", "return", "throw", "yield", "case":
			return true
		}
		// A cast followed by a parenthesised expression keeps them apart:
		// `(char) (x)` must not read as a call.
		if pt == ")" {
			return true
		}
		return operatorBefore(prev, p)
	case "++", "--":
		if operandEnd(prev) {
			return false
		}
		if operatorBefore(prev, p) {
			return true
		}
		return wordEnd(prev)
	}
	switch pt {
	case "(", "[", ".", "::", "@":
		return false
	case "++", "--":
		return p.lastPostfix
	case "!":
		return false
	case "~":
		return false
	}
	if pt == "{" {
		return ct != "}"
	}
	if ct == "}" {
		return pt != "{"
	}
	if ct == "{" {
		return true
	}
	if pt == "?" || pt == ":" || ct == "?" || ct == ":" {
		return !adjacent(prev, cur)
	}
	// Generic delimiters take no space; a comparison or shift does.
	if cur.op == opGenericOpen || prev.op == opGenericOpen || cur.op == opGenericClose {
		return false
	}
	if prev.op == opGenericClose {
		return wordStart(cur)
	}
	if prev.op == opAngle || cur.op == opAngle {
		if prev.op == opAngle && cur.op == opAngle && adjacent(prev, cur) {
			return false
		}
		return true
	}
	if ct == "!" || ct == "~" {
		return wordEnd(prev)
	}
	if ct == "+" || ct == "-" {
		if operandEnd(prev) {
			return true
		}
		if pt == "+" || pt == "-" || operatorBefore(prev, p) {
			return true
		}
		return wordEnd(prev)
	}
	if pt == "+" || pt == "-" {
		if p.lastUnary {
			return false
		}
		return true
	}
	if ct == "->" || pt == "->" {
		return true
	}
	if binaryOp(ct) || ct == "=" {
		return true
	}
	if binaryOp(pt) || pt == "=" {
		return true
	}
	if pt == "," || pt == ";" {
		return true
	}
	if wordEnd(prev) && wordStart(cur) {
		return true
	}
	return false
}

// isDeclarationParen reports whether `name(` begins a method or constructor
// declaration rather than a call. A call's name follows `.`, an operator or
// the start of a statement; a declaration's name follows its return type or a
// modifier.
func isDeclarationParen(name, before *item) bool {
	if name == nil || name.tok.Kind != lexer.Ident || before == nil {
		return false
	}
	switch before.tok.Kind {
	case lexer.Ident:
		return true
	case lexer.Keyword:
		switch before.text {
		case "int", "long", "short", "byte", "char", "boolean", "float", "double", "void",
			"public", "private", "protected", "static", "final", "abstract", "native",
			"synchronized", "default", "strictfp":
			return true
		}
		return false
	}
	return before.op == opGenericClose || before.text == "]"
}

func isControlKeyword(it *item) bool {
	if it == nil || it.comment {
		return false
	}
	switch it.text {
	case "if", "for", "while", "switch", "catch", "synchronized":
		return true
	}
	return false
}

// endsStatement reports whether a token can be the last one of a statement
// whose terminator was a line break. A `{` after such a token starts a block
// statement, and the break before it is what terminates the previous
// statement, so it cannot be removed.
func endsStatement(it *item) bool {
	if it == nil || it.comment {
		return false
	}
	switch it.tok.Kind {
	case lexer.IntLit, lexer.LongLit, lexer.FloatLit, lexer.DoubleLit, lexer.CharLit, lexer.StringLit:
		return true
	}
	switch it.text {
	case "++", "--", "]", "}":
		return true
	}
	return false
}

// operatorBefore reports whether the previous token ends an operator, so the
// token after it has to be separated from it.
func operatorBefore(prev *item, p *printer) bool {
	if prev == nil || prev.comment {
		return false
	}
	if (binaryOp(prev.text) && !prev.wildcard) || prev.op == opAngle {
		return true
	}
	switch prev.text {
	case "=", ",", "?", ":", "->":
		return true
	case "+", "-":
		return !p.lastUnary
	}
	return false
}

func adjacent(prev, cur *item) bool {
	return prev != nil && !prev.comment && prev.tok.End == cur.tok.Off
}

func wordStart(it *item) bool {
	switch it.tok.Kind {
	case lexer.Ident, lexer.IntLit, lexer.LongLit, lexer.FloatLit, lexer.DoubleLit,
		lexer.CharLit, lexer.StringLit, lexer.Keyword:
		return true
	}
	return false
}

func wordEnd(it *item) bool {
	if it == nil || it.comment {
		return false
	}
	if wordStart(it) {
		return true
	}
	switch it.text {
	case ")", "]", "}":
		return true
	}
	return false
}

// operandEnd reports whether a token can end an operand, which is what makes
// a following `+` or `-` binary. A keyword is an operand only when it is a
// value (`this`, `true`, ...); `return` and `new` are not.
func operandEnd(it *item) bool {
	if it == nil || it.comment {
		return false
	}
	switch it.tok.Kind {
	case lexer.Ident, lexer.IntLit, lexer.LongLit, lexer.FloatLit, lexer.DoubleLit,
		lexer.CharLit, lexer.StringLit:
		return true
	case lexer.Keyword:
		switch it.text {
		case "this", "super", "true", "false", "null":
			return true
		}
		return false
	}
	switch it.text {
	case ")", "]", "}", "++", "--":
		return true
	}
	return false
}

func binaryOp(txt string) bool {
	switch txt {
	case "==", "!=", "<=", ">=", "&&", "||",
		"+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", ">>>=",
		"*", "/", "%", "&", "|", "^", "<<":
		return true
	}
	return false
}

// shiftComment moves every line after the first by delta spaces, adding them
// when delta is positive and removing up to -delta when it is negative.
func shiftComment(text string, delta int) string {
	if delta == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		if delta > 0 {
			lines[i] = strings.Repeat(" ", delta) + lines[i]
			continue
		}
		n := -delta
		for n > 0 && strings.HasPrefix(lines[i], " ") {
			lines[i] = lines[i][1:]
			n--
		}
	}
	return strings.Join(lines, "\n")
}

// Unary detection: the caller writes the operator, and this says whether it
// was unary so that the next token does not get a space.
func isUnary(txt string, prev *item) bool {
	switch txt {
	case "!", "~":
		return true
	case "+", "-":
		return !operandEnd(prev)
	}
	return false
}
