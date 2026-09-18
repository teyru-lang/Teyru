package sema

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// This file implements the Lombok compatibility layer. Annotation-driven
// members are generated as ordinary syntax trees and then type-checked like
// user code, so there is no separate lowering path that could drift from the
// language semantics.
//
// Reference: https://projectlombok.org/features/ (all annotations are listed in
// docs/lombok.md, including the ones that cannot work without a runtime the
// Teyru standard library does not ship).

// annoAccessLevel reads the AccessLevel an annotation asks for.
//
// Lombok declares the parameter under three names: `value` on @Getter, @Setter
// and @With, `access` on the three @XxxArgsConstructor annotations and on
// @Builder, and `level` on @FieldDefaults and @FieldNameConstants. An unnamed
// argument is the same parameter written short -- `@Getter(AccessLevel.NONE)`
// is `@Getter(value = AccessLevel.NONE)`.
//
// Reading only the unnamed form meant the spelling Lombok's own documentation
// uses on the constructors, `@NoArgsConstructor(access = AccessLevel.PRIVATE)`,
// looked like an annotation with no access argument at all: the level was
// dropped and a public constructor was generated in its place.
//
// ok is false when no access argument is present, which is not the same as
// AccessLevel.PACKAGE (mods 0, ok true). none reports AccessLevel.NONE, whose
// meaning is "do not generate this member at all".
func annoAccessLevel(a *ast.Annotation, names ...string) (mods ast.Mods, none, ok bool) {
	if a == nil {
		return 0, false, false
	}
	arg := a.Value()
	for _, n := range names {
		if x := a.Arg(n); x != nil {
			arg = x
			break
		}
	}
	if arg == nil {
		return 0, false, false
	}
	name := ""
	switch x := arg.Value.(type) {
	case *ast.Select:
		name = x.Name
	case *ast.Ident:
		name = x.Name
	}
	switch name {
	case "PRIVATE":
		return ast.ModPrivate, false, true
	case "PROTECTED":
		return ast.ModProtected, false, true
	case "PUBLIC":
		return ast.ModPublic, false, true
	case "PACKAGE":
		return 0, false, true
	case "NONE":
		return 0, true, true
	}
	return 0, false, false
}

// annoBool reads a boolean argument.
func annoBool(a *ast.Annotation, name string, def bool) bool {
	if a == nil {
		return def
	}
	arg := a.Arg(name)
	if arg == nil {
		return def
	}
	if lit, ok := arg.Value.(*ast.Literal); ok && lit.Kind == ast.LitBool {
		return lit.Bool
	}
	return def
}

// annoString reads a string argument.
func annoString(a *ast.Annotation, name string) string {
	if a == nil {
		return ""
	}
	arg := a.Arg(name)
	if arg == nil {
		return ""
	}
	if lit, ok := arg.Value.(*ast.Literal); ok && lit.Kind == ast.LitString {
		return lit.Str
	}
	return ""
}

// annoValueString reads the value-form argument of an annotation, as in
// @Singular("fruit").
func annoValueString(a *ast.Annotation) string {
	if a == nil {
		return ""
	}
	v := a.Value()
	if v == nil {
		return ""
	}
	if lit, ok := v.Value.(*ast.Literal); ok && lit.Kind == ast.LitString {
		return lit.Str
	}
	return ""
}

// annoStringList reads a `String[]` argument such as `of = {"a", "b"}`.
func annoStringList(a *ast.Annotation, name string) []string {
	if a == nil {
		return nil
	}
	arg := a.Arg(name)
	if arg == nil {
		return nil
	}
	var out []string
	switch v := arg.Value.(type) {
	case *ast.ArrayInit:
		for _, e := range v.Elems {
			if lit, ok := e.(*ast.Literal); ok && lit.Kind == ast.LitString {
				out = append(out, lit.Str)
			}
		}
	case *ast.Literal:
		if v.Kind == ast.LitString {
			out = append(out, v.Str)
		}
	}
	return out
}

// annoClasses reads a `Class[]` argument such as `@ExtensionMethod({A.class})`.
func annoClasses(a *ast.Annotation) []*ast.TypeExpr {
	if a == nil {
		return nil
	}
	var out []*ast.TypeExpr
	add := func(e ast.Expr) {
		if cl, ok := e.(*ast.ClassLit); ok {
			out = append(out, cl.Type)
		}
	}
	if v := a.Value(); v != nil {
		switch x := v.Value.(type) {
		case *ast.ArrayInit:
			for _, e := range x.Elems {
				add(e)
			}
		case *ast.ClassLit:
			out = append(out, x.Type)
		}
	}
	if arg := a.Arg("value"); arg != nil {
		switch x := arg.Value.(type) {
		case *ast.ArrayInit:
			for _, e := range x.Elems {
				add(e)
			}
		case *ast.ClassLit:
			out = append(out, x.Type)
		}
	}
	return out
}

// hasAnno reports whether any annotation in the list matches.
func hasAnno(annos []*ast.Annotation, names ...string) *ast.Annotation {
	for _, a := range annos {
		if a.Is(names...) {
			return a
		}
	}
	return nil
}

// accessorsOptions holds @Accessors settings, which also drive @Getter/@Setter.
type accessorsOptions struct {
	chain  bool
	fluent bool
	prefix []string
}

func (c *Checker) accessorsOf(annos []*ast.Annotation) accessorsOptions {
	o := accessorsOptions{}
	if a := hasAnno(annos, "Accessors"); a != nil {
		o.chain = annoBool(a, "chain", false)
		o.fluent = annoBool(a, "fluent", false)
		o.prefix = annoStringList(a, "prefix")
	}
	return o
}

// stripPrefix removes the configured prefixes from a field name.
func (o accessorsOptions) stripPrefix(name string) string {
	for _, p := range o.prefix {
		if p != "" && strings.HasPrefix(name, p) && len(name) > len(p) {
			return strings.ToUpper(name[len(p):len(p)+1]) + name[len(p)+1:]
		}
	}
	return name
}

// applyLombok runs the annotation pass for one class. It is called at the end
// of resolveMembers, before vtable layout, so generated members take part in
// overriding and virtual dispatch like declared ones.
func (c *Checker) applyLombok(cl *ast.Class) {
	cd := cl.Decl
	if cd == nil {
		return
	}
	classAnnos := cd.Annos
	accessors := c.accessorsOf(classAnnos)

	// ---- what the member annotations mean, before anything reads them
	c.lombokMemberFlags(cl)

	// ---- class-level structural annotations
	if a := hasAnno(classAnnos, "UtilityClass"); a != nil {
		c.lombokUtilityClass(cl)
	}
	if a := hasAnno(classAnnos, "Value"); a != nil {
		c.lombokValue(cl, accessors, a)
	}
	if a := hasAnno(classAnnos, "FieldDefaults"); a != nil {
		c.lombokFieldDefaults(cl, a)
	}
	if hasAnno(classAnnos, "Data") != nil {
		c.lombokData(cl, accessors)
	}
	if hasAnno(classAnnos, "SuperBuilder") != nil {
		// handled together with @Builder below
	}

	// ---- member annotations
	c.lombokMembers(cl, accessors, classAnnos)

	// ---- class-level generators
	if a := hasAnno(classAnnos, "Getter"); a != nil {
		c.lombokGetter(cl, c.instanceAndStaticFields(cl), onSiteOf(a, classAnnos), accessors)
	}
	if a := hasAnno(classAnnos, "Setter"); a != nil {
		c.lombokSetter(cl, c.instanceAndStaticFields(cl), onSiteOf(a, classAnnos), accessors)
	}
	if a := hasAnno(classAnnos, "ToString"); a != nil {
		c.lombokToString(cl, onSiteOf(a, classAnnos))
	}
	if a := hasAnno(classAnnos, "EqualsAndHashCode"); a != nil {
		c.lombokEqualsHashCode(cl, onSiteOf(a, classAnnos))
	}
	if a := hasAnno(classAnnos, "RequiredArgsConstructor"); a != nil {
		c.lombokCtor(cl, onSiteOf(a, classAnnos), "required")
	}
	if a := hasAnno(classAnnos, "AllArgsConstructor"); a != nil {
		c.lombokCtor(cl, onSiteOf(a, classAnnos), "all")
	}
	if a := hasAnno(classAnnos, "NoArgsConstructor"); a != nil {
		c.lombokCtor(cl, onSiteOf(a, classAnnos), "none")
	}
	if a := hasAnno(classAnnos, "Builder", "SuperBuilder"); a != nil {
		if a.Is("SuperBuilder") {
			c.lombokSuperBuilder(cl, a, classAnnos)
		} else {
			c.lombokBuilder(cl, a, classAnnos)
		}
	}
	if a := hasAnno(classAnnos, "Singular"); a != nil {
		c.errf(cl.Decl.Pos, "TY-INT-0004", "@Singular goes on a builder field, not on the class")
	}
	if a := hasAnno(classAnnos, "StandardException"); a != nil && cl.Kind == ast.KindClass {
		c.lombokStandardException(cl)
	}
	if a := hasAnno(classAnnos, "FieldNameConstants"); a != nil {
		c.lombokFieldNameConstants(cl, a)
	}
	if a := hasAnno(classAnnos, "Helper"); a != nil {
		cl.Mods |= ast.ModStatic
	}
	if a := hasAnno(classAnnos, "Log", "Slf4j", "Log4j", "Log4j2", "CommonsLog", "JBossLog", "Flogger", "XSlf4j"); a != nil {
		c.lombokLog(cl, a)
	}
	c.lombokDelegates(cl)
	if a := hasAnno(classAnnos, "CustomLog"); a != nil {
		c.lombokCustomLog(cl, a)
	}
	if hasAnno(classAnnos, "Jacksonized") != nil {
		// No Jackson serialization exists in Teyru; the annotation is accepted
		// and has no effect (documented in docs/lombok.md).
	}
	if hasAnno(classAnnos, "Var") != nil {
		// deprecated Lombok alias for `var`; nothing to generate
	}
	if hasAnno(classAnnos, "NonFinal") != nil {
		cl.Mods &^= ast.ModFinal
	}
	if hasAnno(classAnnos, "PackagePrivate") != nil {
		for _, f := range cl.Fields {
			f.Mods &^= ast.ModPublic | ast.ModPrivate | ast.ModProtected
		}
		for _, ms := range cl.Methods {
			for _, m := range ms {
				m.Mods &^= ast.ModPublic | ast.ModPrivate | ast.ModProtected
			}
		}
	}
}

