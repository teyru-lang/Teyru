package java

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

// printer turns a program into Java text. It collects the imports the JDK
// needs as it goes -- the standard library is one package's names in Teyru and
// a dozen packages' names in Java, and which of them a file uses is only known
// once it has been printed.
type printer struct {
	out      *strings.Builder
	indent   int
	imports  map[string]bool // every type imported, by fully qualified name
	statics  []string        // import lines the source wrote, kept as written
	decl     map[string]bool // types this program declares itself
	refusals []Refusal
	entry    string
	fileName string
	// cls is the type whose body is being printed, which a constructor's name
	// is: Teyru calls them all `<init>`, and Java wants the class's own name.
	cls string
	// unnamed counts the `_` patterns printed so far. Java 21 has unnamed
	// variables only behind --enable-preview, so each one is printed as a name
	// of its own that nothing refers to.
	unnamed int
}

// unnamedName is a binding name for a pattern that binds nothing.
func (p *printer) unnamedName() string {
	p.unnamed++
	return fmt.Sprintf("tyUnnamed%d", p.unnamed)
}

func (p *printer) line(format string, args ...any) {
	if format == "" {
		p.out.WriteByte('\n')
		return
	}
	p.out.WriteString(strings.Repeat("  ", p.indent))
	fmt.Fprintf(p.out, format, args...)
	p.out.WriteByte('\n')
}

// capture runs fn against a buffer of its own and returns what it wrote. The
// printer needs it where a construct has to be embedded in the middle of a line
// that is already being written -- an anonymous class body, a lambda block, a
// switch expression.
func (p *printer) capture(fn func()) string {
	saved := p.out
	p.out = &strings.Builder{}
	fn()
	text := p.out.String()
	p.out = saved
	return text
}

// refuse records a feature that has no Java spelling. Printing continues, so
// that one run reports every reason rather than only the first.
func (p *printer) refuse(pos source.Pos, feature, reason string) {
	p.refusals = append(p.refusals, Refusal{Pos: pos, Feature: feature, Reason: reason})
}

// imported adds a single-type import and returns the name to print. A type the
// program declares itself, and anything in java.lang, needs no import -- and an
// import that collides with the compilation unit's own type is an error in
// Java, so the program's own type always wins.
func (p *printer) imported(fqn string) string {
	dot := strings.LastIndexByte(fqn, '.')
	pkg, name := fqn[:dot], fqn[dot+1:]
	if p.decl[name] || pkg == "java.lang" {
		return name
	}
	if _, ok := p.imports[fqn]; !ok {
		p.imports[fqn] = true
	}
	return name
}

// isReaderish reports whether an argument is already something Java's
// BufferedReader takes: a nested new whose class is named ...Reader.
func isReaderish(e ast.Expr) bool {
	if n, ok := e.(*ast.New); ok && n.Type != nil {
		return strings.HasSuffix(n.Type.Name, "Reader")
	}
	return false
}

// name rewrites a class name as the source wrote it into its Java spelling and
// records the import it needs. A dotted name is left alone: a java.* path and a
// nested type are both valid Java as written.
func (p *printer) name(pos source.Pos, s string) string {
	if s == "" {
		return s
	}
	if i := strings.IndexByte(s, '.'); i > 0 {
		/* A nested type: Map.Entry. The dotted spelling is valid Java only when
		   the outer class is imported, and leaving it alone without that import
		   is what made a printed program say "package Map does not exist" --
		   javac reads Map as a package when nothing has named the class. So the
		   outer class is what gets the import; a name already written with its
		   own package (java.util.Map.Entry) maps to nothing here and is left
		   alone, which is right, because it needs no import. */
		if fqn, ok := util.JavaClassOf(s[:i]); ok {
			p.imported(fqn)
		}
		return s
	}
	if reason, ok := refuseStdlib[s]; ok {
		p.refuse(pos, "the standard library class "+s, reason)
		return s
	}
	if fqn, ok := util.JavaClassOf(s); ok {
		return p.imported(fqn)
	}
	return s
}

// ---------------------------------------------------------------- the file

// Translate renders the program's files as one Java compilation unit.
func (p *printer) run(files []*ast.File) {
	for _, f := range files {
		for _, d := range f.Types {
			p.decl[d.Name] = true
		}
	}
	p.entry, p.fileName = entryOf(files)
	for _, f := range files {
		p.file(f)
	}
}

// entryOf finds the class that holds `main` -- the class to run -- and the name
// the file must have, which javac ties to the public type it declares.
func entryOf(files []*ast.File) (entry, fileName string) {
	pub := ""
	pubs := 0
	for _, f := range files {
		// `package teyru` is the standard library's package and anything else
		// is refused, so the compilation unit is always the default package.
		if f.DeclaredPackage != "" && f.DeclaredPackage != "teyru" {
			continue
		}
		for _, d := range f.Types {
			if d.Implicit {
				continue
			}
			if d.Mods.Has(ast.ModPublic) {
				pub, pubs = d.Name, pubs+1
			}
			if entry != "" {
				continue
			}
			for _, m := range d.Members {
				md, ok := m.(*ast.MethodDecl)
				if ok && md.Name == "main" && md.Mods.Has(ast.ModStatic) && len(md.Params) == 1 {
					entry = d.Name
				}
			}
		}
	}
	switch {
	case pubs == 1 && pub != "":
		fileName = pub
	case entry != "":
		fileName = entry
	}
	return entry, fileName
}

