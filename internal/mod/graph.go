package mod

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SourceExt is the extension of a Teyru source file. It is here rather than in
// the driver because both the module system and the driver have to find the
// files of a package.
const SourceExt = ".teyru"

// JavaExt is the extension Java sources are written with. A `.java` file is a
// source this compiler reads unchanged: the statement terminator is optional
// (decision D6), so the same program is a Teyru program, and a build that names
// a `.java` file or a directory holding one compiles it. `tests/java-compat/`
// is the corpus that holds the claim to what compiles; nothing about the two
// extensions differs once the file is read.
const JavaExt = ".java"

// IsSource reports whether a file name is a source file of this compiler.
func IsSource(name string) bool {
	return strings.HasSuffix(name, SourceExt) || strings.HasSuffix(name, JavaExt)
}

// Module is the module a build starts from: the directory holding teyru.mod
// and what that file says.
type Module struct {
	Path string
	Dir  string
	File *File
}

// Dep is a module in the build list: the main module, or one of its
// requirements and their requirements.
type Dep struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	// File is the module's own teyru.mod, read from the cache or from the main
	// module's directory. A dependency that ships none contributes no further
	// requirements.
	File *File

	loaded   bool
	used     bool
	verified bool
}

// Graph is everything a build resolves module imports through: the main
// module, the modules it requires, the modules those require, and the
// checksums that say the cached copies are the ones the module file was
// written against.
//
// Modules are loaded lazily. A requirement whose version is not in the cache
// is not an error until a source file actually imports one of its packages --
// a module file lists what the author might need, and a build should not fail
// over a dependency nothing uses.
type Graph struct {
	Main  *Module
	Cache *Cache
	Sums  *Sum

	deps  map[string]*Dep
	avail map[string][]Installed
	names []string // module paths, sorted, for deterministic iteration
	// conflict is set when a requirement raises the version of a module the
	// build has already compiled a package from. It is reported by the next
	// Resolve rather than from require, which runs while a module graph is
	// being read and has no diagnostic to report into.
	conflict *versionConflict
}

// IsModuleDir reports whether a directory is the root of a module: it holds a
// teyru.mod. A module inside a module is a different module, and a build that
// walks a tree has to stop at one.
func IsModuleDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ModuleFileName))
	return err == nil && !st.IsDir()
}

// FindModuleDir walks up from dir looking for teyru.mod, the way a build finds
// the module it is part of.
func FindModuleDir(dir string) (string, bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false, err
	}
	for {
		st, err := os.Stat(filepath.Join(abs, ModuleFileName))
		if err == nil && !st.IsDir() {
			return abs, true, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false, nil
		}
		abs = parent
	}
}

// LoadModule reads the module rooted at dir.
func LoadModule(dir string) (*Module, error) {
	f, err := ReadFile(filepath.Join(dir, ModuleFileName))
	if err != nil {
		return nil, err
	}
	return &Module{Path: f.Module, Dir: dir, File: f}, nil
}

// LoadGraph reads the module containing dir and everything it requires. It
// fails when there is no module to read: a build that passed a directory with
// no teyru.mod above it is not a module build, and the caller decides that.
func LoadGraph(dir string) (*Graph, error) {
	root, ok, err := FindModuleDir(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &NoModuleError{Dir: dir}
	}
	main, err := LoadModule(root)
	if err != nil {
		return nil, err
	}
	cache, err := Open()
	if err != nil {
		return nil, err
	}
	sums, err := LoadSum(filepath.Join(root, SumFileName))
	if err != nil {
		return nil, err
	}
	avail, err := cache.List()
	if err != nil {
		return nil, err
	}
	g := &Graph{Main: main, Cache: cache, Sums: sums, deps: map[string]*Dep{}, avail: map[string][]Installed{}}
	for _, in := range avail {
		g.avail[in.Module] = append(g.avail[in.Module], in)
	}
	g.deps[main.Path] = &Dep{Path: main.Path, Main: true, Dir: root, File: main.File, loaded: true}
	g.names = []string{main.Path}
	for _, r := range main.File.Requires {
		g.require(r.Path, r.Version)
	}
	return g, nil
}

// NoModuleError reports that no teyru.mod was found above a directory.
type NoModuleError struct{ Dir string }

func (e *NoModuleError) Error() string {
	return fmt.Sprintf("no %s in %s or any directory above it", ModuleFileName, e.Dir)
}

// NotCachedError reports a requirement the cache cannot satisfy. Teyru does
// not fetch during a build: a build that quietly reaches the network is a
// build that fails somewhere else, later, with a worse error.
type NotCachedError struct{ Path, Version string }

