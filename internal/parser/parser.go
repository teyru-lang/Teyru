// Package parser builds a Teyru AST from tokens.
package parser

import (
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/lexer"
	"github.com/teyru-lang/Teyru/internal/source"
)

// Parse parses one source file.
func Parse(f *source.File, diags *source.Diagnostics) *ast.File {
	p := &parser{f: f, diags: diags}
	p.toks = lexer.Lex(f, diags)
	p.nl = []bool{true}
	return p.parseFile()
}

type parser struct {
	f      *source.File
	toks   []lexer.Token
	i      int
	diags  *source.Diagnostics
	nl     []bool // stack: are newlines significant?
	spec   int    // >0 while speculating (errors suppressed)
	failed bool
}

// ---------------------------------------------------------------- helpers

func (p *parser) tok() lexer.Token { return p.toks[p.i] }
func (p *parser) peekN(n int) lexer.Token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) pos() source.Pos { return source.Pos{File: p.f, Off: p.tok().Off} }

func (p *parser) is(text string) bool {
	t := p.tok()
	return (t.Kind == lexer.Op || t.Kind == lexer.Keyword) && t.Text == text
}

func (p *parser) isAt(n int, text string) bool {
	t := p.peekN(n)
	return (t.Kind == lexer.Op || t.Kind == lexer.Keyword) && t.Text == text
}

func (p *parser) isIdent(text string) bool {
	t := p.tok()
	return t.Kind == lexer.Ident && t.Text == text
}

func (p *parser) next() lexer.Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) accept(text string) bool {
	if p.is(text) {
		p.next()
		return true
	}
	return false
}

func (p *parser) errf(pos source.Pos, code, format string, args ...any) {
	if p.spec > 0 {
		p.failed = true
		return
	}
	// avoid duplicate errors at the same offset
	if n := len(p.diags.List); n > 0 && p.diags.List[n-1].Pos.Off == pos.Off && p.diags.List[n-1].Pos.File == pos.File {
		return
	}
	p.diags.Errorf(pos, code, format, args...)
}

func (p *parser) expect(text string) source.Pos {
	pos := p.pos()
	if !p.accept(text) {
		p.errf(pos, "TY-SYN-0100", "expected '%s', found %s", text, describe(p.tok()))
	}
	return pos
}

func describe(t lexer.Token) string {
	switch t.Kind {
	case lexer.EOF:
		return "end of file"
	case lexer.StringLit:
		return "string literal"
	case lexer.CharLit:
		return "character literal"
	case lexer.IntLit, lexer.LongLit, lexer.FloatLit, lexer.DoubleLit:
		return "number '" + t.Text + "'"
	}
	return "'" + t.Text + "'"
}

func (p *parser) ident() string {
	t := p.tok()
	if t.Kind == lexer.Ident {
		p.next()
		return t.Text
	}
	p.errf(p.pos(), "TY-SYN-0101", "expected identifier, found %s", describe(t))
	if t.Kind != lexer.EOF && !p.is("}") {
		p.next()
	}
	return "<error>"
}

func (p *parser) pushNL(sig bool) { p.nl = append(p.nl, sig) }
func (p *parser) popNL()          { p.nl = p.nl[:len(p.nl)-1] }
func (p *parser) nlSig() bool     { return p.nl[len(p.nl)-1] }

// lineBreak reports whether a significant line break precedes the current token.
func (p *parser) lineBreak() bool { return p.nlSig() && p.tok().NL }

// terminator checks that a statement/declaration ends here.
func (p *parser) terminator() {
	// A statement ends at a line break, at a ';', or at the '}' that closes the
	// body it is the last statement of. Both spellings are accepted, and the
	// lexer already flags the token after a ';' as starting a new line, so a
	// run of them is consumed here rather than reported as an empty statement.
	for p.accept(";") {
	}
	t := p.tok()
	if t.NL || t.Kind == lexer.EOF || p.is("}") {
		return
	}
	p.errf(p.pos(), "TY-SYN-0003", "expected end of line, found %s", describe(t))
	// recover: skip to next line
	for {
		t := p.tok()
		if t.NL || t.Kind == lexer.EOF || p.is("}") {
			return
		}
		p.next()
	}
}

type mark struct {
	i  int
	nd int
}

func (p *parser) save() mark { return mark{p.i, len(p.nl)} }
func (p *parser) restore(m mark) {
	p.i = m.i
	p.nl = p.nl[:m.nd]
}

// speculate runs fn without reporting errors; it reports success and rewinds on failure.
func (p *parser) speculate(fn func() bool) bool {
	m := p.save()
	p.spec++
	oldFailed := p.failed
	p.failed = false
	ok := fn() && !p.failed
	p.spec--
	p.failed = oldFailed
	if !ok {
		p.restore(m)
	}
	return ok
}

// ---------------------------------------------------------------- file

func (p *parser) parseFile() *ast.File {
	file := &ast.File{Src: p.f}
	// Annotations are NOT skipped here. In Java a compilation unit's annotations
	// belong to the declaration that follows, and a file that starts with one
	// (`@Data` on the first line, the shape every README uses) was losing it:
	// the type declaration loop below parses them and attaches them to the type.
	if p.is("package") {
		p.next()
		file.Package = p.qualifiedName()
		file.DeclaredPackage = file.Package
		p.terminator()
	}
	for p.is("import") {
		pos := p.pos()
		p.next()
		if p.isIdent("module") {
			// module import declaration (JEP 511): parsed and ignored, there
			// is no module system at run time.
			p.next()
			p.qualifiedName()
			for p.accept(".") && p.is("*") {
				p.next()
			}
			p.terminator()
			continue
		}
		imp := &ast.Import{Pos: pos}
		if p.accept("static") {
			imp.Static = true
		}
		// An import path is a dotted name (java.util.List) or a module path
		// (example.com/dep/pkg): both spellings name the same thing to the
		// resolver, so a slash is read as another separator and the two join
		// the same way. A segment may carry a dash, which module paths use
		// (example.com/my-module/pkg) and an identifier cannot.
		var b strings.Builder
		b.WriteString(p.ident())
		for {
			if p.accept(".") {
				if p.accept("*") {
					imp.Star = true
					break
				}
				b.WriteByte('.')
				b.WriteString(p.pathSegment())
				continue
			}
			if p.accept("/") {
				b.WriteByte('.')
				b.WriteString(p.pathSegment())
				continue
			}
			break
		}
		imp.Path = b.String()
		file.Imports = append(file.Imports, imp)
		p.terminator()
	}
	var implicit *ast.ClassDecl
	for p.tok().Kind != lexer.EOF {
		start := p.i
		if p.accept(";") {
			// a lone ';' between declarations is Java's empty declaration
			continue
		}
		pos := p.pos()
		annos := p.parseAnnotations()
		mods := p.parseModifiers()
		if p.isTypeDeclStart() {
			cd := p.parseTypeDecl(mods, annos)
			file.Types = append(file.Types, cd)
		} else {
			if implicit == nil {
				name := p.f.Path
				if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
					name = name[i+1:]
				}
				// A `.java` file names the implicit class the same way: it is
				// the file name, and both extensions are source spellings.
				name = strings.TrimSuffix(strings.TrimSuffix(name, ".teyru"), ".java")
				implicit = &ast.ClassDecl{Pos: pos, Kind: ast.KindClass, Name: sanitizeName(name), Implicit: true, Mods: ast.ModFinal}
				file.Types = append(file.Types, implicit)
			}
			m := p.parseMemberAfterMods(implicit, pos, mods, annos)
			if m != nil {
				implicit.Members = append(implicit.Members, m)
			}
		}
		if p.i == start {
			p.errf(p.pos(), "TY-SYN-0102", "unexpected %s at top level", describe(p.tok()))
			p.next()
		}
	}
	return file
}

