package codegen

import (
	"reflect"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
)

/* Which classes a program's reflection can reach.
 *
 * Reflection is the one part of a program's metadata that names classes its own
 * code never names: a member table holds the invoker of every method of its
 * class, an invoker calls the method, and the class table Class.forName searches
 * holds every class a program may load by name. Written for every class, that
 * data pins the whole standard library into every executable that reflects at
 * all -- measured on this tree at -O2, tests/programs/t146_reflect is 9,694,296
 * bytes with the metadata and 853,616 with neither table.
 *
 * So the compiler emits those two tables for the classes reflection can
 * actually reach and leaves the rest to link-time optimisation, which drops a
 * table nothing refers to along with the invokers it named. This file is the
 * analysis that decides which classes those are.
 *
 * What it has to be is *sound*. A class left out of a member table answers an
 * empty member list, and an empty answer is a wrong answer that only a program
 * asking for that class would notice -- which is exactly the program the
 * compiler was told it could not be. So every rule below widens: a shape the
 * analysis cannot bound makes the answer every class, which is the behaviour
 * this file replaces. The one direction it must never take is a narrow answer
 * for a shape it did not understand.
 *
 * The analysis is a reachability question over the program's own expressions,
 * because that is where the classes are named:
 *
 *   - a *query site* is a call that reads one of the tables -- Class's member
 *     accessors, any method of Field, Method, Constructor or Parameter, and
 *     Class.forName. The classes a query site reads are the classes its
 *     receiver can stand for, which classesOf and ownersOf compute from the
 *     expression: a class literal names one class, getClass names the
 *     receiver's static type and everything below it, a Class variable holds
 *     what was stored in it anywhere, and a Class that came out of another
 *     query is followed back to the class it came from. A receiver the rules
 *     cannot follow -- a parameter, a library method's return value, a field
 *     nothing writes -- makes the answer every class.
 *   - Class.forName loads a class by name, so its argument has to be a name the
 *     analysis can read: a string literal, a `static final String`, or a
 *     concatenation of those. Anything else -- a name read from a file, built
 *     in a loop -- makes the class table every class, because a name the
 *     compiler cannot read is a name it cannot rule out. That is what makes
 *     forName the hard half of this: one call with a computed name is enough to
 *     put the whole library back into the table.
 *
 * A program is not the only code that reflects, and the analysis is precise
 * only about the code it can see a class in. The standard library reflects on
 * classes a program hands it -- Validation validates whatever bean it is given,
 * the container builds whatever bean class it is asked for, Gson binds whatever
 * type it is passed -- and there the receiver is a parameter or a getClass() of
 * an Object, so nothing in the library's own code says which class it will be.
 * The analysis answers that by refusing to prune: a library class that reads a
 * table (see reflectScan.users) is a reflection user, and a program that can
 * reach one -- because it names it, or names a class that names it, and so on
 * to a fixpoint -- is a program whose reflection the analysis cannot bound, so
 * both tables keep every class.
 *
 * The one part of the library that is not a reflection user is java.lang.reflect
 * itself (reflectAPIPath). Class, Field, Method and Constructor are the API: a
 * query inside them is the query their caller made, on the class their caller
 * named -- the API only ever reads a handle it was handed -- so counting them
 * would make every program that reflects a user of the whole library. The call
 * site that entered the API is a query site of its own, and that is what the
 * analysis reads.
 */

// reflectAPIPath is the standard library file java.lang.reflect is written in.
// Its classes are the API rather than users of it; see the comment above.
const reflectAPIPath = "<lib>/26_reflect.teyru"

// reflectSet is the answer for one table: the classes it has to cover, or every
// class when the analysis could not bound it.
type reflectSet struct {
	all bool
	cls map[*ast.Class]bool
}

func newReflectSet() *reflectSet { return &reflectSet{cls: map[*ast.Class]bool{}} }

// add records one class the table has to cover.
func (s *reflectSet) add(c *ast.Class) {
	if c != nil {
		s.cls[c] = true
	}
}

// widen makes the answer every class, which is the answer for a shape the
// analysis cannot bound.
func (s *reflectSet) widen() { s.all = true }

// covers reports whether the table has to cover c.
func (s *reflectSet) covers(c *ast.Class) bool {
	return s.all || s.cls[c]
}

// merge folds a computed class set in; a nil set is every class.
func (s *reflectSet) merge(o clsSet) {
	if o == nil {
		s.widen()
		return
	}
	for c := range o {
		s.add(c)
	}
}

// clsSet is the set of classes an expression can stand for. A nil set is every
// class: it is what every rule that cannot bound an expression answers with, so
// that a shape this file does not know widens the answer rather than narrowing
// it.
type clsSet map[*ast.Class]bool

// merge adds every class of o. Either side being nil makes the answer nil,
// which is the widening direction.
func (s clsSet) merge(o clsSet) clsSet {
	if s == nil || o == nil {
		return nil
	}
	for c := range o {
		s[c] = true
	}
	return s
}

// classes is the class set of a list of classes.
func classes(cs ...*ast.Class) clsSet {
	out := clsSet{}
	for _, c := range cs {
		if c != nil {
			out[c] = true
		}
	}
	return out
}

// covers reports whether the member tables of cl ship.
//
// A table the scan left out is not written at all, and the class record keeps
// the null table and the zero count its initializer was written with, so a
// query on that class answers an empty member list -- which is why the scan has
// to be sound rather than plausible.
func (e *Emitter) covers(cl *ast.Class) bool {
	if e.noTables {
		return false
	}
	if e.reflTables == nil {
		return true
	}
	return e.reflTables.covers(cl)
}

// namesCover reports whether the class table Class.forName searches has to name
// cl.
func (e *Emitter) namesCover(cl *ast.Class) bool {
	if e.noTables {
		return false
	}
	if e.reflNames == nil {
		return true
	}
	return e.reflNames.covers(cl)
}

