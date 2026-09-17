/* tyrt_tls.c - TLS over a socket, built on OpenSSL.
 *
 * The socket layer's conventions are kept here exactly, because the Teyru half
 * of both is one file: a negative result is a failure code, a non-negative one
 * is a count or a handle, a read that returns 0 has met the end of the stream
 * and a read that returns TY_TLS_TIMEOUT has run out of time rather than ended.
 * The failure codes that are TLS's own start below the socket layer's, so the
 * two number spaces cannot collide, and strerror learns them the same way it
 * learned TY_NET_UNKNOWN_HOST.
 *
 * A session is an OpenSSL SSL object behind a small integer, and a context is
 * an SSL_CTX behind one. Handles rather than pointers because a descriptor, a
 * count and a handle are all ints in this runtime -- the Teyru side holds them
 * in fields and the generated code passes them as arguments -- and because a
 * pointer that a program has already closed then fails with a code instead of
 * being dereferenced. The tables are fixed size and a slot is reused when a
 * session is closed, so a server that answers a connection at a time does not
 * run the table out; a program that opens more at once than there are slots is
 * told so with -EMFILE.
 *
 * What the tables do not do is referee. A context may be used by any number of
 * threads at once -- OpenSSL allows that, and a server's context is meant to be
 * shared -- but a session belongs to one thread at a time, in the same way the
 * descriptor under it does: two threads reading one connection through one SSL
 * object would interleave their records. The layer of this file that Teyru sees
 * holds a session in a Socket, and a Socket is one conversation, which is the
 * rule the socket layer already states.
 *
 * Verification is not optional and there is no way to switch it off: a client
 * context is built with SSL_VERIFY_PEER, the system's trust anchors are always
 * loaded, and the host name (or the address literal) is checked against the
 * certificate the peer sent. A TLS client that checks nothing is worse than no
 * TLS at all -- it hands the program a stream it believes is a particular
 * server while anyone on the path can read and rewrite it -- so the only knob
 * this layer has is *which* anchors to trust in addition to the system's, which
 * is what a private CA needs and what a "do not verify" flag would undermine.
 * The server side asks no certificate of its clients (SSL_VERIFY_NONE): mutual
 * TLS is a policy this layer does not implement rather than one it pretends to.
 *
 * The transport is a BIO of this file's own rather than SSL_set_fd, for the
 * socket layer's oldest reason: a write to a socket whose peer has gone raises
 * SIGPIPE, whose default action kills the process, and the only way to have
 * every TLS write carry MSG_NOSIGNAL is to own the writes. It also means the
 * layer reports the socket's own codes: a read timeout set with setSoTimeout
 * arrives here as -EAGAIN and leaves as TY_TLS_TIMEOUT, so a TLS connection
 * times out exactly where a plain one does and a server that answers one
 * connection at a time cannot be held by a silent client over TLS either.
 *
 * This file links OpenSSL, and that is why it is a file of its own: the driver
 * compiles it only for a program that can reach one of its helpers, so a
 * program that does not use TLS is not linked against libssl (see
 * internal/driver/driver.go). It is also why every entry point here is
 * declared by internal/codegen/native_net.go at its call site rather than in
 * tyrt.h: the shared header is not this feature's to change, and a call-site
 * prototype is the mechanism the socket layer already uses for the same reason.
 */

/* Including <winsock2.h> is not free, so the platform layer's socket half is
   behind this macro. This file is POSIX-only in practice -- the driver refuses
   a target without OpenSSL before it compiles anything -- but the macro is what
   makes the declarations of typlat_socket_recv and typlat_socket_send arrive
   at all, and they are what the BIO below is written on. */
#define TYPLAT_WANT_SOCKETS 1

#include "tyrt.h"

#include <errno.h>
#include <limits.h>
#include <openssl/err.h>
#include <openssl/evp.h>
#include <openssl/pem.h>
#include <openssl/ssl.h>
#include <openssl/x509v3.h>
#include <stdarg.h>
#include <stdio.h>
#include <string.h>

/* OpenSSL 1.1 is the floor, and the floor is stated here rather than discovered
   one undeclared identifier at a time. Three of the calls below arrived in 1.1
   and have no predecessor worth using: BIO_meth_new (1.0.2 has none -- a custom
   BIO was written by filling a BIO_METHOD struct by hand), TLS_client_method
   (1.0.2 has SSLv23_client_method), and
   SSL_CTX_set_min_proto_version (1.0.2 can only *add* options to a default that
   already allowed SSLv3 and TLS 1.0). A machine with 1.0.2 headers therefore
   cannot build this file at all, and what a person should see when that happens
   is one sentence saying so, not a page of "undeclared identifier" inside a
   runtime file they did not write. OPENSSL_VERSION_NUMBER is OpenSSL's own
   macro, so this costs nothing and can never disagree with the headers it is
   read from. */
