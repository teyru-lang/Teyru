/* tyrt_net.c - sockets and files.
 *
 * The language has no threads, so none of this is written for concurrency: a
 * descriptor is an int, a server is a loop, and every helper here is a
 * straight-line conversation with the kernel. What it is written for is being
 * correct under load, which for a socket layer means four things, all of them
 * handled below and none of them optional:
 *
 *   - a read may return fewer bytes than asked for, and the caller is told how
 *     many rather than being lied to;
 *   - a write may accept fewer bytes than given, so it is looped until the last
 *     byte is out (the classic bug: a response of 100 KB goes out as one
 *     write() and arrives truncated the moment the peer's window closes);
 *   - a signal may interrupt any of these, so EINTR is retried inside the loop
 *     rather than escaping as a spurious failure;
 *   - a write to a socket whose peer has gone raises SIGPIPE, whose default
 *     action kills the process, so every send carries MSG_NOSIGNAL instead of
 *     the runtime touching the process-wide signal disposition.
 *
 * Time is the other half. A read that blocks forever cannot be told apart from
 * a peer that is merely slow, and a server that hangs cannot be tested, so
 * SO_RCVTIMEO is available through setSoTimeout and reports itself as
 * TY_NET_TIMEOUT. It is deliberately a code of its own: a timeout returning 0
 * would be read as end of file by every loop that checks for it, which is the
 * mistake this file goes out of its way to make impossible.
 *
 * The file half is the same shape without the timing: fopen and the FILE API
 * are avoided on purpose, because their buffering hides both the partial write
 * and the errno that explains the failure, and because a program that answers
 * requests cannot afford a stdio lock. open/read/write/stat/opendir are the
 * whole of it -- no shell, no system(), no path ever reaching a command line.
 *
 * Neither half names an operating system. The socket calls below are the
 * platform layer's (tyrt_plat.h): the BSD calls on POSIX and Winsock on
 * Windows, with the differences -- a handle rather than a descriptor, a Winsock
 * error code rather than errno, an ioctl rather than fcntl, a millisecond DWORD
 * rather than a timeval -- settled inside that layer. The file calls are
 * POSIX-shaped on both, because mingw's CRT is: what differs is which flags a
 * binary file needs, how many arguments mkdir takes and where temporary names
 * go, and those four are layer calls.
 */

/* Including <winsock2.h> is not free -- it brings <windows.h> and its macros
   with it -- so the socket half of the platform layer is behind this macro and
   only this file and the two platform implementations define it. */
#define TYPLAT_WANT_SOCKETS 1

/* tyrt.h and not tyrt_net.h, which is the header this file has but cannot use.
   The driver copies exactly one networking file into the directory it builds
   in -- tyrt_net.c, the one internal/runtime/embed.go embeds -- so a header
   sitting next to this file in the source tree would not be there at build
   time and `#include "tyrt_net.h"` would be a fatal error. The declarations
   this file needs are therefore in it, and tyrt_net.h carries the same ones for
   C that links *against* the layer. tests/native/net_c_test.c includes both, so
   the compiler checks the two agree rather than trusting that they do. */
#include "tyrt.h"

#include <dirent.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

/* The largest name getaddrinfo is handed from this file. A host name longer
   than this is not a name any resolver can use. */
#define TY_HOST_MAX 256

/* accept()'s backlog when the caller does not care. 128 is what Linux's
   somaxconn clamps to by default anyway, so asking for more changes nothing and
   asking for less drops connections a burst would otherwise survive. */
#define TY_BACKLOG_DEFAULT 128

/* The most of a path this file will build out of a prefix, so that a
   pathological prefix cannot run the template past PATH_MAX. */
#define TY_PREFIX_MAX 64

/* How long the directory a temporary name is made in may be. A Windows
   temporary directory is a user profile path and can be long; a path that does
   not fit is refused by the layer rather than truncated into a different
   directory. */
#define TY_TEMP_ROOT_MAX 512

/* The result a read, an accept or a connect gives when it ran out of time. It
   is -EAGAIN because that is what the kernel reports when SO_RCVTIMEO expires
   on a blocking socket, and because every socket here is blocking outside the
   one connect that is non-blocking on purpose: an EAGAIN on a read can only
   mean the timer fired. It must never be confused with the 0 a read returns at
   end of file, which is the bug this whole convention exists to prevent. */
#define TY_NET_TIMEOUT (-EAGAIN)

/* The name did not resolve. getaddrinfo has no errno for this -- it returns
   EAI_* codes of its own -- and Java reports it as an exception type of its
   own rather than as an I/O failure, so it gets a code of its own here. The
   value is far below every errno (all of which are under 2000), so the two
   spaces cannot collide. */
#define TY_NET_UNKNOWN_HOST (-10001)

