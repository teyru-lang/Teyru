package codegen

/* Per-component array classes.
 *
 * Every array value used to carry the one synthetic teyru.Array class, so
 * Class.isArray answered true for all of them and nothing could name int[] or
 * String[]: there was no component type to ask for, and Class.forName("[I") had
 * no class to answer with. Java gives every component type its own array class,
 * and that is what this file emits.
 *
 * What an array class is here is a tyclass with TY_CLS_ARRAY, a component
 * pointer (Class.getComponentType), a source-shaped name ("int[]",
 * "java.lang.String[]"), and the JVM's binary name ("[I", "[Lteyru.String;")
 * for Class.forName. It inherits the one array class's vtable, because every
 * array dispatches the same five methods, and its super is that one class, so
 * instanceof and casts to an array type keep their old answer.
 *
 * The cost is one class record per component type and nothing at run time. The
 * set is bounded the way reflectset.go bounds the member tables: the components
 * a program's reflection can reach (the classes the scan kept, plus the
 * primitives, Object and String), and the array types its own code names. A
 * program that never names an array type and never reflects emits none.
 *
 * The program's unit carries them whatever class the component belongs to.
 * The standard library's unit is compiled once and shared by every program, and
 * an array class a program names is not part of that unit; a member table of a
 * library class is written into the program's unit too (see enterTables), which
 * is where the array types in its signatures are named.
 */

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/util"
)

// arrayClassInfo is one emitted array class.
type arrayClassInfo struct {
	elem  ast.Type // the component, after erasure
	cname string   // the C identifier: arr_...
	full  string   // the binary name: "[I", "[Lteyru.String;"
	jname string   // the name a program reads: "int[]", "java.lang.String[]"
}

// eraseDeep follows type variables to their bound, as erasure does at run time.
// A type variable with no bound erases to Object, which is what an array of it
// is an array of.
func eraseDeep(t ast.Type) ast.Type {
	for {
		tv, ok := t.(*ast.TypeVarType)
		if !ok {
			return t
		}
		if tv.Var == nil || tv.Var.Bound == nil {
			return nil
		}
		t = tv.Var.Bound
	}
}

// arrayTypeKey identifies a component type across the spellings the checker
// leaves: an erased type variable and its bound are one key, so one array class
// serves both.
func arrayTypeKey(t ast.Type) string {
	switch tt := t.(type) {
	case *ast.PrimType:
		return "P" + strconv.Itoa(int(tt.Kind))
	case *ast.ClassType:
		if tt.Class == nil {
			return "?"
		}
		return "C" + tt.Class.Full
	case *ast.ArrayType:
		return "A" + arrayTypeKey(eraseDeep(tt.Elem))
	}
	return "?"
}

// arrayBinaryName is the name Class.forName parses: the JVM descriptor, built
// from the component's binary name.
func arrayBinaryName(elem ast.Type) string {
	switch tt := elem.(type) {
	case *ast.PrimType:
		return "[" + util.Descriptor(tt)
	case *ast.ClassType:
		if tt.Class == nil {
			return "[Ljava.lang.Object;"
		}
		return "[L" + tt.Class.Full + ";"
	case *ast.ArrayType:
		return "[" + arrayBinaryName(eraseDeep(tt.Elem))
	}
	return "[Ljava.lang.Object;"
}

// arrayReportedName is the name Class.getName answers with. Java's getName
// answers the descriptor and its getTypeName answers the source form; this
// runtime has one name, and the source form is the useful one, so an array is
// named after the class its component reports. A standard-library component
// reports its JDK name (decision D8), which is why String[] is
// "java.lang.String[]" while its binary name stays "[Lteyru.String;".
func arrayReportedName(elem ast.Type) string {
	switch tt := elem.(type) {
	case *ast.PrimType:
		return tt.String() + "[]"
	case *ast.ClassType:
		if tt.Class == nil {
			return "java.lang.Object[]"
		}
		return util.JavaName(tt.Class.Full) + "[]"
	case *ast.ArrayType:
		return arrayReportedName(eraseDeep(tt.Elem)) + "[]"
	}
	return "java.lang.Object[]"
}

// componentExpr is the C expression naming a component's class object.
func (e *Emitter) componentExpr(elem ast.Type) string {
	switch tt := elem.(type) {
	case *ast.PrimType:
		return "&cls_" + primClassName(tt.Kind)
	case *ast.ClassType:
		if tt.Class != nil {
			return "&cls_" + mangle(tt.Class.Full)
		}
	case *ast.ArrayType:
		// The component is itself an array type, and the class of that type is
		// the array class of its element.
		return "&" + e.arrayClassFor(tt.Elem)
	}
	return "&cls_" + mangle(e.prog.Builtins.Object.Full)
}