// file checks what a whole compilation unit has to satisfy for Java to accept
// it: one package (Java's file-per-package rule) and, if a type carries a
// package, the standard library's own, which is translated by name so the
// clause goes with it.
func (p *printer) file(f *ast.File) {
	if f.DeclaredPackage != "" && f.DeclaredPackage != "teyru" {
		pos := source.Pos{}
		if len(f.Types) > 0 {
			pos = f.Types[0].Pos
		}
		p.refuse(pos, "package "+f.DeclaredPackage,
			"emit-java prints one compilation unit; a named package needs a file per package")
	}
	for _, imp := range f.Imports {
		if imp.Static {
			// `import static java.lang.Math.*` is Java already. A static import
			// of a class this program declares is not: Java has no way to name
			// a type in the default package from an import, and the classes
			// that import brings in exist nowhere else.
			head := imp.Path
			if i := strings.LastIndexByte(head, '.'); i >= 0 {
				head = head[:i]
			}
			if p.decl[head] {
				p.refuse(imp.Pos, "the static import "+imp.Path,
					"Java cannot statically import a type from the default package, and this program declares "+head)
				continue
			}
			p.statics = append(p.statics, "import static "+imp.Path+star(imp)+";")
			continue
		}
		switch {
		case imp.Path == "teyru" || strings.HasPrefix(imp.Path, "teyru."):
			// the standard library, inlined by the name table above
		default:
			if reason, ok := pathReason(imp.Path); ok {
				p.refuse(imp.Pos, "the import "+imp.Path, reason)
			}
			p.statics = append(p.statics, "import "+imp.Path+star(imp)+";")
		}
	}
}

func (p *printer) types(f *ast.File) {
	for _, d := range f.Types {
		if d.Implicit {
			p.refuse(d.Pos, "the compact source file "+d.Name,
				"an unnamed class implicitly imports java.io.IO (JEP 512); Java 21 has no java.io.IO")
			continue
		}
		p.classDecl(d, false)
		p.line("")
	}
}

// finish puts the header, the imports the body turned out to need, and the
// body together.
func (p *printer) finish(body string) string {
	out := &strings.Builder{}
	saved := p.out
	p.out = out
	p.line("// Java for this program, printed by `teyru emit-java`: statements end")
	p.line("// with ';', `for (a : b : c)` is `for (a; b; c)`, and the standard")
	p.line("// library is spelled with the java.* names it copies.")
	if p.entry != "" {
		p.line("//")
		p.line("// The class to run is %s.", p.entry)
	}
	if len(p.imports) > 0 || len(p.statics) > 0 {
		p.line("")
		for _, imp := range p.statics {
			p.line("%s", imp)
		}
		for _, fqn := range sortedFQNs(p.imports) {
			p.line("import %s;", fqn)
		}
	}
	p.line("")
	p.out.WriteString(body)
	p.out = saved
	return out.String()
}

func sortedFQNs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------------------------------------------------------- declarations

// abstractInJava are the standard library's interfaces that the JDK has as
// abstract classes: a Teyru class may implement them, and a Java class may only
// extend them.
var abstractInJava = map[string]bool{"Writer": true, "Reader": true, "OutputStream": true}

func (p *printer) classDecl(d *ast.ClassDecl, local bool) {
	if d == nil {
		return
	}
	outer := p.cls
	p.cls = d.Name
	defer func() { p.cls = outer }()
	for _, imp := range d.Implements {
		if abstractInJava[imp.Name] {
			p.refuse(d.Pos, "`implements "+imp.Name+"`",
				"java.io."+imp.Name+" is an abstract class, not an interface, so a class cannot implement it")
		}
	}
	p.annotations(d.Annos)
	switch d.Kind {
	case ast.KindEnum:
		p.line("%senum %s%s {", p.mods(d.Mods), d.Name, p.implementsOf(d))
		p.enumBody(d)
		return
	case ast.KindRecord:
		p.line("%srecord %s%s(%s)%s {", p.mods(d.Mods), d.Name,
			p.typeParams(d.TypeParams), p.recordComps(d), p.implementsOf(d))
	case ast.KindAnnotation:
		p.line("%s@interface %s {", p.mods(d.Mods), d.Name)
	case ast.KindInterface:
		p.line("%sinterface %s%s%s {", p.mods(d.Mods), d.Name,
			p.typeParams(d.TypeParams), p.extendsOf(d))
	default:
		p.line("%sclass %s%s%s {", p.mods(d.Mods), d.Name,
			p.typeParams(d.TypeParams), p.extendsOf(d))
	}
	p.indent++
	for _, m := range d.Members {
		p.member(m)
	}
	p.indent--
	p.line("}")
}

func (p *printer) recordComps(d *ast.ClassDecl) string {
	parts := make([]string, 0, len(d.RecordComps))
	for _, c := range d.RecordComps {
		parts = append(parts, p.typeExpr(c.Type)+" "+c.Name)
	}
	return strings.Join(parts, ", ")
}

func (p *printer) enumBody(d *ast.ClassDecl) {
	p.indent++
	for i, c := range d.EnumConsts {
		sep := ","
		if i == len(d.EnumConsts)-1 {
			sep = ";"
		}
		head := c.Name + p.args(c.Args)
		if c.Body == nil {
			p.line("%s%s", head, sep)
			continue
		}
		p.line("%s {", head)
		p.indent++
		for _, m := range c.Body.Members {
			p.member(m)
		}
		p.indent--
		p.line("}%s", sep)
	}
	if len(d.EnumConsts) == 0 {
		p.line(";")
	}
	for _, m := range d.Members {
		p.member(m)
	}
	p.indent--
	p.line("}")
}

func (p *printer) member(m ast.Member) {
	switch d := m.(type) {
	case *ast.FieldDecl:
		p.fieldDecl(d)
	case *ast.MethodDecl:
		p.methodDecl(d)
	case *ast.InitBlock:
		if d.Static {
			p.line("static {")
		} else {
			p.line("{")
		}
		p.blockBody(d.Body)
		p.line("}")
	case *ast.ClassDecl:
		p.classDecl(d, false)
		p.line("")
	default:
		p.refuse(source.Pos{}, fmt.Sprintf("the member %T", m), "the printer does not know this member")
	}
}

