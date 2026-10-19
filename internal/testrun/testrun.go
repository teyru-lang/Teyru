// Package testrun implements `teyru test`: it finds the test classes of a
// package, generates the entry point that runs them, compiles the package with
// that entry point and reports the exit status the tests produced.
//
// The tests themselves are ordinary Teyru methods carrying @Test. Nothing here
// reflects at run time: discovery reads the annotations from the source, and
// the generated main calls each method by name, so a test binary is a program
// like any other and a test that ships nothing carries no runner.
package testrun

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/driver"
	"github.com/teyru-lang/Teyru/internal/mod"
	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Exit codes. A test run is a test run, not a compiler invocation: 0 is every
// test passed, 1 is a test failed, and 2 is the package did not compile.
const (
	exitPass  = 0
	exitFail  = 1
	exitError = 2
)

// Diagnostic codes for a test declaration the runner cannot call. TY-TYP-0117
// is the malformed method; TY-TYP-0118 is a test class the generated entry
// point cannot construct.
const (
	diagMethod = "TY-TYP-0117"
	diagClass  = "TY-TYP-0118"
)

// Run discovers and runs the tests under paths and returns the process exit
// code. An empty paths means the current directory, recursively.
func Run(paths []string) int {
	dirs, err := testDirs(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError
	}
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "teyru: no tests found")
		return exitPass
	}
	failed := false
	found := 0
	for _, dir := range dirs {
		code, n := runDir(dir)
		found += n
		if code == exitError {
			return exitError
		}
		if code == exitFail {
			failed = true
		}
	}
	if found == 0 {
		fmt.Fprintln(os.Stderr, "teyru: no tests found")
	}
	if failed {
		return exitFail
	}
	return exitPass
}

// testDirs turns the command's paths into the directories that hold test
// sources, in a fixed order. A directory is walked recursively, as `teyru
// build` walks one; a file contributes the directory it is in.
func testDirs(paths []string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		switch {
		case p == "...":
			return nil, fmt.Errorf("`...` names nothing on its own: write ./... for this directory and the packages under it")
		case strings.HasSuffix(p, "/..."):
			p = strings.TrimSuffix(p, "/...")
			if p == "" {
				p = "."
			}
		}
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			seen[filepath.Dir(p)] = true
			continue
		}
		root := p
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == root {
					return nil
				}
				// Hidden directories hold tool state, and a module inside a
				// module is built when its own root is the subject.
				if strings.HasPrefix(d.Name(), ".") || mod.IsModuleDir(path) {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), mod.SourceExt) {
				seen[filepath.Dir(path)] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out, nil
}

// runDir discovers the tests of one directory, compiles the directory with a
// generated entry point and runs it. The count is the number of test classes
// found, which is zero when the directory has no tests to run.
func runDir(dir string) (status, tests int) {
	files, err := mod.PackageFiles(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, 0
	}
	diags := &source.Diagnostics{}
	pkg, suites := discover(files, diags)
	if diags.HasErrors() {
		fmt.Fprint(os.Stderr, diags.String())
		return exitError, 0
	}
	if len(suites) == 0 {
		return exitPass, 0
	}
	tmp, err := os.MkdirTemp("", "teyru-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, len(suites)
	}
	defer os.RemoveAll(tmp)
	opts := driver.Options{
		Out:   filepath.Join(tmp, "test"),
		CFile: filepath.Join(tmp, "test.c"),
		// A test run is built to run now, like `teyru run`: -O0 compiles the
		// standard library once and links it instead of putting the whole
		// program through link-time optimisation.
		Opt:           "-O0",
		TestEntry:     generate(pkg, suites),
		TestEntryBase: files[0],
	}
	res, err := driver.Compile(files, opts)
	if err != nil {
		if res != nil && res.Diags != nil {
			fmt.Fprint(os.Stderr, res.Diags.String())
		}
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, len(suites)
	}
	code, err := driver.Run(res.Exe, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, len(suites)
	}
	switch code {
	case 0:
		return exitPass, len(suites)
	case 1:
		return exitFail, len(suites)
	default:
		// A test program that dies without the runner's exit status -- a
		// signal, an exit() of its own -- is a failed run, not a compiler
		// error, and saying which status it was is the only clue left.
		fmt.Fprintf(os.Stderr, "teyru: the test program exited with status %d\n", code)
		return exitFail, len(suites)
	}
}

