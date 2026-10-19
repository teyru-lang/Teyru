package driver

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// The C compiler, driven in parallel.
//
// A build's C sources are independent of one another: they share a header and a
// link, not a compilation. Handing the whole list to one C compiler is one
// process working through it one file at a time, and that is what this driver
// used to do -- every `cc` was a single blocking exec.Command(...).Run(), with
// the generated program, the runtime and the standard library's object all on
// the same command line. The driver is the only place that knows how many
// translation units a build has and how many CPUs the machine has, so it is the
// place that decides the parallelism: it compiles the units itself, one
// compiler per unit and up to one per CPU, and links the objects once.
//
// The C compiler's own scheduling is not this. `clang a.c b.c c.c -o exe` does
// compile the three in some order, but it does it in one process, one at a
// time, and no flag this driver can pass changes that: the units have to be
// separate invocations for the cores to be used.

// cUnit is one translation unit of a build: the source to compile and the
// object it compiles to.
type cUnit struct {
	src string
	obj string
	// lead is what this unit needs before its source that the others do not:
	// the LLVM back end's module is IR whatever the caller named the file, so
	// its unit says so, while the runtime beside it is C, which its extension
	// already says.
	lead []string
}

// ccFailure is a unit the C compiler refused: which unit, what the compiler
// said about it, and the exit status.
//
// The text is kept rather than only streamed because one caller has to read it:
// a module the LLVM back end emitted that does not verify is this compiler's
// bug, and telling that apart from a program the compiler refused takes the
// compiler's own words.
type ccFailure struct {
	Src  string // the source of the unit the compiler refused
	Text string // what the C compiler wrote to stderr about it
	Err  error  // the exit status
}

func (e *ccFailure) Error() string { return filepath.Base(e.Src) + ": " + e.Err.Error() }
func (e *ccFailure) Unwrap() error { return e.Err }

// compileJobs is how many C compilers run at once: one per CPU, and never more
// than there are units to compile.
//
// The count is the machine's rather than a flag's because the number that
// matters is the machine's: a build that leaves cores idle is slower for no
// reason, and one that starts more compilers than there are cores spends its
// time switching between them.
func compileJobs(units int) int {
	jobs := runtime.NumCPU()
	if jobs > units {
		jobs = units
	}
	if jobs < 1 {
		jobs = 1
	}
	return jobs
}

// objectsFor pairs each source with the object it compiles to, in a directory
// of the build's own.
//
// The object is named after the unit's position rather than after the source's
// base name: a native source the caller named can share a base name with one of
// the runtime's files, and two units writing one object would be a build that
// fails for a reason that has nothing to do with the program.
func objectsFor(dir string, srcs []string) ([]cUnit, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	units := make([]cUnit, len(srcs))
	for i, src := range srcs {
		units[i] = cUnit{src: src, obj: filepath.Join(dir, strconv.Itoa(i)+".o")}
	}
	return units, nil
}

// compileUnits compiles every unit to its own object, up to compileJobs of them
// at a time, and reports the first failure.
//
// A unit that fails does not stop the others: they are already running, and the
// caller's next move -- report the compiler's own diagnostics and stop the
// build -- does not depend on how many units got as far as an object. Every
// diagnostic the C compiler writes goes to stderr as it is written, so a
// failure still says what is wrong with which file, and the error says which
// unit the exit status belongs to.
func compileUnits(cc string, flags []string, units []cUnit, verbose bool) error {
	if len(units) == 0 {
		return nil
	}
	// The commands are printed from the dispatcher rather than from the workers
	// so that -v's lines do not interleave: two compilers starting at once would
	// otherwise be two half-written lines.
	print := func(u cUnit) {}
	if verbose {
		var mu sync.Mutex
		print = func(u cUnit) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(os.Stderr, "teyru: %s %s\n", cc, strings.Join(compileArgs(flags, u), " "))
		}
	}
	type job struct {
		i int
		u cUnit
	}
	work := make(chan job)
	errs := make([]error, len(units))
	// One buffer per unit, each written by one goroutine: what the compiler
	// said about a unit that failed, for the caller that needs to read it.
	said := make([]bytes.Buffer, len(units))
	var wg sync.WaitGroup
	for range compileJobs(len(units)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				args := compileArgs(flags, j.u)
				cmd := exec.Command(cc, args...)
				// The compiler's own diagnostics are the only thing that says
				// what is wrong with the generated C, and they are what the
				// single-invocation path printed too: without this a failure
				// here reports an exit status and nothing else.
				cmd.Stderr = io.MultiWriter(os.Stderr, &said[j.i])
				errs[j.i] = cmd.Run()
			}
		}()
	}
	for i, u := range units {
		print(u)
		work <- job{i, u}
	}
	close(work)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return &ccFailure{Src: units[i].src, Text: said[i].String(), Err: err}
		}
	}
	return nil
}

// compileArgs is the command line that compiles one unit: the flags every unit
// of the build shares, then whatever this unit needs said about its own source,
// then the source and the object.
func compileArgs(flags []string, u cUnit) []string {
	args := make([]string, 0, len(flags)+len(u.lead)+4)
	args = append(args, flags...)
	args = append(args, u.lead...)
	args = append(args, "-c", u.src, "-o", u.obj)
	return args
}

// objectList is the objects a build produced, in unit order: what the link
// command names where the compile named the sources.
func objectList(units []cUnit) []string {
	objs := make([]string, len(units))
	for i, u := range units {
		objs[i] = u.obj
	}
	return objs
}

