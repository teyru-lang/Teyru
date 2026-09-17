package codegen

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/parser"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/source"
	"github.com/teyru-lang/Teyru/lib"
)

// This is the check frames.go's header names: every word of a generated frame
// that can hold a reference is named by that frame's map.
//
// A word the map misses is not retention, it is a missing root. The collector
// stops reading the words a map covers -- scan_unmapped walks the complement of
// the mapped extents -- so a reference no map names is an object the collector
// frees while the program still holds it.
//
// Two things make the check worth running on the real thing:
//
//   - the C it reads is the C the emitter writes, for real programs, including
//     the standard library: 4000-odd mapped frames per case. A hand-written
//     snippet would test the snippet.
//   - it finds the words itself, by reading the C the way a reader would
//     (declarations of pointer-typed objects, and the reference fields of the
//     objects the emitter lays out in the frame), rather than by calling
//     frameDeclAt. A check that asked the pass what it thinks it saw could not
//     disagree with the pass.
//
// Four shapes are this check's red cases. Each was a word the pass or the
// emitter left unnamed, each was found by this check failing, and each is fixed
// in the tree this file is committed with:
//
//  1. A qualifier between the stars and the name. Every local of a method that
//     contains a `try` is emitted `T* volatile x` (the longjmp rule: see
//     emit_stmt.go), and a pass that read `volatile` as the name named nothing
//     at all. Measured on a program whose 16 MB array is reachable only through
//     such a local: `System.liveBytes()` read 0 with the maps on and 16 with
//     TEYRU_NO_FRAME_MAPS=1, i.e. the array had been freed while live.
//  2. A declaration inside another declaration's initializer. `T* x = (T*)({ U*
//     y = ...; y; })` declares `y` as well, and a scan that read the first
//     declaration to its own `;` never looked inside it.
//  3. A declaration at the very start of a body. The pass looked for a
//     declaration only just past a `;`, `{`, `(`, `}` or newline, and the first
//     statement of a body has no character before it.
//  4. An object the emitter lays out in the frame (escape.go's stack promotion,
//     emit_stmt.go's stackNew). Its reference fields are words of the frame that
//     no declaration names, because the declaration is the struct. Measured the
//     same way: a promoted object holding a 2M-int array in a field read 0 live
//     bytes with the maps on and 8 with TEYRU_NO_FRAME_MAPS=1.
//
// The perturbations this file was checked against, because a check that has
// never failed is not a check. Each is the one-line change that puts back the
// bug of one shape, each made the test fail, and each was taken out again:
//
//   - shape 1: frameDeclAt's qualifier loop never recognises a qualifier, so
//     `volatile` is read as the name. The `try` case then reports
//     `M_teyru_JsonReader_nextLong_0: v733_text is a pointer-typed object and
//     no registration follows it`, then the same for every other volatile local
//     in the prelude and the program.
//   - shape 2: framePassRange writes the declaration's text instead of scanning
//     its initializer. The `nested` case reports `M_teyru_String_chars_0: _a is
//     a pointer-typed object and no registration follows it`.
//   - shape 3: the position at the start of a body is not a declaration
//     position. The `try` case reports `M_teyru_String_lines_0: v22_out is a
//     pointer-typed object and no registration follows it` and, among others,
//     `M_teyru_Byte_fillCache_0: v57_c is ...`.
//   - shape 4: stackFieldWords returns "". The `stack` case reports
//     `M_teyru_HttpServer_readBody_0: f_buf is a reference field of
//     _stack_v2970_bo, the object the emitter laid out in this frame, and the
//     map does not name it`.
func TestEveryFrameWordIsMapped(t *testing.T) {
	for _, tc := range frameCases() {
		t.Run(tc.name, func(t *testing.T) {
			frameCheckCase(t, tc)
		})
	}
}

// frameCase is one program the check runs over. The prelude is compiled with
// every one of them, so most of the frames the check reads are the standard
// library's; a case is about the shapes its own method adds.
type frameCase struct {
	name string
	src  string
}