func sanitizeName(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || (i > 0 && r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "Main"
	}
	return b.String()
}

func (p *parser) qualifiedName() string {
	parts := []string{p.ident()}
	for p.is(".") && p.peekN(1).Kind == lexer.Ident {
		p.next()
		parts = append(parts, p.ident())
	}
	return strings.Join(parts, ".")
}

func (p *parser) isTypeDeclStart() bool {
	switch {
	case p.is("class"), p.is("interface"), p.is("enum"):
		return true
	case p.is("@") && p.isAt(1, "interface"):
		return true
	case p.isIdent("record") && p.peekN(1).Kind == lexer.Ident && (p.isAt(2, "(") || p.isAt(2, "<")):
		return true
	}
	return false
}

func (p *parser) parseAnnotations() []*ast.Annotation {
	var out []*ast.Annotation
	for p.is("@") && !p.isAt(1, "interface") {
		pos := p.pos()
		p.next()
		name := p.qualifiedName()
		an := &ast.Annotation{Pos: pos, Name: name}
		if p.is("(") && !p.tok().NL {
			p.next()
			p.pushNL(false)
			for !p.is(")") && p.tok().Kind != lexer.EOF {
				arg := &ast.AnnoArg{}
				if p.tok().Kind == lexer.Ident && p.isAt(1, "=") {
					arg.Name = p.ident()
					p.next() // '='
				}
				arg.Value, arg.Anno = p.parseAnnoValue()
				an.Args = append(an.Args, arg)
				if !p.accept(",") {
					break
				}
			}
			p.popNL()
			p.expect(")")
		}
		out = append(out, an)
	}
	return out
}

// parseAnnoValue parses the restricted expression grammar of annotation
// arguments: literals, class literals, enum constants, arrays and nested
// annotations. Anything else is skipped so a stray expression cannot derail
// the parse of the declaration that follows.
func (p *parser) parseAnnoValue() (ast.Expr, *ast.Annotation) {
	if p.is("@") {
		pos := p.pos()
		p.next()
		name := p.qualifiedName()
		an := &ast.Annotation{Pos: pos, Name: name}
		if p.is("(") {
			p.next()
			p.pushNL(false)
			for !p.is(")") && p.tok().Kind != lexer.EOF {
				arg := &ast.AnnoArg{}
				if p.tok().Kind == lexer.Ident && p.isAt(1, "=") {
					arg.Name = p.ident()
					p.next()
				}
				arg.Value, arg.Anno = p.parseAnnoValue()
				an.Args = append(an.Args, arg)
				if !p.accept(",") {
					break
				}
			}
			p.popNL()
			p.expect(")")
		}
		return nil, an
	}
	pos := p.pos()
	base := ast.ExprBase{Pos: pos}
	switch {
	case p.is("{"):
		ai := &ast.ArrayInit{ExprBase: base}
		p.next()
		p.pushNL(false)
		for !p.is("}") && p.tok().Kind != lexer.EOF {
			v, _ := p.parseAnnoValue()
			ai.Elems = append(ai.Elems, v)
			if !p.accept(",") {
				break
			}
		}
		p.popNL()
		p.expect("}")
		ai.SetType(&ast.ArrayType{Elem: ast.TInt})
		return ai, nil
	case p.is("-"):
		p.next()
		v, _ := p.parseAnnoValue()
		if lit, ok := v.(*ast.Literal); ok {
			if lit.Kind == ast.LitInt || lit.Kind == ast.LitLong {
				lit.Int = -lit.Int
			} else if lit.Kind == ast.LitDouble || lit.Kind == ast.LitFloat {
				lit.Flt = -lit.Flt
			}
			lit.Pos = pos
		}
		return v, nil
	case p.tok().Kind == lexer.Ident && p.isAt(1, ".") && p.isAt(2, "class"):
		name := p.tok().Text
		p.next()
		p.next()
		p.next()
		cl := &ast.ClassLit{ExprBase: base, Type: &ast.TypeExpr{Pos: pos, Name: name}}
		cl.SetType(&ast.ClassType{})
		return cl, nil
	case p.tok().Kind == lexer.IntLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitInt, Int: t.Int}, nil
	case p.tok().Kind == lexer.LongLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitLong, Int: t.Int}, nil
	case p.tok().Kind == lexer.FloatLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitFloat, Flt: t.Flt}, nil
	case p.tok().Kind == lexer.DoubleLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitDouble, Flt: t.Flt}, nil
	case p.tok().Kind == lexer.CharLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitChar, Int: t.Int}, nil
	case p.tok().Kind == lexer.StringLit:
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitString, Str: t.Text}, nil
	case p.is("true") || p.is("false"):
		t := p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitBool, Bool: t.Text == "true"}, nil
	case p.tok().Kind == lexer.Ident:
		x := ast.Expr(&ast.Ident{ExprBase: base, Name: p.ident()})
		for p.is(".") && p.peekN(1).Kind == lexer.Ident {
			p.next()
			x = &ast.Select{ExprBase: ast.ExprBase{Pos: pos}, X: x, Name: p.ident()}
		}
		return x, nil
	case p.tok().Kind == lexer.Keyword && primNames[p.tok().Text] && p.isAt(1, "."):
		name := p.tok().Text
		p.next()
		p.next()
		p.expect("class")
		cl := &ast.ClassLit{ExprBase: base, Type: &ast.TypeExpr{Pos: pos, Name: name}}
		cl.SetType(&ast.ClassType{})
		return cl, nil
	}
	// unknown form: swallow one token so parsing can continue
	if p.tok().Kind != lexer.EOF && !p.is(")") && !p.is("}") {
		p.next()
	}
	return &ast.Literal{ExprBase: base, Kind: ast.LitNull}, nil
}

var modifierBits = map[string]ast.Mods{
	"public": ast.ModPublic, "private": ast.ModPrivate, "protected": ast.ModProtected,
	"static": ast.ModStatic, "final": ast.ModFinal, "abstract": ast.ModAbstract,
	"native": ast.ModNative, "synchronized": ast.ModSynchronized, "transient": ast.ModTransient,
	"volatile": ast.ModVolatile, "strictfp": ast.ModStrictfp,
}

func (p *parser) parseModifiers() ast.Mods {
	var m ast.Mods
	for {
		t := p.tok()
		if bit, ok := modifierBits[t.Text]; ok && t.Kind == lexer.Keyword {
			if p.is("synchronized") && p.isAt(1, "(") {
				return m
			}
			m |= bit
			p.next()
			continue
		}
		if p.is("default") && !p.isAt(1, ":") && !p.isAt(1, "->") {
			m |= ast.ModDefault
			p.next()
			continue
		}
		if t.Kind == lexer.Ident && t.Text == "sealed" && (p.peekN(1).Kind == lexer.Keyword || p.peekN(1).Kind == lexer.Ident) {
			m |= ast.ModSealed
			p.next()
			continue
		}
		if t.Kind == lexer.Ident && t.Text == "non" && p.isAt(1, "-") && p.peekN(2).Text == "sealed" {
			m |= ast.ModNonSealed
			p.next()
			p.next()
			p.next()
			continue
		}
		if p.is("@") && !p.isAt(1, "interface") {
			p.parseAnnotations()
			continue
		}
		return m
	}
}

// ---------------------------------------------------------------- declarations