/* What a path names. lib/16_file.teyru reads these values, so TY_FILE_NONE
   must stay 0: it is the answer for a path that is not there, and the answer
   for a call that failed is negative. */
#define TY_FILE_NONE 0
#define TY_FILE_REG 1
#define TY_FILE_DIR 2
#define TY_FILE_OTHER 3

/* ---- small helpers ---------------------------------------------------- */

/* The bytes of a Teyru string as a C string, or NULL when it cannot be one.
   Every path and host name goes through here: a string whose bytes contain a
   NUL cannot name a file, and silently truncating it at the NUL would open the
   wrong one. ty_str_new guarantees the terminator, so the length check is a
   test for an embedded NUL and nothing else. */
static const char *ty_cstr(tystr *s) {
  if (!s) return NULL;
  if ((int64_t)strlen(TY_STR_DATA(s)) != s->blen) return NULL;
  return TY_STR_DATA(s);
}

/* A byte array is the only array this file reads or writes. The element size is
   the test: every byte[] the codegen creates has esize 1, and a short[] or an
   int[] passed here by mistake would otherwise be filled with bytes and read
   back as numbers. */
static int ty_is_bytes(tyarr *a) { return a && a->esize == 1; }

/* The reason a function that also returns an object failed. Such a function
   cannot report it in its return value the way the rest of this file does, and
   the emitter has no way to hand a C pointer back to Teyru, so the caller
   passes in a one-element int[] and reads the element afterwards. Writing the
   element rather than the array is not a detail: the array's header holds the
   class pointer the collector follows, so `*err = e` on a tyarr * would put an
   errno where the class pointer belongs and crash the next collection. The
   check is there for the same reason -- a caller that passed a byte[] or an
   empty array gets no write at all rather than a corrupted neighbour. */
static void ty_set_err(tyarr *err, int32_t v) {
  if (err && err->esize == 4 && err->len >= 1) ((int32_t *)err->data)[0] = v;
}

/* ---- sockets ---------------------------------------------------------- */

int32_t ty_net_listen(int32_t port, int32_t backlog, int32_t reuse) {
  if (port < 0 || port > 65535) return -EINVAL;
  int32_t fd = typlat_socket_open(AF_INET, SOCK_STREAM, 0);
  if (fd < 0) return fd;
  if (reuse) {
    /* A failure here is not fatal: without it the bind may still succeed, and
       refusing to listen because the option was rejected would turn a
       convenience into a requirement. */
    (void)typlat_socket_reuse(fd, 1);
  }
  struct sockaddr_in a;
  memset(&a, 0, sizeof a);
  a.sin_family = AF_INET;
  a.sin_addr.s_addr = htonl(INADDR_ANY);
  a.sin_port = htons((uint16_t)port);
  int r = typlat_socket_bind(fd, (const struct sockaddr *)&a, (int)sizeof a);
  if (r < 0) {
    (void)typlat_socket_close(fd);
    return r;
  }
  r = typlat_socket_listen(fd, backlog > 0 ? backlog : TY_BACKLOG_DEFAULT);
  if (r < 0) {
    (void)typlat_socket_close(fd);
    return r;
  }
  return fd;
}

/* One candidate address, connected and put back into blocking mode. The
   connect is made non-blocking even when the caller asked for no timeout: it
   turns an unbounded wait in the kernel into a wait this code can bound, and it
   is the only way connect can be given a deadline at all. */
static int32_t ty_connect_one(const struct addrinfo *r, int32_t timeout_ms) {
  int32_t fd = typlat_socket_open(r->ai_family, r->ai_socktype, r->ai_protocol);
  if (fd < 0) return fd;
  int e = typlat_socket_nonblocking(fd, 1);
  if (e < 0) {
    (void)typlat_socket_close(fd);
    return e;
  }
  e = typlat_socket_connect(fd, r->ai_addr, (int)r->ai_addrlen);
  if (e != 0) {
    /* -EINPROGRESS is a connect that is running rather than one that failed,
       and an interrupted connect is one of those: the layer reports both the
       same way, because a signal does not abort the attempt. */
    if (e != -EINPROGRESS) {
      (void)typlat_socket_close(fd);
      return e;
    }
    for (;;) {
      int pr = typlat_socket_wait(fd, 1, timeout_ms < 0 ? -1 : timeout_ms);
      if (pr < 0) {
        if (pr == -EINTR) continue; /* a signal is not a deadline */
        (void)typlat_socket_close(fd);
        return pr;
      }
      if (pr == 0) {
        /* The deadline passed with the connection still in flight. The socket
           is closed rather than handed on: a connection that completes later
           would otherwise arrive with nobody waiting for it. */
        (void)typlat_socket_close(fd);
        return TY_NET_TIMEOUT;
      }
      break;
    }
    /* Pollable for writing is not the same as connected: the answer is in
       SO_ERROR, which is where the kernel reports a refused or unreachable
       connection that the non-blocking connect could not return directly. */
    e = typlat_socket_connected(fd);
    if (e < 0) {
      (void)typlat_socket_close(fd);
      return e;
    }
  }
  /* Every read and write in this file assumes a blocking socket, so the flag
     goes back the way it was found. */
  e = typlat_socket_nonblocking(fd, 0);
  if (e < 0) {
    (void)typlat_socket_close(fd);
    return e;
  }
  return fd;
}