// suite is one class's tests and the methods that surround them.
type suite struct {
	name       string
	pos        source.Pos
	tests      []*ast.MethodDecl
	beforeEach *ast.MethodDecl
	afterEach  *ast.MethodDecl
	beforeAll  *ast.MethodDecl
	afterAll   *ast.MethodDecl
}

// discover parses the directory's sources and returns the package they declare
// and the classes that carry tests. Diagnostics for a method the runner cannot
// call go to diags.
func discover(files []string, diags *source.Diagnostics) (string, []*suite) {
	pkg := ""
	var suites []*suite
	for _, path := range files {
		if !strings.HasSuffix(path, mod.SourceExt) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			diags.Errorf(source.Pos{}, "TY-IO-0001", "cannot read %s: %v", path, err)
			continue
		}
		f := parser.Parse(source.NewFile(path, string(data)), diags)
		if pkg == "" {
			pkg = f.DeclaredPackage
		}
		for _, cd := range f.Types {
			if cd.Kind != ast.KindClass {
				continue
			}
			s := &suite{name: cd.Name, pos: cd.Pos}
			readMembers(s, cd, diags)
			if len(s.tests) > 0 {
				suites = append(suites, s)
			}
		}
	}
	return pkg, suites
}

// readMembers collects a class's annotated methods, reporting the ones a
// generated call could not make.
func readMembers(s *suite, cd *ast.ClassDecl, diags *source.Diagnostics) {
	hasCtor, hasNoArgCtor, privateNoArgCtor := false, false, false
	for _, mem := range cd.Members {
		switch m := mem.(type) {
		case *ast.MethodDecl:
			if m.IsCtor {
				hasCtor = true
				if len(m.Params) == 0 {
					hasNoArgCtor = true
					privateNoArgCtor = m.Mods.Has(ast.ModPrivate)
				}
				continue
			}
			names := annotatedNames(m)
			if len(names) == 0 {
				continue
			}
			if len(names) > 1 {
				diags.Errorf(m.Pos, diagMethod, "test method %s.%s carries more than one of @Test, @BeforeEach, @AfterEach, @BeforeAll and @AfterAll", s.name, m.Name)
				continue
			}
			switch names[0] {
			case "Test":
				if !validMethod(diags, s.name, m, names[0], false) {
					continue
				}
				s.tests = append(s.tests, m)
			case "BeforeEach":
				if !validMethod(diags, s.name, m, names[0], false) {
					continue
				}
				if s.beforeEach != nil {
					diags.Errorf(m.Pos, diagMethod, "test class %s declares more than one @BeforeEach method", s.name)
					continue
				}
				s.beforeEach = m
			case "AfterEach":
				if !validMethod(diags, s.name, m, names[0], false) {
					continue
				}
				if s.afterEach != nil {
					diags.Errorf(m.Pos, diagMethod, "test class %s declares more than one @AfterEach method", s.name)
					continue
				}
				s.afterEach = m
			case "BeforeAll":
				if !validMethod(diags, s.name, m, names[0], true) {
					continue
				}
				if s.beforeAll != nil {
					diags.Errorf(m.Pos, diagMethod, "test class %s declares more than one @BeforeAll method", s.name)
					continue
				}
				s.beforeAll = m
			case "AfterAll":
				if !validMethod(diags, s.name, m, names[0], true) {
					continue
				}
				if s.afterAll != nil {
					diags.Errorf(m.Pos, diagMethod, "test class %s declares more than one @AfterAll method", s.name)
					continue
				}
				s.afterAll = m
			}
		case *ast.ClassDecl:
			// A nested class is a class of its own, and its tests would need
			// an enclosing instance to construct. Say so instead of quietly
			// not running them.
			if hasTests(m) {
				diags.Errorf(m.Pos, diagClass, "test class %s is nested in %s; declare it top level", m.Name, s.name)
			}
		}
	}
	if len(s.tests) > 0 && hasCtor && !hasNoArgCtor {
		diags.Errorf(s.pos, diagClass, "test class %s has no constructor with no parameters", s.name)
	}
	if len(s.tests) > 0 && hasCtor && hasNoArgCtor && privateNoArgCtor {
		diags.Errorf(s.pos, diagClass, "test class %s has a private constructor with no parameters", s.name)
	}
	if len(s.tests) > 0 && cd.Mods.Has(ast.ModAbstract) {
		diags.Errorf(s.pos, diagClass, "test class %s is abstract and cannot be constructed", s.name)
	}
}

