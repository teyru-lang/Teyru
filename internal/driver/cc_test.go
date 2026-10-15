package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The C compiler runs more than once per build, and more than one of it at a
// time. Neither is visible in the output of a build, so these tests watch the
// two things that are: how many times a compiler was run, and how many of them
// were running at once.
//
// A real C compiler cannot be asked those questions, so the tests use one that
// answers them instead: a shell script that writes a line when it starts and a
// line when it ends, sleeps long enough for its neighbours to start beside it,
// and creates the output file the driver expects. What it does not do is
// compile anything, which is exactly the point -- these tests are about the
// driver's scheduling, and a real compiler's would only be noise on top of it.

// fakeCC writes a stand-in C compiler and returns its path and its log.
//
// The script is /bin/sh, so a test on a platform without one skips: what it
// stands in for is the driver's use of the compiler, not the compiler.
func fakeCC(t *testing.T) (string, *ccLog) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in C compiler is a shell script")
	}
	dir := t.TempDir()
	log := &ccLog{path: filepath.Join(dir, "cc.log")}
	script := `#!/bin/sh
# A C compiler that compiles nothing, and says so twice per run.
log="$TEYRU_FAKE_CC_LOG"
printf 'start %s %s\n' "$$" "$*" >> "$log"
sleep "${TEYRU_FAKE_CC_SLEEP:-0.1}"
out=
prev=
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then
  : > "$out"
fi
printf 'end %s\n' "$$" >> "$log"
case "$*" in
  *FAILME*) exit 1 ;;
esac
exit 0
`
	cc := filepath.Join(dir, "cc")
	if err := os.WriteFile(cc, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEYRU_FAKE_CC_LOG", log.path)
	t.Setenv("TEYRU_FAKE_CC_SLEEP", "0.1")
	return cc, log
}

// ccLog is what the stand-in compiler wrote: one start line per invocation,
// naming the process and the arguments, and one end line, in the order the two
// happened.
type ccLog struct {
	path string
}

// invocation is one run of the C compiler: the arguments it was given, and
// whether it was a compile (it was given -c) or a link.
type invocation struct {
	args []string
	link bool
}

// read parses the log, in order, into the invocations it records.
func (l *ccLog) read(t *testing.T) []invocation {
	t.Helper()
	data, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatalf("the stand-in compiler wrote no log: %v", err)
	}
	var out []invocation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "start ") {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(line, "start "))
		args = args[1:] // drop the pid
		inv := invocation{args: args, link: !contains(args, "-c")}
		out = append(out, inv)
	}
	return out
}

// overlap is the most invocations that were running at once. The log is in the
// order the lines were written, and an invocation's own start always precedes
// its end, so a running count over the whole file is a running count of the
// compilers that were alive at each line.
func (l *ccLog) overlap(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatalf("the stand-in compiler wrote no log: %v", err)
	}
	live, max := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "start "):
			live++
			if live > max {
				max = live
			}
		case strings.HasPrefix(line, "end "):
			live--
		}
	}
	if live != 0 {
		t.Fatalf("the log has %d invocations that started and did not end", live)
	}
	return max
}

func (l *ccLog) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestCompileJobs pins the count: one compiler per CPU, never more compilers
// than there are units, and never fewer than one.
func TestCompileJobs(t *testing.T) {
	cpus := runtime.NumCPU()
	for _, tc := range []struct{ units, want int }{
		{0, 1},
		{1, 1},
		{cpus, cpus},
		{cpus + 1, cpus},
		{4 * cpus, cpus},
	} {
		if got := compileJobs(tc.units); got != tc.want {
			t.Errorf("compileJobs(%d) = %d, want %d (this machine has %d CPUs)", tc.units, got, tc.want, cpus)
		}
	}
}

