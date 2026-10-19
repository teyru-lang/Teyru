package lsp

import (
	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/lexer"
	"github.com/teyru-lang/Teyru/internal/source"
)

// target is a declaration a reference resolves to: its name's position and
// length, both in the file the reference was written in.
type target struct {
	pos    source.Pos
	length int
}

// resolver walks one file's syntax tree and records, for every reference, the
// declaration it resolved to, and, for every declaration, itself. The
// references are the ones semantic analysis already resolved (`Ident.Ref`,
// `Select.Ref`, `Call.Method`, `TypeExpr.Resolved`); this file only has to
// read them at the right offsets.
type resolver struct {
	f     *ast.File
	toks  []lexer.Token
	refs  map[int]target
	decls map[int]target
}

// resolveDefinition answers the declaration of the identifier at off, or
// false when the position is not on an identifier or the identifier does not
// resolve to a declaration in this file.
func resolveDefinition(f *ast.File, toks []lexer.Token, off int) (target, bool) {
	tok, ok := tokenAt(toks, off)
	if !ok || tok.Kind != lexer.Ident {
		return target{}, false
	}
	if f == nil || f.Src == nil {
		return target{}, false
	}
	r := &resolver{
		f:     f,
		toks:  toks,
		refs:  map[int]target{},
		decls: map[int]target{},
	}
	r.file(f)
	if t, ok := r.refs[tok.Off]; ok {
		return t, true
	}
	if t, ok := r.decls[tok.Off]; ok {
		return t, true
	}
	return target{}, false
}

// tokenAt returns the token whose extent contains off. A position exactly at
// the end of the last token still names that token, which is what a cursor
// placed after a word means.
func tokenAt(toks []lexer.Token, off int) (lexer.Token, bool) {
	for _, t := range toks {
		if t.Kind == lexer.EOF {
			break
		}
		if off >= t.Off && off < t.End {
			return t, true
		}
	}
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].Kind == lexer.EOF {
			continue
		}
		if toks[i].End == off {
			return toks[i], true
		}
		break
	}
	return lexer.Token{}, false
}

func (r *resolver) file(f *ast.File) {
	for _, cd := range f.Types {
		r.class(cd)
	}
}

func (r *resolver) class(cd *ast.ClassDecl) {
	if cd == nil {
		return
	}
	if off, ok := r.identFrom(cd.Pos.Off, cd.Name); ok && cd.Name != "" {
		r.declare(off, cd.Name)
	}
	for _, tp := range cd.TypeParams {
		r.typeParam(tp)
	}
	for _, te := range cd.Extends {
		r.typeExpr(te)
	}
	for _, te := range cd.Implements {
		r.typeExpr(te)
	}
	for _, te := range cd.Permits {
		r.typeExpr(te)
	}
	for _, rc := range cd.RecordComps {
		r.param(rc)
	}
	for _, ec := range cd.EnumConsts {
		r.enumConst(ec)
	}
	for _, m := range cd.Members {
		r.member(m)
	}
}

func (r *resolver) member(m ast.Member) {
	switch v := m.(type) {
	case *ast.FieldDecl:
		r.typeExpr(v.Type)
		for _, vd := range v.Vars {
			r.declare(vd.Pos.Off, vd.Name)
			r.expr(vd.Init)
		}
		for _, acc := range v.Accessor {
			r.block(acc.Body)
		}
	case *ast.MethodDecl:
		for _, tp := range v.TypeParams {
			r.typeParam(tp)
		}
		r.typeExpr(v.Result)
		for _, p := range v.Params {
			r.param(p)
		}
		for _, te := range v.Throws {
			r.typeExpr(te)
		}
		name := v.Name
		if name == "<init>" || v.IsCtor {
			// The source spells a constructor with the class's name; the
			// syntax tree records the constructor as `<init>`.
			if c, ok := r.enclosingClass(v); ok {
				name = c.Name
			}
		}
		if name != "<init>" {
			r.declare(v.Pos.Off, name)
		}
		r.expr(v.Default)
		r.block(v.Body)
	case *ast.InitBlock:
		r.block(v.Body)
	case *ast.ClassDecl:
		r.class(v)
	}
}