func (p *printer) fieldDecl(d *ast.FieldDecl) {
	if len(d.Accessor) > 0 {
		// A Teyru property is not a Java field: reading it is a call, and its
		// backing storage is spelled `field`. Printing only the declaration
		// would drop the accessor bodies, which is the one thing this printer
		// must never do.
		name := ""
		if len(d.Vars) > 0 {
			name = d.Vars[0].Name
		}
		p.refuse(d.Pos, "the native property "+name,
			"Java has no property syntax: the accessors would become methods and every use of the name a call")
		return
	}
	p.annotations(d.Annos)
	for _, v := range d.Vars {
		typ := p.typeExpr(d.Type) + strings.Repeat("[]", v.Dims)
		if v.Init == nil {
			p.line("%s%s %s;", p.mods(d.Mods), typ, v.Name)
			continue
		}
		p.line("%s%s %s = %s;", p.mods(d.Mods), typ, v.Name, p.expr(v.Init))
	}
}

func (p *printer) methodDecl(d *ast.MethodDecl) {
	p.annotations(d.Annos)
	if d.Mods.Has(ast.ModNative) {
		p.refuse(d.Pos, "the native method "+d.Name,
			"a native method is implemented in C and linked in; Java has no such method")
		return
	}
	head := ""
	if d.IsCtor {
		head = d.Name
		if head == "" || head == "<init>" {
			head = p.cls
		}
	} else {
		// A method's type parameters come before its result type in Java.
		if tp := p.typeParams(d.TypeParams); tp != "" {
			head = tp + " "
		}
		head += p.typeExpr(d.Result) + " " + d.Name
	}
	if d.Compact {
		// A compact record constructor has no parameter list of its own.
		p.line("%s%s {", p.mods(d.Mods), head)
		p.blockBody(d.Body)
		p.line("}")
		return
	}
	head += "(" + p.params(d.Params) + ")"
	if len(d.Throws) > 0 {
		head += " throws " + strings.Join(p.typeList(d.Throws), ", ")
	}
	if d.Body == nil {
		if d.Default != nil {
			p.line("%s%s default %s;", p.mods(d.Mods), head, p.expr(d.Default))
			return
		}
		p.line("%s%s;", p.mods(d.Mods), head)
		return
	}
	if d.IsCtor {
		p.checkSuperFirst(d)
	}
	p.line("%s%s {", p.mods(d.Mods), head)
	p.blockBody(d.Body)
	p.line("}")
}

// checkSuperFirst refuses a constructor that does anything before its super or
// this call. Java requires that call to be the first statement of a
// constructor, and Teyru does not, so the program has no Java spelling rather
// than a reordered one.
func (p *printer) checkSuperFirst(d *ast.MethodDecl) {
	if d.Body == nil {
		return
	}
	for i, s := range d.Body.Stmts {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.Call)
		if !ok || !call.ThisCtor {
			continue
		}
		if i != 0 {
			p.refuse(call.Pos, "a constructor that runs statements before its super call",
				"Java requires super(...) or this(...) to be the constructor's first statement")
		}
		return
	}
}

// annotations prints the annotations written on a declaration. The ones that
// are Teyru-only are refused rather than printed: they would not compile, and a
// program whose meaning depends on them is not the same program.
func (p *printer) annotations(as []*ast.Annotation) {
	for _, a := range as {
		simple := a.Name
		if i := strings.LastIndexByte(simple, '.'); i >= 0 {
			simple = simple[i+1:]
		}
		if reason, ok := annotationReasons[simple]; ok {
			p.refuse(a.Pos, "the annotation @"+simple, reason)
			continue
		}
		if a.Name == "" {
			continue
		}
		text := "@" + a.Name
		if len(a.Args) > 0 {
			args := make([]string, 0, len(a.Args))
			for _, arg := range a.Args {
				v := p.annoValue(arg)
				if arg.Name != "" {
					v = arg.Name + " = " + v
				}
				args = append(args, v)
			}
			text += "(" + strings.Join(args, ", ") + ")"
		}
		p.line("%s", text)
	}
}

func (p *printer) annoValue(a *ast.AnnoArg) string {
	if a.Anno != nil {
		text := "@" + a.Anno.Name
		if len(a.Anno.Args) > 0 {
			args := make([]string, 0, len(a.Anno.Args))
			for _, arg := range a.Anno.Args {
				v := p.annoValue(arg)
				if arg.Name != "" {
					v = arg.Name + " = " + v
				}
				args = append(args, v)
			}
			text += "(" + strings.Join(args, ", ") + ")"
		}
		return text
	}
	if a.Value == nil {
		return ""
	}
	return p.expr(a.Value)
}

// mods prints the modifiers that survive into Java, in the order javac's own
// style uses.
func (p *printer) mods(m ast.Mods) string {
	var out []string
	for _, bit := range []struct {
		flag ast.Mods
		text string
	}{
		{ast.ModPublic, "public"},
		{ast.ModProtected, "protected"},
		{ast.ModPrivate, "private"},
		{ast.ModAbstract, "abstract"},
		{ast.ModStatic, "static"},
		{ast.ModSealed, "sealed"},
		{ast.ModNonSealed, "non-sealed"},
		{ast.ModFinal, "final"},
		{ast.ModSynchronized, "synchronized"},
		{ast.ModTransient, "transient"},
		{ast.ModVolatile, "volatile"},
		{ast.ModNative, "native"},
		{ast.ModStrictfp, "strictfp"},
		{ast.ModDefault, "default"},
	} {
		if m.Has(bit.flag) {
			out = append(out, bit.text)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ") + " "
}

func (p *printer) typeParams(tps []*ast.TypeParam) string {
	if len(tps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tps))
	for _, tp := range tps {
		text := tp.Name
		if len(tp.Bounds) > 0 {
			bounds := make([]string, 0, len(tp.Bounds))
			for _, b := range tp.Bounds {
				bounds = append(bounds, p.typeExpr(b))
			}
			text += " extends " + strings.Join(bounds, " & ")
		}
		parts = append(parts, text)
	}
	return "<" + strings.Join(parts, ", ") + ">"
}

func (p *printer) typeList(ts []*ast.TypeExpr) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, p.typeExpr(t))
	}
	return out
}

