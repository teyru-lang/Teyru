package testrun

// Package testrun implements `teyru test`: it finds the test classes of a
// package, generates the entry point that runs them, compiles the package with
// that entry point and reports the exit status the tests produced.
//
// The tests themselves are ordinary Teyru methods carrying @Test. Nothing here
// reflects at run time: discovery reads the annotations from the source, and
// the generated main calls each method by name, so a test binary is a program
// like any other and a test that ships nothing carries no runner.
//
// What the command adds around that program, because a runner that cannot do
// these four things is a runner whose failures cannot be read:
//
//   - stdout is captured per test. A test that prints cannot interleave its
//     output with the PASS/FAIL lines, and the captured text is printed only
//     when the test fails (or under -v). The capture is native -- redirecting a
//     file descriptor is not expressible in the language -- and lives in
//     TestCapture.
//   - a test that hangs is killed and named. The generated entry point prints a
//     progress line to stderr as each test starts; this process watches those
//     lines, so a timeout reports which test was running instead of "the suite
//     timed out". -timeout is the budget for one test, -deadline the budget for
//     one package directory.
//   - -run selects tests by a regular expression over Class.method (or just the
//     method name) and -tag selects by @Tag.
//   - an invocation that finds no tests at all is an error (exit 2), because a
//     mistyped path must not be green. -no-tests-ok restores the old answer.

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/driver"
	"github.com/teyru-lang/Teyru/internal/mod"
	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Exit codes. A test run is a test run, not a compiler invocation: 0 is every
// test passed, 1 is a test failed, and 2 is the package did not compile (or
// the command line did not name anything runnable).
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

// Default budgets. A test that has not finished in a minute is hung, not slow,
// and the CI step that used to burn ninety minutes on one sleep is what these
// numbers are for. Both can be changed, and 0 turns either one off.
const (
	defaultPerTest  = 60 * time.Second
	defaultDeadline = 10 * time.Minute
)

// progressPrefix marks the line the generated program writes to stderr when a
// test starts. The leading control character keeps it from colliding with a
// test's own stderr output by accident.
const progressPrefix = "\x1eTeyruTest begin "

// options is the parsed command line.
type options struct {
	paths     []string
	run       *regexp.Regexp
	tags      []string
	verbose   bool
	perTest   time.Duration
	deadline  time.Duration
	noTestsOK bool
}

// Run discovers and runs the tests under the command's paths and returns the
// process exit code. Flags are parsed here rather than in main because the test
// command's arguments are its own: `-run` would be an unknown compiler flag
// there.
func Run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError
	}
	dirs, err := testDirs(opts.paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError
	}
	failed := false
	found := 0
	for _, dir := range dirs {
		code, n := runDir(dir, opts)
		found += n
		if code == exitError {
			return exitError
		}
		if code == exitFail {
			failed = true
		}
	}
	if found == 0 {
		if opts.noTestsOK {
			fmt.Fprintln(os.Stderr, "teyru: no tests found")
			return exitPass
		}
		fmt.Fprintln(os.Stderr, "teyru: no tests found (pass -no-tests-ok to accept an empty selection)")
		return exitError
	}
	if failed {
		return exitFail
	}
	return exitPass
}

// parseArgs reads the command's own flags and the paths after them.
func parseArgs(args []string) (*options, error) {
	opts := &options{perTest: defaultPerTest, deadline: defaultDeadline}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			opts.paths = append(opts.paths, args[i+1:]...)
			break
		}
		name, value, hasValue := a, "", false
		if strings.HasPrefix(a, "-") {
			if j := strings.IndexByte(a, '='); j >= 0 {
				name, value, hasValue = a[:j], a[j+1:], true
			}
		}
		take := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "-v", "--verbose":
			opts.verbose = true
		case "-no-tests-ok":
			opts.noTestsOK = true
		case "-run":
			v, err := take()
			if err != nil {
				return nil, err
			}
			re, err := regexp.Compile(v)
			if err != nil {
				return nil, fmt.Errorf("bad -run pattern %q: %v", v, err)
			}
			opts.run = re
		case "-tag":
			v, err := take()
			if err != nil {
				return nil, err
			}
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					opts.tags = append(opts.tags, t)
				}
			}
		case "-timeout":
			v, err := take()
			if err != nil {
				return nil, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("bad -timeout %q: %v", v, err)
			}
			opts.perTest = d
		case "-deadline":
			v, err := take()
			if err != nil {
				return nil, err
			}
			d, err := parseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("bad -deadline %q: %v", v, err)
			}
			opts.deadline = d
		case "-h", "-help", "--help":
			return nil, errors.New(usage)
		default:
			if strings.HasPrefix(name, "-") {
				return nil, fmt.Errorf("unknown flag %s (%s)", name, usage)
			}
			opts.paths = append(opts.paths, a)
		}
	}
	return opts, nil
}