// enterTables points the sections a member table and its invokers are written
// into at the unit that carries them -- the program's, whatever class the table
// belongs to -- and returns the function that puts the sections back.
//
// A table of a standard library class names that class's methods: every entry's
// invoker calls one, so the table is code as much as data. It ships only for a
// program that reflects, and a unit compiled once and shared by every program
// cannot carry it, so the program's unit is where it goes. What stays in the
// unit that owns the class is the class's struct, its static fields, its vtable
// and its class record (see enter), which is everything a program that does not
// reflect needs of it.
//
// sideLib is cleared while the tables are written because a literal they use is
// the program's to define: the library's unit does not carry the table, so a
// literal only a table names would have no definition anywhere. A literal a
// library *body* also uses keeps the marking the body gave it and stays the
// library's, which is what makes one literal one object across the two units.
func (e *Emitter) enterTables() func() {
	if !e.split {
		return func() {}
	}
	meta, fns, side := e.meta, e.fns, e.sideLib
	e.meta, e.fns, e.sideLib = &e.own.meta, &e.own.fns, false
	return func() { e.meta, e.fns, e.sideLib = meta, fns, side }
}

// isNilNode reports whether a node is a nil pointer held in an interface. A
// statement or expression the parser left unset is one, and the walk has to
// step over it rather than read the node it matched.
func isNilNode(n any) bool {
	if n == nil {
		return true
	}
	v := reflect.ValueOf(n)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// typeNode wraps a resolved type so the walk can visit it: a class names
// classes in two forms -- a written type, which the walk meets as a TypeExpr,
// and the type the checker resolved it to, which is what a field carries.
type typeNode struct{ t ast.Type }

// callSite is one call in a class's body: the static type of its receiver and
// the method it calls.
type callSite struct {
	recv ast.Type
	m    *ast.Method
}

// targetKey identifies one dispatch question: which methods a call of m on a
// receiver of class recv can run.
type targetKey struct {
	recv *ast.Class
	m    *ast.Method
}

// dispatchTargets answers the classes whose code a call can run: the method it
// names, and the implementation every class below the receiver's static type
// would run for it.
//
// This is the second way a class reaches a reflection user, next to naming it:
// `handler.handle()` runs RouteHandler's code in a class that never writes the
// name RouteHandler down. Which method a dispatch runs is not guessed from the
// name -- a name is not a method, and every wrapper declares valueOf -- but read
// off the same tables the emitter dispatches through: the vtable slot of a
// class call, and the implementation an interface call selects. A receiver type
// the analysis cannot read is taken as reaching every class, which is the
// direction that cannot break a program.
func (s *reflectScan) dispatchTargets(c callSite) clsSet {
	if c.m == nil || c.m.Owner == nil {
		return nil
	}
	out := clsSet{c.m.Owner: true}
	if c.recv == nil {
		// A static or unqualified call runs the method it names.
		return out
	}
	ct, ok := s.e.prog.Erased(c.recv).(*ast.ClassType)
	if !ok || ct.Class == nil {
		return nil
	}
	if c.m.IsStatic() || c.m.IsCtor || c.m.VIndex < 0 && c.m.Selector < 0 {
		// No override can run.
		return out
	}
	key := targetKey{recv: ct.Class, m: c.m}
	if set, ok := s.targets[key]; ok {
		return set
	}
	for sub := range s.sub[ct.Class] {
		if c.m.Selector >= 0 {
			if impl := s.e.prog.Implements(sub, c.m); impl != nil && impl.Owner != nil {
				out[impl.Owner] = true
			}
			continue
		}
		if c.m.VIndex < len(sub.VTable) && sub.VTable[c.m.VIndex] != nil {
			if owner := sub.VTable[c.m.VIndex].Owner; owner != nil {
				out[owner] = true
			}
		}
	}
	s.targets[key] = out
	return out
}

// reflectScan is the analysis. It is built for one program, reads the whole
// tree, and answers with the two tables' class sets.
type reflectScan struct {
	e *Emitter
	// sup and sub are each class's transitive supertypes and subtypes. The
	// checker keeps Subclasses for the superclass chain only, so the
	// implementors of an interface are collected here.
	sup map[*ast.Class]clsSet
	sub map[*ast.Class]clsSet
	// varVals and fldVals are the expressions that can be stored in a variable
	// or a field, and rets the expressions a method can return: what a Class
	// value is followed through. They are flow-insensitive -- every assignment
	// anywhere counts -- which is the safe direction, since a value stored on
	// one path is still counted.
	varVals map[*ast.Var][]ast.Expr
	fldVals map[*ast.Field][]ast.Expr
	rets    map[*ast.Method][]ast.Expr
	// busy guards the one recursive rule: a method whose return value is read
	// through another method's return value.
	busy map[*ast.Method]bool
	// refs is the classes each class names, which is the edge the trigger
	// closure walks.
	refs map[*ast.Class]clsSet
	// calls are the call sites of each class: the receiver's static type and the
	// name called, which is the other edge the trigger closure walks -- a class
	// that dispatches to a reflection user reaches it without naming it.
	calls map[*ast.Class][]callSite
	// targets memoizes the dispatch question per (receiver class, method).
	targets map[targetKey]clsSet
	// curMethod is the method being walked, which is what a return statement
	// belongs to.
	curMethod *ast.Method
	// users are the standard library classes that read a table for a class
	// they were handed, and so cannot be bounded from their own code.
	users map[*ast.Class]bool
	// tables and names are the two answers.
	tables *reflectSet
	names  *reflectSet
}

// reflectionReach answers which classes the member tables and the class table
// Class.forName searches have to cover for this program.
//
// The two answers are independent: a class a program only loads by name needs
// no member table, and a class whose members it reads through a class literal
// needs no entry in the name table. Both are the class set reflection can reach
// through one of the two, and both fall back to every class when the analysis
// cannot bound them.
func (e *Emitter) reflectionReach() (*reflectSet, *reflectSet) {
	s := &reflectScan{
		e:       e,
		sup:     map[*ast.Class]clsSet{},
		sub:     map[*ast.Class]clsSet{},
		varVals: map[*ast.Var][]ast.Expr{},
		fldVals: map[*ast.Field][]ast.Expr{},
		rets:    map[*ast.Method][]ast.Expr{},
		busy:    map[*ast.Method]bool{},
		refs:    map[*ast.Class]clsSet{},
		calls:   map[*ast.Class][]callSite{},
		targets: map[targetKey]clsSet{},
		users:   map[*ast.Class]bool{},
		tables:  newReflectSet(),
		names:   newReflectSet(),
	}
	s.hierarchy()
	for _, cl := range e.prog.Classes {
		if cl == nil {
			continue
		}
		s.index(cl)
	}
	for _, cl := range e.prog.Classes {
		if cl == nil {
			continue
		}
		s.read(cl)
	}
	s.triggers()
	return s.tables, s.names
}

// hierarchy fills sup and sub from the resolved supertypes.
func (s *reflectScan) hierarchy() {
	for _, cl := range s.e.prog.Classes {
		if cl == nil {
			continue
		}
		set := clsSet{}
		var walk func(c *ast.Class)
		walk = func(c *ast.Class) {
			if c == nil {
				return
			}
			if c.Super != nil && c.Super.Class != nil && !set[c.Super.Class] {
				set[c.Super.Class] = true
				walk(c.Super.Class)
			}
			for _, i := range c.Ifaces {
				if i.Class != nil && !set[i.Class] {
					set[i.Class] = true
					walk(i.Class)
				}
			}
		}
		walk(cl)
		s.sup[cl] = set
		for sup := range set {
			if s.sub[sup] == nil {
				s.sub[sup] = clsSet{}
			}
			s.sub[sup][cl] = true
		}
	}
}

// index collects what the value rules read from one class: the expressions
// stored in its variables and fields, the expressions its methods return, and
// the classes it names.
func (s *reflectScan) index(cl *ast.Class) {
	refs := clsSet{}
	s.refs[cl] = refs
	s.walkClass(cl, func(n any) {
		switch v := n.(type) {
		case *ast.LocalVar:
			for _, d := range v.Vars {
				if d.Sym != nil && d.Init != nil {
					s.varVals[d.Sym] = append(s.varVals[d.Sym], d.Init)
				}
			}
		case *ast.Assign:
			switch t := v.X.(type) {
			case *ast.Ident:
				switch r := t.Ref.(type) {
				case *ast.Var:
					s.varVals[r] = append(s.varVals[r], v.Y)
				case *ast.Field:
					s.fldVals[r] = append(s.fldVals[r], v.Y)
				}
			case *ast.Select:
				if f, ok := t.Ref.(*ast.Field); ok {
					s.fldVals[f] = append(s.fldVals[f], v.Y)
				}
			}
		case *ast.ForEach:
			if v.Var != nil && v.Var.Sym != nil {
				s.varVals[v.Var.Sym] = append(s.varVals[v.Var.Sym], v.X)
			}
		case *ast.Return:
			if v.X != nil && s.curMethod != nil {
				s.rets[s.curMethod] = append(s.rets[s.curMethod], v.X)
			}
		case *ast.TypeExpr:
			for c := range s.typeClasses(v.Resolved) {
				refs[c] = true
			}
		case typeNode:
			for c := range s.typeClasses(v.t) {
				refs[c] = true
			}
		case *ast.Call:
			if v.Method != nil {
				refs[v.Method.Owner] = true
				for c := range s.typeClasses(v.RecvType) {
					refs[c] = true
				}
				// Only a call on a receiver can dispatch to another class: a
				// static or unqualified call names its own target, which the
				// mention relation already covers.
				if v.Recv != nil {
					s.calls[cl] = append(s.calls[cl], callSite{recv: v.RecvType, m: v.Method})
				}
			}
		case *ast.Ident:
			switch r := v.Ref.(type) {
			case *ast.Field:
				refs[r.Owner] = true
			case *ast.Class:
				refs[r] = true
			}
		case *ast.Select:
			switch r := v.Ref.(type) {
			case *ast.Field:
				refs[r.Owner] = true
			case *ast.Class:
				refs[r] = true
			}
		case *ast.ClassLit:
			for c := range s.literalClasses(v) {
				refs[c] = true
			}
		case *ast.New:
			for c := range s.typeClasses(v.Type.Resolved) {
				refs[c] = true
			}
			// Creating an instance is what puts a class's code in reach, and
			// the class is named where it is created, so the constructor is a
			// call site like any other.
			if v.Ctor != nil {
				s.calls[cl] = append(s.calls[cl], callSite{m: v.Ctor})
			}
		}
	})
}

// read records what one class's reflection reads.
func (s *reflectScan) read(cl *ast.Class) {
	lib := cl.Prelude()
	api := s.isAPI(cl)
	s.walkClass(cl, func(n any) {
		c, ok := n.(*ast.Call)
		if !ok {
			return
		}
		kind, declared := queryKindOf(c)
		if kind == qNone {
			return
		}
		if kind == qNames {
			if name := s.constString(c); name != "" {
				// A name the compiler can read is a class the table can name,
				// wherever the call is written: the program may reach this
				// library body, and then the load happens.
				s.names.add(s.lookupName(name))
				s.registerArrayName(name)
				return
			}
			if lib && api {
				// Class.forName's own body loads a name it was handed.
				return
			}
			if lib {
				s.users[cl] = true
				return
			}
			// A name the compiler cannot read is a name it cannot rule out:
			// the table keeps every class.
			s.names.widen()
			return
		}
		if lib {
			// A library class that reads a member table reads it for a class a
			// program handed it, which its own code does not name.
			if !api {
				s.users[cl] = true
			}
			return
		}
		switch kind {
		case qClass:
			set := s.classesOf(c.Recv)
			if !declared {
				// The inherited members come from the superclasses, whose
				// tables are read as well.
				set = s.supertypes(set)
			}
			s.tables.merge(set)
		case qMember:
			set := s.ownersOf(c.Recv)
			s.tables.merge(set)
		}
	})
}

// triggers widens both answers when the program can reach a library class that
// reflects on a class of unknown provenance.
//
// The question is reachability, and it is asked over the classes each class
// names: the classes that name a reflection user are the classes that can run
// it, one step at a time, and a class that names one of those can run it too.
// Naming a class is a superset of calling it -- a type in a declaration is not
// a call -- and that is the direction this has to err in: a class the analysis
// wrongly believes reaches reflection costs the saving, and one it wrongly
// believes does not costs a wrong answer.
func (s *reflectScan) triggers() {
	if len(s.users) == 0 {
		return
	}
	// The closure: the classes that reach a reflection user through the calls
	// the library writes down, including the constructor calls -- creating a
	// class's instance is what puts its code in reach, and the class it creates
	// is named where it is created.
	reach := map[*ast.Class]bool{}
	var queue []*ast.Class
	for cl := range s.users {
		reach[cl] = true
		queue = append(queue, cl)
	}
	for len(queue) > 0 {
		cl := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for other, sites := range s.calls {
			if reach[other] {
				continue
			}
			for _, site := range sites {
				if site.m != nil && site.m.Owner == cl {
					reach[other] = true
					queue = append(queue, other)
					break
				}
			}
		}
	}
	// What decides is the program's own code: naming one of those classes. A
	// dispatch is not an edge here, in either direction, and the reason is the
	// same one on both sides: the *instance* a dispatch needs is built by code
	// that names its class, and naming it is a static call, so the class that
	// builds it -- and every class that reaches that class -- is already in the
	// closure. Following dispatches instead would put the whole library in it:
	// `closers.get(i).run()` in the stream code reaches every Runnable in the
	// program, and one of them is the web server's connection task.
	widen := func() {
		s.tables.widen()
		s.names.widen()
	}
	for _, cl := range s.e.prog.Classes {
		if cl == nil || cl.Prelude() {
			continue
		}
		for c := range s.refs[cl] {
			if reach[c] {
				widen()
				return
			}
		}
	}
	// And the tables themselves: a class whose member table ships is a class
	// reflection can invoke the methods of, so a reflection user that ships a
	// table can run its own reflection through one.
	for c := range s.tables.cls {
		if reach[c] {
			widen()
			return
		}
	}
}

// isAPI reports whether a class is part of java.lang.reflect itself.
func (s *reflectScan) isAPI(cl *ast.Class) bool {
	return cl != nil && cl.File != nil && cl.File.Src != nil && cl.File.Src.Path == reflectAPIPath
}

// ------------------------------------------------------------------- the walk

// walkClass visits every node of one class's bodies: its methods and
// constructors, the initializers of its fields, its initializer blocks and its
// enum constants' arguments. A nested class is an entry of its own in the
// program's class list, so the walk does not descend into one.
//
// fn is called for every statement, expression and written type, which is what
// lets one walk collect stored values, class references and query sites.
func (s *reflectScan) walkClass(cl *ast.Class, fn func(any)) {
	fields := func() {
		for _, f := range cl.Fields {
			s.walk(typeNode{f.Type}, fn)
			if f.Decl != nil {
				s.walk(f.Decl.Init, fn)
			}
			s.walk(f.InitExpr, fn)
			s.walk(f.DefaultExpr, fn)
		}
	}
	fields()
	if cl.Decl != nil {
		for _, t := range cl.Decl.Extends {
			s.walk(t, fn)
		}
		for _, t := range cl.Decl.Implements {
			s.walk(t, fn)
		}
	}
	methods := func() {
		for _, ms := range cl.Methods {
			for _, m := range ms {
				s.walkMethod(m, fn)
			}
		}
		for _, m := range cl.Ctors {
			s.walkMethod(m, fn)
		}
	}
	methods()
	if cl.Decl == nil {
		return
	}
	for _, mem := range cl.Decl.Members {
		if ib, ok := mem.(*ast.InitBlock); ok {
			s.walk(ib.Body, fn)
		}
	}
	for _, ec := range cl.Decl.EnumConsts {
		for _, a := range ec.Args {
			s.walk(a, fn)
		}
	}
}

// walkMethod visits one method's body, and its annotation element's default.
func (s *reflectScan) walkMethod(m *ast.Method, fn func(any)) {
	if m == nil {
		return
	}
	prev := s.curMethod
	s.curMethod = m
	defer func() { s.curMethod = prev }()
	if m.Decl != nil {
		for _, p := range m.Decl.Params {
			s.walk(p.Type, fn)
		}
		s.walk(m.Decl.Result, fn)
		for _, t := range m.Decl.Throws {
			s.walk(t, fn)
		}
		s.walk(m.Decl.Default, fn)
	}
	if body := bodyOf(m); body != nil {
		s.walk(body, fn)
	}
}

// walk visits a node and everything inside it.
func (s *reflectScan) walk(n any, fn func(any)) {
	// A field the parser left unset arrives as a typed nil -- a *Block or a
	// *New whose pointer is nil, held in an interface that is not nil -- and
	// every case below reads the node it matched. The walk is a reader of the
	// tree, so it steps over one instead of dereferencing it.
	if isNilNode(n) {
		return
	}
	switch v := n.(type) {
	case nil:
		return
	case typeNode:
		fn(v)
		return
	case *ast.TypeExpr:
		if v == nil {
			return
		}
		fn(v)
		for _, a := range v.Args {
			s.walk(a, fn)
		}
		s.walk(v.Bound, fn)
		return
	case *ast.Block:
		if v == nil {
			return
		}
		for _, st := range v.Stmts {
			s.walk(st, fn)
		}
	case *ast.LocalVar:
		for _, d := range v.Vars {
			s.walk(d.Init, fn)
		}
		s.walk(v.Type, fn)
	case *ast.LocalClass:
		if v == nil {
			return
		}
		s.walk(v.InstanceInit, fn)
	case *ast.ExprStmt:
		s.walk(v.X, fn)
	case *ast.If:
		s.walk(v.Cond, fn)
		s.walk(v.Then, fn)
		s.walk(v.Else, fn)
	case *ast.While:
		s.walk(v.Cond, fn)
		s.walk(v.Body, fn)
	case *ast.DoWhile:
		s.walk(v.Body, fn)
		s.walk(v.Cond, fn)
	case *ast.For:
		for _, st := range v.Init {
			s.walk(st, fn)
		}
		s.walk(v.Cond, fn)
		for _, u := range v.Update {
			s.walk(u, fn)
		}
		s.walk(v.Body, fn)
	case *ast.ForEach:
		if v.Var != nil {
			s.walk(v.Var.Type, fn)
		}
		s.walk(v.X, fn)
		s.walk(v.Body, fn)
	case *ast.Return:
		s.walk(v.X, fn)
	case *ast.Throw:
		s.walk(v.X, fn)
	case *ast.Try:
		if v == nil {
			return
		}
		for _, r := range v.Resources {
			s.walk(r, fn)
		}
		s.walk(v.Body, fn)
		for _, c := range v.Catches {
			for _, t := range c.Types {
				s.walk(t, fn)
			}
			s.walk(c.Body, fn)
		}
		s.walk(v.Finally, fn)
	case *ast.Switch:
		if v == nil {
			return
		}
		s.walk(v.X, fn)
		for _, c := range v.Cases {
			for _, l := range c.Labels {
				s.walk(l, fn)
			}
			s.walk(c.Guard, fn)
			s.walk(c.ArrowX, fn)
			for _, st := range c.Body {
				s.walk(st, fn)
			}
		}
	case *ast.Yield:
		s.walk(v.X, fn)
	case *ast.Labeled:
		s.walk(v.Body, fn)
	case *ast.Assert:
		s.walk(v.Cond, fn)
		s.walk(v.Msg, fn)
	case *ast.Sync:
		s.walk(v.Lock, fn)
		s.walk(v.Body, fn)
	case *ast.Ident, *ast.Literal, *ast.This, *ast.SuperExpr, *ast.ClassLit,
		*ast.Break, *ast.Continue, *ast.Empty:
		// leaves
	case *ast.Select:
		s.walk(v.X, fn)
	case *ast.Index:
		s.walk(v.X, fn)
		s.walk(v.Index, fn)
	case *ast.Call:
		s.walk(v.Recv, fn)
		for _, a := range v.Args {
			s.walk(a, fn)
		}
	case *ast.New:
		s.walk(v.Outer, fn)
		for _, a := range v.Args {
			s.walk(a, fn)
		}
	case *ast.NewArray:
		if v == nil {
			return
		}
		s.walk(v.Elem, fn)
		for _, d := range v.Dims {
			s.walk(d, fn)
		}
		s.walk(v.Init, fn)
	case *ast.ArrayInit:
		if v == nil {
			return
		}
		for _, e := range v.Elems {
			s.walk(e, fn)
		}
	case *ast.Unary:
		s.walk(v.X, fn)
	case *ast.Binary:
		s.walk(v.X, fn)
		s.walk(v.Y, fn)
	case *ast.Assign:
		s.walk(v.X, fn)
		s.walk(v.Y, fn)
	case *ast.Cond:
		s.walk(v.C, fn)
		s.walk(v.X, fn)
		s.walk(v.Y, fn)
	case *ast.Cast:
		s.walk(v.Type, fn)
		s.walk(v.X, fn)
	case *ast.InstanceOf:
		s.walk(v.X, fn)
		s.walk(v.Type, fn)
	case *ast.Lambda:
		for _, p := range v.Params {
			s.walk(p.Type, fn)
		}
		s.walk(v.Body, fn)
		s.walk(v.RecvExpr, fn)
	case *ast.MethodRef:
		s.walk(v.X, fn)
		s.walk(v.TypeX, fn)
	case *ast.SwitchExpr:
		if v == nil {
			return
		}
		s.walk(v.S, fn)
	case *ast.Conv:
		s.walk(v.X, fn)
	}
	fn(n)
}

// --------------------------------------------------------------- the values

// classesOf answers which classes a Class-typed expression can stand for. A nil
// answer is every class.
func (s *reflectScan) classesOf(x ast.Expr) clsSet {
	switch v := x.(type) {
	case nil:
		return nil
	case *ast.ClassLit:
		return s.literalClasses(v)
	case *ast.Cast:
		return s.classesOf(v.X)
	case *ast.Conv:
		return s.classesOf(v.X)
	case *ast.Cond:
		return s.classesOf(v.X).merge(s.classesOf(v.Y))
	case *ast.Assign:
		return s.classesOf(v.Y)
	case *ast.Index:
		return s.elemClasses(v.X)
	case *ast.Ident:
		return s.identValues(v)
	case *ast.Select:
		return s.selectValues(v)
	case *ast.Call:
		return s.callClasses(v)
	}
	return nil
}

// literalClasses answers the classes a class literal names. A literal for a
// primitive names the class the compiler synthesizes for primitives, which has
// no members at all -- Java answers an empty method list for int.class -- so it
// needs no table.
func (s *reflectScan) literalClasses(v *ast.ClassLit) clsSet {
	if v.Type == nil {
		return nil
	}
	switch t := v.Type.Resolved.(type) {
	case *ast.ClassType:
		return classes(t.Class)
	case *ast.ArrayType:
		return classes(s.e.prog.ArrayClass())
	case *ast.PrimType:
		return clsSet{}
	case *ast.TypeVarType:
		return s.typeClasses(t)
	case *ast.WildcardType:
		if t.Bound != nil {
			return s.typeClasses(t.Bound)
		}
	}
	return nil
}

// identValues answers what a variable or field of type Class holds.
func (s *reflectScan) identValues(v *ast.Ident) clsSet {
	switch r := v.Ref.(type) {
	case *ast.Var:
		return s.exprs(s.varVals[r], s.classesOf)
	case *ast.Field:
		return s.fieldValues(r, s.classesOf)
	case *ast.Class:
		return classes(r)
	}
	return nil
}

// selectValues answers what a selected field of type Class holds.
func (s *reflectScan) selectValues(v *ast.Select) clsSet {
	switch r := v.Ref.(type) {
	case *ast.Field:
		return s.fieldValues(r, s.classesOf)
	case *ast.Class:
		return classes(r)
	}
	return nil
}

// fieldValues folds what a field holds: everything assigned to it anywhere, and
// its initializer. A field nothing writes is not a class the analysis can rule
// out -- a library method the program calls may write it -- so it answers every
// class.
func (s *reflectScan) fieldValues(f *ast.Field, get func(ast.Expr) clsSet) clsSet {
	vals := append(append([]ast.Expr{}, s.fldVals[f]...), initOf(f)...)
	if len(vals) == 0 {
		return nil
	}
	return s.exprs(vals, get)
}

// exprs folds one expression rule over a list of expressions.
func (s *reflectScan) exprs(xs []ast.Expr, get func(ast.Expr) clsSet) clsSet {
	if len(xs) == 0 {
		return nil
	}
	out := clsSet{}
	for _, x := range xs {
		out = out.merge(get(x))
		if out == nil {
			return nil
		}
	}
	return out
}

// initOf is a field's initializer as a list, for the rules that read values.
func initOf(f *ast.Field) []ast.Expr {
	if f != nil && f.Decl != nil && f.Decl.Init != nil {
		return []ast.Expr{f.Decl.Init}
	}
	return nil
}

// callClasses answers the classes a call can return, for the calls that build a
// Class: the reflection accessors that hand one out, and any other method,
// through its return expressions.
func (s *reflectScan) callClasses(v *ast.Call) clsSet {
	m := v.Method
	if m == nil {
		return nil
	}
	switch m.Owner.Name + "." + m.Name {
	case "Object.getClass":
		return s.subtypesOf(s.receiverType(v))
	case "Class.forName", "Class.forNameOf":
		if name := s.constString(v); name != "" {
			return classes(s.lookupName(name))
		}
		return nil
	case "Class.getSuperclass":
		return s.supertypes(s.classesOf(v.Recv))
	case "Class.getInterfaces":
		return s.ifacesOf(s.classesOf(v.Recv))
	case "Field.getType":
		return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
			return fieldTypesOf(cl, func(f *ast.Field) ast.Type { return f.Type })
		})
	case "Field.getElementType":
		return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
			return fieldTypesOf(cl, func(f *ast.Field) ast.Type { return s.e.elementOf(f.Type) })
		})
	case "Field.getDeclaringClass", "Method.getDeclaringClass", "Constructor.getDeclaringClass":
		return s.ownersOf(v.Recv)
	case "Method.getReturnType":
		return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
			return methodTypesOf(cl, func(m *ast.Method) []ast.Type { return []ast.Type{m.Result} })
		})
	case "Parameter.getType", "Method.getParameterTypes", "Constructor.getParameterTypes":
		return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
			return methodTypesOf(cl, func(m *ast.Method) []ast.Type { return m.Params })
		})
	case "Method.getExceptionTypes":
		return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
			return methodTypesOf(cl, func(m *ast.Method) []ast.Type {
				var out []ast.Type
				if m.Decl != nil {
					for _, t := range m.Decl.Throws {
						out = append(out, t.Resolved)
					}
				}
				return out
			})
		})
	case "Annotation.annotationType":
		// An annotation object's type is one of the annotation types the
		// program declares, which is a set the analysis can name.
		out := clsSet{}
		for _, cl := range s.e.prog.Classes {
			if cl != nil && cl.Kind == ast.KindAnnotation {
				out[cl] = true
			}
		}
		return out
	}
	return s.returnClasses(m)
}

