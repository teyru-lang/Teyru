/* tyrt_nio.c - the C half of java.nio: datagrams, non-blocking channel I/O and
 * the readiness poll a Selector is written on.
 *
 * The convention is tyrt_net.c's, and so is the reason: a negative answer is
 * -errno, a non-negative one is a count or a descriptor, and the Teyru half
 * turns the integer back into an exception. What is new here is only what a
 * stream socket does not need:
 *
 *   - a datagram has an address per call rather than per connection, so sendto
 *     and recvfrom take or answer one, and the answer is a string the caller
 *     reads from a thread-local slot -- a function cannot answer both a count
 *     and a string, and the sender's address is only meaningful right after the
 *     call that received it;
 *   - a non-blocking write accepts what it accepts, so the count it returns is
 *     an answer rather than a truncation to be hidden (the blocking write in
 *     tyrt_net.c loops until the last byte is out, and doing that on a
 *     non-blocking socket is a spin);
 *   - readiness for several descriptors at once is poll(2)/WSAPoll, which is
 *     what makes a Selector one system call instead of one per channel.
 *
 * Threading: the poll and the receive are waits, so they are stop points. The
 * datagram a recv is filling is on the shadow stack for the whole wait, for the
 * reason tyrt_net.c's read documents.
 */
/* Defined before tyrt.h, because tyrt.h includes tyrt_plat.h and that is where
   the socket headers are: defined after it, the declarations would arrive with
   `struct sockaddr` incomplete and every signature here would disagree with the
   definition in the platform layer. */
#define TYPLAT_WANT_SOCKETS 1
#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* The socket declarations, from the one header that names an operating system:
   tyrt_plat.h includes Winsock on Windows and the BSD headers on POSIX behind
   TYPLAT_WANT_SOCKETS, and every call in this file is the layer's, not the
   C library's. Including <winsock2.h> by hand first would put it in front of
   <windows.h> on one platform only. */

/* tyrt_net.c's helpers, for the two this file reuses. They are declared rather
   than included because that file belongs to a different translation unit and
   exports no header of its own (see tyrt_net.h for why). */
int32_t ty_net_read(int32_t fd, tyarr *buf, int32_t off, int32_t len);
int32_t ty_net_close(int32_t fd);

static int ty_nio_is_bytes(tyarr *a) { return a && a->esize == 1; }

/* The bytes of a Teyru string as a C string, or NULL: a name whose bytes hold a
   NUL would name another host if it were truncated. */
static const char *ty_nio_cstr(tystr *s) {
  if (!s) return NULL;
  if ((int64_t)strlen(TY_STR_DATA(s)) != s->blen) return NULL;
  return TY_STR_DATA(s);
}

/* The sender of the last datagram this thread received, per thread: two threads
   receiving at once must not read each other's address, and a caller reads it
   immediately after the call that set it. */
static _Thread_local tystr *ty_nio_sender;

/* Fill addr from a host name and a port. Answers 0 or a negative code. IPv4
   only, for the reason tyrt_net.c states: a listener binds INADDR_ANY and one
   address family on both sides is what makes a local round trip work. */
static int ty_nio_addr(const char *host, int32_t port, struct sockaddr_in *a) {
  if (port < 0 || port > 65535) return -EINVAL;
  memset(a, 0, sizeof *a);
  a->sin_family = AF_INET;
  a->sin_port = htons((uint16_t)port);
  if (!host || !*host || strcmp(host, "0.0.0.0") == 0) {
    a->sin_addr.s_addr = htonl(INADDR_ANY);
    return 0;
  }
  if (inet_pton(AF_INET, host, &a->sin_addr) != 1) return -EINVAL;
  return 0;
}

/* An unbound UDP socket, or a negative code. */
int32_t ty_nio_open_udp(void) {
  int32_t fd = typlat_socket_open(AF_INET, SOCK_DGRAM, 0);
  if (fd < 0) return fd;
  return fd;
}

int32_t ty_nio_bind_udp(int32_t fd, tystr *host, int32_t port) {
  struct sockaddr_in a;
  const char *h = host ? ty_nio_cstr(host) : NULL;
  int e;
  if (fd < 0) return -EINVAL;
  if (host && !h) return -EINVAL;
  if ((e = ty_nio_addr(h, port, &a)) < 0) return e;
  return typlat_socket_bind(fd, (const struct sockaddr *)&a, (int)sizeof a);
}