// usage is one line because it is printed as part of another diagnostic.
const usage = "usage: teyru test [-run REGEXP] [-tag NAME[,NAME...]] [-timeout DURATION] [-deadline DURATION] [-v] [-no-tests-ok] [paths...]"

// parseDuration takes Go's duration syntax and, as a convenience for the shell,
// a bare number of seconds.
func parseDuration(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return 0, errors.New("must not be negative")
		}
		return d, nil
	}
	var secs float64
	if _, err := fmt.Sscanf(v, "%f", &secs); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), nil
	}
	return 0, errors.New(`write a duration such as 30s, 500ms or 5m`)
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
// generated entry point and runs it. The count is the number of test methods
// the selection kept, which is zero when the directory has no tests to run.
func runDir(dir string, opts *options) (status, tests int) {
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
	suites = selectSuites(suites, opts)
	n := 0
	for _, s := range suites {
		n += len(s.tests) + len(s.disabled)
	}
	if n == 0 {
		return exitPass, 0
	}
	tmp, err := os.MkdirTemp("", "teyru-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, n
	}
	defer os.RemoveAll(tmp)
	compile := driver.Options{
		Out:   filepath.Join(tmp, "test"),
		CFile: filepath.Join(tmp, "test.c"),
		// A test run is built to run now, like `teyru run`: -O0 compiles the
		// standard library once and links it instead of putting the whole
		// program through link-time optimisation.
		Opt:           "-O0",
		TestEntry:     generate(pkg, suites, opts.verbose),
		TestEntryBase: files[0],
	}
	res, err := driver.Compile(files, compile)
	if err != nil {
		if res != nil && res.Diags != nil {
			fmt.Fprint(os.Stderr, res.Diags.String())
		}
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, n
	}
	code, err := runProgram(res.Exe, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return exitError, n
	}
	switch code {
	case 0:
		return exitPass, n
	case 1:
		return exitFail, n
	default:
		// A test program that dies without the runner's exit status -- a
		// signal, an exit() of its own -- is a failed run, not a compiler
		// error, and saying which status it was is the only clue left.
		fmt.Fprintf(os.Stderr, "teyru: the test program exited with status %d\n", code)
		return exitFail, n
	}
}

// runProgram runs the compiled test binary under the two budgets. stderr is
// read line by line: a progress line names a test that just started and resets
// the per-test clock, and everything else is the test's own stderr and is
// passed through.
func runProgram(exe string, opts *options) (int, error) {
	cmd := exec.Command(exe)
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	progress := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if name, ok := progressName(line); ok {
				progress <- name
				continue
			}
			fmt.Fprintln(os.Stderr, line)
		}
		close(progress)
	}()

	// Two clocks: one reset by every progress line (a single test), one that
	// never resets (the whole directory). A zero budget gives a channel that
	// never fires.
	perTest := newDeadline(opts.perTest)
	whole := newDeadline(opts.deadline)
	defer func() {
		perTest.stop()
		whole.stop()
	}()

	current := ""
	for {
		select {
		case name, ok := <-progress:
			if !ok {
				progress = nil
				continue
			}
			current = name
			perTest.reset(opts.perTest)
		case <-perTest.fired():
			_ = cmd.Process.Kill()
			<-exited
			fmt.Printf("FAIL %s: timed out after %s\n", orProgress(current), opts.perTest)
			return exitFail, nil
		case <-whole.fired():
			_ = cmd.Process.Kill()
			<-exited
			fmt.Printf("FAIL %s: the run exceeded %s\n", orProgress(current), opts.deadline)
			return exitFail, nil
		case err := <-exited:
			if err == nil {
				return 0, nil
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		}
	}
}

// orProgress names the point a killed program had reached.
func orProgress(name string) string {
	if name == "" {
		return "(before the first test)"
	}
	return name
}