// returnClasses answers what a method the analysis has no rule for can return:
// the class sets of its return expressions. A method the analysis cannot read
// -- a native, or one already on the stack -- answers every class.
func (s *reflectScan) returnClasses(m *ast.Method) clsSet {
	if s.busy[m] {
		return nil
	}
	rets := s.rets[m]
	if len(rets) == 0 {
		return nil
	}
	s.busy[m] = true
	defer delete(s.busy, m)
	out := clsSet{}
	for _, x := range rets {
		out = out.merge(s.classesOf(x))
		if out == nil {
			return nil
		}
	}
	return out
}

// ownersOf answers which classes a Field, Method, Constructor or Parameter
// object can belong to, which is whose table it is read from. A nil answer is
// every class.
func (s *reflectScan) ownersOf(x ast.Expr) clsSet {
	switch v := x.(type) {
	case nil:
		return nil
	case *ast.Call:
		if v.Method == nil || v.Method.Owner.Name != "Class" {
			if v.Method == nil || !memberClasses[v.Method.Owner.Name] {
				return nil
			}
			// A method of one of reflection's member classes reads the table of
			// the class the member belongs to: the receiver for an instance
			// call (Field.getAnnotation), and the array of members for a static
			// one (Annotation.find).
			if v.Recv != nil {
				return s.ownersOf(v.Recv)
			}
			if len(v.Args) > 0 {
				return s.ownersOf(v.Args[0])
			}
			return nil
		}
		switch v.Method.Name {
		case "getDeclaredFields", "getDeclaredField", "getDeclaredMethods", "getDeclaredMethod",
			"getDeclaredConstructors", "getDeclaredConstructor",
			"getAnnotations", "getAnnotation", "isAnnotationPresent":
			return s.classesOf(v.Recv)
		case "getFields", "getField", "getMethods", "getMethod", "getConstructors", "getConstructor":
			// The inherited members belong to a superclass, so its table is
			// read as well.
			return s.supertypes(s.classesOf(v.Recv))
		}
		return nil
	case *ast.Index:
		return s.ownersOf(v.X)
	case *ast.Ident:
		switch r := v.Ref.(type) {
		case *ast.Var:
			return s.exprs(s.varVals[r], s.ownersOf)
		case *ast.Field:
			return s.fieldValues(r, s.ownersOf)
		}
		return nil
	case *ast.Select:
		if r, ok := v.Ref.(*ast.Field); ok {
			return s.fieldValues(r, s.ownersOf)
		}
		return nil
	case *ast.Cast:
		return s.ownersOf(v.X)
	case *ast.Conv:
		return s.ownersOf(v.X)
	case *ast.Cond:
		return s.ownersOf(v.X).merge(s.ownersOf(v.Y))
	case *ast.Assign:
		return s.ownersOf(v.Y)
	}
	return nil
}

