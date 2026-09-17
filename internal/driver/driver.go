// Package driver wires the front end, semantic analysis and the C back end
// into a single compile pipeline.
package driver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/cache"
	"github.com/teyru-lang/Teyru/internal/codegen"
	"github.com/teyru-lang/Teyru/internal/java"
	"github.com/teyru-lang/Teyru/internal/mod"
	"github.com/teyru-lang/Teyru/internal/parser"
	tyrt "github.com/teyru-lang/Teyru/internal/runtime"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
	"github.com/teyru-lang/Teyru/lib"
)

// Options configures a compilation.
type Options struct {
	Out      string // output executable path
	EmitC    string // if set, also write the generated C here
	CFile    string // generated C path (defaults to a sibling .c of Out)
	CC       string // C compiler (default: clang)
	Opt      string // optimisation flag (default -O2)
	EmitLLVM string // if set, also write LLVM IR here (the backend is clang/LLVM)
	// Target is the platform to build for, as "<os>/<arch>". Empty is the
	// machine this compiler is running on, which is what every build was before
	// targets existed. It selects the C compiler, the flags, the platform half
	// of the runtime and the output suffix; see resolveTarget below.
	Target string
	// Backend selects what compiles the program. The C backend is the default
	// and the one the whole standard library is known to work with; the LLVM
	// backend emits the program's own module, so that nothing hands the program
	// to a C compiler, and refuses (loudly) what it cannot lower.
	Backend string
	// CSourceOnly stops the pipeline once the generated C is written: no C
	// compiler runs, so no executable is produced. `teyru emit` sets it to print
	// the C without leaving a binary behind.
	CSourceOnly bool
	NoLTO       bool // disable link-time optimisation (on by default)
	Verbose     bool
	ExtraCC     []string
	KeptTemp    bool
	// Native lists C sources that implement the program's native methods; they
	// are compiled together with the generated program.
	Native []string
	// Link holds extra arguments for the link step, such as -lm or a path to
	// a static library.
	Link []string
	// NativeHeader, when set, receives a C header declaring every native
	// method the program expects to be implemented.
	NativeHeader string
}

// Result reports the outcome of a compilation.
type Result struct {
	CFile    string
	LLVMFile string
	Exe      string
	// CSource is the generated C of this build. It belongs to the result rather
	// than to a file the caller has to read before the build's cleanup removes
	// it: the C itself is scaffolding and only a path the caller passed with -c
	// outlives the build.
	CSource string
	// Java is the equivalent Java source, and is set only by EmitJava. It
	// carries the class to run and the name the file must have, because javac
	// ties both of those to the source rather than to a flag.
	Java  *java.Result
	Diags *source.Diagnostics
}

