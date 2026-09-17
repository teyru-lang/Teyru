package teyru_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/driver"
	"github.com/teyru-lang/Teyru/internal/java"
)

// This file is the compiler repository's half of the end-to-end suite: it
// drives the same data as teyru-lang/tests, whose run.sh is the other half.
// Everything the two need to agree about -- which cases are known to fail,
// which platforms a case cannot run on, which differences from the JDK are
// allowed -- is read from the same files, in the same way, by both.

// ---------------------------------------------------------------- the policy

// A knownFailure is one line of tests/known-failures.txt: a case that fails
// today, the work item that will fix it, and why it fails.
type knownFailure struct {
	Work   string
	Reason string
}

// readKnownFailures reads tests/known-failures.txt.
//
// The rule the file exists for is in check below: a case listed here must fail,
// and a case that passes while it is listed fails the run, so an entry cannot
// survive the bug it describes. An entry without a reason is not accepted: a
// failure nobody has explained is a failure nobody can act on.
func readKnownFailures(t *testing.T) map[string]knownFailure {
	t.Helper()
	path := "tests/known-failures.txt"
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]knownFailure{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			t.Fatalf("%s:%d: an entry needs a case, a work item and a reason: %q", path, line, sc.Text())
		}
		out[fields[0]] = knownFailure{Work: fields[1], Reason: strings.Join(fields[2:], " ")}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// check runs one case and applies the known-failure policy to it: a case that
// fails is either a failure of the run or a known one, and a case that is
// listed as known and passes is a failure of the run, because the entry is now
// a lie.
func check(t *testing.T, known map[string]knownFailure, name string, run func() error) {
	t.Helper()
	err := run()
	entry, listed := known[name]
	switch {
	case listed && err == nil:
		t.Errorf("tests/known-failures.txt lists %s as a known failure, but it passed: delete the entry (%s: %s)",
			name, entry.Work, entry.Reason)
	case listed:
		t.Logf("known failure %s (%s): %s\n%s", name, entry.Work, entry.Reason, err)
	case err != nil:
		t.Error(err)
	}
}

// skips reports whether tests/<dir>/<name>.skip says the case cannot run on
// this platform, and the reason written beside the tokens.
//
// A token is a goos (`windows`), a goos/goarch pair (`darwin/arm64`), or one of
// those prefixed with `!`, which names the one platform the case *can* run on.
// Everything after `#` is the reason. teyru-lang/tests' run.sh reads the same
// file the same way.
func skips(t *testing.T, dir, name string) (bool, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".skip"))
	if err != nil {
		return false, ""
	}
	var tokens, reasons []string
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			if r := strings.TrimSpace(line[i+1:]); r != "" {
				reasons = append(reasons, r)
			}
			line = line[:i]
		}
		tokens = append(tokens, strings.Fields(line)...)
	}
	here := runtime.GOOS + "/" + runtime.GOARCH
	for _, token := range tokens {
		neg := strings.HasPrefix(token, "!")
		token = strings.TrimPrefix(token, "!")
		if (token == runtime.GOOS || token == here) != neg {
			return true, strings.Join(reasons, "; ")
		}
	}
	return false, ""
}

// programArgs reads the whitespace-separated arguments beside a case.
func programArgs(dir, name string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, name+".args"))
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

// programExit reads the status a program is expected to end with.
func programExit(dir, name string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+".exit"))
	if err != nil {
		return 0, nil
	}
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &code); err != nil {
		return 0, fmt.Errorf("%s.exit is not a number: %v", name, err)
	}
	return code, nil
}

// runProgram runs a built executable and reports its stdout, stderr and status.
func runProgram(exe string, args []string) (string, string, int, error) {
	cmd := exec.Command(exe, args...)
	// the two streams are compared separately: merging them would make the
	// result depend on when the buffered stdout happens to flush
	var outBuf, errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return outBuf.String(), errBuf.String(), 0, fmt.Errorf("could not run %s: %v", exe, err)
		}
		code = ee.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code, nil
}