int32_t ty_net_connect(tystr *host, int32_t port, int32_t timeout_ms) {
  const char *name = ty_cstr(host);
  if (!name || !*name) return -EINVAL;
  if (strlen(name) >= TY_HOST_MAX) return -ENAMETOOLONG;
  if (port < 0 || port > 65535) return -EINVAL;
  char serv[8];
  snprintf(serv, sizeof serv, "%d", (int)port);
  struct addrinfo hints;
  struct addrinfo *res = NULL;
  memset(&hints, 0, sizeof hints);
  /* IPv4 only, and not because IPv6 is hard: a listener binds INADDR_ANY, so a
     name that resolves to ::1 first would reach a server that is right there
     and get refused. One address family on both sides is what makes
     `new Socket("localhost", ss.getLocalPort())` work. */
  hints.ai_family = AF_INET;
  hints.ai_socktype = SOCK_STREAM;
  hints.ai_protocol = IPPROTO_TCP;
  if (getaddrinfo(name, serv, &hints, &res) != 0) return TY_NET_UNKNOWN_HOST;
  int32_t first_err = 0;
  int32_t fd = -1;
  for (struct addrinfo *r = res; r; r = r->ai_next) {
    int32_t c = ty_connect_one(r, timeout_ms);
    if (c >= 0) {
      fd = c;
      break;
    }
    /* The first failure is the one reported, because the first address is the
       one the resolver preferred and therefore the one that explains why the
       name did not work. */
    if (first_err == 0) first_err = c;
  }
  freeaddrinfo(res);
  if (fd >= 0) return fd;
  return first_err != 0 ? first_err : TY_NET_UNKNOWN_HOST;
}

int32_t ty_net_accept(int32_t fd, int32_t timeout_ms) {
  if (fd < 0) return -EINVAL;
  for (;;) {
    if (timeout_ms >= 0) {
      /* accept has no timeout of its own that can be relied on -- SO_RCVTIMEO
         is documented for reads and its effect on accept differs between
         kernels -- so the wait happens first and the accept is then known to
         return rather than block. */
      int pr = typlat_socket_wait(fd, 0, timeout_ms);
      if (pr < 0) {
        if (pr == -EINTR) continue;
        return pr;
      }
      if (pr == 0) return TY_NET_TIMEOUT;
    }
    int32_t c = typlat_socket_accept(fd);
    if (c >= 0) return c;
    if (c == -EINTR) continue;
    /* A client that gave up between the poll and the accept is not a failure of
       this server: the connection it abandoned is dropped and the next one is
       taken. */
    if (c == -ECONNABORTED) continue;
    /* Readiness that was already consumed by the time accept ran. With a
       timeout the poll is re-entered (and will report the deadline); without
       one there is nothing to wait for, so the accept simply runs again. */
    if (c == -EAGAIN || c == -EWOULDBLOCK) {
      if (timeout_ms >= 0) continue;
      continue;
    }
    return c;
  }
}

int32_t ty_net_read(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  if (fd < 0) return -EINVAL;
  if (!ty_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  if (len == 0) return 0; /* read(2) of nothing returns 0, which would read as
                             end of file: answer it here instead */
  for (;;) {
    int64_t n = typlat_socket_recv(fd, buf->data + off, len);
    if (n >= 0) return (int32_t)n; /* short is not an error: it is the answer */
    if (n == -EINTR) continue;
    return (int32_t)n; /* -EAGAIN when SO_RCVTIMEO fired, which the prelude
                          turns into a timeout rather than into end of file */
  }
}

int32_t ty_net_write_all(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  if (fd < 0) return -EINVAL;
  if (!ty_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  const char *p = buf->data + off;
  int32_t done = 0;
  while (done < len) {
    int64_t n = typlat_socket_send(fd, p + done, len - done);
    if (n < 0) {
      if (n == -EINTR) continue;
      return (int32_t)n;
    }
    if (n == 0) {
      /* send returning 0 on a stream socket means the peer is gone; reporting
         it as EPIPE keeps the callers' one error path. */
      return -EPIPE;
    }
    done += (int32_t)n;
  }
  return done;
}

int32_t ty_net_write_str(int32_t fd, tystr *s) {
  if (fd < 0) return -EINVAL;
  if (!s) return -EINVAL;
  int64_t len = s->blen;
  if (len <= 0) return 0;
  if (len > INT32_MAX) return -EINVAL;
  const char *p = TY_STR_DATA(s);
  int32_t done = 0;
  while (done < (int32_t)len) {
    int64_t n = typlat_socket_send(fd, p + done, (int32_t)len - done);
    if (n < 0) {
      if (n == -EINTR) continue;
      return (int32_t)n;
    }
    if (n == 0) return -EPIPE;
    done += (int32_t)n;
  }
  return done;
}

int32_t ty_net_shutdown_write(int32_t fd) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_shutdown_write(fd);
}

int32_t ty_net_close(int32_t fd) {
  if (fd < 0) return -EINVAL;
  /* Not retried on EINTR. On Linux the descriptor is released even when the
     close reports EINTR, and the number can already belong to another
     descriptor by the time a retry ran, so the second close would close
     somebody else's. Reporting the failure and forgetting the descriptor is
     the only safe pair of actions. */
  return typlat_socket_close(fd);
}

int32_t ty_net_set_timeout(int32_t fd, int32_t ms) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_timeout_ms(fd, ms);
}