// instanceAndStaticFields returns the non-synthetic fields of a class.
func (c *Checker) instanceAndStaticFields(cl *ast.Class) []*ast.Field {
	var out []*ast.Field
	for _, f := range cl.Fields {
		if f.IsProp || f.Anno != "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// lombokDelegates generates delegating methods for @Delegate fields.
func (c *Checker) lombokDelegates(cl *ast.Class) {
	for _, f := range cl.Fields {
		ann := delegateAnnoOf(cl, f.Name)
		if ann == nil {
			continue
		}
		ct, ok := f.Type.(*ast.ClassType)
		if !ok {
			continue
		}
		target := ct.Class
		methods := map[string]*ast.Method{}
		collect := func(k *ast.Class) {
			for _, ms := range k.Methods {
				for _, m := range ms {
					if m.IsStatic() || m.Mods.Has(ast.ModPrivate) || m.IsCtor {
						continue
					}
					if _, dup := methods[m.Name]; dup {
						continue
					}
					methods[m.Name] = m
				}
			}
		}
		for k := target; k != nil; k = k.Super.Class {
			if k.Super == nil {
				collect(k)
				break
			}
			collect(k)
		}
		c.eachInterface(target, func(i *ast.Class) bool {
			collect(i)
			return true
		})
		names := make([]string, 0, len(methods))
		for n := range methods {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m := methods[n]
			if hasMethodDecl(cl.Decl, m.Name, len(m.Params)) {
				continue
			}
			var params []ast.Type
			var names2 []string
			var args []ast.Expr
			for i, pt := range m.Params {
				pn := fmt.Sprintf("arg%d", i)
				if i < len(m.ParamNames) {
					pn = m.ParamNames[i]
				}
				params = append(params, pt)
				names2 = append(names2, pn)
				args = append(args, id(pn))
			}
			var body *ast.Block
			if m.Result == ast.TVoid {
				body = blockOf(exprStmtOf(callNew(sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, f.Name), m.Name, args...)))
			} else {
				body = blockOf(returnOf(callNew(sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, f.Name), m.Name, args...)))
			}
			dm := c.newSynthMethod(cl, m.Name, ast.ModPublic, m.Result, params, names2, body, "@Delegate")
			c.addSynthMethod(cl, dm)
		}
	}
}

// delegateAnnoOf finds @Delegate on a field declaration.
func delegateAnnoOf(cl *ast.Class, field string) *ast.Annotation {
	if cl.Decl == nil {
		return nil
	}
	for _, mem := range cl.Decl.Members {
		fd, ok := mem.(*ast.FieldDecl)
		if !ok {
			continue
		}
		for _, vd := range fd.Vars {
			if vd.Name == field {
				return hasAnno(fd.Annos, "Delegate")
			}
		}
	}
	return nil
}

// lombokMemberFlags records what the member annotations mean.
//
// They are read by the generators -- a setter checks @NonNull, a constructor
// collects @NonNull fields, a builder reads @Singular and @ObtainVia, a
// generator steps aside for a @Tolerate name -- so they have to be set before
// any of them runs. They used to be set in the middle of the same pass that
// generates the accessors, which meant `@Setter @NonNull String label` produced
// a setter with no null check, and @Data's required-args constructor had no
// @NonNull field to collect.
func (c *Checker) lombokMemberFlags(cl *ast.Class) {
	if cl.Decl == nil {
		return
	}
	for _, mem := range cl.Decl.Members {
		if md, ok := mem.(*ast.MethodDecl); ok {
			if md.Sym != nil && hasAnno(md.Annos, "Tolerate") != nil {
				md.Sym.Tolerate = true
			}
			continue
		}
		d, ok := mem.(*ast.FieldDecl)
		if !ok {
			continue
		}
		for _, vd := range d.Vars {
			f := vd.Fld
			if f == nil {
				continue
			}
			if hasAnno(d.Annos, "NonNull") != nil {
				f.NonNull = true
			}
			if a := hasAnno(d.Annos, "ObtainVia"); a != nil {
				f.ObtainViaField = annoString(a, "field")
				f.ObtainViaMethod = annoString(a, "method")
				f.ObtainViaStatic = annoBool(a, "isStatic", false)
			}
			if a := hasAnno(d.Annos, "Singular"); a != nil {
				f.Singular = true
				f.SingularName = annoValueString(a)
				if f.SingularName == "" {
					f.SingularName = annoString(a, "value")
				}
			}
			if hasAnno(d.Annos, "Include") != nil {
				f.Include = true
			}
			if hasAnno(d.Annos, "Exclude") != nil {
				f.Exclude = true
			}
		}
	}
}

// lombokMembers handles annotations placed on individual members.
func (c *Checker) lombokMembers(cl *ast.Class, accessors accessorsOptions, classAnnos []*ast.Annotation) {
	cd := cl.Decl
	for _, mem := range cd.Members {
		switch d := mem.(type) {
		case *ast.FieldDecl:
			for _, vd := range d.Vars {
				f := vd.Fld
				if f == nil {
					continue
				}
				if a := hasAnno(d.Annos, "Getter"); a != nil {
					c.lombokGetter(cl, []*ast.Field{f}, onSiteOf(a, d.Annos), accessors)
				}
				if a := hasAnno(d.Annos, "Setter"); a != nil {
					c.lombokSetter(cl, []*ast.Field{f}, onSiteOf(a, d.Annos), accessors)
				}
				if a := hasAnno(d.Annos, "With"); a != nil {
					c.lombokWith(cl, f, onSiteOf(a, d.Annos))
				}
				if a := hasAnno(d.Annos, "FieldNameConstants"); a != nil {
					// field-level variant adds one constant
					c.lombokFieldNameConstants(cl, a)
				}
			}
		case *ast.MethodDecl:
			if d.Sym == nil {
				continue
			}
			// @NonNull on a parameter of a method or constructor is a check at
			// the top of the body. It was looked for on the method instead, so
			// the annotation only worked on a method that also carried it --
			// which is not how it is written, and not what it means.
			c.lombokNonNullParams(d)
			if hasAnno(d.Annos, "SneakyThrows") != nil {
				c.lombokSneakyThrows(d)
			}
			if hasAnno(d.Annos, "Synchronized") != nil {
				c.lombokSynchronized(cl, d)
			}
			if a := hasAnno(d.Annos, "Locked"); a != nil {
				c.lombokLocked(cl, d, a)
			}
			if a := hasAnno(d.Annos, "Builder"); a != nil {
				c.lombokMethodBuilder(cl, d, a)
			}
		}
	}
}

// ---------------------------------------------------------------- getters

func (c *Checker) lombokGetter(cl *ast.Class, fields []*ast.Field, s onSite, o accessorsOptions) {
	a := s.gen
	mods, none, ok := annoAccessLevel(a, "value")
	if none {
		// AccessLevel.NONE: Lombok generates no accessor at all
		return
	}
	if !ok {
		mods = ast.ModPublic
	}
	lazy := annoBool(a, "lazy", false)
	for _, f := range fields {
		// copy per field: a static field must not make the accessors of the
		// instance fields that follow it static as well
		fmods := mods
		if f.Mods.Has(ast.ModStatic) {
			fmods |= ast.ModStatic
		}
		name := getterName(f, o)
		// Lombok does not generate an accessor whose name is already taken,
		// whatever the parameter types are -- which is why @Tolerate exists:
		// it makes lombok treat that member as if it were not there.
		if hasMethodDecl(cl.Decl, name, 0) && !toleratedMember(cl, name) {
			continue
		}
		if lazy {
			c.lombokLazyGetter(cl, f, fmods, name, s)
			continue
		}
		m := c.newSynthMethod(cl, name, fmods, f.Type, nil, nil,
			blockOf(returnOf(thisField(f))), "@Getter")
		m.Anno = "@Getter"
		m.Prop = f
		c.placeOn(m, s.memberAnnos(), nil)
		c.addSynthMethod(cl, m)
	}
}

// getterName applies the JavaBeans rule and @Accessors settings.
func getterName(f *ast.Field, o accessorsOptions) string {
	base := o.stripPrefix(f.Name)
	if o.fluent {
		return base
	}
	prefix := "get"
	if ast.IsPrim(f.Type, ast.Boolean) {
		prefix = "is"
	}
	return prefix + util.Capitalize(base)
}

func setterName(f *ast.Field, o accessorsOptions) string {
	base := o.stripPrefix(f.Name)
	if o.fluent {
		return base
	}
	return "set" + util.Capitalize(base)
}

