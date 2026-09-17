/* tyrt_plat_posix.c - the platform layer on a POSIX system.
 *
 * Every function here is the C library's own call, with three rules applied:
 * a failure becomes the negative errno rather than -1, a signal becomes EINTR
 * rather than a decision the call makes for the caller, and the type of an
 * argument is the POSIX one rather than the shared one (socklen_t, size_t).
 *
 * _GNU_SOURCE is this file's own business and no other file's: two of the calls
 * below are GNU extensions. pthread_getattr_np is the only way to ask a thread
 * for its own stack bounds -- pthread_attr_getstack needs attributes that a
 * running thread does not have -- and accept4 is what makes an accepted
 * descriptor close-on-exec in the same call that accepts it, with no window in
 * which another thread's exec could inherit it.
 *
 * macOS is the one other system this file has branches for, and each branch is
 * one API that Linux and macOS spell differently: there is no
 * pthread_getattr_np, no accept4, and no MSG_NOSIGNAL (a socket option,
 * SO_NOSIGPIPE, takes its place, and it has to be set when the socket is
 * made). Nothing else in this file differs, which is the point of writing the
 * layer against POSIX rather than against Linux.
 *
 * The socket half is gated on TYPLAT_WANT_SOCKETS, which the file that links
 * against it defines; see tyrt_plat.h. Nothing in this file is gated on
 * anything else: it is the POSIX implementation, whole.
 */
#define _GNU_SOURCE 1
#define TYPLAT_WANT_SOCKETS 1
#include "tyrt_plat.h"

#include <fcntl.h>
#include <poll.h>
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <time.h>

#if defined(__APPLE__)
#include <sys/socket.h>
#endif

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
  struct timespec ts;
  ts.tv_sec = (time_t)(ns / 1000000000);
  ts.tv_nsec = (long)(ns % 1000000000);
  /* nanosleep writes what is left back into the same timespec, so an
     interrupted wait resumes for the rest of the duration rather than
     starting over. */
  while (nanosleep(&ts, &ts) != 0 && errno == EINTR) {
  }
}

void typlat_yield(void) { sched_yield(); }

/* -------------------------------------------------- locks, threads, waits */

void typlat_mutex_init(typlat_mutex *m) { pthread_mutex_init(m, NULL); }
void typlat_mutex_lock(typlat_mutex *m) { pthread_mutex_lock(m); }
void typlat_mutex_unlock(typlat_mutex *m) { pthread_mutex_unlock(m); }
void typlat_mutex_destroy(typlat_mutex *m) { pthread_mutex_destroy(m); }

/* Which clock this process's condition variables ended up on. Linux builds them
   on the monotonic clock -- it defaults to CLOCK_REALTIME and every deadline in
   the runtime is measured on the monotonic one -- and a system that does not
   offer the choice gets the wall clock, which is what the timed wait converts
   the duration onto. All of them are built here, so one flag describes all of
   them. */
static int cond_on_monotonic = 0;
static int cond_clock_known = 0;

void typlat_cond_init(typlat_cond *c) {
  pthread_condattr_t ca;
  pthread_condattr_init(&ca);
  int mono = 0;
#if !defined(__APPLE__)
  /* macOS has no pthread_condattr_setclock that can be relied on -- its
     pthread_cond_timedwait is documented against the wall clock -- so the
     attribute is not asked for there at all, and asking for it on a system that
     accepts the call and ignores it would be worse than not asking. */
  mono = pthread_condattr_setclock(&ca, CLOCK_MONOTONIC) == 0;
#endif
  if (!cond_clock_known) {
    cond_on_monotonic = mono;
    cond_clock_known = 1;
  }
  pthread_cond_init(c, &ca);
  pthread_condattr_destroy(&ca);
}

void typlat_cond_wait(typlat_cond *c, typlat_mutex *m) { pthread_cond_wait(c, m); }

int typlat_cond_timedwait(typlat_cond *c, typlat_mutex *m, int64_t timeout_ns) {
  /* pthread_cond_timedwait takes the time to wake up at, on the clock the
     variable was built with, so the duration the caller gave becomes a deadline
     on that clock: the monotonic one where the variable was built on it, the
     wall clock where it was not. Either way the deadline is the current time
     plus the duration, read in the same call, so nothing has to be reconciled
     with a clock the caller was using. */
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
#if defined(__APPLE__)
  /* No pthread_getattr_np here: the stack of a running thread is asked for with
     two macOS functions, and the address they return is the high end of it. */
  void *top = pthread_get_stackaddr_np(pthread_self());
  size_t size = pthread_get_stacksize_np(pthread_self());
  if (top && size) {
    *high = (char *)top;
    *low = (char *)top - size;
  }
#else
  pthread_attr_t attr;
  if (pthread_getattr_np(pthread_self(), &attr) != 0) return;
  void *base = NULL;
  size_t size = 0;
  if (pthread_attr_getstack(&attr, &base, &size) == 0) {
    *low = (char *)base;
    *high = (char *)base + size;
  }
  pthread_attr_destroy(&attr);
#endif
}

