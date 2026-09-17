package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyChangesWithEveryInput(t *testing.T) {
	base := Key{
		Compiler: "0.4.0 go1.26.5",
		Runtime:  "r1",
		Prelude:  "p1",
		Target:   "linux/amd64",
		Opt:      "-O0",
		Backend:  "c",
		Variant:  "reflect=false",
	}
	if base.String() != base.String() {
		t.Fatal("the same key rendered twice is two names")
	}
	if len(base.String()) != 32 {
		t.Fatalf("key %q is not 32 characters", base.String())
	}
	// Every field is an input the artifact depends on: leaving one out of the
	// name is a cache that answers one build with another's artifact.
	cases := []struct {
		name string
		key  Key
	}{
		{"Compiler", func() Key { k := base; k.Compiler = "0.4.1"; return k }()},
		{"Runtime", func() Key { k := base; k.Runtime = "r2"; return k }()},
		{"Prelude", func() Key { k := base; k.Prelude = "p2"; return k }()},
		{"Target", func() Key { k := base; k.Target = "linux/arm64"; return k }()},
		{"Opt", func() Key { k := base; k.Opt = "-O1"; return k }()},
		{"Backend", func() Key { k := base; k.Backend = "llvm"; return k }()},
		{"Variant", func() Key { k := base; k.Variant = "reflect=true"; return k }()},
	}
	for _, c := range cases {
		if c.key.String() == base.String() {
			t.Errorf("a different %s is the same cache name: %s", c.name, base.String())
		}
	}
	// A field boundary that can move must not: "ab"+"c" and "a"+"bc" are
	// different keys, because the fields are length-prefixed.
	a := base
	a.Target, a.Opt = "linux/amd64", "-O0"
	b := base
	b.Target, b.Opt = "linux/amd64-", "O0"
	if a.String() == b.String() {
		t.Error("two different field splits hash to the same name")
	}
}

func TestHashOfIsOrderedAndContentSensitive(t *testing.T) {
	one := []Named{{Name: "a", Data: []byte("hello")}}
	two := []Named{{Name: "a", Data: []byte("hello")}}
	if HashOf(one) != HashOf(two) {
		t.Error("the same inputs hash differently")
	}
	if HashOf(one) == HashOf([]Named{{Name: "a", Data: []byte("hellp")}}) {
		t.Error("one byte different hashes the same")
	}
	if HashOf(one) == HashOf([]Named{{Name: "b", Data: []byte("hello")}}) {
		t.Error("a different name hashes the same")
	}
	if HashOf(one) == HashOf([]Named{{Name: "a", Data: []byte("hello")}, {Name: "b", Data: nil}}) {
		t.Error("an extra input does not change the hash")
	}
}

func TestEntryPutGetHas(t *testing.T) {
	dir := t.TempDir()
	e := (&Cache{dir: dir}).Lookup(Key{Prelude: "p"})

	if e.Has("prelude.o") {
		t.Fatal("an empty entry reports a hit")
	}
	if _, ok := e.Get("prelude.o"); ok {
		t.Fatal("an empty entry returns a file")
	}
	if err := e.Put("prelude.o", []byte("object")); err != nil {
		t.Fatal(err)
	}
	if !e.Has("prelude.o") {
		t.Fatal("a file that was written is not a hit")
	}
	got, ok := e.Get("prelude.o")
	if !ok || string(got) != "object" {
		t.Fatalf("got %q, %v", got, ok)
	}
	// An entry is a hit only if everything asked for is there: a build that
	// found the object but not the header would link against definitions it
	// cannot declare.
	if e.Has("prelude.o", "typrelude.h") {
		t.Fatal("a missing file does not make the entry a miss")
	}
	// A zero-length file is not a hit either: a write that was interrupted
	// before its rename never publishes one, but a cache directory that was
	// truncated must not be read as a whole artifact.
	e.Put("empty", nil)
	if e.Has("empty") {
		t.Fatal("an empty file counts as a hit")
	}
}

func TestEntryPutFileMovesTheArtifact(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "prelude.o")
	if err := os.WriteFile(src, []byte("compiled"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := (&Cache{dir: filepath.Join(dir, "cache")}).Lookup(Key{Prelude: "p"})
	if err := e.PutFile("prelude.o", src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the artifact was copied rather than moved")
	}
	got, ok := e.Get("prelude.o")
	if !ok || string(got) != "compiled" {
		t.Fatalf("got %q, %v", got, ok)
	}
	// A source that is not there is an error, not a silent miss: the caller
	// then links the object it did build rather than an entry with a hole.
	if err := e.PutFile("prelude.o", filepath.Join(dir, "gone.o")); err == nil {
		t.Error("moving a file that does not exist reported success")
	}
}

func TestLookupDistinguishesKeys(t *testing.T) {
	c := &Cache{dir: t.TempDir()}
	a := c.Lookup(Key{Prelude: "p1"})
	b := c.Lookup(Key{Prelude: "p2"})
	if a.Dir() == b.Dir() {
		t.Fatal("two keys share an entry")
	}
	if a.Dir() != c.Lookup(Key{Prelude: "p1"}).Dir() {
		t.Fatal("one key has two entries")
	}
}

func TestOpenHonoursTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	t.Setenv(EnvOff, "")
	if got := Open().Dir(); got != dir {
		t.Errorf("Dir = %q, want %q", got, dir)
	}
	t.Setenv(EnvOff, "1")
	if got := Open(); got != nil {
		t.Errorf("a disabled cache is open: %v", got)
	}
	t.Setenv(EnvOff, "0")
	if got := Open(); got == nil {
		t.Error(`TEYRU_NOCACHE=0 is not "off"`)
	}
	// No cache at all is not an error at the call sites: they ask a nil entry
	// and get a miss.
	var none *Entry
	if none.Has("prelude.o") {
		t.Error("no cache reports a hit")
	}
	if err := none.Put("prelude.o", []byte("x")); err != nil {
		t.Errorf("no cache refuses a write: %v", err)
	}
}