// TestCompileUnitsParallel compiles more units than the machine has CPUs and
// checks what the compiler saw: every unit compiled exactly once, at most one
// compiler per CPU, and more than one at a time when there is more than one CPU
// and more than one unit.
func TestCompileUnitsParallel(t *testing.T) {
	cc, log := fakeCC(t)
	dir := t.TempDir()
	n := 4 * runtime.NumCPU()
	units := make([]cUnit, n)
	for i := range units {
		units[i] = cUnit{
			src: filepath.Join(dir, fmt.Sprintf("u%d.c", i)),
			obj: filepath.Join(dir, fmt.Sprintf("u%d.o", i)),
		}
		if err := os.WriteFile(units[i].src, []byte("/* a unit */\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := compileUnits(cc, []string{"-O0"}, units, false); err != nil {
		t.Fatalf("compileUnits failed: %v", err)
	}
	inv := log.read(t)
	if len(inv) != n {
		t.Fatalf("the compiler ran %d times for %d units, want %d", len(inv), n, n)
	}
	seen := map[string]int{}
	for _, in := range inv {
		src := in.args[len(in.args)-3] // flags... -c <src> -o <obj>
		if got := in.args[len(in.args)-4]; got != "-c" {
			t.Errorf("invocation %v does not compile a single unit", in.args)
		}
		seen[src]++
	}
	for _, u := range units {
		switch seen[u.src] {
		case 1:
		case 0:
			t.Errorf("%s was never compiled", u.src)
		default:
			t.Errorf("%s was compiled %d times", u.src, seen[u.src])
		}
		if _, err := os.Stat(u.obj); err != nil {
			t.Errorf("%s was not written: %v", u.obj, err)
		}
	}
	if max := log.overlap(t); max > compileJobs(n) {
		t.Errorf("%d compilers ran at once, want at most %d", max, compileJobs(n))
	} else if max < 2 && runtime.NumCPU() >= 2 {
		t.Errorf("only %d compiler ran at once for %d units on a %d-CPU machine", max, n, runtime.NumCPU())
	}
}

// TestCompileUnitsReportsTheUnitThatFailed checks that a unit the compiler
// refuses is named in the error, now that a build runs one compiler per unit
// and the exit status on its own no longer says which file it came from.
func TestCompileUnitsReportsTheUnitThatFailed(t *testing.T) {
	cc, _ := fakeCC(t)
	dir := t.TempDir()
	good := cUnit{src: filepath.Join(dir, "good.c"), obj: filepath.Join(dir, "good.o")}
	bad := cUnit{src: filepath.Join(dir, "FAILME.c"), obj: filepath.Join(dir, "bad.o")}
	err := compileUnits(cc, nil, []cUnit{good, bad}, false)
	if err == nil {
		t.Fatal("compileUnits succeeded on a unit the compiler refused")
	}
	if !strings.Contains(err.Error(), "FAILME.c") {
		t.Errorf("error does not name the failing unit: %v", err)
	}
}

// TestBuildCompilesEveryUnitSeparately is the end-to-end half: a real build of a
// real program, through both build paths, with a compiler that records what it
// was asked to do. Each unit is one invocation, the link is another, and the
// link is given the objects rather than the sources -- which is what makes the
// compiling parallel at all.
func TestBuildCompilesEveryUnitSeparately(t *testing.T) {
	cc, log := fakeCC(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "Hello.teyru")
	if err := os.WriteFile(src, []byte("class Hello {\n  public static void main(String[] args) {\n    System.out.println(\"hi\")\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The standard library's cache belongs to the test, not to the user.
	t.Setenv("TEYRU_CACHE_DIR", filepath.Join(dir, "cache"))
	// -O0 and -O1 are the split build; -O2 is the whole-program one. Both have
	// more than one unit, and both compile each of them itself.
	for _, opt := range []string{"-O0", "-O2"} {
		t.Run(opt, func(t *testing.T) {
			log.reset(t)
			if _, err := Compile([]string{src}, Options{Out: filepath.Join(dir, "hello"+opt), Opt: opt, CC: cc}); err != nil {
				t.Fatalf("build failed: %v", err)
			}
			var compiles, links []invocation
			for _, in := range log.read(t) {
				if in.link {
					links = append(links, in)
				} else {
					compiles = append(compiles, in)
				}
			}
			if len(compiles) < 2 {
				t.Fatalf("%d units were compiled; a build is the program, the runtime and (in a split build) the standard library", len(compiles))
			}
			if len(links) != 1 {
				t.Fatalf("the compiler was run %d times to link, want 1", len(links))
			}
			srcs := map[string]bool{}
			var own []string
			for _, in := range compiles {
				if in.args[len(in.args)-4] != "-c" {
					t.Errorf("a compile invocation does not compile one unit: %v", in.args)
				}
				if s := in.args[len(in.args)-3]; srcs[s] {
					t.Errorf("%s was compiled twice", s)
				} else {
					srcs[s] = true
				}
				if strings.HasSuffix(in.args[len(in.args)-3], ".c") == false {
					t.Errorf("a compile invocation names %q as its source", in.args[len(in.args)-3])
				}
				// Every object a compile writes is one of the build's own, in
				// the directory it made for them, except the standard library's:
				// that one is compiled into the build's directory too, but a
				// split build links it from the cache rather than from there,
				// so the link is not expected to name it.
				if obj := in.args[len(in.args)-1]; strings.Contains(obj, string(filepath.Separator)+"obj"+string(filepath.Separator)) {
					own = append(own, obj)
				}
			}
			if len(own) < 2 {
				t.Fatalf("only %d of %d units compiled into the build's own object directory", len(own), len(compiles))
			}
			// The link names the objects and no source: handing it the sources
			// would be the compiler compiling them one at a time again.
			for _, in := range links {
				for _, a := range in.args {
					if strings.HasSuffix(a, ".c") {
						t.Errorf("the link command names the source %s: %v", a, in.args)
					}
				}
				var missing []string
				for _, obj := range own {
					if !contains(in.args, obj) {
						missing = append(missing, obj)
					}
				}
				if len(missing) > 0 {
					t.Errorf("the link command does not name %v", missing)
				}
			}
			if max := log.overlap(t); max < 2 && runtime.NumCPU() >= 2 && len(compiles) >= 2 {
				t.Errorf("only %d compiler ran at once for %d units on a %d-CPU machine", max, len(compiles), runtime.NumCPU())
			}
		})
	}
}