func (p *printer) extendsOf(d *ast.ClassDecl) string {
	var out string
	if len(d.Extends) > 0 {
		out += " extends " + strings.Join(p.typeList(d.Extends), ", ")
	}
	if len(d.Implements) > 0 {
		out += " implements " + strings.Join(p.typeList(d.Implements), ", ")
	}
	if len(d.Permits) > 0 {
		out += " permits " + strings.Join(p.typeList(d.Permits), ", ")
	}
	return out
}

func (p *printer) implementsOf(d *ast.ClassDecl) string {
	if len(d.Implements) > 0 {
		return " implements " + strings.Join(p.typeList(d.Implements), ", ")
	}
	return ""
}

func (p *printer) params(ps []*ast.Param) string {
	out := make([]string, 0, len(ps))
	for _, prm := range ps {
		out = append(out, p.param(prm))
	}
	return strings.Join(out, ", ")
}

func (p *printer) param(prm *ast.Param) string {
	if prm == nil {
		return ""
	}
	if prm.Name == "" && prm.Type != nil {
		// a record pattern: the type, then its component patterns
		inner := make([]string, 0, len(prm.Decomp))
		for _, c := range prm.Decomp {
			inner = append(inner, p.param(c))
		}
		return p.typeExpr(prm.Type) + "(" + strings.Join(inner, ", ") + ")"
	}
	var b strings.Builder
	if prm.Mods != 0 {
		b.WriteString(p.mods(prm.Mods))
	}
	for _, a := range prm.Annos {
		b.WriteString("@" + a.Name + " ")
	}
	if prm.Unnamed || (prm.Type != nil && prm.Name == "_") {
		// `_` binds nothing; Java 21 has unnamed patterns only as a preview
		// feature, so it is printed as a name nothing refers to.
		b.WriteString(p.typeExpr(prm.Type) + " " + p.unnamedName())
		return b.String()
	}
	if prm.Varargs {
		b.WriteString(p.elemType(prm.Type) + "... " + prm.Name)
		return b.String()
	}
	if prm.Type != nil {
		b.WriteString(p.typeExpr(prm.Type) + " ")
	}
	b.WriteString(prm.Name)
	return b.String()
}

// pattern prints a type pattern -- `String s`, a record pattern, or the `_`
// that binds nothing -- the way Java spells it.
func (p *printer) pattern(b *ast.Param, fallback *ast.TypeExpr) string {
	if b == nil {
		return p.typeExpr(fallback)
	}
	if b.Type != nil && primitives[b.Type.Name] {
		p.refuse(b.Pos, "the primitive type pattern "+b.Type.Name,
			"a pattern over a primitive (JEP 507) is a Java 25 feature; Java 21 has reference patterns only")
	}
	if len(b.Decomp) > 0 || b.Name == "" {
		// a record pattern -- `Line(Point p, int n)`, or `Dot()` for a record
		// with no components -- which param already prints
		return p.param(b)
	}
	typ := b.Type
	if typ == nil {
		typ = fallback
	}
	if b.Unnamed || b.Name == "_" {
		return p.typeExpr(typ) + " " + p.unnamedName()
	}
	return p.typeExpr(typ) + " " + b.Name
}

// star is the `.*` an on-demand import ends with.
func star(imp *ast.Import) string {
	if imp.Star {
		return ".*"
	}
	return ""
}

// primitives are the type names Java has as primitives, which a pattern
// cannot name before Java 25.
var primitives = map[string]bool{
	"void": true, "boolean": true, "byte": true, "short": true, "char": true,
	"int": true, "long": true, "float": true, "double": true,
}

// elemType prints the element type of a parameter declared `T...`.
func (p *printer) elemType(t *ast.TypeExpr) string {
	if t == nil {
		return "Object"
	}
	clone := *t
	if clone.Dims > 0 {
		clone.Dims--
	}
	return p.typeExpr(&clone)
}

func (p *printer) args(es []ast.Expr) string {
	if len(es) == 0 {
		return ""
	}
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, p.expr(e))
	}
	return "(" + strings.Join(out, ", ") + ")"
}

// ---------------------------------------------------------------- types

func (p *printer) typeExpr(t *ast.TypeExpr) string {
	if t == nil {
		return "Object"
	}
	var b strings.Builder
	switch t.Wildcard {
	case 1:
		b.WriteByte('?')
	case 2:
		b.WriteString("? extends " + p.typeExpr(t.Bound))
	case 3:
		b.WriteString("? super " + p.typeExpr(t.Bound))
	default:
		name := t.Name
		if name == "" {
			name = "Object"
		}
		b.WriteString(p.name(t.Pos, name))
		if len(t.Args) > 0 {
			args := make([]string, 0, len(t.Args))
			for _, a := range t.Args {
				args = append(args, p.typeExpr(a))
			}
			b.WriteString("<" + strings.Join(args, ", ") + ">")
		}
	}
	b.WriteString(strings.Repeat("[]", t.Dims))
	return b.String()
}

// ---------------------------------------------------------------- statements

func (p *printer) blockBody(b *ast.Block) {
	if b == nil {
		return
	}
	p.indent++
	for _, s := range b.Stmts {
		p.stmt(s)
	}
	p.indent--
}

