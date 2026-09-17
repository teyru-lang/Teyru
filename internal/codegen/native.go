package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teyru-lang/Teyru/internal/ast"
	"github.com/teyru-lang/Teyru/internal/sema"
	"github.com/teyru-lang/Teyru/internal/util"
)

// nativeFn maps a prelude native method onto a runtime helper.
//
// Convention: helpers that take or return object values use `void*`; the
// emitter casts at the call site, so the generated C stays warning-free.
type nativeFn struct {
	fn   string // C helper
	recv string // C cast applied to the receiver ("" for static methods)
	// classHint names a prelude class whose generated tyclass the call site
	// passes to fn as a trailing argument. Object.getClass() needs it: the
	// runtime builds a Class object from a tyclass this header-less C file
	// cannot name.
	classHint string
	// clsArg names a prelude class whose generated tyclass is appended as the
	// call's last argument while fn keeps its own name. classHint cannot do
	// this: it replaces the helper with ty_class_of_cls, which is what one
	// caller needed. Reflection's factories need the other shape -- the runtime
	// builds a Class object and a header-less C file cannot name the program's
	// teyru.Class.
	clsArg string
	// selClass and selMethod name an interface method whose dispatch selector is
	// appended as the call's last argument. The runtime cannot know a selector:
	// the compiler assigns them per program, and a runtime compiled once cannot
	// carry a number only one program uses. Thread.start() is what needs it --
	// the thread it creates has to call the program's own run(), which is the
	// same interface dispatch the generated code makes.
	selClass  string
	selMethod string
	// proto is the C prototype of fn, repeated as an `extern` at every call
	// site. A table that carries its own declaration needs nothing added to
	// tyrt.h, which is what lets a feature (networking, say) ship its native
	// methods in a file of its own. An entry with no proto relies on the
	// prototype tyrt.h already declares.
	proto string
}

// streamTwin is the stream-aware twin of a PrintStream print helper: the same
// output, written to whichever descriptor the receiver names.
type streamTwin struct {
	name  string // C helper taking the receiver first
	proto string // its prototype, repeated at the call site
}

// printStreamClass is the prelude class whose print methods are stream aware.
const printStreamClass = "teyru.PrintStream"

// classOfProto is the prototype of the helper Object.getClass() calls. It is
// not in tyrt.h because the generated program is the only caller.
const classOfProto = "void *ty_class_of_cls(void *, tyclass *)"

// streamTwins maps a stdout helper onto its stream-aware twin, which takes the
// PrintStream as its first argument and writes to the descriptor in the
// stream's `target` field. The helpers are not declared in tyrt.h (the shared
// header is not this package's to change), so the call site declares them
// inline -- the same statement-expression idiom the generated code already uses
// for temporaries.
var streamTwins = map[string]streamTwin{
	"ty_println_void":   {"ty_ps_println_void", "void ty_ps_println_void(void *)"},
	"ty_println_str":    {"ty_ps_println_str", "void ty_ps_println_str(void *, tystr *)"},
	"ty_println_int":    {"ty_ps_println_int", "void ty_ps_println_int(void *, int64_t)"},
	"ty_println_double": {"ty_ps_println_double", "void ty_ps_println_double(void *, double)"},
	"ty_println_float":  {"ty_ps_println_float", "void ty_ps_println_float(void *, float)"},
	"ty_println_bool":   {"ty_ps_println_bool", "void ty_ps_println_bool(void *, int32_t)"},
	"ty_println_char":   {"ty_ps_println_char", "void ty_ps_println_char(void *, uint16_t)"},
	"ty_println_obj":    {"ty_ps_println_obj", "void ty_ps_println_obj(void *, void *)"},
	"ty_print_str":      {"ty_ps_print_str", "void ty_ps_print_str(void *, tystr *)"},
	"ty_print_int":      {"ty_ps_print_int", "void ty_ps_print_int(void *, int64_t)"},
	"ty_print_double":   {"ty_ps_print_double", "void ty_ps_print_double(void *, double)"},
	"ty_print_float":    {"ty_ps_print_float", "void ty_ps_print_float(void *, float)"},
	"ty_print_bool":     {"ty_ps_print_bool", "void ty_ps_print_bool(void *, int32_t)"},
	"ty_print_char":     {"ty_ps_print_char", "void ty_ps_print_char(void *, uint16_t)"},
	"ty_print_obj":      {"ty_ps_print_obj", "void ty_ps_print_obj(void *, void *)"},
}

// psTwin is the stream-aware twin of a PrintStream print method, or nil when
// the method is not one. A class that extends PrintStream forces codegen down
// the stub path (emit_synth builds that call from the table entry alone and
// cannot declare the twin, so nf.fn has to be a function tyrt.h already
// declares), and in such a program every stream writes to stdout again --
// System.err included, because its receiver is a PrintStream too. That is what
// the language did before the field existed, so no program gets worse; the
// complete fix is to declare the twins in tyrt.h and point the table entries at
// them, which is a change to a header this package does not own.
func (e *Emitter) psTwin(m *ast.Method, nf nativeFn) *streamTwin {
	if m.Owner == nil || m.Owner != e.prog.LookupClass(printStreamClass) {
		return nil
	}
	if t, ok := streamTwins[nf.fn]; ok {
		return &t
	}
	return nil
}

// specialNew maps classes whose allocation is owned by the runtime.
var specialNew = map[string]string{
	"StringBuilder": "ty_sb_new",
	"StringBuffer":  "ty_sb_new",
}

// extraNative holds the tables other files contribute, so a feature can add
// its own native methods without editing the core table: every entry here is
// merged into nativeTable at startup. A duplicate key is a programming error
// rather than a silent override, since two features claiming one Teyru method
// would otherwise pick a winner by file order.
var extraNative []map[string]nativeFn

func init() {
	for _, t := range extraNative {
		for k, v := range t {
			if _, dup := nativeTable[k]; dup {
				panic("native method declared twice: " + k)
			}
			nativeTable[k] = v
		}
	}
}

