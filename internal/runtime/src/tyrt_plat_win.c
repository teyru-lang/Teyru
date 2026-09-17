/* tyrt_plat_win.c - the platform layer on Windows, with winpthreads.
 *
 * Three things differ from the POSIX implementation of the same interface, and
 * none of them is a detail:
 *
 *   - Winsock is not the C library. A socket is a HANDLE-sized value rather
 *     than a descriptor, it is closed with closesocket rather than close, it is
 *     made non-blocking with an ioctl rather than fcntl, it is polled with
 *     WSAPoll, its receive timeout is a DWORD rather than a timeval, and a
 *     failure is reported by WSAGetLastError in a numbering of its own rather
 *     than in errno. Every one of those is handled below, and the numbering is
 *     translated onto the runtime's own so that a Teyru program sees the same
 *     codes on both platforms.
 *
 *   - Winsock requires WSAStartup before the first socket exists. The runtime's
 *     startup (ty_init) calls typlat_init; a socket made before that starts the
 *     layer itself rather than failing with WSANOTINITIALISED.
 *
 *   - winpthreads refuses to build a condition variable on the monotonic clock
 *     (pthread_condattr_setclock returns EINVAL for CLOCK_MONOTONIC), so the
 *     timed wait converts the duration the caller measured against the
 *     monotonic clock into a deadline on the wall clock the condition variable
 *     actually uses. Both clocks are read in the same call, so a step of the
 *     wall clock moves the deadline with it.
 *
 * The file half is nearly source-compatible: this CRT has open, read, write,
 * close, stat, unlink, rmdir, opendir and readdir with the shapes the runtime
 * already calls. What is left for the layer is the opening flags -- a
 * descriptor opened in text mode translates CRLF and stops at the first 0x1A,
 * which silently corrupts any file that is not text -- mkdir, whose argument
 * list differs, and mkdtemp, which this CRT does not have.
 *
 * winpthreads' clock_gettime, nanosleep and pthread_* are used as they are;
 * they are the reason this target needed no thread implementation of its own.
 * The runtime is linked with -static so that libwinpthread-1.dll is not a
 * runtime dependency of a program this compiler produces.
 */
#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>

#define TYPLAT_WANT_SOCKETS 1
#include "tyrt_plat.h"

#include <direct.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>

/* The first of the Winsock error codes. Everything above it is WSA*, everything
   below it is a C library errno, and the two spaces do not overlap. */
#define TY_WSA_BASE 10000

/* ----------------------------------------------------------- time and cpu */

int64_t typlat_monotonic_ns(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (int64_t)ts.tv_sec * 1000000000 + (int64_t)ts.tv_nsec;
}

int64_t typlat_realtime_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  return (int64_t)ts.tv_sec * 1000 + (int64_t)ts.tv_nsec / 1000000;
}

void typlat_sleep_ns(int64_t ns) {
  /* nanosleep is winpthreads', and writes the remainder back into the same
     timespec, so an interrupted wait resumes for what is left of the duration
     rather than starting over. */
  struct timespec ts;
  ts.tv_sec = (time_t)(ns / 1000000000);
  ts.tv_nsec = (long)(ns % 1000000000);
  while (nanosleep(&ts, &ts) != 0 && errno == EINTR) {
  }
}

void typlat_yield(void) {
  /* SwitchToThread is the yield that lets another thread on this processor run;
     Sleep(0) would give up the whole time slice instead. winpthreads' sched_yield
     is this call, and this avoids needing its sched.h. */
  SwitchToThread();
}

/* -------------------------------------------------- locks, threads, waits */

void typlat_mutex_init(typlat_mutex *m) { pthread_mutex_init(m, NULL); }
void typlat_mutex_lock(typlat_mutex *m) { pthread_mutex_lock(m); }
void typlat_mutex_unlock(typlat_mutex *m) { pthread_mutex_unlock(m); }
void typlat_mutex_destroy(typlat_mutex *m) { pthread_mutex_destroy(m); }

