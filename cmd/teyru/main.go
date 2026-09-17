// Command teyru is the Teyru compiler: a native compiler that lowers Teyru
// source to C and links it into a standalone binary with no JVM involved.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teyru-lang/Teyru/internal/driver"
	"github.com/teyru-lang/Teyru/internal/mod"
)

const usage = `teyru - the Teyru compiler

usage:
  teyru build [flags] <paths...>   compile to a native executable
  teyru run   [flags] <paths...> [-- args...]  compile and run
  teyru emit  [flags] <paths...>   print the generated C
  teyru emit-llvm [flags] <paths...>  print the LLVM IR the backend feeds to LLVM
  teyru emit-java [flags] <paths...>  print the equivalent Java source
  teyru get <module>@<version>     fetch a module into the cache and require it
  teyru mod init <module-path>     write teyru.mod for a new module
  teyru mod tidy                   make teyru.mod and teyru.sum match the sources
  teyru version                    print the version

paths:
  <dir>       the package tree rooted at <dir>, walked recursively
  ./...       <dir> and every package under it (the module's own packages)
  (none)      the module the current directory is in, or the current
              directory when there is no module

  A directory that holds a teyru.mod is a module: imports of a path that
  names one of its required modules are resolved from the module cache
  ($TEYRUPATH/pkg/mod/<module>@<version>), with no flags.

flags:
  -o <path>     output executable (default a.out)
  -c <path>     keep the generated C at <path>
  --cc <name>   C compiler to use (default clang)
  --target <os/arch>  platform to build for (default this machine):
                linux/amd64, linux/arm64, windows/amd64, darwin/amd64,
                darwin/arm64. A target other than this machine's needs the
                cross compiler for it on PATH (windows/amd64:
                x86_64-w64-mingw32-gcc), and appends its output suffix.
  -O0..-O3      optimisation level (default -O2; -O0 for run). At -O0 and
                -O1 the standard library is compiled once into the user cache
                and linked, instead of the whole program being optimised as
                one; -O2 and above keep the whole-program build
  --llvm-ir <p> write the LLVM IR module to <p> (the backend is clang/LLVM)
  --backend <b> c (default) or llvm: which back end compiles the program.
                The llvm back end emits the program's own LLVM module and
                links the C runtime against it; it refuses, with TY-INT-0100
                and the feature's name, anything it cannot lower
  --no-lto      disable link-time optimisation
  --native <f>  C source implementing the program's native methods (repeatable)
  --link <arg>  extra argument for the link step, such as -lm or a .a path
  --cc-flag <arg>  extra argument for the C compile step (repeatable), such as
                -I <dir> to put a --native-header on the include path
  --native-header <p>  write the C prototypes of every native method to <p>;
                       stops there unless --native is also given
  -v            verbose

emit-java prints the program as the Java source it stands for, and refuses, by
name and with the reason, what has no Java spelling: native properties, native
methods, the Lombok and Spring and Gson shaped layers, and the parts of the
standard library Java has no class for. It prints one compilation unit, so a
program of several packages is refused too.
`

func main() {
	os.Exit(run())
}

