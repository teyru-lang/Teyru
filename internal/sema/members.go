package sema

import (
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/internal/util"
)

func (c *Checker) addField(cl *ast.Class, f *ast.Field) {
	if prev := cl.FieldMap[f.Name]; prev != nil {
		c.errf(f.Pos, "TY-TYP-0010", "duplicate field %s in %s", f.Name, cl.Name)
		return
	}
	f.Owner = cl
	cl.FieldMap[f.Name] = f
	cl.Fields = append(cl.Fields, f)
}

// sortedSynthMethods lists a class's synthesized methods deterministically.
func sortedSynthMethods(cl *ast.Class) []*ast.Method {
	var out []*ast.Method
	for _, name := range sortedMethodNames(cl) {
		out = append(out, cl.Methods[name]...)
	}
	out = append(out, cl.Ctors...)
	return out
}

func (c *Checker) addMethod(cl *ast.Class, m *ast.Method) {
	m.Owner = cl
	m.VIndex = -1
	m.Selector = -1
	tolerate := m.Decl != nil && hasAnno(m.Decl.Annos, "Tolerate") != nil
	for _, prev := range cl.Methods[m.Name] {
		if tolerate || prev.Tolerate {
			continue
		}
		if sameErasedParams(c, prev, m) {
			c.errf(m.Pos, "TY-TYP-0011", "duplicate method %s in %s", describeMethod(m), cl.Name)
			return
		}
	}
	cl.Methods[m.Name] = append(cl.Methods[m.Name], m)
}