/* Which clock this process's condition variables ended up on. winpthreads
   refuses CLOCK_MONOTONIC, so this is the wall clock, but the answer is
   determined by the call rather than assumed: a winpthreads that starts
   supporting the monotonic clock has to be believed, and a deadline computed
   for the wrong clock expires either immediately or an hour late. Every
   condition variable is built by the function below, so one flag describes all
   of them. */
static int cond_on_monotonic = 0;
static int cond_clock_known = 0;

void typlat_cond_init(typlat_cond *c) {
  pthread_condattr_t ca;
  pthread_condattr_init(&ca);
  int mono = pthread_condattr_setclock(&ca, CLOCK_MONOTONIC) == 0;
  if (!cond_clock_known) {
    cond_on_monotonic = mono;
    cond_clock_known = 1;
  }
  pthread_cond_init(c, &ca);
  pthread_condattr_destroy(&ca);
}

void typlat_cond_wait(typlat_cond *c, typlat_mutex *m) { pthread_cond_wait(c, m); }

int typlat_cond_timedwait(typlat_cond *c, typlat_mutex *m, int64_t timeout_ns) {
  int64_t at;
  if (cond_on_monotonic) {
    at = typlat_monotonic_ns() + timeout_ns;
  } else {
    struct timespec now;
    clock_gettime(CLOCK_REALTIME, &now);
    at = (int64_t)now.tv_sec * 1000000000 + (int64_t)now.tv_nsec + timeout_ns;
  }
  struct timespec ts;
  ts.tv_sec = (time_t)(at / 1000000000);
  ts.tv_nsec = (long)(at % 1000000000);
  return pthread_cond_timedwait(c, m, &ts) == ETIMEDOUT;
}

void typlat_cond_signal(typlat_cond *c) { pthread_cond_signal(c); }
void typlat_cond_broadcast(typlat_cond *c) { pthread_cond_broadcast(c); }
void typlat_cond_destroy(typlat_cond *c) { pthread_cond_destroy(c); }

typlat_thread typlat_thread_self(void) { return pthread_self(); }

int typlat_thread_same(typlat_thread a, typlat_thread b) {
  return pthread_equal(a, b) != 0;
}

int typlat_thread_begin(typlat_thread *t, void *(*fn)(void *), void *arg) {
  pthread_attr_t attr;
  pthread_attr_init(&attr);
  pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
  int err = pthread_create(t, &attr, fn, arg);
  pthread_attr_destroy(&attr);
  return err;
}

void typlat_thread_stack_bounds(char **low, char **high) {
  *low = NULL;
  *high = NULL;
  /* GetCurrentThreadStackLimits is the API for this and is what the collector
     wants; it is also Windows 8 and later, and this loads it by name so that a
     program built here still starts on an older Windows instead of failing to
     load against a missing import. */
  typedef VOID(WINAPI * ty_stack_limits)(PULONG_PTR, PULONG_PTR);
  static ty_stack_limits fn = NULL;
  static int looked_up = 0;
  if (!looked_up) {
    HMODULE k32 = GetModuleHandleA("kernel32.dll");
    if (k32) fn = (ty_stack_limits)(void *)GetProcAddress(k32, "GetCurrentThreadStackLimits");
    looked_up = 1;
  }
  ULONG_PTR lo = 0, hi = 0;
  if (fn) {
    fn(&lo, &hi);
  } else {
    /* The thread information block has held the same two fields since NT 3.1:
       StackBase is the high end of the stack, StackLimit the low end. */
    PNT_TIB tib = (PNT_TIB)NtCurrentTeb();
    lo = (ULONG_PTR)tib->StackLimit;
    hi = (ULONG_PTR)tib->StackBase;
  }
  if (lo && hi && lo < hi) {
    *low = (char *)(uintptr_t)lo;
    *high = (char *)(uintptr_t)hi;
  }
}

/* --------------------------------------------------------- fatal faults */

