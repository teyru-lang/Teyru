package codegen

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Debug selects the source-level debug information a C build carries.
//
// The generated C is not the program: a debugger or a backtrace that reads it
// names program.c and lines nobody wrote. With On set, the emitter brackets
// every region it copies from a .teyru file with #line directives, so the C
// compiler's line table -- and the DWARF it writes -- names the file and line
// the statement came from, and returns to the generated file wherever the code
// is the compiler's own. Directives are a preprocessor feature: the program
// they are compiled into is the same one.
//
// With On clear the emitter writes no directive at all, which is what keeps a
// build without -g byte-for-byte the C it was before debug information existed.
type Debug struct {
	On bool
	// CFile is the path the generated C is written to. Every region ends with a
	// directive that hands the C compiler back to this file, and the line
	// number it names is only known once the whole translation unit is
	// assembled (see resolveDebugLines).
	CFile string
}

// resetDirective is the marker the emitter writes where the generated C
// resumes after a region copied from a .teyru file. Its line number is filled
// in by resolveDebugLines, which is the first pass that knows it; a marker that
// reached a C compiler would name a file nobody has.
const resetDirective = `#line 0 "<tyru:generated>"`

// sourceDirective returns the #line directive that makes the C compiler
// attribute the lines that follow to p, and reports whether there is one.
//
// A position with no on-disk file has none: a statement the emitter
// synthesized, and the standard library's files, which are embedded in the
// compiler as <lib>/NN_name.teyru rather than read from disk. Pointing a
// debugger at a path nobody can open is worse than leaving the generated C's
// own line in place.
func (e *Emitter) sourceDirective(p source.Pos) (string, bool) {
	if !e.dbg.On || p.File == nil || p.File.Path == "" {
		return "", false
	}
	// `<lib>/...` is the embedded standard library, and `<builtin>` a position
	// with no file at all: neither exists on the machine the program is
	// debugged on.
	if strings.HasPrefix(p.File.Path, "<") {
		return "", false
	}
	line, _ := p.File.Position(p.Off)
	if line <= 0 {
		return "", false
	}
	d := fmt.Sprintf("#line %d %s", line, cQuote(e.sourcePath(p.File)))
	// Two statements on one source line need the directive once: the mapping
	// is still the one the previous directive established.
	if d == e.lastDirective {
		return "", false
	}
	e.lastDirective = d
	return d, true
}

// sourcePath is the path a source file is named by in the generated C. The
// first name wins, because the same file is asked about once per statement and
// the answer has to be the same every time. It is made absolute so that a
// debugger started anywhere can open it: a relative name is resolved against
// the directory the C compiler ran in, which is not where the program is
// debugged.
func (e *Emitter) sourcePath(f *source.File) string {
	if p, ok := e.srcPaths[f]; ok {
		return p
	}
	p := f.Path
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	e.srcPaths[f] = p
	return p
}

// writeDirective writes d to b as a line of its own. Some generated code is
// captured while it is built and then placed in the middle of a C line -- a
// switch expression becomes a statement expression, which starts after `({`
// on the line that asked for it -- and a directive there would not be a
// directive. The newline puts it back at the start of one.
func writeDirective(b *strings.Builder, d string) {
	if s := b.String(); len(s) > 0 && s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString(d)
	b.WriteByte('\n')
}

// writeReset hands the C compiler back to the generated file, and forgets the
// source line that was active: the next statement's directive must be written
// even when it names the same line, because this one ended the mapping.
func (e *Emitter) writeReset(b *strings.Builder) {
	writeDirective(b, resetDirective)
	e.lastDirective = ""
}

// resolveDebugLines replaces every reset marker with the directive that returns
// to the generated file at the line that follows it.
//
// The number cannot be written where the marker is: the sections of a
// translation unit are emitted in a different order from the one they are
// concatenated in, and the code section does not know how many lines the
// declarations above it will take. Counting the assembled text once is exact
// and costs a pass over it.
func resolveDebugLines(src, cfile string) string {
	if !strings.Contains(src, resetDirective) {
		return src
	}
	if cfile == "" {
		// The caller asked for debug information and named no generated file;
		// the name still has to be one a debugger can print.
		cfile = "program.c"
	}
	q := cQuote(cfile)
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if l != resetDirective {
			continue
		}
		// The directive applies to the line after it, which is line i+2: lines
		// are 1-based and lines[i] is the marker itself.
		lines[i] = fmt.Sprintf("#line %d %s", i+2, q)
	}
	return strings.Join(lines, "\n")
}

// stmtPos returns the position a statement was written at. A statement the
// emitter synthesized has none, and the emitter then writes no directive for
// it.
func stmtPos(s ast.Stmt) source.Pos {
	switch v := s.(type) {
	case *ast.Block:
		return v.Pos
	case *ast.LocalVar:
		return v.Pos
	case *ast.LocalClass:
		if v.Decl != nil {
			return v.Decl.Pos
		}
	case *ast.ExprStmt:
		return v.Pos
	case *ast.If:
		return v.Pos
	case *ast.While:
		return v.Pos
	case *ast.DoWhile:
		return v.Pos
	case *ast.For:
		return v.Pos
	case *ast.ForEach:
		return v.Pos
	case *ast.Return:
		return v.Pos
	case *ast.Break:
		return v.Pos
	case *ast.Continue:
		return v.Pos
	case *ast.Throw:
		return v.Pos
	case *ast.Try:
		return v.Pos
	case *ast.Switch:
		return v.Pos
	case *ast.Yield:
		return v.Pos
	case *ast.Labeled:
		return v.Pos
	case *ast.Assert:
		return v.Pos
	case *ast.Sync:
		return v.Pos
	case *ast.Empty:
		return v.Pos
	}
	return source.Pos{}
}

// cQuote renders s as a C string literal.
func cQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString("\\\"")
		case c == '\\':
			b.WriteString("\\\\")
		case c == '\n':
			b.WriteString("\\n")
		case c == '\r':
			b.WriteString("\\r")
		case c == '\t':
			b.WriteString("\\t")
		case c < 32 || c > 126:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