int32_t ty_net_set_reuse(int32_t fd, int32_t on) {
  if (fd < 0) return -EINVAL;
  return typlat_socket_reuse(fd, on != 0);
}

/* Fills buf with "address:port" for the local (peer == 0) or the remote end.
   Shared by the two exported helpers so that the port and the address are
   always rendered the same way. */
static int ty_addr_text(int32_t fd, int peer, char *buf, size_t n) {
  struct sockaddr_in a;
  int len = (int)sizeof a;
  memset(&a, 0, sizeof a);
  int r = typlat_socket_name(fd, peer, (struct sockaddr *)&a, &len);
  if (r < 0) return r;
  char ip[INET_ADDRSTRLEN];
  /* inet_ntop is the one socket call that is the same function on both
     platforms, so it is called directly rather than through the layer. */
  if (!inet_ntop(AF_INET, &a.sin_addr, ip, sizeof ip)) return -EIO;
  snprintf(buf, n, "%s:%u", ip, (unsigned)ntohs(a.sin_port));
  return 0;
}

int32_t ty_net_local_port(int32_t fd) {
  struct sockaddr_in a;
  int len = (int)sizeof a;
  memset(&a, 0, sizeof a);
  if (fd < 0) return -EINVAL;
  int r = typlat_socket_name(fd, 0, (struct sockaddr *)&a, &len);
  if (r < 0) return r;
  return (int32_t)ntohs(a.sin_port);
}

tystr *ty_net_local_addr(int32_t fd) {
  char buf[64];
  if (fd < 0) return NULL;
  if (ty_addr_text(fd, 0, buf, sizeof buf) != 0) return NULL;
  return ty_str_new(buf, (int64_t)strlen(buf));
}

tystr *ty_net_peer_addr(int32_t fd) {
  char buf[64];
  if (fd < 0) return NULL;
  if (ty_addr_text(fd, 1, buf, sizeof buf) != 0) return NULL;
  return ty_str_new(buf, (int64_t)strlen(buf));
}

tystr *ty_net_strerror(int32_t code) {
  char buf[256];
  if (code == TY_NET_TIMEOUT) {
    /* No errno to quote: nothing failed, time simply ran out. */
    snprintf(buf, sizeof buf, "timed out");
  } else if (code == TY_NET_UNKNOWN_HOST) {
    snprintf(buf, sizeof buf, "name does not resolve");
  } else if (code < 0) {
    /* The layer's text, because the code it describes is the layer's: an errno
       on POSIX and a Winsock error on Windows. It carries the number for the
       same reason it always did -- the text is localized and two failures can
       share a description, while the number cannot. */
    typlat_error_text(-code, buf, sizeof buf);
  } else {
    snprintf(buf, sizeof buf, "no error");
  }
  return ty_str_new(buf, (int64_t)strlen(buf));
}

/* A byte[] as a String, which is Net.stringFrom in the prelude. The bytes are
   decoded as UTF-8 and anything ill-formed becomes U+FFFD, exactly as
   String(byte[]) does in Java: a network read can stop in the middle of a
   sequence, and a string holding the halves of one would be a string whose
   length and bytes disagree, which is what the read API must never see.
   ty_str_of_bytes copies what it keeps, so the array can be reused or collected
   the moment this returns and nothing here holds a reference across an
   allocation. */
tystr *ty_net_bytes_to_str(tyarr *b, int32_t off, int32_t len) {
  if (!ty_is_bytes(b)) return NULL;
  return ty_str_of_bytes(b, off, len);
}

int32_t ty_net_is_timeout(int32_t code) { return code == TY_NET_TIMEOUT; }

