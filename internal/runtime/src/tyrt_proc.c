/* tyrt_proc.c - starting another program, and the pipes that talk to it.
 *
 * This file is the C half of java.lang.ProcessBuilder. The platform layer
 * (tyrt_plat.h) knows how to make a pipe, start a program with its three
 * standard descriptors on pipes, wait for it and kill it; what it does not know
 * is the Teyru representation of any of that, which is what this file is.
 *
 * The convention is tyrt_net.c's, for the same reason: a failure is -errno, a
 * success is a descriptor, a count or 0, and the Teyru half turns the integer
 * back into an exception. A function that has to answer both a descriptor and
 * an object cannot say either in its return value, so it writes into an int64[]
 * the caller passed -- the pattern ty_set_err in tyrt_net.c documents, and here
 * the array is what the runtime found rather than a reason.
 *
 * Every call here that waits -- a read of a pipe nobody is writing to, a
 * write, waitFor -- is a stop point for the collector, with the array on the
 * shadow stack for the whole of the wait. A child process that has not printed
 * anything yet must not stop this process's collections.
 *
 * Two things are deliberately absent. There is no shell: `sh -c` is what a
 * program writes when it wants one, and a layer that joined arguments into a
 * command line itself would be choosing a quoting convention for every caller.
 * And there is no environment variable surface of its own -- the array handed to
 * ty_proc_spawn is the whole environment, exactly as ProcessBuilder.environment
 * is, so a child that must not inherit a variable is expressed by not putting
 * it in the array.
 */
#include "tyrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* A long[] from the generated code, holding what a spawn found: pid, and the
   parent's ends of the child's stdin, stdout and stderr (-1 for one that was
   merged). */
static int ty_is_longs(tyarr *a) { return a && a->esize == 8; }

/* The bytes of a Teyru string as a C string, or NULL. A path, a host name or a
   program name whose bytes contain a NUL would name something else if it were
   truncated, so it is refused rather than cut. */
static const char *ty_proc_cstr(tystr *s) {
  if (!s) return NULL;
  if ((int64_t)strlen(TY_STR_DATA(s)) != s->blen) return NULL;
  return TY_STR_DATA(s);
}

/* A String[] as a NULL-terminated vector of C strings, allocated in one block
   so that a failure frees a single pointer. Answers NULL and sets *bad when an
   element is null or is not a String. */
static char **ty_proc_vector(tyarr *a, int *bad) {
  size_t n, total = 0, k = 0;
  char **out;
  *bad = 0;
  if (!a) return NULL;
  if (a->refs == 0 || a->esize != (int32_t)sizeof(void *)) {
    *bad = 1;
    return NULL;
  }
  n = (size_t)a->len;
  for (size_t i = 0; i < n; i++) {
    tystr *s = ((tystr **)a->data)[i];
    const char *c;
    if (!s) {
      *bad = 1;
      return NULL;
    }
    c = ty_proc_cstr(s);
    if (!c) {
      *bad = 1;
      return NULL;
    }
    total += strlen(c) + 1;
  }
  out = (char **)malloc((n + 1) * sizeof(char *) + total);
  if (!out) {
    *bad = 1;
    return NULL;
  }
  {
    char *blob = (char *)out + (n + 1) * sizeof(char *);
    for (size_t i = 0; i < n; i++) {
      tystr *s = ((tystr **)a->data)[i];
      const char *c = ty_proc_cstr(s);
      size_t len = strlen(c) + 1;
      out[i] = blob + k;
      memcpy(blob + k, c, len);
      k += len;
    }
    out[n] = NULL;
  }
  return out;
}

/* ty_proc_spawn starts argv[0] with argv. env is the child's whole environment,
   or null to inherit this one. out must be a long[] of at least four elements
   and receives pid, stdin, stdout and stderr. The answer is 0 or a negative
   code. */
int32_t ty_proc_spawn(tyarr *argv, tyarr *env, tystr *dir, int32_t merge, tyarr *out) {
  char **av, **ev = NULL;
  const char *d = NULL;
  typlat_proc p;
  int bad = 0, e;
  int64_t *slot;
  if (!ty_is_longs(out)) return -EINVAL;
  if (out->len < 4) return -EINVAL;
  if (!argv || argv->len == 0) return -EINVAL;
  av = ty_proc_vector(argv, &bad);
  if (!av) return bad ? -EINVAL : -ENOMEM;
  if (env) {
    ev = ty_proc_vector(env, &bad);
    if (!ev) {
      free(av);
      return bad ? -EINVAL : -ENOMEM;
    }
  }
  if (dir && dir->blen > 0) {
    d = ty_proc_cstr(dir);
    if (!d) {
      free(av);
      free(ev);
      return -EINVAL;
    }
  }
  /* The vector is a root for the whole of the spawn: the child reads it, and
     the platform layer copies what it needs before this returns. */
  TY_ROOT_PUSH(argv);
  if (env) TY_ROOT_PUSH(env);
  e = typlat_proc_spawn((const char *const *)av, (char *const *)ev, d, merge != 0, &p);
  TY_ROOT_POP();
  if (env) TY_ROOT_POP();
  free(av);
  free(ev);
  if (e < 0) return e;
  slot = (int64_t *)out->data;
  slot[0] = p.pid;
  slot[1] = p.in;
  slot[2] = p.out;
  slot[3] = p.err;
  return 0;
}

