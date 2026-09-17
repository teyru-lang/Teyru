// Package cache is the compiler's build cache: the place the standard library's
// compiled form is kept between builds, so that a program is not recompiled from
// the same standard library sources every time it is built.
//
// What is cached is an artifact that depends on the compiler, the target and the
// prelude, and on nothing about the program being built. What makes reusing it
// sound is the key: a name derived from every input the artifact depends on, so
// a change to any of them is a different entry rather than a stale one. The key
// is the whole safety argument, which is why the pieces below are named after
// the inputs rather than the artifact.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
)

// EnvDir moves the cache, and EnvOff turns it off. A build with the cache off
// compiles everything it would otherwise have reused: that is what the tests
// that measure the cache compare against, and what a user with a read-only home
// directory ends up with rather than an error.
const (
	EnvDir = "TEYRU_CACHE_DIR"
	EnvOff = "TEYRU_NOCACHE"
)

// Cache is one directory of build artifacts.
type Cache struct {
	dir string
}

// Open returns the cache the user's environment names. A cache that cannot be
// opened is not an error: a build whose cache is unavailable is a build that
// compiles what it needs, which is what every build did before the cache
// existed.
func Open() *Cache {
	if off() {
		return nil
	}
	if dir := os.Getenv(EnvDir); dir != "" {
		return &Cache{dir: dir}
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return nil
	}
	return &Cache{dir: filepath.Join(base, "teyru")}
}

// Off reports whether the cache is disabled by the environment.
func off() bool {
	v := strings.TrimSpace(os.Getenv(EnvOff))
	return v != "" && v != "0"
}

// Dir is the cache's directory, or "" when there is none.
func (c *Cache) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// Key names one artifact. Every field is an input the artifact depends on, and
// the reason each is here is that leaving it out would be a cache that answers
// with another build's answer:
//
//   - Compiler and Runtime are what turned the sources into C and what the C is
//     compiled against; two compiler versions can emit different code for the
//     same sources, and the runtime's header is part of what the object means.
//   - Target, Opt and Backend are the flags the object was built with.
//   - Prelude is the standard library's own source, and Variant the part of the
//     build that is not the source: whether the program can reach reflection,
//     which decides whether the member tables ship.
type Key struct {
	Compiler string // the compiler's own version and build
	Runtime  string // a hash of the runtime sources and headers
	Prelude  string // a hash of the standard library's sources
	Target   string // "<os>/<arch>"
	Opt      string // the optimisation flag
	Backend  string // "c" or "llvm"
	Variant  string // extra flags the artifact was built with
}

// String renders the key as one directory name.
func (k Key) String() string {
	var h hash.Hash = sha256.New()
	for _, part := range []string{k.Compiler, k.Runtime, k.Prelude, k.Target, k.Opt, k.Backend, k.Variant} {
		fmt.Fprintf(h, "%d:", len(part))
		io.WriteString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Entry is where one artifact's files live.
type Entry struct {
	dir string
}

// Lookup returns the entry for a key, whether or not anything is in it.
func (c *Cache) Lookup(k Key) *Entry {
	if c == nil {
		return nil
	}
	return &Entry{dir: filepath.Join(c.dir, "prelude", k.String())}
}

// Dir is the entry's directory, or "" when there is no cache.
func (e *Entry) Dir() string {
	if e == nil {
		return ""
	}
	return e.dir
}

// Has reports whether every named file is in the entry. A file that is there but
// empty counts as absent: a partial write is never reported as a hit, which is
// the failure a cache must not have.
func (e *Entry) Has(names ...string) bool {
	if e == nil {
		return false
	}
	for _, n := range names {
		st, err := os.Stat(filepath.Join(e.dir, n))
		if err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// Path is the path of a file in the entry.
func (e *Entry) Path(name string) string {
	if e == nil {
		return ""
	}
	return filepath.Join(e.dir, name)
}

// Put adds a file to the entry. The file is written to a temporary name in the
// same directory and renamed into place, so a reader never sees a half-written
// artifact: the rename is what publishes it.
//
// A cache that cannot be written is not an error. A read-only home directory
// makes every build a cold one, which is slower and still correct.
func (e *Entry) Put(name string, data []byte) error {
	if e == nil {
		return nil
	}
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(e.dir, name+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(e.dir, name))
}

// PutFile adds a file to the entry by moving it there. The source is consumed:
// it is renamed when the two are on the same filesystem and copied when they are
// not, so a build that produced an artifact in a temporary directory does not
// read it back into memory to store it.
func (e *Entry) PutFile(name, src string) error {
	if e == nil {
		return nil
	}
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(e.dir, name)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(e.dir, name+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// Get reads a file from the entry. A missing file is a miss and not an error.
func (e *Entry) Get(name string) ([]byte, bool) {
	if e == nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		return nil, false
	}
	return data, true
}

// HashOf hashes the named pieces and their contents into one string. It is how
// the key's Runtime and Prelude fields are derived: the sources a build's
// artifacts were made from, in a fixed order so that the same inputs always
// hash to the same key.
func HashOf(parts []Named) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%s\x00%d\x00", p.Name, len(p.Data))
		h.Write(p.Data)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Named is one input to a hash.
type Named struct {
	Name string
	Data []byte
}

// Build is what a Key's Compiler field names: which compiler this is. The
// version is the released one, and the revision is what a build from a working
// tree adds: Go records the commit a binary was built from and whether the tree
// was dirty, so a compiler with a change in it -- a change that can emit
// different code for the same prelude -- does not answer with the previous
// compiler's artifacts. A build with no revision to report (a `go run`, or a
// binary built outside a repository) falls back to the version alone.
func Build(version string) string {
	parts := []string{version, runtime.Version(), runtime.GOOS, runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision", "vcs.modified", "vcs.time":
				parts = append(parts, s.Key+"="+s.Value)
			}
		}
	}
	return strings.Join(parts, " ")
}