// Compile turns Teyru sources into a native executable.
func Compile(paths []string, opts Options) (*Result, error) {
	// The target is resolved first so that "no compiler for that platform" is
	// said before a single file is read, let alone a program compiled. opts.CC
	// goes with it: a target this table names no compiler for is built with the
	// one the caller named, which is the only way to build for it at all.
	tgt, err := resolveTarget(opts.Target, opts.CC)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	diags := &source.Diagnostics{}
	// The module a build belongs to is decided before a single file is read:
	// it says what each package's identity is and which directories belong to
	// another module, and both of those shape the walk below.
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
		if st.IsDir() {
			// A directory stands for the package tree rooted at it: every
			// source file underneath belongs to the build -- .teyru, and the
			// .java a Java program is written with -- which is what makes
			// `teyru build ./...` and a layout of one directory per package
			// work without listing files by hand. WalkDir is lexical, so the
			// file order does not depend on the filesystem.
			err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				name := d.Name()
				if d.IsDir() {
					// hidden directories hold tool state, not sources
					if path != p && strings.HasPrefix(name, ".") {
						return fs.SkipDir
					}
					// A module inside a module is a different module: its
					// packages are built when its own root is the subject of
					// the build, not as part of this one.
					if graph != nil && path != p && mod.IsModuleDir(path) {
						return fs.SkipDir
					}
					return nil
				}
				if mod.IsSource(name) {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		files = append(files, p)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s or %s source files found", mod.SourceExt, mod.JavaExt)
	}

	astFiles := parsePrelude(diags)
	program := make([]*ast.File, 0, len(files))
	for _, f := range files {
		program = append(program, parseFile(f, diags))
	}
	// The program's own files are parsed before the packages it imports,
	// because an import of a package of this very module is satisfied by files
	// that are already here, and reading them twice would declare every class
	// in them twice.
	if graph != nil {
		astFiles = append(astFiles, resolveImports(graph, program, diags)...)
	} else {
		astFiles = append(astFiles, program...)
	}
	if diags.HasErrors() {
		return &Result{Diags: diags}, fmt.Errorf("parse errors")
	}
	prog := sema.Check(astFiles, diags)
	if diags.HasErrors() {
		return &Result{Diags: diags}, fmt.Errorf("semantic errors")
	}
	if prog.Main == nil {
		return &Result{Diags: diags}, fmt.Errorf("no entry point: declare a static method named main")
	}
	if opts.NativeHeader != "" {
		if err := writeNativeHeader(opts.NativeHeader, codegen.NativeDecls(prog), codegen.InterfaceSelectors(prog)); err != nil {
			return nil, err
		}
		// A program with native methods cannot link until they are implemented,
		// so `--native-header` with no `--native` source stops here: the caller
		// asked for the prototypes to write an implementation against, and
		// continuing would fail the build with an undefined symbol for a
		// function nobody has written yet. Given an implementation, the same
		// invocation builds it as well, which is what a one-shot build wants.
		if len(opts.Native) == 0 && len(codegen.NativeDecls(prog)) > 0 {
			return &Result{Diags: diags}, nil
		}
	}
	// The LLVM backend emits the program's own module and links it against the
	// runtime, which is the only part of the build that still goes through a C
	// compiler. It refuses a program it cannot lower rather than falling back to
	// the C backend, so a program either builds with it or says why it does not.
	if opts.Backend == BackendLLVM {
		return compileLLVM(prog, opts, diags)
	}
	opt := opts.Opt
	if opt == "" {
		opt = "-O2"
	}
	cc := opts.CC
	if cc == "" {
		// The target's own compiler when it names one -- a cross compiler is not
		// interchangeable with the host's -- and the host's own otherwise, which
		// is the first of clang, gcc and cc that is on PATH, exactly as before.
		cc = tgt.cc
		if cc == "" {
			cc = findCC()
		}
	}
	// A debug build compiles the standard library once and links it, rather than
	// putting the whole program in one translation unit and optimising across
	// it (see compileSplit). The two are the same program: what differs is
	// whether the optimiser sees the standard library's code from the inside.
	if debugOpt(opt) {
		return compileSplit(prog, opts, tgt, opt, cc)
	}
	csrc, link := codegen.Emit(prog)
	// A program whose reachable code can call the TLS layer has to be linked
	// against OpenSSL, and a target that has no OpenSSL for it is refused here,
	// by name and before anything is written, rather than by the linker later.
	if err := checkTLS(tgt, link); err != nil {
		return nil, err
	}

	rtDir, err := os.MkdirTemp("", "teyru-rt-")
	if err != nil {
		return nil, err
	}
	if !opts.KeptTemp {
		defer os.RemoveAll(rtDir)
	}
	// The generated C is scaffolding: it is written into the build's temporary
	// directory unless the caller named a path. Writing it next to the output
	// (`teyru build -o impl prog.teyru` produced impl.c) silently replaced a
	// file of the user's that happened to have that name.
	cfile := opts.CFile
	if cfile == "" {
		cfile = filepath.Join(rtDir, "program.c")
	}
	dir := filepath.Dir(cfile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	rtC := writeRuntime(rtDir, tgt, link)
	if err := os.WriteFile(cfile, []byte(csrc), 0o644); err != nil {
		return nil, err
	}
	if opts.EmitC != "" {
		if err := os.WriteFile(opts.EmitC, []byte(csrc), 0o644); err != nil {
			return nil, err
		}
	}
	// The result reports each output as it is written, so a failure never
	// claims a file that was not produced.
	res := &Result{CFile: cfile, CSource: csrc, Diags: diags}
	// The IR is a file the caller named explicitly, so it is written on every
	// path that gets this far, including the one that skips the C compiler.
	if err := writeLLVMIR(cc, opt, cfile, rtDir, opts.EmitLLVM); err != nil {
		return res, err
	}
	res.LLVMFile = opts.EmitLLVM
	if opts.CSourceOnly {
		// Only the generated C was asked for, so the C compiler is not run at
		// all: it would produce an executable nobody looks at, and leaving one
		// in a temporary directory is exactly what a caller asking for the C
		// does not expect.
		return res, nil
	}
	exe := opts.Out
	// A target whose executables are named by their extension gets one, unless
	// the caller already wrote it: `teyru build -o out prog.teyru --target
	// windows/amd64` produces out.exe, which is a file Windows will run.
	if tgt.suffix != "" && !strings.HasSuffix(exe, tgt.suffix) {
		exe += tgt.suffix
	}
	// -fwrapv: Java's integer arithmetic wraps, and C's is undefined on
	// overflow, which a compiler is free to fold away. It did: `Integer.MIN_VALUE
	// * -1` printed 2147483648 and `-Long.MIN_VALUE` printed 0, because the
	// optimiser answered a question the language says has an answer. Telling the
	// compiler that signed overflow wraps is the same rule Java states.
	base := []string{opt, "-std=gnu11", "-fwrapv", "-fno-strict-aliasing", "-w", "-I", rtDir, cfile}
	base = append(base, strings.Fields(rtC)...)
	base = append(base, opts.Native...)
	base = append(base, tgt.cflags...)
	// The target's own flags, before the libraries: -static has to be seen
	// before the -l that follows it, or the library it is meant to apply to is
	// taken from the dynamic import library instead and the program needs a
	// DLL beside it. ws2_32 is the other one that matters here: the socket
	// layer calls Winsock, and a program that links it without that library
	// does not link at all.
	base = append(base, tgt.ldflags...)
	base = append(base, "-o", exe, "-lm", "-lpthread")
	// OpenSSL, and only for a program that can reach the TLS layer. checkTLS
	// above has already refused the program when the target has none, so
	// tlsLibs is the target's own list here and never an empty one.
	if link.TLS {
		base = append(base, tgt.tlsLibs...)
	}
	base = append(base, opts.Link...)
	base = append(base, opts.ExtraCC...)
	args := append([]string{}, base...)
	// Link-time optimisation lets clang inline runtime helpers (string ops, the
	// allocation fast path) into the generated program. It is on by default and
	// retried without it when the toolchain has no LTO support: only the link
	// flags differ between the two attempts, so a retry is a successful build
	// like any other.
	if !opts.NoLTO {
		args = append([]string{"-flto"}, base...)
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: %s %s\n", cc, strings.Join(args, " "))
	}
	cmd := exec.Command(cc, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if opts.NoLTO {
			return res, fmt.Errorf("C backend failed: %w", err)
		}
		if opts.Verbose {
			fmt.Fprintln(os.Stderr, "teyru: retrying without -flto")
		}
		retry := exec.Command(cc, base...)
		retry.Stderr = os.Stderr
		if err2 := retry.Run(); err2 != nil {
			return res, fmt.Errorf("C backend failed: %w", err)
		}
	}
	res.Exe = exe
	return res, nil
}

// ------------------------------------------------------------------ split builds
//
// A debug build does not put the standard library and the program in one
// translation unit. The standard library's semantic checking and its generated
// C are the same for every program that shares a prelude, a target, an
// optimisation level and a back end, so they are done once and kept in the user
// cache; each build then compiles its own unit against them and links. What
// makes that worth doing is what the standard library costs: hello world's
// generated C is 6.2 MB of which the program is 1.5 KB, so a build that
// recompiles it every time spends its time on code every program shares.
//
// What it gives up is the whole-program view: the optimiser cannot inline the
// standard library into the program, and prune.go decides which vtable slots
// stay filled knowing both units but rewriting only the program's. That is why
// this is the debug path and the default -O2 build is unchanged: a release build
// keeps the whole-program optimisation, and its size and time are what they
// were.
//
// debugOpt is the whole of the policy: the optimisation level decides.
func debugOpt(opt string) bool {
	switch opt {
	case "-O0", "-O1", "-Og":
		return true
	}
	return false
}

// compileSplit builds a program against a standard library compiled once.
func compileSplit(prog *sema.Program, opts Options, tgt *target, opt, cc string) (*Result, error) {
	rtDir, err := os.MkdirTemp("", "teyru-rt-")
	if err != nil {
		return nil, err
	}
	if !opts.KeptTemp {
		defer os.RemoveAll(rtDir)
	}
	// The program's unit first: what it asks for -- reflection, above all --
	// is what the standard library's unit has to carry, and the cache entry is
	// keyed by it.
	start := time.Now()
	unit := codegen.EmitProgram(prog)
	entry := preludeEntry(tgt, opt, cc, unit.Reflect)
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: the program's unit: %s\n", time.Since(start).Round(time.Millisecond))
		start = time.Now()
	}
	libSrc, libObj, err := libraryUnit(entry, rtDir, cc, tgt, opt, unit)
	if err != nil {
		return nil, err
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: the standard library from %s: %s\n", libObj, time.Since(start).Round(time.Millisecond))
		start = time.Now()
	}
	// Which vtable slots stay filled, and whether the program can reach the TLS
	// layer, are questions about the whole program: the answer comes from
	// reading both units, and only the program's is rewritten.
	csrc, link := codegen.Prune(libSrc, unit.Source)
	// A program whose reachable code can call the TLS layer has to be linked
	// against OpenSSL, and a target that has no OpenSSL for it is refused here,
	// by name and before anything is written, rather than by the linker later.
	if err := checkTLS(tgt, link); err != nil {
		return nil, err
	}
	rtC := writeRuntime(rtDir, tgt, link)
	cfile := opts.CFile
	if cfile == "" {
		cfile = filepath.Join(rtDir, "program.c")
	}
	if err := os.MkdirAll(filepath.Dir(cfile), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfile, []byte(csrc), 0o644); err != nil {
		return nil, err
	}
	if opts.EmitC != "" {
		if err := os.WriteFile(opts.EmitC, []byte(csrc), 0o644); err != nil {
			return nil, err
		}
	}
	res := &Result{CFile: cfile, CSource: csrc}
	if err := writeLLVMIR(cc, opt, cfile, rtDir, opts.EmitLLVM); err != nil {
		return res, err
	}
	res.LLVMFile = opts.EmitLLVM
	if opts.CSourceOnly {
		return res, nil
	}
	exe := opts.Out
	if tgt.suffix != "" && !strings.HasSuffix(exe, tgt.suffix) {
		exe += tgt.suffix
	}
	// The same flags a build of one unit uses, plus the two that let the linker
	// do what the optimiser is not there to do: every function and every object
	// in its own section, and the linker dropping the sections nothing
	// reachable refers to. Without them the standard library's unit would be
	// linked whole -- every definition in it is a symbol the program may name,
	// so none of them is file-local -- and a hello world would carry the
	// standard library.
	base := []string{opt, "-std=gnu11", "-fwrapv", "-fno-strict-aliasing", "-w",
		"-ffunction-sections", "-fdata-sections", "-I", rtDir, cfile, libObj}
	base = append(base, strings.Fields(rtC)...)
	base = append(base, opts.Native...)
	base = append(base, tgt.cflags...)
	base = append(base, tgt.ldflags...)
	base = append(base, gcSectionsFlag(tgt)...)
	base = append(base, "-o", exe, "-lm", "-lpthread")
	if link.TLS {
		base = append(base, tgt.tlsLibs...)
	}
	base = append(base, opts.Link...)
	base = append(base, opts.ExtraCC...)
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: %s %s\n", cc, strings.Join(base, " "))
	}
	cmd := exec.Command(cc, base...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("C backend failed: %w", err)
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: compile and link: %s\n", time.Since(start).Round(time.Millisecond))
	}
	res.Exe = exe
	return res, nil
}

