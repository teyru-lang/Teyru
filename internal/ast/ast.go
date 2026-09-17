// Package ast defines the Teyru syntax tree together with the resolved
// symbols and types that semantic analysis attaches to it.
package ast

import (
	"strings"

	"github.com/teyru-lang/Teyru/internal/source"
)

// Modifier flags.
type Mods uint32

const (
	ModPublic Mods = 1 << iota
	ModPrivate
	ModProtected
	ModStatic
	ModFinal
	ModAbstract
	ModNative
	ModSynchronized
	ModTransient
	ModVolatile
	ModDefault
	ModSealed
	ModNonSealed
	ModStrictfp
)

// Has reports whether m contains f.
func (m Mods) Has(f Mods) bool { return m&f != 0 }

// Annotation is a parsed annotation. Teyru keeps the arguments so that
// annotation-driven code generation (Lombok compatibility) can read them.
type Annotation struct {
	Pos  source.Pos
	Name string // simple or qualified name as written
	Args []*AnnoArg
}

// AnnoArg is one annotation argument, either `value` or `name = value`.
type AnnoArg struct {
	Name  string
	Value Expr        // literal, class literal, enum constant, array, or nil
	Anno  *Annotation // when the value is a nested annotation
}

// Arg returns the named argument, or the single value-form argument.
func (a *Annotation) Arg(name string) *AnnoArg {
	for _, x := range a.Args {
		if x.Name == name {
			return x
		}
	}
	return nil
}

// Value returns the value-form argument, if any.
func (a *Annotation) Value() *AnnoArg {
	for _, x := range a.Args {
		if x.Name == "" {
			return x
		}
	}
	return nil
}

// Is reports whether the annotation is one of the given names, matching either
// the simple name or the fully qualified one.
func (a *Annotation) Is(names ...string) bool {
	simple := a.Name
	if i := strings.LastIndexByte(simple, '.'); i >= 0 {
		simple = simple[i+1:]
	}
	for _, n := range names {
		if a.Name == n || simple == n {
			return true
		}
	}
	return false
}

// TypeExpr is a syntactic type reference.
type TypeExpr struct {
	Pos      source.Pos
	Name     string // primitive keyword, "var", "val", or qualified name
	Args     []*TypeExpr
	Dims     int
	Wildcard int // 0 none, 1 ?, 2 ? extends, 3 ? super
	Bound    *TypeExpr
	Resolved Type
}

// File is a compilation unit.
type File struct {
	Src *source.File
	// Package is the file's package identity. For a file of a module it is the
	// import path of the directory rather than the name the file declares, so
	// that two modules may each declare `package util` and stay apart.
	Package string
	// DeclaredPackage is the name the file writes in its `package` clause, kept
	// because the identity above is not always it: an import names a package by
	// the name its author wrote.
	DeclaredPackage string
	Imports         []*Import
	Types           []*ClassDecl
	StaticImports   []*Field
	StaticMethods   map[string][]*Method
	StarImports     []string
}

// Import declaration.
type Import struct {
	Pos    source.Pos
	Path   string
	Static bool
	Star   bool
}

// ClassKind distinguishes type declaration forms.
type ClassKind int

const (
	KindClass ClassKind = iota
	KindInterface
	KindEnum
	KindRecord
	KindAnnotation
)

// TypeParam is a generic parameter declaration.
type TypeParam struct {
	Pos    source.Pos
	Name   string
	Bounds []*TypeExpr
	Sym    *TypeVar
}

// ClassDecl declares a class, interface, enum or record.
type ClassDecl struct {
	Pos         source.Pos
	Kind        ClassKind
	Mods        Mods
	Annos       []*Annotation
	Name        string
	TypeParams  []*TypeParam
	Extends     []*TypeExpr
	Implements  []*TypeExpr
	Permits     []*TypeExpr
	RecordComps []*Param
	EnumConsts  []*EnumConst
	Members     []Member
	Sym         *Class
	Implicit    bool // compact source file wrapper class
}

// EnumConst is an enum constant.
type EnumConst struct {
	Pos  source.Pos
	Name string
	Args []Expr
	Body *ClassDecl
}

// Member is a class body declaration.
type Member interface{ memberNode() }

// FieldDecl declares fields or a native property.
type FieldDecl struct {
	Pos      source.Pos
	Mods     Mods
	Annos    []*Annotation
	Type     *TypeExpr
	Vars     []*VarDeclarator
	Accessor []*Accessor // non-nil for properties
}

