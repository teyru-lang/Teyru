package codegen

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/lib"
)

// A debug build's C has to name the .teyru file the code was written in, and a
// build without -g has to be the C it was before debug information existed.
// This is the check for both halves: the same program emitted twice, with the
// directives only in the first.
func TestDebugLineDirectives(t *testing.T) {
	src := `class DebugMain {
  static int twice(int n) { return n * 2 }
  public static void main(String[] args) {
    int x = twice(2)
    if (x > 0) {
      System.out.println("x=" + x)
    }
  }
}`
	diags := &source.Diagnostics{}
	var files []*ast.File
	for _, f := range lib.Files() {
		files = append(files, parser.Parse(source.NewFile("<lib>/"+f.Name, f.Source), diags))
	}
	files = append(files, parser.Parse(source.NewFile("debug_test.teyru", src), diags))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	prog := sema.Check(files, diags)
	if diags.HasErrors() {
		t.Fatalf("semantic errors in the test program: %v", diags)
	}

	plain, _ := Emit(prog, Debug{})
	if strings.Contains(plain, "#line") {
		t.Error("a build without -g wrote a #line directive")
	}

	debug, _ := Emit(prog, Debug{On: true, CFile: "/tmp/debug_out.c"})
	if strings.Contains(debug, resetDirective) {
		t.Error("a reset marker reached the generated C unresolved")
	}
	abs, err := filepath.Abs("debug_test.teyru")
	if err != nil {
		t.Fatal(err)
	}
	// The lines the program was written at: the two methods and the three
	// statements of main.
	want := map[int]bool{2: false, 3: false, 4: false, 5: false, 6: false}
	lines := strings.Split(debug, "\n")
	resets := 0
	for i, l := range lines {
		if !strings.HasPrefix(l, "#line ") {
			continue
		}
		var n int
		var file string
		if _, err := fmt.Sscanf(l, "#line %d %q", &n, &file); err != nil {
			t.Fatalf("line %d: %q is not a #line directive", i+1, l)
		}
		if file == "/tmp/debug_out.c" {
			resets++
			// A reset names the line after it, which is what keeps the rest of
			// the file attributed to the generated C rather than to the last
			// source line.
			if n != i+2 {
				t.Errorf("line %d: the reset names line %d, but the next line is %d", i+1, n, i+2)
			}
			continue
		}
		// %q in the scan strips the C quoting, which is what is left to compare.
		if file != abs {
			t.Errorf("line %d: directive names %q, want the test source %q", i+1, file, abs)
		}
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for line, seen := range want {
		if !seen {
			t.Errorf("no directive for source line %d", line)
		}
	}
	if resets < 5 {
		t.Errorf("only %d regions returned to the generated file; the mapping would leak", resets)
	}
}

// resolveDebugLines is what turns a marker into a directive naming the
// generated file's next line; the emitter's own output is checked above, and
// this pins the arithmetic it relies on.
func TestResolveDebugLines(t *testing.T) {
	src := "a\n" + resetDirective + "\nb\n" + resetDirective + "\nc"
	got := resolveDebugLines(src, "/tmp/out.c")
	want := "a\n#line 3 \"/tmp/out.c\"\nb\n#line 5 \"/tmp/out.c\"\nc"
	if got != want {
		t.Errorf("resolveDebugLines:\n got %q\nwant %q", got, want)
	}
	// A build with no marker is handed back untouched, which is what makes the
	// pass free when -g is off.
	if got := resolveDebugLines("plain", "/tmp/out.c"); got != "plain" {
		t.Errorf("resolveDebugLines changed text with no marker: %q", got)
	}
}