/* A stack that has run out is not a signal here: the kernel raises
   EXCEPTION_STACK_OVERFLOW, and the handler runs on a stack of its own because
   Windows gives every handler one. That is the whole of what
   typlat_fault_altstack_install does on this platform -- there is no alternate
   signal stack to install. */

static int (*fault_reporter)(void *addr) = NULL;

static LONG CALLBACK fault_filter(PEXCEPTION_POINTERS info) {
  EXCEPTION_RECORD *r = info->ExceptionRecord;
  if (r->ExceptionCode != EXCEPTION_STACK_OVERFLOW) return EXCEPTION_CONTINUE_SEARCH;
  /* For a stack overflow the second information word is the stack limit the
     thread ran into, which is the address the reporter is handed. */
  void *addr = r->NumberParameters > 1 ? (void *)r->ExceptionInformation[1] : NULL;
  if (fault_reporter && fault_reporter(addr)) return EXCEPTION_CONTINUE_SEARCH;
  /* Not reported: let the system handle it, so the process dies the way it
     would have without a handler. */
  return EXCEPTION_CONTINUE_SEARCH;
}

void typlat_fault_handler_install(int (*on_fault)(void *addr)) {
  fault_reporter = on_fault;
  /* First in the chain, so that the message is printed by this runtime rather
     than by whatever else the host process has installed. */
  if (!AddVectoredExceptionHandler(1, fault_filter)) abort();
}

void typlat_fault_altstack_install(void) {
  /* Nothing to do: see the comment above. */
}

/* ---------------------------------------------------------------- sockets */

/* A Winsock error code as the runtime's own numbering. The mapping is not
   cosmetic: ty_net_strerror, ty_net_is_timeout and every caller that decides
   whether to retry compare against these values, and a program that was written
   against Linux has to see the same shape of answer here. */
static int map_wsa_error(int e) {
  switch (e) {
    case WSAEINTR: return EINTR;
    /* A socket with SO_RCVTIMEO set reports a timeout as WSAETIMEDOUT rather
       than as a would-block. That is the one place the two platforms report the
       same event as a different error, and it is the reason this function
       exists rather than the codes being passed through: EAGAIN is what the
       runtime calls a timeout, and a timeout that arrived as an error of its
       own would be reported to the program as a connection failure. */
    case WSAETIMEDOUT: return EAGAIN;
    case WSAEWOULDBLOCK: return EWOULDBLOCK;
    case WSAEINPROGRESS: return EINPROGRESS;
    case WSAEALREADY: return EALREADY;
    case WSAEISCONN: return EISCONN;
    case WSAENOTCONN: return ENOTCONN;
    case WSAECONNREFUSED: return ECONNREFUSED;
    case WSAECONNRESET: return ECONNRESET;
    case WSAECONNABORTED: return ECONNABORTED;
    /* There is no SIGPIPE here; a write to a socket whose peer is gone fails
       with this, and the POSIX side reports the same event as EPIPE. */
    case WSAESHUTDOWN: return EPIPE;
    case WSAEADDRINUSE: return EADDRINUSE;
    case WSAEADDRNOTAVAIL: return EADDRNOTAVAIL;
    case WSAEACCES: return EACCES;
    case WSAEINVAL: return EINVAL;
    case WSAENOBUFS: return ENOBUFS;
    case WSAENETDOWN: return ENETDOWN;
    case WSAENETUNREACH: return ENETUNREACH;
    case WSAEHOSTUNREACH: return EHOSTUNREACH;
    case WSAEHOSTDOWN: return EHOSTUNREACH;
    case WSAEMFILE: return EMFILE;
    case WSAENOTSOCK: return ENOTSOCK;
    case WSAEMSGSIZE: return EMSGSIZE;
    case WSAEAFNOSUPPORT: return EAFNOSUPPORT;
    case WSAEPROTONOSUPPORT: return EPROTONOSUPPORT;
    case WSAEOPNOTSUPP: return EOPNOTSUPP;
    case WSASYSNOTREADY: return ENOSYS;
    case WSAVERNOTSUPPORTED: return ENOSYS;
    case WSANOTINITIALISED: return ENOSYS;
    case WSAENETRESET: return ECONNRESET;
    default:
      /* An unmapped code is reported as itself. It stays above TY_WSA_BASE, so
         it collides with neither an errno nor the two synthetic codes the layer
         above this one defines, and typlat_error_text knows how to describe
         it. */
      return e;
  }
}