func (e *NotCachedError) Error() string {
	return fmt.Sprintf("module %s@%s is not in the module cache: run `teyru get %s@%s`", e.Path, e.Version, e.Path, e.Version)
}

// MissingSumError reports a module the checksum file says nothing about.
type MissingSumError struct{ Path, Version string }

func (e *MissingSumError) Error() string {
	return fmt.Sprintf("module %s@%s has no entry in %s: run `teyru get %s@%s` to fetch it and record its checksum",
		e.Path, e.Version, SumFileName, e.Path, e.Version)
}

// SumMismatchError reports a cached module whose bytes are not the ones
// teyru.sum records. It is the one error in the module system that names a
// security problem: the same version, with different contents.
type SumMismatchError struct{ Path, Version, Want, Got string }

func (e *SumMismatchError) Error() string {
	return fmt.Sprintf("checksum mismatch for %s@%s:\n\t%s records %s\n\tthe cached copy is %s\n"+
		"the cached module is not the one %s was written against. Either the cache was modified or the "+
		"recorded checksum is wrong; nothing was built.",
		e.Path, e.Version, SumFileName, e.Want, e.Got, ModuleFileName)
}

// ModulePaths lists the module paths of the build list, sorted.
func (g *Graph) ModulePaths() []string {
	out := append([]string(nil), g.names...)
	sort.Strings(out)
	return out
}

// BuildList lists the build list, sorted by module path.
func (g *Graph) BuildList() []*Dep {
	out := make([]*Dep, 0, len(g.names))
	for _, n := range g.ModulePaths() {
		out = append(out, g.deps[n])
	}
	return out
}

// require records that some module needs path at version. The selected
// version is the highest one asked for, which is Go's minimal version
// selection over the requirements a build can see: a requirement never lowers
// the version another one asked for.
func (g *Graph) require(modPath, version string) {
	v, err := ParseVersion(version)
	if err != nil {
		// requires reaching here were parsed from a module file, which
		// validates versions; a bad one cannot be selected.
		return
	}
	if d := g.deps[modPath]; d != nil {
		have, err := ParseVersion(d.Version)
		if err != nil || Compare(v, have) <= 0 {
			return
		}
		if d.used {
			// A package of this module was already compiled at the version
			// that was selected then, and one build cannot hold two versions
			// of one module: say which requirement moved it and how to fix it.
			g.conflict = &versionConflict{Path: modPath, Was: d.Version, Now: version}
			return
		}
		d.Version = version
		d.loaded, d.Dir, d.File = false, "", nil
		return
	}
	g.deps[modPath] = &Dep{Path: modPath, Version: version}
	g.names = append(g.names, modPath)
}

// drop removes a module from the build list. `teyru mod tidy` uses it for a
// requirement the sources no longer import: a build list that still held it
// would record its checksum again in the very run that dropped the require, and
// the module would only leave the file on a second run.
//
// A module another one requires is not gone for long: reading that module's own
// teyru.mod puts it back.
func (g *Graph) drop(modPath string) {
	delete(g.deps, modPath)
	for i, n := range g.names {
		if n == modPath {
			g.names = append(g.names[:i], g.names[i+1:]...)
			return
		}
	}
}

// versionConflict is reported when a dependency raises the version of a module
// the build has already compiled packages from.
type versionConflict struct{ Path, Was, Now string }

func (e *versionConflict) Error() string {
	return fmt.Sprintf("module %s is already part of this build at %s, and another requirement asks for %s: "+
		"one build cannot hold two versions of one module. Record the version every requirement agrees on in %s "+
		"and fetch it with `teyru get %s@%s`", e.Path, e.Was, e.Now, ModuleFileName, e.Path, e.Now)
}

// pendingConflict reports a version conflict recorded while reading a module
// file, once, so a broken build is not reported twice.
func (g *Graph) pendingConflict() error {
	c := g.conflict
	g.conflict = nil
	// The nil check is not redundant: returning a nil *versionConflict as an
	// error gives a non-nil interface holding a nil pointer, and every caller
	// that tests the result would then report a conflict that is not there.
	if c == nil {
		return nil
	}
	return c
}

// Pkg is a package the build can compile: a module and a directory inside it,
// named by an import path.
type Pkg struct {
	ImportPath string
	Dir        string
	Module     string
	Version    string
	Main       bool
	// Tail is what an import path continues with after the package: a type for
	// a single-type import (`example.com/dep.Widget`), a type and a member for
	// a static one. Empty when the import names the package itself.
	Tail []string
}