int32_t ty_proc_wait(int64_t pid, tyarr *status) {
  int32_t st = 0;
  int e;
  if (pid <= 0) return -EINVAL;
  if (!status || !status->esize || status->esize != 4 || status->len < 1) return -EINVAL;
  /* Waiting for another process is waiting: a collection must not wait for it,
     and the child may run for as long as it likes. */
  ty_thread_stopped_begin();
  e = typlat_proc_wait(pid, &st);
  ty_thread_stopped_end();
  if (e < 0) return e;
  ((int32_t *)status->data)[0] = st;
  return 0;
}

/* 1 and *status when the child has ended, 0 when it has not, a negative code
   on failure -- the answer Process.exitValue needs without blocking. */
int32_t ty_proc_poll(int64_t pid, tyarr *status) {
  int32_t st = 0;
  int e;
  if (pid <= 0) return -EINVAL;
  if (!status || status->esize != 4 || status->len < 1) return -EINVAL;
  e = typlat_proc_poll(pid, &st);
  if (e < 0) return e;
  if (e == 0) return 0;
  ((int32_t *)status->data)[0] = st;
  return 1;
}

int32_t ty_proc_kill(int64_t pid) {
  if (pid <= 0) return -EINVAL;
  return typlat_proc_kill(pid);
}

/* A read of a pipe, with the tyrt_net.c contract: the count read (0 at end of
   stream, short is an answer), or a negative code. */
int32_t ty_proc_read(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  char *p;
  int64_t n;
  if (fd < 0) return -EINVAL;
  if (!buf || buf->esize != 1) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  if (len == 0) return 0;
  for (;;) {
    p = buf->data + off;
    TY_ROOT_PUSH(buf);
    ty_thread_stopped_begin();
    n = typlat_fd_read(fd, p, len);
    ty_thread_stopped_end();
    TY_ROOT_POP();
    if (n == -EINTR) continue;
    return (int32_t)n;
  }
}

/* A write of a pipe, looped until the last byte is out: a short write reported
   as success is a truncated program's output. */
int32_t ty_proc_write(int32_t fd, tyarr *buf, int32_t off, int32_t len) {
  const char *p;
  int32_t done = 0;
  if (fd < 0) return -EINVAL;
  if (!buf || buf->esize != 1) return -EINVAL;
  if (off < 0 || len < 0 || (int64_t)off + len > buf->len) return -EINVAL;
  p = buf->data + off;
  while (done < len) {
    int64_t n;
    TY_ROOT_PUSH(buf);
    ty_thread_stopped_begin();
    n = typlat_fd_write(fd, p + done, len - done);
    ty_thread_stopped_end();
    TY_ROOT_POP();
    if (n == -EINTR) continue;
    if (n < 0) return (int32_t)n;
    if (n == 0) return done > 0 ? done : -EIO;
    done += (int32_t)n;
  }
  return done;
}

/* The same, for a String's bytes. */
int32_t ty_proc_write_str(int32_t fd, tystr *s) {
  int32_t done = 0;
  const char *p;
  int32_t len;
  if (fd < 0) return -EINVAL;
  if (!s) return -EINVAL;
  p = TY_STR_DATA(s);
  len = (int32_t)s->blen;
  while (done < len) {
    int64_t n;
    TY_ROOT_PUSH(s);
    ty_thread_stopped_begin();
    n = typlat_fd_write(fd, p + done, len - done);
    ty_thread_stopped_end();
    TY_ROOT_POP();
    if (n == -EINTR) continue;
    if (n < 0) return (int32_t)n;
    if (n == 0) return done > 0 ? done : -EIO;
    done += (int32_t)n;
  }
  return done;
}

int32_t ty_proc_close(int32_t fd) {
  if (fd < 0) return 0;
  typlat_close_fd(fd);
  return 0;
}

/* The text for a code, the same shape as ty_net_strerror: the layer's message
   and the number, because the text is localized and two failures can share a
   description while the number cannot. */
tystr *ty_proc_strerror(int32_t code) {
  char buf[256];
  if (code >= 0) {
    snprintf(buf, sizeof buf, "no error");
  } else {
    typlat_error_text(-code, buf, sizeof buf);
  }
  return ty_str_new(buf, (int64_t)strlen(buf));
}