// libraryUnit returns the standard library's source and the object to link it
// from, taking both from the cache when they are there.
//
// The two always come from the same entry: an object and a source from
// different builds would be a link that succeeds against definitions the
// program's unit was not emitted for, which is the failure a cache must not
// have. The entry is keyed by everything they were built from, and a file is
// published by rename, so what is read is a whole artifact of some build.
func libraryUnit(entry *cache.Entry, rtDir, cc string, tgt *target, opt string, unit codegen.Program) (string, string, error) {
	writeRuntimeHeaders(rtDir)
	if entry.Has(preludeHeader, "prelude.c", "prelude.o") {
		hdr, ok := entry.Get(preludeHeader)
		if !ok {
			return "", "", fmt.Errorf("cache: %s is missing", preludeHeader)
		}
		src, ok := entry.Get("prelude.c")
		if !ok {
			return "", "", fmt.Errorf("cache: prelude.c is missing")
		}
		if err := os.WriteFile(filepath.Join(rtDir, preludeHeader), hdr, 0o644); err != nil {
			return "", "", err
		}
		return string(src), entry.Path("prelude.o"), nil
	}
	// Nothing cached: the standard library is checked on its own, emitted, and
	// compiled once. It is the same source the program's unit was checked with
	// -- the driver parses lib/*.teyru as the first compilation units of every
	// build -- and checking it alone is what says the two are independent of the
	// program, which is the property the cache depends on.
	diags := &source.Diagnostics{}
	pa := sema.Check(parsePrelude(diags), diags)
	if diags.HasErrors() {
		return "", "", fmt.Errorf("the standard library does not check on its own: %s", diags.String())
	}
	lib := codegen.EmitLibrary(pa, codegen.LibraryOptions{Reflect: unit.Reflect})
	if !sameStrings(lib.Literals, unit.PreludeLiterals) {
		return "", "", fmt.Errorf("internal error: the standard library's string literals and the program's unit disagree "+
			"(%d against %d); a literal is one object only if both agree which unit defines it. This is a compiler bug",
			len(lib.Literals), len(unit.PreludeLiterals))
	}
	srcFile := filepath.Join(rtDir, "prelude.c")
	hdrFile := filepath.Join(rtDir, preludeHeader)
	if err := os.WriteFile(srcFile, []byte(lib.Source), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(hdrFile, []byte(lib.Header), 0o644); err != nil {
		return "", "", err
	}
	objFile := filepath.Join(rtDir, "prelude.o")
	args := []string{opt, "-std=gnu11", "-fwrapv", "-fno-strict-aliasing", "-w",
		"-ffunction-sections", "-fdata-sections", "-I", rtDir, srcFile, "-c", "-o", objFile}
	args = append(args, tgt.cflags...)
	cmd := exec.Command(cc, args...)
	// The compiler's own diagnostics are the only thing that says what is wrong
	// with the generated C, and they are what the whole-program path prints
	// too: without this a failure here reports an exit status and nothing else.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("the standard library's C backend failed: %w", err)
	}
	// A cache that cannot be written is not an error -- nor is there being no
	// cache at all: the object stays where it was built and this build, at
	// least, is a cold one.
	if err := storeLibrary(entry, lib, objFile); err != nil {
		return lib.Source, objFile, nil
	}
	return lib.Source, entry.Path("prelude.o"), nil
}

// storeLibrary puts the three files a standard library build produces into the
// cache entry: the header and the source, which the next build needs to emit its
// own unit and to read the library's call graph, and the object it links.
func storeLibrary(entry *cache.Entry, lib codegen.Library, objFile string) error {
	if entry == nil {
		return fmt.Errorf("no cache")
	}
	if err := entry.Put(preludeHeader, []byte(lib.Header)); err != nil {
		return err
	}
	if err := entry.Put("prelude.c", []byte(lib.Source)); err != nil {
		return err
	}
	return entry.PutFile("prelude.o", objFile)
}

