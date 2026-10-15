package teyru_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/driver"
)

// TestReflectReach is the reflection reachability scan's test (see
// internal/codegen/reflectset.go): a program that reflects on a standard
// library class keeps that class's member tables, and one that reflects on
// nothing pays for none of the library's.
//
// What the scan must never do is leave out a table a program reads. A table
// left out is not a smaller executable but a wrong answer -- the class answers
// an empty member list -- so the test asks both questions: which tables the
// generated C carries, and what the program answers when it runs.
func TestReflectReach(t *testing.T) {
	haveCC(t)
	const src = `import java.util.*
import java.lang.reflect.*

class Main {
  public static void main(String[] args) throws Exception {
    ArrayList<String> al = new ArrayList<String>()
    al.add("a")
    Class c = al.getClass()
    System.out.println("name=" + c.getName())
    System.out.println("methods=" + c.getDeclaredMethods().length)
    System.out.println("fields=" + c.getDeclaredFields().length)
    Method size = c.getMethod("size")
    System.out.println("size=" + size.invoke(al) + " " + size.getReturnType().getName())
    System.out.println("forName=" + Class.forName("java.util.ArrayList").getName())
  }
}
`
	const want = `name=java.util.ArrayList
methods=29
fields=3
size=1 int
forName=java.util.ArrayList
`
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.teyru")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// The split build (-O0): the member tables are written into the program's
	// unit, and the linker drops the ones the program's entry point does not
	// attach.
	out := filepath.Join(dir, "reflect")
	res, err := driver.Compile([]string{path}, driver.Options{Out: out, Opt: "-O0"})
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, res.Diags)
	}
	wantTables(t, res.CSource)
	got, err := exec.Command(out).CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, got)
	}
	if string(got) != want {
		t.Errorf("the program answered\n%s\n--- want ---\n%s", got, want)
	}
	// The whole-program build (-O2): the tables the program does not use are
	// written and dropped by link-time optimisation, so the same decision has
	// to hold here.
	res2, err := driver.Compile([]string{path}, driver.Options{Opt: "-O2", CSourceOnly: true})
	if err != nil {
		t.Fatalf("emit failed: %v\n%s", err, res2.Diags)
	}
	wantTables(t, res2.CSource)
}

// wantTables checks that the generated C carries the member tables of the class
// the program reflects on and none of the classes it never names.
func wantTables(t *testing.T, src string) {
	t.Helper()
	for _, want := range []string{"mds_teyru_ArrayList", "fds_teyru_ArrayList"} {
		if !strings.Contains(src, want) {
			t.Errorf("the generated C does not carry %s, which the program reflects on", want)
		}
	}
	for _, bad := range []string{"mds_teyru_Validation", "fds_teyru_Validation", "mds_teyru_Gson", "mds_teyru_HttpServer"} {
		if strings.Contains(src, bad) {
			t.Errorf("the generated C carries %s, which this program cannot reflect on", bad)
		}
	}
}

// TestReflectReachFallsBack is the other half: a program whose reflection the
// scan cannot bound keeps every class's tables. A name the compiler cannot read
// is a name it cannot rule out, so `Class.forName` of a string built at run
// time has to leave the class table whole -- and the program has to answer
// correctly for a class it never named.
func TestReflectReachFallsBack(t *testing.T) {
	haveCC(t)
	const src = `import java.util.*
import java.lang.reflect.*

class Main {
  public static void main(String[] args) throws Exception {
    String[] names = { "java.util.ArrayList", "java.util.LinkedList", "java.util.HashMap" }
    for (int i = 0 : i < names.length : i++) {
      Class k = Class.forName(names[i])
      System.out.println(k.getName() + " " + k.getDeclaredMethods().length + " " + k.getDeclaredFields().length)
    }
    // getClass() on an Object-typed value: the receiver's static type bounds
    // nothing, so every class has to keep its tables.
    Object o = new ArrayList<String>()
    Class dyn = o.getClass()
    System.out.println("dyn=" + dyn.getName() + " " + dyn.getDeclaredMethods().length)
  }
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.teyru")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := driver.Compile([]string{path}, driver.Options{Opt: "-O0", CSourceOnly: true})
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, res.Diags)
	}
	// The class table is whole, and so are the tables of classes the program
	// never names.
	for _, want := range []string{"mds_teyru_LinkedList", "mds_teyru_HashMap", "mds_teyru_Validation"} {
		if !strings.Contains(res.CSource, want) {
			t.Errorf("a program that loads a class by a computed name must keep every table; %s is missing", want)
		}
	}
	out := filepath.Join(dir, "fallback")
	res, err = driver.Compile([]string{path}, driver.Options{Out: out, Opt: "-O0"})
	if err != nil {
		t.Fatalf("compile failed: %v\n%s", err, res.Diags)
	}
	got, err := exec.Command(out).CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, got)
	}
	for _, want := range []string{"java.util.ArrayList ", "java.util.LinkedList ", "java.util.HashMap ", "dyn=java.util.ArrayList "} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the program's answer is missing %q:\n%s", want, got)
		}
	}
}