int32_t ty_net_is_unknown_host(int32_t code) {
  return code == TY_NET_UNKNOWN_HOST;
}

/* ---- files ------------------------------------------------------------ */

int32_t ty_file_kind(tystr *path) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  struct stat st;
  if (stat(p, &st) < 0) {
    /* "Not there" is an answer to the question that was asked, not a failure
       of it: File.exists() is the caller and a thrown exception would be
       wrong. */
    if (errno == ENOENT || errno == ENOTDIR) return TY_FILE_NONE;
    return -errno;
  }
  if (S_ISREG(st.st_mode)) return TY_FILE_REG;
  if (S_ISDIR(st.st_mode)) return TY_FILE_DIR;
  return TY_FILE_OTHER;
}

int64_t ty_file_size(tystr *path) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  struct stat st;
  if (stat(p, &st) < 0) return -errno;
  return (int64_t)st.st_size;
}

int32_t ty_file_delete(tystr *path) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  struct stat st;
  if (stat(p, &st) < 0) return -errno;
  /* unlink refuses a directory and rmdir refuses a file, so which one to call
     is decided by the type rather than by trying one and falling back. */
  if (S_ISDIR(st.st_mode)) {
    if (rmdir(p) < 0) return -errno;
    return 0;
  }
  if (unlink(p) < 0) return -errno;
  return 0;
}

int32_t ty_file_mkdirs(tystr *path) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  size_t n = strlen(p);
  char *buf = (char *)malloc(n + 1);
  if (!buf) return -ENOMEM;
  memcpy(buf, p, n + 1);
  /* A trailing slash would make the final component empty; the loop below
     would then try to mkdir("") and fail on a path that is perfectly good. */
  while (n > 1 && buf[n - 1] == '/') buf[--n] = 0;
  for (char *q = buf + 1;; q++) {
    if (*q == '/' || *q == 0) {
      int last = (*q == 0);
      char save = *q;
      *q = 0;
      /* One directory at a time, through the layer: POSIX takes a mode here and
         this CRT does not. What the separators are is the caller's business and
         both platforms take a slash. */
      if (typlat_mkdir(buf) < 0 && errno != EEXIST) {
        int e = errno;
        free(buf);
        return -e;
      }
      if (last) break;
      *q = save;
    }
  }
  free(buf);
  /* EEXIST is not proof of success: a regular file in the way of a component
     makes mkdir fail with the same errno. Asking again settles it. */
  struct stat st;
  if (stat(p, &st) < 0) return -errno;
  if (!S_ISDIR(st.st_mode)) return -ENOTDIR;
  return 0;
}

/* qsort's comparator. The names are char*, so the void* arguments are pointers
   to those pointers. */
static int ty_cmp_names(const void *a, const void *b) {
  return strcmp(*(const char *const *)a, *(const char *const *)b);
}

tyarr *ty_file_list(tystr *path, tyarr *err) {
  if (!err || err->esize != 4 || err->len < 1) return NULL;
  ty_set_err(err, 0);
  const char *p = ty_cstr(path);
  if (!p || !*p) {
    ty_set_err(err, EINVAL);
    return NULL;
  }
  DIR *d = opendir(p);
  if (!d) {
    ty_set_err(err, errno);
    return NULL;
  }
  char **names = NULL;
  size_t count = 0, cap = 0;
  struct dirent *ent;
  errno = 0;
  while ((ent = readdir(d)) != NULL) {
    if (strcmp(ent->d_name, ".") == 0 || strcmp(ent->d_name, "..") == 0) continue;
    if (count == cap) {
      size_t ncap = cap ? cap * 2 : 32;
      char **nn = (char **)realloc(names, ncap * sizeof *nn);
      if (!nn) {
        ty_set_err(err, ENOMEM);
        goto fail;
      }
      names = nn;
      cap = ncap;
    }
    names[count] = strdup(ent->d_name);
    if (!names[count]) {
      ty_set_err(err, ENOMEM);
      goto fail;
    }
    count++;
  }
  if (errno != 0) {
    /* readdir reports a failure by returning NULL and setting errno -- but a
       plain end of directory also leaves errno alone only because it was
       cleared just above. */
    ty_set_err(err, errno);
    goto fail;
  }
  closedir(d);
  d = NULL;
  /* readdir's order is the filesystem's, which is neither stable between runs
     nor the same on two machines. A listing a program prints has to be
     reproducible, so it is sorted here once. */
  if (count > 1) qsort(names, count, sizeof *names, ty_cmp_names);

  /* The array is the only Teyru object this function makes before it returns
     it, and ty_str_new allocates once per element -- any one of those
     allocations can collect. The array is therefore on the shadow stack for
     the whole loop: nothing else keeps a reference to it, and a collection in
     the middle of the loop would otherwise free it and hand the next string the
     same memory. The elements are zeroed by the allocator, so a collection
     triggered by the third string sees an array with two live elements and
     three nulls, which is exactly what tracing expects. */
  tyarr *a = ty_alloc_arr((int64_t)count, 8);
  a->refs = 1; /* the elements are strings: the collector has to follow them */
  TY_ROOT_PUSH(a);
  for (size_t i = 0; i < count; i++) {
    ((void **)a->data)[i] = ty_str_new(names[i], (int64_t)strlen(names[i]));
  }
  TY_ROOT_POP();
  for (size_t i = 0; i < count; i++) free(names[i]);
  free(names);
  return a;

fail:
  /* The failure code was recorded where the caller will read it; closing and
     freeing below must not overwrite it with whatever they set errno to. */
  if (d) closedir(d);
  for (size_t i = 0; i < count; i++) free(names[i]);
  free(names);
  return NULL;
}