// progressName reports whether a stderr line is the runner's own progress
// marker, and the test it names.
func progressName(line string) (string, bool) {
	if !strings.HasPrefix(line, progressPrefix) {
		return "", false
	}
	return strings.TrimPrefix(line, progressPrefix), true
}

// deadline is a timer that can be reset, or that never fires when its duration
// is zero.
type deadline struct {
	t    *time.Timer
	c    <-chan time.Time
	live bool
}

func newDeadline(d time.Duration) *deadline {
	dl := &deadline{}
	if d > 0 {
		dl.t = time.NewTimer(d)
		dl.c = dl.t.C
		dl.live = true
	} else {
		dl.c = make(chan time.Time)
	}
	return dl
}

func (d *deadline) reset(dur time.Duration) {
	if !d.live || dur <= 0 {
		return
	}
	if !d.t.Stop() {
		select {
		case <-d.t.C:
		default:
		}
	}
	d.t.Reset(dur)
}

func (d *deadline) fired() <-chan time.Time { return d.c }

func (d *deadline) stop() {
	if d.live {
		d.t.Stop()
	}
}

// suite is one class's tests and the methods that surround them.
type suite struct {
	name       string
	pos        source.Pos
	tests      []*ast.MethodDecl
	disabled   []disabledTest
	tags       map[*ast.MethodDecl][]string
	classSkip  string // @Disabled on the class itself
	beforeEach *ast.MethodDecl
	afterEach  *ast.MethodDecl
	beforeAll  *ast.MethodDecl
	afterAll   *ast.MethodDecl
}