// Canonical is the import path as the compiler is given it: the module path
// with slashes, so that the package is told apart from every other module's
// package of the same simple name.
func (p *Pkg) Canonical() string {
	parts := append([]string{p.ImportPath}, p.Tail...)
	return strings.Join(parts, ".")
}

// Resolve maps an import path onto the package that provides it.
//
// The second result says whether the path was a module path at all: an import
// of `java.util.List` or of a package of the program's own tree belongs to
// neither the main module's requirements nor the cache, and the compiler's own
// name resolution is left to deal with it.
func (g *Graph) Resolve(dotted string) (*Pkg, bool, error) {
	if err := g.pendingConflict(); err != nil {
		return nil, true, err
	}
	modPath, ok := MatchPath(dotted, g.ModulePaths())
	if !ok {
		// A cached module that no require names is the most useful thing to
		// say about an import that matches nothing: the fix is one command.
		if m, ok := MatchPath(dotted, g.cachedPaths()); ok {
			return nil, true, fmt.Errorf("module %s provides %s but %s does not require it: run `teyru get %s@<version>`",
				m, dotted, ModuleFileName, m)
		}
		if LooksLikeModulePath(dotted) {
			return nil, true, fmt.Errorf("cannot resolve %s: no module provides it, and %s requires none that is named like that",
				dotted, g.Main.Path)
		}
		return nil, false, nil
	}
	dep := g.deps[modPath]
	if dep == nil {
		return nil, true, fmt.Errorf("internal error: module %s is missing from the build list", modPath)
	}
	if _, err := g.load(dep); err != nil {
		return nil, true, err
	}
	// A module is checked against teyru.sum when a package of it is used, not
	// when it is listed: a require whose version is not in the cache is not an
	// error until something needs it.
	if err := g.Verify(dep); err != nil {
		return nil, true, err
	}
	dep.used = true
	pkg, err := PackageIn(dep, dotted)
	if err != nil {
		return nil, true, err
	}
	return pkg, true, nil
}