// enclosingClass finds the class whose members include m, for the source name
// of a constructor.
func (r *resolver) enclosingClass(m *ast.MethodDecl) (*ast.ClassDecl, bool) {
	var found *ast.ClassDecl
	var walk func(cd *ast.ClassDecl) bool
	walk = func(cd *ast.ClassDecl) bool {
		if cd == nil {
			return false
		}
		for _, member := range cd.Members {
			if member == m {
				found = cd
				return true
			}
		}
		for _, member := range cd.Members {
			if nested, ok := member.(*ast.ClassDecl); ok && walk(nested) {
				return true
			}
		}
		return false
	}
	for _, cd := range r.f.Types {
		if walk(cd) {
			return found, true
		}
	}
	return nil, false
}

func (r *resolver) typeParam(tp *ast.TypeParam) {
	if tp == nil {
		return
	}
	r.declare(tp.Pos.Off, tp.Name)
	for _, b := range tp.Bounds {
		r.typeExpr(b)
	}
}

func (r *resolver) param(p *ast.Param) {
	if p == nil {
		return
	}
	r.typeExpr(p.Type)
	if !p.Unnamed {
		r.declare(p.Pos.Off, p.Name)
	}
	for _, d := range p.Decomp {
		r.param(d)
	}
}

func (r *resolver) enumConst(ec *ast.EnumConst) {
	if ec == nil {
		return
	}
	r.declare(ec.Pos.Off, ec.Name)
	for _, a := range ec.Args {
		r.expr(a)
	}
	r.class(ec.Body)
}

func (r *resolver) typeExpr(te *ast.TypeExpr) {
	if te == nil {
		return
	}
	if t, ok := r.typeTarget(te.Resolved); ok {
		for _, off := range r.typeNameOffsets(te) {
			r.refs[off] = t
		}
	}
	for _, a := range te.Args {
		r.typeExpr(a)
	}
	r.typeExpr(te.Bound)
}

// typeNameOffsets returns the offsets of the identifiers that spell the type's
// name: the first segment and, for a qualified name, the last one.
func (r *resolver) typeNameOffsets(te *ast.TypeExpr) []int {
	offs := []int{te.Pos.Off}
	start := -1
	for i, t := range r.toks {
		if t.Off == te.Pos.Off {
			start = i
			break
		}
	}
	if start < 0 {
		return offs
	}
	last := start
	for j := start; j+2 < len(r.toks); j += 2 {
		if r.toks[j].Kind != lexer.Ident || r.toks[j+1].Text != "." || r.toks[j+2].Kind != lexer.Ident {
			break
		}
		last = j + 2
	}
	if last != start {
		offs = append(offs, r.toks[last].Off)
	}
	return offs
}

