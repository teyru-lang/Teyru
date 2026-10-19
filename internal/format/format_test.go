package format

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/source"
)

// sources lists the files the formatter must be idempotent on: the standard
// library and the end-to-end programs.
func sources(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{"../../lib", "../../tests/programs"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) && strings.HasSuffix(dir, "tests/programs") {
				// the tests repository is a submodule; its absence is not a
				// reason to skip the library's own files
				continue
			}
			t.Fatal(err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), Ext) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no sources found")
	}
	return out
}

func TestFormatIdempotent(t *testing.T) {
	for _, path := range sources(t) {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			once, err := Source(path, string(src))
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			twice, err := Source(path, once)
			if err != nil {
				t.Fatalf("format(formatted): %v", err)
			}
			if once != twice {
				t.Fatalf("formatting is not idempotent for %s", path)
			}
		})
	}
}

func TestCommentsPreserved(t *testing.T) {
	for _, path := range sources(t) {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			formatted, err := Source(path, string(src))
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			before := countComments(string(src))
			after := countComments(formatted)
			if before != after {
				t.Fatalf("%s: %d comments before, %d after", path, before, after)
			}
			// The count alone would let one comment be replaced by another;
			// the text of each comment, with indentation removed, must also be
			// the same list in the same order.
			if got, want := commentsOf(formatted), commentsOf(string(src)); !equalStrings(got, want) {
				t.Fatalf("%s: comment text changed", path)
			}
		})
	}
}

// TestFormatParses checks that formatting does not turn a source the parser
// accepts into one it rejects.
func TestFormatParses(t *testing.T) {
	for _, path := range sources(t) {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			formatted, err := Source(path, string(src))
			if err != nil {
				t.Fatalf("format: %v", err)
			}
			before := parseDiags(t, path, string(src))
			after := parseDiags(t, path, formatted)
			if before != after {
				t.Fatalf("%s: %d parse diagnostics before, %d after\n%s", path, before, after, formatted)
			}
		})
	}
}

func parseDiags(t *testing.T, path, text string) int {
	t.Helper()
	diags := &source.Diagnostics{}
	parser.Parse(source.NewFile(path, text), diags)
	return len(diags.List)
}

func TestStyle(t *testing.T) {
	src := `package demo
class Main{
static   void main( String[]args ){
int x=1+2
if(x>2){
System.out.println("hi")// trailing
}
/* keep
   this */
}
}
`
	want := `package demo
class Main {
  static void main(String[] args) {
    int x = 1 + 2
    if (x > 2) {
      System.out.println("hi") // trailing
    }
    /* keep
       this */
  }
}
`
	got, err := Source("demo.teyru", src)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCollect(t *testing.T) {
	dir := t.TempDir()
	must := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(dir, "a.teyru"), "class A {}\n")
	must(filepath.Join(dir, "sub", "b.teyru"), "class B {}\n")
	must(filepath.Join(dir, "sub", "notes.txt"), "not source\n")
	must(filepath.Join(dir, ".git", "hook.teyru"), "class C {}\n")
	must(filepath.Join(dir, "tests", "fixture.teyru"), "class D {}\n")

	files, err := Collect([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.teyru"), filepath.Join(dir, "sub", "b.teyru")}
	if len(files) != len(want) {
		t.Fatalf("collected %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("collected %v, want %v", files, want)
		}
	}
}

func TestRefusesLexError(t *testing.T) {
	if _, err := Source("bad.teyru", "class A { String s = \"unterminated\n}\n"); err == nil {
		t.Fatal("a source the lexer rejects must not be formatted")
	}
}

// commentsOf returns every comment's text with each line's indentation
// removed, so a comment that moved columns still compares equal.
func commentsOf(text string) []string {
	var out []string
	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], `"""`):
			i += 3
			for i < len(text) && !strings.HasPrefix(text[i:], `"""`) {
				i++
			}
			i += 3
		case text[i] == '"' || text[i] == '\'':
			quote := text[i]
			i++
			for i < len(text) {
				if text[i] == '\\' {
					i += 2
					continue
				}
				if text[i] == quote {
					i++
					break
				}
				i++
			}
		case strings.HasPrefix(text[i:], "//"):
			j := i
			for j < len(text) && text[j] != '\n' && text[j] != '\r' {
				j++
			}
			out = append(out, trimLines(text[i:j]))
			i = j
		case strings.HasPrefix(text[i:], "/*"):
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				out = append(out, trimLines(text[i:]))
				return out
			}
			j := i + end + 4
			out = append(out, trimLines(text[i:j]))
			i = j
		default:
			i++
		}
	}
	return out
}

func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "\n")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countComments is an independent counter: it walks the text itself rather
// than reusing the formatter's gap scanner, so a bug in that scanner cannot
// hide a dropped comment.
func countComments(text string) int {
	n := 0
	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], `"""`):
			i += 3
			for i < len(text) && !strings.HasPrefix(text[i:], `"""`) {
				i++
			}
			i += 3
		case text[i] == '"' || text[i] == '\'':
			quote := text[i]
			i++
			for i < len(text) {
				if text[i] == '\\' {
					i += 2
					continue
				}
				if text[i] == quote {
					i++
					break
				}
				i++
			}
		case strings.HasPrefix(text[i:], "//"):
			n++
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				i++
			}
		case strings.HasPrefix(text[i:], "/*"):
			n++
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return n
			}
			i += end + 4
		default:
			i++
		}
	}
	return n
}