tyarr *ty_file_read_bytes(tystr *path, tyarr *err) {
  if (!err || err->esize != 4 || err->len < 1) return NULL;
  ty_set_err(err, 0);
  const char *p = ty_cstr(path);
  if (!p || !*p) {
    ty_set_err(err, EINVAL);
    return NULL;
  }
  int fd = typlat_open_read(p);
  if (fd < 0) {
    ty_set_err(err, errno);
    return NULL;
  }
  /* The buffer is grown as the file is read rather than sized from stat,
     because stat's answer is a snapshot and is wrong for a file that grows, a
     pipe, and anything under /proc. It is plain malloc'd memory: the collector
     never sees it, so no amount of reading can move it. */
  size_t cap = 8192, len = 0;
  char *buf = (char *)malloc(cap);
  if (!buf) {
    (void)close(fd);
    ty_set_err(err, ENOMEM);
    return NULL;
  }
  for (;;) {
    if (len == cap) {
      if (cap > (size_t)1 << 30) {
        free(buf);
        (void)close(fd);
        ty_set_err(err, EFBIG);
        return NULL;
      }
      char *nb = (char *)realloc(buf, cap * 2);
      if (!nb) {
        free(buf);
        (void)close(fd);
        ty_set_err(err, ENOMEM);
        return NULL;
      }
      buf = nb;
      cap *= 2;
    }
    ssize_t n = read(fd, buf + len, cap - len);
    if (n < 0) {
      if (errno == EINTR) continue;
      int e = errno;
      free(buf);
      (void)close(fd);
      ty_set_err(err, e);
      return NULL;
    }
    if (n == 0) break;
    len += (size_t)n;
  }
  (void)close(fd);
  tyarr *a = ty_alloc_arr((int64_t)len, 1);
  memcpy(a->data, buf, len);
  free(buf);
  return a;
}

/* The write loop, over a raw buffer so that the bytes and the string entry
   points share it: a short write is the same bug whichever kind of data hit
   it. */
static int32_t ty_write_fd(int fd, const char *data, int64_t len) {
  int64_t done = 0;
  while (done < len) {
    ssize_t n = write(fd, data + done, (size_t)(len - done));
    if (n < 0) {
      if (errno == EINTR) continue;
      return -errno;
    }
    if (n == 0) return -EIO; /* a full disk reports itself this way */
    done += n;
  }
  return len > INT32_MAX ? INT32_MAX : (int32_t)len;
}

int32_t ty_file_write_bytes(tystr *path, tyarr *buf, int32_t off, int32_t len,
                            int32_t append) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  if (!ty_is_bytes(buf)) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  /* The layer's open, not open(): this CRT translates a CRLF pair and stops at
     a 0x1A in the default text mode, which would corrupt every byte written
     here that is not text. */
  int fd = typlat_open_write(p, append);
  if (fd < 0) return -errno;
  int32_t r = ty_write_fd(fd, buf->data + off, len);
  if (r < 0) {
    int e = -r;
    (void)close(fd);
    return -e;
  }
  /* close is where a write error that the kernel buffered finally surfaces, so
     its failure is reported as the failure of the write rather than thrown
     away: a full disk is exactly this case. */
  if (close(fd) < 0) return -errno;
  return r;
}

int32_t ty_file_write_str(tystr *path, tystr *s, int32_t append) {
  const char *p = ty_cstr(path);
  if (!p || !*p) return -EINVAL;
  if (!s) return -EINVAL;
  int fd = typlat_open_write(p, append);
  if (fd < 0) return -errno;
  int32_t r = ty_write_fd(fd, TY_STR_DATA(s), s->blen);
  if (r < 0) {
    int e = -r;
    (void)close(fd);
    return -e;
  }
  if (close(fd) < 0) return -errno;
  return r;
}