#if OPENSSL_VERSION_NUMBER < 0x10100000L
#error "the TLS layer needs OpenSSL 1.1 or newer (SSL_set1_host, BIO_meth_new, TLS_client_method and SSL_CTX_set_min_proto_version are not in 1.0.x)"
#endif

/* ---- the codes -------------------------------------------------------- */

/* The certificate the peer sent is not trusted: it does not chain to an anchor,
   it has expired, it does not cover this use, or the signature does not
   verify. Everything a certificate can be wrong about except the name, which
   has a code of its own because the two are different things to a user: one is
   "this server is not who it claims to be", the other is "this certificate is
   not trusted to say". */
#define TY_TLS_CERT (-10002)

/* The chain verified, and the certificate is not for the host that was asked
   for. */
#define TY_TLS_HOST (-10003)

/* The handshake failed for a reason that is neither of the two above: a
   protocol mismatch, a peer that went away in the middle of it, a connection
   that was refused or reset under it, or a record that did not decrypt. */
#define TY_TLS_HANDSHAKE (-10004)

/* The certificate or the key this layer was handed cannot be used: it is not
   PEM, or the key does not belong to the certificate. A server that cannot
   present a certificate cannot serve, and this is said when the server is
   given one rather than on the first connection. */
#define TY_TLS_CONFIG (-10005)

/* -EAGAIN, which is what the socket layer answers when a read ran out of time
   rather than failed, and what the socket layer's own file calls
   TY_NET_TIMEOUT. The name is repeated here because this file cannot include
   tyrt_net.h -- the driver copies the .c files of the runtime into a build and
   no header of the socket layer goes with them -- and the value has to be the
   same one: a timeout reported as an ordinary errno is read as the end of the
   stream by every loop that checks for a 0, and a TLS connection that ends
   early instead of timing out is a request answered from half a response.
   tyrt_net.c's TY_NET_TIMEOUT is -EAGAIN; so is this. */
#define TY_TLS_TIMEOUT (-EAGAIN)

/* How many contexts and sessions may exist at once. Contexts are few -- one per
   certificate a program serves and one per trust configuration a client uses
   -- and sessions are one per TLS connection, which for this prelude's server
   is one at a time. The numbers are limits rather than growth: a program that
   wants a thousand sessions at once needs a table this layer does not own. */
#define TY_TLS_CTX_MAX 32
#define TY_TLS_SESSION_MAX 256

/* The longest reason kept for the last failure on a thread. OpenSSL's own text
   is longer than this and is truncated rather than reformatted. */
#define TY_TLS_DETAIL_MAX 256

/* ---- what a failure is ------------------------------------------------ */

/* The reason the last call on *this thread* failed. A thread-local rather than
   a field of anything: the failure is reported through a return code and read
   immediately afterwards by the Teyru half, on the same thread, and two threads
   doing TLS at once must not overwrite each other's reason. */
static __thread char ty_tls_detail[TY_TLS_DETAIL_MAX];

/* The negative code the BIO below was given by the socket layer, or 0 when the
   last transport call reported nothing. It is how a failure that came from the
   socket -- most importantly a read timeout -- is told apart from one that came
   from the protocol, since OpenSSL reports both as "the call failed". */
static __thread int32_t ty_tls_bio_err;

static void ty_tls_reason(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  vsnprintf(ty_tls_detail, sizeof ty_tls_detail, fmt, ap);
  va_end(ap);
}

/* The front of OpenSSL's error queue, which is where the text of a protocol or
   certificate failure is. The queue is one per thread and is drained here: a
   reason left behind would otherwise be reported as the reason for the *next*
   failure. */
static void ty_tls_reason_openssl(void) {
  unsigned long e = ERR_get_error();
  ERR_clear_error();
  if (e == 0) {
    ty_tls_reason("the connection failed");
    return;
  }
  ty_tls_reason("%s", ERR_reason_error_string(e) ? ERR_reason_error_string(e) : "the connection failed");
}

/* Why an SSL call that returned ret <= 0 did. The verify result is asked first,
   and that order is the point: when a chain does not verify OpenSSL fails the
   handshake with "certificate verify failed", which says nothing about *what*
   was wrong with the certificate, while SSL_get_verify_result still holds the
   specific reason -- the name not matching, the chain being incomplete, the
   certificate being expired -- and that is the sentence a person needs. */
