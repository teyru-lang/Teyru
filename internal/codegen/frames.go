package codegen

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
)

// This file is the C back end's half of the collector's frame maps, and it is
// the half that has to be right: a map that names every word of a frame that
// holds a reference lets the collector stop scanning that frame's words one at a
// time, and a map that misses one turns the collector into a use-after-free that
// shows up as a wrong answer days later.
//
// A map is what the generated function tells the runtime about its own frame,
// and there are three parts to it:
//
//   - the words. `framePass` reads the emitted body and, after every declaration
//     of a pointer-typed C object, writes the address of that object into the
//     frame's slot array. Every reference a generated function keeps in a
//     variable, a parameter or a statement expression's temporary is a
//     pointer-typed object, and a declaration of one is a declaration the pass
//     sees: the pass works on the text because that is where those declarations
//     are -- the emitter writes them in a hundred places, and a rule that had to
//     be applied at each of them would be a rule with a hundred chances to be
//     forgotten. One word in a frame is not a declaration of a pointer: the
//     reference fields of an object the emitter lays out in the frame itself
//     (escape.go's stack promotion), whose declaration is a struct. No text
//     scan can know a struct's fields, so the emitter names them where it lays
//     the object out and the pass gives them slots there (frameFieldsMark).
//     `TestEveryFrameWordIsMapped` in frames_test.go is the check that the rule
//     did not get one, over the C of real programs.
//
//     The rule is a rule and not a heuristic because of what a miss costs. A
//     word a map does not name, in a frame that has a map, is not retention:
//     `scan_unmapped` in tyrt.c skips the extent of every frame a map covers,
//     so the word is read by nobody and the object it held is freed while the
//     program still points at it. Four shapes were exactly that before they
//     were fixed, and frames_test.go's header lists them: a qualifier between
//     the stars and the name (`T* volatile x`, i.e. every local of a method
//     with a `try`), a declaration inside another declaration's initializer, a
//     declaration at the very start of a body, and a stack-promoted object's
//     fields.
//   - the extent. The runtime's `ty_frame_enter` records the frame's C-stack
//     extent; the collector reads everything outside it conservatively. That is
//     what makes a frame the map describes precise and leaves the runtime's own
//     frames exactly as they were.
//
//   - the kill. A word that names a reference keeps the object alive for as long
//     as the word holds it, and a local that is dead but still assigned holds
//     its object to the end of the function (measured: a `try` makes every local
//     of the method volatile, the C compiler then may not drop the store, and a
//     dead 8 MB array stayed live for the whole of main -- 8194 kB of live set
//     where 2 kB was owed). `killSites` finds where a reference's last use is
//     and the emitter writes a null store after it.
//
// What is *not* in the map, and why: the C compiler's own words. A generated
// function's frame is laid out by clang or gcc, and the words it uses for the
// values it keeps across a call, for the registers it saves, and for the
// outgoing arguments of the call it is making are not ours to name. The map
// names what we can name; a frame with no map at all is scanned conservatively
// by the collector the old way, and for a frame with one the register half of
// the map (`saved[]`, read by ty_frame_enter_ out of the ABI's callee-saved
// registers) and the extent's deliberately low floor are what cover the values
// the compiler keeps where we cannot see them. The reasoning is in tyrt.h's
// tyframe section, which is also where the direction is stated: a word we can
// name and did not is the bug, and any *other* word left unnamed is a decision.