func frameCases() []frameCase {
	return []frameCase{{
		name: "try",
		// A local of a method that contains a `try` is volatile, and the object
		// it holds is the shape that mattered most: the one the collector freed
		// while live.
		src: `
class FrameTry {
  static void churn(int n) {
    Object[] keep = new Object[64]
    for (int i = 0 : i < n : i++) {
      keep[i % 64] = new Object()
    }
  }
  public static void main(String[] args) {
    int[] big = new int[2 * 1024 * 1024]
    String s = "s" + args.length
    try {
      System.out.println(s)
    } catch (Throwable t) {
      System.out.println(t)
    }
    churn(100)
    System.gc()
    System.out.println(big[0])
  }
}`,
	}, {
		name: "nested",
		// A declaration inside a declaration's initializer: the temporaries the
		// emitter makes for a `new`, for a call's arguments, and for a value in
		// flight between two calls all live there.
		src: `
class FrameNested {
  static Object pick(Object a, Object b) { return a }
  static Object[] build(int n) { return new Object[n] }
  public static void main(String[] args) {
    Object[] xs = new Object[4]
    xs[0] = pick(new Object(), build(2))
    for (int i = 0 : i < xs.length : i++) {
      xs[i] = new Object()
    }
    System.out.println(xs.length + pick("a", "b").toString())
  }
}`,
	}, {
		name: "closures",
		// Boxed values, a captured local and a string built by concatenation:
		// the words that hold what a closure and a boxing conversion created.
		src: `
class FrameClosures {
  static int twice(int n) { return n * 2 }
  public static void main(String[] args) {
    Integer boxed = 1000
    String tag = "t"
    Runnable r = () -> System.out.println(tag + boxed)
    r.run()
    System.out.println(twice(boxed) + tag.length())
  }
}`,
	}, {
		name: "stack",
		// An object the emitter lays out in the frame: a second reading of its
		// reference field is what makes it escape, so the field is the only
		// reference to the array it holds.
		src: `
class FrameHeld {
  int[] data
  FrameHeld(int n) { data = new int[n] }
  void set(int i, int v) { data[i] = v }
  int at(int i) { return data[i] }
}
class FrameStack {
  static void churn(int n) {
    Object[] keep = new Object[64]
    for (int i = 0 : i < n : i++) {
      keep[i % 64] = new Object()
    }
  }
  public static void main(String[] args) {
    FrameHeld h = new FrameHeld(2 * 1024 * 1024)
    h.set(0, 21)
    churn(100)
    System.gc()
    System.out.println(h.at(0))
  }
}`,
	}}
}

// frameFunc is one generated function: its text and the name its own frame
// comment gives it.
type frameFunc struct {
	name string
	text string
}

// emitFrameCase compiles one program the way the driver does -- the standard
// library first, then the program -- and returns the generated C.
func emitFrameCase(t *testing.T, src string) string {
	t.Helper()
	diags := &source.Diagnostics{}
	var files []*ast.File
	for _, f := range lib.Files() {
		files = append(files, parser.Parse(source.NewFile("<lib>/"+f.Name, f.Source), diags))
	}
	files = append(files, parser.Parse(source.NewFile("frames_test.teyru", src), diags))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	prog := sema.Check(files, diags)
	if diags.HasErrors() {
		t.Fatalf("semantic errors in the test program: %v", diags)
	}
	c, _ := Emit(prog)
	return c
}

// frameCounts is what the check proves it looked at.
type frameCounts struct {
	decls     int // pointer-typed declarations examined
	qualified int // ... with a qualifier between the stars and the name
	nested    int // ... inside another declaration's initializer
	atStart   int // ... at the very start of a body
	fields    int // reference fields of stack-promoted objects examined
}

// frameCheckCase runs the whole check over one program.
func frameCheckCase(t *testing.T, tc frameCase) {
	t.Helper()
	c := emitFrameCase(t, tc.src)
	refs := classRefFields(c)
	funcs := mappedFrames(c)
	if len(funcs) < 100 {
		t.Fatalf("only %d mapped frames in the generated C; the check would prove nothing", len(funcs))
	}
	var counts frameCounts
	for _, f := range funcs {
		frameCheckFunc(t, f, refs, &counts)
	}
	if counts.decls < 100 {
		t.Errorf("only %d pointer-typed declarations were checked", counts.decls)
	}
	// The shapes the check exists for, each of which the emitter must keep
	// producing: a green run over programs that no longer contain them would be
	// a green run over nothing.
	if counts.qualified == 0 {
		t.Error("no declaration carried a qualifier between the stars and the name; the volatile shape is not covered")
	}
	if counts.nested == 0 {
		t.Error("no declaration sat inside another declaration's initializer; that shape is not covered")
	}
	if counts.atStart == 0 {
		t.Error("no body began with a declaration; that shape is not covered")
	}
	if counts.fields == 0 {
		t.Error("no stack-promoted object with a reference field was found; that shape is not covered")
	}
}

