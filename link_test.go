package teyru_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/driver"
)

// TestTLSReachability drives the decision that says whether a build compiles
// internal/runtime/src/tyrt_tls.c and links -lssl: the program's own call graph
// reaching a TLS entry point.
//
// The observable is the driver's own refusal. windows/amd64 is a target with no
// OpenSSL (mingw-w64 ships none), so checkTLS answers -- before anything is
// written, let alone linked -- exactly for the programs a build would have
// reached for libssl for. A program that calls a TLS method is refused; a
// program whose only route to the TLS layer is reflection is built, and reaches
// the weak stubs in tyrt_net.c at run time instead (tests/programs/
// t241_reflect_tls_unlinked, which asserts the message they raise).
//
// The second case is the one W9 is about. The generated C carries a member table
// for every class as soon as a program reflects at all, and before the rule the
// fixpoint that decides this followed those tables into every method body -- so
// a program that never names TLS was refused for every target without OpenSSL,
// which is the whole Gson- and Spring-shaped half of the standard library on
// Windows, macOS and a cross build.
//
// The first case is what keeps that from being achieved by forgetting TLS
// altogether: a program that does call the layer still finds it, and still hears
// that this target has none rather than a link that ends in an undefined
// reference to SSL_CTX_new.
func TestTLSReachability(t *testing.T) {
	haveCC(t)
	if _, err := exec.LookPath("x86_64-w64-mingw32-gcc"); err != nil {
		t.Skip("x86_64-w64-mingw32-gcc is not on PATH: windows/amd64 cannot be built here")
	}
	// The two programs differ in one thing: how they name the TLS layer. The
	// reflection one names it in a string, the direct one names it in code.
	const viaReflection = `import java.lang.reflect.*

class Main {
  public static void main(String[] args) {
    Class tls = Class.forName("teyru.Tls")
    System.out.println("loaded " + tls.getName())
  }
}
`
	const viaCallSite = `import teyru.*

class Main {
  public static void main(String[] args) {
    System.out.println("ctx=" + Tls.clientContext(""))
  }
}
`
	cases := []struct {
		name    string
		src     string
		refused bool
		why     string
	}{
		{"reflect", viaReflection, false,
			"reflection names the TLS layer, so the program's own code does not reach it"},
		{"call", viaCallSite, true,
			"a call site names the TLS layer"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		file := filepath.Join(dir, c.name+".teyru")
		if err := os.WriteFile(file, []byte(c.src), 0o644); err != nil {
			t.Fatal(err)
		}
		// CSourceOnly: the question is which program the driver links OpenSSL
		// for, and the C compiler is not part of the answer -- which is also
		// what makes this a test that can run where no mingw compiler is
		// installed, once the target is checked below.
		res, err := driver.Compile([]string{file}, driver.Options{
			Target:      "windows/amd64",
			Out:         filepath.Join(dir, c.name+".exe"),
			CSourceOnly: true,
		})
		switch {
		case c.refused && err == nil:
			t.Errorf("windows/amd64 has no OpenSSL, but %s was built: %s", c.name, c.why)
		case c.refused && !strings.Contains(err.Error(), "OpenSSL"):
			t.Errorf("%s: refused for the wrong reason: %v", c.name, err)
		case !c.refused && err != nil:
			// A refusal comes back with no result at all: the driver answers
			// before it writes anything, so there is no C to report.
			diags := ""
			if res != nil {
				diags = "\n" + res.Diags.String()
			}
			t.Errorf("%s must be buildable for a target without OpenSSL (%s), got: %v%s",
				c.name, c.why, err, diags)
		}
	}
}
