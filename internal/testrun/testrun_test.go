package testrun

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/mod"
	"github.com/teyru-lang/Teyru/internal/source"
)

// writeProject writes files into a fresh directory and returns it.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// discoverDir parses a directory the way runDir does.
func discoverDir(t *testing.T, dir string) (string, []*suite, *source.Diagnostics) {
	t.Helper()
	files, err := mod.PackageFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	diags := &source.Diagnostics{}
	pkg, suites := discover(files, diags)
	return pkg, suites, diags
}

func TestDiscoverFindsTestsAndLifecycle(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"AlphaTest.teyru": `package demo

class AlphaTest {
  @BeforeAll
  public static void once() {
  }
  @BeforeEach
  public void setUp() {
  }
  @AfterEach
  public void tearDown() {
  }
  @AfterAll
  public static void done() {
  }
  @Test
  public void one() {
  }
  @Test
  public void two() {
  }
}
`,
	})
	pkg, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	if pkg != "demo" {
		t.Errorf("package = %q, want demo", pkg)
	}
	if len(suites) != 1 {
		t.Fatalf("found %d suites, want 1", len(suites))
	}
	s := suites[0]
	if s.name != "AlphaTest" {
		t.Errorf("class = %q, want AlphaTest", s.name)
	}
	if len(s.tests) != 2 || s.tests[0].Name != "one" || s.tests[1].Name != "two" {
		t.Errorf("tests = %v, want one and two in declaration order", methodNames(s.tests))
	}
	for name, m := range map[string]string{
		"beforeEach": "setUp",
		"afterEach":  "tearDown",
		"beforeAll":  "once",
		"afterAll":   "done",
	} {
		var got string
		switch name {
		case "beforeEach":
			got = methodName(s.beforeEach)
		case "afterEach":
			got = methodName(s.afterEach)
		case "beforeAll":
			got = methodName(s.beforeAll)
		case "afterAll":
			got = methodName(s.afterAll)
		}
		if got != m {
			t.Errorf("@%s = %q, want %q", name, got, m)
		}
	}
}

func TestDiscoverIgnoresClassesWithoutTests(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"Plain.teyru": `class Plain {
  public void notATest() {
  }
}
`,
	})
	_, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	if len(suites) != 0 {
		t.Errorf("found %d suites, want none", len(suites))
	}
}

func TestDiscoverDiagnosesMalformedMethods(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"BadTest.teyru": `class BadTest {
  @Test
  public static void staticTest() {
  }
  @Test
  public void withParam(int x) {
  }
  @Test
  private void hidden() {
  }
  @Test
  @BeforeEach
  public void twice() {
  }
  @Test
  public native void bodiless()
}
`,
	})
	_, _, diags := discoverDir(t, dir)
	if !diags.HasErrors() {
		t.Fatal("malformed test methods were accepted")
	}
	want := []string{
		"must be an instance method",
		"must take no parameters",
		"must not be private",
		"carries more than one",
		"must have a body",
	}
	text := diags.String()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("diagnostics do not mention %q:\n%s", w, text)
		}
	}
	for _, d := range diags.List {
		if d.Code != diagMethod {
			t.Errorf("%s: code = %s, want %s", d.Pos, d.Code, diagMethod)
		}
	}
}

func TestDiscoverDiagnosesClassWithoutNoArgConstructor(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"CtorTest.teyru": `class CtorTest {
  public CtorTest(int x) {
  }
  @Test
  public void one() {
  }
}
`,
	})
	_, _, diags := discoverDir(t, dir)
	if !diags.HasErrors() {
		t.Fatal("a test class with no no-arg constructor was accepted")
	}
	if !strings.Contains(diags.String(), diagClass) {
		t.Errorf("diagnostics do not carry %s:\n%s", diagClass, diags.String())
	}
}