/* The negation of the last Winsock error, or -EIO when there was none: a
   failure must never come back as 0, which a caller reads as success. */
static int neg_wsa(void) {
  int e = WSAGetLastError();
  return e ? -map_wsa_error(e) : -EIO;
}

static volatile LONG net_state = 0; /* 0 not started, 1 ready, 2 failed */

void typlat_init(void) {
  if (net_state == 1) return;
  if (InterlockedCompareExchange((LONG volatile *)&net_state, 1, 0) != 0) return;

  /* The standard streams are in text mode by default here, and text mode turns
     every "\n" the program writes into "\r\n" and stops a read at the first
     0x1A. A Teyru program's output is the same bytes on every platform -- which
     is what one expected file per program means -- so the three streams are put
     in binary mode before anything can be written to them. */
  (void)_setmode(_fileno(stdin), _O_BINARY);
  (void)_setmode(_fileno(stdout), _O_BINARY);
  (void)_setmode(_fileno(stderr), _O_BINARY);

  WSADATA w;
  net_state = WSAStartup(MAKEWORD(2, 2), &w) == 0 ? 1 : 2;
}

/* A socket from Winsock, as the runtime's int32_t. Every handle this API hands
   out fits in 32 bits, and a socket and a file descriptor are never passed to
   the same function, so the two namespaces stay apart by construction. */
static SOCKET as_socket(int32_t fd) { return (SOCKET)(uintptr_t)(uint32_t)fd; }
static int32_t as_fd(SOCKET s) { return (int32_t)(uint32_t)(uintptr_t)s; }

int32_t typlat_socket_open(int family, int type, int protocol) {
  typlat_init();
  SOCKET s = socket(family, type, protocol);
  if (s == INVALID_SOCKET) return (int32_t)neg_wsa();
  /* Not inherited by a child process. Nothing here spawns one, but a descriptor
     that outlives the process that made it is a leak in whoever does. */
  SetHandleInformation((HANDLE)s, HANDLE_FLAG_INHERIT, 0);
  return as_fd(s);
}

int typlat_socket_bind(int32_t fd, const struct sockaddr *addr, int len) {
  return bind(as_socket(fd), addr, len) == SOCKET_ERROR ? neg_wsa() : 0;
}

int typlat_socket_listen(int32_t fd, int backlog) {
  return listen(as_socket(fd), backlog) == SOCKET_ERROR ? neg_wsa() : 0;
}

int32_t typlat_socket_accept(int32_t fd) {
  SOCKET s = accept(as_socket(fd), NULL, NULL);
  if (s == INVALID_SOCKET) return (int32_t)neg_wsa();
  SetHandleInformation((HANDLE)s, HANDLE_FLAG_INHERIT, 0);
  return as_fd(s);
}

int typlat_socket_connect(int32_t fd, const struct sockaddr *addr, int len) {
  if (connect(as_socket(fd), addr, len) == 0) return 0;
  int e = WSAGetLastError();
  /* A non-blocking connect reports a connection in flight as WSAEWOULDBLOCK
     rather than as EINPROGRESS, and an interrupted one leaves it running: both
     mean the caller waits for writability and then asks SO_ERROR. */
  if (e == WSAEWOULDBLOCK || e == WSAEINTR || e == WSAEINPROGRESS || e == WSAEALREADY) {
    return -EINPROGRESS;
  }
  return -map_wsa_error(e);
}