// preludeHeader is the header the standard library's unit declares and the
// program's unit includes, and preludeHeaderFile likewise.
const preludeHeader = "typrelude.h"

// preludeEntry is the cache entry the standard library's unit and object live
// in for one build's settings.
func preludeEntry(tgt *target, opt, cc string, reflect bool) *cache.Entry {
	return cache.Open().Lookup(cache.Key{
		Compiler: cache.Build(version),
		Runtime:  runtimeHash(tgt),
		Prelude:  preludeHash(),
		Target:   tgt.name,
		Opt:      opt,
		Backend:  BackendC,
		// What the artifact depends on that is not in the sources: whether the
		// program can reach reflection, which decides whether the member tables
		// ship, and which C compiler built it, because two compilers produce
		// different objects from the same source.
		Variant: fmt.Sprintf("reflect=%v cc=%s", reflect, filepath.Base(cc)),
	})
}

// preludeHash hashes the standard library's sources: the compiler's own copy of
// lib/*.teyru, which is what its artifacts are built from.
func preludeHash() string {
	files := lib.Files()
	parts := make([]cache.Named, 0, len(files))
	for _, f := range files {
		parts = append(parts, cache.Named{Name: f.Name, Data: []byte(f.Source)})
	}
	return cache.HashOf(parts)
}

// runtimeHash hashes the runtime's sources and headers. The standard library's
// object is compiled against them, so a change to either is a different object.
func runtimeHash(tgt *target) string {
	return cache.HashOf([]cache.Named{
		{Name: "tyrt.h", Data: []byte(tyrt.Header)},
		{Name: "tyrt_plat.h", Data: []byte(tyrt.PlatHeader)},
		{Name: "tyrt.c", Data: []byte(tyrt.Core)},
		{Name: "tyrt2.c", Data: []byte(tyrt.Extra)},
		{Name: "tyrt_net.c", Data: []byte(tyrt.Net)},
		{Name: "tyrt_reflect.c", Data: []byte(tyrt.Reflect)},
		{Name: "tyrt_thread.c", Data: []byte(tyrt.Thread)},
		{Name: "tyrt_tls.c", Data: []byte(tyrt.TLS)},
		{Name: tgt.platSrc, Data: []byte(tgt.platText)},
	})
}

// writeRuntimeHeaders writes the runtime's headers into dir, which is what the
// standard library's unit needs of the runtime: it includes tyrt.h and nothing
// else, and it is compiled before what the whole program links is known.
func writeRuntimeHeaders(dir string) {
	must(os.WriteFile(filepath.Join(dir, "tyrt.h"), []byte(tyrt.Header), 0o644))
	must(os.WriteFile(filepath.Join(dir, "tyrt_plat.h"), []byte(tyrt.PlatHeader), 0o644))
}

// gcSectionsFlag is how a target's linker is told to drop what nothing
// reachable refers to. Only the split build passes it: it is what keeps a
// debug build from carrying the standard library whole, and the release build
// reaches the same place through link-time optimisation with the whole program
// in view.
func gcSectionsFlag(tgt *target) []string {
	if strings.HasPrefix(tgt.name, "darwin/") {
		return []string{"-Wl,-dead_strip"}
	}
	return []string{"-Wl,--gc-sections"}
}

