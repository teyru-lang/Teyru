package codegen

import (
	"testing"
)

// TestRuntimeProtos pins the prototypes the LLVM back end reads out of the
// runtime's header. A call made with the wrong types is not a build failure but
// wrong output -- a 32-bit argument handed to a helper that reads 64 -- so the
// reader is worth a test of its own.
func TestRuntimeProtos(t *testing.T) {
	cases := []struct {
		name   string
		ret    string
		params []string
	}{
		{"ty_alloc_slow", "ptr", []string{"i64"}},
		{"ty_array_new", "ptr", []string{"i64", "i64"}},
		{"ty_str_concat", "ptr", []string{"ptr", "ptr"}},
		{"ty_str_eq", "i32", []string{"ptr", "ptr"}},
		{"ty_str_of_long", "ptr", []string{"i64"}},
		{"ty_str_of_char", "ptr", []string{"i16"}},
		{"ty_str_of_bool", "ptr", []string{"i32"}},
		{"ty_str_of_double", "ptr", []string{"double"}},
		{"ty_str_of_float", "ptr", []string{"float"}},
		{"ty_str_of_obj", "ptr", []string{"ptr"}},
		{"ty_str_intern", "ptr", []string{"ptr"}},
		{"ty_d2i", "i32", []string{"double"}},
		{"ty_d2l", "i64", []string{"double"}},
		{"ty_div_int", "i32", []string{"i32", "i32"}},
		{"ty_div_long", "i64", []string{"i64", "i64"}},
		{"ty_rem_int", "i32", []string{"i32", "i32"}},
		{"ty_rem_long", "i64", []string{"i64", "i64"}},
		{"ty_npe", "ptr", nil},
		{"ty_aioobe", "ptr", []string{"i64", "i64"}},
		{"ty_checkcast", "ptr", []string{"ptr", "ptr"}},
		{"ty_instanceof", "i32", []string{"ptr", "ptr"}},
		{"ty_object_hash", "i32", []string{"ptr"}},
		{"ty_throw", "void", []string{"ptr"}},
		{"ty_unimplemented", "void", []string{"ptr"}},
		{"ty_clinit", "void", []string{"ptr"}},
		{"ty_gc_register_static", "void", []string{"ptr"}},
		{"ty_safepoint_slow", "void", nil},
		{"ty_thread_join_all", "void", nil},
		{"ty_init", "void", nil},
		{"ty_println_str", "void", []string{"ptr"}},
		{"ty_ps_println_int", "void", []string{"ptr", "i64"}},
		{"ty_class_of_cls", "ptr", []string{"ptr", "ptr"}},
		{"ty_class_name", "ptr", []string{"ptr"}},
		{"ty_make_ex", "ptr", []string{"ptr", "ptr"}},
		{"ty_illegal_state", "ptr", []string{"ptr"}},
		{"ty_assertfail", "ptr", []string{"ptr"}},
		{"ty_arraystore", "ptr", nil},
		{"ty_negarr", "ptr", nil},
		{"ty_arith", "ptr", []string{"ptr"}},
	}
	for _, c := range cases {
		got, ok := rtProtoOf(c.name)
		if !ok {
			t.Errorf("%s: the header parser did not find it", c.name)
			continue
		}
		if got.ret != c.ret {
			t.Errorf("%s: result is %s, want %s", c.name, got.ret, c.ret)
		}
		if len(got.params) != len(c.params) {
			t.Errorf("%s: %d parameters, want %d (%v)", c.name, len(got.params), len(c.params), got.params)
			continue
		}
		for i := range c.params {
			if got.params[i] != c.params[i] {
				t.Errorf("%s: parameter %d is %s, want %s", c.name, i, got.params[i], c.params[i])
			}
		}
	}
}

// TestRuntimeProtosAreDeclared checks that the parser is reading declarations
// and not, say, every identifier in the file: a helper that is defined inline in
// the header is not callable through a declaration, and the back end has to
// inline it itself.
func TestRuntimeProtosAreDeclared(t *testing.T) {
	for _, name := range []string{"ty_alloc", "ty_safepoint", "ty_array_store_ref", "ty_itab"} {
		if _, ok := rtProtoOf(name); ok {
			t.Errorf("%s is defined inline in the header; a module cannot call it through a declaration", name)
		}
	}
}