static int32_t ty_tls_failed(SSL *ssl, int ret) {
  long v = SSL_get_verify_result(ssl);
  if (v != X509_V_OK) {
    const char *text = X509_verify_cert_error_string(v);
    ty_tls_reason("%s", text ? text : "the certificate could not be verified");
    if (v == X509_V_ERR_HOSTNAME_MISMATCH || v == X509_V_ERR_IP_ADDRESS_MISMATCH) {
      return TY_TLS_HOST;
    }
    return TY_TLS_CERT;
  }
  /* The socket gave up before the protocol did: a read timeout is the one that
     matters, because it is the difference between a peer that is slow and a
     peer that is gone. */
  if (ty_tls_bio_err == TY_TLS_TIMEOUT) {
    ty_tls_reason("timed out");
    return TY_TLS_TIMEOUT;
  }
  int e = SSL_get_error(ssl, ret);
  switch (e) {
    case SSL_ERROR_ZERO_RETURN:
      ty_tls_reason("the peer closed the connection during the handshake");
      return TY_TLS_HANDSHAKE;
    case SSL_ERROR_SYSCALL:
      if (ty_tls_bio_err != 0) {
        ty_tls_reason("the connection failed: errno %d", -ty_tls_bio_err);
        return TY_TLS_HANDSHAKE;
      }
      ty_tls_reason("the connection was closed by the peer");
      return TY_TLS_HANDSHAKE;
    default:
      ty_tls_reason_openssl();
      return TY_TLS_HANDSHAKE;
  }
}

/* ---- the handle tables ------------------------------------------------ */

static SSL_CTX *ty_tls_ctxs[TY_TLS_CTX_MAX];
static SSL *ty_tls_sessions[TY_TLS_SESSION_MAX];
static typlat_mutex ty_tls_lock;
/* The layer's own initialisation -- the lock, OpenSSL itself, and the BIO
   method -- runs exactly once, however many threads reach it at the same
   time. */
static pthread_once_t ty_tls_once = PTHREAD_ONCE_INIT;
static BIO_METHOD *ty_tls_bio_method;

static void ty_tls_bio_setup(void);

static void ty_tls_setup(void) {
  typlat_mutex_init(&ty_tls_lock);
  /* OpenSSL 3 sets itself up on first use, and this call is what states that
     the library is wanted: a build against 1.1 needs it, and making it here
     means no other call has to know which version it was built against. */
  OPENSSL_init_ssl(0, NULL);
  ty_tls_bio_setup();
}

static void ty_tls_ready(void) { pthread_once(&ty_tls_once, ty_tls_setup); }

/* A handle for a context, or a negative code when there is no free slot. */
static int32_t ty_tls_ctx_add(SSL_CTX *c) {
  int32_t out = -EMFILE;
  typlat_mutex_lock(&ty_tls_lock);
  for (int32_t i = 0; i < TY_TLS_CTX_MAX; i++) {
    if (!ty_tls_ctxs[i]) {
      ty_tls_ctxs[i] = c;
      out = i + 1;
      break;
    }
  }
  typlat_mutex_unlock(&ty_tls_lock);
  return out;
}

static SSL_CTX *ty_tls_ctx_of(int32_t handle) {
  if (handle <= 0 || handle > TY_TLS_CTX_MAX) return NULL;
  return ty_tls_ctxs[handle - 1];
}

static int32_t ty_tls_ctx_release(int32_t handle) {
  if (handle <= 0 || handle > TY_TLS_CTX_MAX) return -EBADF;
  typlat_mutex_lock(&ty_tls_lock);
  SSL_CTX *c = ty_tls_ctxs[handle - 1];
  ty_tls_ctxs[handle - 1] = NULL;
  typlat_mutex_unlock(&ty_tls_lock);
  if (!c) return -EBADF;
  SSL_CTX_free(c);
  return 0;
}

static int32_t ty_tls_session_add(SSL *s) {
  int32_t out = -EMFILE;
  typlat_mutex_lock(&ty_tls_lock);
  for (int32_t i = 0; i < TY_TLS_SESSION_MAX; i++) {
    if (!ty_tls_sessions[i]) {
      ty_tls_sessions[i] = s;
      out = i + 1;
      break;
    }
  }
  typlat_mutex_unlock(&ty_tls_lock);
  return out;
}

static SSL *ty_tls_session_of(int32_t handle) {
  if (handle <= 0 || handle > TY_TLS_SESSION_MAX) return NULL;
  return ty_tls_sessions[handle - 1];
}