func (p *printer) stmt(s ast.Stmt) {
	switch d := s.(type) {
	case *ast.Block:
		p.line("{")
		p.blockBody(d)
		p.line("}")
	case *ast.Empty:
		p.line(";")
	case *ast.LocalVar:
		p.localVar(d)
	case *ast.LocalClass:
		p.classDecl(d.Decl, true)
		p.line("")
	case *ast.ExprStmt:
		p.line("%s;", p.expr(d.X))
	case *ast.If:
		p.ifStmt(d)
	case *ast.While:
		p.line("while (%s) {", p.expr(d.Cond))
		p.body(d.Body)
		p.line("}")
	case *ast.DoWhile:
		p.line("do {")
		p.body(d.Body)
		p.line("} while (%s);", p.expr(d.Cond))
	case *ast.For:
		p.forStmt(d)
	case *ast.ForEach:
		p.line("for (%s : %s) {", p.param(d.Var), p.expr(d.X))
		p.body(d.Body)
		p.line("}")
	case *ast.Return:
		if d.X == nil {
			p.line("return;")
		} else {
			p.line("return %s;", p.expr(d.X))
		}
	case *ast.Break:
		if d.Label != "" {
			p.line("break %s;", d.Label)
		} else {
			p.line("break;")
		}
	case *ast.Continue:
		if d.Label != "" {
			p.line("continue %s;", d.Label)
		} else {
			p.line("continue;")
		}
	case *ast.Throw:
		p.line("throw %s;", p.expr(d.X))
	case *ast.Try:
		p.tryStmt(d)
	case *ast.Switch:
		p.switchStmt(d)
	case *ast.Yield:
		p.line("yield %s;", p.expr(d.X))
	case *ast.Labeled:
		p.line("%s:", d.Label)
		p.stmt(d.Body)
	case *ast.Assert:
		if d.Msg == nil {
			p.line("assert %s;", p.expr(d.Cond))
		} else {
			p.line("assert %s : %s;", p.expr(d.Cond), p.expr(d.Msg))
		}
	case *ast.Sync:
		p.line("synchronized (%s) {", p.expr(d.Lock))
		p.blockBody(d.Body)
		p.line("}")
	default:
		p.refuse(source.Pos{}, fmt.Sprintf("the statement %T", s), "the printer does not know this statement")
	}
}

// ifStmt prints an if/else chain with `else if` kept as written rather than
// nested in a block nobody wrote.
func (p *printer) ifStmt(d *ast.If) {
	head := "if (" + p.expr(d.Cond) + ") {"
	for {
		p.line("%s", head)
		p.body(d.Then)
		if d.Else == nil {
			p.line("}")
			return
		}
		nxt, ok := d.Else.(*ast.If)
		if !ok {
			p.line("} else {")
			p.body(d.Else)
			p.line("}")
			return
		}
		head = "} else if (" + p.expr(nxt.Cond) + ") {"
		d = nxt
	}
}

// body prints the body of a control statement: a block keeps its braces, and a
// single statement gets them, because a Teyru statement ends at a newline and a
// Java one at a `;`, and the two do not nest the same way.
func (p *printer) body(s ast.Stmt) {
	if b, ok := s.(*ast.Block); ok {
		p.blockBody(b)
		return
	}
	p.indent++
	p.stmt(s)
	p.indent--
}

func (p *printer) localVar(d *ast.LocalVar) {
	p.annotations(d.Annos)
	declared := ""
	if d.Type != nil {
		declared = d.Type.Name
	}
	switch declared {
	case "val", "var":
		for _, v := range d.Vars {
			if v.Init == nil {
				p.refuse(v.Pos, "the inferred declaration `"+declared+" "+v.Name+"`",
					"an inferred type needs a value to infer it from, and there is none")
				continue
			}
			// `val` is not a Java keyword: it is a final variable whose type
			// is inferred, which is what `final var` says.
			final := ""
			if declared == "val" {
				final = "final "
			}
			p.line("%svar %s%s = %s;", final, v.Name, strings.Repeat("[]", v.Dims), p.expr(v.Init))
		}
		return
	}
	typ := p.typeExpr(d.Type)
	for _, v := range d.Vars {
		if v.Init == nil {
			p.line("%s %s%s;", typ, v.Name, strings.Repeat("[]", v.Dims))
			continue
		}
		p.line("%s %s%s = %s;", typ, v.Name, strings.Repeat("[]", v.Dims), p.expr(v.Init))
	}
}

func (p *printer) forStmt(d *ast.For) {
	var init, update []string
	for _, s := range d.Init {
		init = append(init, p.forInit(s))
	}
	for _, e := range d.Update {
		update = append(update, p.expr(e))
	}
	cond := ""
	if d.Cond != nil {
		cond = p.expr(d.Cond)
	}
	// Teyru separates the three parts of a for with `:`, Java with `;`.
	p.line("for (%s; %s; %s) {", strings.Join(init, ", "), cond, strings.Join(update, ", "))
	p.body(d.Body)
	p.line("}")
}

// forInit prints one statement of a Java `for` header, which carries no `;` of
// its own.
func (p *printer) forInit(s ast.Stmt) string {
	switch d := s.(type) {
	case *ast.ExprStmt:
		return p.expr(d.X)
	case *ast.LocalVar:
		parts := make([]string, 0, len(d.Vars))
		for _, v := range d.Vars {
			if v.Init == nil {
				p.refuse(v.Pos, "the for initialiser "+v.Name,
					"a for header declares and assigns in one statement")
				continue
			}
			parts = append(parts, v.Name+strings.Repeat("[]", v.Dims)+" = "+p.expr(v.Init))
		}
		return p.typeExpr(d.Type) + " " + strings.Join(parts, ", ")
	default:
		p.refuse(source.Pos{}, fmt.Sprintf("the for initialiser %T", s), "the printer does not know this statement")
		return ""
	}
}