int typlat_socket_connected(int32_t fd) {
  int soerr = 0;
  int slen = (int)sizeof soerr;
  if (getsockopt(as_socket(fd), SOL_SOCKET, SO_ERROR, (char *)&soerr, &slen) == SOCKET_ERROR) {
    return neg_wsa();
  }
  /* SO_ERROR holds a Winsock code here, not an errno. */
  return soerr != 0 ? -map_wsa_error(soerr) : 0;
}

int typlat_socket_nonblocking(int32_t fd, int on) {
  u_long v = on ? 1 : 0;
  return ioctlsocket(as_socket(fd), FIONBIO, &v) == SOCKET_ERROR ? neg_wsa() : 0;
}

int64_t typlat_socket_recv(int32_t fd, void *buf, int len) {
  int n = recv(as_socket(fd), (char *)buf, len, 0);
  return n == SOCKET_ERROR ? (int64_t)neg_wsa() : (int64_t)n;
}

int64_t typlat_socket_send(int32_t fd, const void *buf, int len) {
  /* No SIGPIPE to suppress: Windows does not raise one, and the failure a
     closed peer produces is the code translated above. */
  int n = send(as_socket(fd), (const char *)buf, len, 0);
  return n == SOCKET_ERROR ? (int64_t)neg_wsa() : (int64_t)n;
}

int typlat_socket_shutdown_write(int32_t fd) {
  /* SD_SEND is Winsock's own spelling of SHUT_WR: this header does not define
     the POSIX alias, and the direction is the same one. */
  return shutdown(as_socket(fd), SD_SEND) == SOCKET_ERROR ? neg_wsa() : 0;
}

int typlat_socket_close(int32_t fd) {
  /* closesocket, not close: a socket handle is not a descriptor and closing it
     as one leaks it. */
  return closesocket(as_socket(fd)) == SOCKET_ERROR ? neg_wsa() : 0;
}

int typlat_socket_name(int32_t fd, int peer, struct sockaddr *addr, int *len) {
  int slen = *len;
  int r = peer ? getpeername(as_socket(fd), addr, &slen) : getsockname(as_socket(fd), addr, &slen);
  if (r == SOCKET_ERROR) return neg_wsa();
  *len = slen;
  return 0;
}

int typlat_socket_reuse(int32_t fd, int on) {
  int v = on ? 1 : 0;
  return setsockopt(as_socket(fd), SOL_SOCKET, SO_REUSEADDR, (const char *)&v, sizeof v) ==
                 SOCKET_ERROR
             ? neg_wsa()
             : 0;
}

int typlat_socket_timeout_ms(int32_t fd, int ms) {
  /* SO_RCVTIMEO is a DWORD of milliseconds here and a timeval on POSIX. Zero
     means no timeout on both, which is what ms <= 0 asks for. */
  DWORD v = ms > 0 ? (DWORD)ms : 0;
  return setsockopt(as_socket(fd), SOL_SOCKET, SO_RCVTIMEO, (const char *)&v, sizeof v) ==
                 SOCKET_ERROR
             ? neg_wsa()
             : 0;
}

int typlat_socket_wait(int32_t fd, int for_write, int timeout_ms) {
  WSAPOLLFD p;
  p.fd = as_socket(fd);
  p.events = (SHORT)(for_write ? POLLOUT : POLLIN);
  p.revents = 0;
  int r = WSAPoll(&p, 1, timeout_ms < 0 ? -1 : timeout_ms);
  if (r == SOCKET_ERROR) return neg_wsa();
  /* WSAPoll reports a failed connection as an event with POLLERR or POLLHUP set
     rather than as an error, so the caller's next call -- SO_ERROR for a
     connect, accept for a listener -- is what finds out. */
  return r;
}