var nativeTable = map[string]nativeFn{
	// ---- Object
	"Object.toString()":     {fn: "ty_object_tostring", recv: "void*"},
	"Object.hashCode()":     {fn: "ty_obj_hash", recv: "void*"},
	"Object.equals(Object)": {fn: "ty_obj_eq", recv: "void*"},
	"Object.getClass()":     {fn: "ty_class_of", recv: "void*", classHint: "teyru.Class"},
	// The monitor operations `synchronized` is built on: the lock these take is
	// the one ty_sync_enter takes on the same receiver, so a thread that is
	// inside wait() has given up the monitor of the object it is notified
	// through. tyrt_thread.c has the monitor itself.
	"Object.wait(J)":     {fn: "ty_mon_wait", recv: "void*"},
	"Object.notify()":    {fn: "ty_mon_notify", recv: "void*"},
	"Object.notifyAll()": {fn: "ty_mon_notify_all", recv: "void*"},
	"Class.getName()":    {fn: "ty_class_name", recv: "void*"},

	// ---- java.lang.Thread
	//
	// lib/35_thread.teyru declares these. start0 is the one entry that needs a
	// selector as well as its arguments: it runs the thread, and the thread runs
	// the program's own run(), which only the compiler knows how to dispatch.
	"Thread.start0(Thread,J,String)": {
		fn: "ty_thread_start0", selClass: "teyru.Runnable", selMethod: "run",
	},
	"Thread.join0(J)":      {fn: "ty_thread_join"},
	"Thread.alive0(J)":     {fn: "ty_thread_alive"},
	"Thread.nextId0()":     {fn: "ty_thread_next_id"},
	"Thread.current0()":    {fn: "ty_thread_current_obj"},
	"Thread.currentId0()":  {fn: "ty_thread_current_id"},
	"Thread.bind0(Thread)": {fn: "ty_thread_bind"},
	"Thread.sleep(J)":      {fn: "ty_thread_sleep_ms"},
	"Thread.yield()":       {fn: "ty_thread_yield"},

	// ---- java.lang.reflect
	//
	// One entry per prelude native in lib/26_reflect.teyru. The receivers are
	// static methods taking a class handle as an int64_t, or a member object
	// whose fields the C side never reads -- it is handed the handle, the table
	// and the index, which is what a member object is.
	"Class.handleOf(Class)":                      {fn: "ty_class_handle"},
	"Class.simpleNameOf(J)":                      {fn: "ty_class_simplename"},
	"Class.modsOf(J)":                            {fn: "ty_class_mods"},
	"Class.isPrimOf(J)":                          {fn: "ty_class_isprim"},
	"Class.isArrayOf(J)":                         {fn: "ty_class_isarray"},
	"Class.isEnumOf(J)":                          {fn: "ty_class_isenum"},
	"Class.isRecordOf(J)":                        {fn: "ty_class_isrecord"},
	"Class.isInterfaceOf(J)":                     {fn: "ty_class_isinterface"},
	"Class.superOf(J)":                           {fn: "ty_class_superof"},
	"Class.isInstanceOf(J,Object)":               {fn: "ty_class_isinstance"},
	"Class.assignableOf(J,J)":                    {fn: "ty_class_assignable"},
	"Class.ifaceCountOf(J)":                      {fn: "ty_class_ifacecount"},
	"Class.ifaceAtOf(J,I)":                       {fn: "ty_class_ifaceat"},
	"Class.fieldCountOf(J,Z)":                    {fn: "ty_class_fieldcount"},
	"Class.fieldNameOf(J,Z,I)":                   {fn: "ty_field_name"},
	"Class.fieldTypeOf(J,Z,I)":                   {fn: "ty_field_type"},
	"Class.fieldElemOf(J,Z,I)":                   {fn: "ty_field_elem"},
	"Class.fieldOwnerOf(J,Z,I)":                  {fn: "ty_field_owner"},
	"Class.fieldModsOf(J,Z,I)":                   {fn: "ty_field_mods"},
	"Class.fieldGetOf(J,Z,I,Object)":             {fn: "ty_field_get"},
	"Class.fieldSetOf(J,Z,I,Object,Object,Z)":    {fn: "ty_field_set"},
	"Class.methodCountOf(J,Z)":                   {fn: "ty_class_methodcount"},
	"Class.methodNameOf(J,Z,I)":                  {fn: "ty_method_name"},
	"Class.methodOwnerOf(J,Z,I)":                 {fn: "ty_method_owner"},
	"Class.methodModsOf(J,Z,I)":                  {fn: "ty_method_mods"},
	"Class.methodRetOf(J,Z,I)":                   {fn: "ty_method_ret"},
	"Class.methodParamCountOf(J,Z,I)":            {fn: "ty_method_paramcount"},
	"Class.methodParamOf(J,Z,I,I)":               {fn: "ty_method_param"},
	"Class.methodInvokeOf(J,Z,I,Object,AObject)": {fn: "ty_method_invoke"},
	"Class.ctorCountOf(J)":                       {fn: "ty_class_ctorcount"},
	"Class.ctorModsOf(J,I)":                      {fn: "ty_ctor_mods"},
	"Class.ctorParamCountOf(J,I)":                {fn: "ty_ctor_paramcount"},
	"Class.ctorParamOf(J,I,I)":                   {fn: "ty_ctor_param"},
	"Class.ctorNewOf(J,I,AObject)":               {fn: "ty_ctor_new"},
	"Class.newInstOf(J)":                         {fn: "ty_class_newinst"},
	"Class.enumCountOf(J)":                       {fn: "ty_class_enumcount"},
	"Class.enumAtOf(J,I)":                        {fn: "ty_class_enumat"},
	"Sha1.of(AB)":                                {fn: "ty_sha1_bytes"},
	"Class.annCountOf(J)":                        {fn: "ty_class_anncount"},
	"Class.annAtOf(J,I)":                         {fn: "ty_class_annat"},
	"Class.fieldAnnCountOf(J,Z,I)":               {fn: "ty_field_anncount"},
	"Class.fieldAnnAtOf(J,Z,I,I)":                {fn: "ty_field_annat"},
	"Class.methodAnnCountOf(J,Z,I)":              {fn: "ty_method_anncount"},
	"Class.paramAnnCountOf(J,Z,I,I)":             {fn: "ty_method_paramanncount"},
	"Class.paramNameOf(J,Z,I,I)":                 {fn: "ty_method_paramname"},
	"Class.ctorParamNameOf(J,I,I)":               {fn: "ty_ctor_paramname"},
	"Class.paramAnnAtOf(J,Z,I,I,I)":              {fn: "ty_method_paramannat"},
	"Class.methodAnnAtOf(J,Z,I,I)":               {fn: "ty_method_annat"},
	"Class.ctorAnnCountOf(J,I)":                  {fn: "ty_ctor_anncount"},
	"Class.ctorParamAnnCountOf(J,I,I)":           {fn: "ty_ctor_paramanncount"},
	"Class.ctorParamAnnAtOf(J,I,I,I)":            {fn: "ty_ctor_paramannat"},
	"Class.ctorAnnAtOf(J,I,I)":                   {fn: "ty_ctor_annat"},
	"Annotation.typeOf(J)":                       {fn: "ty_ann_type"},
	"Annotation.argCountOf(J)":                   {fn: "ty_ann_argcount"},
	"Annotation.argNameOf(J,I)":                  {fn: "ty_ann_argname"},
	"Annotation.argKindOf(J,I)":                  {fn: "ty_ann_argkind"},
	"Annotation.argIntOf(J,I)":                   {fn: "ty_ann_argint"},
	"Annotation.argDoubleOf(J,I)":                {fn: "ty_ann_argdouble"},
	"Annotation.argStrOf(J,I)":                   {fn: "ty_ann_argstr"},
	"Annotation.argClassOf(J,I)":                 {fn: "ty_ann_argclass"},
	"Annotation.argIndexOf(J,String)":            {fn: "ty_ann_argindex"},
	"Annotation.sameOf(J,J)":                     {fn: "ty_ann_same"},
	"Class.makeOf(J)":                            {fn: "ty_class_make", clsArg: "teyru.Class"},
	"Array.newArrayOf(J,I)":                      {fn: "ty_reflect_array_new"},
	"Array.lengthOf(Object)":                     {fn: "ty_reflect_array_len"},
	"Array.getOf(Object,I)":                      {fn: "ty_reflect_array_get"},
	"Array.setOf(Object,I,Object)":               {fn: "ty_reflect_array_set"},
	"Reflect.toInt(Object)":                      {fn: "ty_rv_int"},
	"Reflect.toLong(Object)":                     {fn: "ty_rv_long"},
	"Reflect.toDouble(Object)":                   {fn: "ty_rv_double"},
	"Reflect.toFloat(Object)":                    {fn: "ty_rv_float"},
	"Reflect.toShort(Object)":                    {fn: "ty_rv_short"},
	"Reflect.toByte(Object)":                     {fn: "ty_rv_byte"},
	"Reflect.toChar(Object)":                     {fn: "ty_rv_char"},
	"Reflect.toBool(Object)":                     {fn: "ty_rv_bool"},

	// ---- String
	"String.length()":           {fn: "ty_str_len", recv: "tystr*"},
	"String.isEmpty()":          {fn: "ty_str_isempty", recv: "tystr*"},
	"String.charAt(I)":          {fn: "ty_str_charat", recv: "tystr*"},
	"String.equals(Object)":     {fn: "ty_str_eq_obj", recv: "tystr*"},
	"String.hashCode()":         {fn: "ty_str_hash", recv: "tystr*"},
	"String.indexOf(String)":    {fn: "ty_str_indexof", recv: "tystr*"},
	"String.substring(I)":       {fn: "ty_str_sub_from", recv: "tystr*"},
	"String.substring(I,I)":     {fn: "ty_str_sub", recv: "tystr*"},
	"String.toUpperCase()":      {fn: "ty_str_upper", recv: "tystr*"},
	"String.toLowerCase()":      {fn: "ty_str_lower", recv: "tystr*"},
	"String.trim()":             {fn: "ty_str_trim", recv: "tystr*"},
	"String.contains(String)":   {fn: "ty_str_contains", recv: "tystr*"},
	"String.startsWith(String)": {fn: "ty_str_starts", recv: "tystr*"},
	"String.endsWith(String)":   {fn: "ty_str_ends", recv: "tystr*"},
	"String.replace(C,C)":       {fn: "ty_str_replace", recv: "tystr*"},
	"String.compareTo(String)":  {fn: "ty_str_cmp", recv: "tystr*"},
	"String.concat(String)":     {fn: "ty_str_concat", recv: "tystr*"},
	"String.toString()":         {fn: "ty_str_ident", recv: "tystr*"},
	// constructors are named <init> by the parser
	"String.<init>(String)": {fn: "ty_str_copy", recv: "tystr*"},
	/* A string out of code units (char[]) or code points (int[]): the two
	   constructors differ in what one element of the array is, which is what
	   the key already says. */
	"String.<init>(AC)":     {fn: "ty_str_of_chars", recv: "tyarr*"},
	"String.<init>(AC,I,I)": {fn: "ty_str_of_chars_part", recv: "tyarr*"},
	"String.<init>(AI,I,I)": {fn: "ty_str_of_ints", recv: "tyarr*"},
	/* The byte-array constructors decode: bytes from outside are UTF-8 and
	   anything ill-formed in them is U+FFFD, the way java.lang.String's own
	   constructors read them. */
	"String.<init>(AB)":      {fn: "ty_str_of_bytes_all", recv: "tyarr*"},
	"String.<init>(AB,I,I)":  {fn: "ty_str_of_bytes", recv: "tyarr*"},
	"String.valueOf(I)":      {fn: "ty_str_of_int"},
	"String.valueOf(J)":      {fn: "ty_str_of_long"},
	"String.valueOf(D)":      {fn: "ty_str_of_double"},
	"String.valueOf(F)":      {fn: "ty_str_of_float"},
	"String.valueOf(Z)":      {fn: "ty_str_of_bool"},
	"String.valueOf(C)":      {fn: "ty_str_of_char"},
	"String.valueOf(Object)": {fn: "ty_str_of_obj"},

	// ---- boxed primitives

	"Integer.max(I,I)": {fn: "ty_max_int"},
	"Integer.min(I,I)": {fn: "ty_min_int"},

	"Long.max(J,J)": {fn: "ty_max_long"},
	"Long.min(J,J)": {fn: "ty_min_long"},

	"Double.toString(D)": {fn: "ty_str_of_double"},
	"Double.isNaN(D)":    {fn: "ty_isnan"},
	"Double.max(D,D)":    {fn: "ty_math_max_double"},
	"Double.min(D,D)":    {fn: "ty_math_min_double"},

	"Float.toString(F)": {fn: "ty_str_of_float"},
	"Float.max(F,F)":    {fn: "ty_math_max_float"},
	"Float.min(F,F)":    {fn: "ty_math_min_float"},

	"Boolean.toString(Z)": {fn: "ty_str_of_bool"},

	"Character.isDigit(C)":      {fn: "ty_is_digit"},
	"Character.isLetter(C)":     {fn: "ty_is_letter"},
	"Character.isWhitespace(C)": {fn: "ty_is_whitespace"},

	// ---- Math
	"Math.abs(I)":   {fn: "ty_abs_int"},
	"Math.abs(J)":   {fn: "ty_abs_long"},
	"Math.abs(D)":   {fn: "ty_abs_double"},
	"Math.max(I,I)": {fn: "ty_max_int"},
	"Math.min(I,I)": {fn: "ty_min_int"},
	"Math.max(J,J)": {fn: "ty_max_long"},
	"Math.min(J,J)": {fn: "ty_min_long"},
	"Math.max(D,D)": {fn: "ty_math_max_double"},
	"Math.min(D,D)": {fn: "ty_math_min_double"},
	"Math.sqrt(D)":  {fn: "sqrt"},
	"Math.pow(D,D)": {fn: "pow"},
	"Math.floor(D)": {fn: "floor"},
	"Math.ceil(D)":  {fn: "ceil"},
	"Math.round(D)": {fn: "ty_math_round_long"},
	"Math.random()": {fn: "ty_random"},

	// ---- System
	"System.currentTimeMillis()":            {fn: "ty_millis"},
	"System.nanoTime()":                     {fn: "ty_nanos"},
	"System.exit(I)":                        {fn: "ty_exit"},
	"System.arraycopy(Object,I,Object,I,I)": {fn: "ty_arraycopy"},

	// ---- PrintStream
	"PrintStream.println()":       {fn: "ty_println_void"},
	"PrintStream.println(String)": {fn: "ty_println_str"},
	"PrintStream.println(I)":      {fn: "ty_println_int"},
	"PrintStream.println(J)":      {fn: "ty_println_int"},
	"PrintStream.println(D)":      {fn: "ty_println_double"},
	"PrintStream.println(F)":      {fn: "ty_println_float"},
	"PrintStream.println(Z)":      {fn: "ty_println_bool"},
	"PrintStream.println(C)":      {fn: "ty_println_char"},
	"PrintStream.println(Object)": {fn: "ty_println_obj"},
	"PrintStream.print(String)":   {fn: "ty_print_str"},
	"PrintStream.print(I)":        {fn: "ty_print_int"},
	"PrintStream.print(J)":        {fn: "ty_print_int"},
	"PrintStream.print(D)":        {fn: "ty_print_double"},
	"PrintStream.print(F)":        {fn: "ty_print_float"},
	"PrintStream.print(Z)":        {fn: "ty_print_bool"},
	"PrintStream.print(C)":        {fn: "ty_print_char"},
	"PrintStream.print(Object)":   {fn: "ty_print_obj"},

	// ---- java.io.IO (implicitly imported in compact source files)
	"IO.println()":       {fn: "ty_println_void"},
	"IO.println(String)": {fn: "ty_println_str"},
	"IO.println(I)":      {fn: "ty_println_int"},
	"IO.println(J)":      {fn: "ty_println_int"},
	"IO.println(D)":      {fn: "ty_println_double"},
	"IO.println(F)":      {fn: "ty_println_float"},
	"IO.println(Z)":      {fn: "ty_println_bool"},
	"IO.println(C)":      {fn: "ty_println_char"},
	"IO.println(Object)": {fn: "ty_println_obj"},
	"IO.print(String)":   {fn: "ty_print_str"},
	"IO.print(I)":        {fn: "ty_print_int"},
	"IO.print(J)":        {fn: "ty_print_int"},
	"IO.print(D)":        {fn: "ty_print_double"},
	"IO.print(F)":        {fn: "ty_print_float"},
	"IO.print(Z)":        {fn: "ty_print_bool"},
	"IO.print(C)":        {fn: "ty_print_char"},
	"IO.print(Object)":   {fn: "ty_print_obj"},
	"IO.readln()":        {fn: "ty_readln"},

	// ---- StringBuilder
	"StringBuilder.append(Object)": {fn: "ty_sb_append_obj", recv: "void*"},
	"StringBuilder.append(I)":      {fn: "ty_sb_append_int", recv: "void*"},
	"StringBuilder.append(J)":      {fn: "ty_sb_append_long", recv: "void*"},
	"StringBuilder.append(C)":      {fn: "ty_sb_append_char", recv: "void*"},
	"StringBuilder.append(D)":      {fn: "ty_sb_append_double", recv: "void*"},
	"StringBuilder.append(Z)":      {fn: "ty_sb_append_bool", recv: "void*"},
	"StringBuilder.toString()":     {fn: "ty_sb_tostring", recv: "void*"},
	"StringBuilder.length()":       {fn: "ty_sb_len", recv: "void*"},
	// the two constructors that take an argument: codegen only special-cases
	// `new StringBuilder()`, so these allocate through the ordinary path and
	// the buffer has to be created by hand.
	"StringBuilder.init(I)": {fn: "ty_sb_init", recv: "void*"},
	"StringBuffer.init(I)":  {fn: "ty_sb_init", recv: "void*"},

	// ---- Enum
	"Enum.ordinal()":      {fn: "ty_enum_ordinal", recv: "void*"},
	"Enum.name()":         {fn: "ty_enum_name", recv: "void*"},
	"Enum.toString()":     {fn: "ty_enum_name", recv: "void*"},
	"Enum.hashCode()":     {fn: "ty_enum_ordinal", recv: "void*"},
	"Enum.equals(Object)": {fn: "ty_obj_eq", recv: "void*"},
	"Enum.compareTo(O)":   {fn: "ty_enum_compare", recv: "void*"},

	// ---- the rest of java.lang
	//
	// Everything below completes the prelude against Java's java.lang: the
	// float half of Math, the trig and rounding functions, the character
	// classifiers, System's environment and property lookups, the radix and
	// bit-twiddling surface of the wrappers, the string methods that were
	// missing, and StringBuilder/StringBuffer. Every helper is declared in
	// internal/runtime/src/tyrt.h, so no entry needs a proto.

	"Math.abs(F)":             {fn: "ty_abs_float"},
	"Math.max(F,F)":           {fn: "ty_math_max_float"},
	"Math.min(F,F)":           {fn: "ty_math_min_float"},
	"Math.round(F)":           {fn: "ty_math_round_int"},
	"Math.cbrt(D)":            {fn: "ty_math_cbrt"},
	"Math.exp(D)":             {fn: "exp"},
	"Math.log(D)":             {fn: "log"},
	"Math.log10(D)":           {fn: "log10"},
	"Math.sin(D)":             {fn: "sin"},
	"Math.cos(D)":             {fn: "cos"},
	"Math.tan(D)":             {fn: "tan"},
	"Math.asin(D)":            {fn: "asin"},
	"Math.acos(D)":            {fn: "acos"},
	"Math.atan(D)":            {fn: "atan"},
	"Math.atan2(D,D)":         {fn: "atan2"},
	"Math.hypot(D,D)":         {fn: "hypot"},
	"Math.sinh(D)":            {fn: "sinh"},
	"Math.cosh(D)":            {fn: "cosh"},
	"Math.tanh(D)":            {fn: "tanh"},
	"Math.IEEEremainder(D,D)": {fn: "remainder"},
	"Math.copySign(D,D)":      {fn: "copysign"},
	"Math.copySign(F,F)":      {fn: "copysignf"},
	"Math.nextAfter(D,D)":     {fn: "nextafter"},
	"Math.nextAfter(F,D)":     {fn: "nextafterf"},
	"Math.fma(D,D,D)":         {fn: "fma"},
	"Math.toRadians(D)":       {fn: "ty_math_to_radians"},
	"Math.toDegrees(D)":       {fn: "ty_math_to_degrees"},
	"Math.rint(D)":            {fn: "rint"},
	"Math.signum(D)":          {fn: "ty_signum_double"},
	"Math.signum(F)":          {fn: "ty_signum_float"},
	"Math.floorDiv(I,I)":      {fn: "ty_math_floor_div_int"},
	"Math.floorDiv(J,J)":      {fn: "ty_math_floor_div_long"},
	"Math.floorMod(I,I)":      {fn: "ty_math_floor_mod_int"},
	"Math.floorMod(J,J)":      {fn: "ty_math_floor_mod_long"},

	"System.identityHashCode(Object)": {fn: "ty_identity_hash"},
	"System.getenv(String)":           {fn: "ty_getenv", recv: "tystr*"},
	"System.getProperty(String)":      {fn: "ty_get_property", recv: "tystr*"},
	"System.gc()":                     {fn: "ty_gc"},

	// System.in: its two methods take no arguments, so the receiver is the
	// only thing the helpers see.
	"InputStream.read()":   {fn: "ty_in_read", recv: "void*"},
	"InputStream.readln()": {fn: "ty_in_readln", recv: "void*"},

	// ---- Character
	"Character.isLetterOrDigit(C)": {fn: "ty_is_letter_or_digit"},
	"Character.isAlphabetic(C)":    {fn: "ty_is_alphabetic"},
	"Character.isUpperCase(C)":     {fn: "ty_is_upper_case"},
	"Character.isLowerCase(C)":     {fn: "ty_is_lower_case"},
	"Character.toUpperCase(C)":     {fn: "ty_char_upper"},
	"Character.toLowerCase(C)":     {fn: "ty_char_lower"},
	"Character.getNumericValue(C)": {fn: "ty_char_numeric"},
	"Character.digit(C,I)":         {fn: "ty_char_digit"},

	// ---- Byte / Short / Boolean
	// Byte.equals and Short.equals were the two wrappers with no entry at all,
	// which is worse than a wrong one: nativeCall returns the literal 0 for a
	// key it does not know, so Byte.valueOf(1).equals(Byte.valueOf(1)) was
	// compiled to `false` with nothing in the generated C to show for it.

	// ---- Integer
	"Integer.parsable(String,I)":       {fn: "ty_str_parsable_int", recv: "tystr*"},
	"Integer.parseIntDigits(String,I)": {fn: "ty_str_toint_radix", recv: "tystr*"},
	"Integer.toString(I,I)":            {fn: "ty_radix_string_int"},
	"Integer.toUnsignedString(I,I)":    {fn: "ty_unsigned_string_int"},
	"Integer.compareUnsigned(I,I)":     {fn: "ty_int_cmp_unsigned"},
	"Integer.signum(I)":                {fn: "ty_int_signum"},
	"Integer.bitCount(I)":              {fn: "ty_int_bit_count"},
	"Integer.numberOfLeadingZeros(I)":  {fn: "ty_int_nlz"},
	"Integer.numberOfTrailingZeros(I)": {fn: "ty_int_ntz"},
	"Integer.highestOneBit(I)":         {fn: "ty_int_highest_one"},
	"Integer.lowestOneBit(I)":          {fn: "ty_int_lowest_one"},
	"Integer.reverse(I)":               {fn: "ty_int_reverse"},
	"Integer.reverseBytes(I)":          {fn: "ty_int_reverse_bytes"},
	"Integer.rotateLeft(I,I)":          {fn: "ty_int_rotate_left"},
	"Integer.rotateRight(I,I)":         {fn: "ty_int_rotate_right"},

	// ---- Long
	"Long.parsableLong(String,I)":    {fn: "ty_str_parsable_long", recv: "tystr*"},
	"Long.parseLongDigits(String,I)": {fn: "ty_str_tolong_radix", recv: "tystr*"},
	"Long.toString(J,I)":             {fn: "ty_radix_string_long"},
	"Long.toUnsignedString(J,I)":     {fn: "ty_unsigned_string_long"},
	"Long.compareUnsigned(J,J)":      {fn: "ty_long_cmp_unsigned"},
	"Long.signum(J)":                 {fn: "ty_long_signum"},
	"Long.bitCount(J)":               {fn: "ty_long_bit_count"},
	"Long.numberOfLeadingZeros(J)":   {fn: "ty_long_nlz"},
	"Long.numberOfTrailingZeros(J)":  {fn: "ty_long_ntz"},
	"Long.highestOneBit(J)":          {fn: "ty_long_highest_one"},
	"Long.lowestOneBit(J)":           {fn: "ty_long_lowest_one"},
	"Long.reverse(J)":                {fn: "ty_long_reverse"},
	"Long.reverseBytes(J)":           {fn: "ty_long_reverse_bytes"},
	"Long.rotateLeft(J,I)":           {fn: "ty_long_rotate_left"},
	"Long.rotateRight(J,I)":          {fn: "ty_long_rotate_right"},

	// ---- Float / Double
	"Float.parsableFloat(String)":      {fn: "ty_str_parsable_float", recv: "tystr*"},
	"Float.parseFloatDigits(String)":   {fn: "ty_str_tofloat_val", recv: "tystr*"},
	"Float.isNaN(F)":                   {fn: "ty_float_isnan"},
	"Float.isInfinite(F)":              {fn: "ty_float_is_infinite"},
	"Float.isFinite(F)":                {fn: "ty_float_is_finite"},
	"Float.floatToIntBits(F)":          {fn: "ty_float_bits"},
	"Float.floatToRawIntBits(F)":       {fn: "ty_float_raw_bits"},
	"Float.intBitsToFloat(I)":          {fn: "ty_bits_float"},
	"Double.parsableDouble(String)":    {fn: "ty_str_parsable_double", recv: "tystr*"},
	"Double.parseDoubleDigits(String)": {fn: "ty_str_todouble_val", recv: "tystr*"},
	"Double.isInfinite(D)":             {fn: "ty_double_is_infinite"},
	"Double.isFinite(D)":               {fn: "ty_double_is_finite"},
	"Double.doubleToLongBits(D)":       {fn: "ty_double_bits"},
	"Double.doubleToRawLongBits(D)":    {fn: "ty_double_raw_bits"},
	"Double.longBitsToDouble(J)":       {fn: "ty_bits_double"},

	// ---- String
	"String.isBlank()":                     {fn: "ty_str_isblank", recv: "tystr*"},
	"String.codePointAt(I)":                {fn: "ty_str_code_point_at", recv: "tystr*"},
	"String.codePointBefore(I)":            {fn: "ty_str_code_point_before", recv: "tystr*"},
	"String.codePointCount(I,I)":           {fn: "ty_str_code_point_count", recv: "tystr*"},
	"String.offsetByCodePoints(I,I)":       {fn: "ty_str_offset_by_code_points", recv: "tystr*"},
	"String.getChars(I,I,AC,I)":            {fn: "ty_str_get_chars", recv: "tystr*"},
	"String.regionMatches(Z,I,String,I,I)": {fn: "ty_str_region_matches", recv: "tystr*"},
	"String.equalsIgnoreCase(String)":      {fn: "ty_str_eq_ic", recv: "tystr*"},
	"String.compareToIgnoreCase(String)":   {fn: "ty_str_cmp_ic", recv: "tystr*"},
	"String.startsWith(String,I)":          {fn: "ty_str_starts_from", recv: "tystr*"},
	"String.indexOf(I)":                    {fn: "ty_str_indexof_ch", recv: "tystr*"},
	"String.indexOf(I,I)":                  {fn: "ty_str_indexof_ch_from", recv: "tystr*"},
	"String.indexOf(String,I)":             {fn: "ty_str_indexof_from", recv: "tystr*"},
	"String.lastIndexOf(I)":                {fn: "ty_str_lastindexof_ch", recv: "tystr*"},
	"String.lastIndexOf(I,I)":              {fn: "ty_str_lastindexof_ch_from", recv: "tystr*"},
	"String.lastIndexOf(String)":           {fn: "ty_str_lastindexof", recv: "tystr*"},
	"String.lastIndexOf(String,I)":         {fn: "ty_str_lastindexof_from", recv: "tystr*"},
	"String.repeat(I)":                     {fn: "ty_str_repeat", recv: "tystr*"},
	"String.strip()":                       {fn: "ty_str_strip", recv: "tystr*"},
	"String.stripLeading()":                {fn: "ty_str_strip_leading", recv: "tystr*"},
	"String.stripTrailing()":               {fn: "ty_str_strip_trailing", recv: "tystr*"},
	"String.toCharArray()":                 {fn: "ty_str_tochararray", recv: "tystr*"},
	"String.getBytes()":                    {fn: "ty_str_getbytes", recv: "tystr*"},
	"String.intern()":                      {fn: "ty_str_interned", recv: "tystr*"},
	"String.replace(String,String)":        {fn: "ty_str_replace_str", recv: "tystr*"},
	"String.valueOf(AC)":                   {fn: "ty_str_of_chars"},
	"String.valueOf(AC,I,I)":               {fn: "ty_str_of_chars_part"},
	"String.formatArgs(String,AObject)":    {fn: "ty_str_format", recv: "tystr*"},

	// ---- StringBuilder and StringBuffer: one helper set, two classes
	"StringBuilder.appendRaw(String)":      {fn: "ty_sb_append_str", recv: "void*"},
	"StringBuilder.append(F)":              {fn: "ty_sb_append_float", recv: "void*"},
	"StringBuilder.append(AC)":             {fn: "ty_sb_append_chars", recv: "void*"},
	"StringBuilder.insertRaw(I,String)":    {fn: "ty_sb_insert_str", recv: "void*"},
	"StringBuilder.insert(I,Object)":       {fn: "ty_sb_insert_obj", recv: "void*"},
	"StringBuilder.insert(I,I)":            {fn: "ty_sb_insert_int", recv: "void*"},
	"StringBuilder.insert(I,J)":            {fn: "ty_sb_insert_long", recv: "void*"},
	"StringBuilder.insert(I,F)":            {fn: "ty_sb_insert_float", recv: "void*"},
	"StringBuilder.insert(I,D)":            {fn: "ty_sb_insert_double", recv: "void*"},
	"StringBuilder.insert(I,Z)":            {fn: "ty_sb_insert_bool", recv: "void*"},
	"StringBuilder.insert(I,C)":            {fn: "ty_sb_insert_char", recv: "void*"},
	"StringBuilder.insert(I,AC)":           {fn: "ty_sb_insert_chars", recv: "void*"},
	"StringBuilder.delete(I,I)":            {fn: "ty_sb_delete", recv: "void*"},
	"StringBuilder.deleteCharAt(I)":        {fn: "ty_sb_delete_charat", recv: "void*"},
	"StringBuilder.replaceRaw(I,I,String)": {fn: "ty_sb_replace", recv: "void*"},
	"StringBuilder.reverse()":              {fn: "ty_sb_reverse", recv: "void*"},
	"StringBuilder.charAt(I)":              {fn: "ty_sb_charat", recv: "void*"},
	"StringBuilder.setCharAt(I,C)":         {fn: "ty_sb_set_charat", recv: "void*"},
	"StringBuilder.isEmpty()":              {fn: "ty_sb_isempty", recv: "void*"},
	"StringBuilder.capacity()":             {fn: "ty_sb_capacity", recv: "void*"},
	"StringBuilder.ensureCapacity(I)":      {fn: "ty_sb_ensure", recv: "void*"},
	"StringBuilder.setLength(I)":           {fn: "ty_sb_set_length", recv: "void*"},
	"StringBuilder.substring(I)":           {fn: "ty_sb_substring", recv: "void*"},
	"StringBuilder.substring(I,I)":         {fn: "ty_sb_substring_to", recv: "void*"},
	"StringBuilder.indexOf(String)":        {fn: "ty_sb_indexof", recv: "void*"},
	"StringBuilder.indexOf(String,I)":      {fn: "ty_sb_indexof_from", recv: "void*"},
	"StringBuilder.lastIndexOf(String)":    {fn: "ty_sb_lastindexof", recv: "void*"},

	"StringBuffer.appendRaw(String)": {fn: "ty_sb_append_str", recv: "void*"},
	"StringBuffer.append(F)":         {fn: "ty_sb_append_float", recv: "void*"},
	"StringBuffer.append(AC)":        {fn: "ty_sb_append_chars", recv: "void*"},
	// StringBuffer's own entry points: the type appeared in the table only
	// through the mixin-shaped methods above, so a plain append or length on
	// it reached the run-time's "no implementation" trap.
	"StringBuffer.append(Object)":         {fn: "ty_sb_append_obj", recv: "void*"},
	"StringBuffer.append(I)":              {fn: "ty_sb_append_int", recv: "void*"},
	"StringBuffer.append(J)":              {fn: "ty_sb_append_long", recv: "void*"},
	"StringBuffer.append(C)":              {fn: "ty_sb_append_char", recv: "void*"},
	"StringBuffer.append(D)":              {fn: "ty_sb_append_double", recv: "void*"},
	"StringBuffer.append(Z)":              {fn: "ty_sb_append_bool", recv: "void*"},
	"StringBuffer.toString()":             {fn: "ty_sb_tostring", recv: "void*"},
	"StringBuffer.length()":               {fn: "ty_sb_len", recv: "void*"},
	"StringBuffer.insertRaw(I,String)":    {fn: "ty_sb_insert_str", recv: "void*"},
	"StringBuffer.insert(I,Object)":       {fn: "ty_sb_insert_obj", recv: "void*"},
	"StringBuffer.insert(I,I)":            {fn: "ty_sb_insert_int", recv: "void*"},
	"StringBuffer.insert(I,J)":            {fn: "ty_sb_insert_long", recv: "void*"},
	"StringBuffer.insert(I,F)":            {fn: "ty_sb_insert_float", recv: "void*"},
	"StringBuffer.insert(I,D)":            {fn: "ty_sb_insert_double", recv: "void*"},
	"StringBuffer.insert(I,Z)":            {fn: "ty_sb_insert_bool", recv: "void*"},
	"StringBuffer.insert(I,C)":            {fn: "ty_sb_insert_char", recv: "void*"},
	"StringBuffer.insert(I,AC)":           {fn: "ty_sb_insert_chars", recv: "void*"},
	"StringBuffer.delete(I,I)":            {fn: "ty_sb_delete", recv: "void*"},
	"StringBuffer.deleteCharAt(I)":        {fn: "ty_sb_delete_charat", recv: "void*"},
	"StringBuffer.replaceRaw(I,I,String)": {fn: "ty_sb_replace", recv: "void*"},
	"StringBuffer.reverse()":              {fn: "ty_sb_reverse", recv: "void*"},
	"StringBuffer.charAt(I)":              {fn: "ty_sb_charat", recv: "void*"},
	"StringBuffer.setCharAt(I,C)":         {fn: "ty_sb_set_charat", recv: "void*"},
	"StringBuffer.isEmpty()":              {fn: "ty_sb_isempty", recv: "void*"},
	"StringBuffer.capacity()":             {fn: "ty_sb_capacity", recv: "void*"},
	"StringBuffer.ensureCapacity(I)":      {fn: "ty_sb_ensure", recv: "void*"},
	"StringBuffer.setLength(I)":           {fn: "ty_sb_set_length", recv: "void*"},
	"StringBuffer.substring(I)":           {fn: "ty_sb_substring", recv: "void*"},
	"StringBuffer.substring(I,I)":         {fn: "ty_sb_substring_to", recv: "void*"},
	"StringBuffer.indexOf(String)":        {fn: "ty_sb_indexof", recv: "void*"},
	"StringBuffer.indexOf(String,I)":      {fn: "ty_sb_indexof_from", recv: "void*"},
	"StringBuffer.lastIndexOf(String)":    {fn: "ty_sb_lastindexof", recv: "void*"},
}