// frameRegDecl is the line every frame's prologue is built from. `_tyfs` is the
// slot array -- the addresses of the frame's reference words -- and `_tyfs_n` is
// how many have been recorded so far; the array is sized by framePass's count.
const (
	frameVar   = "_tyfr"
	frameSlots = "_tyfs"
	// framePut is how a registration is written: through the runtime, which
	// knows how many words the frame's array holds and refuses an index past
	// the end by name rather than writing into the C compiler's words.
	framePut = "ty_frame_put"
	// frameStmtEnd is written into a body after every statement, where the
	// pass gives a statement's temporaries their words back. It is a comment
	// and the pass takes it out again.
	frameStmtEnd = "/*ty-s*/"
	// frameFieldsMark stands where a stack-promoted object is declared and
	// carries the address expressions of the reference fields that object
	// holds, up to `*/`; the pass gives each one a slot and takes the comment
	// out. A struct the emitter lays out in the frame (see stackNew) keeps its
	// reference words in the frame like any other object, and the pass cannot
	// find them by reading a declaration -- the declaration is the struct, not
	// a pointer -- so the emitter, which knows the class, names them here.
	// The loop is over the object's scope: the words are named where the object
	// is, because a prologue entry could not name a variable the prologue
	// cannot see.
	frameFieldsMark = "/*ty-f:"
)

// frameFieldsMarkFor renders the marker that has a stack-promoted object's
// reference words recorded: the address expressions of its reference fields
// (`_stack_v7_c.f_data`), which is what the pass gives slots to.
func frameFieldsMarkFor(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return frameFieldsMark + " " + strings.Join(fields, " ") + " */"
}

// frameDeclAt reads a declaration of a pointer-typed C object that starts at or
// after `i` (which is the start of the body or just past a `;`, `{`, `(`, `}` or
// newline) and returns the declared name, where the initializer starts (`eq`,
// -1 when the declaration has none), where the declaration ends, and what it
// was.
//
// The shapes it must recognise are the ones the emitter writes: `T* x = ...`,
// `T* x;`, multi-word types (`unsigned char* p`), a qualifier between the stars
// and the name (`T* volatile x`, which is every local of a method that contains
// a `try`), and `tycatch c;` -- the last one because the exception a catch frame
// holds is a reference the handler needs and the frame's own words are the only
// place it lives.
//
// `eq` is what lets the caller look inside the initializer: a statement
// expression there declares objects of its own, and they are words of this
// frame too -- see framePassRange.
func frameDeclAt(body string, i int) (name string, isCatch bool, eq, end int, ok bool) {
	j := i
	for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
		j++
	}
	/* `tycatch NAME;` first, and on its own: it is a struct rather than a
	   pointer, and the exception it holds is a reference the handler reads. */
	if strings.HasPrefix(body[j:], "tycatch") && j+7 < len(body) &&
		(body[j+7] == ' ' || body[j+7] == '\t') {
		k := j + 7
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		ns := k
		for k < len(body) && isIdentByte(body[k]) {
			k++
		}
		if k == ns || k >= len(body) || body[k] != ';' {
			return "", false, 0, 0, false
		}
		return body[ns:k], true, -1, k + 1, true
	}
	// a run of type words: identifiers separated by whitespace
	words := 0
	for {
		k := j
		for k < len(body) && (isIdentByte(body[k])) {
			k++
		}
		if k == j {
			break
		}
		words++
		if k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			j = k
			for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
				j++
			}
			continue
		}
		j = k
		break
	}
	if words == 0 {
		return "", false, 0, 0, false
	}
	// then a pointer: one or more `*` (with optional space), then an optional
	// run of qualifiers, then the name, then `=` or `;`.
	stars := 0
	for j < len(body) && (body[j] == '*' || body[j] == ' ' || body[j] == '\t') {
		if body[j] == '*' {
			stars++
		}
		j++
	}
	if stars == 0 {
		return "", false, 0, 0, false
	}
	/* A qualifier sits between the stars and the name: the emitter writes
	   `T* volatile x` for every local of a method that contains a `try`, and a
	   scan that read `volatile` as the name would leave the object that word
	   holds unnamed -- which is a missing root, not retention, because the map
	   is what makes the collector stop reading the frame word by word. */
	for {
		k := j
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		qs := k
		for k < len(body) && isIdentByte(body[k]) {
			k++
		}
		if !isQualifier(body[qs:k]) {
			// Not a qualifier: this is the name, and the spaces the loop
			// skipped are not part of it.
			j = qs
			break
		}
		j = k
	}
	ns := j
	for j < len(body) && isIdentByte(body[j]) {
		j++
	}
	if j == ns {
		return "", false, 0, 0, false
	}
	name = body[ns:j]
	k := j
	for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
		k++
	}
	if k >= len(body) || (body[k] != '=' && body[k] != ';') {
		return "", false, 0, 0, false
	}
	if body[k] == ';' {
		return name, false, -1, k + 1, true
	}
	/* With an initializer the declaration runs to the semicolon that ends it,
	   and that is not the next one: an initializer is allowed to be a statement
	   expression, and a statement expression is braces full of semicolons. The
	   scan tracks brackets, braces and string literals so that the `;` it stops
	   at is the declaration's own, and the registration is written after it --
	   writing it before the `=` would leave `T* x <reg> = ...`, which is what
	   the first version of this did. */
	end = declEnd(body, k)
	if end < 0 {
		return "", false, 0, 0, false
	}
	return name, false, k, end, true
}

