/* tyrt_plat.h - the operating system, and the only header that names it.
 *
 * Everything in the runtime that is not the same call on every platform is
 * declared here and implemented in tyrt_plat_posix.c or tyrt_plat_win.c. The
 * rest of the runtime -- the collector in tyrt.c, the threads and monitors in
 * tyrt_thread.c, the sockets and files in tyrt_net.c -- calls these and carries
 * no platform conditionals of its own, so "what does this runtime do on
 * Windows" has one file to read instead of a hundred scattered #ifdefs.
 *
 * ------------------------------------------------------------------- shape
 *
 * Three things shape the interface:
 *
 *   - A failure is a negative error code, never a bare -1 with errno beside it.
 *     `errno` is a thread-local of the C library on POSIX and is not set at all
 *     by a Winsock call, so a caller that reached for it would be right on one
 *     platform and wrong on the other. Each function here returns 0, a count, a
 *     descriptor, or the negation of the code that describes the failure, in
 *     the same numbering the Teyru-visible layer already uses (-EAGAIN for a
 *     timeout, and so on). The Windows side maps its own error space onto that
 *     numbering; see map_wsa_error in tyrt_plat_win.c for the translation and
 *     for the one case -- a timed-out read -- where Windows reports a different
 *     code for the same event.
 *
 *   - A socket is an int32_t, as it has always been in this runtime, on both
 *     platforms. A Winsock SOCKET is a pointer-sized handle rather than a file
 *     descriptor, but every handle Windows hands out fits in 32 bits, and the
 *     conversion lives here. Descriptors and sockets remain two separate
 *     namespaces, as they were: a file descriptor is only ever passed to the
 *     file functions and a socket only to the socket functions.
 *
 *   - What is *not* here is as deliberate as what is. mingw's C library is a
 *     POSIX-shaped CRT: open, read, write, close, stat, unlink, rmdir, opendir,
 *     readdir and closedir exist with the signatures this runtime already
 *     calls, so tyrt_net.c calls them directly and there is nothing to
 *     abstract. The four places where the shape differs -- the flags open needs
 *     (a text-mode CRT would translate CRLF under a binary file), mkdir's arity,
 *     mkdtemp, and where the temporary directory is -- are behind
 *     typlat_open_read and friends below.
 *
 * ------------------------------------------------------------------ sockets
 *
 * The socket declarations are behind TYPLAT_WANT_SOCKETS because including
 * <winsock2.h> is not free: on Windows it pulls in <windows.h>, which defines
 * min and max as macros and drags a hundred thousand lines of declarations into
 * the translation unit. The generated program and the four runtime files that
 * never open a socket do not include them; tyrt_net.c and the two platform
 * implementations define the macro before including tyrt.h and do.
 */
#ifndef TYRT_PLAT_H
#define TYRT_PLAT_H

#include <stdint.h>
#include <stddef.h>
#include <errno.h>

#if defined(_WIN32)
#define TYPLAT_WINDOWS 1
/* The threading backend is winpthreads on Windows and the C library's threads
   on POSIX. Both provide pthread.h, and this is the only place either is
   named: tyrt_thread.c works with the types below and nothing else. */
#include <pthread.h>
/* read, write and close for a file descriptor live in io.h on this CRT. */
#include <io.h>
#else
#define TYPLAT_POSIX 1
#include <pthread.h>
#include <unistd.h>
#endif

/* ----------------------------------------------------------- time and cpu */

/* Nanoseconds from a clock that does not jump: CLOCK_MONOTONIC on POSIX, and
   QueryPerformanceCounter's equivalent behind winpthreads' clock_gettime on
   Windows. Every deadline in the runtime is measured on this clock, because a
   deadline compared against the wall clock expires an hour early the first time
   the machine steps its time. */
int64_t typlat_monotonic_ns(void);

/* Milliseconds since the epoch, which is what System.currentTimeMillis means and
   the one thing here that is *meant* to follow the wall clock. */
int64_t typlat_realtime_ms(void);

/* Sleep for a duration, restarting the wait when a signal interrupts it: Java's
   Thread.sleep promises the whole duration, not however much of it fits before
   a handler ran. */
void typlat_sleep_ns(int64_t ns);

/* Give up the rest of this thread's time slice. */
void typlat_yield(void);

/* -------------------------------------------------- locks, threads, waits */

/* The heap lock, each thread's monitor state, and the world lock the collector
   stops the world through are all one of these. */