/* ---- small helpers ---------------------------------------------------- */

/* The bytes of a Teyru string as a C string, or NULL when it cannot be one.
   The same test the socket layer applies to a host name: a string whose bytes
   contain a NUL cannot name a host or hold a PEM document, and truncating it
   silently would use the wrong one. */
static const char *ty_tls_cstr(tystr *s) {
  if (!s) return NULL;
  if ((int64_t)strlen(TY_STR_DATA(s)) != s->blen) return NULL;
  return TY_STR_DATA(s);
}

/* A byte[] is the only array this file reads or writes, and the element size is
   the test: a short[] passed here by mistake would otherwise be filled with
   bytes and read back as numbers. */
static int ty_tls_is_bytes(tyarr *a) { return a && a->esize == 1; }

/* ---- the transport ---------------------------------------------------- */

/* OpenSSL reads and writes through this BIO, so every byte a TLS session sends
   carries MSG_NOSIGNAL the way every byte the socket layer sends does. The
   descriptor is the BIO's data and is *not* closed when the BIO is freed: it
   belongs to the Socket, and this layer only borrows it for as long as the
   session lives. */
static int ty_tls_bio_write(BIO *b, const char *buf, int len) {
  if (len <= 0) return 0;
  BIO_clear_retry_flags(b);
  int32_t fd = (int32_t)(intptr_t)BIO_get_data(b);
  int64_t n = typlat_socket_send(fd, buf, len);
  if (n < 0) {
    ty_tls_bio_err = (int32_t)n;
    /* A signal and a deadline are both "try again" at this level: OpenSSL is
       told to retry, and the code the socket layer gave is kept so that a
       timeout can be reported as one once it reaches the SSL call. */
    if (n == -EINTR || n == -EAGAIN) BIO_set_retry_write(b);
    return -1;
  }
  return (int)n;
}

static int ty_tls_bio_read(BIO *b, char *buf, int len) {
  if (len <= 0) return 0;
  BIO_clear_retry_flags(b);
  int32_t fd = (int32_t)(intptr_t)BIO_get_data(b);
  int64_t n = typlat_socket_recv(fd, buf, len);
  if (n < 0) {
    ty_tls_bio_err = (int32_t)n;
    if (n == -EINTR || n == -EAGAIN) BIO_set_retry_read(b);
    return -1;
  }
  /* 0 is the end of the stream, which is what a BIO reports as end of file. */
  return (int)n;
}

static long ty_tls_bio_ctrl(BIO *b, int cmd, long num, void *ptr) {
  (void)b;
  (void)num;
  (void)ptr;
  /* Flushing a socket is nothing, and OpenSSL asks: a control that answered 0
     would be read as a failure to flush. */
  return cmd == BIO_CTRL_FLUSH ? 1 : 0;
}

static int ty_tls_bio_create(BIO *b) {
  BIO_set_init(b, 1);
  BIO_set_data(b, NULL);
  return 1;
}

static int ty_tls_bio_destroy(BIO *b) {
  if (b) BIO_set_data(b, NULL);
  return 1;
}

static void ty_tls_bio_setup(void) {
  /* The type is the one OpenSSL hands out for a module of its own rather than
     one of the built-in numbers: a BIO that claimed to be, say, BIO_TYPE_SOCKET
     would be asked to behave like one by code that knows the type. */
  ty_tls_bio_method = BIO_meth_new(BIO_get_new_index() | BIO_TYPE_SOURCE_SINK, "teyru socket");
  if (!ty_tls_bio_method) return;
  BIO_meth_set_write(ty_tls_bio_method, ty_tls_bio_write);
  BIO_meth_set_read(ty_tls_bio_method, ty_tls_bio_read);
  BIO_meth_set_ctrl(ty_tls_bio_method, ty_tls_bio_ctrl);
  BIO_meth_set_create(ty_tls_bio_method, ty_tls_bio_create);
  BIO_meth_set_destroy(ty_tls_bio_method, ty_tls_bio_destroy);
}

/* A BIO over a descriptor, or NULL when the method could not be made. */
static BIO *ty_tls_bio_new(int32_t fd) {
  if (!ty_tls_bio_method) return NULL;
  BIO *b = BIO_new(ty_tls_bio_method);
  if (!b) return NULL;
  BIO_set_data(b, (void *)(intptr_t)fd);
  return b;
}

/* ---- contexts --------------------------------------------------------- */