func describeMethod(m *ast.Method) string {
	var parts []string
	for _, p := range m.Params {
		parts = append(parts, p.String())
	}
	name := m.Name
	if m.IsCtor {
		name = m.Owner.Name
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

func sameErasedParams(c *Checker, a, b *ast.Method) bool {
	if len(a.Params) != len(b.Params) {
		return false
	}
	for i := range a.Params {
		if !sameType(c.erasure(a.Params[i]), c.erasure(b.Params[i])) {
			return false
		}
	}
	return true
}

func (c *Checker) methodEnv(cl *ast.Class, tps []*ast.TypeParam, m *ast.Method) *typeEnv {
	env := c.classEnv(cl)
	if len(tps) == 0 {
		return env
	}
	menv := &typeEnv{cls: cl, tvars: map[string]*ast.TypeVar{}, parent: env, file: cl.File}
	for _, tp := range tps {
		tv := &ast.TypeVar{Name: tp.Name, ID: c.tvID}
		c.tvID++
		tp.Sym = tv
		menv.tvars[tp.Name] = tv
		m.TypeParams = append(m.TypeParams, tv)
	}
	for _, tp := range tps {
		if len(tp.Bounds) > 0 {
			for _, b := range tp.Bounds {
				tp.Sym.Bounds = append(tp.Sym.Bounds, c.resolveType(menv, b))
			}
			tp.Sym.Bound = tp.Sym.Bounds[0]
		} else {
			tp.Sym.Bound = c.objType
			tp.Sym.Bounds = []ast.Type{c.objType}
		}
	}
	return menv
}

func (c *Checker) resolveMembers(cl *ast.Class) {
	cd := cl.Decl
	if cd == nil {
		return
	}
	env := c.classEnv(cl)
	isIface := cl.IsInterface()

	if cd.Kind == ast.KindRecord {
		var ptypes []ast.Type
		var pnames []string
		for _, rc := range cd.RecordComps {
			t := c.resolveType(env, rc.Type)
			f := &ast.Field{Name: rc.Name, Type: t, Mods: ast.ModPrivate | ast.ModFinal, Pos: rc.Pos, Storage: true}
			c.addField(cl, f)
			ptypes = append(ptypes, t)
			pnames = append(pnames, rc.Name)
		}
		// accessors unless declared explicitly
		for _, rc := range cd.RecordComps {
			if hasMethodDecl(cd, rc.Name, 0) {
				continue
			}
			f := cl.FieldMap[rc.Name]
			m := &ast.Method{Name: rc.Name, Mods: ast.ModPublic, Result: f.Type, Pos: rc.Pos, SynthKind: "record-get", Prop: f}
			c.addMethod(cl, m)
		}
		// canonical constructor unless declared (non-compact) with same signature
		canon := true
		for _, mem := range cd.Members {
			if md, ok := mem.(*ast.MethodDecl); ok && md.IsCtor && !md.Compact && len(md.Params) == len(ptypes) {
				same := true
				for i, p := range md.Params {
					if !sameType(c.resolveType(env, p.Type), ptypes[i]) {
						same = false
					}
				}
				if same {
					canon = false
				}
			}
		}
		if canon {
			var compact *ast.MethodDecl
			for _, mem := range cd.Members {
				if md, ok := mem.(*ast.MethodDecl); ok && md.Compact {
					compact = md
				}
			}
			m := &ast.Method{Name: "<init>", IsCtor: true, Mods: ast.ModPublic, Params: ptypes, ParamNames: pnames, Result: ast.TVoid, Pos: cd.Pos, SynthKind: "record-ctor", Decl: compact}
			if compact != nil {
				compact.Sym = m
				m.Mods = compact.Mods
			}
			c.addCtor(cl, m)
		}
		for _, name := range []string{"toString", "hashCode", "equals"} {
			n := 0
			if name == "equals" {
				n = 1
			}
			if hasMethodDecl(cd, name, n) {
				continue
			}
			m := &ast.Method{Name: name, Mods: ast.ModPublic, Pos: cd.Pos, SynthKind: "record-" + name}
			switch name {
			case "toString":
				m.Result = c.strType
			case "hashCode":
				m.Result = ast.TInt
			case "equals":
				m.Result = ast.TBoolean
				m.Params = []ast.Type{c.objType}
				m.ParamNames = []string{"o"}
			}
			c.addMethod(cl, m)
		}
	}

	if cd.Kind == ast.KindEnum {
		self := &ast.ClassType{Class: cl}
		for i, ec := range cd.EnumConsts {
			f := &ast.Field{Name: ec.Name, Type: self, Mods: ast.ModPublic | ast.ModStatic | ast.ModFinal, Pos: ec.Pos, Storage: true, EnumOrd: i}
			c.addField(cl, f)
			cl.EnumConsts = append(cl.EnumConsts, f)
			if ec.Body != nil {
				ec.Body.Name = cl.Name + "$" + ec.Name
				sub := c.declareClass(cl.File, ec.Body, cl)
				delete(cl.Nested, ec.Body.Name)
				sub.Anon = true
				sub.Inner = false
				sub.Mods |= ast.ModStatic | ast.ModFinal
				sub.Super = self
				sub.Resolved = true
				cl.Subclasses = append(cl.Subclasses, sub)
				cl.Mods &^= ast.ModFinal
				c.resolveMembers(sub)
			}
		}
		vals := &ast.Method{Name: "values", Mods: ast.ModPublic | ast.ModStatic, Result: &ast.ArrayType{Elem: self}, Pos: cd.Pos, SynthKind: "enum-values"}
		c.addMethod(cl, vals)
		vo := &ast.Method{Name: "valueOf", Mods: ast.ModPublic | ast.ModStatic, Result: self, Params: []ast.Type{c.strType}, ParamNames: []string{"name"}, Pos: cd.Pos, SynthKind: "enum-valueOf"}
		c.addMethod(cl, vo)
	}

	for _, mem := range cd.Members {
		switch d := mem.(type) {
		case *ast.FieldDecl:
			c.resolveFieldDecl(cl, env, d, isIface)
		case *ast.MethodDecl:
			if d.Compact {
				continue
			}
			m := &ast.Method{Name: d.Name, Mods: d.Mods, Decl: d, Pos: d.Pos, IsCtor: d.IsCtor}
			d.Sym = m
			menv := c.methodEnv(cl, d.TypeParams, m)
			if d.IsCtor {
				m.Result = ast.TVoid
				if cd.Kind == ast.KindEnum {
					m.Mods = (m.Mods &^ (ast.ModPublic | ast.ModProtected)) | ast.ModPrivate
				}
			} else {
				m.Result = c.resolveType(menv, d.Result)
			}
			for i, p := range d.Params {
				t := c.resolveType(menv, p.Type)
				m.Params = append(m.Params, t)
				m.ParamNames = append(m.ParamNames, p.Name)
				m.ParamAnnos = append(m.ParamAnnos, p.Annos)
				if p.Varargs {
					if i != len(d.Params)-1 {
						c.errf(p.Pos, "TY-TYP-0012", "varargs parameter must be last")
					}
					m.Varargs = true
				}
			}
			if isIface && !d.IsCtor {
				if d.Body == nil {
					if !m.Mods.Has(ast.ModStatic) && !m.Mods.Has(ast.ModPrivate) {
						m.Mods |= ast.ModAbstract
					}
				} else if !m.Mods.Has(ast.ModStatic) && !m.Mods.Has(ast.ModPrivate) && !m.Mods.Has(ast.ModDefault) {
					c.errf(d.Pos, "TY-TYP-0013", "interface method with a body must be default, static or private")
				}
				if !m.Mods.Has(ast.ModPrivate) {
					m.Mods |= ast.ModPublic
				}
			}
			if d.Body == nil && !m.Mods.Has(ast.ModAbstract) && !m.Mods.Has(ast.ModNative) {
				c.errf(d.Pos, "TY-TYP-0014", "method %s needs a body", d.Name)
			}
			if d.Body != nil && (m.Mods.Has(ast.ModAbstract) || m.Mods.Has(ast.ModNative)) {
				c.errf(d.Pos, "TY-TYP-0015", "abstract or native method %s cannot have a body", d.Name)
			}
			if m.Mods.Has(ast.ModAbstract) && !isIface && !cl.Mods.Has(ast.ModAbstract) && cd.Kind != ast.KindEnum {
				c.errf(d.Pos, "TY-TYP-0016", "abstract method %s in non-abstract class %s", d.Name, cl.Name)
			}
			if m.Mods.Has(ast.ModNative) {
				// A native method has no Teyru body: the C symbol is either one
				// the runtime provides (prelude classes) or one the program
				// links against. `teyru build --native-header` prints the exact
				// signatures to implement.
				m.Native = nativeName(cl, m)
				m.External = !cl.Builtin
			}
			if cd.Implicit && !d.IsCtor && d.Name != "main" {
				m.Mods |= ast.ModStatic
			}
			// In a compact source file every member is a static member of the
			// implicit class, main included, whether or not it takes the
			// `String[] args` parameter. Leaving main non-static delegated the
			// entry point to codegen's instance-main path, which does not
			// compile the String[] form (emit.go's `_main_obj` value cast).
			if cd.Implicit && d.Name == "main" {
				m.Mods |= ast.ModStatic
			}
			if d.IsCtor {
				c.addCtor(cl, m)
			} else {
				c.addMethod(cl, m)
			}
		case *ast.ClassDecl, *ast.InitBlock:
		}
	}

	if cl.Inner && cl.OuterField == nil {
		f := &ast.Field{Name: "this$0", Type: &ast.ClassType{Class: cl.Outer}, Mods: ast.ModPrivate | ast.ModFinal, Pos: cd.Pos, Storage: true}
		cl.OuterField = f
		// the outer instance must come first so every subclass shares the slot
		c.addField(cl, f)
		if len(cl.Fields) > 1 {
			copy(cl.Fields[1:], cl.Fields[:len(cl.Fields)-1])
			cl.Fields[0] = f
		}
	}
	if cd.Kind == ast.KindEnum && cl.ClInit == nil && len(cd.EnumConsts) > 0 {
		cl.ClInit = &ast.Method{Name: "<clinit>", Owner: cl, Mods: ast.ModStatic, Result: ast.TVoid, Pos: cd.EnumConsts[0].Pos, SynthKind: "clinit"}
	}
	for _, mem := range cd.Members {
		if ib, ok := mem.(*ast.InitBlock); ok && ib.Static && cl.ClInit == nil {
			cl.ClInit = &ast.Method{Name: "<clinit>", Owner: cl, Mods: ast.ModStatic, Result: ast.TVoid, Pos: ib.Pos, SynthKind: "clinit"}
		}
	}
	for _, f := range cl.Fields {
		if f.Mods.Has(ast.ModStatic) && f.Decl != nil && f.Decl.Init != nil && cl.ClInit == nil {
			cl.ClInit = &ast.Method{Name: "<clinit>", Owner: cl, Mods: ast.ModStatic, Result: ast.TVoid, Pos: f.Pos, SynthKind: "clinit"}
		}
	}
	if len(cl.Ctors) == 0 && !isIface {
		mods := ast.ModPublic
		if cd.Kind == ast.KindEnum {
			mods = ast.ModPrivate
		}
		m := &ast.Method{Name: "<init>", IsCtor: true, Mods: mods, Result: ast.TVoid, Pos: cd.Pos, SynthKind: "default-ctor"}
		c.addCtor(cl, m)
	}
	if cd.Implicit {
		for _, fs := range cl.Fields {
			fs.Mods |= ast.ModStatic
		}
	}
	if cd.Kind == ast.KindEnum {
		c.enumBodyCtors(cl)
	}
	c.checkNativeSymbols(cl)
}

// enumBodyCtors gives every enum constant that declares a body the constructor
// Java gives it: the enum's own signature, forwarded to the enum's constructor.
//
// A constant's body is a subclass, and it is declared while the constants are
// being walked -- before the enum's own constructors have been resolved from
// the members after the `:` -- so it was handed the ordinary synthesized
// no-argument constructor. That one then chained to a constructor that has
// parameters and passed none of them, which is not C that compiles:
//
//	enum Op { ADD(1) { ... } : Op(int code) { ... } }
//
// The test programs had no enum with both a body and constructor arguments,
// which is why nothing caught it.
func (c *Checker) enumBodyCtors(cl *ast.Class) {
	if len(cl.Ctors) == 0 {
		return
	}
	target := cl.Ctors[0]
	for _, sub := range cl.Subclasses {
		if !sub.Anon {
			continue
		}
		var kept []*ast.Method
		for _, k := range sub.Ctors {
			if k.SynthKind != "default-ctor" {
				kept = append(kept, k)
			}
		}
		sub.Ctors = kept
		if len(kept) > 0 {
			continue
		}
		c.addCtor(sub, &ast.Method{
			Name: "<init>", IsCtor: true, Mods: ast.ModPrivate,
			Result: ast.TVoid, Pos: target.Pos, SynthKind: "anon-ctor", Forward: target,
			Params: target.Params, ParamNames: target.ParamNames,
		})
	}
}

// checkNativeSymbols rejects two native methods of one class that would have to
// share a single C symbol: the linker would bind both to one definition and one
// of the overloads would silently run the other's code.
//
// The encodings nativeParam produces are injective for the types the language
// has today, so this is a backstop rather than a path a program is expected to
// reach: it is what keeps a future type, or a class whose name reads like an
// array descriptor (`A[]` and a class called `AA` both encode to `AA`), from
// turning into a wrong answer at run time.
func (c *Checker) checkNativeSymbols(cl *ast.Class) {
	seen := map[string]*ast.Method{}
	var all []*ast.Method
	for _, name := range sortedMethodNames(cl) {
		all = append(all, cl.Methods[name]...)
	}
	all = append(all, cl.Ctors...)
	for _, m := range all {
		if m == nil || !m.Mods.Has(ast.ModNative) || m.Native == "" {
			continue
		}
		if prev, ok := seen[m.Native]; ok {
			c.errf(m.Pos, "TY-TYP-0097", "native methods %s and %s both need the C symbol %s",
				describeMethod(prev), describeMethod(m), m.Native)
			continue
		}
		seen[m.Native] = m
	}
}

func (c *Checker) addCtor(cl *ast.Class, m *ast.Method) {
	m.Owner = cl
	m.VIndex = -1
	m.Selector = -1
	for _, prev := range cl.Ctors {
		if sameErasedParams(c, prev, m) {
			c.errf(m.Pos, "TY-TYP-0011", "duplicate constructor %s", describeMethod(m))
			return
		}
	}
	cl.Ctors = append(cl.Ctors, m)
}

func hasMethodDecl(cd *ast.ClassDecl, name string, nparams int) bool {
	for _, mem := range cd.Members {
		if md, ok := mem.(*ast.MethodDecl); ok && md.Name == name && len(md.Params) == nparams && !md.IsCtor {
			return true
		}
	}
	return false
}

// nativeName is the C symbol the runtime must provide for a native method.
//
// The symbol has to tell two overloads apart, so the parameter descriptors are
// part of it; nativeParam renders one parameter.
func nativeName(cl *ast.Class, m *ast.Method) string {
	var b strings.Builder
	b.WriteString("tyn_")
	b.WriteString(util.Mangle(cl.Full))
	b.WriteByte('_')
	b.WriteString(util.Mangle(m.Name))
	for _, p := range m.Params {
		b.WriteByte('_')
		b.WriteString(nativeParam(p))
	}
	return b.String()
}

// nativeParam is the symbol component of one parameter type. The rule is
// util.ParamDescriptor's: an array carries its element descriptor after the `A`,
// because a bare `A` for every array is not injective -- `size(int[])` and
// `size(A)` were both tyn_Foo_size_A, so two overloads shared one C function
// and one of them ran the other's code. Primitive, class and type-variable
// parameters keep their encoding, so the symbols tests/native and
// docs/native.md prescribe for those still hold; the table in docs/native.md
// has to say `A` plus the element descriptor for arrays.
func nativeParam(t ast.Type) string { return util.ParamDescriptor(t) }

func (c *Checker) resolveFieldDecl(cl *ast.Class, env *typeEnv, d *ast.FieldDecl, isIface bool) {
	if d.Type.Name == "var" || d.Type.Name == "val" {
		c.errf(d.Type.Pos, "TY-TYP-0018", "'%s' is only allowed for local variables; fields need an explicit type", d.Type.Name)
	}
	base := c.resolveType(env, d.Type)
	for _, vd := range d.Vars {
		t := base
		for i := 0; i < vd.Dims; i++ {
			t = &ast.ArrayType{Elem: t}
		}
		mods := d.Mods
		if isIface {
			mods |= ast.ModPublic | ast.ModStatic | ast.ModFinal
		}
		f := &ast.Field{Name: vd.Name, Type: t, Mods: mods, Pos: vd.Pos, Decl: vd, Storage: true, Annos: d.Annos}
		vd.Fld = f
		if d.Accessor != nil {
			c.resolveProperty(cl, f, d, vd)
		}
		c.addField(cl, f)
	}
}

func (c *Checker) resolveProperty(cl *ast.Class, f *ast.Field, d *ast.FieldDecl, vd *ast.VarDeclarator) {
	f.IsProp = true
	if len(d.Vars) > 1 {
		c.errf(d.Pos, "TY-PROP-0001", "a property declaration must declare exactly one name")
	}
	vis := d.Mods & (ast.ModPublic | ast.ModPrivate | ast.ModProtected)
	f.Mods = (d.Mods &^ (ast.ModPublic | ast.ModProtected)) | ast.ModPrivate
	needStorage := vd.Init != nil
	var hasGet, hasSet bool
	for _, acc := range d.Accessor {
		if acc.Body == nil {
			needStorage = true
		} else if blockMentionsField(acc.Body) {
			needStorage = true
		}
		if acc.IsSet {
			if hasSet {
				c.errf(acc.Pos, "TY-PROP-0002", "duplicate set accessor")
				continue
			}
			hasSet = true
			needStorage = needStorage || acc.Body == nil
		} else {
			if hasGet {
				c.errf(acc.Pos, "TY-PROP-0002", "duplicate get accessor")
				continue
			}
			hasGet = true
		}
		mods := acc.Mods & (ast.ModPublic | ast.ModPrivate | ast.ModProtected)
		if mods == 0 {
			mods = vis
		}
		mods |= d.Mods & ast.ModStatic
		m := &ast.Method{Mods: mods, Pos: acc.Pos, Accessor: acc, Prop: f}
		acc.Sym = m
		if acc.IsSet {
			m.Name = "set" + util.Capitalize(f.Name)
			m.Result = ast.TVoid
			m.Params = []ast.Type{f.Type}
			m.ParamNames = []string{acc.ParamName}
			f.Setter = m
		} else {
			prefix := "get"
			if ast.IsPrim(f.Type, ast.Boolean) {
				prefix = "is"
			}
			m.Name = prefix + util.Capitalize(f.Name)
			m.Result = f.Type
			f.Getter = m
		}
		c.addMethod(cl, m)
	}
	f.Storage = needStorage
	if !needStorage {
		for _, bad := range []ast.Mods{ast.ModFinal, ast.ModVolatile, ast.ModTransient} {
			if d.Mods.Has(bad) {
				c.errf(d.Pos, "TY-PROP-0003", "computed property %s cannot use storage modifiers", f.Name)
				break
			}
		}
	}
	if d.Mods.Has(ast.ModFinal) && hasSet {
		c.errf(d.Pos, "TY-PROP-0004", "final property %s cannot declare a setter", f.Name)
	}
}

func blockMentionsField(b *ast.Block) bool {
	found := false
	walkBlock(b, func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && id.Name == "field" {
			found = true
		}
	})
	return found
}

// ---------------------------------------------------------------- layout

func (c *Checker) layout(cl *ast.Class) {
	if cl.Laidout {
		c.assignSelectors(cl)
		return
	}
	cl.Laidout = true
	if cl.Super != nil {
		c.layout(cl.Super.Class)
		cl.InstFields = append([]*ast.Field(nil), cl.Super.Class.InstFields...)
		cl.VTable = append([]*ast.Method(nil), cl.Super.Class.VTable...)
	}
	for _, i := range cl.Ifaces {
		c.layout(i.Class)
	}
	c.addInstanceFields(cl)
	if !cl.IsInterface() {
		if cl.Special == "Object" {
			for _, nm := range []string{"toString", "hashCode", "equals"} {
				for _, m := range cl.Methods[nm] {
					if len(m.Params) != 0 && nm != "equals" {
						continue
					}
					if m.IsStatic() || m.Mods.Has(ast.ModPrivate) {
						continue
					}
					m.VIndex = len(cl.VTable)
					cl.VTable = append(cl.VTable, m)
				}
			}
		}
		for _, name := range sortedMethodNames(cl) {
			for _, m := range cl.Methods[name] {
				if m.IsStatic() || m.Mods.Has(ast.ModPrivate) || m.IsCtor {
					continue
				}
				placed := false
				for i, sm := range cl.VTable {
					if sm == m {
						m.VIndex = i
						placed = true
						break
					}
					if sm.Name == m.Name && c.overrides(cl, m, sm) {
						m.VIndex = i
						m.Overrides = sm
						sm.Overridden = true
						markOverridden(sm)
						cl.VTable[i] = m
						placed = true
						break
					}
				}
				if !placed {
					m.VIndex = len(cl.VTable)
					cl.VTable = append(cl.VTable, m)
				}
			}
		}
	}
	c.assignSelectors(cl)
}

func markOverridden(m *ast.Method) {
	seen := map[*ast.Method]bool{}
	for x := m.Overrides; x != nil && !seen[x]; x = x.Overrides {
		seen[x] = true
		x.Overridden = true
	}
}

// addInstanceFields appends storage fields (including capture fields added later).
func (c *Checker) addInstanceFields(cl *ast.Class) {
	have := map[*ast.Field]bool{}
	for _, f := range cl.InstFields {
		have[f] = true
	}
	for _, f := range cl.Fields {
		if f.Mods.Has(ast.ModStatic) || !f.Storage || have[f] {
			continue
		}
		f.Index = len(cl.InstFields)
		cl.InstFields = append(cl.InstFields, f)
	}
}

func (c *Checker) assignSelectors(cl *ast.Class) {
	if !cl.IsInterface() {
		return
	}
	for _, name := range sortedMethodNames(cl) {
		for _, m := range cl.Methods[name] {
			if m.IsStatic() || m.Mods.Has(ast.ModPrivate) || m.Selector >= 0 {
				continue
			}
			m.Selector = c.selector
			c.selector++
		}
	}
}

func sortedMethodNames(cl *ast.Class) []string {
	var names []string
	for n := range cl.Methods {
		names = append(names, n)
	}
	sortStrings(names)
	// preserve declaration order where possible for readability
	if cl.Decl != nil {
		order := map[string]int{}
		i := 0
		for _, mem := range cl.Decl.Members {
			switch d := mem.(type) {
			case *ast.MethodDecl:
				if _, ok := order[d.Name]; !ok {
					order[d.Name] = i
					i++
				}
			}
		}
		stableSortBy(names, func(a, b string) bool {
			oa, okA := order[a]
			ob, okB := order[b]
			if okA && okB {
				return oa < ob
			}
			return okA && !okB
		})
	}
	return names
}

// overrides reports whether m (declared in cl) overrides sm.
func (c *Checker) overrides(cl *ast.Class, m, sm *ast.Method) bool {
	if len(m.Params) != len(sm.Params) || sm.IsStatic() || sm.Mods.Has(ast.ModPrivate) {
		return false
	}
	sup := c.asSuper(&ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, sm.Owner)
	for i := range m.Params {
		pt := sm.Params[i]
		if sup != nil {
			pt = c.subst(pt, bindings(sm.Owner, sup.Args))
		}
		if !sameType(c.erasure(m.Params[i]), c.erasure(pt)) && !sameType(c.erasure(m.Params[i]), c.erasure(sm.Params[i])) {
			return false
		}
	}
	return true
}

func typeVarArgs(cl *ast.Class) []ast.Type {
	var out []ast.Type
	for _, tv := range cl.TypeParams {
		out = append(out, &ast.TypeVarType{Var: tv})
	}
	return out
}

// Implementation finds the concrete method invoked for interface method im on class cl.
func (c *Checker) Implementation(cl *ast.Class, im *ast.Method) *ast.Method {
	for k := cl; k != nil; {
		for _, m := range k.Methods[im.Name] {
			if !m.IsStatic() && !m.IsCtor && !m.Mods.Has(ast.ModAbstract) && (m == im || c.overridesIface(cl, m, im)) {
				return m
			}
		}
		if k.Super == nil {
			break
		}
		k = k.Super.Class
	}
	// default methods
	var found *ast.Method
	c.eachInterface(cl, func(i *ast.Class) bool {
		for _, m := range i.Methods[im.Name] {
			if m.Mods.Has(ast.ModDefault) && (m == im || c.overridesIface(cl, m, im)) {
				if found == nil || c.isSubInterface(m.Owner, found.Owner) {
					found = m
				}
			}
		}
		return true
	})
	return found
}

func (c *Checker) isSubInterface(a, b *ast.Class) bool {
	res := false
	c.eachInterface(a, func(i *ast.Class) bool {
		if i == b && a != b {
			res = true
			return false
		}
		return true
	})
	return res
}

func (c *Checker) overridesIface(cl *ast.Class, m, im *ast.Method) bool {
	if len(m.Params) != len(im.Params) {
		return false
	}
	sup := c.asSuper(&ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, im.Owner)
	for i := range m.Params {
		pt := im.Params[i]
		mt := m.Params[i]
		if sup != nil {
			pt = c.subst(pt, bindings(im.Owner, sup.Args))
		}
		if m.Owner != cl {
			if ms := c.asSuper(&ast.ClassType{Class: cl, Args: typeVarArgs(cl)}, m.Owner); ms != nil {
				mt = c.subst(mt, bindings(m.Owner, ms.Args))
			}
		}
		if !sameType(c.erasure(mt), c.erasure(pt)) && !sameType(c.erasure(m.Params[i]), c.erasure(im.Params[i])) {
			return false
		}
	}
	return true
}

// eachInterface visits all superinterfaces of cl (transitively, including via superclasses).
func (c *Checker) eachInterface(cl *ast.Class, fn func(*ast.Class) bool) {
	seen := map[*ast.Class]bool{}
	var visit func(k *ast.Class) bool
	visit = func(k *ast.Class) bool {
		if k == nil {
			return true
		}
		for _, i := range k.Ifaces {
			if seen[i.Class] {
				continue
			}
			seen[i.Class] = true
			if !fn(i.Class) || !visit(i.Class) {
				return false
			}
		}
		if k.Super != nil {
			return visit(k.Super.Class)
		}
		return true
	}
	if cl.IsInterface() {
		seen[cl] = true
		if !fn(cl) {
			return
		}
	}
	visit(cl)
}

// AllInterfaces returns every interface implemented by cl.
func (c *Checker) AllInterfaces(cl *ast.Class) []*ast.Class {
	var out []*ast.Class
	c.eachInterface(cl, func(i *ast.Class) bool {
		out = append(out, i)
		return true
	})
	return out
}

func (c *Checker) checkAbstracts(cl *ast.Class) {
	if cl.IsInterface() || cl.Mods.Has(ast.ModAbstract) || cl.Decl == nil {
		return
	}
	pos := cl.Decl.Pos
	for _, m := range cl.VTable {
		if m.Mods.Has(ast.ModAbstract) {
			// enum constants with bodies implement abstract enum methods
			if cl.Kind == ast.KindEnum && len(cl.Subclasses) > 0 {
				continue
			}
			c.errf(pos, "TY-TYP-0019", "%s must implement abstract method %s from %s", className(cl), describeMethod(m), m.Owner.Name)
			return
		}
	}
	c.eachInterface(cl, func(i *ast.Class) bool {
		for _, name := range sortedMethodNames(i) {
			for _, im := range i.Methods[name] {
				if im.IsStatic() || im.Mods.Has(ast.ModPrivate) || !im.Mods.Has(ast.ModAbstract) {
					continue
				}
				if impl := c.Implementation(cl, im); impl == nil {
					if c.objectHas(im) {
						continue
					}
					if cl.Kind == ast.KindEnum && len(cl.Subclasses) > 0 {
						continue
					}
					c.errf(pos, "TY-TYP-0019", "%s must implement %s from %s", className(cl), describeMethod(im), i.Name)
					return false
				}
			}
		}
		return true
	})
}

func (c *Checker) objectHas(im *ast.Method) bool {
	for _, m := range c.b.Object.Methods[im.Name] {
		if len(m.Params) == len(im.Params) {
			return true
		}
	}
	return false
}

func className(cl *ast.Class) string {
	if cl.Anon {
		return "anonymous class"
	}
	return cl.Name
}

// posOf is a helper for synthesized nodes.
func posOf(cl *ast.Class) source.Pos {
	if cl.Decl != nil {
		return cl.Decl.Pos
	}
	return source.Pos{}
}