// ------------------------------------------------------------- the frames

var (
	// frameDefRe finds a function definition: a line that ends in a `{` and
	// whose text before it is a signature rather than a call or a declaration.
	frameDefRe = regexp.MustCompile(`(?m)^[A-Za-z_][^\n()]*\([^\n;]*\)[ \t]*\{[ \t]*$`)
	// frameEnterRe finds the call that links a frame into the thread's chain; a
	// function with none has no map.
	frameEnterRe = regexp.MustCompile(`ty_frame_enter\(&_tyfr, _tyfs, \d+, \d+\); /\* frame (\w+) \*/`)
	// framePutRe is a registration, anywhere: this is the set of words a frame's
	// map names.
	framePutRe = regexp.MustCompile(`ty_frame_put\(&_tyfr, \d+, \(void\*\)&(\w+)(\.ex)?\);`)
	// frameProloguePutRe is one line of the block of registrations the emitter
	// writes after frame_enter -- the words it put in the map itself: the
	// parameters and the words for values in flight.
	frameProloguePutRe = regexp.MustCompile(`[ \t]*ty_frame_put\(&_tyfr, \d+, \(void\*\)&\w+(\.ex)?\);[ \t]*\n`)
)

// mappedFrames splits the generated C into the functions that carry a map.
func mappedFrames(c string) []frameFunc {
	var out []frameFunc
	for _, loc := range frameDefRe.FindAllStringIndex(c, -1) {
		brace := strings.IndexByte(c[loc[0]:loc[1]], '{')
		if brace < 0 {
			continue
		}
		text := c[loc[0]:cFuncEnd(c, loc[0]+brace)]
		if m := frameEnterRe.FindStringSubmatch(text); m != nil {
			out = append(out, frameFunc{name: m[1], text: text})
		}
	}
	return out
}

// cFuncEnd returns the offset just past the `}` that closes the brace at `i`,
// skipping string and character literals: a brace inside a literal is not a
// brace.
func cFuncEnd(c string, i int) int {
	depth := 0
	for j := i; j < len(c); j++ {
		switch c[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j + 1
			}
		case '"', '\'':
			q := c[j]
			for j++; j < len(c); j++ {
				if c[j] == '\\' {
					j++
					continue
				}
				if c[j] == q {
					break
				}
			}
		}
	}
	return len(c)
}

// --------------------------------------------------------- the two checks

// frameCheckFunc checks one frame: every declaration of a pointer-typed object
// in its body is followed by a registration of that object, and every reference
// field of an object the emitter laid out in the frame is registered too.
//
// The body starts where the prologue ends. The emitter writes the prologue --
// the frame record, the slot array, the `ty_frame_enter` call and the
// registrations that follow it -- before the body it captured and rewrote, so
// what follows that block is the text the pass scanned.
func frameCheckFunc(t *testing.T, f frameFunc, refs map[string][]string, counts *frameCounts) {
	t.Helper()
	loc := frameEnterRe.FindStringIndex(f.text)
	if loc == nil {
		return
	}
	body := strings.TrimPrefix(f.text[loc[1]:], "\n")
	for {
		m := frameProloguePutRe.FindStringIndex(body)
		if m == nil || m[0] != 0 {
			break
		}
		body = body[m[1]:]
	}
	named := map[string]bool{}
	for _, m := range framePutRe.FindAllStringSubmatch(f.text, -1) {
		named[m[1]] = true
	}
	frameCheckDecls(t, f, body, named, counts)
	frameCheckStackFields(t, f, body, refs, counts)
}

