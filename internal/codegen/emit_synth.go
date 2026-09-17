package codegen

import (
	"fmt"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// nativeKey identifies a native (runtime-implemented) method precisely.
func nativeKey(m *ast.Method) string {
	return m.Owner.Name + "." + util.KeySignature(m.Name, m.Params)
}

// emitSynthetic writes the C body of a compiler-synthesized method.
// forNameKey is Class.forName's compiled form. It is not a native method with a
// binding like the rest of reflection: its table has to be written inside its
// own body, which is a shape a binding cannot express.
const forNameKey = "Class.forNameOf(String)"

func (e *Emitter) emitSynthetic(cl *ast.Class, m *ast.Method) {
	e.indent = 0
	fmt.Fprintf(e.code, "%s%s {\n", e.link, e.signature(m))
	e.indent++
	e.stackCheck()
	if nativeKey(m) == forNameKey {
		// Class.forName searches the program's classes. The table cannot live at
		// file scope -- see forNameTable -- so it is written here, in the body
		// of the one function that reads it.
		table, count := e.forNameTable()
		if e.split {
			// The table is the program's unit's, and this body -- the standard
			// library's -- reaches it through a call, because a table naming
			// the program's classes cannot be part of a unit compiled once and
			// shared by every program.
			e.line("return (C_%s*)%s(a0, &cls_%s);\n",
				mangle(m.Owner.Full), table, mangle(m.Owner.Full))
		} else {
			e.line("return (C_%s*)ty_class_forname_in(a0, %s, %d, &cls_%s);\n",
				mangle(m.Owner.Full), table, count, mangle(m.Owner.Full))
		}
		e.indent--
		e.code.WriteString("}\n\n")
		return
	}
	if nf, ok := nativeTable[nativeKey(m)]; ok {
		args := make([]string, 0, len(m.Params))
		for i := range m.Params {
			args = append(args, fmt.Sprintf("a%d", i))
		}
		recv := ""
		if !m.IsStatic() {
			recv = "this"
		}
		// nativeInlineCall is where the receiver cast, the appended class and
		// the helper's own prototype are applied, so a synthesized native and a
		// call site in a prelude body reach the runtime the same way.
		call := e.nativeInlineCall(nf, m, recv, args)
		if m.Result == ast.TVoid {
			e.line("(void)%s;\n", call)
		} else {
			e.line("return %s;\n", call)
		}
		e.indent--
		e.code.WriteString("}\n\n")
		return
	}
	switch m.SynthKind {
	case "record-get":
		e.line("return this->f_%s;\n", mangle(m.Prop.Name))
	case "record-ctor":
		e.emitRecordAssign(cl, m)
		e.line("ty_clinit(&cls_%s);\n", mangle(e.prog.Builtins.Record.Full))
	case "record-toString":
		e.recordToString(cl)
	case "record-hashCode":
		e.recordHashCode(cl)
	case "record-equals":
		e.recordEquals(cl)
	case "enum-values":
		e.enumValues(cl, m)
	case "enum-valueOf":
		e.enumValueOf(cl, m)
	case "default-ctor":
		e.emitCtorBody(cl, m)
	case "":
		if m.Accessor != nil {
			store := e.storageOf(m.Prop)
			if m.Accessor.IsSet {
				e.line("%s = a0;\n", store)
			} else {
				e.line("return %s;\n", store)
			}
		} else if m.Mods.Has(ast.ModNative) {
			// A native method of a built-in class has no entry in the native
			// table, so nothing implements it. An empty body would return
			// whatever the register happened to hold -- teyru.Double.compareTo
			// returned a pointer-sized number and teyru.Byte.valueOf a Bad
			// pointer -- so fail with the method's name instead.
			e.line("ty_unimplemented(%s);\n", e.cstr(cl.Full+"."+m.Name))
		} else {
			e.line("/* empty */\n")
		}
	default:
		if strings.HasPrefix(m.SynthKind, "array-") {
			e.arrayMethod(m)
			break
		}
		e.line("/* synthetic %s */\n", m.SynthKind)
	}
	e.indent--
	e.code.WriteString("}\n\n")
}

func (e *Emitter) arrayMethod(m *ast.Method) {
	switch m.SynthKind {
	case "array-tostring":
		e.line("return ty_str_intern(\"[array]\");\n")
	case "array-hashcode":
		e.line("return 0;\n")
	case "array-equals":
		e.line("return (void*)this == (void*)a0;\n")
	case "array-clone":
		e.line("return (void*)this;\n")
	}
}

func (e *Emitter) recordToString(cl *ast.Class) {
	var parts []string
	for _, rc := range cl.Decl.RecordComps {
		f := cl.FieldMap[rc.Name]
		v := e.stringOperand(&ast.Select{ExprBase: ast.ExprBase{Pos: rc.Pos, T: f.Type}, X: &ast.This{}, Name: rc.Name})
		_ = v
		parts = append(parts, fmt.Sprintf("ty_str_concat(ty_str_intern(%s), %s)", e.cstr(rc.Name+"="), e.fieldString(f)))
	}
	expr := "ty_str_intern(" + e.cstr(cl.Name+"[") + ")"
	for i, p := range parts {
		if i > 0 {
			expr = "ty_str_concat(" + expr + ", ty_str_intern(\", \"))"
		}
		expr = "ty_str_concat(" + expr + ", " + p + ")"
	}
	e.line("return ty_str_concat(%s, ty_str_intern(\"]\"));\n", expr)
}

func (e *Emitter) fieldString(f *ast.Field) string {
	if e.isStringType(f.Type) {
		return "((tystr*)this->f_" + mangle(f.Name) + ")"
	}
	if p, ok := f.Type.(*ast.PrimType); ok {
		switch p.Kind {
		case ast.Boolean:
			return "ty_str_of_bool(this->f_" + mangle(f.Name) + ")"
		case ast.Char:
			return "ty_str_of_char(this->f_" + mangle(f.Name) + ")"
		case ast.Double, ast.Float:
			return "ty_str_of_double((double)this->f_" + mangle(f.Name) + ")"
		default:
			return "ty_str_of_long((int64_t)this->f_" + mangle(f.Name) + ")"
		}
	}
	return "ty_str_of_obj((tyobj*)this->f_" + mangle(f.Name) + ")"
}

func (e *Emitter) recordHashCode(cl *ast.Class) {
	e.line("int32_t _h = 1;\n")
	for _, rc := range cl.Decl.RecordComps {
		f := cl.FieldMap[rc.Name]
		n := "this->f_" + mangle(f.Name)
		if p, ok := f.Type.(*ast.PrimType); ok {
			switch p.Kind {
			case ast.Boolean:
				e.line("_h = 31 * _h + (%s ? 1231 : 1237);\n", n)
			case ast.Char, ast.Byte, ast.Short, ast.Int:
				e.line("_h = 31 * _h + (int32_t)%s;\n", n)
			case ast.Long:
				e.line("_h = 31 * _h + (int32_t)(%s ^ ((uint64_t)%s >> 32));\n", n, n)
			case ast.Double:
				// Java hashes a floating component through the wrapper's own
				// hashCode, which is the bits with every NaN collapsed to one
				// value -- the fold the runtime used to do, now written in the
				// prelude beside Double.hashCode(double).
				e.line("_h = 31 * _h + %s;\n", e.wrapperCall(e.wrapperStatic(ast.Double, "hashCode"), n))
			case ast.Float:
				e.line("_h = 31 * _h + %s;\n", e.wrapperCall(e.wrapperStatic(ast.Float, "hashCode"), n))
			}
		} else {
			e.line("_h = 31 * _h + ((%s) ? ((int32_t(*)(void*))((tyobj*)%s)->cls->vtable[1])((void*)%s) : 0);\n", n, n, n)
		}
	}
	e.line("return _h;\n")
}

func (e *Emitter) recordEquals(cl *ast.Class) {
	e.line("if (this == (%s*)a0) return 1;\n", cname(cl))
	e.line("if (a0 == NULL || !ty_instanceof((tyobj*)a0, &cls_%s)) return 0;\n", mangle(cl.Full))
	e.line("%s* _o = (%s*)a0;\n", cname(cl), cname(cl))
	for _, rc := range cl.Decl.RecordComps {
		f := cl.FieldMap[rc.Name]
		n := "this->f_" + mangle(f.Name)
		on := "_o->f_" + mangle(f.Name)
		if _, ok := f.Type.(*ast.PrimType); ok {
			e.line("if (%s != %s) return 0;\n", n, on)
		} else if e.isStringType(f.Type) {
			e.line("if (!ty_str_eq((tystr*)%s, (tystr*)%s)) return 0;\n", n, on)
		} else {
			e.line("if (!ty_obj_equal((tyobj*)%s, (tyobj*)%s)) return 0;\n", n, on)
		}
	}
	e.line("return 1;\n")
}

func (e *Emitter) enumValues(cl *ast.Class, m *ast.Method) {
	var vals []string
	for _, f := range cl.EnumConsts {
		vals = append(vals, "((void*)G_"+mangle(cl.Full)+"_"+mangle(f.Name)+")")
	}
	e.line("tyarr* _a = ty_array_new(%d, 8);\n", len(vals))
	e.line("_a->refs = 1;\n")
	for i, v := range vals {
		e.line("((void**)_a->data)[%d] = %s;\n", i, v)
	}
	e.line("return _a;\n")
}

func (e *Emitter) enumValueOf(cl *ast.Class, m *ast.Method) {
	for _, f := range cl.EnumConsts {
		e.line("if (ty_str_eq((tystr*)a0, ((tyEnumBase*)G_%s_%s)->name)) return G_%s_%s;\n",
			mangle(cl.Full), mangle(f.Name), mangle(cl.Full), mangle(f.Name))
	}
	e.line("ty_throw((tyobj*)ty_illarg(%s));\n", e.cstr("No enum constant"))
	e.line("return NULL;\n")
}