int32_t ty_nio_sendto(int32_t fd, tyarr *buf, int32_t off, int32_t len,
                      tystr *host, int32_t port) {
  struct sockaddr_in a;
  const char *h;
  int e;
  int64_t n;
  if (fd < 0 || !ty_nio_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  h = ty_nio_cstr(host);
  if (!h) return -EINVAL;
  if ((e = ty_nio_addr(h, port, &a)) < 0) return e;
  TY_ROOT_PUSH(buf);
  ty_thread_stopped_begin();
  n = typlat_socket_sendto(fd, buf->data + off, len, (const struct sockaddr *)&a, (int)sizeof a);
  ty_thread_stopped_end();
  TY_ROOT_POP();
  return (int32_t)n;
}

/* A datagram. out, when it is an int[] of at least two elements, receives the
   sender's port in [0] and its address as a string is left in this thread's
   sender slot (ty_nio_sender0 reads it). The answer is the byte count, or a
   negative code; a buffer smaller than the datagram is EMSGSIZE, not a shorter
   message, because UDP has message boundaries and a truncated one is wrong data
   rather than less data. */
int32_t ty_nio_recvfrom(int32_t fd, tyarr *buf, int32_t off, int32_t len, tyarr *out) {
  struct sockaddr_in a;
  int alen = (int)sizeof a;
  int64_t n;
  char text[64];
  char ip[46];
  if (fd < 0 || !ty_nio_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  if (len == 0) return 0;
  memset(&a, 0, sizeof a);
  for (;;) {
    TY_ROOT_PUSH(buf);
    ty_thread_stopped_begin();
    n = typlat_socket_recvfrom(fd, buf->data + off, len, (struct sockaddr *)&a, &alen);
    ty_thread_stopped_end();
    TY_ROOT_POP();
    if (n == -EINTR) continue;
    break;
  }
  if (n < 0) return (int32_t)n;
  if (!inet_ntop(AF_INET, &a.sin_addr, ip, sizeof ip)) return -EIO;
  snprintf(text, sizeof text, "%s", ip);
  ty_nio_sender = ty_str_new(text, (int64_t)strlen(text));
  if (out && out->esize == 4 && out->len >= 1) {
    ((int32_t *)out->data)[0] = (int32_t)ntohs(a.sin_port);
  }
  return (int32_t)n;
}

/* The sender of the last datagram received on this thread, or NULL before one
   has been. Kept in a thread-local rather than in the runtime's heap of
   Teyru-visible objects because it is read exactly once, by the code right after
   the call that set it. */
tystr *ty_nio_sender0(void) {
  return ty_nio_sender ? ty_nio_sender : ty_str_new("", 0);
}

int32_t ty_nio_close(int32_t fd) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_close(fd);
}

/* A non-blocking write: the count the kernel accepted, which may be shorter than
   the caller asked for and is an answer rather than an error -- a channel's
   write contracts exactly that, and the caller keeps the rest in its buffer. */
int32_t ty_nio_write_some(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  int64_t n;
  if (fd < 0 || !ty_nio_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  if (len == 0) return 0;
  TY_ROOT_PUSH(buf);
  ty_thread_stopped_begin();
  n = typlat_socket_send(fd, buf->data + off, len);
  ty_thread_stopped_end();
  TY_ROOT_POP();
  return (int32_t)n;
}

/* The blocking read tyrt_net.c already has, for a channel that is in blocking
   mode: the same contract (count, 0 at end of stream, -EAGAIN for a timeout). */
int32_t ty_nio_read(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  return ty_net_read(fd, buf, off, len);
}

int32_t ty_nio_nonblocking(int32_t fd, int32_t on) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_nonblocking(fd, on != 0);
}

/* A wait on several descriptors. fds and events are int[] of the same length,
   revents receives the mask for each (1 readable, 2 writable, 4 error) and is
   written only for the entries that are ready. timeout_ms < 0 waits forever.
   The answer is the number ready, 0 on timeout, or a negative code. */
int32_t ty_nio_poll(tyarr *fds, tyarr *events, tyarr *revents, int32_t timeout_ms) {
  int32_t n;
  int r;
  if (!fds || !events || !revents) return -EINVAL;
  if (fds->esize != 4 || events->esize != 4 || revents->esize != 4) return -EINVAL;
  n = (int32_t)fds->len;
  if (events->len < n || revents->len < n) return -EINVAL;
  if (n == 0) {
    /* Nothing registered is still a wait with a deadline, and a Selector that
       blocked forever on an empty set would be untestable. */
    if (timeout_ms > 0) typlat_sleep_ns((int64_t)timeout_ms * 1000000);
    return 0;
  }
  /* The poll is a wait like any other: a collection must not wait for it, and
     the arrays it reads are plain integers, so nothing here is a root. */
  ty_thread_stopped_begin();
  r = typlat_poll((int32_t *)fds->data, (int32_t *)events->data, (int32_t *)revents->data,
                  n, timeout_ms);
  ty_thread_stopped_end();
  return (int32_t)r;
}

tystr *ty_nio_strerror(int32_t code) {
  char buf[256];
  if (code >= 0) {
    snprintf(buf, sizeof buf, "no error");
  } else {
    typlat_error_text(-code, buf, sizeof buf);
  }
  return ty_str_new(buf, (int64_t)strlen(buf));
}

/* ---- stream channels --------------------------------------------------- */

/* A TCP socket in non-blocking mode. A channel starts that way: blocking mode is
   the option, not the default, because a Selector is the reason the channel
   exists. */
int32_t ty_nio_open_tcp(void) {
  int32_t fd = typlat_socket_open(AF_INET, SOCK_STREAM, 0);
  if (fd < 0) return fd;
  {
    int e = typlat_socket_nonblocking(fd, 1);
    if (e < 0) {
      (void)typlat_socket_close(fd);
      return e;
    }
  }
  return fd;
}

/* Begin a connect. 0 is connected already, -EINPROGRESS is a connect in flight
   (what a channel's connect() reports as false and finishConnect waits out), a
   negative code is a failure. */
int32_t ty_nio_connect_start(int32_t fd, tystr *host, int32_t port) {
  struct sockaddr_in a;
  const char *h = ty_nio_cstr(host);
  int e;
  if (fd < 0 || !h || !*h) return -EINVAL;
  if ((e = ty_nio_addr(h, port, &a)) < 0) return e;
  return typlat_socket_connect(fd, (const struct sockaddr *)&a, (int)sizeof a);
}

int32_t ty_nio_connect_finish(int32_t fd) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_connected(fd);
}