// nativeCall renders a call to a prelude native method.
func (e *Emitter) nativeCall(m *ast.Method, recv string, args []ast.Expr) string {
	nf, ok := nativeTable[nativeKey(m)]
	if !ok {
		// A native method with no binding is this compiler's bug rather than
		// the program's: the prelude declared an operation the runtime has no
		// helper for. It used to emit 0 -- the program built, ran and answered
		// wrong -- so the call fails loudly where it is made instead.
		return "({ ty_unimplemented(" + e.cstr(m.Owner.Full+"."+m.Name) + "); 0; })"
	}
	vals := make([]string, 0, len(args))
	for i, a := range args {
		var want ast.Type
		if i < len(m.Params) {
			want = m.Params[i]
		}
		vals = append(vals, e.coerce(e.expr(a), a.GetType(), want))
	}
	return e.nativeInlineCall(nf, m, recv, vals)
}

// nativeInlineCall renders the call to a bound helper: the receiver, then the
// arguments, with the class the runtime needs appended where the table says so.
// A call site in a prelude method and a synthesized body both come through
// here, so clsArg, classHint and proto cannot be honoured by one of them and
// forgotten by the other -- forgotten clsArg is a missing argument and a
// mismatched prototype, which the C compiler reports as an error inside the
// runtime's header.
func (e *Emitter) nativeInlineCall(nf nativeFn, m *ast.Method, recv string, vals []string) string {
	var call string
	if t := e.psTwin(m, nf); t != nil {
		// The receiver comes first: which descriptor the text goes to is a
		// property of the PrintStream object, not of the method.
		body := t.name + "(" + strings.Join(append([]string{"(void*)" + recv}, vals...), ", ") + ")"
		call = "({ extern " + t.proto + "; " + body + "; })"
	} else if nf.clsArg != "" {
		if cl := e.prog.LookupClass(nf.clsArg); cl != nil {
			all := append([]string{}, vals...)
			all = append(all, "(tyclass*)&cls_"+mangle(cl.Full))
			call = nf.fn + "(" + strings.Join(all, ", ") + ")"
		} else {
			call = "0"
		}
	} else if nf.selClass != "" {
		if cl := e.prog.LookupClass(nf.selClass); cl != nil && e.selectorOf(cl, nf.selMethod) >= 0 {
			all := append([]string{}, vals...)
			all = append(all, fmt.Sprintf("(int32_t)%d", e.selectorOf(cl, nf.selMethod)))
			call = nf.fn + "(" + strings.Join(all, ", ") + ")"
		} else {
			// No such interface method in this program, so there is nothing to
			// dispatch to. A helper called without its selector would read
			// whatever the register held, which is how a thread would run
			// something that is not run().
			call = "({ ty_unimplemented(" + e.cstr(nf.selClass+"."+nf.selMethod) + "); 0; })"
		}
	} else if nf.classHint != "" && e.prog.LookupClass(nf.classHint) != nil {
		// getClass hands the runtime the tyclass of this program's Class, so
		// that the object it returns is an instance of it.
		cl := e.prog.LookupClass(nf.classHint)
		all := append([]string{"(void*)" + recv}, vals...)
		all = append(all, "(void*)&cls_"+mangle(cl.Full))
		call = "({ extern " + classOfProto + "; ty_class_of_cls(" + strings.Join(all, ", ") + "); })"
	} else {
		var parts []string
		if m.IsStatic() {
			// for static natives the cast describes the first argument
			if nf.recv != "" && len(vals) > 0 {
				vals[0] = "(" + nf.recv + ")" + vals[0]
			}
		} else if nf.recv != "" {
			parts = append(parts, "("+nf.recv+")"+recv)
		}
		parts = append(parts, vals...)
		call = nf.fn + "(" + strings.Join(parts, ", ") + ")"
		if nf.proto != "" {
			call = "({ extern " + nf.proto + "; " + call + "; })"
		}
	}
	if e.isRef(m.Result) {
		return "(" + e.ctype(m.Result) + ")" + call
	}
	return call
}