func (p *parser) parseTypeDecl(mods ast.Mods, annos []*ast.Annotation) *ast.ClassDecl {
	cd := &ast.ClassDecl{Pos: p.pos(), Mods: mods, Annos: annos}
	switch {
	case p.accept("class"):
		cd.Kind = ast.KindClass
	case p.accept("interface"):
		cd.Kind = ast.KindInterface
	case p.accept("enum"):
		cd.Kind = ast.KindEnum
	case p.is("@"):
		p.next()
		p.next()
		cd.Kind = ast.KindAnnotation
	default:
		p.next() // record
		cd.Kind = ast.KindRecord
	}
	cd.Name = p.ident()
	if p.is("<") {
		cd.TypeParams = p.parseTypeParams()
	}
	if cd.Kind == ast.KindRecord {
		p.expect("(")
		p.pushNL(false)
		for !p.is(")") && p.tok().Kind != lexer.EOF {
			cd.RecordComps = append(cd.RecordComps, p.parseParam())
			if !p.accept(",") {
				break
			}
		}
		p.popNL()
		p.expect(")")
	}
	for {
		switch {
		case p.accept("extends"):
			cd.Extends = append(cd.Extends, p.parseType())
			for p.accept(",") {
				cd.Extends = append(cd.Extends, p.parseType())
			}
		case p.accept("implements"):
			cd.Implements = append(cd.Implements, p.parseType())
			for p.accept(",") {
				cd.Implements = append(cd.Implements, p.parseType())
			}
		case p.isIdent("permits"):
			p.next()
			cd.Permits = append(cd.Permits, p.parseType())
			for p.accept(",") {
				cd.Permits = append(cd.Permits, p.parseType())
			}
		default:
			goto body
		}
	}
body:
	p.parseClassBody(cd)
	return cd
}

func (p *parser) parseTypeParams() []*ast.TypeParam {
	var out []*ast.TypeParam
	p.expect("<")
	for {
		p.parseAnnotations()
		tp := &ast.TypeParam{Pos: p.pos(), Name: p.ident()}
		if p.accept("extends") {
			tp.Bounds = append(tp.Bounds, p.parseType())
			for p.accept("&") {
				tp.Bounds = append(tp.Bounds, p.parseType())
			}
		}
		out = append(out, tp)
		if !p.accept(",") {
			break
		}
	}
	p.expect(">")
	return out
}

func (p *parser) parseClassBody(cd *ast.ClassDecl) {
	p.expect("{")
	p.pushNL(true)
	defer func() {
		p.popNL()
		p.expect("}")
	}()
	if cd.Kind == ast.KindEnum {
		p.parseEnumConstants(cd)
	}
	for !p.is("}") && p.tok().Kind != lexer.EOF {
		if p.accept(";") {
			// Java allows ';' in a class body: after the enum constants, after
			// a member, and after the body of an anonymous class
			continue
		}
		start := p.i
		pos := p.pos()
		annos := p.parseAnnotations()
		mods := p.parseModifiers()
		if m := p.parseMemberAfterMods(cd, pos, mods, annos); m != nil {
			cd.Members = append(cd.Members, m)
		}
		if p.i == start {
			p.errf(p.pos(), "TY-SYN-0103", "unexpected %s in class body", describe(p.tok()))
			p.next()
		}
	}
}

func (p *parser) parseEnumConstants(cd *ast.ClassDecl) {
	if p.accept(":") {
		return
	}
	for p.tok().Kind == lexer.Ident {
		p.parseAnnotations()
		ec := &ast.EnumConst{Pos: p.pos(), Name: p.ident()}
		if p.is("(") {
			ec.Args = p.parseArgs()
		}
		if p.is("{") {
			body := &ast.ClassDecl{Pos: p.pos(), Kind: ast.KindClass, Name: "", Mods: ast.ModFinal}
			p.parseClassBody(body)
			ec.Body = body
		}
		cd.EnumConsts = append(cd.EnumConsts, ec)
		if !p.accept(",") {
			break
		}
	}
	// Java writes ';' after the constant list, Teyru writes ':'; the ';' is
	// consumed here, and the class body loop would skip it as an empty
	// declaration if it were not.
	if p.accept(";") {
		return
	}
	if !p.accept(":") && !p.is("}") {
		p.errf(p.pos(), "TY-SYN-0104", "expected ',' ':' or '}' after enum constants")
	}
}

func (p *parser) parseMemberAfterMods(cd *ast.ClassDecl, pos source.Pos, mods ast.Mods, annos []*ast.Annotation) ast.Member {
	if p.isTypeDeclStart() {
		return p.parseTypeDecl(mods, annos)
	}
	if p.is("{") {
		return &ast.InitBlock{Pos: pos, Static: mods.Has(ast.ModStatic), Body: p.parseBlock()}
	}
	var tparams []*ast.TypeParam
	if p.is("<") {
		tparams = p.parseTypeParams()
	}
	// constructor
	if p.tok().Kind == lexer.Ident && p.tok().Text == cd.Name && p.isAt(1, "(") {
		md := &ast.MethodDecl{Pos: p.pos(), Mods: mods, Annos: annos, TypeParams: tparams, IsCtor: true, Name: "<init>"}
		p.next()
		md.Params = p.parseParams()
		if p.accept("throws") {
			md.Throws = p.parseTypeList()
		}
		if p.is("{") {
			md.Body = p.parseBlock()
		} else {
			p.terminator()
		}
		return md
	}
	if cd.Kind == ast.KindRecord && p.tok().Kind == lexer.Ident && p.tok().Text == cd.Name && p.isAt(1, "{") {
		md := &ast.MethodDecl{Pos: p.pos(), Mods: mods, Annos: annos, IsCtor: true, Compact: true, Name: "<init>"}
		p.next()
		md.Body = p.parseBlock()
		return md
	}
	typ := p.parseType()
	namePos := p.pos()
	name := p.ident()
	if p.is("(") {
		md := &ast.MethodDecl{Pos: namePos, Mods: mods, Annos: annos, TypeParams: tparams, Result: typ, Name: name}
		md.Params = p.parseParams()
		for p.accept("[") {
			p.expect("]")
			typ.Dims++
		}
		if p.accept("throws") {
			md.Throws = p.parseTypeList()
		}
		if cd.Kind == ast.KindAnnotation && p.accept("default") {
			md.Default = p.parseExpr()
		}
		if p.is("{") {
			md.Body = p.parseBlock()
		} else {
			p.terminator()
		}
		return md
	}
	fd := &ast.FieldDecl{Pos: pos, Mods: mods, Annos: annos, Type: typ}
	fd.Vars = append(fd.Vars, p.parseDeclaratorRest(namePos, name))
	for p.accept(",") {
		np := p.pos()
		fd.Vars = append(fd.Vars, p.parseDeclaratorRest(np, p.ident()))
	}
	if p.is("{") && p.isAccessorBlock() {
		fd.Accessor = p.parseAccessors()
		return fd
	}
	p.terminator()
	return fd
}

func (p *parser) isAccessorBlock() bool {
	for n := 1; ; n++ {
		t := p.peekN(n)
		if t.Kind == lexer.Keyword && modifierBits[t.Text] != 0 {
			continue
		}
		if t.Kind == lexer.Op && t.Text == "@" {
			return true
		}
		return t.Kind == lexer.Ident && (t.Text == "get" || t.Text == "set")
	}
}

func (p *parser) parseAccessors() []*ast.Accessor {
	var out []*ast.Accessor
	p.expect("{")
	p.pushNL(true)
	for !p.is("}") && p.tok().Kind != lexer.EOF {
		p.parseAnnotations()
		acc := &ast.Accessor{Pos: p.pos(), Mods: p.parseModifiers()}
		switch {
		case p.isIdent("get"):
			p.next()
		case p.isIdent("set"):
			p.next()
			acc.IsSet = true
			acc.ParamName = "value"
			if p.accept("(") {
				acc.ParamName = p.ident()
				p.expect(")")
			}
		default:
			p.errf(p.pos(), "TY-SYN-0105", "expected 'get' or 'set' accessor, found %s", describe(p.tok()))
			p.next()
			continue
		}
		if p.is("{") {
			acc.Body = p.parseBlock()
		} else {
			p.terminator()
		}
		out = append(out, acc)
	}
	p.popNL()
	p.expect("}")
	return out
}