// arrayClassFor answers the C name of the array class of a component type,
// registering it the first time it is asked for. The declaration goes into the
// unit's declarations immediately; the definition is written after every class
// record, because it names the vtable and the component's class object, and
// neither is declared yet at the point the first array type is met.
func (e *Emitter) arrayClassFor(t ast.Type) string {
	elem := eraseDeep(t)
	if elem == nil {
		elem = &ast.ClassType{Class: e.prog.Builtins.Object}
	}
	key := arrayTypeKey(elem)
	if info, ok := e.arrayClasses[key]; ok {
		return "cls_" + info.cname
	}
	info := &arrayClassInfo{
		elem:  elem,
		full:  arrayBinaryName(elem),
		jname: arrayReportedName(elem),
	}
	info.cname = "arr_" + mangle(info.full)
	if e.arrayClasses == nil {
		e.arrayClasses = map[string]*arrayClassInfo{}
	}
	e.arrayClasses[key] = info
	e.arrayOrder = append(e.arrayOrder, info)
	// The standard library's unit is compiled once and linked into every
	// program, so its array classes are file-local: the program's unit defines
	// its own copy for the same component, and two external definitions of one
	// symbol would not link. A library reference and a program reference to
	// "String[]" are then two class objects; nothing in the library reads a
	// program's array class, and the program's ty_prog_array_of is the table a
	// program's reflection uses.
	if e.lib {
		// The declaration goes into the unit's data section rather than its
		// types: the types section becomes the shared header, and a static
		// declaration there would make every program's unit inherit a
		// file-local class it then cannot define.
		fmt.Fprintf(&e.own.data, "static tyclass cls_%s;\n", info.cname)
	} else {
		fmt.Fprintf(&e.own.types, "%styclass cls_%s;\n", e.declLink(), info.cname)
	}
	return "cls_" + info.cname
}

// registerArrayClass names an array class without using it, which is what a
// component the reflection scan reached needs: Array.newInstance and
// Class.forName can ask for it with no expression in the program naming its
// array type.
func (e *Emitter) registerArrayClass(elem ast.Type) {
	e.arrayClassFor(elem)
}

// registerReflectionArrays emits the array classes a reflecting program can ask
// for without naming: the primitives, Object and String, and every class the
// member tables cover. The scan already bounded that set; a program whose scan
// widened to every class pays one record per class, which is the same
// direction the member tables take.
func (e *Emitter) registerReflectionArrays() {
	for k := ast.Boolean; k <= ast.Double; k++ {
		e.registerArrayClass(&ast.PrimType{Kind: k})
	}
	if e.prog.Builtins.Object != nil {
		e.registerArrayClass(&ast.ClassType{Class: e.prog.Builtins.Object})
	}
	if e.prog.Builtins.String != nil {
		e.registerArrayClass(&ast.ClassType{Class: e.prog.Builtins.String})
	}
	for _, cl := range e.prog.Classes {
		if cl == nil {
			continue
		}
		// A class the member tables cover, or one forName can load by name:
		// Array.newInstance and a "[L...;" descriptor can ask for either.
		if !e.covers(cl) && !e.namesCover(cl) {
			continue
		}
		e.registerArrayClass(&ast.ClassType{Class: cl})
	}
}

// emitArrayClassDefs writes the records registered by arrayClassFor. They are
// written after the class metadata so the vtable and the component class
// object they name are already declared.
func (e *Emitter) emitArrayClassDefs() {
	if len(e.arrayOrder) == 0 {
		return
	}
	base := e.prog.ArrayClass()
	link := e.link
	if e.lib {
		link = "static "
	}
	super, nvt, vtable := "NULL", 0, "NULL"
	if base != nil {
		super = "&cls_" + mangle(base.Full)
		nvt = len(base.VTable)
		vtable = "vt_" + mangle(base.Full)
	}
	for _, info := range e.arrayOrder {
		fmt.Fprintf(&e.own.data, "%styclass cls_%s = {\n", link, info.cname)
		fmt.Fprintf(&e.own.data, "  .name = %q, .id = -1, .flags = TY_CLS_ARRAY,\n", info.full)
		fmt.Fprintf(&e.own.data, "  .jname = %q,\n", info.jname)
		fmt.Fprintf(&e.own.data, "  .super = %s, .nvt = %d, .vtable = %s,\n", super, nvt, vtable)
		fmt.Fprintf(&e.own.data, "  .mods = %d, .comp = %s,\n", jPrimClassMods, e.componentExpr(info.elem))
		e.own.data.WriteString("};\n")
	}
}

// emitArrayLookup writes the program's component-to-array-class table, which is
// what Class.forName("[I") and Array.newInstance(T.class, n) look through. It is
// a function-local static for the same reason the forName table is: a table at
// file scope that names every array class is a root, and link-time optimisation
// cannot drop it from a program that never asks.
func (e *Emitter) emitArrayLookup() {
	if e.lib {
		// The program's unit defines it; the standard library only calls the
		// runtime, which calls it.
		return
	}
	if len(e.arrayOrder) == 0 {
		e.own.code.WriteString("\ntyclass* ty_prog_array_of(tyclass* comp) {\n  (void)comp;\n  return NULL;\n}\n")
		return
	}
	var names []string
	for _, info := range e.arrayOrder {
		names = append(names, "&cls_"+info.cname)
	}
	fmt.Fprintf(&e.own.code, "\ntyclass* ty_prog_array_of(tyclass* comp) {\n  static tyclass* table[%d] = {%s};\n  for (int i = 0; i < %d; i++) if (table[i]->comp == comp) return table[i];\n  return NULL;\n}\n",
		len(names), strings.Join(names, ", "), len(names))
}