// frameCheckDecls is the rule frames.go states: after every declaration of a
// pointer-typed C object, the address of that object is written into the
// frame's slot array.
func frameCheckDecls(t *testing.T, f frameFunc, body string, named map[string]bool, counts *frameCounts) {
	t.Helper()
	decls := frameDeclRe.FindAllStringSubmatchIndex(body, -1)
	// Where one declaration sits inside another's initializer, which is where
	// the emitter puts a statement expression's temporaries.
	open := []int{}
	nested := make([]bool, len(decls))
	for i, m := range decls {
		for len(open) > 0 && open[len(open)-1] <= m[0] {
			open = open[:len(open)-1]
		}
		nested[i] = len(open) > 0
		if body[m[10]:m[11]] == "=" {
			if end := cDeclEnd(body, m[1]-1); end > m[0] {
				open = append(open, end)
			}
		}
	}
	for i, m := range decls {
		typ := body[m[2]:m[3]]    // the type
		qual := body[m[6]:m[7]]   // what stood between the stars and the name
		name := body[m[8]:m[9]]   // the declared name
		term := body[m[10]:m[11]] // the `=` or `;` that ends the head
		if strings.Contains(typ, "typeof") {
			// `__typeof__(EXPR) n` declares an object whose type is not written
			// as a pointer type; frameCheckCopies reads what it copies instead.
			continue
		}
		counts.decls++
		if qual != "" {
			counts.qualified++
		}
		if nested[i] {
			counts.nested++
		}
		if m[0] == 0 && !strings.ContainsAny(body[:1], "\n;{(") {
			// The region begins with the declaration, so the pass has no
			// separator to look past: this is the first statement of a body.
			counts.atStart++
		}
		end := m[1]
		if term == "=" {
			if end = cDeclEnd(body, m[1]-1); end < 0 {
				t.Errorf("frame %s: the declaration of %s does not end", f.name, name)
				continue
			}
		}
		if !frameRegAt(body, end, name) {
			t.Errorf("frame %s: %s is a pointer-typed object and no registration follows it:\n\t%s",
				f.name, name, frameSnippet(body, m[0]))
		}
	}
	// The same rule for a catch frame's exception: `tycatch c;` is a struct,
	// and the reference it holds is the handler's.
	for _, m := range frameCatchRe.FindAllStringSubmatchIndex(body, -1) {
		name := body[m[2]:m[3]]
		if !frameRegAt(body, m[1], name+".ex") {
			t.Errorf("frame %s: the catch frame's %s holds an exception and no registration follows it:\n\t%s",
				f.name, name, frameSnippet(body, m[0]))
		}
	}
	frameCheckCopies(t, f, body, named)
}

// frameCheckCopies checks the declarations whose type the C does not spell: a
// `__typeof__(EXPR) n = (EXPR);` temporary. What makes its word safe is that it
// holds a copy of a value the map already names -- every expression that
// creates an object goes into a frame word first (frameTemp), and a read of a
// named object's field is reachable from that object -- so a copy whose
// expression names a word of this map needs no registration of its own, and one
// whose expression names none is a word nothing names.
func frameCheckCopies(t *testing.T, f frameFunc, body string, named map[string]bool) {
	t.Helper()
	for _, m := range frameTypeofRe.FindAllStringSubmatchIndex(body, -1) {
		expr := body[m[2]:m[3]]
		name := body[m[4]:m[5]]
		term := body[m[6]:m[7]]
		if !frameCastRe.MatchString(expr) {
			continue // not a copy of a reference
		}
		end := m[1]
		if term == "=" {
			if end = cDeclEnd(body, m[1]-1); end < 0 {
				continue
			}
		}
		if frameRegAt(body, end, name) || frameMentions(expr, named) {
			continue
		}
		t.Errorf("frame %s: the copy %s = %s holds a reference and neither it nor anything it names is registered:\n\t%s",
			f.name, name, expr, frameSnippet(body, m[0]))
	}
}

// frameCheckStackFields checks the words of an object the emitter laid out in
// the frame: the object is a struct, its reference fields are words of the
// frame, and no declaration of a pointer names them. Which fields those are is
// the class's layout, and the generated C states it itself, in the assertions
// under the class record: _Static_assert(offsetof(struct C_X, f_y) == n).
func frameCheckStackFields(t *testing.T, f frameFunc, body string, refs map[string][]string, counts *frameCounts) {
	t.Helper()
	for _, m := range frameStackDeclRe.FindAllStringSubmatch(body, -1) {
		ctype, slot := m[1], m[2]
		for _, field := range refs[ctype] {
			counts.fields++
			if !strings.Contains(body, "(void*)&"+slot+"."+field+");") {
				t.Errorf("frame %s: %s is a reference field of %s, the object the emitter laid out in this frame, and the map does not name it",
					f.name, field, slot)
			}
		}
	}
}