// isQualifier reports whether a word between a declaration's stars and its name
// is a C qualifier rather than the name.
func isQualifier(w string) bool {
	switch w {
	case "volatile", "const", "restrict", "__restrict", "__restrict__":
		return true
	}
	return false
}

// declEnd returns the offset just past the `;` that ends a declaration whose
// initializer starts at `i`.
func declEnd(body string, i int) int {
	depth := 0
	for j := i; j < len(body); j++ {
		switch c := body[j]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '"', '\'':
			for j++; j < len(body); j++ {
				if body[j] == '\\' {
					j++
					continue
				}
				if body[j] == c {
					break
				}
			}
		case ';':
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// framePass rewrites one function body so that every pointer-typed object it
// declares has its address recorded in the frame's slot array, and returns the
// number of words recorded -- which is the size that array is given in the
// prologue.
//
// A temporary's word is given back at the end of the statement that declared it.
// A temporary lives inside one statement expression, and the statement
// expression is inside one statement; leaving its word set would keep whatever
// object it was last handed alive until the frame returned, which is the same
// hole the kills close for named variables (measured on the same shape: the
// `_a` a `new int[2M]` is built in kept the array alive for the rest of main
// after the variable that was supposed to own it had been cleared). The word is
// cleared, never the variable: what the temporary holds is read out of it by the
// statement expression that made it, and the clear happens after that.
//
// The scan does not stop at the end of a declaration: the initializer of one is
// a statement expression as often as not (`T* x = (T*)({ U* y = ...; y; })`),
// and `y` is a pointer-typed object of this frame like any other. Reading the
// declaration to its own `;` and continuing from there would leave every
// declaration inside it unnamed, which is what framePassRange exists to avoid.
func framePass(body string, first int) (string, int) {
	n := first
	var pending []int
	return framePassRange(body, &n, &pending), n - first
}

// framePassRange is framePass over one range of a body: the body itself, or the
// initializer of a declaration the scan has just read the `=` of.
//
// `n` is the next slot index and `pending` the temporaries of the statement
// being scanned; both are shared with the enclosing call, because a temporary
// declared inside an initializer is a temporary of the same statement and is
// given its word back at the same statement end.
//
// The range begins at a declaration position, so the scan looks for one there
// and not only after a separator: the first statement of a body is a declaration
// as often as any other, and a scan that only looked just past a `;` or a `{`
// left the first one out. A declaration inside a loop is registered once per
// iteration with the same index (see the fixed-site note below).
func framePassRange(s string, n *int, pending *[]int) string {
	var out strings.Builder
	out.Grow(len(s) + len(s)/8)
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], frameStmtEnd) {
			// The statement that just ended can no longer have a value in
			// flight, so the words its temporaries were recorded in go back.
			for _, k := range *pending {
				fmt.Fprintf(&out, " %s(&%s, %d, NULL);", framePut, frameVar, k)
			}
			*pending = (*pending)[:0]
			i += len(frameStmtEnd)
			continue
		}
		if strings.HasPrefix(s[i:], frameFieldsMark) {
			// The reference fields of an object the emitter laid out in this
			// frame: words of this frame that no declaration names, named here
			// because the emitter knows the class's layout and the pass does
			// not. They are not given back at the statement's end -- the object
			// outlives the statement -- so a dead object's field is retention
			// until the frame returns, which is this file's usual direction.
			end := strings.Index(s[i:], "*/")
			if end < 0 {
				out.WriteString(s[i:])
				break
			}
			for _, nm := range strings.Fields(s[i+len(frameFieldsMark) : i+end]) {
				fmt.Fprintf(&out, " %s(&%s, %d, (void*)&%s);", framePut, frameVar, *n, nm)
				*n++
			}
			i += end + len("*/")
			continue
		}
		if i == 0 || strings.IndexByte(";{(\n}", s[i-1]) >= 0 {
			name, isCatch, eq, end, ok := frameDeclAt(s, i)
			if ok {
				/* A site has a fixed index, not a running one. A declaration
				   inside a loop is registered once per iteration, and a
				   counter that advanced each time would walk off the end of
				   the slot array on the second one; writing the same index
				   again is the same word. */
				if eq >= 0 {
					// The initializer is scanned for declarations of its own;
					// the registration of this one is written after them, and
					// after the `;` that ends it.
					out.WriteString(s[i:eq])
					out.WriteString(framePassRange(s[eq:end], n, pending))
				} else {
					out.WriteString(s[i:end])
				}
				if isCatch {
					// The pending exception is the one reference a catch frame
					// holds.
					fmt.Fprintf(&out, " %s(&%s, %d, (void*)&%s.ex);", framePut, frameVar, *n, name)
				} else {
					fmt.Fprintf(&out, " %s(&%s, %d, (void*)&%s);", framePut, frameVar, *n, name)
					// The emitter's own temporaries start with an underscore;
					// the variables of the source do not. Only a temporary is
					// given back by the statement that declared it.
					if strings.HasPrefix(name, "_") {
						*pending = append(*pending, *n)
					}
				}
				*n++
				i = end
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// ---------------------------------------------------------------- the kills

// blockKills decides where a block's own variables stop being references, and
// is the whole of the analysis: for each variable the block declares, the last
// statement *of that block* whose text mentions it. The null store goes there.
//
// Three things about that rule are what make it safe rather than clever, and
// each of them is a shape a more precise rule would get wrong:
//
//   - the unit is the statement, and a statement's text includes every nested
//     block inside it. A variable used in a loop body has its last mention at
//     the *loop* statement, so its kill is written after the loop and never
//     inside it: a kill inside the body would be seen by the next iteration,
//     which reads a variable the program believes it still has.
//
//   - only the declaring block decides. A variable declared in the enclosing
//     block and used in a branch keeps living after the branch, and a kill
//     written inside the branch -- where the analysis of the branch alone would
//     put it -- would clear a value the statements after the branch read.
//
//   - nothing is decided for a variable the block does not declare: a foreach
//     variable, a pattern binding, a capture bound to the closure's field, and
//     a variable of an enclosing function's frame all keep their object for as
//     long as their storage does. That is retention, which is what every gap in
//     this file costs; the other direction would be a reference the program
//     still needs, cleared.
//
// The `try` in the caller is why this matters at all: a method containing one
// emits all of its locals volatile, the C compiler may not then drop a store to
// one, and a dead reference therefore sits in memory for the whole of the
// method keeping its object alive. That is the shape the red test is.

// declInBlock lists the variables a block declares itself, by the C name they
// are bound to. A variable bound to anything but a plain C name -- a capture,
// which is a field of the closure -- is left out.
func (e *Emitter) declInBlock(b *ast.Block) map[*ast.Var]bool {
	out := map[*ast.Var]bool{}
	if b == nil {
		return out
	}
	var mark func(v *ast.Var)
	mark = func(v *ast.Var) {
		if v == nil || !ast.IsRef(v.Type) || v.ID < 0 {
			return
		}
		if n, ok := e.locals[v]; ok && (strings.HasPrefix(n, "this->") || n == "this") {
			return
		}
		out[v] = true
	}
	for _, s := range b.Stmts {
		if lv, ok := s.(*ast.LocalVar); ok {
			for _, d := range lv.Vars {
				mark(d.Sym)
			}
		}
	}
	return out
}

// frameLeave is the epilogue every mapped frame runs before it returns: the
// record has to leave the chain the collector walks. Written before every
// `return`, including the ones inside a finally, so a frame never outlives its
// storage. A body that falls off its end gets one too.
func frameLeave(body string) string {
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		t := strings.TrimLeft(ln, " \t")
		if strings.HasPrefix(t, "return ") || strings.HasPrefix(t, "return;") {
			indent := ln[:len(ln)-len(t)]
			lines[i] = indent + "ty_frame_leave(&" + frameVar + "); " + t
		}
	}
	return strings.Join(lines, "\n")
}

// frameID is the map's identity, for a diagnostic that has to say which function
// a suspicious word came from without putting a name in the executable: a hash
// of the function's C name, which the emitted comment beside the function spells
// out so a report can be read back to a name.
func frameID(cname string) int32 {
	h := fnv.New32a()
	h.Write([]byte(cname))
	return int32(h.Sum32() & 0x7fffffff)
}

// frameReset drops the frame context while another function's body is emitted
// inside this one. The emitters for a synthetic wrapper and for a lambda write a
// function of their own from where they are called, which can be inside the body
// of the method being mapped: the statements of that nested body are not this
// frame's, and the map being built must not claim their words.
func (e *Emitter) frameReset() func() {
	prevMap, prevUse, prevTemps, prevBase := e.frameMap, e.useNow, e.frameTemps, e.frameBase
	e.frameMap, e.useNow = false, nil
	return func() {
		e.frameMap, e.useNow, e.frameTemps, e.frameBase = prevMap, prevUse, prevTemps, prevBase
	}
}

// emitMappedBody writes a function body with its frame map around it: the
// prologue that declares the map and links it into the thread's chain, the body
// with the address of every pointer-typed object it declares recorded in it, and
// the leave before every return.
//
// `extra` are the reference words the body does not declare -- the C parameters,
// whose storage is the caller's outgoing argument area and the callee's own
// frame, and which are registered here because a parameter is a reference a
// program keeps and the map has to name it like any other.
//
// A function whose frame holds no reference at all -- an arithmetic helper, a
// getter over a primitive field -- records nothing, emits no map, and is scanned
// by the collector the way the runtime's own frames are. That is deliberate: a
// frame with no map in it costs the collector nothing and can hide nothing,
// because the conservative pass covers exactly the frames the maps do not.
func (e *Emitter) emitMappedBody(cname string, extra []string, body func()) {
	if frameMapsOff {
		// The body as it was before any of this: no map, no kill stores, no
		// words for values in flight. The collector scans the frame the way it
		// scans the runtime's own. See frameMapsOff.
		body()
		return
	}
	prevMap, prevUse := e.frameMap, e.useNow
	e.frameMap = true
	text := e.capture(body)
	e.frameMap, e.useNow = prevMap, prevUse
	temps := e.frameTemps
	words := append(append([]string{}, extra...), temps...)
	reg, n := framePass(text, len(words))
	if n == 0 && len(words) == 0 {
		e.code.WriteString(reg)
		return
	}
	reg = frameLeave(reg)
	n += len(words)
	e.line("tyframe %s;\n", frameVar)
	for _, nm := range temps {
		// NULL, because the word is in the map from the moment the frame is
		// entered and a word nothing has written yet is whatever the stack
		// held.
		e.line("void *%s = NULL;\n", nm)
	}
	// Zeroed: a word whose registration has not run yet -- a declaration inside
	// a branch or a loop body that this execution has not reached -- holds
	// nothing, and a word holding whatever the stack had would be believed by
	// the collector as a reference.
	e.line("void *%s[%d] = {0};\n", frameSlots, n)
	e.line("ty_frame_enter(&%s, %s, %d, %d); /* frame %s */\n",
		frameVar, frameSlots, n, frameID(cname), cname)
	for i, nm := range words {
		e.line("%s(&%s, %d, (void*)&%s);\n", framePut, frameVar, i, nm)
	}
	e.code.WriteString(reg)
	e.line("ty_frame_leave(&%s);\n", frameVar)
}

// frameTemp returns a C expression whose value is `v` -- a reference-valued
// expression already rendered -- held in a word of the current frame.
//
// It is what a value in flight needs. `f(g(), h())` evaluates g(), then h(),
// then calls f, and the object g() returned has to survive h(): the C compiler
// keeps it in a callee-saved register, or saves that register in a frame of its
// own, or spills it into a word of this frame, and none of those is a word the
// map can name. Putting the value in a frame word of our own first makes it one
// the map does name, for exactly as long as the statement runs -- which is as
// long as any value can still be waiting for the call that consumes it.
//
// A site has one word for the whole function, and a site inside a loop writes
// the same word every iteration: the value the previous iteration left there is
// dead by the time the next one writes, and the statement end gives the word
// back anyway (emitBlockInner).
func (e *Emitter) frameTemp(t ast.Type, v string) string {
	k := len(e.frameTemps)
	name := fmt.Sprintf("_tyft%d", k)
	e.frameTemps = append(e.frameTemps, name)
	return fmt.Sprintf("({ %s = (void*)(%s); (%s)%s; })", name, v, e.ctype(t), name)
}

// frameWrap is which expressions have their value put in a frame word. It is
// the ones that *create* the object they answer with: a call, an allocation, a
// closure, a boxing conversion, a string concatenation (which the emitter lowers
// to a call that builds one), and a switch expression (which the emitter lowers
// to a block with a local in it).
//
// A field or an element read is deliberately not here, and does not need to be:
// the object it answers with is reachable from the object it was read out of,
// and that one is either a word of the map (a variable), a static the collector
// walks by address, or the result of one of the expressions above -- which is in
// a word of the map by this rule.
func (e *Emitter) frameWrap(x ast.Expr) bool {
	if frameTempsOff || !e.frameMap || x == nil {
		return false
	}
	t := x.GetType()
	if t == nil || !ast.IsRef(t) {
		return false
	}
	switch x.(type) {
	case *ast.Call, *ast.New, *ast.NewArray, *ast.ArrayInit, *ast.Lambda, *ast.MethodRef,
		*ast.SwitchExpr, *ast.Conv, *ast.Binary:
		return true
	}
	return false
}

// funcParams returns the C names of a method's reference-typed parameters, with
// the receiver first when there is one. These are the frame's words that are not
// declarations the body makes.
func (e *Emitter) funcParams(m *ast.Method) []string {
	var out []string
	if m != nil && !m.IsStatic() {
		out = append(out, "this")
	}
	if m == nil {
		return out
	}
	for i, t := range m.Params {
		if !ast.IsRef(t) {
			continue
		}
		out = append(out, fmt.Sprintf("a%d", i))
	}
	return out
}

// Two switches for the diagnosis of the mechanism itself, read from the
// environment so that a run can be repeated with one part of it off without the
// compiler being rebuilt either way. They exist for the same reason
// TEYRU_GC_NOEXCL does.
var (
	frameTempsOff = os.Getenv("TEYRU_NO_FRAME_TEMPS") != ""
	killsOff      = os.Getenv("TEYRU_NO_KILLS") != ""
	// frameMapsOff turns the mechanism off: the body is emitted as it was
	// before any of this, the collector scans the frame the way it scans the
	// runtime's own, and neither the kills nor the words for values in flight
	// are written. It is the switch that says whether a difference the maps
	// make is what broke a program, and it exists because a change to the root
	// set has to be able to be taken back out in one word.
	frameMapsOff = os.Getenv("TEYRU_NO_FRAME_MAPS") != ""
)