/* --------------------------------------------------------- fatal faults */

/* Big enough for a handler that only formats a line, with the slack the API
   asks for. SIGSTKSZ is not a constant here -- glibc makes it a sysconf call
   -- so it cannot size an array, and the smallest stack the kernel accepts is
   a few kilobytes on every system this builds for. */
#define TY_ALTSTACK_BYTES (64 * 1024)

/* The runtime's reporter, and whether one has been installed. Written once,
   from ty_init, before any thread exists; read from the handler, which runs on
   whatever thread faulted. */
static int (*fault_reporter)(void *addr) = NULL;

/* The handler. It does not longjmp and it does not return to the faulting
   instruction: a handler entered because the stack ran out has nothing to
   return to, and one that returned would fault again on the same address
   forever. */
static void fault_trampoline(int sig, siginfo_t *si, void *uc) {
  (void)uc;
  if (fault_reporter && fault_reporter(si ? si->si_addr : NULL)) return;
  /* Not the fault this runtime reports. Put the default action back and take
     the signal again, so the process dies the way it would have without a
     handler, and a debugger or a core file sees the fault that actually
     happened rather than a handler that swallowed it. */
  signal(SIGSEGV, SIG_DFL);
  raise(sig);
}

void typlat_fault_handler_install(int (*on_fault)(void *addr)) {
  fault_reporter = on_fault;
  /* SA_ONSTACK: run on the alternate stack, which is the only stack a thread
     that exhausted its own has left. The handler's own signal is blocked for
     the duration of the handler by default, which is what keeps a fault inside
     the handler from re-entering it. */
  struct sigaction sa;
  memset(&sa, 0, sizeof sa);
  sa.sa_sigaction = fault_trampoline;
  sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
  sigemptyset(&sa.sa_mask);
  if (sigaction(SIGSEGV, &sa, NULL) != 0) abort();
}

void typlat_fault_altstack_install(void) {
  char *p = (char *)malloc(TY_ALTSTACK_BYTES);
  if (!p) abort();
  /* The members are set one at a time and never in a positional initializer:
     the order of stack_t's fields is not the same on every system (glibc puts
     ss_flags before ss_size, the BSDs the other way round), and a struct whose
     two sizes were swapped is a stack the kernel rejects. */
  stack_t ss;
  memset(&ss, 0, sizeof ss);
  ss.ss_sp = p;
  ss.ss_size = TY_ALTSTACK_BYTES;
  ss.ss_flags = 0;
  /* Never freed, and never installed over: this is the thread's alternate
     stack for as long as the thread runs, and the thread cannot run again
     after the last frame of its start function returns. */
  if (sigaltstack(&ss, NULL) != 0) abort();
}

/* ---------------------------------------------------------------- sockets */

/* The negation of errno, or -EIO when a call failed without setting it, so that
   a failure is never 0 and never the positive number that a success could be. */
static int neg_errno(void) { return errno ? -errno : -EIO; }

void typlat_init(void) {
  /* Nothing to bring up: the socket layer of a POSIX system is part of the
     process from the start, and a POSIX standard stream is already in binary
     mode -- there is no text mode to be in. */
}

int32_t typlat_socket_open(int family, int type, int protocol) {
  int fd = socket(family, type, protocol);
  if (fd < 0) return (int32_t)neg_errno();
#if defined(__APPLE__)
  /* This system has no MSG_NOSIGNAL, which is the per-write way of saying
     "a write to a socket whose peer has gone must not raise SIGPIPE". Its
     equivalent is a socket option, and it has to be set on the socket before
     anything is written to it: the default action of SIGPIPE is to kill the
     process, which is not a decision a socket write may make for a program. */
  int on = 1;
  (void)setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, sizeof on);
#endif
  return (int32_t)fd;
}

int typlat_socket_bind(int32_t fd, const struct sockaddr *addr, int len) {
  return bind(fd, addr, (socklen_t)len) < 0 ? neg_errno() : 0;
}

int typlat_socket_listen(int32_t fd, int backlog) {
  return listen(fd, backlog) < 0 ? neg_errno() : 0;
}

int32_t typlat_socket_accept(int32_t fd) {
  /* accept4 with SOCK_CLOEXEC rather than accept followed by fcntl: a
     descriptor that another thread's exec could inherit for even a moment is a
     descriptor that leaks into whatever that thread started. */
#if defined(__APPLE__)
  /* macOS has no accept4, so the two steps are one line apart instead: the
     descriptor is created and closed-on-exec before anything else can see it
     -- this layer is called by a runtime that does not start threads inside
     accept, and the window is the width of one fcntl. */
  int c = accept(fd, NULL, NULL);
  if (c >= 0) (void)fcntl(c, F_SETFD, FD_CLOEXEC);
#else
  int c = accept4(fd, NULL, NULL, SOCK_CLOEXEC);
#endif
  return c < 0 ? (int32_t)neg_errno() : (int32_t)c;
}