// sameStrings reports whether two sorted lists of strings are the same list.
func sameStrings(a, b []string) bool {
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

// BackendC and BackendLLVM are the two back ends. The C backend is the default:
// it is what the whole standard library is known to build with.
const (
	BackendC    = "c"
	BackendLLVM = "llvm"
)

// compileLLVM compiles a program with the LLVM backend: the module is written by
// this compiler (codegen.EmitLLVM), and the C compiler is handed only the
// runtime, which is C. A refusal is a diagnostic, not a fallback.
func compileLLVM(prog *sema.Program, opts Options, diags *source.Diagnostics) (*Result, error) {
	res := &Result{Diags: diags}
	// The target decides which half of the runtime is compiled beside the module
	// and what the output is called, exactly as it does for the C back end.
	tgt, err := resolveTarget(opts.Target, opts.CC)
	if err != nil {
		return nil, err
	}
	// The module this back end writes carries a target triple and calls this
	// machine's C library and the runtime's C directly, so it is right for the
	// platform it was written for and for no other. Handing it to another
	// platform's linker would produce something wrong in a way nobody sees until
	// it runs there, so a target it was not written for is refused here.
	if tgt.name != "linux/amd64" {
		diags.Errorf(source.Pos{}, "TY-INT-0101",
			"the llvm back end compiles for linux/amd64 only (%s was asked for); use the c back end for other platforms", tgt.name)
		return res, fmt.Errorf("the llvm backend has no such target")
	}
	ir, refusal := codegen.EmitLLVM(prog)
	if refusal != nil {
		diags.Errorf(refusal.Pos, refusal.Code, "%s", refusal.Message())
		return res, fmt.Errorf("the llvm backend cannot compile this program")
	}
	// What the module can reach is what this back end wrote, so the same
	// question the C back end's scan asks is asked of the IR -- and the same
	// answer is used: the program is refused by name on a target with no
	// OpenSSL, and linked against it otherwise.
	link := codegen.LinkForIR(ir)
	if err := checkTLS(tgt, link); err != nil {
		return res, err
	}
	rtDir, err := os.MkdirTemp("", "teyru-rt-")
	if err != nil {
		return nil, err
	}
	if !opts.KeptTemp {
		defer os.RemoveAll(rtDir)
	}
	// The module is this build's scaffolding, in the same sense the C backend's
	// generated C is: written into the temporary directory unless the caller
	// named a path.
	mfile := opts.CFile
	if mfile == "" {
		mfile = filepath.Join(rtDir, "program.ll")
	}
	if err := os.MkdirAll(filepath.Dir(mfile), 0o755); err != nil {
		return nil, err
	}
	rtC := writeRuntime(rtDir, tgt, link)
	if err := os.WriteFile(mfile, []byte(ir), 0o644); err != nil {
		return nil, err
	}
	if opts.EmitC != "" {
		// `-c` asks for the output of the back end that ran, which for this back
		// end is the module.
		if err := os.WriteFile(opts.EmitC, []byte(ir), 0o644); err != nil {
			return nil, err
		}
	}
	res.CFile = mfile
	res.CSource = ir
	if opts.EmitLLVM != "" {
		// The module is written here rather than asked of clang: this is the
		// module the compiler emitted.
		if err := os.WriteFile(opts.EmitLLVM, []byte(ir), 0o644); err != nil {
			return nil, err
		}
		res.LLVMFile = opts.EmitLLVM
	}
	if opts.CSourceOnly {
		// only the module was asked for, so nothing is linked
		return res, nil
	}
	opt := opts.Opt
	if opt == "" {
		opt = "-O2"
	}
	cc := opts.CC
	if cc == "" {
		cc = tgt.cc
		if cc == "" {
			cc = findCC()
		}
	}
	// -fwrapv and -fno-strict-aliasing are the runtime's, not the program's: the
	// generated module states its own wrapping arithmetic and its own loads.
	// -x ir names the module as IR whatever it was called, and -x none ends that
	// so the runtime is still read as C.
	base := []string{opt, "-std=gnu11", "-fwrapv", "-fno-strict-aliasing", "-w", "-I", rtDir,
		"-x", "ir", mfile, "-x", "none"}
	base = append(base, strings.Fields(rtC)...)
	base = append(base, opts.Native...)
	base = append(base, tgt.cflags...)
	base = append(base, tgt.ldflags...)
	exe := opts.Out
	if tgt.suffix != "" && !strings.HasSuffix(exe, tgt.suffix) {
		exe += tgt.suffix
	}
	base = append(base, "-o", exe, "-lm", "-lpthread")
	if link.TLS {
		base = append(base, tgt.tlsLibs...)
	}
	base = append(base, opts.Link...)
	base = append(base, opts.ExtraCC...)
	args := append([]string{}, base...)
	// Link-time optimisation lets clang inline the runtime's helpers (the
	// allocation fast path's slow half, the string operations) into the module,
	// which is what the C backend's build gets. It is retried without it when
	// the toolchain has no LTO support: only the link flags differ.
	if !opts.NoLTO {
		args = append([]string{"-flto"}, base...)
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: llvm backend: %s %s\n", cc, strings.Join(args, " "))
	}
	// The compiler's stderr is read rather than streamed, because a module the
	// back end emitted that does not verify is this compiler's bug and has to be
	// reported as one: otherwise the program's author sees a parse error in a
	// file they never wrote and cannot tell it apart from a limit of the back
	// end.
	var errOut bytes.Buffer
	cmd := exec.Command(cc, args...)
	cmd.Stderr = io.MultiWriter(os.Stderr, &errOut)
	if err := cmd.Run(); err != nil {
		if opts.NoLTO {
			return res, backendFailure(errOut.String(), mfile, err)
		}
		if opts.Verbose {
			fmt.Fprintln(os.Stderr, "teyru: retrying without -flto")
		}
		retry := exec.Command(cc, base...)
		retry.Stderr = os.Stderr
		if err2 := retry.Run(); err2 != nil {
			return res, backendFailure(errOut.String(), mfile, err)
		}
	}
	res.Exe = exe
	return res, nil
}

// backendFailure reports a failed build. A module this compiler wrote that the
// LLVM parser rejects is a defect in the back end, so it is named as one: the
// diagnostic says which line did not verify, and says that the file is the
// compiler's output rather than the program.
func backendFailure(errText, mfile string, err error) error {
	if strings.Contains(errText, mfile) {
		line := ""
		for _, l := range strings.Split(errText, "\n") {
			if strings.HasPrefix(l, mfile) {
				line = l
				break
			}
		}
		return fmt.Errorf("the llvm backend emitted a module that does not verify, which is a bug in the backend and not a limit of it: %s", line)
	}
	return fmt.Errorf("llvm backend failed: %w", err)
}

// writeLLVMIR asks the C compiler for the LLVM module of the generated C. The
// backend is clang, whose middle and back end are LLVM, so the module-level IR
// can be inspected or fed to llc/opt directly. An empty path means the caller
// asked for no IR, so callers can invoke this on every path that produces an
// output without checking first.
func writeLLVMIR(cc, opt, cfile, rtDir, out string) error {
	if out == "" {
		return nil
	}
	irArgs := []string{"-S", "-emit-llvm", "-std=gnu11", "-fwrapv", "-fno-strict-aliasing", "-w",
		"-I", rtDir, cfile, "-o", out}
	if opt != "" {
		irArgs = append(irArgs, opt)
	}
	ir := exec.Command(cc, irArgs...)
	ir.Stderr = os.Stderr
	if err := ir.Run(); err != nil {
		return fmt.Errorf("LLVM IR emission failed: %w", err)
	}
	return nil
}

// parsePrelude reads the standard library, which is Teyru source shipped with
// the compiler, as the first compilation units of every program.
func parsePrelude(diags *source.Diagnostics) []*ast.File {
	files := lib.Files()
	out := make([]*ast.File, 0, len(files))
	for _, f := range files {
		out = append(out, parseSource("<lib>/"+f.Name, f.Source, diags))
	}
	return out
}

func parseFile(path string, diags *source.Diagnostics) *ast.File {
	data, err := os.ReadFile(path)
	if err != nil {
		diags.Errorf(source.Pos{}, "TY-IO-0001", "cannot read %s: %v", path, err)
		return &ast.File{Src: source.NewFile(path, "")}
	}
	return parseSource(path, string(data), diags)
}

func parseSource(path, text string, diags *source.Diagnostics) *ast.File {
	// The parser reads a module path (`example.com/dep/pkg`) as readily as a
	// dotted one, so the source goes in as it is written on disk and every
	// position in a diagnostic is the file's own.
	return parser.Parse(source.NewFile(path, text), diags)
}

// ------------------------------------------------------------- module mode
//
// A build is a module build when there is a teyru.mod above its sources. It
// then resolves an import path the way Go does -- the module path plus a
// directory inside it -- and pulls the packages it names out of the module
// cache. A build with no teyru.mod above it compiles exactly the files it was
// given, which is what a single file on a command line has always done.

// openModule finds the module a build belongs to. A build outside a module is
// not an error: the compiler has always been usable on one file with no
// project around it. A module file that is there and cannot be read is an
// error, because the build would otherwise silently ignore every import that
// names a package the cache holds.
func openModule(paths []string, diags *source.Diagnostics) *mod.Graph {
	dir := ""
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.IsDir() {
			dir = p
		} else {
			dir = filepath.Dir(p)
		}
		break
	}
	if dir == "" {
		dir = "."
	}
	g, err := mod.LoadGraph(dir)
	if err == nil {
		return g
	}
	var noMod *mod.NoModuleError
	if errors.As(err, &noMod) {
		return nil
	}
	diags.Errorf(source.Pos{}, "TY-IO-0101", "%v", err)
	return nil
}

// moduleResolver gives every file of a build the identity its import path
// says it has, and loads the packages the program imports from the cache.
type moduleResolver struct {
	graph *mod.Graph
	diags *source.Diagnostics
	// seen holds the files already part of the build, by absolute path. An
	// import that is satisfied by a file the walk already collected must not
	// add it a second time: the same class declared twice is a duplicate
	// definition, and the emitter would write its C twice.
	seen map[string]bool
	// dirs remembers which package each directory declares, so that one
	// directory holding two packages is reported instead of being silently
	// merged into one.
	dirs map[string]*dirPkg
	deps []*ast.File
}