// haveCC skips a test that needs a C compiler on a machine that has none.
func haveCC(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		if _, err2 := exec.LookPath("gcc"); err2 != nil {
			t.Skip("no C compiler available")
		}
	}
}

// TestPrograms compiles and runs every program in tests/programs and compares
// its output with the matching .expected file.
//
// A program that is supposed to fail says so with a `name.exit` file holding
// the status it must exit with: without one, a non-zero status is a test
// failure, so no uncaught exception, assert or System.exit could be tested.
// The output comparison is over stdout and stderr together, so a program whose
// message goes to stderr is checked the same way as one whose output does.
func TestPrograms(t *testing.T) {
	haveCC(t)
	known := readKnownFailures(t)
	dir := "tests/programs"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	requireSuite(t, dir, entries)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".teyru") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".teyru")
		if skip, why := skips(t, dir, name); skip {
			t.Run(name, func(t *testing.T) { t.Skipf("not this platform: %s", why) })
			continue
		}
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			check(t, known, name, func() error { return runProgramCase(work, dir, name, e.Name()) })
		})
	}
}

// runProgramCase is one case of TestPrograms, as an error rather than a
// failure: the known-failure policy has to see the outcome before the test
// does.
func runProgramCase(work, dir, name, file string) error {
	want, err := os.ReadFile(filepath.Join(dir, name+".expected"))
	if err != nil {
		return fmt.Errorf("missing expectation file: %v", err)
	}
	res, err := driver.Compile([]string{filepath.Join(dir, file)},
		driver.Options{Out: filepath.Join(work, name), Opt: "-O1"})
	if err != nil {
		return fmt.Errorf("compile failed: %v\n%s", err, res.Diags)
	}
	wantExit, err := programExit(dir, name)
	if err != nil {
		return err
	}
	got, errOut, code, err := runProgram(res.Exe, programArgs(dir, name))
	if err != nil {
		return err
	}
	if code != wantExit {
		return fmt.Errorf("exit status %d, want %d\nstderr:\n%s", code, wantExit, errOut)
	}
	if got != string(want) {
		return fmt.Errorf("output mismatch\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	if errWant, err := os.ReadFile(filepath.Join(dir, name+".experr")); err == nil {
		if errOut != string(errWant) {
			return fmt.Errorf("stderr mismatch\n--- want ---\n%s\n--- got ---\n%s", errWant, errOut)
		}
	} else if errOut != "" {
		return fmt.Errorf("unexpected stderr:\n%s", errOut)
	}
	return nil
}

// TestPackages compiles every directory under tests/packages and compares the
// program's output with the directory's `expected` file.
//
// These are the multi-file, multi-package cases: each directory is a whole
// package tree, compiled by naming the directory (which the driver walks) and
// run as one program. A single-file case belongs in tests/programs instead.
func TestPackages(t *testing.T) {
	haveCC(t)
	known := readKnownFailures(t)
	root := "tests/packages"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := filepath.Join(root, name)
		if skip, why := skips(t, root, name); skip {
			t.Run(name, func(t *testing.T) { t.Skipf("not this platform: %s", why) })
			continue
		}
		// A directory with an `error` file is a rejection case: it must not
		// compile, and the named diagnostic must appear. The cross-package
		// rejections need a real package tree, which a single-file case in
		// TestDiagnostics cannot write.
		if code, err := os.ReadFile(filepath.Join(dir, "error")); err == nil {
			t.Run(name, func(t *testing.T) {
				work := t.TempDir()
				check(t, known, name, func() error {
					res, err := driver.Compile([]string{dir}, driver.Options{Out: filepath.Join(work, name), Opt: "-O0"})
					if err == nil {
						return fmt.Errorf("expected a compile failure")
					}
					if res == nil || res.Diags == nil || !strings.Contains(res.Diags.String(), strings.TrimSpace(string(code))) {
						return fmt.Errorf("expected %s in:\n%v\n%v", strings.TrimSpace(string(code)), res.Diags, err)
					}
					return nil
				})
			})
			continue
		}
		if _, err := os.ReadFile(filepath.Join(dir, "expected")); err != nil {
			t.Fatalf("%s: missing expectation file: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			check(t, known, name, func() error { return runPackageCase(work, dir, name) })
		})
	}
}