// elemClasses answers the classes the elements of an array expression can stand
// for, which is what an index into a Class[] reads.
func (s *reflectScan) elemClasses(x ast.Expr) clsSet {
	switch v := x.(type) {
	case *ast.Call:
		if v.Method == nil {
			return nil
		}
		switch v.Method.Owner.Name + "." + v.Method.Name {
		case "Class.getInterfaces":
			return s.ifacesOf(s.classesOf(v.Recv))
		case "Method.getParameterTypes", "Constructor.getParameterTypes", "Parameter.getType":
			return s.memberTypes(s.ownersOf(v.Recv), func(cl *ast.Class) []ast.Type {
				return methodTypesOf(cl, func(m *ast.Method) []ast.Type { return m.Params })
			})
		case "Method.getExceptionTypes":
			return nil
		}
		return nil
	case *ast.Ident:
		if r, ok := v.Ref.(*ast.Var); ok {
			return s.exprs(s.varVals[r], s.elemClasses)
		}
		return nil
	case *ast.Select:
		if r, ok := v.Ref.(*ast.Field); ok {
			return s.fieldValues(r, s.elemClasses)
		}
		return nil
	case *ast.ArrayInit:
		out := clsSet{}
		for _, e := range v.Elems {
			out = out.merge(s.classesOf(e))
			if out == nil {
				return nil
			}
		}
		return out
	case *ast.Cast:
		return s.elemClasses(v.X)
	case *ast.Conv:
		return s.elemClasses(v.X)
	}
	return nil
}