func (c *Checker) lombokSetter(cl *ast.Class, fields []*ast.Field, s onSite, o accessorsOptions) {
	a := s.gen
	mods, none, ok := annoAccessLevel(a, "value")
	if none {
		// AccessLevel.NONE: Lombok generates no setter at all
		return
	}
	if !ok {
		mods = ast.ModPublic
	}
	for _, f := range fields {
		if f.Mods.Has(ast.ModFinal) {
			continue
		}
		if hasMethodDecl(cl.Decl, setterName(f, o), 1) && !toleratedMember(cl, setterName(f, o)) {
			continue
		}
		mmods := mods
		if f.Mods.Has(ast.ModStatic) {
			mmods |= ast.ModStatic
		}
		stmts := []ast.Stmt{}
		if f.NonNull {
			msg := f.Name + " is marked non-null but is null"
			stmts = append(stmts, ifOf(isNull(id("value")), throwOf(newObj(c.b.NPE, strLit(msg))), nil))
		}
		stmts = append(stmts, exprStmtOf(assignTo(thisField(f), id("value"))))
		var result ast.Type = ast.TVoid
		if o.chain {
			result = &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}
			stmts = append(stmts, returnOf(thisStat(cl)))
		}
		m := c.newSynthMethod(cl, setterName(f, o), mmods, result, []ast.Type{f.Type}, []string{"value"},
			blockOf(stmts...), "")
		m.Anno = "@Setter"
		c.placeOn(m, s.memberAnnos(), s.paramAnnos())
		c.addSynthMethod(cl, m)
	}
}

// lombokLazyGetter implements @Getter(lazy = true): the value is computed once
// and cached in a synthesized holder field.
func (c *Checker) lombokLazyGetter(cl *ast.Class, f *ast.Field, mods ast.Mods, name string, s onSite) {
	// The holder is the field's boxed type, because "not computed yet" is told
	// apart from "computed" by null: an int holder has no null to test, and the
	// getter for one did not compile at all.
	holder := &ast.Field{
		Name: "__lazy$" + f.Name, Type: c.boxed(f.Type),
		Mods: ast.ModPrivate | ast.ModVolatile, Pos: f.Pos, Storage: true,
		Anno: "@Getter(lazy)",
	}
	c.addSynthField(cl, holder)
	var init ast.Expr = nullLit()
	if f.Decl != nil && f.Decl.Init != nil {
		init = f.Decl.Init
		// Lombok moves the initializer into the getter: the field keeps no
		// initializer of its own, or the constructor would evaluate it as well
		// and the lazy value would be computed twice.
		f.Decl.Init = nil
	}
	body := blockOf(
		ifOf(isNull(thisField(holder)),
			blockOf(
				exprStmtOf(assignTo(thisField(holder), init)),
			), nil),
		returnOf(thisField(holder)),
	)
	m := c.newSynthMethod(cl, name, mods, f.Type, nil, nil, body, "")
	m.Anno = "@Getter(lazy)"
	c.placeOn(m, s.memberAnnos(), nil)
	c.addSynthMethod(cl, m)
}

// ---------------------------------------------------------------- tostring

func (c *Checker) lombokToString(cl *ast.Class, s onSite) {
	a := s.gen
	fields := c.toStringFields(cl, a)
	if hasMethodDecl(cl.Decl, "toString", 0) {
		return
	}
	includeNames := annoBool(a, "includeFieldNames", true)
	parts := []ast.Expr{strLit(cl.Name + "(")}
	first := true
	for _, f := range fields {
		label := ""
		if includeNames {
			label = f.Name + "="
		}
		if !first {
			parts = append(parts, strLit(", "+label))
		} else if label != "" {
			parts = append(parts, strLit(label))
		}
		first = false
		parts = append(parts, thisField(f))
	}
	if annoBool(a, "callSuper", false) && cl.Super != nil {
		parts = append(parts, strLit("; super="), superCall("toString"))
	}
	parts = append(parts, strLit(")"))
	m := c.newSynthMethod(cl, "toString", ast.ModPublic, c.strType, nil, nil,
		blockOf(returnOf(concatStr(parts...))), "")
	m.Anno = "@ToString"
	c.placeOn(m, s.memberAnnos(), nil)
	c.addSynthMethod(cl, m)
}