typedef pthread_mutex_t typlat_mutex;
#define TYPLAT_MUTEX_INITIALIZER PTHREAD_MUTEX_INITIALIZER

void typlat_mutex_init(typlat_mutex *m);
void typlat_mutex_lock(typlat_mutex *m);
void typlat_mutex_unlock(typlat_mutex *m);
void typlat_mutex_destroy(typlat_mutex *m);

/* A condition variable, and the clock its timed waits are measured on. That
   clock is monotonic on POSIX and the wall clock on Windows, where winpthreads
   refuses CLOCK_MONOTONIC for a condition variable -- pthread_condattr_setclock
   returns EINVAL -- so typlat_cond_init records which clock this variable got
   and a relative timeout below is converted into a deadline on that clock. The
   interface takes a *duration* and not a deadline for exactly that reason: the
   caller's clock and the variable's clock are not always the same one, and the
   translation belongs where that is known. */
typedef pthread_cond_t typlat_cond;
#define TYPLAT_COND_INITIALIZER PTHREAD_COND_INITIALIZER

void typlat_cond_init(typlat_cond *c);
void typlat_cond_wait(typlat_cond *c, typlat_mutex *m);
/* Waits until signalled or until timeout_ns has passed. Returns 1 when the time
   ran out and 0 when the variable was signalled. */
int typlat_cond_timedwait(typlat_cond *c, typlat_mutex *m, int64_t timeout_ns);
void typlat_cond_signal(typlat_cond *c);
void typlat_cond_broadcast(typlat_cond *c);
void typlat_cond_destroy(typlat_cond *c);

typedef pthread_t typlat_thread;

typlat_thread typlat_thread_self(void);
int typlat_thread_same(typlat_thread a, typlat_thread b);
/* Starts a detached thread: nothing in the language waits for a thread to leave
   the operating system, and a thread that is never joined must not hold its
   stack until the program ends. Returns 0, or the error number pthread_create
   reports -- an EAGAIN a program has to see rather than a thread that silently
   never ran. */
int typlat_thread_begin(typlat_thread *t, void *(*fn)(void *), void *arg);
/* The stack bounds of the *calling* thread: *low is the lowest address, *high
   the highest. The collector scans each thread's stack, so it needs both, and
   neither is knowable from C portably: POSIX has pthread_getattr_np, a GNU
   extension, and Windows has GetCurrentThreadStackLimits. Either one leaving
   both at NULL is a runtime the collector cannot scan, and the caller treats it
   as such. */
void typlat_thread_stack_bounds(char **low, char **high);

/* --------------------------------------------------------- fatal faults */

/* The last thing standing between a thread that has run out of stack and a
   silent death: a handler for the fault the machine raises, installed once for
   the process before any thread exists. `on_fault` is handed the address the
   fault was at and answers 1 when it has dealt with it, 0 when the fault is not
   the one this runtime reports -- and then the fault is taken again with the
   default action, so the program dies the way it would have without a handler
   and a debugger or a core file sees the real fault rather than a handler that
   swallowed it.

   The handler runs on the alternate signal stack, which is the point of it:
   the fault this exists to report is one the thread's own stack caused, and a
   handler entered on that stack has no room to run at all. */
void typlat_fault_handler_install(int (*on_fault)(void *addr));

/* Give the calling thread an alternate signal stack. Per thread, because the
   alternate stack is: a handler for one thread's fault runs on that thread's
   alternate stack, and there is no way to share one. Called from thread start,
   so a spawned thread that overflows is reported like the main one. */
void typlat_fault_altstack_install(void);

/* ---------------------------------------------------------------- sockets */

/* Platform startup, once, before the program runs. Called by ty_init, which
   every generated program passes through, and again -- harmlessly -- by the
   first socket, so that a program cannot reach a socket on a platform that
   needed starting up first. It does two things on Windows and nothing on POSIX:

     - WSAStartup, without which there is no socket layer at all.

     - Puts stdin, stdout and stderr in binary mode. A CRT in its default text
       mode translates every "\n" a program writes into "\r\n" and treats a
       received 0x1A as end of file. Teyru has one expected output per program
       for every platform, and one console and pipe behaviour: a line written by
       a Teyru program is the same bytes wherever it runs, which is what makes a
       program's output comparable, testable and identical in a pipeline. Java's
       println follows the platform's line separator instead; that is a decision
       this runtime does not take, because it would make every text file a
       program writes depend on the machine it ran on.

   It is declared outside the gate below because it names no socket type: no
   file has to include Winsock to start the platform. */
void typlat_init(void);