// Accessor is a property get/set accessor.
type Accessor struct {
	Pos       source.Pos
	Mods      Mods
	IsSet     bool
	ParamName string
	Body      *Block // nil for default accessor
	Sym       *Method
	Prop      *Field
}

// VarDeclarator is `name [dims] [= init]`.
type VarDeclarator struct {
	Pos  source.Pos
	Name string
	Dims int
	Init Expr
	Sym  *Var   // locals
	Fld  *Field // fields
}

// Param is a method, lambda or pattern parameter.
type Param struct {
	Pos     source.Pos
	Mods    Mods
	Type    *TypeExpr // nil for implicitly typed lambda parameters
	Name    string
	Varargs bool
	Annos   []*Annotation
	Sym     *Var
	// Decomp holds the component patterns of a record pattern (JEP 440).
	Decomp []*Param
	// Unnamed marks `_`, which binds nothing (JEP 456).
	Unnamed bool
	// Comps holds the record components a record pattern destructures into.
	Comps []*Field
}

// RecordComps lists a record's components in declaration order.
func (c *Class) RecordComps() []*Field {
	var out []*Field
	if c.Decl == nil {
		return nil
	}
	for _, rc := range c.Decl.RecordComps {
		if f := c.FieldMap[rc.Name]; f != nil {
			out = append(out, f)
		}
	}
	return out
}

// MethodDecl declares a method or constructor.
type MethodDecl struct {
	Pos        source.Pos
	Mods       Mods
	Annos      []*Annotation
	TypeParams []*TypeParam
	Result     *TypeExpr // nil for constructors
	Name       string
	Params     []*Param
	Throws     []*TypeExpr
	Body       *Block
	IsCtor     bool
	Compact    bool // compact record constructor
	// Default is an annotation element's default value, the expression after
	// `default` in `@interface X { int n() default 0 }`. It is kept because a
	// reader of the annotation at run time has to answer with it: an element a
	// use did not write answers its default, which is what Java requires.
	Default Expr
	Sym     *Method
}

// InitBlock is an instance or static initializer.
type InitBlock struct {
	Pos    source.Pos
	Static bool
	Body   *Block
}

func (*FieldDecl) memberNode()  {}
func (*MethodDecl) memberNode() {}
func (*InitBlock) memberNode()  {}
func (*ClassDecl) memberNode()  {}

// ---------------------------------------------------------------- statements

// Stmt is a statement.
type Stmt interface{ stmtNode() }

type (
	Block struct {
		Pos   source.Pos
		End   source.Pos
		Stmts []Stmt
	}
	LocalVar struct {
		Pos   source.Pos
		Mods  Mods
		Annos []*Annotation
		Type  *TypeExpr // Name "var"/"val" for inference
		Vars  []*VarDeclarator
	}
	LocalClass struct {
		Decl *ClassDecl
		// Instance and InstanceInit are the instance an @Helper local class is
		// used through: Lombok declares one right below the class and lets the
		// statements after it call the class's methods unqualified.
		Instance     *Var
		InstanceInit *New
	}
	ExprStmt struct {
		Pos source.Pos
		X   Expr
	}
	If struct {
		Pos  source.Pos
		Cond Expr
		Then Stmt
		Else Stmt
	}
	While struct {
		Pos  source.Pos
		Cond Expr
		Body Stmt
	}
	DoWhile struct {
		Pos  source.Pos
		Body Stmt
		Cond Expr
	}
	For struct {
		Pos    source.Pos
		Init   []Stmt
		Cond   Expr
		Update []Expr
		Body   Stmt
	}
	ForEach struct {
		Pos  source.Pos
		Var  *Param // Type.Name may be var/val
		X    Expr
		Body Stmt
		// resolved
		Iterable bool
		Elem     Type
	}
	Return struct {
		Pos source.Pos
		X   Expr
	}
	Break struct {
		Pos   source.Pos
		Label string
	}
	Continue struct {
		Pos   source.Pos
		Label string
	}
	Throw struct {
		Pos source.Pos
		X   Expr
	}
	Try struct {
		Pos       source.Pos
		Resources []Stmt // LocalVar or ExprStmt
		Body      *Block
		Catches   []*Catch
		Finally   *Block
	}
	Catch struct {
		Pos     source.Pos
		Types   []*TypeExpr
		Name    string
		Unnamed bool // `catch (T _)`
		Body    *Block
		Sym     *Var
	}
	Switch struct {
		Pos   source.Pos
		X     Expr
		Cases []*Case
		Arrow bool
		// resolved
		Kind SwitchKind
		// Exhaustive is set for a pattern switch whose patterns cover every
		// value of the selector (JLS 14.11.2). Constant labels are not counted,
		// so an enum switch with every constant and no default is not
		// exhaustive: javac requires the return after it, and so does this
		// checker.
		Exhaustive bool
	}
	Yield struct {
		Pos source.Pos
		X   Expr
	}
	Labeled struct {
		Pos   source.Pos
		Label string
		Body  Stmt
	}
	Assert struct {
		Pos  source.Pos
		Cond Expr
		Msg  Expr
	}
	Sync struct {
		Pos  source.Pos
		Lock Expr
		Body *Block
	}
	Empty struct {
		Pos source.Pos
	}
)

