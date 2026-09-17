package codegen

import (
	"fmt"

	"github.com/teyru-lang/Teyru/internal/ast"
)

// staticName is the C global holding a class's static field.
func staticName(cl *ast.Class, f *ast.Field) string {
	return "G_" + mangle(cl.Full) + "_" + mangle(f.Name)
}

// emitStaticFields declares the C globals for a class's static fields.
func (e *Emitter) emitStaticFields(cl *ast.Class) {
	for _, f := range cl.Fields {
		if !f.Mods.Has(ast.ModStatic) {
			continue
		}
		ct := e.ctype(f.Type)
		if e.split && !e.lib && e.sideLib {
			// A standard library global is defined by the library's unit and
			// named from the program's through the header.
			return
		}
		fmt.Fprintf(e.data, "%s%s %s = %s;\n", e.link, ct, staticName(cl, f), zeroOf(ct))
		if e.split && e.lib {
			e.headerDecls += fmt.Sprintf("extern %s %s;\n", ct, staticName(cl, f))
		}
	}
}

// clinitRefs lists the static field addresses the collector has to treat as
// roots. The address of the field itself is registered, because the collector
// reads *(void**)address to find the object: one extra level of indirection
// would make it mark the address of the global instead of what the global
// points at, and an object reachable only from a static would be collected.
//
// Only reference-typed fields are registered. A primitive static holds no
// pointer, and the collector reads a whole pointer word from every address it
// is handed: registering an int32 global made it read four bytes of whatever
// global followed (found by running the test programs under AddressSanitizer).
func (e *Emitter) clinitRefs(prelude bool) string {
	var b []byte
	for _, cl := range e.prog.Classes {
		// One translation unit holds both halves, so there is nothing to
		// separate: the split build is the one that registers the standard
		// library's roots from the library's own unit (see emitLibraryInstall).
		if e.split && cl.Prelude() != prelude {
			continue
		}
		for _, f := range cl.Fields {
			if f.Mods.Has(ast.ModStatic) && e.isRef(f.Type) {
				b = append(b, []byte("\tty_gc_register_static((void*)&"+staticName(cl, f)+");\n")...)
			}
		}
	}
	return string(b)
}

// emitClInit writes a class's static initializer.
func (e *Emitter) emitClInit(cl *ast.Class) {
	if cl.ClInit == nil {
		return
	}
	m := cl.ClInit
	fmt.Fprintf(e.fns, "%s%s;\n", e.link, e.signature(m))
	e.indent = 0
	restoreFrame := e.frameReset()
	defer restoreFrame()
	fmt.Fprintf(e.code, "%s%s {\n", e.link, e.signature(m))
	e.indent++
	if cl.Super != nil {
		e.line("ty_clinit(&cls_%s);\n", mangle(cl.Super.Class.Full))
	}
	if cl.Decl != nil {
		for _, mem := range cl.Decl.Members {
			fd, ok := mem.(*ast.FieldDecl)
			if !ok {
				continue
			}
			for _, vd := range fd.Vars {
				f := vd.Fld
				if f == nil || !f.Mods.Has(ast.ModStatic) || vd.Init == nil {
					continue
				}
				e.line("%s = %s;\n", staticName(cl, f), e.coerce(e.expr(vd.Init), vd.Init.GetType(), f.Type))
			}
		}
		for _, mem := range cl.Decl.Members {
			if ib, ok := mem.(*ast.InitBlock); ok && ib.Static {
				e.emitBlockInner(ib.Body)
			}
		}
	}
	// synthesized static initializers (@Log and friends)
	for _, f := range cl.Fields {
		if f.InitExpr != nil && f.Mods.Has(ast.ModStatic) && (f.Decl == nil || f.Decl.Init == nil) {
			e.line("%s = %s;\n", staticName(cl, f), e.coerce(e.expr(f.InitExpr), f.InitExpr.GetType(), f.Type))
		}
	}
	e.emitEnumInit(cl)
	e.indent--
	e.code.WriteString("}\n\n")
}

// emitEnumInit materialises the enum constants.
func (e *Emitter) emitEnumInit(cl *ast.Class) {
	if cl.Kind != ast.KindEnum {
		return
	}
	for i, ec := range cl.Decl.EnumConsts {
		f := cl.FieldMap[ec.Name]
		if f == nil {
			continue
		}
		cls := cl
		for _, sub := range cl.Subclasses {
			if sub.Name == cl.Name+"$"+ec.Name {
				cls = sub
			}
		}
		g := staticName(cl, f)
		e.line("cts_%s[%d] = (void*)%s;\n", mangle(cl.Full), i, g)
		e.line("%s = (%s)ty_alloc(sizeof(%s));\n", g, e.ctype(f.Type), cname(cls))
		e.line("%s->obj.cls = &cls_%s;\n", g, mangle(cls.Full))
		e.line("((tyEnumBase*)%s)->ordinal = %d;\n", g, f.EnumOrd)
		e.line("((tyEnumBase*)%s)->name = ty_str_intern(%s);\n", g, e.cstr(ec.Name))
		// The constant's arguments go to the enum's own constructor -- the one
		// declared after the `:`. Nothing called it, so `P(1)` dropped the 1
		// and `P.v()` answered 0 where javac answers 1. A constant with a body
		// is a subclass, and its synthesized constructor is a no-argument one
		// that forwards to the enum's with nothing, so it is the enum's
		// constructor that is called here for both shapes: the fields of a
		// subclass start where the superclass's do, which is the same cast the
		// rest of the backend makes.
		// A constant with a body is a subclass, so its own constructor is the
		// one to call; it forwards the arguments to the enum's.
		ctor := enumCtor(cls, len(ec.Args))
		if ctor == nil {
			ctor = enumCtor(cl, len(ec.Args))
		}
		if ctor != nil {
			// the receiver is a fresh object this body just allocated, so it is
			// the constant's own arguments that sequence here
			ops := []seqOperand{{text: "(" + cname(cls) + "*)" + g}}
			for i, a := range ec.Args {
				var want ast.Type
				if i < len(ctor.Params) {
					want = ctor.Params[i]
				}
				ops = append(ops, seqOperand{x: a, text: e.coerce(e.expr(a), a.GetType(), want)})
			}
			e.line("%s;\n", e.callTo(ops, e.cfunc(ctor)+"(", ")"))
		}
	}
	// reflection's table, filled once every constant exists: the address of a
	// constant is not known until it has been allocated, so a write beside the
	// allocation would store the null the static started as.
	for i, ec := range cl.Decl.EnumConsts {
		if f := cl.FieldMap[ec.Name]; f != nil {
			e.line("cts_%s[%d] = (void*)%s;\n", mangle(cl.Full), i, staticName(cl, f))
		}
	}
}

// enumCtor picks the constructor an enum constant with n arguments calls.
func enumCtor(cl *ast.Class, n int) *ast.Method {
	for _, c := range cl.Ctors {
		if len(c.Params) == n {
			return c
		}
	}
	return nil
}
