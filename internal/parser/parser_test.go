package parser

import (
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
)

func parse(t *testing.T, src string) (*ast.File, string) {
	t.Helper()
	d := &source.Diagnostics{}
	f := Parse(source.NewFile("t.teyru", src), d)
	return f, d.String()
}

func TestClassMembers(t *testing.T) {
	src := `class A {
  public int x = 1
  private String name
  public A(int v) {
    x = v
  }
  public int get() {
    return x
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	if len(f.Types) != 1 || f.Types[0].Name != "A" {
		t.Fatalf("expected class A, got %+v", f.Types)
	}
	if len(f.Types[0].Members) != 4 {
		t.Errorf("expected 4 members, got %d", len(f.Types[0].Members))
	}
}

func TestStatementsTerminateAtNewline(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    int a = 1
    int b = 2
    System.out.println(a + b)
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
}

func TestMissingTerminatorIsReported(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    int a = 1 int b = 2
  }
}
`
	_, errs := parse(t, src)
	if !strings.Contains(errs, "TY-SYN") {
		t.Errorf("expected a syntax diagnostic, got %q", errs)
	}
}

func TestContinuationAfterOperator(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    int a = 1 +
      2
    System.out.println(a)
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("an expression may continue after an operator: %s", errs)
	}
}

func TestChainedCallAcrossLines(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    String s = "x"
      .toUpperCase()
      .trim()
    System.out.println(s)
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("a leading dot continues the expression: %s", errs)
	}
}

func TestBasicForColons(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    for (int i = 0 : i < 10 : i++) {
      System.out.println(i)
    }
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	body := f.Types[0].Members[0].(*ast.MethodDecl).Body
	loop, ok := body.Stmts[0].(*ast.For)
	if !ok {
		t.Fatalf("expected a basic for, got %T", body.Stmts[0])
	}
	if len(loop.Init) != 1 || loop.Cond == nil || len(loop.Update) != 1 {
		t.Errorf("for parts incomplete: %+v", loop)
	}
}

func TestTernaryInsideForHeader(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    int n = 3
    for (int i = 0 : i < n ? 5 : 6 : i++) {
      break
    }
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("the conditional ':' must not split the header: %s", errs)
	}
}

// The Java spelling of a statement ends in ';' rather than at a line break
// (decision D6): several statements may share a line, a statement may be empty,
// and a declaration may have no initializer.
func TestSemicolonsSeparateStatements(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    int a = 1; int b = 2;
    int c;
    c = a + b;
    if (a > 0) ; else c = 0;
    System.out.println(c);
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	body := f.Types[0].Members[0].(*ast.MethodDecl).Body
	if len(body.Stmts) != 6 {
		t.Fatalf("expected 6 statements, got %d: %+v", len(body.Stmts), body.Stmts)
	}
	if lv, ok := body.Stmts[2].(*ast.LocalVar); !ok || lv.Vars[0].Init != nil {
		t.Errorf("expected a declaration with no initializer, got %#v", body.Stmts[2])
	}
	if _, ok := body.Stmts[4].(*ast.If).Then.(*ast.Empty); !ok {
		t.Errorf("'if (a > 0) ;' is an empty statement: %#v", body.Stmts[4])
	}
}

// `for (a; b; c)` and `for (a : b : c)` are the same statement, and `for (;;)`
// spells a loop with no condition at all.
func TestJavaForHeader(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    for (int i = 0; i < 10; i++) {
      System.out.println(i);
    }
    for (;;) {
      break;
    }
    int i = 0, j = 0;
    for (i = 0, j = 1; i < j; i++, j--) {
      System.out.println(i);
    }
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	body := f.Types[0].Members[0].(*ast.MethodDecl).Body
	first, ok := body.Stmts[0].(*ast.For)
	if !ok || len(first.Init) != 1 || first.Cond == nil || len(first.Update) != 1 {
		t.Fatalf("for parts incomplete: %#v", body.Stmts[0])
	}
	empty, ok := body.Stmts[1].(*ast.For)
	if !ok || len(empty.Init) != 0 || empty.Cond != nil || len(empty.Update) != 0 {
		t.Fatalf("for (;;) must have no parts: %#v", body.Stmts[1])
	}
	two, ok := body.Stmts[3].(*ast.For)
	if !ok || len(two.Init) != 2 || len(two.Update) != 2 {
		t.Fatalf("comma-separated for parts: %#v", body.Stmts[3])
	}
}