// SwitchKind is the selector category resolved by sema.
type SwitchKind int

const (
	SwitchInt SwitchKind = iota
	SwitchString
	SwitchEnum
	SwitchType
)

// Case is a switch case; Default is set for `default`.
type Case struct {
	Pos     source.Pos
	Null    bool // `case null`
	Labels  []Expr
	Pattern *Param // type pattern `case Type name`
	Default bool
	Guard   Expr
	Body    []Stmt // colon form statements, or arrow body (single ExprStmt/Block/Throw)
	ArrowX  Expr   // arrow form expression body
	// Arrow records that the case was written with `->`, which the emitter needs
	// to know: an arrow case never falls into the next one. The expression form
	// says so by having an ArrowX, but a block and a throw both land in Body
	// looking exactly like the statements of a colon case.
	Arrow bool
}

func (*Block) stmtNode()      {}
func (*LocalVar) stmtNode()   {}
func (*LocalClass) stmtNode() {}
func (*ExprStmt) stmtNode()   {}
func (*If) stmtNode()         {}
func (*While) stmtNode()      {}
func (*DoWhile) stmtNode()    {}
func (*For) stmtNode()        {}
func (*ForEach) stmtNode()    {}
func (*Return) stmtNode()     {}
func (*Break) stmtNode()      {}
func (*Continue) stmtNode()   {}
func (*Throw) stmtNode()      {}
func (*Try) stmtNode()        {}
func (*Switch) stmtNode()     {}
func (*Yield) stmtNode()      {}
func (*Labeled) stmtNode()    {}
func (*Assert) stmtNode()     {}
func (*Sync) stmtNode()       {}
func (*Empty) stmtNode()      {}

// --------------------------------------------------------------- expressions

// Expr is an expression.
type Expr interface {
	GetPos() source.Pos
	GetType() Type
	SetType(Type)
}

// ExprBase carries position and resolved type.
type ExprBase struct {
	Pos source.Pos
	T   Type
}

func (e *ExprBase) GetPos() source.Pos { return e.Pos }
func (e *ExprBase) GetType() Type      { return e.T }
func (e *ExprBase) SetType(t Type)     { e.T = t }

// LitKind is a literal kind.
type LitKind int

const (
	LitInt LitKind = iota
	LitLong
	LitFloat
	LitDouble
	LitChar
	LitString
	LitBool
	LitNull
)