func (r *resolver) expr(e ast.Expr) {
	switch v := e.(type) {
	case nil, *ast.Literal, *ast.This, *ast.SuperExpr:
	case *ast.Ident:
		if t, ok := r.refTarget(v.Ref); ok {
			r.refs[v.Pos.Off] = t
		}
	case *ast.Select:
		if t, ok := r.refTarget(v.Ref); ok {
			if off, ok := r.nameAfter(v.Pos.Off, v.Name); ok {
				r.refs[off] = t
			}
		}
		r.expr(v.X)
	case *ast.Index:
		r.expr(v.X)
		r.expr(v.Index)
	case *ast.Call:
		if t, ok := r.refTarget(v.Method); ok {
			off := v.Pos.Off
			if v.Recv != nil {
				if o, ok := r.nameAfter(v.Pos.Off, v.Name); ok {
					off = o
				}
			}
			r.refs[off] = t
		}
		r.expr(v.Recv)
		for _, a := range v.TypeArgs {
			r.typeExpr(a)
		}
		for _, a := range v.Args {
			r.expr(a)
		}
	case *ast.New:
		r.typeExpr(v.Type)
		for _, a := range v.Args {
			r.expr(a)
		}
		r.class(v.Body)
		r.expr(v.Outer)
	case *ast.NewArray:
		r.typeExpr(v.Elem)
		for _, d := range v.Dims {
			r.expr(d)
		}
		if v.Init != nil {
			r.expr(v.Init)
		}
	case *ast.ArrayInit:
		for _, x := range v.Elems {
			r.expr(x)
		}
	case *ast.Unary:
		r.expr(v.X)
	case *ast.Binary:
		r.expr(v.X)
		r.expr(v.Y)
	case *ast.Assign:
		r.expr(v.X)
		r.expr(v.Y)
	case *ast.Cond:
		r.expr(v.C)
		r.expr(v.X)
		r.expr(v.Y)
	case *ast.Cast:
		r.typeExpr(v.Type)
		r.expr(v.X)
	case *ast.InstanceOf:
		r.expr(v.X)
		r.typeExpr(v.Type)
		r.param(v.Binding)
	case *ast.Lambda:
		for _, p := range v.Params {
			r.param(p)
		}
		switch body := v.Body.(type) {
		case ast.Expr:
			r.expr(body)
		case *ast.Block:
			r.block(body)
		}
	case *ast.MethodRef:
		r.expr(v.X)
		r.typeExpr(v.TypeX)
	case *ast.SwitchExpr:
		if v.S != nil {
			r.switchStmt(v.S)
		}
	case *ast.ClassLit:
		r.typeExpr(v.Type)
	case *ast.Conv:
		r.expr(v.X)
	}
}

func (r *resolver) block(b *ast.Block) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		r.stmt(s)
	}
}

func (r *resolver) stmt(s ast.Stmt) {
	switch v := s.(type) {
	case nil:
	case *ast.Block:
		r.block(v)
	case *ast.LocalVar:
		r.typeExpr(v.Type)
		for _, vd := range v.Vars {
			r.declare(vd.Pos.Off, vd.Name)
			r.expr(vd.Init)
		}
	case *ast.LocalClass:
		r.class(v.Decl)
	case *ast.ExprStmt:
		r.expr(v.X)
	case *ast.If:
		r.expr(v.Cond)
		r.stmt(v.Then)
		r.stmt(v.Else)
	case *ast.While:
		r.expr(v.Cond)
		r.stmt(v.Body)
	case *ast.DoWhile:
		r.stmt(v.Body)
		r.expr(v.Cond)
	case *ast.For:
		for _, x := range v.Init {
			r.stmt(x)
		}
		r.expr(v.Cond)
		for _, x := range v.Update {
			r.expr(x)
		}
		r.stmt(v.Body)
	case *ast.ForEach:
		r.param(v.Var)
		r.expr(v.X)
		r.stmt(v.Body)
	case *ast.Return:
		r.expr(v.X)
	case *ast.Throw:
		r.expr(v.X)
	case *ast.Try:
		for _, x := range v.Resources {
			r.stmt(x)
		}
		r.block(v.Body)
		for _, c := range v.Catches {
			for _, te := range c.Types {
				r.typeExpr(te)
			}
			if !c.Unnamed {
				if off, ok := r.identFrom(c.Pos.Off, c.Name); ok {
					r.declare(off, c.Name)
				}
			}
			r.block(c.Body)
		}
		r.block(v.Finally)
	case *ast.Switch:
		r.switchStmt(v)
	case *ast.Yield:
		r.expr(v.X)
	case *ast.Labeled:
		r.stmt(v.Body)
	case *ast.Assert:
		r.expr(v.Cond)
		r.expr(v.Msg)
	case *ast.Sync:
		r.expr(v.Lock)
		r.block(v.Body)
	}
}