// nativeCallArgs is kept for the simple path used by exprStmt.
func nativeCall(m *ast.Method, args string) string {
	key := m.Owner.Name + "." + m.Name
	if fn, ok := nativeTable[key]; ok {
		return fn.fn + "(" + args + ")"
	}
	return m.Native + "(" + args + ")"
}

// nativeSignature is the C prototype of a method implemented outside the
// generated program: an instance method receives its receiver first, and every
// parameter keeps the C type of its Teyru type.
func (e *Emitter) nativeSignature(m *ast.Method) string {
	// object parameters and results are void*: the C side sees the runtime
	// representation, and the header stays independent of generated types
	ret := e.ctype(m.Result)
	if e.isRef(m.Result) {
		ret = "void *"
	}
	var params []string
	if !m.IsStatic() {
		params = append(params, "void *self")
	}
	for i, p := range m.Params {
		t := e.ctype(p)
		if e.isRef(p) {
			t = "void *"
		}
		params = append(params, t+" a"+fmt.Sprint(i))
	}
	return ret + " " + m.Native + "(" + strings.Join(params, ", ") + ")"
}

// NativeDecl describes one method a program has to implement in C. It is what
// `teyru build --native-header` writes out.
type NativeDecl struct {
	Signature string // the C prototype to define
	Method    string // the Teyru method, for the comment above it
}