// toStringFields applies of/exclude/onlyExplicitlyIncluded.
func (c *Checker) toStringFields(cl *ast.Class, a *ast.Annotation) []*ast.Field {
	of := annoStringList(a, "of")
	exclude := map[string]bool{}
	for _, n := range annoStringList(a, "exclude") {
		exclude[n] = true
	}
	only := annoBool(a, "onlyExplicitlyIncluded", false)
	ofSet := map[string]bool{}
	for _, n := range of {
		ofSet[n] = true
	}
	var out []*ast.Field
	for _, f := range c.instanceAndStaticFields(cl) {
		if f.Mods.Has(ast.ModStatic) {
			continue
		}
		if len(of) > 0 {
			if ofSet[f.Name] {
				out = append(out, f)
			}
			continue
		}
		if exclude[f.Name] || f.Exclude {
			continue
		}
		if only && !f.Include {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------- equals/hashCode

func (c *Checker) lombokEqualsHashCode(cl *ast.Class, s onSite) {
	a := s.gen
	of := annoStringList(a, "of")
	exclude := map[string]bool{}
	for _, n := range annoStringList(a, "exclude") {
		exclude[n] = true
	}
	ofSet := map[string]bool{}
	for _, n := range of {
		ofSet[n] = true
	}
	callSuper := annoBool(a, "callSuper", false)
	only := annoBool(a, "onlyExplicitlyIncluded", false)
	var fields []*ast.Field
	for _, f := range c.instanceAndStaticFields(cl) {
		if f.Mods.Has(ast.ModStatic) {
			continue
		}
		if len(of) > 0 {
			if ofSet[f.Name] {
				fields = append(fields, f)
			}
			continue
		}
		if exclude[f.Name] || f.Exclude {
			continue
		}
		// onlyExplicitlyIncluded: the fields marked @EqualsAndHashCode.Include
		// (or @ToString.Include) are the ones that count
		if only && !f.Include {
			continue
		}
		fields = append(fields, f)
	}
	if !hasMethodDecl(cl.Decl, "equals", 1) {
		c.lombokEquals(cl, fields, callSuper, s)
	}
	if !hasMethodDecl(cl.Decl, "hashCode", 0) {
		c.lombokHashCode(cl, fields, callSuper, s)
	}
}

func (c *Checker) lombokEquals(cl *ast.Class, fields []*ast.Field, callSuper bool, s onSite) {
	self := &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}
	objType := &ast.ClassType{Class: c.b.Object}
	stmts := []ast.Stmt{
		ifOf(eq(thisStat(cl), id("o")), blockOf(returnOf(boolLit(true))), nil),
		ifOf(binop("||", isNull(id("o")),
			&ast.Unary{ExprBase: ast.ExprBase{Pos: pos()}, Op: "!",
				X: &ast.InstanceOf{ExprBase: ast.ExprBase{Pos: pos()}, X: id("o"),
					Type: &ast.TypeExpr{Pos: pos(), Name: cl.Name, Resolved: self}}}),
			blockOf(returnOf(boolLit(false))), nil),
	}
	if callSuper && cl.Super != nil {
		stmts = append(stmts, ifOf(
			&ast.Unary{ExprBase: ast.ExprBase{Pos: pos()}, Op: "!",
				X: superCall("equals", id("o"))},
			blockOf(returnOf(boolLit(false))), nil))
	}
	if len(fields) > 0 {
		other := &ast.VarDeclarator{Pos: pos(), Name: "other",
			Init: &ast.Cast{ExprBase: ast.ExprBase{Pos: pos()}, Type: &ast.TypeExpr{Pos: pos(), Name: cl.Name, Resolved: self}, X: id("o")}}
		stmts = append(stmts, &ast.LocalVar{Pos: pos(), Type: &ast.TypeExpr{Pos: pos(), Name: cl.Name, Resolved: self}, Vars: []*ast.VarDeclarator{other}})
	}
	for _, f := range fields {
		other := sel(id("other"), f.Name)
		mine := thisField(f)
		var differ ast.Expr
		switch {
		case ast.IsPrim(f.Type, ast.Double):
			differ = binop("!=", callNew(id("Double"), "compare", mine, other), intLit(0))
		case ast.IsPrim(f.Type, ast.Float):
			differ = binop("!=", callNew(id("Float"), "compare", mine, other), intLit(0))
		case util.IsPrim(f.Type):
			differ = binop("!=", mine, other)
		default:
			// reference: null-safe equality
			differ = binop("||",
				binop("&&", isNull(mine), binop("!=", other, nullLit())),
				binop("&&", binop("!=", mine, nullLit()),
					&ast.Unary{ExprBase: ast.ExprBase{Pos: pos()}, Op: "!",
						X: callNew(mine, "equals", other)}))
		}
		stmts = append(stmts, ifOf(differ, blockOf(returnOf(boolLit(false))), nil))
	}
	stmts = append(stmts, returnOf(boolLit(true)))
	m := c.newSynthMethod(cl, "equals", ast.ModPublic, ast.TBoolean,
		[]ast.Type{objType}, []string{"o"}, blockOf(stmts...), "")
	m.Anno = "@EqualsAndHashCode"
	// Lombok puts onParam_ on the parameter of the generated equals method.
	c.placeOn(m, s.memberAnnos(), s.paramAnnos())
	c.addSynthMethod(cl, m)
}

func (c *Checker) lombokHashCode(cl *ast.Class, fields []*ast.Field, callSuper bool, s onSite) {
	start := ast.Expr(intLit(1))
	if callSuper && cl.Super != nil {
		start = superCall("hashCode")
	}
	stmts := []ast.Stmt{
		&ast.LocalVar{Pos: pos(), Type: &ast.TypeExpr{Pos: pos(), Name: "int", Resolved: ast.TInt},
			Vars: []*ast.VarDeclarator{{Pos: pos(), Name: "result", Init: start}}},
	}
	for _, f := range fields {
		var h ast.Expr
		switch {
		case ast.IsPrim(f.Type, ast.Boolean):
			h = &ast.Cond{ExprBase: ast.ExprBase{Pos: pos()}, C: thisField(f), X: intLit(1231), Y: intLit(1237)}
		case ast.IsPrim(f.Type, ast.Long):
			h = &ast.Cast{ExprBase: ast.ExprBase{Pos: pos()}, Type: &ast.TypeExpr{Pos: pos(), Name: "int", Resolved: ast.TInt},
				X: binop("^", thisField(f), binop(">>>", thisField(f), intLit(32)))}
		case ast.IsPrim(f.Type, ast.Double):
			h = callNew(id("Double"), "hashCode", thisField(f))
		case ast.IsPrim(f.Type, ast.Float):
			h = callNew(id("Float"), "hashCode", thisField(f))
		case util.IsPrim(f.Type):
			h = thisField(f)
		default:
			h = &ast.Cond{ExprBase: ast.ExprBase{Pos: pos()}, C: isNull(thisField(f)), X: intLit(0), Y: callNew(thisField(f), "hashCode")}
		}
		stmts = append(stmts, exprStmtOf(assignTo(id("result"),
			binop("+", binop("*", intLit(31), id("result")), h))))
	}
	stmts = append(stmts, returnOf(id("result")))
	m := c.newSynthMethod(cl, "hashCode", ast.ModPublic, ast.TInt, nil, nil, blockOf(stmts...), "")
	m.Anno = "@EqualsAndHashCode"
	// hashCode takes no parameter, so only the member annotations apply.
	c.placeOn(m, s.memberAnnos(), nil)
	c.addSynthMethod(cl, m)
}

// ---------------------------------------------------------------- constructors

// ctorFields selects the fields a generated constructor initializes.
func (c *Checker) ctorFields(cl *ast.Class, kind string) []*ast.Field {
	var out []*ast.Field
	for _, f := range c.instanceAndStaticFields(cl) {
		if f.Mods.Has(ast.ModStatic) {
			continue
		}
		switch kind {
		case "all":
			if f.Decl != nil && f.Decl.Init != nil && f.Mods.Has(ast.ModFinal) {
				continue
			}
			out = append(out, f)
		case "required":
			hasInit := f.Decl != nil && f.Decl.Init != nil
			if (f.Mods.Has(ast.ModFinal) && !hasInit) || f.NonNull {
				out = append(out, f)
			}
		}
	}
	return out
}

func (c *Checker) lombokCtor(cl *ast.Class, s onSite, kind string) {
	a := s.gen
	fields := c.ctorFields(cl, kind)
	var params []ast.Type
	var names []string
	stmts := []ast.Stmt{}
	for _, f := range fields {
		params = append(params, f.Type)
		names = append(names, f.Name)
		if f.NonNull {
			msg := f.Name + " is marked non-null but is null"
			stmts = append(stmts, ifOf(isNull(id(f.Name)), throwOf(newObj(c.b.NPE, strLit(msg))), nil))
		}
		stmts = append(stmts, exprStmtOf(assignTo(thisField(f), id(f.Name))))
	}
	mods, none, ok := annoAccessLevel(a, "access")
	if none {
		// Lombok's handler returns before generating anything
		return
	}
	if !ok {
		mods = ast.ModPublic
	}
	c.dropDefaultCtor(cl)
	if sn := annoString(a, "staticName"); sn != "" {
		c.lombokStaticFactory(cl, sn, params, names, stmts, mods, s)
		return
	}
	m := &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: mods,
		Result: ast.TVoid, Params: params, ParamNames: names, Body: blockOf(stmts...), Pos: pos()}
	m.Anno = "@" + kind + "ArgsConstructor"
	c.placeOn(m, s.ctorAnnos(), s.paramAnnos())
	c.addSynthCtor(cl, m)
}

// dropDefaultCtor removes the constructor resolveMembers synthesized for a
// class that declared none.
//
// Lombok injects its constructor into the class declaration, and javac, finding
// a constructor declared, never adds the implicit one. Teyru adds the implicit
// public no-argument constructor while resolving members, which happens before
// the Lombok pass, so a generated no-argument constructor found the signature
// taken and addSynthCtor stepped aside for it:
// `@NoArgsConstructor(access = AccessLevel.PRIVATE)` on a class with no
// constructors of its own left the class with the implicit public constructor
// and no private one, which is the annotation doing nothing at all.
//
// @AllArgsConstructor and @RequiredArgsConstructor need this too, and for the
// same reason: Lombok's `class A { int x; }` with @AllArgsConstructor has one
// constructor, `A(int)`, and no `A()` at all.
func (c *Checker) dropDefaultCtor(cl *ast.Class) {
	kept := make([]*ast.Method, 0, len(cl.Ctors))
	for _, m := range cl.Ctors {
		if m.SynthKind == "default-ctor" {
			continue
		}
		kept = append(kept, m)
	}
	cl.Ctors = kept
}

// lombokStaticFactory emits `static Cls of(args) { return new Cls(args) }`.
func (c *Checker) lombokStaticFactory(cl *ast.Class, name string, params []ast.Type, names []string, ctorStmts []ast.Stmt, mods ast.Mods, s onSite) {
	inner := &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPrivate,
		Result: ast.TVoid, Params: params, ParamNames: names, Body: blockOf(ctorStmts...), Pos: pos()}
	c.placeOn(inner, s.ctorAnnos(), s.paramAnnos())
	c.addSynthCtor(cl, inner)
	args := make([]ast.Expr, len(names))
	for i, n := range names {
		args[i] = id(n)
	}
	m := c.newSynthMethod(cl, name, (mods&^(ast.ModProtected))|ast.ModStatic, &ast.ClassType{Class: cl, Args: typeVarArgs(cl)},
		params, names, blockOf(returnOf(newObj(cl, args...))), "")
	m.Anno = "staticConstructor"
	c.placeOn(m, s.memberAnnos(), nil)
	c.addSynthMethod(cl, m)
}

func (c *Checker) lombokData(cl *ast.Class, o accessorsOptions) {
	cl.Mods &^= ast.ModFinal
	if !hasMethodDecl(cl.Decl, "toString", 0) {
		c.lombokToStringNoAnno(cl)
	}
	c.lombokEqualsHashCodeNoAnno(cl)
	fields := c.instanceAndStaticFields(cl)
	for _, f := range fields {
		if f.Mods.Has(ast.ModStatic) {
			continue
		}
		f.Mods |= ast.ModPrivate
		c.lombokGetter(cl, []*ast.Field{f}, onSiteOf(&ast.Annotation{Name: "Getter"}, nil), o)
		if !f.Mods.Has(ast.ModFinal) {
			c.lombokSetter(cl, []*ast.Field{f}, onSiteOf(&ast.Annotation{Name: "Setter"}, nil), o)
		}
	}
	c.lombokCtor(cl, onSiteOf(&ast.Annotation{Name: "RequiredArgsConstructor"}, nil), "required")
}

func (c *Checker) lombokValue(cl *ast.Class, o accessorsOptions, a *ast.Annotation) {
	wasFinal := cl.Mods.Has(ast.ModFinal)
	cl.Mods |= ast.ModFinal
	c.checkLombokFinal(cl, wasFinal)
	for _, f := range c.instanceAndStaticFields(cl) {
		f.Mods |= ast.ModPrivate | ast.ModFinal
	}
	c.lombokGetter(cl, c.instanceAndStaticFields(cl), onSiteOf(&ast.Annotation{Name: "Getter"}, nil), o)
	if !hasMethodDecl(cl.Decl, "toString", 0) {
		c.lombokToStringNoAnno(cl)
	}
	c.lombokEqualsHashCodeNoAnno(cl)
	cta := &ast.Annotation{Name: "AllArgsConstructor"}
	if sn := annoString(a, "staticConstructor"); sn != "" {
		cta.Args = append(cta.Args, &ast.AnnoArg{Name: "staticName", Value: strLit(sn)})
	}
	c.lombokCtor(cl, onSiteOf(cta, nil), "all")
}

func (c *Checker) lombokToStringNoAnno(cl *ast.Class) {
	c.lombokToString(cl, onSiteOf(&ast.Annotation{Name: "ToString"}, nil))
}

func (c *Checker) lombokEqualsHashCodeNoAnno(cl *ast.Class) {
	c.lombokEqualsHashCode(cl, onSiteOf(&ast.Annotation{Name: "EqualsAndHashCode"}, nil))
}

func (c *Checker) lombokUtilityClass(cl *ast.Class) {
	wasFinal := cl.Mods.Has(ast.ModFinal)
	cl.Mods |= ast.ModFinal
	c.checkLombokFinal(cl, wasFinal)
	for _, f := range cl.Fields {
		f.Mods |= ast.ModStatic
	}
	for _, ms := range cl.Methods {
		for _, m := range ms {
			if !m.IsCtor {
				m.Mods |= ast.ModStatic
			}
		}
	}
	for _, ctor := range cl.Ctors {
		ctor.Mods = (ctor.Mods &^ (ast.ModPublic | ast.ModProtected)) | ast.ModPrivate
	}
	cl.Utility = true
}