tystr *ty_file_temp_dir(tystr *prefix) {
  /* Where temporary names go is one of the four things that differ between the
     platforms, so the layer answers it: TMPDIR or /tmp, and the directory
     Windows keeps for the same purpose, which is neither of those. */
  char base[TY_TEMP_ROOT_MAX];
  typlat_temp_root(base, sizeof base);
  char pfx[TY_PREFIX_MAX];
  size_t k = 0;
  /* The bytes of a string are its own tail, so there is no second null to test
     here -- only the string itself. */
  if (prefix) {
    for (int64_t i = 0; i < prefix->blen && k + 1 < sizeof pfx; i++) {
      char c = TY_STR_DATA(prefix)[i];
      /* A slash in the prefix would turn the template into a different
         directory, and mkdtemp would then create it somewhere the caller did
         not ask for. */
      pfx[k++] = (c == '/') ? '_' : c;
    }
  }
  pfx[k] = 0;
  size_t n = strlen(base) + 1 + k + 6 + 1;
  char *tpl = (char *)malloc(n);
  if (!tpl) return NULL;
  snprintf(tpl, n, "%s/%sXXXXXX", base, pfx);
  /* The layer creates the directory and replaces the X's, and what makes that
     safe is the layer's business: mkdtemp is atomic here, and on Windows it is
     a create that fails rather than overwrites when another process chose the
     name first. */
  if (!typlat_mkdtemp(tpl)) {
    free(tpl);
    return NULL;
  }
  tystr *s = ty_str_new(tpl, (int64_t)strlen(tpl));
  free(tpl);
  return s;
}


/* ------------------------------------------------------------------- sha-1

   RFC 6455's handshake is SHA-1 of the client's key and a fixed string, so the
   web layer needs it. It is here rather than in the standard library because
   the library has no hashing and this is the one hash a protocol the library
   speaks requires; a program that wants another can write it in Teyru. */

typedef struct {
  uint32_t h[5];
  uint64_t bits;
  uint8_t buf[64];
  size_t n;
} ty_sha1_ctx;

static uint32_t sha1_rol(uint32_t v, int n) { return (v << n) | (v >> (32 - n)); }

static void sha1_block(ty_sha1_ctx *c, const uint8_t *p) {
  uint32_t w[80];
  for (int i = 0; i < 16; i++) {
    w[i] = ((uint32_t)p[i * 4] << 24) | ((uint32_t)p[i * 4 + 1] << 16) |
           ((uint32_t)p[i * 4 + 2] << 8) | (uint32_t)p[i * 4 + 3];
  }
  for (int i = 16; i < 80; i++) {
    w[i] = sha1_rol(w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16], 1);
  }
  uint32_t a = c->h[0], b = c->h[1], d = c->h[2], e = c->h[3], f = c->h[4];
  for (int i = 0; i < 80; i++) {
    uint32_t k, t;
    if (i < 20) {
      k = 0x5a827999u;
      t = (b & d) | ((~b) & e);
    } else if (i < 40) {
      k = 0x6ed9eba1u;
      t = b ^ d ^ e;
    } else if (i < 60) {
      k = 0x8f1bbcdcu;
      t = (b & d) | (b & e) | (d & e);
    } else {
      k = 0xca62c1d6u;
      t = b ^ d ^ e;
    }
    uint32_t tmp = sha1_rol(a, 5) + t + f + k + w[i];
    f = e;
    e = d;
    d = sha1_rol(b, 30);
    b = a;
    a = tmp;
  }
  c->h[0] += a;
  c->h[1] += b;
  c->h[2] += d;
  c->h[3] += e;
  c->h[4] += f;
}

void ty_sha1(const uint8_t *data, int64_t len, uint8_t out[20]) {
  ty_sha1_ctx c;
  c.h[0] = 0x67452301u;
  c.h[1] = 0xefcdab89u;
  c.h[2] = 0x98badcfeu;
  c.h[3] = 0x10325476u;
  c.h[4] = 0xc3d2e1f0u;
  c.bits = 0;
  c.n = 0;
  for (int64_t i = 0; i < len; i++) {
    c.buf[c.n++] = data[i];
    if (c.n == 64) {
      sha1_block(&c, c.buf);
      c.bits += 512;
      c.n = 0;
    }
  }
  /* the padding: a one bit, zeros, and the length in bits as a big-endian
     sixty-four bit number */
  uint64_t total = c.bits + (uint64_t)c.n * 8;
  c.buf[c.n++] = 0x80;
  if (c.n > 56) {
    while (c.n < 64) c.buf[c.n++] = 0;
    sha1_block(&c, c.buf);
    c.n = 0;
  }
  while (c.n < 56) c.buf[c.n++] = 0;
  for (int i = 7; i >= 0; i--) {
    c.buf[c.n++] = (uint8_t)((total >> (i * 8)) & 0xff);
  }
  sha1_block(&c, c.buf);
  for (int i = 0; i < 5; i++) {
    out[i * 4] = (uint8_t)(c.h[i] >> 24);
    out[i * 4 + 1] = (uint8_t)(c.h[i] >> 16);
    out[i * 4 + 2] = (uint8_t)(c.h[i] >> 8);
    out[i * 4 + 3] = (uint8_t)(c.h[i]);
  }
}