func (p *printer) tryStmt(d *ast.Try) {
	head := "try"
	if len(d.Resources) > 0 {
		res := make([]string, 0, len(d.Resources))
		for _, r := range d.Resources {
			switch x := r.(type) {
			case *ast.LocalVar:
				for _, v := range x.Vars {
					res = append(res, p.typeExpr(x.Type)+" "+v.Name+" = "+p.expr(v.Init))
				}
			case *ast.ExprStmt:
				if _, isName := x.X.(*ast.Ident); !isName {
					p.refuse(x.Pos, "the try-with-resources resource "+p.expr(x.X),
						"Java takes a variable declaration or a variable name here, not an expression")
				}
				res = append(res, p.expr(x.X))
			}
		}
		head += " (" + strings.Join(res, "; ") + ")"
	}
	p.line("%s {", head)
	p.blockBody(d.Body)
	prefix := "} "
	for _, c := range d.Catches {
		types := make([]string, 0, len(c.Types))
		for _, t := range c.Types {
			types = append(types, p.typeExpr(t))
		}
		name := c.Name
		if c.Unnamed || name == "_" {
			name = p.unnamedName()
		}
		p.line("%scatch (%s %s) {", prefix, strings.Join(types, " | "), name)
		p.blockBody(c.Body)
	}
	if d.Finally != nil {
		p.line("%sfinally {", prefix)
		p.blockBody(d.Finally)
	}
	p.line("}")
}

func (p *printer) switchStmt(d *ast.Switch) {
	p.line("switch (%s) {", p.expr(d.X))
	p.indent++
	for _, c := range d.Cases {
		p.switchCase(c)
	}
	p.indent--
	p.line("}")
}

func (p *printer) switchCase(c *ast.Case) {
	switch {
	case c.Arrow && c.ArrowX != nil:
		p.line("%s -> %s;", p.caseHead(c), p.expr(c.ArrowX))
	case c.Arrow && len(c.Body) == 1:
		if _, ok := c.Body[0].(*ast.Block); ok {
			p.line("%s -> {", p.caseHead(c))
			p.body(c.Body[0])
			p.line("}")
			return
		}
		// one statement: `case L -> expr;`, `-> throw e;`
		text := strings.TrimSpace(p.capture(func() { p.stmt(c.Body[0]) }))
		p.line("%s -> %s", p.caseHead(c), text)
	case c.Arrow:
		p.line("%s -> {", p.caseHead(c))
		for _, s := range c.Body {
			p.stmt(s)
		}
		p.line("}")
	default:
		p.line("%s:", p.caseHead(c))
		p.indent++
		for _, s := range c.Body {
			p.stmt(s)
		}
		p.indent--
	}
}

// caseHead is the label part of a case, without the arrow or the colon.
func (p *printer) caseHead(c *ast.Case) string {
	var parts []string
	if c.Null {
		parts = append(parts, "null")
	}
	if c.Pattern != nil {
		parts = append(parts, p.pattern(c.Pattern, nil))
	}
	for _, l := range c.Labels {
		parts = append(parts, p.expr(l))
	}
	head := ""
	if len(parts) == 0 {
		head = "default"
	} else {
		head = "case " + strings.Join(parts, ", ")
	}
	if c.Guard != nil {
		head += " when " + p.expr(c.Guard)
	}
	return head
}

// ---------------------------------------------------------------- expressions

// Precedence levels, from the loosest binding to the tightest, used to put back
// the parentheses Teyru's parser does not keep in the tree.
const (
	precLambda = 1
	precAssign = 2
	precCond   = 3
	precOrOr   = 4
	precAndAnd = 5
	precBitOr  = 6
	precBitXor = 7
	precBitAnd = 8
	precEq     = 9
	precRel    = 10
	precShift  = 11
	precAdd    = 12
	precMul    = 13
	precUnary  = 14
	precPost   = 15
	precPrim   = 16
)

func (p *printer) expr(e ast.Expr) string { return p.exprAt(e, 0) }

func (p *printer) exprAt(e ast.Expr, min int) string {
	if e == nil {
		return ""
	}
	text, prec := p.raw(e)
	if prec < min {
		return "(" + text + ")"
	}
	return text
}

func (p *printer) raw(e ast.Expr) (string, int) {
	switch d := e.(type) {
	case *ast.Literal:
		return p.literal(d), precPrim
	case *ast.Ident:
		return p.name(d.Pos, d.Name), precPrim
	case *ast.Select:
		if d.Raw {
			p.refuse(d.Pos, "the property backing storage `field`",
				"Java has no property, so there is no backing storage to name")
		}
		return p.exprAt(d.X, precPrim) + "." + d.Name, precPrim
	case *ast.Index:
		return p.exprAt(d.X, precPost) + "[" + p.expr(d.Index) + "]", precPost
	case *ast.Call:
		return p.call(d), precPost
	case *ast.New:
		return p.newExpr(d), precPost
	case *ast.NewArray:
		return p.newArray(d), precPost
	case *ast.ArrayInit:
		elems := make([]string, 0, len(d.Elems))
		for _, x := range d.Elems {
			elems = append(elems, p.expr(x))
		}
		return "{" + strings.Join(elems, ", ") + "}", precPrim
	case *ast.Unary:
		return p.unary(d)
	case *ast.Binary:
		return p.binary(d)
	case *ast.Assign:
		return p.exprAt(d.X, precCond+1) + " " + d.Op + " " + p.exprAt(d.Y, precAssign), precAssign
	case *ast.Cond:
		return p.exprAt(d.C, precCond+1) + " ? " + p.exprAt(d.X, precCond) + " : " + p.exprAt(d.Y, precCond), precCond
	case *ast.Cast:
		return "(" + p.typeExpr(d.Type) + ") " + p.exprAt(d.X, precUnary), precUnary
	case *ast.InstanceOf:
		text := p.exprAt(d.X, precRel) + " instanceof "
		if d.Binding != nil {
			return text + p.pattern(d.Binding, d.Type), precRel
		}
		return text + p.typeExpr(d.Type), precRel
	case *ast.Lambda:
		return p.lambda(d), precLambda
	case *ast.MethodRef:
		return p.methodRef(d), precLambda
	case *ast.This:
		if d.Qual != "" {
			return d.Qual + ".this", precPrim
		}
		return "this", precPrim
	case *ast.SuperExpr:
		return "super", precPrim
	case *ast.SwitchExpr:
		return strings.TrimSuffix(p.capture(func() { p.switchStmt(d.S) }), "\n"), precPrim
	case *ast.ClassLit:
		return p.typeExpr(d.Type) + ".class", precPrim
	case *ast.Conv:
		// The checker inserts a conversion; emit-java prints the tree the
		// parser built, so seeing one means the printer was handed a checked
		// tree and would be printing a lowering rather than the program.
		p.refuse(d.Pos, "a checker-inserted conversion", "emit-java prints the syntax tree, not a lowered one")
		return p.expr(d.X), precPrim
	default:
		p.refuse(source.Pos{}, fmt.Sprintf("the expression %T", e), "the printer does not know this expression")
		return "/* unknown */", precPrim
	}
}