// run does the work and returns the exit code. It is a function rather than the
// body of main so that a deferred cleanup actually runs: os.Exit skips deferred
// calls, and `teyru run` used to leave its compiled program and generated C
// behind in the temporary directory on every invocation.
func run() int {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return 2
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	// The module commands take no compiler flags: their arguments are a module
	// path and a version, and a `-o` among them would have to be an error.
	switch cmd {
	case "mod":
		return runMod(args)
	case "get":
		return runGet(args)
	}
	opts := driver.Options{Out: "a.out"}
	var files []string
	var progArgs []string
	afterSep := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case afterSep:
			progArgs = append(progArgs, a)
		case a == "--":
			afterSep = true
		case a == "-o":
			i++
			if i < len(args) {
				opts.Out = args[i]
			}
		case a == "-c":
			i++
			if i < len(args) {
				opts.CFile = args[i]
				opts.EmitC = args[i]
			}
		case a == "--cc":
			i++
			if i < len(args) {
				opts.CC = args[i]
			}
		case a == "--no-lto":
			opts.NoLTO = true
		case a == "-v":
			opts.Verbose = true
		case a == "--native":
			i++
			if i < len(args) {
				opts.Native = append(opts.Native, args[i])
			}
		case a == "--link":
			i++
			if i < len(args) {
				opts.Link = append(opts.Link, args[i])
			}
		case a == "--cc-flag":
			// An argument for the C *compile* step rather than the link step:
			// a program whose native methods are implemented in C needs its
			// generated header on the include path, and until this existed the
			// only way to say so was the Go API, which left the suite's native
			// fixture unrunnable from the command line.
			i++
			if i < len(args) {
				opts.ExtraCC = append(opts.ExtraCC, args[i])
			}
		case a == "--native-header":
			i++
			if i < len(args) {
				opts.NativeHeader = args[i]
			}
		case a == "--llvm-ir":
			i++
			if i < len(args) {
				opts.EmitLLVM = args[i]
			}
		case a == "--target":
			// The platform to build for, as <os>/<arch> (linux/amd64,
			// windows/amd64, ...). The driver resolves it and refuses one it has
			// no compiler for; omitting it builds for this machine, as it
			// always did.
			i++
			if i < len(args) {
				opts.Target = args[i]
			}
		case a == "--backend":
			i++
			if i < len(args) {
				opts.Backend = args[i]
			}
		case strings.HasPrefix(a, "--backend="):
			// the `--flag=value` spelling, which is what a build script writes
			opts.Backend = strings.TrimPrefix(a, "--backend=")
		case len(a) > 2 && a[0] == '-' && a[1] == 'O':
			opts.Opt = a
		default:
			files = append(files, a)
		}
	}

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println(driver.Version())
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	case "build", "run", "emit", "emit-llvm", "emit-java":
	default:
		fmt.Fprintf(os.Stderr, "teyru: unknown command %q\n", cmd)
		fmt.Print(usage)
		return 2
	}
	switch opts.Backend {
	case "", driver.BackendC, driver.BackendLLVM:
	default:
		fmt.Fprintf(os.Stderr, "teyru: unknown backend %q: the back ends are c (the default) and llvm\n", opts.Backend)
		return 2
	}

	// `./...` is Go's spelling of "this directory and every package under it".
	// The driver compiles a directory as its whole tree already, so the
	// wildcard is the directory it names -- and a build with no path at all is
	// the module the current directory is in.
	paths, err := expandPaths(files)
	if err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return 2
	}
	files = paths

	if cmd == "run" {
		// `run` builds to execute now, so it builds the way a build whose
		// compile time is what matters does: -O0, which compiles the standard
		// library once and links the object rather than putting the whole
		// program through link-time optimisation (internal/driver's
		// compileSplit). -O2 and above keep the whole-program build, and
		// `teyru run -O2 prog.teyru` asks for that one explicitly.
		if opts.Opt == "" {
			opts.Opt = "-O0"
		}
		dir, err := os.MkdirTemp("", "teyru-build-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(dir)
		opts.Out = filepath.Join(dir, "program")
		opts.CFile = filepath.Join(dir, "program.c")
	}
	if cmd == "emit" {
		// `emit` prints the generated C: it must not compile or link anything,
		// and it must not leave a binary behind, so the C compiler is skipped.
		// The C itself goes to the driver's temporary directory like every other
		// build: a fixed path in the shared temporary directory is one
		// concurrent emit away from handing back another program's source.
		opts.EmitC = ""
		opts.CSourceOnly = true
	}
	if cmd == "emit-llvm" {
		dir, err := os.MkdirTemp("", "teyru-llvm-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(dir)
		opts.Out = filepath.Join(dir, "program")
		opts.CFile = filepath.Join(dir, "program.c")
		opts.EmitLLVM = filepath.Join(dir, "program.ll")
		// the IR is written before the link step, and nobody looks at the
		// executable an `emit-llvm` run would produce: asking for the IR used to
		// fail outright when that unrequested link failed
		opts.CSourceOnly = true
	}

	if cmd == "emit-java" {
		// The Java printer is the other half of the JDK differential: it prints
		// the program's own files as the Java they stand for, refusing by name
		// what has no Java spelling rather than printing something that only
		// looks like the program.
		res, err := driver.EmitJava(files, opts)
		if err != nil {
			if res != nil && res.Diags != nil && len(res.Diags.List) > 0 {
				fmt.Fprint(os.Stderr, res.Diags.String())
			}
			fail(err)
		}
		if res.Java != nil && len(res.Java.Refusals) > 0 {
			fmt.Fprint(os.Stderr, res.Diags.String())
			fail(fmt.Errorf("cannot emit Java for this program: %d feature(s) have no Java spelling", len(res.Java.Refusals)))
		}
		os.Stdout.WriteString(res.Java.Text)
		return 0
	}

	start := time.Now()
	res, err := driver.Compile(files, opts)
	if err != nil {
		if res != nil && res.Diags != nil && len(res.Diags.List) > 0 {
			fmt.Fprint(os.Stderr, res.Diags.String())
		}
		fail(err)
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "teyru: compiled in %s\n", time.Since(start).Round(time.Millisecond))
	}
	switch cmd {
	case "emit":
		os.Stdout.WriteString(res.CSource)
	case "emit-llvm":
		data, err := os.ReadFile(res.LLVMFile)
		if err != nil {
			fail(err)
		}
		os.Stdout.Write(data)
	case "run":
		code, err := driver.Run(res.Exe, progArgs)
		if err != nil {
			fail(err)
		}
		if code >= 128 && code < 256 {
			fmt.Fprintf(os.Stderr, "teyru: the program was killed by signal %d\n", code-128)
		}
		return code
	default:
		fmt.Println(res.Exe)
	}
	return 0
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
	os.Exit(1)
}