// fieldTypesOf lists the declared types of one class's fields through a
// selector.
func fieldTypesOf(cl *ast.Class, sel func(*ast.Field) ast.Type) []ast.Type {
	out := make([]ast.Type, 0, len(cl.Fields))
	for _, f := range cl.Fields {
		out = append(out, sel(f))
	}
	return out
}

// methodTypesOf lists the types one class's methods and constructors declare
// through a selector.
func methodTypesOf(cl *ast.Class, sel func(*ast.Method) []ast.Type) []ast.Type {
	var out []ast.Type
	for _, name := range mangleOrder(cl) {
		for _, m := range cl.Methods[name] {
			out = append(out, sel(m)...)
		}
	}
	for _, m := range cl.Ctors {
		out = append(out, sel(m)...)
	}
	return out
}

// memberTypes answers the classes a set of member types name: the field types
// of a set of classes, or the parameter and result types of their methods,
// which is what the reflection accessors hand out as Class objects.
func (s *reflectScan) memberTypes(owners clsSet, sel func(*ast.Class) []ast.Type) clsSet {
	if owners == nil {
		return nil
	}
	out := clsSet{}
	for cl := range owners {
		for _, t := range sel(cl) {
			out = out.merge(s.typeClasses(t))
			if out == nil {
				return nil
			}
		}
	}
	return out
}

