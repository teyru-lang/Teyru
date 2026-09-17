package codegen

import (
	"strings"
	"testing"
)

// The two units of a split build, cut down to what the scan reads: a class
// record per class, a vtable per class, a method whose body dispatches, and the
// string literal the emitter lays out with a `_Static_assert` under it.
//
// The program's unit is the shape that matters: a unit of a split build defines
// everything with external linkage, so the scan reads a definition from any
// line that starts one, and the assertion under a literal is a line that starts
// like a function -- a name, a parenthesis, an argument list -- and ends like a
// declaration. Read as a function it has no body, so the scan that looks for the
// brace closing it runs on into the definitions below and swallows them.
const (
	pruneLibrary = `tyclass cls_teyru_Object = {
  .name = "teyru.Object", .id = 1, .flags = 0,
  .super = NULL, .niface = 0, .ifaces = if_teyru_Object,
  .nvt = 8, .vtable = vt_teyru_Object, .clinit = NULL,
};
tyclass* if_teyru_Object[] = {NULL};
void M_teyru_Object_toString_0(void* this) {
}
`
	pruneProgram = `tyclass cls_Main = {
  .name = "Main", .id = 2, .flags = 0,
  .super = &cls_teyru_Object, .niface = 0, .ifaces = if_Main,
  .nvt = 8, .vtable = vt_Main, .clinit = NULL,
};
tyclass* if_Main[] = {NULL};
struct S0_s { tystr h; char b[12]; int32_t bc[1]; };
struct S0_s S0 = {{&cls_teyru_Object, 11, 11, 1u}, "hello world", {0}};
_Static_assert(offsetof(struct S0_s, b) == sizeof(tystr), "a literal's bytes follow its header");
_Static_assert(offsetof(struct S0_s, bc) == TY_STR_BC_OFF(11), "a literal's breadcrumbs sit where TY_STR_BC looks");
void M_Main_main_0(C_Main* this) {
  ((void(*)(C_Main*))((this)->obj.cls->vtable[7]))((C_Main*)this);
  ((void(*)(C_Main*))((this)->obj.cls->vtable[7]))((C_Main*)this);
}
void* vt_Main[8] = {(void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0, (void*)M_Main_main_0};
int main(int argc, char** argv) {
  M_Main_main_0(0);
  return 0;
}
`
)

// A slot a live dispatch reads is a slot the program jumps through at run time,
// and pruning it to NULL is a crash rather than a wrong answer. What the scan
// has to survive is a definition it cannot read: an earlier one whose end it
// guesses wrong takes the definitions after it out of the reachability walk with
// it, and every dispatch written below them is lost.
func TestPruneKeepsWhatADefinitionAfterAnAssertionDispatches(t *testing.T) {
	rewritten, _ := pruneVtablesWith(pruneLibrary, pruneProgram)
	vt := lineWith(t, rewritten, "void* vt_Main[8]")
	if strings.Contains(vt, "vtable") {
		t.Fatalf("the scan did not rewrite the table: %q", vt)
	}
	slots := strings.Split(strings.TrimSuffix(strings.TrimPrefix(vt, "void* vt_Main[8] = {"), "};"), ", ")
	if len(slots) != 8 {
		t.Fatalf("the table came back with %d slots: %q", len(slots), vt)
	}
	// Slot 7 is the one the live method dispatches through.
	if slots[7] == "NULL" {
		t.Errorf("the slot a reached method dispatches through was dropped: %q", vt)
	}
	// And a slot nothing dispatches through is the thing pruning is for, so the
	// check above is not one that would pass with the scan switched off.
	if slots[4] != "NULL" {
		t.Errorf("a slot no dispatch reads was kept: %q", vt)
	}
	// What comes back is the program's unit: the library's lines were read and
	// are not returned, because the library's unit is the one a cache keeps and
	// carries no vtables to rewrite.
	if !strings.Contains(rewritten, "void M_Main_main_0") {
		t.Error("the program's own definitions are not in the result")
	}
	if strings.Contains(rewritten, "cls_teyru_Object = {") {
		t.Error("the library's part came back with the program's")
	}
}

// lineWith returns the one line of src that starts with prefix.
func lineWith(t *testing.T, src, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starts with %q", prefix)
	return ""
}