#if defined(TYPLAT_WANT_SOCKETS)
#if defined(_WIN32)
#include <winsock2.h>
#include <ws2tcpip.h>
#else
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
/* getaddrinfo and freeaddrinfo are the same function on both platforms and are
   called directly rather than through the layer; the declarations are here
   because they are socket declarations and this is where the socket headers
   are included. */
#include <netdb.h>
#endif

/* Create an IPv4 TCP socket, or a negative code. */
int32_t typlat_socket_open(int family, int type, int protocol);
int typlat_socket_bind(int32_t fd, const struct sockaddr *addr, int len);
int typlat_socket_listen(int32_t fd, int backlog);
/* The next queued connection, or a negative code. A descriptor this layer
   returns is closed with typlat_socket_close and nothing else. */
int32_t typlat_socket_accept(int32_t fd);

/* Start a connect. On a non-blocking socket the attempt usually does not finish
   here: -EINPROGRESS (or -EINTR, which leaves it running) means the caller has
   to wait for writability with typlat_socket_wait and then ask
   typlat_socket_connected. */
int typlat_socket_connect(int32_t fd, const struct sockaddr *addr, int len);
/* 0 when the connection that was in flight came up, a negative code when it did
   not. The answer is in SO_ERROR, which is where the kernel reports a refused
   or unreachable connection that connect could not return directly. */
int typlat_socket_connected(int32_t fd);
int typlat_socket_nonblocking(int32_t fd, int on);

/* A count, which may be short and is not an error, or a negative code. -EAGAIN
   is what a socket with a read timeout reports when the timer fires. */
int64_t typlat_socket_recv(int32_t fd, void *buf, int len);
/* Bytes accepted, which is what makes the caller's loop correct, or a negative
   code. A write whose peer has gone raises SIGPIPE on POSIX by default, so the
   send carries MSG_NOSIGNAL there and needs no such thing on Windows, where no
   signal is raised and the failure is reported as a code. */
int64_t typlat_socket_send(int32_t fd, const void *buf, int len);

/* Half close: the end of the stream goes out while the other direction stays
   readable. */
int typlat_socket_shutdown_write(int32_t fd);
int typlat_socket_close(int32_t fd);

/* The local (peer == 0) or remote end of a connection, as a sockaddr the caller
   casts to sockaddr_in. *len is the size of the buffer on the way in and the
   size written on the way out. */
int typlat_socket_name(int32_t fd, int peer, struct sockaddr *addr, int *len);

int typlat_socket_reuse(int32_t fd, int on);
/* SO_RCVTIMEO: one read or accept call's deadline, not a whole exchange's.
   ms <= 0 clears it, which means "wait forever" in both APIs. */
int typlat_socket_timeout_ms(int32_t fd, int ms);

/* Wait until a socket is readable (for_write == 0) or writable. Returns a
   positive value when it is ready, 0 when the deadline passed, or a negative
   code -- -EINTR in particular, which is a signal and not a deadline, so a
   caller that bounds a wait with a timeout retries rather than reporting it. */
int typlat_socket_wait(int32_t fd, int for_write, int timeout_ms);

/* The text for a positive error code, with the number in it: strerror's message
   and "(errno N)" on POSIX, the system message and "(wsa N)" for a Winsock code
   on Windows. Never empty. */
void typlat_error_text(int code, char *buf, size_t n);
#endif /* TYPLAT_WANT_SOCKETS */

/* ------------------------------------------------------------------ files */

/* Open for reading, or the descriptor, or -1. */
int typlat_open_read(const char *path);
/* Open for writing, creating it, truncating it unless append is set, in binary:
   a CRT left in text mode translates a CRLF pair and treats a 0x1A as end of
   file, which corrupts every file that is not a text file this runtime reads or
   writes. This is the whole reason the two opens are here rather than in
   tyrt_net.c. */
int typlat_open_write(const char *path, int append);

/* Create one directory. POSIX takes a mode, this CRT does not, and a call that
   had to be written twice is a call that belongs in the layer. */
int typlat_mkdir(const char *path);

/* The directory a temporary name is made in: TMPDIR, or /tmp, or the directory
   Windows keeps for it. No trailing separator. */
void typlat_temp_root(char *buf, size_t n);
/* Create the directory named by tpl, which ends in six X's; the X's are
   replaced in place and the result is the directory's name. Returns 1 on
   success. The replacement is atomic, so two programs starting at once cannot
   choose the same name. */
int typlat_mkdtemp(char *tpl);

#endif /* TYRT_PLAT_H */