// annotatedNames returns the test annotations a method carries, by simple name.
func annotatedNames(m *ast.MethodDecl) []string {
	var out []string
	for _, name := range []string{"Test", "BeforeEach", "AfterEach", "BeforeAll", "AfterAll"} {
		for _, a := range m.Annos {
			if a.Is(name) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// hasTests reports whether a nested class, or any class inside it, carries a
// test annotation.
func hasTests(cd *ast.ClassDecl) bool {
	for _, mem := range cd.Members {
		switch m := mem.(type) {
		case *ast.MethodDecl:
			if len(annotatedNames(m)) > 0 {
				return true
			}
		case *ast.ClassDecl:
			if hasTests(m) {
				return true
			}
		}
	}
	return false
}

// validMethod reports whether the generated entry point can call the method,
// and diagnoses why it cannot.
func validMethod(diags *source.Diagnostics, class string, m *ast.MethodDecl, anno string, allowStatic bool) bool {
	ok := true
	if m.Mods.Has(ast.ModStatic) && !allowStatic {
		diags.Errorf(m.Pos, diagMethod, "@%s method %s.%s must be an instance method", anno, class, m.Name)
		ok = false
	}
	if len(m.Params) != 0 {
		diags.Errorf(m.Pos, diagMethod, "@%s method %s.%s must take no parameters", anno, class, m.Name)
		ok = false
	}
	if m.Mods.Has(ast.ModPrivate) {
		diags.Errorf(m.Pos, diagMethod, "@%s method %s.%s must not be private", anno, class, m.Name)
		ok = false
	}
	if m.Body == nil {
		diags.Errorf(m.Pos, diagMethod, "@%s method %s.%s must have a body", anno, class, m.Name)
		ok = false
	}
	return ok
}

// generate writes the entry point: one main that constructs each test class,
// runs its methods in declaration order and prints a line per test.
func generate(pkg string, suites []*suite) string {
	var b strings.Builder
	if pkg != "" {
		fmt.Fprintf(&b, "package %s\n\n", pkg)
	}
	b.WriteString("// Generated by `teyru test`. This class is the program's entry point;\n")
	b.WriteString("// the package's own main, if it has one, is not called.\n")
	b.WriteString("class __TeyruTestMain {\n")
	b.WriteString("  public static void main(String[] args) {\n")
	b.WriteString("    int passed = 0\n")
	b.WriteString("    int failed = 0\n")
	for _, s := range suites {
		writeSuite(&b, s)
	}
	b.WriteString("    System.out.println(passed + \" passed, \" + failed + \" failed\")\n")
	b.WriteString("    System.exit(failed == 0 ? 0 : 1)\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

func writeSuite(b *strings.Builder, s *suite) {
	fmt.Fprintf(b, "    // %s\n", s.name)
	b.WriteString("    {\n")
	b.WriteString("      boolean __setup = true\n")
	needsInstance := s.beforeAll != nil && !isStatic(s.beforeAll) || s.afterAll != nil && !isStatic(s.afterAll)
	if needsInstance {
		fmt.Fprintf(b, "      %s __all = null\n", s.name)
	}
	if s.beforeAll != nil {
		b.WriteString("      try {\n")
		if !isStatic(s.beforeAll) {
			fmt.Fprintf(b, "        __all = new %s()\n", s.name)
			fmt.Fprintf(b, "        __all.%s()\n", s.beforeAll.Name)
		} else {
			fmt.Fprintf(b, "        %s.%s()\n", s.name, s.beforeAll.Name)
		}
		b.WriteString("      } catch (Throwable __e) {\n")
		b.WriteString("        __setup = false\n")
		b.WriteString("        failed = failed + 1\n")
		fmt.Fprintf(b, "        System.out.println(\"FAIL %s.<beforeAll>: \" + TestReport.message(__e))\n", s.name)
		b.WriteString("        System.out.println(TestReport.describe(__e))\n")
		b.WriteString("      }\n")
	}
	b.WriteString("      if (__setup) {\n")
	for _, t := range s.tests {
		writeTest(b, s, t)
	}
	if s.afterAll != nil {
		b.WriteString("        try {\n")
		if isStatic(s.afterAll) {
			fmt.Fprintf(b, "          %s.%s()\n", s.name, s.afterAll.Name)
		} else {
			b.WriteString("          if (__all == null) {\n")
			fmt.Fprintf(b, "            __all = new %s()\n", s.name)
			b.WriteString("          }\n")
			fmt.Fprintf(b, "          __all.%s()\n", s.afterAll.Name)
		}
		b.WriteString("        } catch (Throwable __e) {\n")
		b.WriteString("          failed = failed + 1\n")
		fmt.Fprintf(b, "          System.out.println(\"FAIL %s.<afterAll>: \" + TestReport.message(__e))\n", s.name)
		b.WriteString("          System.out.println(TestReport.describe(__e))\n")
		b.WriteString("        }\n")
	}
	b.WriteString("      }\n")
	b.WriteString("    }\n")
}

func writeTest(b *strings.Builder, s *suite, t *ast.MethodDecl) {
	b.WriteString("        {\n")
	b.WriteString("          boolean __ok = true\n")
	b.WriteString("          String __msg = \"\"\n")
	b.WriteString("          Throwable __err = null\n")
	fmt.Fprintf(b, "          %s __t = null\n", s.name)
	b.WriteString("          try {\n")
	fmt.Fprintf(b, "            __t = new %s()\n", s.name)
	if s.beforeEach != nil {
		fmt.Fprintf(b, "            __t.%s()\n", s.beforeEach.Name)
	}
	fmt.Fprintf(b, "            __t.%s()\n", t.Name)
	b.WriteString("          } catch (Throwable __e) {\n")
	b.WriteString("            __ok = false\n")
	b.WriteString("            __msg = TestReport.message(__e)\n")
	b.WriteString("            __err = __e\n")
	b.WriteString("          }\n")
	if s.afterEach != nil {
		b.WriteString("          if (__t != null) {\n")
		b.WriteString("            try {\n")
		fmt.Fprintf(b, "              __t.%s()\n", s.afterEach.Name)
		b.WriteString("            } catch (Throwable __e) {\n")
		b.WriteString("              if (__ok) {\n")
		b.WriteString("                __ok = false\n")
		b.WriteString("                __msg = TestReport.message(__e)\n")
		b.WriteString("                __err = __e\n")
		b.WriteString("              }\n")
		b.WriteString("            }\n")
		b.WriteString("          }\n")
	}
	b.WriteString("          if (__ok) {\n")
	b.WriteString("            passed = passed + 1\n")
	fmt.Fprintf(b, "            System.out.println(\"PASS %s.%s\")\n", s.name, t.Name)
	b.WriteString("          } else {\n")
	b.WriteString("            failed = failed + 1\n")
	fmt.Fprintf(b, "            System.out.println(\"FAIL %s.%s: \" + __msg)\n", s.name, t.Name)
	b.WriteString("            System.out.println(TestReport.describe(__err))\n")
	b.WriteString("          }\n")
	b.WriteString("        }\n")
}

func isStatic(m *ast.MethodDecl) bool {
	return m.Mods.Has(ast.ModStatic)
}