func (p *parser) parseDeclaratorRest(pos source.Pos, name string) *ast.VarDeclarator {
	vd := &ast.VarDeclarator{Pos: pos, Name: name}
	for p.accept("[") {
		p.expect("]")
		vd.Dims++
	}
	if p.accept("=") {
		vd.Init = p.parseVarInit()
	}
	return vd
}

func (p *parser) parseVarInit() ast.Expr {
	if p.is("{") {
		return p.parseArrayInit()
	}
	return p.parseExpr()
}

func (p *parser) parseArrayInit() *ast.ArrayInit {
	ai := &ast.ArrayInit{ExprBase: ast.ExprBase{Pos: p.pos()}}
	p.expect("{")
	p.pushNL(false)
	for !p.is("}") && p.tok().Kind != lexer.EOF {
		ai.Elems = append(ai.Elems, p.parseVarInit())
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect("}")
	return ai
}

func (p *parser) parseTypeList() []*ast.TypeExpr {
	out := []*ast.TypeExpr{p.parseType()}
	for p.accept(",") {
		out = append(out, p.parseType())
	}
	return out
}

func (p *parser) parseParams() []*ast.Param {
	var out []*ast.Param
	p.expect("(")
	p.pushNL(false)
	for !p.is(")") && p.tok().Kind != lexer.EOF {
		out = append(out, p.parseParam())
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect(")")
	return out
}

func (p *parser) parseParam() *ast.Param {
	annos := p.parseAnnotations()
	pr := &ast.Param{Annos: annos, Mods: p.parseModifiers()}
	pr.Type = p.parseType()
	if p.accept("...") {
		pr.Varargs = true
		pr.Type.Dims++
	}
	pr.Pos = p.pos()
	if p.is("this") {
		p.next()
		pr.Name = "this"
	} else {
		pr.Name = p.ident()
	}
	for p.accept("[") {
		p.expect("]")
		pr.Type.Dims++
	}
	return pr
}

// ---------------------------------------------------------------- types

var primNames = map[string]bool{"boolean": true, "byte": true, "short": true, "char": true, "int": true, "long": true, "float": true, "double": true, "void": true}

func (p *parser) parseType() *ast.TypeExpr {
	p.parseAnnotations()
	te := &ast.TypeExpr{Pos: p.pos()}
	t := p.tok()
	if t.Kind == lexer.Keyword && primNames[t.Text] {
		p.next()
		te.Name = t.Text
	} else if p.is("?") {
		p.next()
		te.Name = "?"
		te.Wildcard = 1
		if p.accept("extends") {
			te.Wildcard = 2
			te.Bound = p.parseType()
		} else if p.accept("super") {
			te.Wildcard = 3
			te.Bound = p.parseType()
		}
		return te
	} else {
		var parts []string
		parts = append(parts, p.ident())
		// a `<` that begins a line never starts the type arguments of the
		// reference before it: a newline ends the construct (see continues()).
		if p.is("<") && p.continues() {
			te.Args = p.parseTypeArgs()
		}
		for p.is(".") && p.peekN(1).Kind == lexer.Ident {
			p.next()
			parts = append(parts, p.ident())
			if p.is("<") && p.continues() {
				te.Args = p.parseTypeArgs()
			}
		}
		te.Name = strings.Join(parts, ".")
	}
	for p.is("[") && p.isAt(1, "]") {
		p.next()
		p.next()
		te.Dims++
	}
	return te
}

func (p *parser) parseTypeArgs() []*ast.TypeExpr {
	p.expect("<")
	args := []*ast.TypeExpr{}
	if p.accept(">") {
		return args // diamond
	}
	p.pushNL(false)
	for {
		args = append(args, p.parseType())
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect(">")
	return args
}

// ---------------------------------------------------------------- statements

func (p *parser) parseBlock() *ast.Block {
	b := &ast.Block{Pos: p.pos()}
	p.expect("{")
	p.pushNL(true)
	for !p.is("}") && p.tok().Kind != lexer.EOF {
		start := p.i
		if s := p.parseStatement(); s != nil {
			b.Stmts = append(b.Stmts, s)
		}
		if p.i == start {
			p.errf(p.pos(), "TY-SYN-0106", "unexpected %s", describe(p.tok()))
			p.next()
		}
	}
	p.popNL()
	b.End = p.pos()
	p.expect("}")
	return b
}

func (p *parser) parseStatement() ast.Stmt {
	pos := p.pos()
	t := p.tok()
	switch {
	case p.is(";"):
		// the empty statement of Java (`if (x);`): the statement that does
		// nothing and completes normally
		p.next()
		return &ast.Empty{Pos: pos}
	case p.is("{"):
		return p.parseBlock()
	case p.is("if"):
		p.next()
		s := &ast.If{Pos: pos, Cond: p.parseParenExpr()}
		s.Then = p.parseStatement()
		if p.is("else") {
			p.next()
			s.Else = p.parseStatement()
		}
		return s
	case p.is("while"):
		p.next()
		s := &ast.While{Pos: pos, Cond: p.parseParenExpr()}
		s.Body = p.parseStatement()
		return s
	case p.is("do"):
		p.next()
		s := &ast.DoWhile{Pos: pos, Body: p.parseStatement()}
		p.expect("while")
		s.Cond = p.parseParenExpr()
		p.terminator()
		return s
	case p.is("for"):
		return p.parseFor()
	case p.is("try"):
		return p.parseTry()
	case p.is("switch"):
		s := p.parseSwitch()
		return s
	case p.is("return"):
		p.next()
		s := &ast.Return{Pos: pos}
		if !p.tok().NL && !p.is(";") && !p.is("}") && p.tok().Kind != lexer.EOF {
			s.X = p.parseExpr()
		}
		p.terminator()
		return s
	case p.is("break"), p.is("continue"):
		p.next()
		label := ""
		if p.tok().Kind == lexer.Ident && !p.tok().NL {
			label = p.ident()
		}
		p.terminator()
		if t.Text == "break" {
			return &ast.Break{Pos: pos, Label: label}
		}
		return &ast.Continue{Pos: pos, Label: label}
	case p.is("throw"):
		p.next()
		if p.tok().NL {
			p.errf(p.pos(), "TY-SYN-0003", "throw expression must start on the same line")
		}
		s := &ast.Throw{Pos: pos, X: p.parseExpr()}
		p.terminator()
		return s
	// `yield` is contextual: it starts a statement unless what follows shows
	// the name is being used as a variable or a method
	case p.isIdent("yield") && !p.isAt(1, "=") && !p.isAt(1, "(") && !p.isAt(1, ".") &&
		!p.isAt(1, "[") && !p.isAt(1, "++") && !p.isAt(1, "--") && !p.peekN(1).NL:
		p.next()
		s := &ast.Yield{Pos: pos, X: p.parseExpr()}
		p.terminator()
		return s
	case p.is("synchronized"):
		p.next()
		s := &ast.Sync{Pos: pos, Lock: p.parseParenExpr()}
		s.Body = p.parseBlock()
		return s
	case p.is("assert"):
		p.next()
		s := &ast.Assert{Pos: pos, Cond: p.parseExpr()}
		if p.accept(":") {
			s.Msg = p.parseExpr()
		}
		p.terminator()
		return s
	case t.Kind == lexer.Ident && p.isAt(1, ":") && !p.isAt(1, "::"):
		p.next()
		p.next()
		return &ast.Labeled{Pos: pos, Label: t.Text, Body: p.parseStatement()}
	case p.isTypeDeclStart() || ((p.is("abstract") || p.is("final") || p.is("static")) && p.isLocalClassAhead()):
		mods := p.parseModifiers()
		return &ast.LocalClass{Decl: p.parseTypeDecl(mods, nil)}
	case p.is("@"):
		// An annotation may introduce a local class declaration -- @Helper is
		// the one that matters -- and if it does not, it belongs to a local
		// variable or to something else, which the paths below read.
		if lc := p.tryLocalClass(); lc != nil {
			return lc
		}
	}
	if lv := p.tryLocalVar(true); lv != nil {
		p.terminator()
		return lv
	}
	x := p.parseExpr()
	p.terminator()
	return &ast.ExprStmt{Pos: pos, X: x}
}

// pathSegment reads one segment of an import path: an identifier, optionally
// with dashes in it.
func (p *parser) pathSegment() string {
	s := p.ident()
	for p.is("-") {
		p.next()
		s += "-" + p.ident()
	}
	return s
}

// tryLocalClass reads an annotated local class declaration, or answers nil and
// leaves the tokens where they were.
func (p *parser) tryLocalClass() *ast.LocalClass {
	pos := p.pos()
	ok := p.speculate(func() bool {
		p.parseAnnotations()
		p.parseModifiers()
		return p.is("class") || p.is("interface") || p.is("enum") || p.is("record")
	})
	if !ok {
		return nil
	}
	// re-parse for real so positions and nested errors are reported
	p.restoreTo(pos)
	annos := p.parseAnnotations()
	mods := p.parseModifiers()
	return &ast.LocalClass{Decl: p.parseTypeDecl(mods, annos)}
}

func (p *parser) isLocalClassAhead() bool {
	for n := 0; ; n++ {
		t := p.peekN(n)
		if t.Kind == lexer.Keyword && modifierBits[t.Text] != 0 {
			continue
		}
		return (t.Kind == lexer.Keyword && (t.Text == "class" || t.Text == "interface" || t.Text == "enum")) ||
			(t.Kind == lexer.Ident && t.Text == "record")
	}
}

// tryLocalVar parses `[mods] Type name [= init], ...` when the tokens form one.
func (p *parser) tryLocalVar(allowMulti bool) *ast.LocalVar {
	pos := p.pos()
	var lv *ast.LocalVar
	var annos []*ast.Annotation
	ok := p.speculate(func() bool {
		annos = p.parseAnnotations()
		mods := p.parseModifiers()
		t := p.tok()
		if t.Kind != lexer.Ident && !(t.Kind == lexer.Keyword && primNames[t.Text] && t.Text != "void") {
			return false
		}
		typ := p.parseType()
		if p.tok().Kind != lexer.Ident || p.tok().NL {
			return false
		}
		if !(p.isAt(1, "=") || p.isAt(1, ",") || p.isAt(1, "[") || p.isAt(1, ":") || p.isAt(1, ")") || p.isAt(1, ";") || p.peekN(1).NL || p.isAt(1, "}") || p.peekN(1).Kind == lexer.EOF) {
			return false
		}
		lv = &ast.LocalVar{Pos: pos, Mods: mods, Type: typ, Annos: annos}
		return true
	})
	if !ok {
		return nil
	}
	// re-parse for real (types are cheap) so positions and nested errors are reported
	p.restoreTo(pos)
	lv.Annos = p.parseAnnotations()
	lv.Mods = p.parseModifiers()
	lv.Type = p.parseType()
	np := p.pos()
	lv.Vars = append(lv.Vars, p.parseDeclaratorRest(np, p.ident()))
	for allowMulti && p.accept(",") {
		np := p.pos()
		lv.Vars = append(lv.Vars, p.parseDeclaratorRest(np, p.ident()))
	}
	return lv
}

func (p *parser) restoreTo(pos source.Pos) {
	for p.i > 0 && p.toks[p.i].Off > pos.Off {
		p.i--
	}
}

func (p *parser) parseParenExpr() ast.Expr {
	p.expect("(")
	p.pushNL(false)
	x := p.parseExpr()
	p.popNL()
	p.expect(")")
	return x
}

func (p *parser) parseFor() ast.Stmt {
	pos := p.pos()
	p.next()
	p.expect("(")
	p.pushNL(false)
	// enhanced for?
	var fe *ast.ForEach
	p.speculate(func() bool {
		p.parseAnnotations()
		mods := p.parseModifiers()
		typ := p.parseType()
		if p.tok().Kind != lexer.Ident || !p.isAt(1, ":") {
			return false
		}
		prm := &ast.Param{Pos: p.pos(), Mods: mods, Type: typ, Name: p.ident()}
		p.next() // ':'
		fe = &ast.ForEach{Pos: pos, Var: prm}
		return true
	})
	if fe != nil {
		fe.X = p.parseExpr()
		p.popNL()
		p.expect(")")
		fe.Body = p.parseStatement()
		return fe
	}
	s := &ast.For{Pos: pos}
	if !p.isForSep() {
		if lv := p.tryLocalVar(true); lv != nil {
			s.Init = append(s.Init, lv)
		} else {
			for {
				ep := p.pos()
				s.Init = append(s.Init, &ast.ExprStmt{Pos: ep, X: p.parseExpr()})
				if !p.accept(",") {
					break
				}
			}
		}
	}
	p.forSep()
	if !p.isForSep() {
		s.Cond = p.parseExpr()
	}
	p.forSep()
	if !p.is(")") {
		for {
			s.Update = append(s.Update, p.parseExpr())
			if !p.accept(",") {
				break
			}
		}
	}
	p.popNL()
	p.expect(")")
	s.Body = p.parseStatement()
	return s
}

// isForSep reports whether the classic for header is at one of its separators.
func (p *parser) isForSep() bool { return p.is(":") || p.is(";") }

// forSep consumes the separator between the three parts of a classic for
// header. Teyru spells it ':' and Java spells it ';'; both are accepted
// (decision D6), and the diagnostic for a missing one keeps the older wording.
func (p *parser) forSep() {
	if p.accept(";") {
		return
	}
	p.expect(":")
}

func (p *parser) parseTry() ast.Stmt {
	s := &ast.Try{Pos: p.pos()}
	p.next()
	if p.accept("(") {
		p.pushNL(true)
		for !p.is(")") && p.tok().Kind != lexer.EOF {
			rp := p.pos()
			if lv := p.tryLocalVar(false); lv != nil {
				s.Resources = append(s.Resources, lv)
			} else {
				p.pushNL(false)
				s.Resources = append(s.Resources, &ast.ExprStmt{Pos: rp, X: p.parseExpr()})
				p.popNL()
			}
			if p.accept(";") {
				// Java separates the resources with ';', Teyru with a line
				// break; the ';' is the end of the one just read either way.
				continue
			}
			if !p.is(")") && !p.tok().NL {
				p.errf(p.pos(), "TY-SYN-0107", "try resources must be separated by line breaks")
				break
			}
		}
		p.popNL()
		p.expect(")")
	}
	s.Body = p.parseBlock()
	for p.is("catch") {
		c := &ast.Catch{Pos: p.pos()}
		p.next()
		p.expect("(")
		p.pushNL(false)
		p.parseModifiers()
		c.Types = append(c.Types, p.parseType())
		for p.accept("|") {
			c.Types = append(c.Types, p.parseType())
		}
		c.Name = p.ident()
		p.popNL()
		p.expect(")")
		c.Body = p.parseBlock()
		s.Catches = append(s.Catches, c)
	}
	if p.accept("finally") {
		s.Finally = p.parseBlock()
	}
	if len(s.Catches) == 0 && s.Finally == nil && len(s.Resources) == 0 {
		p.errf(s.Pos, "TY-SYN-0108", "try requires catch or finally")
	}
	return s
}

func (p *parser) parseSwitch() *ast.Switch {
	s := &ast.Switch{Pos: p.pos()}
	p.next()
	s.X = p.parseParenExpr()
	p.expect("{")
	p.pushNL(true)
	first := true
	for !p.is("}") && p.tok().Kind != lexer.EOF {
		c := &ast.Case{Pos: p.pos()}
		if p.accept("default") {
			c.Default = true
		} else if p.accept("case") {
			p.pushNL(false)
			for {
				if p.is("default") {
					p.next()
					c.Default = true
				} else if p.is("null") && (p.isAt(1, "->") || p.isAt(1, ":") || p.isAt(1, ",")) {
					p.next()
					c.Null = true
				} else if pat := p.tryTypePattern(); pat != nil {
					c.Pattern = pat
				} else {
					c.Labels = append(c.Labels, p.parseTernary())
				}
				if !p.accept(",") {
					break
				}
			}
			if p.isIdent("when") {
				p.next()
				c.Guard = p.parseTernary()
			}
			p.popNL()
		} else {
			p.errf(p.pos(), "TY-SYN-0109", "expected 'case' or 'default', found %s", describe(p.tok()))
			p.next()
			continue
		}
		arrow := p.is("->")
		if first {
			s.Arrow = arrow
			first = false
		} else if arrow != s.Arrow {
			p.errf(p.pos(), "TY-SYN-0110", "cannot mix '->' and ':' case labels")
		}
		if arrow {
			p.next()
			c.Arrow = true
			switch {
			case p.is("{"):
				c.Body = []ast.Stmt{p.parseBlock()}
			case p.is("throw"):
				c.Body = []ast.Stmt{p.parseStatement()}
			default:
				c.ArrowX = p.parseExpr()
				p.terminator()
			}
		} else {
			p.expect(":")
			for !p.is("case") && !p.is("default") && !p.is("}") && p.tok().Kind != lexer.EOF {
				start := p.i
				if st := p.parseStatement(); st != nil {
					c.Body = append(c.Body, st)
				}
				if p.i == start {
					p.next()
				}
			}
		}
		s.Cases = append(s.Cases, c)
	}
	p.popNL()
	p.expect("}")
	return s
}

func (p *parser) tryTypePattern() *ast.Param { return p.tryTypePatternOpt(false) }

// tryTypePatternOpt parses a type pattern; inComponent allows `)` to end it
// (record pattern component lists).
func (p *parser) tryTypePatternOpt(inComponent bool) *ast.Param {
	var prm *ast.Param
	p.speculate(func() bool {
		p.parseModifiers()
		t := p.tok()
		if t.Kind != lexer.Ident && !(t.Kind == lexer.Keyword && primNames[t.Text]) {
			return false
		}
		typ := p.parseType()
		if p.is("(") {
			// a record with no components still matches with `case Dot()`
			prm = p.parseRecordComponents(typ)
			return p.patternEnd(inComponent)
		}
		if p.tok().Kind != lexer.Ident || p.isIdent("when") {
			return false
		}
		prm = &ast.Param{Pos: p.pos(), Type: typ, Name: p.ident()}
		if prm.Name == "_" {
			prm.Unnamed = true
		}
		return p.patternEnd(inComponent)
	})
	return prm
}

// patternEnd reports whether the token after a pattern may legitimately follow it.
func (p *parser) patternEnd(inComponent bool) bool {
	if p.is("->") || p.is(":") || p.isIdent("when") || p.is(",") {
		return true
	}
	return inComponent && p.is(")")
}

// parseRecordComponents parses the `(pattern, pattern, ...)` part of a record
// pattern, with typ already parsed as the record type.
func (p *parser) parseRecordComponents(typ *ast.TypeExpr) *ast.Param {
	prm := &ast.Param{Pos: typ.Pos, Type: typ}
	p.expect("(")
	p.pushNL(false)
	for !p.is(")") && p.tok().Kind != lexer.EOF {
		comp := p.tryTypePatternOpt(true)
		if comp == nil {
			if p.tok().Kind != lexer.Ident {
				break
			}
			comp = &ast.Param{Pos: p.pos(), Name: p.ident()}
			if comp.Name == "_" {
				comp.Unnamed = true
			}
		}
		prm.Decomp = append(prm.Decomp, comp)
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect(")")
	return prm
}

// ---------------------------------------------------------------- expressions

func (p *parser) parseExpr() ast.Expr {
	return p.parseAssign()
}

var assignOps = map[string]bool{"=": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true, "&=": true, "|=": true, "^=": true, "<<=": true, ">>=": true, ">>>=": true}

func (p *parser) parseAssign() ast.Expr {
	if lam := p.tryLambda(); lam != nil {
		return lam
	}
	x := p.parseTernary()
	t := p.tok()
	if t.Kind == lexer.Op && assignOps[t.Text] {
		// `>` `>=` produced by lexer for `>>=` is already a single token.
		pos := p.pos()
		p.next()
		y := p.parseAssign()
		return &ast.Assign{ExprBase: ast.ExprBase{Pos: pos}, Op: t.Text, X: x, Y: y}
	}
	return x
}

func (p *parser) tryLambda() ast.Expr {
	pos := p.pos()
	t := p.tok()
	if t.Kind == lexer.Ident && p.isAt(1, "->") {
		p.next()
		p.next()
		lam := &ast.Lambda{ExprBase: ast.ExprBase{Pos: pos}, Params: []*ast.Param{{Pos: pos, Name: t.Text}}}
		lam.Body = p.lambdaBody()
		return lam
	}
	if !p.is("(") {
		return nil
	}
	// find matching paren
	depth := 0
	j := p.i
	for ; j < len(p.toks); j++ {
		tk := p.toks[j]
		if tk.Kind == lexer.Op && tk.Text == "(" {
			depth++
		} else if tk.Kind == lexer.Op && tk.Text == ")" {
			depth--
			if depth == 0 {
				break
			}
		} else if tk.Kind == lexer.EOF {
			return nil
		}
	}
	if j+1 >= len(p.toks) || !(p.toks[j+1].Kind == lexer.Op && p.toks[j+1].Text == "->") {
		return nil
	}
	lam := &ast.Lambda{ExprBase: ast.ExprBase{Pos: pos}}
	p.next()
	p.pushNL(false)
	for !p.is(")") && p.tok().Kind != lexer.EOF {
		if p.tok().Kind == lexer.Ident && (p.isAt(1, ",") || p.isAt(1, ")")) {
			np := p.pos()
			lam.Params = append(lam.Params, &ast.Param{Pos: np, Name: p.ident()})
		} else {
			lam.Params = append(lam.Params, p.parseParam())
		}
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect(")")
	p.expect("->")
	lam.Body = p.lambdaBody()
	return lam
}

func (p *parser) lambdaBody() any {
	if p.is("{") {
		return p.parseBlock()
	}
	return p.parseExpr()
}

func (p *parser) parseTernary() ast.Expr {
	c := p.parseBinary(0)
	if p.is("?") && p.continues() {
		pos := p.pos()
		p.next()
		p.pushNL(false)
		var x ast.Expr
		if lam := p.tryLambda(); lam != nil {
			x = lam
		} else {
			x = p.parseTernary()
		}
		p.popNL()
		p.expect(":")
		var y ast.Expr
		if lam := p.tryLambda(); lam != nil {
			y = lam
		} else {
			y = p.parseTernary()
		}
		return &ast.Cond{ExprBase: ast.ExprBase{Pos: pos}, C: c, X: x, Y: y}
	}
	return c
}

// continues reports whether the current (binary/selector) token continues the
// expression, i.e. it is not separated by a significant line break or it is a
// legal leading continuation operator.
func (p *parser) continues() bool {
	if !p.lineBreak() {
		return true
	}
	t := p.tok()
	if t.Kind != lexer.Op && !(t.Kind == lexer.Keyword && t.Text == "instanceof") {
		return false
	}
	switch t.Text {
	case "+", "-", "++", "--", "(", "[", "!", "~", "{", "@", "<":
		return false
	}
	return true
}

var binPrec = [][]string{
	{"||"},
	{"&&"},
	{"|"},
	{"^"},
	{"&"},
	{"==", "!="},
	{"<", ">", "<=", ">=", "instanceof"},
	{"<<", ">>", ">>>"},
	{"+", "-"},
	{"*", "/", "%"},
}

// binOp returns the binary operator at the cursor (joining `>` `>` into shifts)
// and the number of tokens it spans.
func (p *parser) binOp() (string, int) {
	t := p.tok()
	if t.Kind == lexer.Keyword && t.Text == "instanceof" {
		return "instanceof", 1
	}
	if t.Kind != lexer.Op {
		return "", 0
	}
	if t.Text == ">" {
		t1 := p.peekN(1)
		if t1.Kind == lexer.Op && t1.Off == t.End {
			if t1.Text == ">" {
				t2 := p.peekN(2)
				if t2.Kind == lexer.Op && t2.Text == ">" && t2.Off == t1.End {
					return ">>>", 3
				}
				return ">>", 2
			}
		}
	}
	return t.Text, 1
}

func (p *parser) parseBinary(level int) ast.Expr {
	if level == len(binPrec) {
		return p.parseUnary()
	}
	x := p.parseBinary(level + 1)
	for {
		op, n := p.binOp()
		if op == "" || !contains(binPrec[level], op) || !p.continues() {
			return x
		}
		pos := p.pos()
		for k := 0; k < n; k++ {
			p.next()
		}
		if op == "instanceof" {
			io := &ast.InstanceOf{ExprBase: ast.ExprBase{Pos: pos}, X: x}
			final := p.accept("final")
			io.Type = p.parseType()
			if p.is("(") {
				io.Binding = p.parseRecordComponents(io.Type)
			} else if p.tok().Kind == lexer.Ident && !p.lineBreak() {
				io.Binding = &ast.Param{Pos: p.pos(), Type: io.Type, Name: p.ident()}
				if io.Binding.Name == "_" {
					io.Binding.Unnamed = true
				}
				if final {
					io.Binding.Mods = ast.ModFinal
				}
			}
			x = io
			continue
		}
		y := p.parseBinary(level + 1)
		x = &ast.Binary{ExprBase: ast.ExprBase{Pos: pos}, Op: op, X: x, Y: y}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (p *parser) parseUnary() ast.Expr {
	pos := p.pos()
	t := p.tok()
	if t.Kind == lexer.Op {
		switch t.Text {
		case "+", "-", "!", "~", "++", "--":
			p.next()
			x := p.parseUnary()
			// fold negative literals so -2147483648 works
			if lit, ok := x.(*ast.Literal); ok && t.Text == "-" && (lit.Kind == ast.LitInt || lit.Kind == ast.LitLong) {
				lit.Int = -lit.Int
				lit.Pos = pos
				return lit
			}
			if lit, ok := x.(*ast.Literal); ok && t.Text == "-" && (lit.Kind == ast.LitDouble || lit.Kind == ast.LitFloat) {
				lit.Flt = -lit.Flt
				lit.Pos = pos
				return lit
			}
			return &ast.Unary{ExprBase: ast.ExprBase{Pos: pos}, Op: t.Text, X: x}
		case "(":
			if c := p.tryCast(); c != nil {
				return c
			}
		}
	}
	return p.parsePostfix(p.parsePrimary())
}

func (p *parser) tryCast() ast.Expr {
	pos := p.pos()
	var typ *ast.TypeExpr
	ok := p.speculate(func() bool {
		p.next()
		t := p.tok()
		isPrim := t.Kind == lexer.Keyword && primNames[t.Text]
		if t.Kind != lexer.Ident && !isPrim {
			return false
		}
		typ = p.parseType()
		for p.accept("&") {
			p.parseType()
		}
		if !p.accept(")") {
			return false
		}
		if isPrim && typ.Dims == 0 {
			return true
		}
		// A cast and a parenthesised expression are the same tokens until the
		// token after the `)` is read, and there is no symbol table here to ask
		// whether what is inside the parentheses names a type. A line break
		// settles it instead: an expression ends at the newline, so nothing on
		// the next line can be the operand of a cast. Without this, `a = (b)`
		// followed by another statement was read as the cast `(b) <statement>`.
		if p.lineBreak() {
			return false
		}
		n := p.tok()
		switch n.Kind {
		case lexer.Ident, lexer.IntLit, lexer.LongLit, lexer.FloatLit, lexer.DoubleLit, lexer.CharLit, lexer.StringLit:
			return true
		case lexer.Keyword:
			return n.Text == "this" || n.Text == "new" || n.Text == "super" || n.Text == "true" || n.Text == "false" || n.Text == "null" || n.Text == "switch" || primNames[n.Text]
		case lexer.Op:
			return n.Text == "(" || n.Text == "!" || n.Text == "~"
		}
		return false
	})
	if !ok {
		return nil
	}
	var x ast.Expr
	if lam := p.tryLambda(); lam != nil {
		x = lam
	} else {
		x = p.parseUnary()
	}
	return &ast.Cast{ExprBase: ast.ExprBase{Pos: pos}, Type: typ, X: x}
}

func (p *parser) parseArgs() []ast.Expr {
	p.expect("(")
	p.pushNL(false)
	var args []ast.Expr
	for !p.is(")") && p.tok().Kind != lexer.EOF {
		args = append(args, p.parseExpr())
		if !p.accept(",") {
			break
		}
	}
	p.popNL()
	p.expect(")")
	if args == nil {
		args = []ast.Expr{}
	}
	return args
}

func (p *parser) parsePostfix(x ast.Expr) ast.Expr {
	for {
		pos := p.pos()
		switch {
		case p.is(".") && p.continues():
			p.next()
			var targs []*ast.TypeExpr
			if p.is("<") {
				targs = p.parseTypeArgs()
			}
			switch {
			case p.is("new"):
				// qualified inner class creation: outer.new Inner()
				n := p.parseNew()
				if nn, ok := n.(*ast.New); ok {
					nn.Outer = x
				}
				x = n
				continue
			case p.is("this"):
				p.next()
				x = &ast.This{ExprBase: ast.ExprBase{Pos: pos}, Qual: exprName(x)}
				continue
			case p.is("class"):
				p.next()
				x = &ast.ClassLit{ExprBase: ast.ExprBase{Pos: pos}, Type: &ast.TypeExpr{Pos: x.GetPos(), Name: exprName(x)}}
				continue
			}
			if p.is("super") && !p.isAt(1, "::") {
				// Interface.super.method(...): a qualified super call
				p.next()
				p.expect(".")
				name := p.ident()
				call := &ast.Call{ExprBase: ast.ExprBase{Pos: pos}, Name: name, Super: true, Qual: exprName(x)}
				if p.is("(") {
					call.Args = p.parseArgs()
				}
				x = call
				continue
			}
			name := p.ident()
			if p.is("(") && !p.lineBreak() {
				x = &ast.Call{ExprBase: ast.ExprBase{Pos: pos}, Recv: x, Name: name, TypeArgs: targs, Args: p.parseArgs()}
			} else {
				x = &ast.Select{ExprBase: ast.ExprBase{Pos: pos}, X: x, Name: name}
			}
		case p.is("[") && !p.lineBreak():
			if p.isAt(1, "]") {
				// Type[]::new or Type[].class
				te := &ast.TypeExpr{Pos: x.GetPos(), Name: exprName(x)}
				for p.is("[") && p.isAt(1, "]") {
					p.next()
					p.next()
					te.Dims++
				}
				if p.accept("::") {
					mr := &ast.MethodRef{ExprBase: ast.ExprBase{Pos: pos}, TypeX: te}
					if p.accept("new") {
						mr.Name = "new"
					} else {
						mr.Name = p.ident()
					}
					return mr
				}
				p.expect(".")
				p.expect("class")
				x = &ast.ClassLit{ExprBase: ast.ExprBase{Pos: pos}, Type: te}
				continue
			}
			p.next()
			p.pushNL(false)
			idx := p.parseExpr()
			p.popNL()
			p.expect("]")
			x = &ast.Index{ExprBase: ast.ExprBase{Pos: pos}, X: x, Index: idx}
		case (p.is("++") || p.is("--")) && !p.tok().NL:
			op := p.next().Text
			x = &ast.Unary{ExprBase: ast.ExprBase{Pos: pos}, Op: op, X: x, Postfix: true}
		case p.is("::") && p.continues():
			p.next()
			mr := &ast.MethodRef{ExprBase: ast.ExprBase{Pos: pos}, X: x}
			if p.is("<") {
				p.parseTypeArgs()
			}
			if p.accept("new") {
				mr.Name = "new"
			} else {
				mr.Name = p.ident()
			}
			x = mr
		case p.is("<") && p.continues() && isTypeLike(x) && p.genericTypeRefAhead():
			// Type<Args>::new or Type<Args>::method
			te := &ast.TypeExpr{Pos: x.GetPos(), Name: exprName(x)}
			te.Args = p.parseTypeArgs()
			p.expect("::")
			mr := &ast.MethodRef{ExprBase: ast.ExprBase{Pos: pos}, TypeX: te}
			if p.accept("new") {
				mr.Name = "new"
			} else {
				mr.Name = p.ident()
			}
			x = mr
		default:
			return x
		}
	}
}

func isTypeLike(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.Ident:
		return true
	case *ast.Select:
		return isTypeLike(v.X)
	}
	return false
}

func (p *parser) genericTypeRefAhead() bool {
	m := p.save()
	p.spec++
	old := p.failed
	p.failed = false
	p.parseTypeArgs()
	ok := !p.failed && p.is("::")
	p.failed = old
	p.spec--
	p.restore(m)
	return ok
}

func exprName(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.Select:
		return exprName(v.X) + "." + v.Name
	}
	return "<error>"
}

func (p *parser) parsePrimary() ast.Expr {
	pos := p.pos()
	t := p.tok()
	base := ast.ExprBase{Pos: pos}
	switch t.Kind {
	case lexer.IntLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitInt, Int: t.Int}
	case lexer.LongLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitLong, Int: t.Int}
	case lexer.FloatLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitFloat, Flt: t.Flt}
	case lexer.DoubleLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitDouble, Flt: t.Flt}
	case lexer.CharLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitChar, Int: t.Int}
	case lexer.StringLit:
		p.next()
		return &ast.Literal{ExprBase: base, Kind: ast.LitString, Str: t.Text}
	case lexer.Ident:
		p.next()
		if p.is("(") && !p.lineBreak() {
			return &ast.Call{ExprBase: base, Name: t.Text, Args: p.parseArgs()}
		}
		return &ast.Ident{ExprBase: base, Name: t.Text}
	case lexer.Keyword:
		switch t.Text {
		case "true", "false":
			p.next()
			return &ast.Literal{ExprBase: base, Kind: ast.LitBool, Bool: t.Text == "true"}
		case "null":
			p.next()
			return &ast.Literal{ExprBase: base, Kind: ast.LitNull}
		case "this":
			p.next()
			if p.is("(") && !p.lineBreak() {
				return &ast.Call{ExprBase: base, Name: "<this>", Args: p.parseArgs(), ThisCtor: true}
			}
			return &ast.This{ExprBase: base}
		case "super":
			p.next()
			if p.is("(") && !p.lineBreak() {
				return &ast.Call{ExprBase: base, Name: "<super>", Args: p.parseArgs(), ThisCtor: true, Super: true}
			}
			if p.accept("::") {
				mr := &ast.MethodRef{ExprBase: base, X: &ast.SuperExpr{ExprBase: base}, Name: p.ident()}
				return mr
			}
			p.expect(".")
			name := p.ident()
			if p.is("(") {
				return &ast.Call{ExprBase: base, Recv: &ast.SuperExpr{ExprBase: base}, Name: name, Args: p.parseArgs(), Super: true}
			}
			return &ast.Select{ExprBase: base, X: &ast.SuperExpr{ExprBase: base}, Name: name}
		case "new":
			return p.parseNew()
		case "switch":
			s := p.parseSwitch()
			return &ast.SwitchExpr{ExprBase: base, S: s}
		default:
			if primNames[t.Text] {
				te := p.parseType()
				if p.accept("::") {
					p.expect("new")
					return &ast.MethodRef{ExprBase: base, TypeX: te, Name: "new"}
				}
				p.expect(".")
				p.expect("class")
				return &ast.ClassLit{ExprBase: base, Type: te}
			}
		}
	case lexer.Op:
		if t.Text == "(" {
			p.next()
			p.pushNL(false)
			x := p.parseExpr()
			p.popNL()
			p.expect(")")
			return x
		}
	}
	p.errf(pos, "TY-SYN-0111", "expected expression, found %s", describe(t))
	if t.Kind != lexer.EOF && !p.is("}") && !p.is(")") {
		p.next()
	}
	return &ast.Literal{ExprBase: base, Kind: ast.LitNull}
}