int typlat_socket_connect(int32_t fd, const struct sockaddr *addr, int len) {
  if (connect(fd, addr, (socklen_t)len) == 0) return 0;
  /* EINTR leaves the connect running rather than aborting it, which is exactly
     what EINPROGRESS means, so the two take one path: the caller waits for
     writability and asks SO_ERROR. */
  if (errno == EINTR || errno == EINPROGRESS) return -EINPROGRESS;
  return neg_errno();
}

int typlat_socket_connected(int32_t fd) {
  int soerr = 0;
  socklen_t slen = sizeof soerr;
  if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &slen) < 0) return neg_errno();
  return soerr != 0 ? -soerr : 0;
}

int typlat_socket_nonblocking(int32_t fd, int on) {
  int flags = fcntl(fd, F_GETFL, 0);
  if (flags < 0) return neg_errno();
  int want = on ? (flags | O_NONBLOCK) : (flags & ~O_NONBLOCK);
  if (flags == want) return 0;
  return fcntl(fd, F_SETFL, want) < 0 ? neg_errno() : 0;
}

int64_t typlat_socket_recv(int32_t fd, void *buf, int len) {
  ssize_t n = recv(fd, buf, (size_t)len, 0);
  return n < 0 ? (int64_t)neg_errno() : (int64_t)n;
}

int64_t typlat_socket_send(int32_t fd, const void *buf, int len) {
  /* MSG_NOSIGNAL: without it a write to a socket whose peer has gone raises
     SIGPIPE, whose default action kills the process. The alternative is
     sigaction(SIG_IGN) process-wide, which is a decision about the whole
     program made by a socket write. */
#if defined(__APPLE__)
  /* The option set when the socket was made is what does this here. */
  ssize_t n = send(fd, buf, (size_t)len, 0);
#else
  ssize_t n = send(fd, buf, (size_t)len, MSG_NOSIGNAL);
#endif
  return n < 0 ? (int64_t)neg_errno() : (int64_t)n;
}

int typlat_socket_shutdown_write(int32_t fd) {
  return shutdown(fd, SHUT_WR) < 0 ? neg_errno() : 0;
}

int typlat_socket_close(int32_t fd) {
  /* Not retried on EINTR. On Linux the descriptor is released even when the
     close reports EINTR, and the number can already belong to another
     descriptor by the time a retry ran. */
  return close(fd) < 0 ? neg_errno() : 0;
}

int typlat_socket_name(int32_t fd, int peer, struct sockaddr *addr, int *len) {
  socklen_t slen = (socklen_t)*len;
  int r = peer ? getpeername(fd, addr, &slen) : getsockname(fd, addr, &slen);
  if (r < 0) return neg_errno();
  *len = (int)slen;
  return 0;
}

int typlat_socket_reuse(int32_t fd, int on) {
  int v = on ? 1 : 0;
  return setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &v, sizeof v) < 0 ? neg_errno() : 0;
}

int typlat_socket_timeout_ms(int32_t fd, int ms) {
  struct timeval tv;
  tv.tv_sec = ms > 0 ? ms / 1000 : 0;
  tv.tv_usec = ms > 0 ? (ms % 1000) * 1000 : 0;
  /* A whole zero timeval is what "no timeout" is spelled as here, which is what
     ms <= 0 means, so no floor is needed for a duration shorter than the
     resolution: the argument is whole milliseconds to begin with. */
  return setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv) < 0 ? neg_errno() : 0;
}

int typlat_socket_wait(int32_t fd, int for_write, int timeout_ms) {
  struct pollfd p;
  p.fd = fd;
  p.events = for_write ? POLLOUT : POLLIN;
  p.revents = 0;
  int r = poll(&p, 1, timeout_ms < 0 ? -1 : timeout_ms);
  return r < 0 ? neg_errno() : r;
}

void typlat_error_text(int code, char *buf, size_t n) {
  const char *m = strerror(code);
  if (!m || !*m) m = "unknown error";
  /* The number goes in because the text is localized on some systems and
     because two failures can share a description; the number cannot. */
  snprintf(buf, n, "%s (errno %d)", m, code);
}

/* ------------------------------------------------------------------ files */

int typlat_open_read(const char *path) { return open(path, O_RDONLY); }

int typlat_open_write(const char *path, int append) {
  return open(path, O_WRONLY | O_CREAT | (append ? O_APPEND : O_TRUNC), 0666);
}

int typlat_mkdir(const char *path) { return mkdir(path, 0777); }

void typlat_temp_root(char *buf, size_t n) {
  const char *base = getenv("TMPDIR");
  if (!base || !*base) base = "/tmp";
  snprintf(buf, n, "%s", base);
}

int typlat_mkdtemp(char *tpl) { return mkdtemp(tpl) != NULL; }