// typeClasses answers the classes one type can name. A primitive names the
// class the compiler synthesizes for primitives, which carries no members.
func (s *reflectScan) typeClasses(t ast.Type) clsSet {
	switch v := s.e.prog.Erased(t).(type) {
	case *ast.ClassType:
		return classes(v.Class)
	case *ast.ArrayType:
		return classes(s.e.prog.ArrayClass())
	case *ast.PrimType:
		return clsSet{}
	case *ast.TypeVarType:
		return s.typeClasses(v)
	case *ast.WildcardType:
		if v.Bound != nil {
			return s.typeClasses(v.Bound)
		}
	}
	return nil
}

// receiverType is the static type of a call's receiver, which is what
// Object.getClass answers the subclasses of.
func (s *reflectScan) receiverType(v *ast.Call) ast.Type {
	if v.RecvType != nil {
		return v.RecvType
	}
	if v.Recv != nil {
		return v.Recv.GetType()
	}
	return nil
}

// subtypesOf answers a type and everything below it: the dynamic type of a
// value of static type T is some class assignable to T.
func (s *reflectScan) subtypesOf(t ast.Type) clsSet {
	set := s.typeClasses(t)
	if set == nil {
		return nil
	}
	out := clsSet{}
	for c := range set {
		out[c] = true
		for sub := range s.sub[c] {
			out[sub] = true
		}
	}
	return out
}

