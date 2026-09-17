package driver

import (
	"strings"
	"testing"
)

// TestVersionShape pins the shape of the string `teyru version` prints, because
// it is the one output of this program a script is likely to parse: the version
// first, then the platform in parentheses. The version itself is not asserted --
// it changes when the release does -- but a placeholder is, because the string
// this file replaces had drifted three releases behind the project and nothing
// noticed.
func TestVersionShape(t *testing.T) {
	got := Version()
	if !strings.HasPrefix(got, "teyru ") {
		t.Fatalf("Version() = %q, want a leading %q", got, "teyru ")
	}
	if !strings.HasSuffix(got, ")") {
		t.Fatalf("Version() = %q, want the platform in trailing parentheses", got)
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(got, "teyru "), ")")
	ver, platform, ok := strings.Cut(rest, " (")
	if !ok || ver == "" || platform == "" {
		t.Fatalf("Version() = %q, want \"<version> (<os>/<arch>)\"", got)
	}
	if strings.ContainsAny(ver, " \t") {
		t.Fatalf("the version %q contains whitespace", ver)
	}
	if ver == "0.0.0" || ver == "dev" || ver == "" {
		t.Fatalf("the version is %q, which is a placeholder rather than a release", ver)
	}
	if want := strings.Count(platform, "/"); want != 1 {
		t.Fatalf("the platform %q is not <os>/<arch>", platform)
	}
}
