package testrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	src := generate(pkg, suites)
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