// disabledTest is one @Test that carries @Disabled.
type disabledTest struct {
	method *ast.MethodDecl
	reason string
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
			s := &suite{name: cd.Name, pos: cd.Pos, tags: map[*ast.MethodDecl][]string{}}
			if reason, ok := annoString(cd.Annos, "Disabled"); ok {
				s.classSkip = reason
			}
			readMembers(s, cd, diags)
			if len(s.tests) > 0 || len(s.disabled) > 0 {
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
				if tags := annoStrings(m.Annos, "Tag"); len(tags) > 0 {
					s.tags[m] = tags
				}
				if reason, ok := annoString(m.Annos, "Disabled"); ok {
					s.disabled = append(s.disabled, disabledTest{method: m, reason: reason})
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
	if len(s.tests)+len(s.disabled) > 0 && hasCtor && !hasNoArgCtor {
		diags.Errorf(s.pos, diagClass, "test class %s has no constructor with no parameters", s.name)
	}
	if len(s.tests)+len(s.disabled) > 0 && hasCtor && hasNoArgCtor && privateNoArgCtor {
		diags.Errorf(s.pos, diagClass, "test class %s has a private constructor with no parameters", s.name)
	}
	if len(s.tests)+len(s.disabled) > 0 && cd.Mods.Has(ast.ModAbstract) {
		diags.Errorf(s.pos, diagClass, "test class %s is abstract and cannot be constructed", s.name)
	}
}

// selectSuites applies -run and -tag. The disabled list is filtered too, so
// `-run` does not print a skip line for a test it did not select.
func selectSuites(suites []*suite, opts *options) []*suite {
	if opts.run == nil && len(opts.tags) == 0 {
		return suites
	}
	out := make([]*suite, 0, len(suites))
	for _, s := range suites {
		cp := *s
		cp.tests = nil
		cp.disabled = nil
		for _, m := range s.tests {
			if opts.selects(s.name, m, s.tags[m]) {
				cp.tests = append(cp.tests, m)
			}
		}
		for _, d := range s.disabled {
			if opts.selects(s.name, d.method, s.tags[d.method]) {
				cp.disabled = append(cp.disabled, d)
			}
		}
		if len(cp.tests)+len(cp.disabled) > 0 {
			out = append(out, &cp)
		}
	}
	return out
}

// selects reports whether one test matches -run and -tag.
func (o *options) selects(class string, m *ast.MethodDecl, tags []string) bool {
	if o.run != nil {
		if !o.run.MatchString(class+"."+m.Name) && !o.run.MatchString(m.Name) && !o.run.MatchString(class) {
			return false
		}
	}
	if len(o.tags) > 0 {
		for _, want := range o.tags {
			for _, have := range tags {
				if want == have {
					return true
				}
			}
		}
		return false
	}
	return true
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

// annoString reads a string annotation element. The second result is whether
// the annotation is present at all, so `@Disabled` with no reason can be told
// from the absence of @Disabled. An element written without a name binds to
// `value`.
func annoString(annos []*ast.Annotation, name string) (string, bool) {
	for _, a := range annos {
		if !a.Is(name) {
			continue
		}
		for _, arg := range a.Args {
			if arg.Name != "" && arg.Name != "value" {
				continue
			}
			if lit, ok := arg.Value.(*ast.Literal); ok && lit.Kind == ast.LitString {
				return lit.Str, true
			}
		}
		return "", true
	}
	return "", false
}

// annoStrings reads every comma-separated string of every @name annotation,
// which is how @Tag("slow,db") selects on two tags with one annotation.
func annoStrings(annos []*ast.Annotation, name string) []string {
	var out []string
	for _, a := range annos {
		if !a.Is(name) {
			continue
		}
		for _, arg := range a.Args {
			if arg.Name != "" && arg.Name != "value" {
				continue
			}
			lit, ok := arg.Value.(*ast.Literal)
			if !ok || lit.Kind != ast.LitString {
				continue
			}
			for _, part := range strings.Split(lit.Str, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
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
func generate(pkg string, suites []*suite, verbose bool) string {
	var b strings.Builder
	if pkg != "" {
		fmt.Fprintf(&b, "package %s\n\n", pkg)
	}
	b.WriteString("// Generated by `teyru test`. This class is the program's entry point;\n")
	b.WriteString("// the package's own main, if it has one, is not called.\n")
	b.WriteString("class __TeyruTestMain {\n")
	fmt.Fprintf(&b, "  private static final boolean __verbose = %t\n", verbose)
	b.WriteString("  public static void main(String[] args) {\n")
	b.WriteString("    int passed = 0\n")
	b.WriteString("    int failed = 0\n")
	b.WriteString("    int skipped = 0\n")
	for _, s := range suites {
		writeSuite(&b, s)
	}
	b.WriteString("    if (skipped > 0) {\n")
	b.WriteString("      System.out.println(passed + \" passed, \" + failed + \" failed, \" + skipped + \" skipped\")\n")
	b.WriteString("    } else {\n")
	b.WriteString("      System.out.println(passed + \" passed, \" + failed + \" failed\")\n")
	b.WriteString("    }\n")
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
		if s.classSkip != "" {
			b.WriteString("        skipped = skipped + 1\n")
			fmt.Fprintf(b, "        System.out.println(%s)\n", teyruString("SKIP "+s.name+"."+t.Name+": "+s.classSkip))
			continue
		}
		writeTest(b, s, t)
	}
	for _, d := range s.disabled {
		b.WriteString("        skipped = skipped + 1\n")
		reason := d.reason
		if reason == "" {
			reason = s.classSkip
		}
		if reason == "" {
			fmt.Fprintf(b, "        System.out.println(%s)\n", teyruString("SKIP "+s.name+"."+d.method.Name))
		} else {
			fmt.Fprintf(b, "        System.out.println(%s)\n", teyruString("SKIP "+s.name+"."+d.method.Name+": "+reason))
		}
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
	fmt.Fprintf(b, "          System.err.println(\"%s%s.%s\")\n", progressPrefix, s.name, t.Name)
	b.WriteString("          TestCapture.begin()\n")
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
	b.WriteString("          String __out = TestCapture.end()\n")
	b.WriteString("          if (__ok) {\n")
	b.WriteString("            passed = passed + 1\n")
	b.WriteString("            if (__verbose) {\n")
	b.WriteString("              TestReport.output(__out)\n")
	b.WriteString("            }\n")
	fmt.Fprintf(b, "            System.out.println(\"PASS %s.%s\")\n", s.name, t.Name)
	b.WriteString("          } else {\n")
	b.WriteString("            failed = failed + 1\n")
	b.WriteString("            TestReport.output(__out)\n")
	fmt.Fprintf(b, "            System.out.println(\"FAIL %s.%s: \" + __msg)\n", s.name, t.Name)
	b.WriteString("            System.out.println(TestReport.describe(__err))\n")
	b.WriteString("          }\n")
	b.WriteString("        }\n")
}

// teyruString renders s as a Teyru string literal.
func teyruString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteString(string(r))
		}
	}
	b.WriteByte('"')
	return b.String()
}

func isStatic(m *ast.MethodDecl) bool {
	return m.Mods.Has(ast.ModStatic)
}