type (
	Literal struct {
		ExprBase
		Kind LitKind
		Int  uint64
		Flt  float64
		Str  string
		Bool bool
	}
	// Ident is a simple name; resolved to local, field, property, or type.
	Ident struct {
		ExprBase
		Name string
		Ref  any // *Var, *Field, *Class
	}
	// Select is `X.Name`; resolved to field, property, array length, or type/package.
	Select struct {
		ExprBase
		X    Expr
		Name string
		Ref  any  // *Field, *Class, "length"
		Raw  bool // property backing storage access (contextual `field`)
	}
	Index struct {
		ExprBase
		X     Expr
		Index Expr
	}
	Call struct {
		ExprBase
		Recv     Expr // nil for unqualified
		Name     string
		TypeArgs []*TypeExpr
		Args     []Expr
		Super    bool   // super.m(...)
		Qual     string // Interface.super.m(...) names the super interface
		// resolved
		Method   *Method
		Static   bool
		RecvType Type
		ThisCtor bool // this(...) / super(...) constructor chaining
	}
	New struct {
		ExprBase
		Type  *TypeExpr
		Args  []Expr
		Body  *ClassDecl
		Outer Expr
		Ctor  *Method
	}
	NewArray struct {
		ExprBase
		Elem  *TypeExpr
		Dims  []Expr
		Extra int
		Init  *ArrayInit
	}
	ArrayInit struct {
		ExprBase
		Elems []Expr
		Elem  Type
	}
	Unary struct {
		ExprBase
		Op      string // + - ! ~ ++pre --pre post++ post--
		X       Expr
		Postfix bool
	}
	Binary struct {
		ExprBase
		Op   string
		X, Y Expr
		// resolved operand type after promotion
		OpType Type
	}
	Assign struct {
		ExprBase
		Op     string // = += ...
		X, Y   Expr
		OpType Type
		// TargetPrim is the type the target's value is read and written at:
		// the unboxed type of a boxed target, and the target's own type when
		// it is already primitive. It is what a back end unboxes a boxed
		// compound-assignment target to and boxes the result back from
		// (JLS 15.26.2 with 5.1.8 and 5.1.7).
		TargetPrim Type
	}
	Cond struct {
		ExprBase
		C, X, Y Expr
	}
	Cast struct {
		ExprBase
		Type *TypeExpr
		X    Expr
	}
	InstanceOf struct {
		ExprBase
		X       Expr
		Type    *TypeExpr
		Binding *Param
	}
	Lambda struct {
		ExprBase
		Params []*Param
		Body   any // Expr or *Block
		// resolved
		Iface    *Method // functional interface method
		Captures []*Var
		CapThis  bool
		Class    *Class // synthesized closure class
		// Outer is the lambda this one is written inside, when it is nested.
		// A body that needs the enclosing instance makes every lambda around
		// it carry the reference, so the chain from the use outwards is what
		// says which closures capture it.
		Outer *Lambda
		// ExprStmt marks a body that is a statement expression and whose
		// functional method returns void (JLS 15.27.2).
		ExprStmt bool
		// PreCaptures holds captures registered before the body is checked,
		// such as the receiver of a bound method reference.
		PreCaptures []*Var
		// RecvVar and RecvExpr hold the receiver of a bound method reference
		// that has to be evaluated once, at the reference itself.
		RecvVar  *Var
		RecvExpr Expr
	}
	MethodRef struct {
		ExprBase
		X     Expr      // receiver expression or type
		TypeX *TypeExpr // when the left side is a type (incl. arrays)
		Name  string    // method name or "new"
		Lam   *Lambda   // desugared lambda
	}
	This struct {
		ExprBase
		Qual string
		Var  *Var
	}
	SuperExpr struct {
		ExprBase
	}
	SwitchExpr struct {
		ExprBase
		S *Switch
	}
	ClassLit struct {
		ExprBase
		Type *TypeExpr
	}
	// Conv is a conversion inserted by sema (boxing, unboxing, widening).
	Conv struct {
		ExprBase
		X Expr
	}
)

// Synthetic node for a boxing/unboxing/primitive conversion.

// ---------------------------------------------------------------- symbols

// Var is a local variable or parameter.
type Var struct {
	Name     string
	Type     Type
	Final    bool
	Pos      source.Pos
	ID       int
	Captured bool
	Assigns  int
	Owner    *Method
	// Field is set for captured variables accessed inside a closure class.
	Field *Field
}

// Field is a class field or property backing storage.
type Field struct {
	Name     string
	Type     Type
	Owner    *Class
	Mods     Mods
	Pos      source.Pos
	Index    int // struct slot for instance fields
	Decl     *VarDeclarator
	Getter   *Method // property accessors
	Setter   *Method
	IsProp   bool
	Storage  bool // has backing storage
	ConstVal any  // compile-time constant for static finals
	EnumOrd  int
	// Annos are the annotations written on the declaration this field came
	// from. They live on the FieldDecl, which may declare several fields, so
	// the checker copies them here: everything that reads them -- the Lombok
	// pass, the container, the emitter's reflection metadata -- wants them per
	// field.
	Annos []*Annotation
	// annotation-driven members (Lombok compatibility)
	NonNull bool
	// Singular marks a @Singular builder field: the builder accumulates into a
	// collection instead of replacing it.
	Singular     bool
	SingularName string
	// ObtainViaField and ObtainViaMethod let @Builder.ObtainVia tell the builder
	// to read the value from somewhere other than the field itself.
	ObtainViaField  string
	ObtainViaMethod string
	// ObtainViaStatic marks `@Builder.ObtainVia(isStatic = true, method = ...)`:
	// the method is a static one of the type being built, called with the
	// instance to read the value from.
	ObtainViaStatic bool
	Include         bool // @ToString.Include / @EqualsAndHashCode.Include
	Exclude         bool // @ToString.Exclude / @EqualsAndHashCode.Exclude
	DefaultExpr     Expr // @Builder.Default initializer
	InitExpr        Expr // synthesized static initializer run from <clinit>
	Anno            string
}