int32_t ty_nio_accept(int32_t fd) {
  if (fd < 0) return -EINVAL;
  {
    int32_t c = typlat_socket_accept(fd);
    if (c < 0) return c;
    {
      int e = typlat_socket_nonblocking(c, 1);
      if (e < 0) {
        (void)typlat_socket_close(c);
        return e;
      }
    }
    return c;
  }
}

/* A listening socket in non-blocking mode. */
int32_t ty_nio_listen(int32_t port, int32_t backlog) {
  int32_t fd = typlat_socket_open(AF_INET, SOCK_STREAM, 0);
  struct sockaddr_in a;
  int e;
  if (fd < 0) return fd;
  (void)typlat_socket_reuse(fd, 1);
  if ((e = ty_nio_addr(NULL, port, &a)) < 0) {
    (void)typlat_socket_close(fd);
    return e;
  }
  if ((e = typlat_socket_bind(fd, (const struct sockaddr *)&a, (int)sizeof a)) < 0 ||
      (e = typlat_socket_listen(fd, backlog > 0 ? backlog : 128)) < 0) {
    (void)typlat_socket_close(fd);
    return e;
  }
  if ((e = typlat_socket_nonblocking(fd, 1)) < 0) {
    (void)typlat_socket_close(fd);
    return e;
  }
  return fd;
}

int32_t ty_nio_local_port(int32_t fd) {
  struct sockaddr_in a;
  int len = (int)sizeof a;
  if (fd < 0) return -EINVAL;
  memset(&a, 0, sizeof a);
  if (typlat_socket_name(fd, 0, (struct sockaddr *)&a, &len) < 0) return -EINVAL;
  return (int32_t)ntohs(a.sin_port);
}

int32_t ty_nio_shutdown_write(int32_t fd) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_shutdown_write(fd);
}

int32_t ty_nio_set_blocking(int32_t fd, int32_t on) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_nonblocking(fd, on ? 0 : 1);
}