type dirPkg struct {
	name string
	file string
}

// resolveImports returns the program's files followed by the files of every
// package they import, all of them positioned in the module they belong to.
func resolveImports(graph *mod.Graph, program []*ast.File, diags *source.Diagnostics) []*ast.File {
	r := &moduleResolver{graph: graph, diags: diags, seen: map[string]bool{}, dirs: map[string]*dirPkg{}}
	for _, f := range program {
		abs := absPath(f.Src.Path)
		r.seen[abs] = true
		r.place(f, abs, "")
	}
	// The queue grows while it is walked: a package pulled in for one import
	// has imports of its own, which may pull in more packages.
	queue := append([]*ast.File(nil), program...)
	for i := 0; i < len(queue); i++ {
		for _, imp := range queue[i].Imports {
			pkg := r.resolveImport(imp)
			if pkg == nil {
				continue
			}
			queue = append(queue, r.loadPackage(pkg)...)
		}
	}
	out := append([]*ast.File(nil), program...)
	return append(out, r.deps...)
}

// resolveImport turns an import of a module package into the import path the
// checker resolves names through. Anything that is not a module import -- the
// prelude's `java.*` spellings, a package of the program compiled without a
// module -- is left exactly as written.
func (r *moduleResolver) resolveImport(imp *ast.Import) *mod.Pkg {
	pkg, isModule, err := r.graph.Resolve(imp.Path)
	if err != nil {
		code := "TY-IO-0102"
		var mismatch *mod.SumMismatchError
		if errors.As(err, &mismatch) {
			code = "TY-IO-0103"
		}
		r.diags.Errorf(imp.Pos, code, "%v", err)
		return nil
	}
	if !isModule {
		return nil
	}
	// The path is rewritten to the module path with slashes: the package's own
	// files are named that way below, and the checker splits an import path at
	// its last dot to find the type in it.
	imp.Path = pkg.Canonical()
	if len(pkg.Tail) == 0 && !imp.Static {
		// `import example.com/dep/pkg` names the package, not one type in it:
		// that is an on-demand import of every name the package declares.
		imp.Star = true
	}
	return pkg
}

// loadPackage parses the sources of one package, unless they are already part
// of the build, and gives them the package's import path as their identity.
func (r *moduleResolver) loadPackage(pkg *mod.Pkg) []*ast.File {
	files, err := mod.PackageFiles(pkg.Dir)
	if err != nil {
		r.diags.Errorf(source.Pos{}, "TY-IO-0102", "cannot read package %s: %v", pkg.ImportPath, err)
		return nil
	}
	var out []*ast.File
	for _, path := range files {
		abs := absPath(path)
		if r.seen[abs] {
			continue
		}
		r.seen[abs] = true
		f := parseFile(path, r.diags)
		r.place(f, abs, pkg.ImportPath)
		out = append(out, f)
		r.deps = append(r.deps, f)
	}
	return out
}

// place gives a file the package identity its directory has: the import path
// of that directory, for a file inside the main module, or the import path of
// the package it was loaded from. A declared package name is a name, not an
// identity, and two modules may both declare `package util` -- the identity is
// what keeps them apart.
func (r *moduleResolver) place(f *ast.File, abs, importPath string) {
	if importPath == "" {
		p, ok := r.graph.MainPackagePath(filepath.Dir(abs))
		if !ok {
			// A file from outside the module: it keeps the package it declares,
			// because there is no import path that could name it.
			return
		}
		importPath = p
	}
	if name := f.Package; name != "" {
		dir := filepath.Dir(abs)
		if prev := r.dirs[dir]; prev == nil {
			r.dirs[dir] = &dirPkg{name: name, file: abs}
		} else if prev.name != name {
			r.diags.Errorf(source.Pos{}, "TY-IO-0104",
				"%s declares package %s, but %s in the same directory declares %s", abs, name, prev.file, prev.name)
		}
	}
	f.Package = importPath
}

// absPath is the key a file is held under: one file is one compilation unit,
// however it was reached.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

