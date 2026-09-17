package java

import (
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/source"
)

// translate parses Teyru source and prints it as Java, the way `teyru
// emit-java` does.
func translate(t *testing.T, src string) *Result {
	t.Helper()
	diags := &source.Diagnostics{}
	f := parser.Parse(source.NewFile("prog.teyru", src), diags)
	if diags.HasErrors() {
		t.Fatalf("parse failed: %s", diags)
	}
	return Translate([]*ast.File{f})
}

func TestTranslate(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string // lines that must appear in the Java, in order
		not  []string // lines that must not appear
	}{
		{
			name: "statements gain their semicolons",
			src:  "class A {\n  public static void main(String[] args) {\n    int a = 1\n    a++\n  }\n}\n",
			want: []string{"int a = 1;", "a++;", "public static void main(String[] args) {"},
		},
		{
			name: "a for header becomes Java's",
			src:  "class A {\n  public static void main(String[] args) {\n    for (int i = 0 : i < 3 : i++) {\n      System.out.println(i)\n    }\n  }\n}\n",
			want: []string{"for (int i = 0; i < 3; i++) {"},
			not:  []string{"i < 3 : i++"},
		},
		{
			name: "an enhanced for keeps its colon",
			src:  "class A {\n  public static void main(String[] args) {\n    for (String s : args) {\n      System.out.println(s)\n    }\n  }\n}\n",
			want: []string{"for (String s : args) {"},
		},
		{
			name: "val is a final variable of an inferred type",
			src:  "class A {\n  public static void main(String[] args) {\n    val a = 1\n    var b = \"x\"\n  }\n}\n",
			want: []string{"final var a = 1;", "var b = \"x\";"},
		},
		{
			name: "the standard library's names bring their imports",
			src:  "import teyru.*\n\nclass A {\n  public static void main(String[] args) {\n    List<String> xs = new ArrayList<String>()\n    System.out.println(xs.size())\n  }\n}\n",
			want: []string{"import java.util.ArrayList;", "import java.util.List;", "List<String> xs = new ArrayList<String>();"},
			not:  []string{"import teyru"},
		},
		{
			name: "java.lang needs no import",
			src:  "class A {\n  public static void main(String[] args) {\n    System.out.println(Math.abs(-1))\n  }\n}\n",
			want: []string{"System.out.println(Math.abs(-1));"},
			not:  []string{"import java.lang.Math"},
		},
		{
			name: "a type this program declares is not imported over",
			src:  "class List {\n}\n\nclass A {\n  public static void main(String[] args) {\n    List xs = new List()\n  }\n}\n",
			not:  []string{"import java.util.List"},
		},
		{
			name: "a cast keeps the parentheses a precedence change needs",
			src:  "class A {\n  public static void main(String[] args) {\n    int a = 1\n    int b = 2\n    int c = (a + b) * 3\n    int d = -(a + b)\n  }\n}\n",
			want: []string{"int c = (a + b) * 3;", "int d = -(a + b);"},
		},
		{
			name: "a switch keeps its labels and gains its semicolons",
			src:  "class A {\n  public static void main(String[] args) {\n    switch (args.length) {\n      case 1:\n        System.out.println(\"one\")\n        break\n      default:\n        System.out.println(\"other\")\n    }\n  }\n}\n",
			want: []string{"switch (args.length) {", "case 1:", "System.out.println(\"one\");", "break;", "default:"},
		},
		{
			name: "a property is refused by name",
			src:  "class A {\n  public int years {\n    get {\n      return 1\n    }\n  }\n  public static void main(String[] args) {\n  }\n}\n",
			not:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := translate(t, tc.src)
			if len(tc.want) == 0 && len(tc.not) == 0 {
				// the refusal cases: the feature is named and the reason is given
				if len(got.Refusals) == 0 {
					t.Fatalf("expected a refusal, got:\n%s", got.Text)
				}
				if r := got.Refusals[0]; !strings.Contains(r.Feature, "property") || r.Reason == "" {
					t.Errorf("refusal should name the property and say why: %+v", r)
				}
				return
			}
			if len(got.Refusals) > 0 {
				t.Fatalf("unexpected refusal: %v", got.Refusals)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Text, want) {
					t.Errorf("Java is missing %q:\n%s", want, got.Text)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(got.Text, not) {
					t.Errorf("Java should not contain %q:\n%s", not, got.Text)
				}
			}
		})
	}
}

// TestEntryAndFile checks the two names the JDK needs and the source cannot
// give: the class to run, and the name the file must have for javac.
func TestEntryAndFile(t *testing.T) {
	got := translate(t, "class Helper {\n}\n\nclass Main {\n  public static void main(String[] args) {\n    System.out.println(1)\n  }\n}\n")
	if got.Entry != "Main" {
		t.Errorf("entry class is %q, want Main", got.Entry)
	}
	if got.File != "Main" {
		t.Errorf("file name is %q, want Main", got.File)
	}
	got = translate(t, "public class Api {\n  public static void main(String[] args) {\n  }\n}\n\nclass Helper {\n}\n")
	if got.File != "Api" {
		t.Errorf("a public type names the file: got %q, want Api", got.File)
	}
}