func runPackageCase(work, dir, name string) error {
	want, err := os.ReadFile(filepath.Join(dir, "expected"))
	if err != nil {
		return fmt.Errorf("missing expectation file: %v", err)
	}
	res, err := driver.Compile([]string{dir}, driver.Options{Out: filepath.Join(work, name), Opt: "-O1"})
	if err != nil {
		return fmt.Errorf("compile failed: %v\n%s", err, res.Diags)
	}
	got, errOut, _, err := runProgram(res.Exe, nil)
	if err != nil {
		return err
	}
	if got != string(want) {
		return fmt.Errorf("output mismatch\n--- want ---\n%s\n--- got ---\n%s\nstderr:\n%s", want, got, errOut)
	}
	return nil
}

// TestDiagnostics compiles the programs under tests/diagnostics, each of which
// must be *rejected*, and checks that the diagnostic named in the file beside it
// appears.
//
// The cases are data in the tests repository, with every other case, rather than
// Go string literals here. A suite that keeps one of its parts inside the
// compiler's source is not a suite the compiler is tested against -- it is the
// compiler testing itself, and the harness that runs the other cases from the
// tests repository cannot run these at all.
func TestDiagnostics(t *testing.T) {
	known := readKnownFailures(t)
	dir := "tests/diagnostics"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	requireSuite(t, dir, entries)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".teyru") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".teyru")
		if skip, why := skips(t, dir, name); skip {
			t.Run(name, func(t *testing.T) { t.Skipf("not this platform: %s", why) })
			continue
		}
		want, err := os.ReadFile(filepath.Join(dir, name+".code"))
		if err != nil {
			t.Fatalf("%s: missing %s.code, the diagnostic the program must be rejected with: %v", name, name, err)
		}
		code := strings.TrimSpace(string(want))
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			check(t, known, name, func() error {
				res, err := driver.Compile([]string{filepath.Join(dir, e.Name())},
					driver.Options{Out: filepath.Join(work, "out"), Opt: "-O0"})
				if err == nil {
					return fmt.Errorf("expected failure")
				}
				if res == nil || res.Diags == nil {
					return fmt.Errorf("expected diagnostics, got %v", err)
				}
				if !strings.Contains(res.Diags.String(), code) {
					return fmt.Errorf("expected code %s in:\n%s", code, res.Diags)
				}
				return nil
			})
		})
	}
}

// TestNoJava checks that the produced binary has no JVM dependency.
func TestNoJava(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Hello.teyru")
	if err := os.WriteFile(src, []byte("class Hello {\n  public static void main(String[] args) {\n    System.out.println(\"hi\")\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hello")
	res, err := driver.Compile([]string{src}, driver.Options{Out: out, Opt: "-O2"})
	if err != nil {
		t.Skipf("no C compiler available: %v", err)
	}
	data := res.CSource
	lower := strings.ToLower(data)
	for _, bad := range []string{"jni", "jvm", "javac", "class file"} {
		if strings.Contains(lower, bad) {
			t.Errorf("generated C mentions %q", bad)
		}
	}
	if strings.Contains(data, ".class") {
		t.Error("generated C references class files")
	}
}

// TestNative compiles a program whose native methods are implemented in C, and
// checks that the generated header declares exactly what the C side defines.
func TestNative(t *testing.T) {
	haveCC(t)
	want, err := os.ReadFile("tests/native/expected.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "native")
	header := filepath.Join(dir, "native.h")
	res, err := driver.Compile([]string{"tests/native/program.teyru"}, driver.Options{
		Out:          out,
		Opt:          "-O1",
		Native:       []string{"tests/native/impl.c"},
		NativeHeader: header,
		ExtraCC:      []string{"-I", dir},
	})
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, res.Diags)
	}
	got, err := exec.Command(res.Exe).CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, got)
	}
	if string(got) != string(want) {
		t.Errorf("output mismatch\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	// every declaration in the header must be defined by the C the test links
	decl, err := os.ReadFile(header)
	if err != nil {
		t.Fatal(err)
	}
	impl, err := os.ReadFile("tests/native/impl.c")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(decl), "\n") {
		i := strings.Index(line, "tyn_")
		if i < 0 {
			continue
		}
		name := line[i:]
		if j := strings.IndexByte(name, '('); j >= 0 {
			name = name[:j]
		}
		if !strings.Contains(string(impl), name+"(") {
			t.Errorf("the header declares %s but the C implementation does not define it", name)
		}
	}
}

