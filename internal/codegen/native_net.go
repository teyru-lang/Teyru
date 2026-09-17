package codegen

// The native methods of the networking and file layer: lib/15_net.teyru and
// lib/16_file.teyru declare them, and this table points each one at the C
// helper in internal/runtime/src/tyrt_net.c.
//
// Two of the fields in nativeFn are what make the layer self-contained. `proto`
// is the C prototype of the helper, repeated as an `extern` declaration at
// every call site, so nothing has to be added to tyrt.h -- a header this
// feature does not own and must not edit. `fn` names a helper whose calling
// convention is the one the whole file follows: a negative result is -errno,
// a non-negative one is a count, a descriptor or a port. The Teyru half turns
// those integers into exceptions, which is why `recv` is empty everywhere here:
// every method this table describes is `public static native` in a final class
// (see the header of lib/15_net.teyru for why that is a requirement rather than
// a style choice), so the emitter casts arguments and never a receiver.
//
// The prototypes below are the definitions in tyrt_net.c, character for
// character; a mismatch is a compile error in the generated program rather than
// a surprise at run time, because the compiler checks the extern at each call
// site against the definition it is linking against.

// nativeNetTable is registered by being initialized rather than by an init
// function, and that is deliberate: Go runs every package-level variable
// initializer before any init function, and native.go's init -- the one that
// copies extraNative into nativeTable -- would otherwise run first and copy an
// empty slice. The order is not a matter of luck either way: files reach the
// compiler in lexical order, so "native.go" sorts before "native_net.go", and
// a plain `func init()` here would be too late. The transitive reference
// through registerNativeNet is what makes Go initialize extraNative first.
var nativeNetTable = registerNativeNet(map[string]nativeFn{
	// ---- sockets ----
	//
	// listen0 takes the port, the backlog and the reuse flag before the bind,
	// because SO_REUSEADDR set after a bind does nothing: without it a server
	// that restarts while a connection is in TIME_WAIT cannot have its own port
	// back for up to a minute.
	"Net.listen0(I,I,Z)": {
		fn:    "ty_net_listen",
		proto: "int32_t ty_net_listen(int32_t, int32_t, int32_t)",
	},
	"Net.connect0(String,I,I)": {
		fn:    "ty_net_connect",
		proto: "int32_t ty_net_connect(tystr *, int32_t, int32_t)",
	},
	"Net.accept0(I,I)": {
		fn:    "ty_net_accept",
		proto: "int32_t ty_net_accept(int32_t, int32_t)",
	},
	// The read returns however many bytes the kernel had, 0 at end of stream,
	// or a negative code -- never a short count pretending to be the whole
	// request, and never 0 for a timeout.
	"Net.read0(I,AB,I,I)": {
		fn:    "ty_net_read",
		proto: "int32_t ty_net_read(int32_t, tyarr *, int32_t, int32_t)",
	},
	// The write loops until the last byte is out. A short write reported as
	// success is the classic truncation bug this entry exists to prevent.
	"Net.write0(I,AB,I,I)": {
		fn:    "ty_net_write_all",
		proto: "int32_t ty_net_write_all(int32_t, tyarr *, int32_t, int32_t)",
	},
	"Net.writeStr0(I,String)": {
		fn:    "ty_net_write_str",
		proto: "int32_t ty_net_write_str(int32_t, tystr *)",
	},
	"Net.shutdownWrite0(I)": {
		fn:    "ty_net_shutdown_write",
		proto: "int32_t ty_net_shutdown_write(int32_t)",
	},
	"Net.close0(I)": {
		fn:    "ty_net_close",
		proto: "int32_t ty_net_close(int32_t)",
	},
	"Net.setTimeout0(I,I)": {
		fn:    "ty_net_set_timeout",
		proto: "int32_t ty_net_set_timeout(int32_t, int32_t)",
	},
	"Net.setReuse0(I,Z)": {
		fn:    "ty_net_set_reuse",
		proto: "int32_t ty_net_set_reuse(int32_t, int32_t)",
	},
	"Net.localPort0(I)": {
		fn:    "ty_net_local_port",
		proto: "int32_t ty_net_local_port(int32_t)",
	},
	"Net.localAddr0(I)": {
		fn:    "ty_net_local_addr",
		proto: "tystr *ty_net_local_addr(int32_t)",
	},
	"Net.peerAddr0(I)": {
		fn:    "ty_net_peer_addr",
		proto: "tystr *ty_net_peer_addr(int32_t)",
	},
	// The message for a failure code. Like the addresses above, the string is
	// allocated and handed straight back, so no root is needed: the collector
	// never runs between the allocation and the return.
	"Net.strerror0(I)": {
		fn:    "ty_net_strerror",
		proto: "tystr *ty_net_strerror(int32_t)",
	},
	// Whether a code is one of the two that are not errno at all. Teyru asks
	// instead of comparing numbers, so the numbering stays in tyrt_net.c.
	"Net.isTimeout0(I)": {
		fn:    "ty_net_is_timeout",
		proto: "int32_t ty_net_is_timeout(int32_t)",
	},
	"Net.isUnknownHost0(I)": {
		fn:    "ty_net_is_unknown_host",
		proto: "int32_t ty_net_is_unknown_host(int32_t)",
	},
	// The byte[] a protocol is read as, on its way back to being a String: the
	// bytes are decoded as UTF-8 and anything ill-formed in them becomes U+FFFD,
	// which is what a read that stopped in the middle of a sequence needs. The
	// array never outlives the call, so it needs no root either.
	"Net.stringFrom(AB,I,I)": {
		fn:    "ty_net_bytes_to_str",
		proto: "tystr *ty_net_bytes_to_str(tyarr *, int32_t, int32_t)",
	},

	// ---- TLS ----
	//
	// One entry per native in the TLS half of lib/15_net.teyru. They are the
	// reason internal/runtime/src/tyrt_tls.c exists as a file of its own: a
	// build compiles that file, and links OpenSSL, only when the program can
	// reach one of the helpers below -- so the socket layer's own programs keep
	// their plaintext-only dependencies, and a program that cannot reach TLS
	// (a hello world) is not linked against libssl at all.
	//
	// The one that returns an object -- tlsDetail0, the text of the last
	// failure -- allocates it and hands it straight back, so it needs no root,
	// exactly as Net.strerror0 above does not.
	"Net.tlsClientContext0(String)": {
		fn:    "ty_tls_client_context",
		proto: "int32_t ty_tls_client_context(tystr *)",
	},
	"Net.tlsServerContext0(String,String)": {
		fn:    "ty_tls_server_context",
		proto: "int32_t ty_tls_server_context(tystr *, tystr *)",
	},
	"Net.tlsFreeContext0(I)": {
		fn:    "ty_tls_context_free",
		proto: "int32_t ty_tls_context_free(int32_t)",
	},
	// The client handshake: the context, the connected descriptor, and the host
	// the certificate has to be for.
	"Net.tlsConnect0(I,I,String)": {
		fn:    "ty_tls_connect",
		proto: "int32_t ty_tls_connect(int32_t, int32_t, tystr *)",
	},
	// The server handshake: the context holds the certificate and the key, and
	// there is no name to check.
	"Net.tlsAccept0(I,I)": {
		fn:    "ty_tls_accept",
		proto: "int32_t ty_tls_accept(int32_t, int32_t)",
	},
	// The same contract as Net.read0 and Net.write0, over a session: a read
	// returns what the record had, 0 at the end of the stream and a negative
	// code on failure, and a write loops until the last byte is out.
	"Net.tlsRead0(I,AB,I,I)": {
		fn:    "ty_tls_read",
		proto: "int32_t ty_tls_read(int32_t, tyarr *, int32_t, int32_t)",
	},
	"Net.tlsWrite0(I,AB,I,I)": {
		fn:    "ty_tls_write_all",
		proto: "int32_t ty_tls_write_all(int32_t, tyarr *, int32_t, int32_t)",
	},
	"Net.tlsWriteStr0(I,String)": {
		fn:    "ty_tls_write_str",
		proto: "int32_t ty_tls_write_str(int32_t, tystr *)",
	},
	"Net.tlsClose0(I)": {
		fn:    "ty_tls_close",
		proto: "int32_t ty_tls_close(int32_t)",
	},
	// What the last failure on this thread was about, and which family it
	// belongs to. The Teyru half turns the pair into the exception that names
	// it, so no code of this layer's ever reaches a caller.
	"Net.tlsDetail0()": {
		fn:    "ty_tls_detail_text",
		proto: "tystr *ty_tls_detail_text(void)",
	},
	"Net.tlsKind0(I)": {
		fn:    "ty_tls_kind",
		proto: "int32_t ty_tls_kind(int32_t)",
	},

	// ---- files ----
	"Fs.kind0(String)": {
		fn:    "ty_file_kind",
		proto: "int32_t ty_file_kind(tystr *)",
	},
	"Fs.size0(String)": {
		fn:    "ty_file_size",
		proto: "int64_t ty_file_size(tystr *)",
	},
	"Fs.delete0(String)": {
		fn:    "ty_file_delete",
		proto: "int32_t ty_file_delete(tystr *)",
	},
	"Fs.mkdirs0(String)": {
		fn:    "ty_file_mkdirs",
		proto: "int32_t ty_file_mkdirs(tystr *)",
	},
	// The two entries that return an object, and so cannot report a failure in
	// their return value. The reason goes into element 0 of an int[] the Teyru
	// side made; the C refuses an array that is not an int[] rather than
	// writing through the wrong one. `A` is the int[]: the descriptor for any
	// array is the same character, and the helper checks the element size.
	"Fs.list0(String,AI)": {
		fn:    "ty_file_list",
		proto: "tyarr *ty_file_list(tystr *, tyarr *)",
	},
	"Fs.readBytes0(String,AB)": {
		fn:    "ty_file_read_bytes",
		proto: "tyarr *ty_file_read_bytes(tystr *, tyarr *)",
	},
	// The append flag is an int32_t on the C side and a boolean here, which is
	// the same width by construction: Teyru's boolean is int32_t everywhere.
	"Fs.writeBytes0(String,AB,I,I,Z)": {
		fn:    "ty_file_write_bytes",
		proto: "int32_t ty_file_write_bytes(tystr *, tyarr *, int32_t, int32_t, int32_t)",
	},
	"Fs.writeStr0(String,String,Z)": {
		fn:    "ty_file_write_str",
		proto: "int32_t ty_file_write_str(tystr *, tystr *, int32_t)",
	},
	"Fs.tempDir0(String)": {
		fn:    "ty_file_temp_dir",
		proto: "tystr *ty_file_temp_dir(tystr *)",
	},
})

// registerNativeNet adds a table to the ones native.go merges into nativeTable,
// and returns it so that the call can stand in a variable initializer. The
// append is the whole job; the reason it is a function is the ordering
// described above nativeNetTable.
func registerNativeNet(t map[string]nativeFn) map[string]nativeFn {
	extraNative = append(extraNative, t)
	return t
}