// MatchPath returns the longest module path among paths that is a prefix of an
// import path. Longest, because one module path may be a prefix of another
// (`example.com/a` and `example.com/a/pkg`), and the import names the more
// specific of the two.
func MatchPath(dotted string, paths []string) (modPath string, ok bool) {
	// The module path is matched in its dotted spelling and returned as it was
	// written: the two spellings differ by more than one character, so folding
	// the dotted form back would turn the dot of `example.com` into a slash.
	best, bestLen := "", 0
	for _, m := range paths {
		p := DottedPath(m)
		if (dotted == p || strings.HasPrefix(dotted, p+".")) && len(p) > bestLen {
			best, bestLen = m, len(p)
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// PackageIn resolves an import path inside one module: the package is the
// longest run of elements that names a directory with sources in it, and
// whatever follows is a type or a member of one.
func PackageIn(dep *Dep, dotted string) (*Pkg, error) {
	pre := DottedPath(dep.Path)
	rest := strings.TrimPrefix(strings.TrimPrefix(dotted, pre), ".")
	parts, err := dottedTail(rest)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dotted, err)
	}
	rel, tail, err := findPackage(dep.Dir, parts)
	if err != nil {
		return nil, fmt.Errorf("%s: %s@%s: %w", dotted, dep.Path, dep.Version, err)
	}
	return &Pkg{
		ImportPath: ImportPathOfPackage(dep.Path, rel),
		Dir:        filepath.Join(dep.Dir, filepath.FromSlash(rel)),
		Module:     dep.Path,
		Version:    dep.Version,
		Main:       dep.Main,
		Tail:       tail,
	}, nil
}

// cachedPaths lists the modules the cache holds, sorted.
func (g *Graph) cachedPaths() []string {
	out := make([]string, 0, len(g.avail))
	for m := range g.avail {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// dottedTail splits the part of an import path that follows the module path
// into its elements.
func dottedTail(rest string) ([]string, error) {
	if rest == "" {
		return nil, nil
	}
	parts := strings.Split(rest, ".")
	for _, p := range parts {
		// `.` and `..` are directories, not package names, and a path with one
		// in it is not an import path: it would name files outside the module.
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("not a valid import path")
		}
	}
	return parts, nil
}

// findPackage splits the elements after a module path into the directory that
// holds the package and whatever continues after the package name.
//
// The split is decided by the disk: the longest run of elements that names a
// directory inside the module is the package, and the rest is a type or a
// member. `example.com.dep` and `example.com.dep.Widget` therefore both end at
// the module root, and `example.com.dep.pkg.Widget.of` at its pkg directory.
func findPackage(modDir string, parts []string) (rel string, tail []string, err error) {
	if len(parts) == 0 {
		if !HasSources(modDir) {
			return "", nil, fmt.Errorf("the module root has no %s sources", SourceExt)
		}
		return "", nil, nil
	}
	empty := ""
	for n := len(parts); n >= 1; n-- {
		cand := strings.Join(parts[:n], "/")
		dir := filepath.Join(modDir, filepath.FromSlash(cand))
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			continue
		}
		if HasSources(dir) {
			return cand, parts[n:], nil
		}
		if empty == "" {
			empty = cand
		}
	}
	if empty != "" {
		return "", nil, fmt.Errorf("directory %s holds no %s sources", empty, SourceExt)
	}
	if HasSources(modDir) {
		return "", parts, nil
	}
	return "", nil, fmt.Errorf("no such package in %s", modDir)
}

// load brings a module into the build: it verifies the cached copy against
// teyru.sum and folds the module's own requirements into the build list.
func (g *Graph) load(d *Dep) (*Dep, error) {
	if d.loaded {
		return d, nil
	}
	if d.Main {
		d.Dir, d.File, d.loaded = g.Main.Dir, g.Main.File, true
		return d, nil
	}
	d.Dir = g.Cache.Dir(d.Path, d.Version)
	if !g.Cache.Has(d.Path, d.Version) {
		return nil, &NotCachedError{Path: d.Path, Version: d.Version}
	}
	// load marks the module before reading it, so that two modules requiring
	// each other do not recurse forever.
	d.loaded = true
	f, err := ReadFile(filepath.Join(d.Dir, ModuleFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return nil, err
	}
	d.File = f
	for _, r := range f.Requires {
		g.require(r.Path, r.Version)
	}
	return d, nil
}

// Verify checks a cached module against teyru.sum. A module with no recorded
// checksum is not accepted: the point of the file is that every fetched module
// has one, and a build that skips the check is a build that accepts whatever
// the last write to the cache put there.
func (g *Graph) Verify(d *Dep) error {
	if d.Main || d.verified {
		return nil
	}
	d.verified = true
	want, ok := g.Sums.Lookup(d.Path, d.Version, false)
	if !ok {
		return &MissingSumError{Path: d.Path, Version: d.Version}
	}
	got, err := HashDir(d.Dir)
	if err != nil {
		return err
	}
	if got != want {
		return &SumMismatchError{Path: d.Path, Version: d.Version, Want: want, Got: got}
	}
	// The module's own teyru.mod decides which modules the build list holds,
	// so a recorded hash for it is checked too. It is not required: the tree
	// hash covers the file's bytes as well.
	if wantMod, ok := g.Sums.Lookup(d.Path, d.Version, true); ok {
		modPath := filepath.Join(d.Dir, ModuleFileName)
		if _, err := os.Stat(modPath); err == nil {
			gotMod, err := HashFile(modPath)
			if err != nil {
				return err
			}
			if gotMod != wantMod {
				return &SumMismatchError{Path: d.Path + "/" + ModuleFileName, Version: d.Version, Want: wantMod, Got: gotMod}
			}
		}
	}
	return nil
}

// MainPackagePath returns the import path that names the package a directory
// of the main module holds. This is what gives a package its identity: two
// directories are two packages even when both declare `package util`.
func (g *Graph) MainPackagePath(dir string) (string, bool) {
	rel, err := filepath.Rel(g.Main.Dir, dir)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return ImportPathOfPackage(g.Main.Path, rel), true
}

// PackageFiles lists the Teyru sources of one package: the files directly in
// dir, in lexical order. A package is one directory -- a subdirectory is
// another package, and pulling one in because it happens to sit underneath
// would compile a program the import never asked for (a dependency's `cmd`
// directory carries a second entry point).
func PackageFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || !IsSource(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// HasSources reports whether a directory holds a package at all.
func HasSources(dir string) bool {
	files, err := PackageFiles(dir)
	return err == nil && len(files) > 0
}

// ModFilePath is the path of the module file in a module directory.
func (m *Module) ModFilePath() string { return filepath.Join(m.Dir, ModuleFileName) }

// SumFilePath is the path of the checksum file in a module directory.
func (m *Module) SumFilePath() string { return filepath.Join(m.Dir, SumFileName) }

// ImportPath is the import path of a package inside this module, from a
// module-relative directory spelled with slashes.
func (m *Module) ImportPath(rel string) string {
	return ImportPathOfPackage(m.Path, path.Clean(filepath.ToSlash(rel)))
}