// SelectorDecl names one interface method and the dispatch selector the
// compiler assigned to it, so that native code can call back into Teyru.
type SelectorDecl struct {
	Iface    string
	Method   string
	Selector int
	// Params are the parameter types, erased. They are part of the macro name
	// because an overloaded interface method is otherwise two `#define`s of the
	// same name with different values -- `List.listIterator()` and
	// `List.listIterator(int)` both spelled TY_SEL_LIST_LISTITERATOR, and the
	// second definition silently wins. A C caller has to be able to name the
	// one it means.
	Params string
}

// InterfaceSelectors lists every interface method of a program with its
// selector, in a stable order.
func InterfaceSelectors(p *sema.Program) []SelectorDecl {
	var out []SelectorDecl
	for _, cl := range p.Classes {
		if !cl.Builtin && !cl.IsInterface() {
			continue
		}
		if !cl.IsInterface() {
			continue
		}
		names := make([]string, 0, len(cl.Methods))
		for name := range cl.Methods {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, m := range cl.Methods[name] {
				if m.Selector < 0 || m.IsStatic() || m.IsCtor {
					continue
				}
				var desc strings.Builder
				for _, p := range m.Params {
					desc.WriteString(util.Descriptor(p))
				}
				out = append(out, SelectorDecl{Iface: cl.Name, Method: m.Name,
					Selector: m.Selector, Params: desc.String()})
			}
		}
	}
	return out
}