// supertypes answers every class above a set of classes.
func (s *reflectScan) supertypes(set clsSet) clsSet {
	if set == nil {
		return nil
	}
	out := clsSet{}
	for c := range set {
		out[c] = true
		for sup := range s.sup[c] {
			out[sup] = true
		}
	}
	return out
}

// ifacesOf answers the interfaces a set of classes implements, which is what
// Class.getInterfaces() hands out.
func (s *reflectScan) ifacesOf(set clsSet) clsSet {
	if set == nil {
		return nil
	}
	out := clsSet{}
	for c := range set {
		for sup := range s.sup[c] {
			if sup.IsInterface() {
				out[sup] = true
			}
		}
	}
	return out
}

// lookupName resolves a class name the way Class.forName does: the binary name
// first, then the simple name under the standard library's package, which is
// what makes forName("java.lang.String") find teyru.String. A name that resolves
// to nothing -- Class.forName("Nope") -- names no class, and the table needs no
// entry for it.
func (s *reflectScan) lookupName(name string) *ast.Class {
	// An array descriptor names its component: forName("[Ljava.lang.String;")
	// is answered by the array class of the class the component names, so that
	// class is what the table has to carry. A primitive component names no
	// class at all -- its array class is emitted for every program.
	if strings.HasPrefix(name, "[") {
		i := 0
		for i < len(name) && name[i] == '[' {
			i++
		}
		rest := name[i:]
		if len(rest) >= 2 && rest[0] == 'L' && rest[len(rest)-1] == ';' {
			return s.lookupName(rest[1 : len(rest)-1])
		}
		return nil
	}
	if cl := s.e.prog.LookupClass(name); cl != nil {
		return cl
	}
	if cl := s.e.prog.ProgramClass(name); cl != nil {
		return cl
	}
	simple := name
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		simple = name[dot+1:]
	}
	if cl := s.e.prog.LookupClass("teyru." + simple); cl != nil {
		return cl
	}
	return s.e.prog.ProgramClass(simple)
}