func (c *Checker) lombokFieldDefaults(cl *ast.Class, a *ast.Annotation) {
	// AccessLevel.NONE -- the parameter's default -- and AccessLevel.PACKAGE
	// both come back as no modifier, which is what "leave the field's own
	// access alone" should do.
	level, _, _ := annoAccessLevel(a, "level")
	makeFinal := annoBool(a, "makeFinal", false)
	for _, f := range c.instanceAndStaticFields(cl) {
		if f.Mods&(ast.ModPublic|ast.ModPrivate|ast.ModProtected) == 0 {
			f.Mods |= level
		}
		if makeFinal {
			f.Mods |= ast.ModFinal
		}
	}
}

// checkLombokFinal reports the classes that have just been made final by an
// annotation, which the check in resolveHeader cannot see: that one runs before
// Lombok, so `class Ext extends V` compiled for a @Value or @UtilityClass class
// where Lombok says `cannot inherit from final V`.
func (c *Checker) checkLombokFinal(cl *ast.Class, wasFinal bool) {
	if wasFinal {
		// the modifier was written by hand and resolveHeader already read it
		return
	}
	for _, sub := range cl.Subclasses {
		if sub.Decl == nil {
			continue
		}
		pos := sub.Decl.Pos
		if sub.Kind == ast.KindClass && len(sub.Decl.Extends) > 0 {
			pos = sub.Decl.Extends[0].Pos
		}
		c.errf(pos, "TY-TYP-0007", "cannot extend final class %s", cl.Name)
	}
}

// ---------------------------------------------------------------- builder

// lombokBuilder generates a nested Builder class for @Builder.
func (c *Checker) lombokBuilder(cl *ast.Class, a *ast.Annotation, classAnnos []*ast.Annotation) {
	c.lombokBuilderFor(cl, a, classAnnos, nil)
}

// lombokBuilderFor generates a builder; fields overrides the set of fields the
// builder covers, which @SuperBuilder uses to include the inherited ones.
func (c *Checker) lombokBuilderFor(cl *ast.Class, a *ast.Annotation, classAnnos []*ast.Annotation, given []*ast.Field) {
	fields := given
	if fields == nil {
		fields = c.ctorFields(cl, "all")
	}
	if len(fields) == 0 {
		fields = c.instanceAndStaticFields(cl)
	}
	builderName := annoString(a, "builderClassName")
	if builderName == "" {
		builderName = cl.Name + "Builder"
	}
	buildName := annoString(a, "buildMethodName")
	if buildName == "" {
		buildName = "build"
	}
	factoryName := annoString(a, "builderMethodName")
	if factoryName == "" {
		factoryName = "builder"
	}
	toBuilder := annoBool(a, "toBuilder", false)
	setterPrefix := annoString(a, "setterPrefix")

	// an all-args constructor the builder can call
	allArgs := &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPublic, Result: ast.TVoid, Pos: pos()}
	for _, f := range fields {
		allArgs.Params = append(allArgs.Params, f.Type)
		allArgs.ParamNames = append(allArgs.ParamNames, f.Name)
		var stmts []ast.Stmt
		if f.NonNull {
			msg := f.Name + " is marked non-null but is null"
			stmts = append(stmts, ifOf(isNull(id(f.Name)), throwOf(newObj(c.b.NPE, strLit(msg))), nil))
		}
		stmts = append(stmts, exprStmtOf(assignTo(thisField(f), id(f.Name))))
		allArgs.Body = blockOf(append(stmtsFrom(allArgs.Body), stmts...)...)
	}
	// Lombok's builder calls a constructor of its own, so the class has one and
	// the implicit no-argument constructor must go: `@Builder class A { int x }`
	// has `A(int)` and no `A()` (see dropDefaultCtor).
	c.dropDefaultCtor(cl)
	c.addSynthCtor(cl, allArgs)

	// the builder class itself
	b := c.newBuilderClass(cl, builderName)
	builderType := &ast.ClassType{Class: b}
	for _, f := range fields {
		bf := &ast.Field{Name: f.Name, Type: f.Type, Mods: ast.ModPrivate, Pos: pos(), Storage: true, Owner: b}
		if f.Decl != nil && f.Decl.Init != nil {
			bf.DefaultExpr = f.Decl.Init // @Builder.Default
		}
		c.addSynthField(b, bf)
		if f.Singular {
			c.lombokSingular(b, builderType, f, setterPrefix)
			continue
		}
		// Builder field(T value) { this.f = value; return this }
		body := blockOf(
			exprStmtOf(assignTo(sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, f.Name), id("value"))),
			returnOf(&ast.This{ExprBase: ast.ExprBase{Pos: pos(), T: builderType}}),
		)
		bm := c.newSynthMethod(b, builderSetterName(f.Name, setterPrefix), ast.ModPublic, builderType, []ast.Type{f.Type}, []string{"value"}, body, "")
		bm.Anno = "@Builder"
		c.addSynthMethod(b, bm)
	}
	// build()
	args := make([]ast.Expr, len(fields))
	for i, f := range fields {
		args[i] = c.obtainExpr(f)
		if f.Singular {
			// @Singular: the built object gets its own copy, and never null
			args[i] = callNamed(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, c.singularCopyName(f))
		}
	}
	build := c.newSynthMethod(b, buildName, ast.ModPublic, &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, nil, nil,
		blockOf(returnOf(newObj(cl, args...))), "")
	build.Anno = "@Builder"
	c.addSynthMethod(b, build)

	// static builder() on the annotated class
	if !hasMethodDecl(cl.Decl, factoryName, 0) {
		fac := c.newSynthMethod(cl, factoryName, (ast.ModPublic | ast.ModStatic), &ast.ClassType{Class: b}, nil, nil,
			blockOf(returnOf(newObj(b))), "")
		fac.Anno = "@Builder"
		c.addSynthMethod(cl, fac)
	}
	if toBuilder {
		// toBuilder() hands back a builder seeded from the instance it is
		// called on: the copy is what makes `p.toBuilder().b(9).build()` keep
		// the fields it does not touch. Returning a fresh builder instead
		// silently dropped every value the instance carried.
		local := "$builder"
		stmts := []ast.Stmt{&ast.LocalVar{Pos: pos(),
			Type: &ast.TypeExpr{Pos: pos(), Name: builderName, Resolved: builderType},
			Vars: []*ast.VarDeclarator{{Pos: pos(), Name: local, Init: newObj(b)}}}}
		for _, f := range fields {
			src := c.obtainSourceExpr(f)
			if f.Singular {
				// a @Singular collection is copied into the builder's own
				// collection, not shared with the instance being copied, so a
				// later add does not reach back into it
				stmts = append(stmts, ifOf(binop("!=", src, nullLit()),
					exprStmtOf(callNamed(id(local), singularAllName(f, setterPrefix), src)), nil))
				continue
			}
			stmts = append(stmts, exprStmtOf(callNamed(id(local), builderSetterName(f.Name, setterPrefix), src)))
		}
		stmts = append(stmts, returnOf(id(local)))
		tb := c.newSynthMethod(cl, "toBuilder", ast.ModPublic, builderType, nil, nil,
			blockOf(stmts...), "")
		tb.Anno = "@Builder"
		c.addSynthMethod(cl, tb)
	}
	// initialize @Builder.Default fields
	for _, bf := range b.Fields {
		if bf.DefaultExpr == nil {
			continue
		}
		for _, ctor := range b.Ctors {
			// the builder's synthesized constructor has no body yet
			var stmts []ast.Stmt
			if ctor.Body != nil {
				stmts = ctor.Body.Stmts
			}
			stmts = append(stmts, exprStmtOf(assignTo(
				sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, bf.Name), bf.DefaultExpr)))
			ctor.Body = blockOf(stmts...)
		}
	}
	c.layout(b)
}

// obtainExpr is how the builder reads a value for the object it builds: its
// own field, which the builder's setter method wrote.
//
// @Builder.ObtainVia is deliberately not consulted here. It says how a value is
// obtained "given an instance" (Lombok's javadoc), which is the toBuilder()
// direction -- build() reads what the builder was given, and only toBuilder
// reads an existing object. Reading it here made
// `@Builder.ObtainVia(method = "doubled")` compile against the builder class,
// which has no such method.
func (c *Checker) obtainExpr(f *ast.Field) ast.Expr {
	return sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, f.Name)
}

// obtainSourceExpr is how toBuilder reads a field of the instance it copies:
// the field itself, or the one @Builder.ObtainVia names. Lombok documents the
// two forms of the annotation as `this.value` and `this.method()`, both of
// them on the instance being converted into a builder.
func (c *Checker) obtainSourceExpr(f *ast.Field) ast.Expr {
	src := &ast.This{ExprBase: ast.ExprBase{Pos: pos()}}
	switch {
	case f.ObtainViaMethod != "" && f.ObtainViaStatic:
		// `SelfType.method(this)`: a static method of the type being built,
		// handed the instance to read the value from
		var recv ast.Expr
		if f.Owner != nil {
			recv = id(f.Owner.Full)
		}
		return callNamed(recv, f.ObtainViaMethod, src)
	case f.ObtainViaMethod != "":
		return callNamed(src, f.ObtainViaMethod)
	case f.ObtainViaField != "":
		return sel(src, f.ObtainViaField)
	}
	return sel(src, f.Name)
}