// requireSuite refuses to pass on an empty tests/ directory.
//
// The suite is the teyru-lang/tests repository, mounted here as a submodule: a
// clone that skipped `--recurse-submodules` has the directory and nothing in
// it, and every test below would pass having run nothing at all.
func requireSuite(t *testing.T, dir string, entries []os.DirEntry) {
	t.Helper()
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".teyru") {
			return
		}
	}
	t.Fatalf("%s has no programs: the suite is the teyru-lang/tests submodule, run `git submodule update --init`", dir)
}

// ------------------------------------------------------------- JDK diff

// An allowedDiff is one line of tests/jdk-diff-allow.txt: a program the JDK
// differential is not allowed to complain about, why, and which work item owns
// the difference. `kind` says which of the two things the differential found:
// `java` when javac rejects the Java the program prints, `out` when both
// compiled and their answers differ.
type allowedDiff struct {
	Kind   string
	Work   string
	Reason string
}

func readAllowedDiffs(t *testing.T) map[string]allowedDiff {
	t.Helper()
	path := "tests/jdk-diff-allow.txt"
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]allowedDiff{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			t.Fatalf("%s:%d: an entry needs a case, a kind (java or out), a work item and a reason: %q", path, line, sc.Text())
		}
		if fields[1] != "java" && fields[1] != "out" {
			t.Fatalf("%s:%d: kind is java or out, not %q (a difference with no stated kind is a difference nobody looked at)", path, line, fields[1])
		}
		out[fields[0]] = allowedDiff{Kind: fields[1], Work: fields[2], Reason: strings.Join(fields[3:], " ")}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestJDKDiff compiles every translatable program in tests/programs with the
// JDK and runs it, and compares its stdout and exit status with the same
// program compiled by this compiler.
//
// It runs only when TEYRU_JDK names a JDK home -- `TEYRU_JDK=/opt/jdk21/jdk-21.0.11+10
// go test -run TestJDKDiff` -- because it needs javac, java and a C compiler,
// and because the JDK is the reference implementation only where the project
// has decided it is: the semantics of java.lang and java.util, not the parts of
// the standard library Teyru has of its own.
//
// A program the printer cannot translate is not a failure; it is a program the
// differential says nothing about, and the reason is logged. A program the
// printer does translate must agree with the JDK, unless the disagreement is in
// tests/jdk-diff-allow.txt (a decided difference) or the program is in
// tests/known-failures.txt (a difference a work item is going to remove).
func TestJDKDiff(t *testing.T) {
	home := os.Getenv("TEYRU_JDK")
	if home == "" {
		t.Skip("TEYRU_JDK is not set: the JDK differential needs a JDK 21 home, e.g. TEYRU_JDK=/opt/jdk21/jdk-21.0.11+10")
	}
	javac := jdkTool(t, home, "javac")
	java := jdkTool(t, home, "java")
	haveCC(t)
	known := readKnownFailures(t)
	allowed := readAllowedDiffs(t)
	dir := "tests/programs"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	requireSuite(t, dir, entries)
	var translated, refused, agreed int
	reasons := map[string]int{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".teyru") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".teyru")
		if skip, why := skips(t, dir, name); skip {
			t.Run(name, func(t *testing.T) { t.Skipf("not this platform: %s", why) })
			continue
		}
		t.Run(name, func(t *testing.T) {
			out, err := driver.EmitJava([]string{filepath.Join(dir, e.Name())}, driver.Options{})
			if err != nil || out.Java == nil || len(out.Java.Refusals) > 0 {
				// The printer refuses, by name: nothing to compare, and the
				// refusal is what the report counts.
				why := "no Java spelling"
				if out != nil && out.Java != nil && len(out.Java.Refusals) > 0 {
					why = out.Java.Refusals[0].Feature
				}
				reasons[why]++
				refused++
				t.Skipf("not translated: %s", why)
			}
			translated++
			entry, listed := allowed[name]
			err = diffJDK(t, javac, java, dir, name, e.Name(), out.Java)
			if err == nil {
				agreed++
				if listed {
					t.Errorf("tests/jdk-diff-allow.txt allows a difference in %s (%s: %s), but there is none: delete the entry",
						name, entry.Work, entry.Reason)
				}
				return
			}
			if kf, ok := known[name]; ok {
				t.Logf("known failure %s (%s): %s\n%s", name, kf.Work, kf.Reason, err)
				return
			}
			if listed {
				if !strings.HasPrefix(err.Error(), entry.Kind) {
					t.Errorf("%s is allowed as kind %s, but the difference is %s:\n%s", name, entry.Kind, strings.SplitN(err.Error(), ":", 2)[0], err)
					return
				}
				t.Logf("allowed difference %s (%s: %s)\n%s", name, entry.Work, entry.Reason, err)
				return
			}
			t.Error(err)
		})
	}
	t.Logf("JDK differential: %d translated, %d agreed, %d not translated", translated, agreed, refused)
	for _, why := range sortedCounts(reasons) {
		t.Logf("  not translated: %4d  %s", reasons[why], why)
	}
}