// Method is a method, constructor or accessor.
type Method struct {
	Name       string
	Owner      *Class
	Mods       Mods
	TypeParams []*TypeVar
	Params     []Type
	ParamNames []string
	Result     Type
	Varargs    bool
	IsCtor     bool
	Decl       *MethodDecl
	// ParamAnnos are the annotations written on each parameter, one list per
	// parameter. They are a parameter's own (Java's getParameterAnnotations),
	// and where @Value and @Autowired are read from when the container builds
	// a bean.
	ParamAnnos [][]*Annotation
	Accessor   *Accessor
	Prop       *Field
	Pos        source.Pos
	VIndex     int // vtable slot, -1 if not virtual
	Selector   int // interface selector id, -1 if none
	Overrides  *Method
	Overridden bool
	LLName     string
	Native     string // native C symbol when implemented in the runtime
	Synthetic  func() // body generator marker
	SynthKind  string
	// Body is a compiler-synthesized body (annotation processing). It is
	// type-checked like an ordinary method body.
	Body         *Block
	Anno         string // the annotation that generated this member
	Checked      bool   // sema has already checked this synthesized body
	Tolerate     bool   // @Tolerate: allow a generated duplicate
	SyncOn       *Field // @Synchronized lock field for static methods
	ParamNonNull []bool
	Lambda       *Lambda
	Used         bool
	Bridge       *Method
	Forward      *Method // anonymous-class constructor forwards to this target
	ThisVar      *Var
	ParamVars    []*Var
	Locals       []*Var
	HasTry       bool
	External     bool
}

// IsStatic reports whether the method is static.
func (m *Method) IsStatic() bool { return m.Mods.Has(ModStatic) }

// Class is a resolved class, interface, enum or record.
type Class struct {
	Name       string // simple name
	Full       string // qualified name
	Kind       ClassKind
	Mods       Mods
	Decl       *ClassDecl
	File       *File
	Outer      *Class
	Owner      *Class // enclosing class for synthesized nested types
	TypeParams []*TypeVar
	Super      *ClassType
	Ifaces     []*ClassType
	Fields     []*Field
	FieldMap   map[string]*Field
	Methods    map[string][]*Method
	Ctors      []*Method
	Nested     map[string]*Class
	VTable     []*Method
	InstFields []*Field // full layout including inherited
	ID         int
	Resolved   bool
	Laidout    bool
	Builtin    bool // prelude class
	Inner      bool // has outer instance
	OuterField *Field
	Captures   []*Var
	CapFields  map[*Var]*Field
	LocalOwner *Method
	// LocalScopes is the scope chain of the enclosing method at the point a
	// local or anonymous class is declared, so that its body can see the
	// variables that are in scope there (JLS 6.3).
	LocalScopes []map[string]*Var
	// LocalClasses is the local classes in scope where a local or anonymous
	// class is declared. A local class belongs to its block (JLS 6.3), so two
	// methods may each declare `class Local` and they are different types;
	// naming one through the enclosing type's Nested map -- one namespace --
	// could only ever hold one of them.
	LocalClasses map[string]*Class
	Special      string // "String", "array", "Object", box names
	Subclasses   []*Class
	EnumConsts   []*Field
	ClInit       *Method
	Anon         bool
	Instantiable bool
	Utility      bool // @UtilityClass
	Lambda       *Lambda
}

// IsInterface reports whether c is an interface.
func (c *Class) IsInterface() bool { return c.Kind == KindInterface || c.Kind == KindAnnotation }

// TypeVar is a generic type parameter symbol.
type TypeVar struct {
	Name  string
	Bound Type
	// Bounds holds every bound of an intersection (JLS 4.4: `<T extends A & B>`)
	// with Bound kept equal to Bounds[0] for the single-bound call sites. A
	// member of any bound is a member of the variable, so lookups walk the
	// whole list; erasure is the leftmost bound, as the JLS requires.
	Bounds []Type
	ID     int
}