func (p *printer) literal(d *ast.Literal) string {
	switch d.Kind {
	case ast.LitInt:
		return strconv.FormatInt(int64(int32(uint32(d.Int))), 10)
	case ast.LitLong:
		return strconv.FormatInt(int64(d.Int), 10) + "L"
	case ast.LitFloat:
		return floatText(d.Flt, 32) + "f"
	case ast.LitDouble:
		return floatText(d.Flt, 64)
	case ast.LitChar:
		return "'" + escapeRune(rune(uint32(d.Int)), '\'') + "'"
	case ast.LitString:
		return quote(d.Str)
	case ast.LitBool:
		if d.Bool {
			return "true"
		}
		return "false"
	case ast.LitNull:
		return "null"
	}
	return "null"
}

// floatText prints a floating-point literal so that javac reads back the same
// value: the shortest round-tripping form, with a fraction or an exponent so
// that it is a floating-point literal at all.
func floatText(f float64, bits int) string {
	s := strconv.FormatFloat(f, 'g', -1, bits)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// quote prints a string literal so that javac reads back the same code units.
// The bytes are WTF-8, so the walk is over sequences and not over code points:
// a lone surrogate -- "\uD800" on its own, which is a string Java holds -- is
// its three-byte form here and has no UTF-8 spelling at all, so ranging over the
// string the way Go does would answer U+FFFD for it and print a program that
// says something else.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := wtf8Rune(s[i:])
		b.WriteString(escapeRune(r, '"'))
		i += n
	}
	b.WriteByte('"')
	return b.String()
}

// wtf8Rune decodes one WTF-8 sequence: the code point it spells, which for a
// surrogate standing alone is the surrogate itself, and how many bytes it took.
// A byte that cannot begin a sequence is answered as itself, one byte wide,
// which is as far as a string this repository builds can be malformed.
func wtf8Rune(s string) (rune, int) {
	b := s[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b&0xE0 == 0xC0 && len(s) >= 2:
		return rune(b&0x1F)<<6 | rune(s[1]&0x3F), 2
	case b&0xF0 == 0xE0 && len(s) >= 3:
		return rune(b&0x0F)<<12 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), 3
	case b&0xF8 == 0xF0 && len(s) >= 4:
		return rune(b&0x07)<<18 | rune(s[1]&0x3F)<<12 | rune(s[2]&0x3F)<<6 | rune(s[3]&0x3F), 4
	}
	return rune(b), 1
}

// escapeRune prints one character so that Java reads the same code unit back: a
// control character becomes an escape, and everything else is written as
// itself, which is what keeps CJK and emoji readable in the output.
func escapeRune(r rune, quote rune) string {
	switch {
	case r == '\\':
		return "\\\\"
	case r == quote:
		return "\\" + string(quote)
	case r == '\n':
		return "\\n"
	case r == '\r':
		return "\\r"
	case r == '\t':
		return "\\t"
	case r == '\b':
		return "\\b"
	case r == '\f':
		return "\\f"
	// A surrogate has no UTF-8 spelling, so it is written as the escape that
	// names the code unit -- and Go's own conversion of such a rune would say
	// U+FFFD, which is a different string.
	case r >= 0xD800 && r <= 0xDFFF:
		return fmt.Sprintf("\\u%04x", r)
	case r < 0x20 || r == 0x7f:
		return fmt.Sprintf("\\u%04x", r)
	}
	return string(r)
}

func (p *printer) call(d *ast.Call) string {
	args := make([]string, 0, len(d.Args))
	for _, a := range d.Args {
		args = append(args, p.expr(a))
	}
	recv := ""
	switch {
	case d.ThisCtor && d.Super:
		// super(...) is a constructor call in a constructor, not a method call
		return "super(" + strings.Join(args, ", ") + ")"
	case d.ThisCtor:
		return "this(" + strings.Join(args, ", ") + ")"
	case d.Super && d.Qual != "":
		recv = d.Qual + ".super."
	case d.Super:
		recv = "super."
	case d.Recv != nil:
		recv = p.exprAt(d.Recv, precPrim) + "."
	}
	// A method's explicit type arguments go between the receiver and the name:
	// Teyru writes `items.<T>head()`, which is also Java's spelling, and
	// `items.head<T>()` is not Java at all.
	typeArgs := ""
	if len(d.TypeArgs) > 0 {
		typeArgs = "<" + strings.Join(p.typeList(d.TypeArgs), ", ") + ">"
	}
	return recv + typeArgs + d.Name + "(" + strings.Join(args, ", ") + ")"
}