// classRefFields maps a generated struct name to the reference fields the
// emitted C says it has, from the assertions that tie the field layout to the
// offsets the collector walks.
func classRefFields(c string) map[string][]string {
	out := map[string][]string{}
	for _, m := range offsetofRe.FindAllStringSubmatch(c, -1) {
		out[m[1]] = append(out[m[1]], m[2])
	}
	return out
}

// ------------------------------------------------------------- the readers

var (
	// frameDeclRe reads the head of a declaration of a pointer-typed C object:
	// a type (words, or a `__typeof__(...)`), at least one `*`, an optional
	// qualifier run, the name, and the `=` or `;` after it. The head is where
	// the match ends, so the scan goes on inside an initializer -- which is
	// where the declarations of a statement expression are.
	frameDeclRe = regexp.MustCompile(`(?:^|[;{}(\n])[ \t]*` +
		`((?:__typeof__\((?:[^()]|\([^()]*\))*\)|[A-Za-z_]\w*)(?:[ \t]+[A-Za-z_]\w*)*?)` +
		`[ \t]*(\*+)[ \t]*((?:(?:volatile|const|restrict|__restrict|__restrict__)[ \t]+)*)([A-Za-z_]\w*)[ \t]*([=;])`)
	// frameCatchRe reads `tycatch c;`.
	frameCatchRe = regexp.MustCompile(`(?:^|[;{}(\n])[ \t]*tycatch[ \t]+([A-Za-z_]\w*)[ \t]*;`)
	// frameTypeofRe reads a `__typeof__(EXPR) n` declaration.
	frameTypeofRe = regexp.MustCompile(`(?:^|[;{}(\n])[ \t]*__typeof__\(((?:[^()]|\([^()]*\))*)\)[ \t]+([A-Za-z_]\w*)[ \t]*([=;])`)
	// frameCastRe is a cast to a pointer type, which is how the emitter renders
	// a reference-valued operand; a `__typeof__` around an expression without
	// one is a value that is not a reference.
	frameCastRe = regexp.MustCompile(`\([A-Za-z_]\w*[ \t]*\*+\)`)
	// frameStackDeclRe reads a stack-promoted object: the struct, by value.
	frameStackDeclRe = regexp.MustCompile(`(?:^|[;{}(\n])[ \t]*([A-Za-z_]\w*)[ \t]+(_stack_[A-Za-z_]\w*)[ \t]*;`)
	// offsetofRe reads the generated assertions about a class's reference
	// fields: the offset the collector walks, and the name it walks to.
	offsetofRe = regexp.MustCompile(`offsetof\(struct (\w+), (\w+)\)`)
	// framePutHead is the front of a registration.
	framePutHead = "ty_frame_put(&_tyfr, "
)

// frameRegAt reports whether a registration of `name` follows position `i`.
func frameRegAt(s string, i int, name string) bool {
	tail := strings.TrimLeft(s[i:], " \t")
	if !strings.HasPrefix(tail, framePutHead) {
		return false
	}
	rest := tail[len(framePutHead):]
	k := 0
	for k < len(rest) && rest[k] >= '0' && rest[k] <= '9' {
		k++
	}
	if k == 0 {
		return false
	}
	return strings.HasPrefix(rest[k:], ", (void*)&"+name+");")
}

// cDeclEnd returns the offset just past the `;` that ends a declaration whose
// head ends at `i` (the `=`), tracking brackets, braces and literals so that
// the `;` it stops at is the declaration's own.
func cDeclEnd(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '"', '\'':
			q := s[j]
			for j++; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == q {
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

// frameMentions reports whether an expression names one of `names`.
func frameMentions(expr string, names map[string]bool) bool {
	for _, id := range identRe.FindAllString(expr, -1) {
		if names[id] {
			return true
		}
	}
	return false
}

var identRe = regexp.MustCompile(`[A-Za-z_]\w*`)

// frameSnippet is the first line of a declaration, for a failure message.
func frameSnippet(body string, i int) string {
	s := body[i:]
	if n := strings.IndexByte(s, '\n'); n >= 0 {
		s = s[:n]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return fmt.Sprintf("%q", s)
}