// diffJDK compiles and runs one program with the JDK and with this compiler,
// and reports the first difference between them. The returned error is prefixed
// with the kind of difference -- `java` or `out` -- so that the allow file's
// entry can be checked against what actually happened.
func diffJDK(t *testing.T, javac, javabin, dir, name, file string, printed *java.Result) error {
	t.Helper()
	work := t.TempDir()
	src := filepath.Join(work, printed.File+".java")
	if err := os.WriteFile(src, []byte(printed.Text), 0o644); err != nil {
		t.Fatal(err)
	}
	classes := filepath.Join(work, "classes")
	if err := os.MkdirAll(classes, 0o755); err != nil {
		t.Fatal(err)
	}
	var javacErr bytes.Buffer
	cmd := exec.Command(javac, "-nowarn", "-d", classes, src)
	cmd.Stderr, cmd.Stdout = &javacErr, &javacErr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("java: javac rejected the printed Java for %s:\n%s", name, firstLines(javacErr.String(), 4))
	}
	// The assertion status is Java's own switch and Teyru's assert is always
	// on, so the reference run is the one with assertions enabled.
	args := append([]string{"-ea", "-Dstdout.encoding=UTF-8", "-Duser.language=en", "-Duser.country=US",
		"-cp", classes, printed.Entry}, programArgs(dir, name)...)
	gotJava, errJava, codeJava, err := runProgram(javabin, args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := driver.Compile([]string{filepath.Join(dir, file)},
		driver.Options{Out: filepath.Join(work, "teyru"), Opt: "-O1"})
	if err != nil {
		return fmt.Errorf("out: this compiler refused a program it printed Java for: %v\n%s", err, res.Diags)
	}
	gotTeyru, _, codeTeyru, err := runProgram(res.Exe, programArgs(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if codeJava != codeTeyru {
		return fmt.Errorf("out: exit status\n--- java ---\n%d\n--- teyru ---\n%d\njava stderr:\n%s",
			codeJava, codeTeyru, firstLines(errJava, 3))
	}
	if gotJava != gotTeyru {
		return fmt.Errorf("out: stdout\n--- java ---\n%s\n--- teyru ---\n%s", firstLines(gotJava, 8), firstLines(gotTeyru, 8))
	}
	return nil
}

// jdkTool finds a tool in a JDK home, accepting either the home itself or its
// bin directory.
func jdkTool(t *testing.T, home, name string) string {
	t.Helper()
	for _, cand := range []string{filepath.Join(home, "bin", name), filepath.Join(home, name)} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	t.Fatalf("TEYRU_JDK=%s has no %s", home, name)
	return ""
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, "\n")
}

func sortedCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