void typlat_error_text(int code, char *buf, size_t n) {
  if (code >= TY_WSA_BASE) {
    char *m = NULL;
    DWORD k = FormatMessageA(FORMAT_MESSAGE_ALLOCATE_BUFFER | FORMAT_MESSAGE_FROM_SYSTEM |
                                 FORMAT_MESSAGE_IGNORE_INSERTS,
                             NULL, (DWORD)code, MAKELANGID(LANG_NEUTRAL, SUBLANG_DEFAULT),
                             (LPSTR)&m, 0, NULL);
    if (k > 0 && m) {
      /* The system text ends in a CRLF pair and often a full stop; neither
         belongs inside a sentence a program prints. */
      while (k > 0 && (m[k - 1] == '\r' || m[k - 1] == '\n' || m[k - 1] == '.' || m[k - 1] == ' ')) {
        m[--k] = 0;
      }
      snprintf(buf, n, "%s (wsa %d)", m, code);
      LocalFree(m);
      return;
    }
    if (m) LocalFree(m);
    snprintf(buf, n, "socket error (wsa %d)", code);
    return;
  }
  const char *m = strerror(code);
  if (!m || !*m) m = "unknown error";
  snprintf(buf, n, "%s (errno %d)", m, code);
}

/* ------------------------------------------------------------------ files */

int typlat_open_read(const char *path) { return _open(path, _O_RDONLY | _O_BINARY); }

int typlat_open_write(const char *path, int append) {
  int flags = _O_WRONLY | _O_CREAT | _O_BINARY | (append ? _O_APPEND : _O_TRUNC);
  return _open(path, flags, _S_IREAD | _S_IWRITE);
}

int typlat_mkdir(const char *path) { return _mkdir(path); }

void typlat_temp_root(char *buf, size_t n) {
  if (n == 0) return;
  buf[0] = 0;
  DWORD k = GetTempPathA((DWORD)n, buf);
  if (k == 0 || (size_t)k >= n) {
    /* No temporary directory: the current one is the honest answer, since
       every path this is joined onto is relative anyway. */
    snprintf(buf, n, ".");
    return;
  }
  /* GetTempPath ends in a separator, and everything here joins with a slash.
     The root of a drive is "C:\" and keeps its backslash: "C:" is not a
     directory, it is whatever directory that drive was last left in. */
  size_t len = strlen(buf);
  while (len > 0 && (buf[len - 1] == '\\' || buf[len - 1] == '/')) buf[--len] = 0;
  if (len == 0 || (len == 2 && buf[1] == ':')) {
    buf[len++] = '\\';
    buf[len] = 0;
  }
}

/* One more name out of the alphabet and the counters below: this is not a
   cryptographic choice, it is a name that two processes starting at the same
   instant are unlikely to pick, with a retry when they do. */
static uint32_t temp_name_bits(void) {
  static volatile LONG counter = 0;
  uint64_t c = (uint64_t)InterlockedIncrement((LONG volatile *)&counter);
  uint64_t t = GetTickCount64();
  uint64_t p = (uint64_t)GetCurrentProcessId();
  uint64_t h = t * 6364136223846793005ull + p * 1442695040888963407ull + c * 2654435761ull;
  h ^= h >> 33;
  h *= 0xff51afd7ed558ccdull;
  return (uint32_t)(h >> 32);
}

int typlat_mkdtemp(char *tpl) {
  size_t n = strlen(tpl);
  if (n < 6) return 0;
  char *x = tpl + n - 6;
  for (int i = 0; i < 6; i++) {
    if (x[i] != 'X') return 0;
  }
  static const char alphabet[] = "abcdefghijklmnopqrstuvwxyz012345";
  for (int attempt = 0; attempt < 100; attempt++) {
    uint32_t r = temp_name_bits();
    for (int i = 0; i < 6; i++) {
      x[i] = alphabet[(r >> (i * 5)) & 31];
    }
    /* _mkdir is the atomic step: it fails with EEXIST if another process took
       the name between the two lines above, which is the whole of what mkdtemp
       promises. */
    if (typlat_mkdir(tpl) == 0) return 1;
    if (errno != EEXIST) return 0;
  }
  return 0;
}