func TestGenerateCallsTestsAndReports(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"AlphaTest.teyru": `package demo

class AlphaTest {
  @BeforeEach
  public void setUp() {
  }
  @AfterEach
  public void tearDown() {
  }
  @Test
  public void one() {
  }
}
`,
	})
	pkg, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	src := generate(pkg, suites, false)
	for _, want := range []string{
		"package demo",
		"class __TeyruTestMain",
		"__t = new AlphaTest()",
		"__t.setUp()",
		"__t.one()",
		"__t.tearDown()",
		`"PASS AlphaTest.one"`,
		`"FAIL AlphaTest.one: "`,
		`" passed, "`,
		"System.exit(failed == 0 ? 0 : 1)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source does not contain %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "AlphaTest.main") {
		t.Error("the generated entry point calls the package's own main")
	}
	for _, want := range []string{
		"TestCapture.begin()",
		"TestCapture.end()",
		"System.err.println(\"\x1eTeyruTest begin AlphaTest.one\")",
		"TestReport.output(__out)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source does not contain %q:\n%s", want, src)
		}
	}
}

func TestTestDirsWalksRecursively(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"one/A.teyru":         "class A {\n}\n",
		"one/two/B.teyru":     "class B {\n}\n",
		"one/.hidden/C.teyru": "class C {\n}\n",
	})
	dirs, err := testDirs([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Join(dir, "one"):        true,
		filepath.Join(dir, "one", "two"): true,
	}
	if len(dirs) != len(want) {
		t.Fatalf("dirs = %v, want %v", dirs, want)
	}
	for _, d := range dirs {
		if !want[d] {
			t.Errorf("unexpected directory %s", d)
		}
	}
}

func TestTestDirsFileNamesItsDirectory(t *testing.T) {
	dir := writeProject(t, map[string]string{"A.teyru": "class A {\n}\n"})
	dirs, err := testDirs([]string{filepath.Join(dir, "A.teyru")})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != dir {
		t.Errorf("dirs = %v, want [%s]", dirs, dir)
	}
}

func methodNames(ms []*ast.MethodDecl) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func methodName(m *ast.MethodDecl) string {
	if m == nil {
		return ""
	}
	return m.Name
}

func TestParseArgsReadsEveryFlag(t *testing.T) {
	opts, err := parseArgs([]string{
		"-run", "Calculator.*adds",
		"-tag=slow,db",
		"-timeout", "250ms",
		"-deadline=3m",
		"-v",
		"-no-tests-ok",
		"./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.run == nil || !opts.run.MatchString("Calculator.adds") || opts.run.MatchString("Calculator.subtracts") {
		t.Errorf("-run did not compile to the requested expression")
	}
	if len(opts.tags) != 2 || opts.tags[0] != "slow" || opts.tags[1] != "db" {
		t.Errorf("tags = %v, want [slow db]", opts.tags)
	}
	if opts.perTest != 250*time.Millisecond {
		t.Errorf("perTest = %v, want 250ms", opts.perTest)
	}
	if opts.deadline != 3*time.Minute {
		t.Errorf("deadline = %v, want 3m", opts.deadline)
	}
	if !opts.verbose || !opts.noTestsOK {
		t.Errorf("verbose/noTestsOK = %v/%v, want true/true", opts.verbose, opts.noTestsOK)
	}
	if len(opts.paths) != 1 || opts.paths[0] != "./..." {
		t.Errorf("paths = %v, want [./...]", opts.paths)
	}
}

func TestParseArgsRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"-unknown"},
		{"-run"},
		{"-run", "("},
		{"-timeout", "soon"},
		{"-timeout", "-1s"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v) accepted a bad command line", args)
		}
	}
}

func TestParseArgsDefaultsAreFinite(t *testing.T) {
	opts, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.perTest != defaultPerTest || opts.deadline != defaultDeadline {
		t.Errorf("defaults = %v/%v, want %v/%v", opts.perTest, opts.deadline, defaultPerTest, defaultDeadline)
	}
}