func (p *parser) parseNew() ast.Expr {
	pos := p.pos()
	p.expect("new")
	p.parseAnnotations()
	te := &ast.TypeExpr{Pos: p.pos()}
	t := p.tok()
	if t.Kind == lexer.Keyword && primNames[t.Text] {
		p.next()
		te.Name = t.Text
	} else {
		parts := []string{p.ident()}
		if p.is("<") && p.continues() {
			te.Args = p.parseTypeArgs()
		}
		for p.is(".") {
			p.next()
			parts = append(parts, p.ident())
			if p.is("<") && p.continues() {
				te.Args = p.parseTypeArgs()
			}
		}
		te.Name = strings.Join(parts, ".")
	}
	if p.is("[") {
		na := &ast.NewArray{ExprBase: ast.ExprBase{Pos: pos}, Elem: te}
		for p.is("[") {
			p.next()
			if p.accept("]") {
				na.Extra++
				continue
			}
			if na.Extra > 0 {
				p.errf(p.pos(), "TY-SYN-0112", "array dimension expression after empty dimension")
			}
			p.pushNL(false)
			na.Dims = append(na.Dims, p.parseExpr())
			p.popNL()
			p.expect("]")
		}
		if p.is("{") {
			if len(na.Dims) > 0 {
				p.errf(p.pos(), "TY-SYN-0113", "array creation with both dimensions and initializer")
			}
			na.Init = p.parseArrayInit()
		}
		return na
	}
	n := &ast.New{ExprBase: ast.ExprBase{Pos: pos}, Type: te}
	n.Args = p.parseArgs()
	if p.is("{") && !p.lineBreak() {
		body := &ast.ClassDecl{Pos: p.pos(), Kind: ast.KindClass, Mods: ast.ModFinal}
		p.parseClassBody(body)
		n.Body = body
	}
	return n
}