func (p *printer) newExpr(d *ast.New) string {
	args := make([]string, 0, len(d.Args))
	for _, a := range d.Args {
		args = append(args, p.expr(a))
	}
	// `o.new Inner()` names the enclosing instance Java's own syntax wants,
	// and dropping it would leave the nested class with no outer instance.
	qual := ""
	if d.Outer != nil {
		qual = p.exprAt(d.Outer, precPrim) + "."
	}
	/* new BufferedReader("text") is a Teyru convenience: Java's BufferedReader
	   takes a Reader, so the same program is its BufferedReader over a
	   StringReader, and printing the Teyru form makes javac reject the file
	   ("String cannot be converted to Reader"). The test is the argument's
	   shape rather than its type, because a string literal is a String and
	   nothing else -- which is what the programs using this form write. A
	   String variable would print the Teyru form and be refused, which is a
	   visible failure rather than a wrong answer. */
	if d.Outer == nil && len(d.Args) == 1 && d.Type != nil && d.Type.Name == "BufferedReader" {
		if lit, ok := d.Args[0].(*ast.Literal); ok && lit.Kind == ast.LitString {
			return "new BufferedReader(new java.io.StringReader(" + args[0] + "))"
		}
		/* Any other one-argument form -- a Reader, which Java takes as it
		   stands, or a stream, which it needs wrapped and which this printer
		   cannot tell apart because the type of an argument is not in the tree
		   it walks -- is refused rather than printed wrong. The differential
		   counts a refused program as not translated, which is a stated gap,
		   and the program still runs in the suite. */
		if !isReaderish(d.Args[0]) {
			p.refuse(d.Type.Pos, "new BufferedReader(<stream>)", "Java's BufferedReader takes a Reader and this printer cannot see the argument's type")
		}
	}
	head := qual + "new " + p.typeExpr(d.Type) + "(" + strings.Join(args, ", ") + ")"
	if d.Body == nil {
		return head
	}
	// An anonymous class: its members are the body, and they are printed where
	// the expression is, so the enclosing line breaks here.
	body := p.capture(func() {
		p.line("")
		p.indent++
		for _, m := range d.Body.Members {
			p.member(m)
		}
		p.indent--
	})
	return head + " {" + body + strings.Repeat("  ", p.indent) + "}"
}

func (p *printer) newArray(d *ast.NewArray) string {
	elems := ""
	if d.Init != nil {
		parts := make([]string, 0, len(d.Init.Elems))
		for _, x := range d.Init.Elems {
			parts = append(parts, p.expr(x))
		}
		elems = " {" + strings.Join(parts, ", ") + "}"
	}
	dims := make([]string, 0, len(d.Dims)+d.Extra)
	for _, x := range d.Dims {
		dims = append(dims, p.expr(x))
	}
	for i := 0; i < d.Extra; i++ {
		dims = append(dims, "")
	}
	return "new " + p.typeExpr(d.Elem) + "[" + strings.Join(dims, "][") + "]" + elems
}

func (p *printer) unary(d *ast.Unary) (string, int) {
	if d.Postfix {
		return p.exprAt(d.X, precPost) + d.Op, precPost
	}
	if d.Op == "++" || d.Op == "--" {
		return d.Op + p.exprAt(d.X, precUnary), precUnary
	}
	text := p.exprAt(d.X, precUnary)
	// `- -x` is not `--x`: a space keeps two signs from becoming one operator.
	if (d.Op == "-" || d.Op == "+") && strings.HasPrefix(text, d.Op) {
		return d.Op + " " + text, precUnary
	}
	return d.Op + text, precUnary
}

func (p *printer) binary(d *ast.Binary) (string, int) {
	prec, ok := map[string]int{
		"||": precOrOr, "&&": precAndAnd,
		"|": precBitOr, "^": precBitXor, "&": precBitAnd,
		"==": precEq, "!=": precEq,
		"<": precRel, ">": precRel, "<=": precRel, ">=": precRel,
		"<<": precShift, ">>": precShift, ">>>": precShift,
		"+": precAdd, "-": precAdd,
		"*": precMul, "/": precMul, "%": precMul,
	}[d.Op]
	if !ok {
		p.refuse(d.Pos, "the operator "+d.Op, "the printer does not know it")
		prec = precMul
	}
	left, right := p.exprAt(d.X, prec), p.exprAt(d.Y, prec+1)
	// Two class literals are incomparable in Java unless one side is widened:
	// `Point.class == Integer.class` compares Class<Point> with Class<Integer>,
	// which javac rejects even though both are Class objects at run time.
	if d.Op == "==" || d.Op == "!=" {
		_, lx := d.X.(*ast.ClassLit)
		_, rx := d.Y.(*ast.ClassLit)
		if lx && rx {
			left, right = "(Class<?>) "+left, "(Class<?>) "+right
		}
	}
	// Left-associative: the right operand needs parentheses to keep the shape
	// the tree has, and the left one does not.
	return left + " " + d.Op + " " + right, prec
}

func (p *printer) lambda(d *ast.Lambda) string {
	head := ""
	if len(d.Params) == 1 && d.Params[0].Type == nil && !d.Params[0].Unnamed {
		head = d.Params[0].Name
	} else {
		head = "(" + p.params(d.Params) + ")"
	}
	body, ok := d.Body.(*ast.Block)
	if !ok {
		return head + " -> " + p.exprAt(d.Body.(ast.Expr), 0)
	}
	text := p.capture(func() {
		p.line("")
		p.indent++
		for _, s := range body.Stmts {
			p.stmt(s)
		}
		p.indent--
	})
	return head + " -> {" + text + strings.Repeat("  ", p.indent) + "}"
}

func (p *printer) methodRef(d *ast.MethodRef) string {
	head := ""
	switch {
	case d.TypeX != nil:
		head = p.typeExpr(d.TypeX)
	case d.X != nil:
		head = p.exprAt(d.X, precPrim)
	}
	return head + "::" + d.Name
}