/* The anchors a TLS client verifies against: the ones the system's own store
   names, plus every certificate in pem when it is not empty. SSL_CERT_FILE and
   SSL_CERT_DIR are honoured by OpenSSL here, which is how a program that is
   handed a CA by its environment rather than by its arguments works. */
static int ty_tls_load_cas(SSL_CTX *ctx, const char *pem, int64_t len) {
  if (SSL_CTX_set_default_verify_paths(ctx) != 1) {
    ty_tls_reason("no system trust store could be loaded");
    return -1;
  }
  if (len <= 0) return 0;
  BIO *bio = BIO_new_mem_buf(pem, (int)len);
  if (!bio) {
    ty_tls_reason("out of memory reading the CA file");
    return -1;
  }
  X509_STORE *store = SSL_CTX_get_cert_store(ctx);
  X509 *x;
  int n = 0;
  while ((x = PEM_read_bio_X509_AUX(bio, NULL, NULL, NULL)) != NULL) {
    if (X509_STORE_add_cert(store, x) != 1) {
      /* The same CA twice is not a failure: a bundle that lists a root two
         times would otherwise make the whole file unusable. */
      unsigned long e = ERR_peek_last_error();
      if (ERR_GET_REASON(e) != X509_R_CERT_ALREADY_IN_HASH_TABLE) {
        ERR_clear_error();
        X509_free(x);
        BIO_free(bio);
        ty_tls_reason("the CA file does not hold a usable certificate");
        return -1;
      }
      ERR_clear_error();
    }
    X509_free(x);
    n++;
  }
  BIO_free(bio);
  ERR_clear_error();
  if (n == 0) {
    ty_tls_reason("the CA file holds no PEM certificate");
    return -1;
  }
  return 0;
}

/* A client context. Verification is on; ca_pem adds anchors to the system's. */
int32_t ty_tls_client_context(tystr *ca_pem) {
  ty_tls_detail[0] = 0;
  ty_tls_bio_err = 0;
  const char *pem = NULL;
  int64_t len = 0;
  if (ca_pem && ca_pem->blen > 0) {
    pem = ty_tls_cstr(ca_pem);
    if (!pem) return -EINVAL;
    len = ca_pem->blen;
  }
  ty_tls_ready();
  ERR_clear_error();
  SSL_CTX *ctx = SSL_CTX_new(TLS_client_method());
  if (!ctx) {
    ty_tls_reason_openssl();
    return TY_TLS_HANDSHAKE;
  }
  /* TLS 1.2 is the floor. A client that would still speak TLS 1.0 or 1.1 is a
     client that can be talked down to something broken, and nothing that speaks
     HTTPS today needs it. */
  if (SSL_CTX_set_min_proto_version(ctx, TLS1_2_VERSION) != 1 ||
      ty_tls_load_cas(ctx, pem, len) != 0) {
    SSL_CTX_free(ctx);
    if (!ty_tls_detail[0]) ty_tls_reason_openssl();
    return TY_TLS_CONFIG;
  }
  /* No callback: a chain that does not verify fails the handshake, and
     ty_tls_failed then says what was wrong with it. A callback could only
     accept one, which is the thing this layer refuses to offer. */
  SSL_CTX_set_verify(ctx, SSL_VERIFY_PEER, NULL);
  SSL_CTX_set_mode(ctx, SSL_MODE_AUTO_RETRY);
  int32_t h = ty_tls_ctx_add(ctx);
  if (h < 0) {
    SSL_CTX_free(ctx);
    ty_tls_reason("too many TLS contexts");
    return h;
  }
  return h;
}

/* The certificate chain and the private key of a server, both as PEM text. The
   key and the certificate are checked against each other here rather than at
   the first connection, because a server that cannot present its certificate is
   a server that should not have started. */
