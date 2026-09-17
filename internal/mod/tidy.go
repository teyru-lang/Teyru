package mod

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Tidy makes a module's requirements and checksums agree with what its sources
// import:
//
//   - a require no source imports is dropped,
//   - a module a source imports that the cache holds is added at the highest
//     version the cache can satisfy it with,
//   - teyru.sum keeps the build list and nothing else, and every module it
//     keeps is verified against its recorded checksum.
//
// It never fetches: a requirement the cache cannot satisfy is reported and
// left alone, because a `teyru mod tidy` that reaches the network is one
// nobody can run in a sandbox or read in a review.
//
// The returned notes describe what was done and what still needs the user, in
// the order the work happened.
func Tidy(g *Graph) ([]string, error) {
	imports, err := ModuleImports(g.Main)
	if err != nil {
		return nil, err
	}
	var notes []string
	used := map[string]bool{}
	for _, imp := range imports {
		modPath, version, err := g.provider(imp)
		if err != nil {
			return notes, err
		}
		if modPath == "" {
			if LooksLikeModulePath(imp) {
				notes = append(notes, fmt.Sprintf("%s is imported but no module the cache holds provides it: nothing was added for it", imp))
			}
			continue
		}
		if modPath == g.Main.Path {
			continue // a package of this module: an import, not a requirement
		}
		used[modPath] = true
		switch r := g.Main.File.Get(modPath); {
		case r == nil:
			if g.Main.File.Add(modPath, version) {
				notes = append(notes, fmt.Sprintf("added requirement %s %s", modPath, version))
			}
			g.require(modPath, version)
		case r.Version != version:
			// Raising a version is a decision with consequences the sources
			// cannot show, so it is reported rather than taken.
			notes = append(notes, fmt.Sprintf("kept the requirement %s %s: %s is only in the cache as %s, and neither was fetched by this command",
				modPath, r.Version, imp, version))
		}
	}
	for _, r := range append([]*Require(nil), g.Main.File.Requires...) {
		if used[r.Path] {
			continue
		}
		if g.Main.File.Remove(r.Path) {
			g.drop(r.Path)
			notes = append(notes, fmt.Sprintf("dropped requirement %s %s: no source imports it", r.Path, r.Version))
		}
	}
	notes, err = g.Prune(notes)
	if err != nil {
		return notes, err
	}
	if err := g.Main.File.Write(g.Main.ModFilePath()); err != nil {
		return notes, err
	}
	return notes, nil
}

// provider finds the module and version that can satisfy an import path: one
// the build list already holds, or one the cache holds. An empty module path
// means no module can be named for it.
func (g *Graph) provider(dotted string) (modPath, version string, err error) {
	if mod, ok := MatchPath(dotted, g.ModulePaths()); ok {
		if dep := g.deps[mod]; dep != nil {
			if _, err := PackageIn(dep, dotted); err == nil {
				return mod, dep.Version, nil
			}
			// The path matches the module, but the selected version does not
			// hold that package. A version the cache does hold may, and that
			// is what the requirement should say.
			if v, ok := g.cachedVersion(mod, dotted); ok {
				return mod, v, nil
			}
			return mod, dep.Version, nil
		}
	}
	if mod, ok := MatchPath(dotted, g.cachedPaths()); ok {
		if v, ok := g.cachedVersion(mod, dotted); ok {
			return mod, v, nil
		}
	}
	return "", "", nil
}

// cachedVersion returns the highest cached version of a module that holds the
// package an import path names.
func (g *Graph) cachedVersion(mod, dotted string) (string, bool) {
	versions := g.avail[mod]
	for i := len(versions) - 1; i >= 0; i-- {
		in := versions[i]
		if _, err := PackageIn(&Dep{Path: mod, Version: in.Text, Dir: in.Dir}, dotted); err == nil {
			return in.Text, true
		}
	}
	return "", false
}

// Prune rewrites teyru.sum so that it covers the build list and nothing else,
// verifying every module it keeps and recording the checksums of modules the
// requirements added. A module the cache does not hold keeps the lines it has:
// its checksum cannot be recomputed, and dropping the line would only hide the
// fetch that is missing.
func (g *Graph) Prune(notes []string) ([]string, error) {
	sums := &Sum{}
	// The list grows while it is walked: reading a module's own teyru.mod adds
	// its requirements, which is exactly the set of modules a build needs.
	for i := 0; i < len(g.names); i++ {
		dep := g.deps[g.names[i]]
		if dep.Main {
			continue
		}
		if _, err := g.load(dep); err != nil {
			var notCached *NotCachedError
			if errors.As(err, &notCached) {
				notes = append(notes, fmt.Sprintf("module %s@%s is not in the cache: run `teyru get %s@%s`",
					dep.Path, dep.Version, dep.Path, dep.Version))
				for _, modFile := range []bool{false, true} {
					if h, ok := g.Sums.Lookup(dep.Path, dep.Version, modFile); ok {
						sums.Set(dep.Path, dep.Version, h, modFile)
					}
				}
				continue
			}
			return notes, err
		}
		tree, modFile, err := TreeHash(dep.Dir)
		if err != nil {
			return notes, err
		}
		want, recorded := g.Sums.Lookup(dep.Path, dep.Version, false)
		if recorded && want != tree {
			return notes, &SumMismatchError{Path: dep.Path, Version: dep.Version, Want: want, Got: tree}
		}
		if !recorded {
			notes = append(notes, fmt.Sprintf("recorded the checksum of %s %s", dep.Path, dep.Version))
		}
		sums.Set(dep.Path, dep.Version, tree, false)
		if modFile != "" {
			if wantMod, ok := g.Sums.Lookup(dep.Path, dep.Version, true); ok && wantMod != modFile {
				return notes, &SumMismatchError{Path: dep.Path + "/" + ModuleFileName, Version: dep.Version, Want: wantMod, Got: modFile}
			}
			sums.Set(dep.Path, dep.Version, modFile, true)
		}
	}
	for _, key := range g.Sums.Modules() {
		i := strings.LastIndexByte(key, '@')
		mod, ver := key[:i], key[i+1:]
		if _, ok := sums.Lookup(mod, ver, false); !ok {
			if _, ok := sums.Lookup(mod, ver, true); !ok {
				notes = append(notes, fmt.Sprintf("dropped the %s entry for %s %s: nothing in the build list needs it", SumFileName, mod, ver))
			}
		}
	}
	return notes, sums.Write(g.Main.SumFilePath())
}

// ModuleImports lists the import paths every source file of a module names,
// sorted and deduplicated.
//
// Files are parsed rather than pattern matched: an `import` inside a string or
// a comment is not an import, and a requirement dropped because of one would
// break the build the file belongs to.
func ModuleImports(m *Module) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	err := filepath.WalkDir(m.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p == m.Dir {
				return nil
			}
			// Hidden directories hold tool state, and a nested module is a
			// different module: its imports are not this module's.
			if strings.HasPrefix(name, ".") || IsModuleDir(p) {
				return fs.SkipDir
			}
			return nil
		}
		if !IsSource(name) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// A file that does not parse still has imports, and a tidy that
		// refuses to run on a file with a syntax error is one nobody can use
		// while fixing it. The imports that did parse are the ones that count.
		diags := &source.Diagnostics{}
		f := parser.Parse(source.NewFile(p, string(data)), diags)
		for _, imp := range f.Imports {
			if imp.Static || seen[imp.Path] {
				continue
			}
			seen[imp.Path] = true
			out = append(out, imp.Path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