// expandPaths turns the paths of a build into the directories and files the
// driver reads.
func expandPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		// Inside a module, `teyru build` builds the module: its main package
		// and every package under it. Without one it is the current directory,
		// which is what the driver defaults to anyway.
		if root, ok, err := mod.FindModuleDir("."); err == nil && ok {
			return []string{root}, nil
		}
		return nil, nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		switch {
		case p == "...":
			return nil, fmt.Errorf("`...` names nothing on its own: write ./... for this directory and the packages under it")
		case strings.HasSuffix(p, "/..."):
			dir := strings.TrimSuffix(p, "/...")
			if dir == "" {
				dir = "."
			}
			out = append(out, dir)
		default:
			out = append(out, p)
		}
	}
	return out, nil
}

// runMod dispatches `teyru mod <subcommand>`.
func runMod(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, "teyru: mod wants a subcommand: init, tidy\n")
		return 2
	}
	switch args[0] {
	case "init":
		return modInit(args[1:])
	case "tidy":
		return modTidy(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "teyru: unknown mod subcommand %q\n", args[0])
		fmt.Fprint(os.Stderr, "teyru: mod wants a subcommand: init, tidy\n")
		return 2
	}
}

// modInit writes a teyru.mod in the current directory.
func modInit(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, "teyru: mod init wants a module path, such as example.com/myapp\n")
		return 2
	}
	path := args[0]
	if !mod.ValidPath(path) {
		fmt.Fprintf(os.Stderr, "teyru: %q is not a module path: it is the prefix every import path of this module starts with, such as example.com/myapp\n", path)
		return 2
	}
	if _, err := os.Stat(mod.ModuleFileName); err == nil {
		fmt.Fprintf(os.Stderr, "teyru: %s already exists\n", mod.ModuleFileName)
		return 1
	}
	if err := mod.New(path).Write(mod.ModuleFileName); err != nil {
		fail(err)
	}
	fmt.Printf("teyru: created %s for module %s\n", mod.ModuleFileName, path)
	return 0
}

// modTidy makes teyru.mod and teyru.sum agree with the module's sources.
func modTidy(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, "teyru: mod tidy takes no arguments\n")
		return 2
	}
	graph, err := loadGraph(".")
	if err != nil {
		fail(err)
	}
	notes, err := mod.Tidy(graph)
	for _, n := range notes {
		fmt.Fprintf(os.Stderr, "teyru: %s\n", n)
	}
	if err != nil {
		fail(err)
	}
	if len(notes) == 0 {
		fmt.Fprintf(os.Stderr, "teyru: %s and %s are already as the sources want them\n", mod.ModuleFileName, mod.SumFileName)
	}
	return 0
}

// runGet fetches one module version into the cache and records it.
func runGet(args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, "teyru: get wants one module and version, such as example.com/dep@v1.0.0\n")
		return 2
	}
	path, version, ok := strings.Cut(args[0], "@")
	if !ok || path == "" || version == "" {
		fmt.Fprintf(os.Stderr, "teyru: %q is not <module>@<version>: write `teyru get example.com/dep@v1.0.0`\n", args[0])
		return 2
	}
	if !mod.ValidPath(path) {
		fmt.Fprintf(os.Stderr, "teyru: %q is not a module path\n", path)
		return 2
	}
	if _, err := mod.ParseVersion(version); err != nil {
		fmt.Fprintf(os.Stderr, "teyru: %v\n", err)
		return 2
	}
	if err := mod.CheckImportPath(path, version); err != nil {
		fail(err)
	}
	graph, err := loadGraph(".")
	if err != nil {
		fail(err)
	}
	cached := graph.Cache.Has(path, version)
	if cached {
		fmt.Fprintf(os.Stderr, "teyru: %s@%s is already in the cache\n", path, version)
	} else {
		fmt.Fprintf(os.Stderr, "teyru: fetching %s@%s\n", path, version)
	}
	fetcher := mod.NewGitFetcher()
	fetcher.Log = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "teyru: "+format+"\n", args...) }
	dir, err := graph.Cache.Fetch(fetcher, path, version)
	if err != nil {
		fail(err)
	}
	tree, modFile, err := mod.TreeHash(dir)
	if err != nil {
		fail(err)
	}
	// An entry that is already there is checked, never overwritten: a module
	// version with two different contents is the one thing the checksum file
	// exists to make impossible.
	if want, ok := graph.Sums.Lookup(path, version, false); ok && want != tree {
		fail(&mod.SumMismatchError{Path: path, Version: version, Want: want, Got: tree})
	}
	graph.Sums.Set(path, version, tree, false)
	if modFile != "" {
		graph.Sums.Set(path, version, modFile, true)
	}
	if err := graph.Sums.Write(graph.Main.SumFilePath()); err != nil {
		fail(err)
	}
	graph.Main.File.Add(path, version)
	if err := graph.Main.File.Write(graph.Main.ModFilePath()); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "teyru: %s %s -> %s\n", path, version, dir)
	return 0
}

// loadGraph loads the module the current directory is in, with the error a
// user can act on when there is none.
func loadGraph(dir string) (*mod.Graph, error) {
	g, err := mod.LoadGraph(dir)
	if err != nil {
		var noMod *mod.NoModuleError
		if errors.As(err, &noMod) {
			return nil, fmt.Errorf("%w: run `teyru mod init <module-path>` to start one", err)
		}
		return nil, err
	}
	return g, nil
}