// singularKind reports how a @Singular field accumulates: it must be a List or
// a Map the standard library knows how to build.
func singularKind(t ast.Type) (elem []ast.Type, kind string) {
	ct, ok := t.(*ast.ClassType)
	if !ok || ct.Class == nil {
		return nil, ""
	}
	name := ct.Class.Name
	switch name {
	case "List", "ArrayList", "Collection", "Iterable", "Set", "HashSet", "LinkedList":
		if len(ct.Args) == 1 {
			return ct.Args, "list"
		}
	case "Map", "HashMap", "SortedMap", "TreeMap", "LinkedHashMap":
		if len(ct.Args) == 2 {
			return ct.Args, "map"
		}
	}
	return nil, ""
}

// toleratedMember reports whether a method or constructor of that name carries
// @Tolerate.
//
// @Tolerate makes Lombok "act as if it does not exist" (its own words), so a
// generator that would otherwise step aside for the name generates its own
// member after all: `@Setter private Date date` plus `@Tolerate public void
// setDate(String)` ends up as two overloads, which is the case Lombok documents
// the annotation with.
func toleratedMember(cl *ast.Class, name string) bool {
	if cl.Decl == nil {
		return false
	}
	for _, mem := range cl.Decl.Members {
		md, ok := mem.(*ast.MethodDecl)
		if ok && md.Name == name && hasAnno(md.Annos, "Tolerate") != nil {
			return true
		}
	}
	return false
}

// builderSetterName is the name of a builder's setter for one field: the field
// name itself, or the prefix and the name with its first letter raised, which
// is what Lombok's setterPrefix asks for (`with` and `name` make `withName`).
func builderSetterName(name, setterPrefix string) string {
	if setterPrefix == "" {
		return name
	}
	return setterPrefix + util.Capitalize(name)
}

// singularAdderName is the name of the builder method that accumulates a
// @Singular field one element at a time: addXs by default, the name given to
// @Singular("name"), or the field name itself for a Map.
func singularAdderName(f *ast.Field, setterPrefix string) string {
	adder := f.SingularName
	if adder == "" {
		adder = "add" + strings.ToUpper(f.Name[:1]) + f.Name[1:]
	}
	if _, kind := singularKind(f.Type); kind == "map" {
		adder = f.Name
	}
	if setterPrefix != "" {
		// Lombok's setterPrefix is a prefix on a *setter* name: with "with",
		// `name` becomes `withName`, not `withname`.
		return setterPrefix + util.Capitalize(adder)
	}
	return adder
}

// singularAllName is the builder method that takes a whole collection of a
// @Singular field.
func singularAllName(f *ast.Field, setterPrefix string) string {
	return singularAdderName(f, setterPrefix) + "All"
}

// lombokSingular generates the accumulating builder methods of a @Singular
// field: one value at a time, a whole collection, and a clear.
func (c *Checker) lombokSingular(b *ast.Class, builderType *ast.ClassType, f *ast.Field, setterPrefix string) {
	elems, kind := singularKind(f.Type)
	if kind == "" {
		c.errf(pos(), "TY-INT-0005", "@Singular needs a List or Map field, found %s", f.Type)
		return
	}
	this := func() ast.Expr { return &ast.This{ExprBase: ast.ExprBase{Pos: pos(), T: builderType}} }
	field := func() ast.Expr { return sel(this(), f.Name) }
	adder := singularAdderName(f, setterPrefix)
	clear := "clear" + strings.ToUpper(f.Name[:1]) + f.Name[1:]

	// the collection is created on first use, so an untouched builder passes an
	// empty one to the built object
	var mkElem ast.Expr
	if kind == "map" {
		mkElem = newObj(c.b.Boxes[ast.Int], nil) // placeholder, replaced below
		mkElem = c.newHashMapOf(elems)
	} else {
		mkElem = c.newArrayListOf(elems)
	}
	lazy := ifOf(isNull(field()), exprStmtOf(assignTo(field(), mkElem)), nil)

	if kind == "map" {
		// Builder key(K key, V value) { ...; this.f.put(key, value); return this }
		body := blockOf(
			lazy,
			exprStmtOf(callNamed(field(), "put", id("key"), id("value"))),
			returnOf(this()),
		)
		m := c.newSynthMethod(b, adder, ast.ModPublic, builderType, elems, []string{"key", "value"}, body, "")
		m.Anno = "@Singular"
		c.addSynthMethod(b, m)
	} else {
		// Builder value(E value) { ...; this.f.add(value); return this }
		body := blockOf(
			lazy,
			exprStmtOf(callNamed(field(), "add", id("value"))),
			returnOf(this()),
		)
		m := c.newSynthMethod(b, adder, ast.ModPublic, builderType, elems[:1], []string{"value"}, body, "")
		m.Anno = "@Singular"
		c.addSynthMethod(b, m)
	}
	// Builder addAllF(Collection<E> values) { ...; this.f.addAll(values); return this }
	pushName := "addAll"
	if kind == "map" {
		pushName = "putAll"
	}
	all := c.newSynthMethod(b, singularAllName(f, setterPrefix), ast.ModPublic, builderType, []ast.Type{f.Type}, []string{"values"},
		blockOf(
			lazy,
			exprStmtOf(callNamed(field(), pushName, id("values"))),
			returnOf(this()),
		), "")
	all.Anno = "@Singular"
	c.addSynthMethod(b, all)
	c.lombokSingularCopy(b, builderType, f)
	// Builder clearF() { this.f = null; return this }
	clr := c.newSynthMethod(b, clear, ast.ModPublic, builderType, nil, nil,
		blockOf(
			exprStmtOf(assignTo(field(), nullLit())),
			returnOf(this()),
		), "")
	clr.Anno = "@Singular"
	c.addSynthMethod(b, clr)
}

// singularCopyName is the builder helper that hands build() the collection of a
// @Singular field: its own copy, and never null.
func (c *Checker) singularCopyName(f *ast.Field) string {
	return "$" + f.Name
}

// lombokSingularCopy generates that helper on the builder.
func (c *Checker) lombokSingularCopy(b *ast.Class, builderType *ast.ClassType, f *ast.Field) {
	elems, kind := singularKind(f.Type)
	this := func() ast.Expr { return &ast.This{ExprBase: ast.ExprBase{Pos: pos(), T: builderType}} }
	field := func() ast.Expr { return sel(this(), f.Name) }
	var empty, copy ast.Expr
	if kind == "map" {
		empty, copy = c.newHashMapOf(elems), c.newMapCopy(elems, field())
	} else {
		empty, copy = c.newArrayListOf(elems), c.newListCopy(elems, field())
	}
	body := blockOf(
		ifOf(isNull(field()), blockOf(returnOf(empty)), nil),
		returnOf(copy),
	)
	m := c.newSynthMethod(b, c.singularCopyName(f), ast.ModPrivate, f.Type, nil, nil, body, "")
	m.Anno = "@Singular"
	c.addSynthMethod(b, m)
}

// newArrayListOf renders `new ArrayList<E>()`.
func (c *Checker) newArrayListOf(elems []ast.Type) ast.Expr {
	return c.newGenericOf(c.arrayListClass(), elems)
}

// newListCopy renders `new ArrayList<E>(source)`.
func (c *Checker) newListCopy(elems []ast.Type, source ast.Expr) ast.Expr {
	n := c.newGenericOf(c.arrayListClass(), elems).(*ast.New)
	n.Args = []ast.Expr{source}
	return n
}

// newHashMapOf renders `new HashMap<K, V>()`.
func (c *Checker) newHashMapOf(elems []ast.Type) ast.Expr {
	return c.newGenericOf(c.hashMapClass(), elems)
}

// newMapCopy renders `new HashMap<K, V>(source)`.
func (c *Checker) newMapCopy(elems []ast.Type, source ast.Expr) ast.Expr {
	n := c.newGenericOf(c.hashMapClass(), elems).(*ast.New)
	n.Args = []ast.Expr{source}
	return n
}

// newGenericOf renders `new Cls<A, B>(...)` for a class the standard library
// provides.
func (c *Checker) newGenericOf(cl *ast.Class, elems []ast.Type) ast.Expr {
	if cl == nil {
		return nullLit()
	}
	te := &ast.TypeExpr{Pos: pos(), Name: cl.Name, Resolved: &ast.ClassType{Class: cl, Args: elems}}
	return &ast.New{
		ExprBase: ast.ExprBase{Pos: pos(), T: &ast.ClassType{Class: cl, Args: elems}},
		Type:     te,
	}
}

// arrayListClass and hashMapClass find the standard library collections.
func (c *Checker) arrayListClass() *ast.Class { return c.global["teyru.ArrayList"] }
func (c *Checker) hashMapClass() *ast.Class   { return c.global["teyru.HashMap"] }

