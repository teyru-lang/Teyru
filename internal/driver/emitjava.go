package driver

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/java"
	"github.com/teyru-lang/Teyru/internal/source"
)

// EmitJava prints a Teyru program as the Java source it stands for, which is
// what the JDK differential compiles with javac and compares against this
// compiler's own answer.
//
// Only the program's own files are translated: the standard library is
// rewritten by name into the java.* classes it copies, so printing it would
// print the library rather than the program. What cannot be translated is
// reported as a diagnostic and nothing is printed -- a half-translated program
// would be a differential over a program nobody wrote.
func EmitJava(paths []string, opts Options) (*Result, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	diags := &source.Diagnostics{}
	graph := openModule(paths, diags)
	if diags.HasErrors() {
		return &Result{Diags: diags}, fmt.Errorf("module errors")
	}
	files := []string{}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if path != p && strings.HasPrefix(name, ".") {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, ".teyru") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .teyru source files found")
	}
	program := make([]*ast.File, 0, len(files))
	for _, f := range files {
		program = append(program, parseFile(f, diags))
	}
	if graph != nil {
		program = append(program, resolveImports(graph, program, diags)...)
	}
	if diags.HasErrors() {
		return &Result{Diags: diags}, fmt.Errorf("parse errors")
	}
	// A program that spans more than one compilation unit cannot be printed as
	// one: Java ties a file to the package and the public type in it, and the
	// printer has one output for one invocation. Refusing by name beats
	// printing several units into one file.
	packages := map[string]bool{}
	for _, f := range program {
		if f.DeclaredPackage != "" && f.DeclaredPackage != "teyru" {
			packages[f.DeclaredPackage] = true
		}
	}
	if len(program) > 1 || len(packages) > 0 {
		name := "several files"
		if len(packages) > 0 {
			for p := range packages {
				name = "the package " + p
			}
		}
		diags.Errorf(source.Pos{}, java.DiagCode,
			"cannot emit Java for %s: one emit-java run prints one compilation unit", name)
		return &Result{Diags: diags}, fmt.Errorf("cannot print Java for this program")
	}
	// The standard library's own files are not part of the program: they are
	// inlined by the printer's name table, and printing them would print the
	// library.
	out := java.Translate(program)
	for _, r := range out.Refusals {
		diags.Errorf(r.Pos, java.DiagCode, "%s", r.Message())
	}
	return &Result{Diags: diags, Java: out}, nil
}