func (r *resolver) switchStmt(s *ast.Switch) {
	r.expr(s.X)
	for _, c := range s.Cases {
		for _, l := range c.Labels {
			r.expr(l)
		}
		r.param(c.Pattern)
		r.expr(c.Guard)
		for _, st := range c.Body {
			r.stmt(st)
		}
		r.expr(c.ArrowX)
	}
}

func (r *resolver) declare(off int, name string) {
	if name == "" || name == "<init>" || name == "_" {
		return
	}
	r.decls[off] = target{pos: source.Pos{File: r.f.Src, Off: off}, length: len(name)}
}

// refTarget turns a resolved symbol into a declaration target, refusing one
// that lives in another file: go-to-definition here answers inside the open
// document, and a jump into the standard library would be a guess about a
// file the editor never opened.
func (r *resolver) refTarget(ref any) (target, bool) {
	switch v := ref.(type) {
	case *ast.Var:
		if v == nil || v.Pos.File != r.f.Src || v.Name == "" {
			return target{}, false
		}
		return target{pos: v.Pos, length: len(v.Name)}, true
	case *ast.Field:
		if v == nil || v.Pos.File != r.f.Src || v.Name == "" {
			return target{}, false
		}
		return target{pos: v.Pos, length: len(v.Name)}, true
	case *ast.Method:
		if v == nil || v.Pos.File != r.f.Src {
			return target{}, false
		}
		name := v.Name
		if name == "<init>" {
			if v.Owner == nil {
				return target{}, false
			}
			name = v.Owner.Name
		}
		if name == "" {
			return target{}, false
		}
		return target{pos: v.Pos, length: len(name)}, true
	case *ast.Class:
		return r.classTarget(v)
	}
	return target{}, false
}

func (r *resolver) typeTarget(t ast.Type) (target, bool) {
	switch v := t.(type) {
	case *ast.ClassType:
		if v == nil {
			return target{}, false
		}
		return r.classTarget(v.Class)
	case *ast.ArrayType:
		if v == nil {
			return target{}, false
		}
		return r.typeTarget(v.Elem)
	case *ast.WildcardType:
		if v == nil || v.Bound == nil {
			return target{}, false
		}
		return r.typeTarget(v.Bound)
	}
	return target{}, false
}

func (r *resolver) classTarget(c *ast.Class) (target, bool) {
	if c == nil || c.Decl == nil || c.Name == "" || c.Decl.Pos.File != r.f.Src {
		return target{}, false
	}
	off, ok := r.identFrom(c.Decl.Pos.Off, c.Name)
	if !ok {
		return target{}, false
	}
	return target{pos: source.Pos{File: r.f.Src, Off: off}, length: len(c.Name)}, true
}

// identFrom finds the first identifier called name at or after off. A class
// declaration's position is the `class` keyword (or `@` of `@interface`), so
// the name is found by searching forward from it.
func (r *resolver) identFrom(from int, name string) (int, bool) {
	for _, t := range r.toks {
		if t.Off < from {
			continue
		}
		if t.Kind == lexer.Ident && t.Text == name {
			return t.Off, true
		}
		if t.Kind == lexer.EOF {
			break
		}
	}
	return 0, false
}

// nameAfter finds the identifier that names a member in `x.name`: the scan
// starts at the dot and skips a type-argument list, as in `x.<T>name(...)`.
func (r *resolver) nameAfter(from int, name string) (int, bool) {
	depth := 0
	for _, t := range r.toks {
		if t.Off < from {
			continue
		}
		switch t.Text {
		case "<":
			depth++
			continue
		case ">":
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth > 0 {
			continue
		}
		if t.Kind == lexer.Ident && t.Text == name {
			return t.Off, true
		}
		if t.Kind == lexer.EOF {
			break
		}
	}
	return 0, false
}
