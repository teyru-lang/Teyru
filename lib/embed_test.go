package lib

import (
	"strings"
	"testing"
)

func TestFilesAreOrderedAndComplete(t *testing.T) {
	files := Files()
	if len(files) == 0 {
		t.Fatal("the standard library is empty")
	}
	seen := map[string]string{}
	for i, f := range files {
		// The standard library is package teyru, except for the test
		// framework, which lives in teyru.test so that the annotation names
		// say what they are for.
		if !strings.HasPrefix(f.Source, "package teyru\n") && !strings.HasPrefix(f.Source, "package teyru.test\n") {
			t.Errorf("%s: the standard library is in package teyru or teyru.test", f.Name)
		}
		if i > 0 && f.Name < files[i-1].Name {
			t.Errorf("%s comes after %s; the numeric prefix must fix the order", f.Name, files[i-1].Name)
		}
		for _, line := range strings.Split(f.Source, "\n") {
			name, ok := declaredName(line)
			if !ok {
				continue
			}
			if prev, dup := seen[name]; dup {
				t.Errorf("%s is declared twice: %s and %s", name, prev, f.Name)
			}
			seen[name] = f.Name
		}
	}
	// the runtime and the compiler both rely on these being present
	for _, want := range []string{"Object", "String", "StringBuilder", "Math", "System", "Throwable"} {
		if seen[want] == "" {
			t.Errorf("the standard library does not declare %s", want)
		}
	}
}

// declaredName reports the type declared by a top level line.
func declaredName(line string) (string, bool) {
	for _, kw := range []string{"class ", "interface ", "enum ", "record "} {
		i := strings.Index(line, kw)
		if i < 0 {
			continue
		}
		// the keyword has to start the declaration, after any modifiers
		head := strings.TrimSpace(line[:i])
		if head != "" && !strings.HasSuffix(head, "public") && !strings.HasSuffix(head, "final") &&
			!strings.HasSuffix(head, "abstract") && !strings.HasSuffix(head, "sealed") {
			continue
		}
		rest := line[i+len(kw):]
		end := strings.IndexAny(rest, " <{(:")
		if end < 0 {
			continue
		}
		return rest[:end], true
	}
	return "", false
}