// A ';' is legal where nothing is declared: after a member, after an enum's
// constant list, and after the body of an anonymous class.
func TestSemicolonWhereNothingIsDeclared(t *testing.T) {
	src := `enum E {
  A, B;

  ;
  public int f() {
    return 1;
  }
}

class A {
  Runnable r = new Runnable() {
    public void run() {
    }
  };
  int x = 1;;
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	if len(f.Types[0].EnumConsts) != 2 {
		t.Errorf("expected 2 constants, got %d", len(f.Types[0].EnumConsts))
	}
	if got := len(f.Types[0].Members); got != 1 {
		t.Errorf("expected the enum's one method, got %d members", got)
	}
	if got := len(f.Types[1].Members); got != 2 {
		t.Errorf("expected the class's two fields, got %d members", got)
	}
}

// A declaration with no body ends at its ';', and a try-with-resources header
// separates its resources with one.
func TestSemicolonsInDeclarations(t *testing.T) {
	src := `interface I {
  void f();
  int g();
}

class A {
  public static void main(String[] args) {
    try (java.io.StringWriter a = new java.io.StringWriter(); java.io.StringWriter b = new java.io.StringWriter()) {
      a.write("x");
    } catch (RuntimeException e) {
    } finally {
    }
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	if got := len(f.Types[0].Members); got != 2 {
		t.Errorf("expected the interface's two methods, got %d members", got)
	}
	body := f.Types[1].Members[0].(*ast.MethodDecl).Body
	tr, ok := body.Stmts[0].(*ast.Try)
	if !ok || len(tr.Resources) != 2 {
		t.Fatalf("expected a try with 2 resources, got %#v", body.Stmts[0])
	}
}

func TestEnumMemberSeparator(t *testing.T) {
	src := `enum E {
  A, B

  :
  public int f() {
    return 1
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	if len(f.Types[0].EnumConsts) != 2 {
		t.Errorf("expected 2 constants, got %d", len(f.Types[0].EnumConsts))
	}
}

func TestRecordComponentsAndPatterns(t *testing.T) {
	src := `record P(int x, int y) {
}

class A {
  static String f(Object o) {
    return switch (o) {
      case P(int x, int y) -> "p"
      case String s when s.length() > 2 -> "long"
      default -> "other"
    }
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
}

func TestAccessorsAndField(t *testing.T) {
	src := `class A {
  private int v
  public int value {
    get {
      return field
    }
    set {
      field = value
    }
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	fd, ok := f.Types[0].Members[1].(*ast.FieldDecl)
	if !ok || len(fd.Accessor) != 2 {
		t.Fatalf("expected a property with two accessors, got %+v", f.Types[0].Members[1])
	}
}

func TestAnnotationArguments(t *testing.T) {
	src := `class A {
  @SuppressWarnings({"a", "b"})
  @Named(value = "x", count = 3)
  public void f() {
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
	md := f.Types[0].Members[0].(*ast.MethodDecl)
	if len(md.Annos) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(md.Annos))
	}
	if got := md.Annos[1].Arg("count"); got == nil {
		t.Error("named annotation argument lost")
	}
}

func TestUnnamedVariables(t *testing.T) {
	src := `class A {
  public static void main(String[] args) {
    try {
      f()
    } catch (RuntimeException _) {
    }
  }
  static void f() {
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("parse errors: %s", errs)
	}
}

func TestYieldOnItsOwnLine(t *testing.T) {
	src := `class A {
  static int f(int n) {
    return switch (n) {
      case 1 -> {
        int v = 10
        yield v
      }
      default -> {
        yield -1
      }
    }
  }
}
`
	f, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("yield may start a line: %s", errs)
	}
	body := f.Types[0].Members[0].(*ast.MethodDecl).Body
	sw := body.Stmts[0].(*ast.Return).X.(*ast.SwitchExpr)
	block, ok := sw.S.Cases[0].Body[0].(*ast.Block)
	if !ok {
		t.Fatalf("expected a case body block, got %T", sw.S.Cases[0].Body[0])
	}
	if _, ok := block.Stmts[len(block.Stmts)-1].(*ast.Yield); !ok {
		t.Errorf("expected a yield statement, got %T", block.Stmts[len(block.Stmts)-1])
	}
}

func TestYieldAsAVariableName(t *testing.T) {
	src := `class A {
  static int f() {
    int yield = 5
    yield = yield + 1
    return yield
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("yield is contextual and may be a variable: %s", errs)
	}
}

func TestRecordPatternWithoutComponents(t *testing.T) {
	src := `record Dot() {
}

class A {
  static String f(Object o) {
    return switch (o) {
      case Dot() -> "dot"
      default -> "other"
    }
  }
}
`
	_, errs := parse(t, src)
	if errs != "" {
		t.Fatalf("a record with no components still matches with Dot(): %s", errs)
	}
}