int32_t ty_tls_server_context(tystr *cert_pem, tystr *key_pem) {
  ty_tls_detail[0] = 0;
  ty_tls_bio_err = 0;
  const char *cert = cert_pem ? ty_tls_cstr(cert_pem) : NULL;
  const char *key = key_pem ? ty_tls_cstr(key_pem) : NULL;
  if (!cert || !*cert || !key || !*key) return -EINVAL;
  ty_tls_ready();
  ERR_clear_error();
  SSL_CTX *ctx = SSL_CTX_new(TLS_server_method());
  if (!ctx) {
    ty_tls_reason_openssl();
    return TY_TLS_HANDSHAKE;
  }
  /* A server asks nothing of its clients: mutual TLS is a policy this layer
     does not implement, and pretending to accept a client certificate while
     ignoring it would be worse than saying so. */
  SSL_CTX_set_verify(ctx, SSL_VERIFY_NONE, NULL);
  if (SSL_CTX_set_min_proto_version(ctx, TLS1_2_VERSION) != 1) {
    SSL_CTX_free(ctx);
    ty_tls_reason_openssl();
    return TY_TLS_CONFIG;
  }
  BIO *cbio = BIO_new_mem_buf(cert, (int)cert_pem->blen);
  if (!cbio) {
    SSL_CTX_free(ctx);
    ty_tls_reason("out of memory reading the certificate");
    return TY_TLS_CONFIG;
  }
  X509 *leaf = PEM_read_bio_X509_AUX(cbio, NULL, NULL, NULL);
  if (!leaf) {
    BIO_free(cbio);
    SSL_CTX_free(ctx);
    ERR_clear_error();
    ty_tls_reason("the certificate is not a PEM certificate");
    return TY_TLS_CONFIG;
  }
  if (SSL_CTX_use_certificate(ctx, leaf) != 1) {
    X509_free(leaf);
    BIO_free(cbio);
    SSL_CTX_free(ctx);
    ty_tls_reason_openssl();
    return TY_TLS_CONFIG;
  }
  X509_free(leaf);
  /* Everything after the leaf in the same file is the rest of the chain, which
     is what a certificate that is not self-signed has to send: a client that
     has the root but not the intermediate cannot build the path without it.
     add_extra_chain_cert takes the reference on success and the caller keeps it
     on failure. */
  X509 *extra;
  while ((extra = PEM_read_bio_X509(cbio, NULL, NULL, NULL)) != NULL) {
    if (SSL_CTX_add_extra_chain_cert(ctx, extra) != 1) {
      X509_free(extra);
      BIO_free(cbio);
      SSL_CTX_free(ctx);
      ty_tls_reason_openssl();
      return TY_TLS_CONFIG;
    }
  }
  /* End of file, or a line that is not a certificate: either way the chain is
     what it is, and the error queue holds a decode complaint that is not one. */
  ERR_clear_error();
  BIO_free(cbio);
  BIO *kbio = BIO_new_mem_buf(key, (int)key_pem->blen);
  if (!kbio) {
    SSL_CTX_free(ctx);
    ty_tls_reason("out of memory reading the private key");
    return TY_TLS_CONFIG;
  }
  EVP_PKEY *pkey = PEM_read_bio_PrivateKey(kbio, NULL, NULL, NULL);
  BIO_free(kbio);
  if (!pkey) {
    SSL_CTX_free(ctx);
    ERR_clear_error();
    ty_tls_reason("the private key is not a PEM private key");
    return TY_TLS_CONFIG;
  }
  if (SSL_CTX_use_PrivateKey(ctx, pkey) != 1) {
    EVP_PKEY_free(pkey);
    SSL_CTX_free(ctx);
    ty_tls_reason_openssl();
    return TY_TLS_CONFIG;
  }
  EVP_PKEY_free(pkey);
  if (SSL_CTX_check_private_key(ctx) != 1) {
    SSL_CTX_free(ctx);
    ERR_clear_error();
    ty_tls_reason("the private key does not belong to the certificate");
    return TY_TLS_CONFIG;
  }
  int32_t h = ty_tls_ctx_add(ctx);
  if (h < 0) {
    SSL_CTX_free(ctx);
    ty_tls_reason("too many TLS contexts");
    return h;
  }
  return h;
}

int32_t ty_tls_context_free(int32_t ctx) {
  ty_tls_detail[0] = 0;
  return ty_tls_ctx_release(ctx);
}

/* ---- the handshake ---------------------------------------------------- */

/* The name the certificate has to be for, and the SNI name asked for. An
   address literal and a name are checked against different fields of a
   certificate, so which one this is decides both calls: set1_ip_asc only
   answers for something that parses as an address, and SNI must not carry an
   address at all (RFC 6066). */
static int ty_tls_set_host(SSL *ssl, const char *host) {
  X509_VERIFY_PARAM *p = SSL_get0_param(ssl);
  X509_VERIFY_PARAM_set_hostflags(p, X509_CHECK_FLAG_NO_PARTIAL_WILDCARDS);
  if (X509_VERIFY_PARAM_set1_ip_asc(p, host) == 1) return 0;
  if (X509_VERIFY_PARAM_set1_host(p, host, 0) != 1) return -1;
  /* A peer that serves several certificates uses the name to pick one, and the
     check above is what makes the one it picked the right one. */
  SSL_set_tlsext_host_name(ssl, host);
  return 0;
}