// lombokSuperBuilder supports @SuperBuilder: the builder of a subclass covers
// every field of the hierarchy, so a chain written against the subclass builds
// a complete object.
//
// Lombok does this with a builder hierarchy whose methods are typed by a
// self-referential type parameter. Teyru builds one flat builder instead: the
// subclass builder takes the inherited fields too and hands them to the
// subclass constructor, which passes the parent's share to the parent's
// constructor. The chain reads the same, and no generics are involved.
func (c *Checker) lombokSuperBuilder(cl *ast.Class, a *ast.Annotation, classAnnos []*ast.Annotation) {
	chain := c.superChain(cl)
	if len(chain) > 1 && !chain[0].Mods.Has(ast.ModAbstract) {
		// the root has to accept the fields the subclass builder passes up
		c.ensureAllArgsCtor(chain[0], true)
	}
	for i := 1; i < len(chain); i++ {
		c.ensureAllArgsCtor(chain[i-1], true)
	}
	c.ensureAllArgsCtor(cl, false)
	var own []*ast.Field
	for _, f := range c.hierarchyFields(cl) {
		if f.Decl != nil && f.Decl.Init != nil && f.Mods.Has(ast.ModFinal) {
			continue
		}
		own = append(own, f)
	}
	c.lombokBuilderFor(cl, a, classAnnos, own)
}

// superChain lists the classes from the root of the hierarchy down to cl.
func (c *Checker) superChain(cl *ast.Class) []*ast.Class {
	var chain []*ast.Class
	for k := cl; k != nil; k = astSuper(k) {
		chain = append([]*ast.Class{k}, chain...)
	}
	return chain
}

func astSuper(cl *ast.Class) *ast.Class {
	if cl.Super == nil || cl.Super.Class == nil || cl.Super.Class.Builtin {
		return nil
	}
	return cl.Super.Class
}

// ensureAllArgsCtor gives a class the constructor a subclass builder needs: one
// parameter per field of the whole hierarchy up to that class.
func (c *Checker) ensureAllArgsCtor(cl *ast.Class, protected bool) {
	fields := c.hierarchyFields(cl)
	for _, ctor := range cl.Ctors {
		if len(ctor.Params) == len(fields) {
			return
		}
	}
	mods := ast.ModPublic
	if protected {
		mods = ast.ModProtected
	}
	m := &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: mods, Result: ast.TVoid, Pos: pos()}
	var stmts []ast.Stmt
	sup := astSuper(cl)
	// chain to the parent with its share of the parameters
	if sup != nil {
		var args []ast.Expr
		for _, f := range c.hierarchyFields(sup) {
			args = append(args, id(f.Name))
		}
		if len(args) > 0 {
			stmts = append(stmts, exprStmtOf(superCall("<init>", args...)))
		}
	}
	for _, f := range fields {
		m.Params = append(m.Params, f.Type)
		m.ParamNames = append(m.ParamNames, f.Name)
		if !c.hierarchyFieldOf(sup, f) {
			stmts = append(stmts, exprStmtOf(assignTo(thisField(f), id(f.Name))))
		}
	}
	m.Body = blockOf(stmts...)
	c.addSynthCtor(cl, m)
}

// hierarchyFields lists the instance fields of a class and its ancestors, in the
// order a constructor takes them.
func (c *Checker) hierarchyFields(cl *ast.Class) []*ast.Field {
	var out []*ast.Field
	for _, k := range c.superChain(cl) {
		for _, f := range c.instanceAndStaticFields(k) {
			if f.Mods.Has(ast.ModStatic) || f.IsProp {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

func (c *Checker) hierarchyFieldOf(cl *ast.Class, f *ast.Field) bool {
	if cl == nil {
		return false
	}
	for _, k := range c.hierarchyFields(cl) {
		if k == f {
			return true
		}
	}
	return false
}

func stmtsFrom(b *ast.Block) []ast.Stmt {
	if b == nil {
		return nil
	}
	return b.Stmts
}

// newBuilderClass creates the nested builder type.
func (c *Checker) newBuilderClass(owner *ast.Class, name string) *ast.Class {
	cd := &ast.ClassDecl{Pos: pos(), Kind: ast.KindClass, Name: name, Mods: ast.ModPublic | ast.ModStatic}
	b := c.newClass(name, owner.Full+"$"+name, ast.KindClass)
	b.Decl = cd
	b.File = owner.File
	b.Owner = owner
	// The builder is a nested class of the annotated one, and Java grants a
	// nested class access to its nest's private members: a builder for a class
	// with a private constructor has to be able to call it, which is what the
	// nest test reads. It has no enclosing instance, so Inner stays false.
	b.Outer = owner
	b.Mods = cd.Mods
	b.Builtin = owner.Builtin
	cd.Sym = b
	b.Super = c.objType
	b.Resolved = true
	owner.Nested[name] = b
	c.addSynthCtor(b, &ast.Method{Name: "<init>", Owner: b, IsCtor: true, Mods: ast.ModPublic, Result: ast.TVoid, Pos: pos()})
	return b
}

// lombokMethodBuilder supports @Builder on a constructor or static method.
func (c *Checker) lombokMethodBuilder(cl *ast.Class, d *ast.MethodDecl, a *ast.Annotation) {
	builderName := annoString(a, "builderClassName")
	if builderName == "" {
		// Lombok names the builder after the *return type* of the target, so
		// `Box of(...)` in another class gets BoxBuilder, not ThatClassBuilder.
		builderName = cl.Name + "Builder"
		if d.Sym != nil {
			switch rt := d.Sym.Result.(type) {
			case *ast.ClassType:
				builderName = rt.Class.Name + "Builder"
			case *ast.PrimType:
				if rt.Kind == ast.Void {
					builderName = "VoidBuilder"
				}
			}
		}
	}
	buildName := annoString(a, "buildMethodName")
	if buildName == "" {
		buildName = "build"
	}
	factoryName := annoString(a, "builderMethodName")
	if factoryName == "" {
		factoryName = "builder"
	}
	if _, exists := cl.Nested[builderName]; exists {
		return
	}
	b := c.newBuilderClass(cl, builderName)
	var params []ast.Type
	var names []string
	for _, p := range d.Params {
		t := c.resolveType(c.classEnv(cl), p.Type)
		params = append(params, t)
		names = append(names, p.Name)
		bf := &ast.Field{Name: p.Name, Type: t, Mods: ast.ModPrivate, Pos: p.Pos, Storage: true, Owner: b}
		c.addSynthField(b, bf)
		body := blockOf(
			exprStmtOf(assignTo(sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, p.Name), id("value"))),
			returnOf(&ast.This{ExprBase: ast.ExprBase{Pos: pos(), T: &ast.ClassType{Class: b}}}),
		)
		bm := c.newSynthMethod(b, p.Name, ast.ModPublic, &ast.ClassType{Class: b}, []ast.Type{t}, []string{"value"}, body, "")
		bm.Anno = "@Builder"
		c.addSynthMethod(b, bm)
	}
	args := make([]ast.Expr, len(names))
	for i, n := range names {
		args[i] = sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, n)
	}
	var buildExpr ast.Expr
	if d.IsCtor {
		buildExpr = newObj(cl, args...)
	} else if d.Sym != nil && d.Sym.IsStatic() {
		// `Cl.method(args)`: the builder is a nested class, and an unqualified
		// call resolves inside it against the class it is nested in, which is
		// where the annotated method lives.
		buildExpr = callNew(id(cl.Full), d.Name, args...)
	} else {
		// An instance method is called on a fresh instance of the class that
		// declares it, which is what Lombok does: the builder has no enclosing
		// instance to call it on (it is created by a static factory).
		buildExpr = callNew(newObj(cl), d.Name, args...)
	}
	result := d.Sym.Result
	if d.IsCtor {
		result = &ast.ClassType{Class: cl, Args: typeVarArgs(cl)}
	}
	build := c.newSynthMethod(b, buildName, ast.ModPublic, result, nil, nil,
		blockOf(returnOf(buildExpr)), "")
	build.Anno = "@Builder"
	c.addSynthMethod(b, build)

	fac := c.newSynthMethod(cl, factoryName, ast.ModPublic|ast.ModStatic, &ast.ClassType{Class: b}, nil, nil,
		blockOf(returnOf(newObj(b))), "")
	fac.Anno = "@Builder"
	c.addSynthMethod(cl, fac)
	c.layout(b)
}

// ---------------------------------------------------------------- misc

func (c *Checker) lombokWith(cl *ast.Class, f *ast.Field, s onSite) {
	fields := c.ctorFields(cl, "all")
	var params []ast.Type
	var names []string
	for _, x := range fields {
		params = append(params, x.Type)
		names = append(names, x.Name)
	}
	args := make([]ast.Expr, len(fields))
	for i, x := range fields {
		if x == f {
			args[i] = id("value")
			continue
		}
		args[i] = sel(&ast.This{ExprBase: ast.ExprBase{Pos: pos()}}, x.Name)
	}
	m := c.newSynthMethod(cl, "with"+util.Capitalize(f.Name), ast.ModPublic, &ast.ClassType{Class: cl, Args: typeVarArgs(cl)},
		[]ast.Type{f.Type}, []string{"value"}, blockOf(returnOf(newObj(cl, args...))), "")
	m.Anno = "@With"
	c.placeOn(m, s.memberAnnos(), s.paramAnnos())
	c.addSynthMethod(cl, m)
}

func (c *Checker) lombokNonNullParams(d *ast.MethodDecl) {
	if d.Body == nil || d.Sym == nil {
		return
	}
	var pre []ast.Stmt
	for _, p := range d.Params {
		if hasAnno(p.Annos, "NonNull") == nil {
			continue
		}
		msg := p.Name + " is marked non-null but is null"
		pre = append(pre, ifOf(isNull(id(p.Name)),
			throwOf(newObj(c.b.NPE, strLit(msg))), nil))
	}
	d.Body.Stmts = append(pre, d.Body.Stmts...)
}

func (c *Checker) lombokSneakyThrows(d *ast.MethodDecl) {
	if d.Body == nil {
		return
	}
	cat := &ast.Catch{Pos: pos(), Types: []*ast.TypeExpr{{Pos: pos(), Name: "Throwable"}}, Name: "t", Body: blockOf(throwOf(id("t")))}
	t := &ast.Try{Pos: pos(), Body: d.Body, Catches: []*ast.Catch{cat}}
	d.Body = blockOf(t)
}

// ensureClInit gives a class a static initializer to hang a synthesized field's
// value on. resolveMembers creates one from a field that declares an
// initializer, but a synthesized field has no declaration to read, so a value
// put on InitExpr would never be written.
func (c *Checker) ensureClInit(cl *ast.Class) {
	if cl.ClInit == nil {
		cl.ClInit = &ast.Method{Name: "<clinit>", Owner: cl, Mods: ast.ModStatic,
			Result: ast.TVoid, Pos: pos(), SynthKind: "clinit"}
	}
}

// newObjectExpr builds `new Object()` for a field the checker synthesizes.
//
// It cannot go through newObj and the body checker: Lombok runs after the
// members pass, so nothing type-checks the expression it plants, and the
// emitter reads a New's type off the node itself -- with none set it emitted a
// bare NULL, which is how the lock field came out null.
func (c *Checker) newObjectExpr() *ast.New {
	ct := &ast.ClassType{Class: c.b.Object}
	return &ast.New{
		ExprBase: ast.ExprBase{Pos: pos(), T: ct},
		Type:     &ast.TypeExpr{Pos: pos(), Name: c.b.Object.Name, Resolved: ct},
	}
}

func (c *Checker) lombokSynchronized(cl *ast.Class, d *ast.MethodDecl) {
	if d.Body == nil {
		return
	}
	var lock ast.Expr = &ast.This{ExprBase: ast.ExprBase{Pos: pos()}}
	if d.Sym != nil && d.Sym.IsStatic() {
		lock = id("__lock$" + cl.Name)
		lf := cl.FieldMap["__lock$"+cl.Name]
		if lf == nil {
			lf = &ast.Field{Name: "__lock$" + cl.Name, Type: c.objType,
				Mods: ast.ModPrivate | ast.ModStatic | ast.ModFinal, Pos: pos(), Storage: true, Anno: "@Synchronized"}
			// Lombok writes `private static final Object $LOCK = new Object()`.
			// The field was left null here, which is not a lock: synchronized
			// throws NullPointerException on a null monitor (JLS 14.19), and
			// the block only appeared to work because entering it on address
			// zero went unchecked.
			lf.InitExpr = c.newObjectExpr()
			c.ensureClInit(cl)
			c.addSynthField(cl, lf)
		}
		d.Sym.SyncOn = lf
		lock = id(lf.Name)
	}
	d.Body = blockOf(&ast.Sync{Pos: pos(), Lock: lock, Body: d.Body})
}

// lombokLocked wraps the body in a synchronized block on a named field.
func (c *Checker) lombokLocked(cl *ast.Class, d *ast.MethodDecl, a *ast.Annotation) {
	if d.Body == nil {
		return
	}
	name := annoString(a, "value")
	if name == "" {
		name = "__lock$" + cl.Name
	}
	lf := cl.FieldMap[name]
	if lf == nil {
		lf = &ast.Field{Name: name, Type: c.objType,
			Mods: ast.ModPrivate | ast.ModStatic | ast.ModFinal, Pos: pos(), Storage: true, Anno: "@Locked"}
		// the same null monitor @Synchronized had; see lombokSynchronized
		lf.InitExpr = c.newObjectExpr()
		c.ensureClInit(cl)
		c.addSynthField(cl, lf)
	}
	d.Body = blockOf(&ast.Sync{Pos: pos(), Lock: id(name), Body: d.Body})
}

// lombokLog creates the `log` field for the @Log family.
func (c *Checker) lombokLog(cl *ast.Class, a *ast.Annotation) {
	if cl.FieldMap["log"] != nil {
		return
	}
	logCls := c.global["Logger"]
	if logCls == nil {
		return
	}
	f := &ast.Field{Name: "log", Type: &ast.ClassType{Class: logCls},
		Mods: ast.ModPrivate | ast.ModStatic | ast.ModFinal, Pos: pos(), Storage: true, Anno: "@" + a.Name}
	c.addSynthField(cl, f)
	if cl.ClInit == nil {
		cl.ClInit = &ast.Method{Name: "<clinit>", Owner: cl, Mods: ast.ModStatic, Result: ast.TVoid, Pos: pos(), SynthKind: "clinit"}
	}
	init := newObj(logCls, strLit(cl.Name))
	init.SetType(&ast.ClassType{Class: logCls})
	init.Ctor = c.simpleCtor(logCls, init.Args)
	f.InitExpr = init
}

func (c *Checker) lombokStandardException(cl *ast.Class) {
	strT := c.strType
	c.addSynthCtor(cl, &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPublic,
		Result: ast.TVoid, Pos: pos(), Body: blockOf()})
	c.addSynthCtor(cl, &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPublic,
		Result: ast.TVoid, Params: []ast.Type{strT}, ParamNames: []string{"message"},
		Body: blockOf(exprStmtOf(superCall("<init>", id("message")))), Pos: pos()})
	thr := &ast.ClassType{Class: c.b.Throwable}
	c.addSynthCtor(cl, &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPublic,
		Result: ast.TVoid, Params: []ast.Type{strT, thr}, ParamNames: []string{"message", "cause"},
		Body: blockOf(exprStmtOf(superCall("<init>", id("message"), id("cause")))), Pos: pos()})
	c.addSynthCtor(cl, &ast.Method{Name: "<init>", Owner: cl, IsCtor: true, Mods: ast.ModPublic,
		Result: ast.TVoid, Params: []ast.Type{thr}, ParamNames: []string{"cause"}, Pos: pos(),
		Body: blockOf(exprStmtOf(superCall("<init>",
			// Lombok's cause-only constructor copies the message out of the
			// cause: `super(cause)` leaves getMessage() null, which is what a
			// catch block printing the message then shows.
			&ast.Cond{ExprBase: ast.ExprBase{Pos: pos()},
				C: binop("!=", id("cause"), nullLit()),
				X: callNamed(id("cause"), "getMessage"),
				Y: nullLit()},
			id("cause"))))})
}