// compileAndLink compiles every unit of a build in parallel and links the
// objects, which is the whole of what a build asks a C compiler for.
//
// mode adds that mode's flags to both halves, and it has to be both: the
// compile is what writes the intermediate representation into the object and
// the link is what optimises across the units. The failure a caller retries
// with a lesser mode is the toolchain's, and it can come from either half, so a
// retry redoes both.
func compileAndLink(cc string, cflags, ldflags []string, units []cUnit, target string, mode ltoMode, verbose bool) error {
	cf, ld := cflags, ldflags
	if cfExtra, ldExtra := ltoFlags(cc, target, compileJobs(len(units)), mode); len(cfExtra) > 0 || len(ldExtra) > 0 {
		cf = append(append([]string{}, cfExtra...), cflags...)
		ld = append(append([]string{}, ldExtra...), ldflags...)
	}
	if err := compileUnits(cc, cf, units, verbose); err != nil {
		return fmt.Errorf("C backend failed: %w", err)
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "teyru: %s %s\n", cc, strings.Join(ld, " "))
	}
	cmd := exec.Command(cc, ld...)
	// The linker's diagnostics are what says why a build did not link.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("C backend failed: %w", err)
	}
	return nil
}

// ltoMode is how a build asks for link-time optimisation, and in which form.
//
// The parallel form is the same optimisation as the plain one, done by a
// toolchain that can spread it over the machine's cores. It is the form a
// release build asks for first; the plain form is the retry for a toolchain
// that does not know the parallel spelling.
type ltoMode int

const (
	// ltoOff is a build with no link-time optimisation.
	ltoOff ltoMode = iota
	// ltoPlain is -flto: the linker's own optimisation, in one process.
	ltoPlain
	// ltoParallel is the toolchain's parallel spelling of the same optimisation.
	ltoParallel
)

// ccFamily is the C compiler family, as far as the LTO flags are concerned.
type ccFamily int

const (
	// familyUnknown is a compiler this driver cannot name from its path. Both
	// flags below are accepted by the two families it might be, so the parallel
	// spelling is still asked for.
	familyUnknown ccFamily = iota
	familyGCC
	familyClang
)

// compilerFamily names the C compiler family from the name the caller gave it.
//
// The name is all this can use. Asking the compiler would be another process in
// every build, and the two parallel spellings are chosen by the driver rather
// than probed: a toolchain that rejects the one asked for is retried with the
// plain form, which is what the retry has always been for.
func compilerFamily(cc string) ccFamily {
	name := strings.ToLower(filepath.Base(cc))
	switch {
	case strings.Contains(name, "clang"):
		return familyClang
	case strings.Contains(name, "gcc"), strings.Contains(name, "g++"):
		return familyGCC
	}
	return familyUnknown
}

// hasLLD reports whether an lld the C driver can be pointed at is on PATH.
//
// clang's LTO is parallel only through lld: --lto-partitions splits code
// generation into that many tasks after the optimisation has run. Without lld
// there is nothing to ask, and the plain form is used.
func hasLLD() bool {
	_, err := exec.LookPath("ld.lld")
	return err == nil
}

// ltoFlags is the flags one mode adds to the compile command and to the link
// command, in that order. jobs is the build's parallelism budget, the same
// number compileJobs gives the compilers.
//
// GCC's -flto=auto runs its LTRANS back end on every core, and is the parallel
// spelling that needs nothing but the compiler. clang's -flto is parallel only
// in lld, so the link is given lld and one code-generation partition per job:
// the number is the machine's rather than a constant so that the flags say what
// the build is actually asking for. More partitions than jobs was measured to
// buy nothing -- the serial optimisation is most of an LTO link, and 8, 16 and
// 32 partitions came out within a second of each other -- while moving the hot
// code between layouts, which is worth a few per cent either way.
//
// clang's -flto=thin is not used even though it is the other parallel spelling:
// it parallelises per module, and a release build's program is one generated
// module, so it ran the same work on one core and measured no faster.
func ltoFlags(cc, target string, jobs int, mode ltoMode) (compile, link []string) {
	switch mode {
	case ltoPlain:
		return []string{"-flto"}, []string{"-flto"}
	case ltoParallel:
		switch compilerFamily(cc) {
		case familyGCC:
			return []string{"-flto=auto"}, []string{"-flto=auto"}
		case familyClang:
			if strings.HasPrefix(target, "linux/") && hasLLD() {
				partitions := "-Wl,--lto-partitions=" + strconv.Itoa(jobs)
				return []string{"-flto"}, []string{"-flto", "-fuse-ld=lld", partitions}
			}
		}
		// A compiler this driver cannot name, or a clang whose linker is not
		// lld: -flto=auto is the parallel form where the compiler has one and
		// the plain form where it does not.
		return []string{"-flto=auto"}, []string{"-flto=auto"}
	}
	return nil, nil
}

// ltoAttempts is the order a release build tries its LTO modes in: the parallel
// form, then the plain one when that is a different command line, then no LTO.
//
// The last attempt is what makes a toolchain with no LTO support at all a
// successful build like any other; the middle one is the retry for a toolchain
// that knows -flto but not the parallel spelling. When the two produce the same
// command line the middle attempt is not repeated.
func ltoAttempts(cc, target string, jobs int, lto bool) []ltoMode {
	if !lto {
		return []ltoMode{ltoOff}
	}
	pcf, pld := ltoFlags(cc, target, jobs, ltoParallel)
	ccf, cld := ltoFlags(cc, target, jobs, ltoPlain)
	modes := []ltoMode{ltoParallel}
	if !sameStrings(pcf, ccf) || !sameStrings(pld, cld) {
		modes = append(modes, ltoPlain)
	}
	return append(modes, ltoOff)
}

// ltoName is what a mode is called in a verbose build's retry line.
func ltoName(mode ltoMode) string {
	switch mode {
	case ltoParallel:
		return "parallel LTO"
	case ltoPlain:
		return "-flto"
	}
	return "no LTO"
}