// NativeDecls lists the native methods a program has to implement in C.
func NativeDecls(p *sema.Program) []NativeDecl {
	e := &Emitter{prog: p}
	return e.nativeDecls()
}

// nativeDecls lists every native method of a program that is not part of the
// standard library, in a stable order.
func (e *Emitter) nativeDecls() []NativeDecl {
	var out []NativeDecl
	seen := map[string]bool{}
	for _, cl := range e.prog.Classes {
		if cl.Builtin {
			continue
		}
		names := make([]string, 0, len(cl.Methods))
		for name := range cl.Methods {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, m := range cl.Methods[name] {
				if !m.External || seen[m.Native] {
					continue
				}
				seen[m.Native] = true
				out = append(out, NativeDecl{
					Signature: e.nativeSignature(m),
					Method:    cl.Full + "." + m.Name,
				})
			}
		}
		// Constructors are not in cl.Methods (they are named <init> and live in
		// cl.Ctors), but a native constructor links against tyn_Class__init__...
		// like any other native method: leaving them out made the linker ask for
		// a symbol the header never declared.
		for _, m := range cl.Ctors {
			if !m.External || seen[m.Native] {
				continue
			}
			seen[m.Native] = true
			out = append(out, NativeDecl{
				Signature: e.nativeSignature(m),
				Method:    cl.Full + "." + cl.Name,
			})
		}
	}
	return out
}