/* A session over an already connected descriptor, with the handshake done. mode
   is 0 for the client and 1 for the server; the client is the one that carries
   a name to check the certificate against. */
static int32_t ty_tls_handshake(int32_t ctx, int32_t fd, const char *host, int server) {
  ty_tls_detail[0] = 0;
  ty_tls_bio_err = 0;
  SSL_CTX *c = ty_tls_ctx_of(ctx);
  if (!c) {
    ty_tls_reason("the TLS context is not open");
    return -EBADF;
  }
  if (fd < 0) return -EINVAL;
  if (!server) {
    if (!host || !*host) return -EINVAL;
    if (strlen(host) > 255) return -ENAMETOOLONG;
  }
  ty_tls_ready();
  ERR_clear_error();
  SSL *ssl = SSL_new(c);
  if (!ssl) {
    ty_tls_reason_openssl();
    return TY_TLS_HANDSHAKE;
  }
  /* Partial writes are enabled so the write loop below can be the socket
     layer's loop -- send until the last byte is out -- and moving the buffer
     between them is allowed, which is what the loop does. */
  SSL_set_mode(ssl, SSL_MODE_ENABLE_PARTIAL_WRITE | SSL_MODE_ACCEPT_MOVING_WRITE_BUFFER | SSL_MODE_AUTO_RETRY);
  if (!server && ty_tls_set_host(ssl, host) != 0) {
    SSL_free(ssl);
    ERR_clear_error();
    ty_tls_reason("the host name cannot be checked against a certificate");
    return TY_TLS_HOST;
  }
  BIO *bio = ty_tls_bio_new(fd);
  if (!bio) {
    SSL_free(ssl);
    ty_tls_reason("the transport could not be set up");
    return TY_TLS_HANDSHAKE;
  }
  /* The same BIO for both directions: it is a descriptor, and SSL_set_bio
     frees it once when the two are the same. */
  SSL_set_bio(ssl, bio, bio);
  ty_tls_bio_err = 0;
  int r = server ? SSL_accept(ssl) : SSL_connect(ssl);
  if (r != 1) {
    int32_t code = ty_tls_failed(ssl, r);
    SSL_free(ssl);
    ty_tls_bio_err = 0;
    return code;
  }
  int32_t h = ty_tls_session_add(ssl);
  if (h < 0) {
    SSL_free(ssl);
    ty_tls_reason("too many TLS connections at once");
    return h;
  }
  return h;
}

int32_t ty_tls_connect(int32_t ctx, int32_t fd, tystr *host) {
  const char *name = host ? ty_tls_cstr(host) : NULL;
  if (!name) return -EINVAL;
  return ty_tls_handshake(ctx, fd, name, 0);
}

int32_t ty_tls_accept(int32_t ctx, int32_t fd) {
  return ty_tls_handshake(ctx, fd, NULL, 1);
}

/* ---- the stream ------------------------------------------------------- */