func TestDisabledAndTagAreReadFromTheSource(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"SelectTest.teyru": `package demo

@Disabled("whole class")
class SkippedTest {
  @Test
  public void never() {
  }
}

class SelectTest {
  @Test
  @Disabled("needs a database")
  public void disabledTest() {
  }

  @Test
  @Tag("slow,db")
  public void tagged() {
  }

  @Test
  public void plain() {
  }
}
`,
	})
	_, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	var sel *suite
	for _, s := range suites {
		if s.name == "SelectTest" {
			sel = s
		}
	}
	if sel == nil {
		t.Fatal("SelectTest was not discovered")
	}
	if len(sel.disabled) != 1 || sel.disabled[0].method.Name != "disabledTest" || sel.disabled[0].reason != "needs a database" {
		t.Errorf("disabled = %+v, want disabledTest with its reason", sel.disabled)
	}
	if got := sel.tags[sel.tests[0]]; len(got) != 2 || got[0] != "slow" || got[1] != "db" {
		t.Errorf("tags = %v, want [slow db]", got)
	}
	if sel.classSkip != "" {
		t.Errorf("SelectTest took its skip reason from another class: %q", sel.classSkip)
	}
	var skip *suite
	for _, s := range suites {
		if s.name == "SkippedTest" {
			skip = s
		}
	}
	if skip == nil || skip.classSkip != "whole class" {
		t.Fatalf("class-level @Disabled was not read: %+v", skip)
	}
}

func TestSelectSuitesFiltersRunAndTag(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"SelectTest.teyru": `class SelectTest {
  @Test
  public void adds() {
  }

  @Test
  public void subtracts() {
  }

  @Test
  @Tag("slow")
  public void slowOne() {
  }

  @Test
  @Disabled("later")
  public void later() {
  }
}
`,
	})
	_, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	re := regexp.MustCompile("adds")
	got := selectSuites(suites, &options{run: re})
	if len(got) != 1 || len(got[0].tests) != 1 || got[0].tests[0].Name != "adds" {
		t.Fatalf("-run selects = %v, want just adds", suiteNames(got))
	}
	got = selectSuites(suites, &options{tags: []string{"slow"}})
	if len(got) != 1 || len(got[0].tests) != 1 || got[0].tests[0].Name != "slowOne" {
		t.Fatalf("-tag selects = %v, want just slowOne", suiteNames(got))
	}
	got = selectSuites(suites, &options{run: regexp.MustCompile("later")})
	if len(got) != 1 || len(got[0].disabled) != 1 || got[0].disabled[0].method.Name != "later" {
		t.Fatalf("-run did not keep the disabled test it selected: %v", suiteNames(got))
	}
	got = selectSuites(suites, &options{run: regexp.MustCompile("nothing")})
	if len(got) != 0 {
		t.Fatalf("a pattern matching nothing kept %v", suiteNames(got))
	}
}

func suiteNames(suites []*suite) []string {
	out := make([]string, 0, len(suites))
	for _, s := range suites {
		names := methodNames(s.tests)
		for _, d := range s.disabled {
			names = append(names, d.method.Name+"!")
		}
		out = append(out, s.name+":"+strings.Join(names, ","))
	}
	return out
}

func TestGeneratedEntryCapturesAndNames(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"AlphaTest.teyru": `class AlphaTest {
  @Test
  public void one() {
  }
  @Test
  @Disabled("later")
  public void skipped() {
  }
}
`,
	})
	pkg, suites, diags := discoverDir(t, dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	src := generate(pkg, suites, true)
	for _, want := range []string{
		"TestCapture.begin()",
		"TestCapture.end()",
		progressPrefix + "AlphaTest.one",
		"TestReport.output(__out)",
		`"SKIP AlphaTest.skipped: later"`,
		"private static final boolean __verbose = true",
		"skipped = skipped + 1",
		`" passed, " + failed + " failed, " + skipped + " skipped"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source does not contain %q:\n%s", want, src)
		}
	}
}

func TestNoTestsIsAnErrorUnlessAllowed(t *testing.T) {
	empty := t.TempDir()
	if code := Run([]string{empty}); code != exitError {
		t.Errorf("empty directory exit = %d, want %d", code, exitError)
	}
	if code := Run([]string{"-no-tests-ok", empty}); code != exitPass {
		t.Errorf("-no-tests-ok exit = %d, want %d", code, exitPass)
	}
}