// writeNativeHeader writes the C prototypes a program has to implement, so
// that native methods can be written against a declaration the compiler
// generated instead of a name the author has to guess.
func writeNativeHeader(path string, decls []codegen.NativeDecl, sels []codegen.SelectorDecl) error {
	var b strings.Builder
	b.WriteString("/* native methods declared by this program.\n")
	b.WriteString("   Implement each one and pass the file back with `--native <file.c>`. */\n\n")
	b.WriteString("#ifndef TEYRU_NATIVE_H\n#define TEYRU_NATIVE_H\n\n")
	b.WriteString("#include \"tyrt.h\"\n\n")
	if len(decls) == 0 {
		b.WriteString("/* this program declares no native methods */\n")
	}
	for _, d := range decls {
		fmt.Fprintf(&b, "/* %s */\n%s;\n\n", d.Method, d.Signature)
	}
	if len(sels) > 0 {
		b.WriteString("/* Dispatch selectors of interface methods. A native method can call\n")
		b.WriteString("   back into Teyru with\n")
		b.WriteString("     ((int32_t (*)(void *, int32_t)) ty_itab(obj, SEL))(obj, arg) */\n\n")
		for _, s := range sels {
			name := "TY_SEL_" + mangleForHeader(s.Iface) + "_" + mangleForHeader(s.Method)
			if s.Params != "" {
				name += "_" + mangleForHeader(s.Params)
			}
			fmt.Fprintf(&b, "#define %s %d\n", name, s.Selector)
		}
		b.WriteString("\n")
	}
	b.WriteString("#endif\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

// mangleForHeader turns a Teyru name into the upper case identifier used in
// the generated header.
func mangleForHeader(s string) string {
	return strings.ToUpper(util.Mangle(s))
}

// -------------------------------------------------------- target support
//
// Everything that differs between the platforms this compiler can build for is
// in the table below. The generated C is the same for every target, and so are
// five of the runtime's six translation units: what a target changes is which C
// compiler runs, which flags it is given, which half of the runtime is compiled
// beside the rest, and what the output is called. A target that is not the
// machine this is: the compiler here is a cross compiler, and a build that
// cannot find one says so instead of producing something that does not run.

// target is one row of the table: a platform, and what building for it takes.
type target struct {
	name string
	// cc is the compiler to look for. Empty means the host's own -- the first of
	// clang, gcc and cc that is on PATH -- which is what a build with no target
	// has always used. A target whose OS differs from the host's must name one:
	// an ordinary cc cannot produce a program for another operating system.
	cc string
	// platSrc and platText are the platform half of the runtime: the file name
	// the build writes it under, and its text, which is embedded in the compiler
	// (internal/runtime/embed.go).
	platSrc  string
	platText string
	// suffix is appended to an output path that does not already end in it.
	suffix string
	// cflags and ldflags are added to the compile and link command lines. Empty
	// for most targets: the platform is detected by the C preprocessor's own
	// _WIN32 rather than by a define this table hands it.
	cflags  []string
	ldflags []string
	// tlsLibs are the libraries a build for this target links when the program
	// can reach the TLS layer. Empty means this target has no TLS: a program
	// that reaches it is refused by name (checkTLS below) instead of being
	// linked against a library that is not there, which is a failure whose
	// message is the linker's rather than the program author's.
	tlsLibs []string
	// tlsWhy is what that refusal says about the target. Empty for a target with
	// tlsLibs.
	tlsWhy string
}

// targets is the list of platforms this compiler knows how to build for. It is
// not a promise that each one has been run: linux/amd64 is the suite's, and a
// target that needs a cross toolchain this machine does not have fails at the
// compiler, with the compiler's own error, rather than silently.
var targets = map[string]*target{
	"linux/amd64": {
		name:     "linux/amd64",
		platSrc:  "tyrt_plat_posix.c",
		platText: tyrt.PlatPosix,
		// OpenSSL is here, so the TLS layer is: a program that reaches it is
		// compiled with internal/runtime/src/tyrt_tls.c and linked against
		// these two. -lssl does not pull in -lcrypto on every platform's
		// linker, and libssl's own symbols are not enough: the X509 and EVP
		// calls this layer makes are libcrypto's.
		tlsLibs: []string{"-lssl", "-lcrypto"},
	},
	"linux/arm64": {
		name:     "linux/arm64",
		cc:       "aarch64-linux-gnu-gcc",
		platSrc:  "tyrt_plat_posix.c",
		platText: tyrt.PlatPosix,
		// The same two libraries, for that architecture: a cross build needs
		// the target's OpenSSL installed, which is what the linker will say if
		// it is not there. This compiler cannot know where a cross toolchain
		// keeps its libraries, and guessing would be worse than the error.
		tlsLibs: []string{"-lssl", "-lcrypto"},
	},
	"windows/amd64": {
		name:     "windows/amd64",
		cc:       "x86_64-w64-mingw32-gcc",
		platSrc:  "tyrt_plat_win.c",
		platText: tyrt.PlatWin,
		suffix:   ".exe",
		// -lws2_32 is Winsock, which the socket layer calls. -static is what
		// makes the program stand alone: the threads are winpthreads, and
		// without it every program this compiler produced would need
		// libwinpthread-1.dll next to it.
		ldflags: []string{"-lws2_32", "-static"},
		// mingw-w64 has no OpenSSL: there is no libssl.a or libssl.dll for the
		// target, so the TLS layer cannot be built for it at all, and a program
		// that can reach it is refused with this sentence rather than with an
		// undefined reference to SSL_CTX_new.
		tlsWhy: "mingw-w64 ships no OpenSSL, so a program built for it has no TLS library to link",
	},
	// The Apple targets are built by the host's own clang, which is the only
	// compiler that has an SDK to build against: there is no cross compiler for
	// macOS that this table could name, so asking for one from another host is
	// an error rather than a build that fails somewhere less obvious.
	"darwin/amd64": {
		name:     "darwin/amd64",
		platSrc:  "tyrt_plat_posix.c",
		platText: tyrt.PlatPosix,
		tlsWhy:   darwinTLSWhy,
	},
	"darwin/arm64": {
		name:     "darwin/arm64",
		platSrc:  "tyrt_plat_posix.c",
		platText: tyrt.PlatPosix,
		tlsWhy:   darwinTLSWhy,
	},
}

// darwinTLSWhy is what a macOS build that reaches the TLS layer is told. macOS
// does not ship OpenSSL at all -- its TLS is SecureTransport and, since 10.15,
// Network.framework -- so a build against libssl only works from a Homebrew or
// MacPorts prefix that the compiler would have to be told about, and the
// relative paths such a build records are not ones a program can be shipped
// with. Saying so is what keeps this a named refusal instead of an include
// that does not resolve.
const darwinTLSWhy = "macOS ships SecureTransport rather than OpenSSL, and this compiler's TLS layer is written against OpenSSL"

// hostTarget is what a build with no target asked for gets: the machine this
// compiler is running on, with the compiler it already has.
func hostTarget() *target {
	name := runtime.GOOS + "/" + runtime.GOARCH
	if t, ok := targets[name]; ok {
		c := *t
		// A host builds with its own compiler: the one the table names for this
		// platform if it is installed, and otherwise the first of clang, gcc and
		// cc, which is what a build has always used. On Windows the two are not
		// interchangeable -- the runtime is written against winpthreads' pthread
		// and against Winsock, and a clang configured for the MSVC target has
		// neither -- so the compiler the platform is known to be built with is
		// preferred rather than left to whichever happens to be first on PATH.
		c.cc = ""
		if t.cc != "" {
			if _, err := exec.LookPath(t.cc); err == nil {
				c.cc = t.cc
			}
		}
		return &c
	}
	return &target{
		name:     name,
		platSrc:  platSrcFor(runtime.GOOS),
		platText: platTextFor(runtime.GOOS),
		// A platform this table does not know is one whose OpenSSL this
		// compiler has never been told about, so TLS is refused by name rather
		// than attempted and failed at the link.
		tlsWhy: "this compiler knows of no OpenSSL for " + name,
	}
}

// platSrcFor is the platform half of the runtime an OS compiles. Anything that
// is not Windows is POSIX, which is a real statement and not a default: the
// layer's POSIX implementation is the one the C library of every other system
// this could be built on provides.
func platSrcFor(goos string) string {
	if goos == "windows" {
		return "tyrt_plat_win.c"
	}
	return "tyrt_plat_posix.c"
}

func platTextFor(goos string) string {
	if goos == "windows" {
		return tyrt.PlatWin
	}
	return tyrt.PlatPosix
}

// resolveTarget turns a --target into the target a build uses. An empty name is
// the host. An unknown name, or one whose compiler is not installed, is an
// error: a build must not quietly produce a program for somewhere else.
//
// cc is the C compiler the caller named with --cc, and it is what a target this
// table names no compiler for is built with. The Apple rows are the ones with
// none -- there is no cross compiler for macOS that a table could name, so the
// only thing that can build for them is a compiler that runs here and targets
// them, which is what --cc hands the build (zig cc -target <arch>-macos, behind
// a wrapper, is the one that has been measured). A --cc names the build's
// compiler for every target for the same reason: the target says which platform
// the program is for, and --cc says what turns it into one.
func resolveTarget(name, cc string) (*target, error) {
	if name == "" || name == runtime.GOOS+"/"+runtime.GOARCH {
		return hostTarget(), nil
	}
	t, ok := targets[name]
	if !ok {
		names := make([]string, 0, len(targets))
		for k := range targets {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown target %q: known targets are %s", name, strings.Join(names, ", "))
	}
	// The compiler the build will actually run: the one the caller named, or
	// the target's own. Checking the one that will be used is the point of the
	// check -- a target whose compiler is missing is refused here whether the
	// table names it or the caller does, and a target that names none is
	// refused only when the caller named none either.
	want := t.cc
	if cc != "" {
		want = cc
	}
	if want == "" {
		return nil, fmt.Errorf("no C compiler for %s on a %s host: building for it needs a compiler that runs here and targets it, and neither this table nor --cc names one",
			name, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if _, err := exec.LookPath(want); err != nil {
		return nil, fmt.Errorf("no C compiler for %s: %s is not on PATH", name, want)
	}
	return t, nil
}

// writeRuntime materialises the C runtime next to the generated program.
func writeRuntime(dir string, tgt *target, link codegen.Link) string {
	// The header only has to exist next to the sources: it is found through the
	// -I on the command line, so it is written but never reported back.
	must(os.WriteFile(filepath.Join(dir, "tyrt.h"), []byte(tyrt.Header), 0o644))
	// The platform layer's interface, which every one of the six sources below
	// includes: what the runtime may call, and where an operating system call is
	// allowed to be made from.
	must(os.WriteFile(filepath.Join(dir, "tyrt_plat.h"), []byte(tyrt.PlatHeader), 0o644))
	c1 := filepath.Join(dir, "tyrt.c")
	c2 := filepath.Join(dir, "tyrt2.c")
	c3 := filepath.Join(dir, "tyrt_net.c")
	c4 := filepath.Join(dir, "tyrt_reflect.c")
	must(os.WriteFile(c1, []byte(tyrt.Core), 0o644))
	must(os.WriteFile(c2, []byte(tyrt.Extra), 0o644))
	must(os.WriteFile(c3, []byte(tyrt.Net), 0o644))
	must(os.WriteFile(c4, []byte(tyrt.Reflect), 0o644))
	c5 := filepath.Join(dir, "tyrt_thread.c")
	must(os.WriteFile(c5, []byte(tyrt.Thread), 0o644))
	// The Unicode tables: data, not code, and generated (internal/tools/
	// genunicode). They are compiled with every program because a program that
	// touches a string at all may reach one of their readers, and the linker
	// drops the tables a program never reads.
	c9 := filepath.Join(dir, "tyrt_unicode.c")
	must(os.WriteFile(c9, []byte(tyrt.Unicode), 0o644))
	// The platform half of the runtime, and the one file that differs between
	// targets: exactly one of the two is compiled, and which one is the target's
	// business rather than the C compiler's.
	c6 := filepath.Join(dir, tgt.platSrc)
	must(os.WriteFile(c6, []byte(tgt.platText), 0o644))
	files := c1 + " " + c2 + " " + c3 + " " + c4 + " " + c5 + " " + c6 + " " + c9
	// The TLS file is the one part of the runtime a build may leave out, and
	// leaving it out is the whole point of it being a file: it includes
	// OpenSSL's headers and calls OpenSSL's functions, so a translation unit
	// holding it makes the link need -lssl whether or not the program can reach
	// one of its helpers. A program that cannot does not pay for it -- not the
	// dependency, not the compile, not the bytes.
	if link.TLS {
		c7 := filepath.Join(dir, "tyrt_tls.c")
		must(os.WriteFile(c7, []byte(tyrt.TLS), 0o644))
		files += " " + c7
	}
	return files
}

// checkTLS refuses a program that can reach the TLS layer when the target it is
// being built for has no OpenSSL for it.
//
// Refusing rather than ignoring is the point: the compiler cannot produce a
// working TLS call for such a target, and the alternative it would be trading
// this for is a link that ends in "undefined reference to SSL_CTX_new" -- a
// message that names a symbol in a library the program's author never mentioned.
// The message below names the target, the feature, the reason and a way out.
//
// "Can reach" is the emitted C's own reachability (see codegen.Link), and it is
// a superset of what the program will run: a program that reflects carries the
// table that names every class, so a reflecting program reaches the TLS layer
// whether or not it means to. Saying so here rather than linking and failing at
// run time is the same choice the rest of this compiler makes -- the language
// refuses what it cannot do, rather than doing something else (AGENTS.md §5).
func checkTLS(tgt *target, link codegen.Link) error {
	if !link.TLS || tgt.tlsLibs != nil {
		return nil
	}
	return fmt.Errorf("TLS is not available for %s: %s. The program's reachable code calls the TLS layer, "+
		"so it cannot be built for that target as it is; build it for %s instead, or take the TLS path out of it",
		tgt.name, tgt.tlsWhy, tlsTargets())
}

// tlsTargets names the targets a program that uses TLS can be built for, for the
// message above. It is read from the table rather than written out, so a target
// that gains the libraries gains the sentence too.
func tlsTargets() string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.tlsLibs != nil {
			names = append(names, t.name)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func findCC() string {
	// TEYRU_CC names the C compiler a build uses, which is what runs the test
	// suite's gcc leg: clang is preferred when both are installed, so without a
	// way to say "gcc" the second half of the matrix could not be run at all.
	if cc := os.Getenv("TEYRU_CC"); cc != "" {
		if _, err := exec.LookPath(cc); err == nil {
			return cc
		}
	}
	for _, c := range []string{"clang", "gcc", "cc"} {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return "cc"
}

// Run executes a compiled program.
func Run(exe string, args []string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		code := ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			// a program killed by a signal has no exit status: Go reports 255,
			// which is also what a program that really exits 255 reports. Say
			// which signal it was, and use the status a shell would.
			return 128 + int(ws.Signal()), nil
		}
		return code, nil
	}
	return 1, err
}

// version is the compiler's version, and it is a variable rather than a constant
// so that a release build can set it from the tag it is cutting:
//
//	go build -ldflags "-X github.com/teyru-lang/Teyru/internal/driver.version=0.4.0"
//
// A build from the tree reports this default, which is the number the release is
// cut with -- the two agreeing is the point, so when the release changes, this
// changes in the same commit as the tag's release notes. It said 0.2.0 while the
// project had moved well past it, which is how a version string stops being
// information.
var version = "0.4.1"

// Version reports the compiler version string.
func Version() string {
	return "teyru " + version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}
