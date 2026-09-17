package main

// genunicode turns the Unicode data files of one Unicode release into the C
// tables the runtime classifies and case maps with. The output is a file of
// this repository, checked in, and the generator is re-run rather than trusted:
// `make unicode-tables` writes it, and `go test ./internal/tools/genunicode`
// fails when the file on disk and the one the data produce have drifted apart.
//
// The data files are the standard's own (UnicodeData.txt, SpecialCasing.txt and
// PropList.txt of the release below) and they are read from the directory next
// to this file, so the generator needs no network and no toolchain beyond Go.
//
// The reference implementation is OpenJDK 21, which is Unicode 15.0, so the
// tables are 15.0 as well: every expectation the tests hold against the JDK is
// then an expectation the tables can meet. The data files are distributed under
// the Unicode License v3, which THIRD-PARTY-NOTICES.md carries and
// scripts/check-notices.sh checks.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// unicodeVersion is the release the data files are from. It is written into
	// the generated file, so a reader can tell which tables they are looking at
	// without going to the data.
	unicodeVersion = "15.0.0"

	// The defaults, relative to the repository root.
	dataDir = "internal/tools/genunicode/data"
	outPath = "internal/runtime/src/tyrt_unicode.c"
)

func main() {
	var (
		check = flag.Bool("check", false, "report whether the file on disk is what the data produce, and write nothing")
		out   = flag.String("o", outPath, "the file to write, or to compare against with -check")
		data  = flag.String("data", dataDir, "the directory holding the Unicode data files")
		quiet = flag.Bool("q", false, "print nothing when the output is current")
	)
	flag.Parse()

	// A relative path is relative to the repository, not to the directory the
	// command was typed in, so `go run ./internal/tools/genunicode` from the
	// root, `go test` from this package and `make unicode-tables` all mean the
	// same files.
	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	dir := resolve(root, *data)
	path := resolve(root, *out)

	want, err := generate(dir)
	if err != nil {
		fail(err)
	}
	if *check {
		have, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		if !bytes.Equal(have, want) {
			fmt.Fprintf(os.Stderr, "genunicode: %s is not what the data in %s produce\n", path, dir)
			fmt.Fprintln(os.Stderr, "genunicode: run `make unicode-tables` and commit the result")
			os.Exit(1)
		}
		if !*quiet {
			fmt.Printf("genunicode: %s is current\n", path)
		}
		return
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		fail(err)
	}
	if !*quiet {
		fmt.Printf("genunicode: wrote %s (%d bytes)\n", path, len(want))
	}
}

// repoRoot is the directory holding go.mod: the anchor every default path is
// relative to.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod in this directory or above it: run genunicode from the repository")
		}
		dir = parent
	}
}

func resolve(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genunicode:", err)
	os.Exit(1)
}

// generate reads the data files in dir and returns the generated C.
func generate(dir string) ([]byte, error) {
	u, err := readUCD(dir)
	if err != nil {
		return nil, err
	}
	t, err := u.tables()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.emit(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