/* The digest as the twenty bytes of a Teyru array, which is what the handshake
   asks for: the caller base64s it. */
tyarr *ty_sha1_bytes(tyarr *data) {
  uint8_t *in = NULL;
  int64_t len = 0;
  if (data) {
    in = (uint8_t *)data->data;
    len = data->len;
  }
  tyarr *out = ty_alloc_arr(20, 1);
  out->refs = 0;
  ty_sha1(in, len, (uint8_t *)out->data);
  return out;
}

/* ------------------------------------------------------------ TLS, unlinked */

/* What the TLS entry points answer when the TLS layer was not compiled in.

   A build compiles tyrt_tls.c only when the program's reachable code calls one
   of these (internal/codegen/prune.go decides, and internal/driver links OpenSSL
   for exactly those programs), so that a program which never uses TLS needs
   neither the headers nor the library. What that decision cannot see is
   reflection: the generated C carries the body of every method, including the
   ones whose dispatch slots the pruner emptied, and those bodies still call
   these symbols. Whether such a program links at all then depends on the C
   compiler dropping functions nothing references -- clang and gcc -O1 and above
   do, gcc -O0 does not, and there the link ends in `undefined reference to
   ty_tls_kind`: a message naming a symbol the program's author never wrote, for
   a call their program cannot make.

   So each entry point has a weak definition here, in a file every build
   compiles. A program that does link the TLS layer gets the strong definition
   from tyrt_tls.c and never reaches these; a program that does not gets a
   named failure the first time reflection actually calls one, which is the
   behaviour the plan asks of an unlinked TLS layer, and is catchable rather
   than fatal because a program reaching a method by name is running its own
   code and may want to answer for it.

   The message names the Teyru method (the key in internal/codegen/native_net.go,
   which is the name a reader can search for) and the reason, and says nothing
   about OpenSSL being absent from the machine: the library may be installed and
   the program simply never needed it. */
static void tls_absent(const char *sym) __attribute__((noreturn));
static void tls_absent(const char *sym) {
  char msg[128];
  snprintf(msg, sizeof msg, "%s: this program was not linked against OpenSSL", sym);
  ty_throw(ty_make_ex(TY_UNSUP, msg));
}

__attribute__((weak)) int32_t ty_tls_client_context(tystr *ca_pem) {
  (void)ca_pem;
  tls_absent("Net.tlsClientContext0");
}
__attribute__((weak)) int32_t ty_tls_server_context(tystr *cert_pem, tystr *key_pem) {
  (void)cert_pem;
  (void)key_pem;
  tls_absent("Net.tlsServerContext0");
}
__attribute__((weak)) int32_t ty_tls_context_free(int32_t ctx) {
  (void)ctx;
  tls_absent("Net.tlsFreeContext0");
}
__attribute__((weak)) int32_t ty_tls_connect(int32_t ctx, int32_t fd, tystr *host) {
  (void)ctx;
  (void)fd;
  (void)host;
  tls_absent("Net.tlsConnect0");
}
__attribute__((weak)) int32_t ty_tls_accept(int32_t ctx, int32_t fd) {
  (void)ctx;
  (void)fd;
  tls_absent("Net.tlsAccept0");
}
__attribute__((weak)) int32_t ty_tls_read(int32_t session, tyarr *buf, int32_t off, int32_t len) {
  (void)session;
  (void)buf;
  (void)off;
  (void)len;
  tls_absent("Net.tlsRead0");
}
__attribute__((weak)) int32_t ty_tls_write_all(int32_t session, tyarr *buf, int32_t off, int32_t len) {
  (void)session;
  (void)buf;
  (void)off;
  (void)len;
  tls_absent("Net.tlsWrite0");
}
__attribute__((weak)) int32_t ty_tls_write_str(int32_t session, tystr *s) {
  (void)session;
  (void)s;
  tls_absent("Net.tlsWriteStr0");
}
__attribute__((weak)) int32_t ty_tls_close(int32_t session) {
  (void)session;
  tls_absent("Net.tlsClose0");
}
__attribute__((weak)) tystr *ty_tls_detail_text(void) { tls_absent("Net.tlsDetail0"); }
__attribute__((weak)) int32_t ty_tls_kind(int32_t code) {
  (void)code;
  tls_absent("Net.tlsKind0");
}