int32_t ty_tls_read(int32_t session, tyarr *buf, int32_t off, int32_t len) {
  ty_tls_detail[0] = 0;
  ty_tls_bio_err = 0;
  SSL *ssl = ty_tls_session_of(session);
  if (!ssl) return -EBADF;
  if (!ty_tls_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  if (len == 0) return 0; /* read(2) of nothing returns 0, which would read as
                             end of file: answer it here instead */
  ERR_clear_error();
  for (;;) {
    ty_tls_bio_err = 0;
    int n = SSL_read(ssl, buf->data + off, len);
    if (n > 0) return n; /* short is not an error: it is the answer */
    int e = SSL_get_error(ssl, n);
    switch (e) {
      case SSL_ERROR_ZERO_RETURN:
        /* The peer's close_notify: the end of the stream, which is the 0 a
           plain read gives at the same point. */
        return 0;
      case SSL_ERROR_WANT_READ:
      case SSL_ERROR_WANT_WRITE:
        /* A blocking descriptor only reports this when the transport below was
           interrupted or timed out, and a timeout is an answer rather than
           something to wait through again. */
        if (ty_tls_bio_err == TY_TLS_TIMEOUT) {
          ty_tls_reason("timed out");
          return TY_TLS_TIMEOUT;
        }
        continue;
      case SSL_ERROR_SYSCALL:
        if (ty_tls_bio_err == TY_TLS_TIMEOUT) {
          ty_tls_reason("timed out");
          return TY_TLS_TIMEOUT;
        }
        if (n == 0 && ty_tls_bio_err == 0) return 0; /* the peer closed without
                                                        a close_notify: the end
                                                        of the stream all the
                                                        same */
        ty_tls_reason("the connection failed: errno %d", -ty_tls_bio_err);
        return TY_TLS_HANDSHAKE;
      default:
        ty_tls_reason_openssl();
        return TY_TLS_HANDSHAKE;
    }
  }
}

int32_t ty_tls_write_all(int32_t session, tyarr *buf, int32_t off, int32_t len) {
  ty_tls_detail[0] = 0;
  ty_tls_bio_err = 0;
  SSL *ssl = ty_tls_session_of(session);
  if (!ssl) return -EBADF;
  if (!ty_tls_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  const char *p = buf->data + off;
  int32_t done = 0;
  ERR_clear_error();
  while (done < len) {
    ty_tls_bio_err = 0;
    int n = SSL_write(ssl, p + done, len - done);
    if (n > 0) {
      done += n;
      continue;
    }
    int e = SSL_get_error(ssl, n);
    if (e == SSL_ERROR_WANT_READ || e == SSL_ERROR_WANT_WRITE) {
      if (ty_tls_bio_err == TY_TLS_TIMEOUT) {
        ty_tls_reason("timed out");
        return TY_TLS_TIMEOUT;
      }
      continue;
    }
    if (e == SSL_ERROR_ZERO_RETURN) {
      ty_tls_reason("the peer closed the connection");
      return -EPIPE;
    }
    if (e == SSL_ERROR_SYSCALL) {
      if (ty_tls_bio_err == TY_TLS_TIMEOUT) {
        ty_tls_reason("timed out");
        return TY_TLS_TIMEOUT;
      }
      /* The peer being gone is the one failure the socket layer reports as
         EPIPE, and a caller that knows that name should see it here too. */
      if (ty_tls_bio_err == -EPIPE || ty_tls_bio_err == -ECONNRESET) {
        ty_tls_reason("the connection was closed by the peer");
        return -EPIPE;
      }
      ty_tls_reason("the connection failed: errno %d", -ty_tls_bio_err);
      return TY_TLS_HANDSHAKE;
    }
    ty_tls_reason_openssl();
    return TY_TLS_HANDSHAKE;
  }
  return done;
}

int32_t ty_tls_write_str(int32_t session, tystr *s) {
  if (!s) return -EINVAL;
  if (s->blen <= 0) return 0;
  if (s->blen > INT32_MAX) return -EINVAL;
  /* The string's bytes are the same shape as an array's, and the write loop is
     the same loop: an array view of them would allocate on every write. */
  tyarr view;
  memset(&view, 0, sizeof view);
  view.len = s->blen;
  view.data = TY_STR_DATA(s);
  view.esize = 1;
  return ty_tls_write_all(session, &view, 0, (int32_t)s->blen);
}

int32_t ty_tls_close(int32_t session) {
  ty_tls_detail[0] = 0;
  if (session <= 0 || session > TY_TLS_SESSION_MAX) return -EBADF;
  typlat_mutex_lock(&ty_tls_lock);
  SSL *ssl = ty_tls_sessions[session - 1];
  ty_tls_sessions[session - 1] = NULL;
  typlat_mutex_unlock(&ty_tls_lock);
  if (!ssl) return -EBADF;
  /* The close_notify is sent and not waited for: SSL_shutdown's first call
     writes it and returns 0 when the peer's has not arrived, which is where
     this stops. A peer that has already gone makes it fail, and that failure is
     not reported: the connection is being closed either way, and the
     descriptor's own close is the socket layer's to report. */
  ty_tls_bio_err = 0;
  (void)SSL_shutdown(ssl);
  SSL_free(ssl);
  ERR_clear_error();
  return 0;
}

/* ---- the text --------------------------------------------------------- */

/* The reason the last failure on this thread had, for the exception the Teyru
   half raises. Allocated and handed straight back, so no root is needed: the
   collector cannot run between the allocation and the return. */
tystr *ty_tls_detail_text(void) {
  return ty_str_new(ty_tls_detail, (int64_t)strlen(ty_tls_detail));
}

/* Which family a failure code belongs to: 1 certificate, 2 host name, 3
   handshake, 4 configuration, 0 none of them (a socket failure or a timeout,
   which the socket layer already knows how to name). The Teyru half asks
   instead of comparing numbers, so the numbering stays in this file. */
int32_t ty_tls_kind(int32_t code) {
  if (code == TY_TLS_CERT) return 1;
  if (code == TY_TLS_HOST) return 2;
  if (code == TY_TLS_HANDSHAKE) return 3;
  if (code == TY_TLS_CONFIG) return 4;
  return 0;
}