// registerArrayName emits the array class a forName descriptor names. The
// class table carries the component, not the descriptor (Class.forName parses
// a "[" name itself), but the array class has to exist: forName("[I") and
// forName("[[Ljava.lang.String;") answer with it, and the reachability scan is
// the only place that reads the name before the table is written.
func (s *reflectScan) registerArrayName(name string) {
	t := s.descriptorType(name)
	if t == nil {
		return
	}
	if at, ok := t.(*ast.ArrayType); ok {
		s.e.arrayClassFor(at.Elem)
	}
}

// descriptorType parses a JVM array descriptor into the array type it names,
// or nil for anything that is not one.
func (s *reflectScan) descriptorType(name string) ast.Type {
	dims := 0
	for dims < len(name) && name[dims] == '[' {
		dims++
	}
	if dims == 0 {
		return nil
	}
	rest := name[dims:]
	var elem ast.Type
	if len(rest) == 1 {
		kinds := map[byte]ast.PrimKind{
			'Z': ast.Boolean, 'B': ast.Byte, 'S': ast.Short, 'C': ast.Char,
			'I': ast.Int, 'J': ast.Long, 'F': ast.Float, 'D': ast.Double,
		}
		k, ok := kinds[rest[0]]
		if !ok {
			return nil
		}
		elem = &ast.PrimType{Kind: k}
	} else if len(rest) >= 2 && rest[0] == 'L' && rest[len(rest)-1] == ';' {
		cl := s.lookupName(rest[1 : len(rest)-1])
		if cl == nil {
			return nil
		}
		elem = &ast.ClassType{Class: cl}
	} else {
		return nil
	}
	for range dims {
		elem = &ast.ArrayType{Elem: elem}
	}
	return elem
}

// constString answers the compile-time string a Class.forName argument is, or
// "" when the analysis cannot read it. A name it cannot read is a name it
// cannot rule out, so the caller widens the class table.
func (s *reflectScan) constString(c *ast.Call) string {
	if len(c.Args) == 0 {
		return ""
	}
	return s.constExpr(c.Args[0])
}

// constExpr evaluates the constant strings a name argument can be written as: a
// literal, a `static final String` field, and a concatenation of those. Every
// other shape is a name the analysis cannot read.
func (s *reflectScan) constExpr(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Literal:
		if v.Kind == ast.LitString {
			return v.Str
		}
	case *ast.Ident:
		if f, ok := v.Ref.(*ast.Field); ok {
			if str, ok := s.e.prog.ConstantString(f); ok {
				return str
			}
		}
	case *ast.Select:
		if f, ok := v.Ref.(*ast.Field); ok {
			if str, ok := s.e.prog.ConstantString(f); ok {
				return str
			}
		}
	case *ast.Binary:
		if v.Op == "+" {
			l, r := s.constExpr(v.X), s.constExpr(v.Y)
			if l != "" && r != "" {
				return l + r
			}
		}
	case *ast.Conv:
		return s.constExpr(v.X)
	case *ast.Cast:
		return s.constExpr(v.X)
	}
	return ""
}

// ------------------------------------------------------------- query sites

// queryKind is what a call reads of the two tables reflection emits.
type queryKind int

const (
	// qNone: the call reads neither table. Class.getName, isAssignableFrom and
	// getEnumConstants are here: they read the class record, which every class
	// carries whether or not reflection ships.
	qNone queryKind = iota
	// qClass: the call reads the member tables of the classes its receiver
	// stands for.
	qClass
	// qMember: the call reads the member table of the class the Field, Method,
	// Constructor or Parameter it is called on belongs to.
	qMember
	// qNames: the call loads a class by name.
	qNames
)

// classQueries are the Class methods that read a member table. declared says
// whether the method reads the class's own table or the table of its whole
// superclass chain as well.
var classQueries = map[string]bool{
	"getDeclaredFields":       true,
	"getFields":               true,
	"getDeclaredField":        true,
	"getField":                true,
	"getDeclaredMethods":      true,
	"getMethods":              true,
	"getDeclaredMethod":       true,
	"getMethod":               true,
	"getDeclaredConstructors": true,
	"getConstructors":         true,
	"getDeclaredConstructor":  true,
	"getConstructor":          true,
	"newInstance":             true,
	"getAnnotations":          true,
	"getAnnotation":           true,
	"isAnnotationPresent":     true,
}

// classDeclaredQueries are the Class methods above that read only the class's
// own table.
var classDeclaredQueries = map[string]bool{
	"getDeclaredFields":       true,
	"getDeclaredField":        true,
	"getDeclaredMethods":      true,
	"getDeclaredMethod":       true,
	"getDeclaredConstructors": true,
	"getDeclaredConstructor":  true,
	"newInstance":             true,
	"getAnnotations":          true,
	"getAnnotation":           true,
	"isAnnotationPresent":     true,
}

// memberClasses are reflection's member wrappers: every method of one reads the
// member table of the class the member belongs to. Parameter reads the table of
// the method it is a parameter of, and Annotation the table of the element it
// was written on -- the annotations of a class, a field or a method.
var memberClasses = map[string]bool{
	"Field": true, "Method": true, "Constructor": true, "Parameter": true, "Annotation": true,
}

// queryKindOf reports what a call reads.
func queryKindOf(c *ast.Call) (queryKind, bool) {
	m := c.Method
	if m == nil || m.Owner == nil {
		return qNone, false
	}
	switch m.Owner.Name {
	case "Class":
		if m.Name == "forName" || m.Name == "forNameOf" {
			return qNames, false
		}
		if classQueries[m.Name] {
			return qClass, classDeclaredQueries[m.Name]
		}
	case "Field", "Method", "Constructor", "Parameter", "Annotation":
		if memberClasses[m.Owner.Name] {
			return qMember, false
		}
	}
	return qNone, false
}