func (c *Checker) lombokFieldNameConstants(cl *ast.Class, a *ast.Annotation) {
	const name = "Fields"
	if _, exists := cl.Nested[name]; exists {
		return
	}
	// Lombok puts `level` on the generated inner type and leaves the constants
	// themselves public (`innerTypeName` is not read; the name is Fields).
	level, _, ok := annoAccessLevel(a, "level")
	if !ok {
		level = ast.ModPublic
	}
	cd := &ast.ClassDecl{Pos: pos(), Kind: ast.KindClass, Name: name, Mods: level | ast.ModStatic | ast.ModFinal}
	f := c.newClass(name, cl.Full+"$"+name, ast.KindClass)
	f.Decl = cd
	f.File = cl.File
	f.Owner = cl
	f.Mods = cd.Mods
	f.Builtin = cl.Builtin
	cd.Sym = f
	f.Super = c.objType
	f.Resolved = true
	cl.Nested[name] = f
	prefix := annoString(a, "prefix")
	for _, fld := range c.instanceAndStaticFields(cl) {
		cf := &ast.Field{Name: fld.Name, Type: c.strType,
			Mods: ast.ModPublic | ast.ModStatic | ast.ModFinal, Pos: pos(), Storage: true,
			Anno: "@FieldNameConstants"}
		cf.ConstVal = constValue{s: prefix + fld.Name, kind: ast.LitString, ok: true}
		cf.InitExpr = strLit(prefix + fld.Name)
		if f.ClInit == nil {
			f.ClInit = &ast.Method{Name: "<clinit>", Owner: f, Mods: ast.ModStatic,
				Result: ast.TVoid, Pos: pos(), SynthKind: "clinit"}
		}
		c.addSynthField(f, cf)
	}
	c.layout(f)
}

// lombokExtensionMethods records @ExtensionMethod classes for call rewriting.
func (c *Checker) lombokExtensionMethods(cl *ast.Class) []*ast.Class {
	cd := cl.Decl
	if cd == nil {
		return nil
	}
	a := hasAnno(cd.Annos, "ExtensionMethod")
	if a == nil {
		return nil
	}
	var out []*ast.Class
	for _, te := range annoClasses(a) {
		if x := c.resolveType(c.classEnv(cl), te); x != nil {
			if ct, ok := x.(*ast.ClassType); ok {
				out = append(out, ct.Class)
			}
		}
	}
	return out
}

// applyLombokToProgram runs the pass over every class, twice for nested
// builders that only appear once their owner is processed.
func (c *Checker) applyLombokToProgram() {
	generatedOn = map[*ast.Method]*onCopies{}
	seen := map[*ast.Class]bool{}
	for i := 0; i < 2; i++ {
		for _, cl := range append([]*ast.Class(nil), c.classes...) {
			if cl.Builtin || seen[cl] {
				continue
			}
			seen[cl] = true
			c.applyLombok(cl)
		}
	}
	c.extensions = map[*ast.Class][]*ast.Class{}
	for _, cl := range c.classes {
		if cl.Builtin {
			continue
		}
		if exts := c.lombokExtensionMethods(cl); len(exts) > 0 {
			c.extensions[cl] = exts
			for _, e := range cl.Subclasses {
				c.extensions[e] = exts
			}
		}
	}
}
