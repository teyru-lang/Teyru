#!/bin/sh
# Compiles and times the benchmark programs, optionally against a JVM baseline.
# Prints two tables of programs -- a short scale and a long scale -- and then the
# three rows of the README table that are not programs: startup (100 runs of a
# hello world), executable size, and peak RSS.
#
#   sh scripts/bench.sh            # both tables, 3 runs each, best time
#   JAVA=0 sh scripts/bench.sh     # skip the JVM comparison
#   RUNS=1 sh scripts/bench.sh     # single run
#   SHORT_ONLY=1 sh scripts/bench.sh
#   LONG_ONLY=1 sh scripts/bench.sh
#   LIMIT=120 sh scripts/bench.sh  # seconds a single run may take
#
# Two tables because one number per program cannot answer both questions. A few
# tens of milliseconds is mostly the JVM starting, so the short table is a
# statement about startup and about small programs -- it is what the README's
# "several times faster" rows were once based on, and reading it as throughput is
# the mistake this script now makes visible. The long table asks the same
# programs the same question with a scale that keeps them running for about a
# second, where the JVM has warmed up and the answer is about the program.
#
# The scale is a command-line argument the program reads, and the *same* number
# is given to both sides, so a row cannot compare two different amounts of work.
# The short scales are the programs' own defaults; the long ones are in scale_of
# below, chosen so that a run takes roughly a second on a quiet machine -- the
# times printed are the evidence for whether that is what happened, and a row
# whose time is far from a second should have its scale adjusted rather than be
# believed.
#
# Measure on a quiet machine. Every number here is a wall clock, so a compiler
# build or another agent working in the background shows up as a slower program,
# and the "best of RUNS" in the short table is the most sensitive to it.
# Since "measure on a quiet machine" is a request nobody can verify after the
# fact, the run prints the host, the tree and the load average it saw on both
# sides of the tables: a table pasted into a document without them is a table
# nobody can tell apart from one taken on a loaded box. The load line on the way
# out includes this script's own compiler work, which is why the line on the way
# in is the one that says whether the box was quiet.
#
# A ratio is printed as !(out) rather than a number when the two sides printed
# different answers: those two runs are not the same program, so the ratio would
# not be a comparison of two implementations. The answer is the last line each
# side prints, which is where every benchmark here puts its result, so the check
# separates "the two sides did different work" from "the two sides reported
# different stopwatch readings" -- bench_invoke prints two timings of its own
# before the line that is its answer. bench_string_cjk is the row that prints
# !(out) today, and it stops being one when the string semantics land and its
# answers agree.
set -e
cd "$(dirname "$0")/.."
# The compiler is built from the tree being measured, unless BIN names one to
# use instead (that is how an older commit's numbers are reproduced). It used
# to fall back to ./teyru when that file happened to exist, so a stale binary
# left in the working directory silently produced the whole table: the
# published size row came from a build of a completely different source tree.
BIN=${BIN:-}
BIN_TEMP=
if [ -z "$BIN" ]; then
  BIN=$(mktemp "${TMPDIR:-/tmp}/teyru-bench.XXXXXX")
  BIN_TEMP=$BIN
  go build -o "$BIN" ./cmd/teyru
fi
[ -x "$BIN" ] || { echo "bench: $BIN is not an executable" >&2; exit 1; }
RUNS=${RUNS:-3}
JAVA=${JAVA:-1}
LIMIT=${LIMIT:-3600}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; [ -z "$BIN_TEMP" ] || rm -f "$BIN_TEMP"' EXIT

# loadavg: the three load averages, or "?" where there is no /proc
loadavg() {
  if [ -r /proc/loadavg ]; then
    cut -d' ' -f1-3 /proc/loadavg
  else
    printf '?'
  fi
}

# the tree the numbers came from, so a pasted table names its own build: the
# compiler is built from the working tree, and a working tree is not a version
tree=$(git rev-parse --short HEAD 2>/dev/null || printf '?')
subject=$(git log -1 --format=%s 2>/dev/null || printf '?')
dirty=""
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
  dirty=" + uncommitted changes"
fi
host=$(uname -n 2>/dev/null || printf '?')
arch=$(uname -m 2>/dev/null || printf '?')
cpus=$( (nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || printf '?') | tr -d '\n')
if [ -n "$BIN_TEMP" ]; then
  binsrc="built from this tree"
else
  binsrc="BIN=$BIN"
fi

printf '# %s %s, %s cpus\n' "$host" "$arch" "$cpus"
printf '# tree %s (%s)%s, compiler %s\n' "$tree" "$subject" "$dirty" "$binsrc"
printf '# RUNS=%s JAVA=%s -O2, whole-program wall clock including process start\n' "$RUNS" "$JAVA"
printf '# load average (1m 5m 15m) entering the tables: %s\n' "$(loadavg)"

# best: runs the command $RUNS times and prints the smallest wall time
best() {
  i=0
  out=""
  while [ "$i" -lt "$RUNS" ]; do
    start=$(date +%s.%N)
    timeout "$LIMIT" "$@" >/dev/null 2>&1 || true
    end=$(date +%s.%N)
    out="$out$(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.4f\n", b-a}')
"
    i=$((i+1))
  done
  printf '%s' "$out" | sort -g | head -1
}

# fmt_ratio: prints a/b, or "-" when either side was not measured
fmt_ratio() {
  if [ -n "$1" ] && [ -n "$2" ]; then
    awk -v a="$1" -v b="$2" 'BEGIN{ if (b>0) printf "%.2fx", a/b; else print "-" }'
  else
    printf '-'
  fi
}

# answer_of: the last line the command prints. Every benchmark here prints its
# answer last -- a sum, a length, or bench_invoke's equality flag -- so the two
# sides' last lines are what "the same work" means, and a row whose answers differ
# has its ratio replaced by !(out) instead of printed. The same scale argument is
# not a guarantee of the same work: bench_string_cjk reads the same argument and,
# today, counts bytes on one side and characters on the other.
#
# Comparing the last line rather than all of them is deliberate: line-set
# comparison was the first version, and it flipped bench_invoke to !(out) whenever
# its two stopwatch lines happened to read the same in both of that side's runs --
# a check that is right by luck is worse than no check, because the row it broke is
# the one row that loses and whose factor the README quotes.
answer_of() {
  timeout "$LIMIT" "$@" 2>/dev/null | awk 'NF { last = $0 } END { print last }' || true
}

# worst_rss: runs the program $RUNS times and prints the largest peak RSS in kB
# (/usr/bin/time -v is GNU time; without it the measurement is skipped).
#
# Largest, not smallest. The timing columns are the smallest of RUNS because a
# slow run is a run that was interfered with; a *small* memory reading is not the
# same kind of evidence -- the same hello world measures 2,116 kB on about one run
# in twenty and 4,200-4,450 kB on the rest, and the small reading means the run did
# not fault its heap in rather than that the program needs less. A row whose
# purpose is the program's footprint should therefore report the worst run, and
# five runs is enough to land on the mode: the largest of five sits at 4,400-4,450
# kB, while the smallest of five is the one that occasionally reads 2,116.
#
# The program is the direct child of /usr/bin/time, and the timeout wraps both:
# `timeout LIMIT /usr/bin/time -v prog` reports prog's peak, while
# `/usr/bin/time -v timeout LIMIT prog` reports the wrapper's, and the wrapper's
# is a floor of about 9.9 MB rather than an offset -- `timeout 5 /bin/true` is
# 9,892 kB against 1,424 kB for /bin/true, the same hello is 9,904 against 4,172,
# and a Java hello that really does use 51 MB is unaffected. That floor is what
# made this row say "5.1x less memory" for a program that differs by 12x, and it
# also floors every light row of the long table.
worst_rss() {
  [ -x /usr/bin/time ] || return 0
  i=0
  out=""
  while [ "$i" -lt "$RUNS" ]; do
    v=$(timeout "$LIMIT" /usr/bin/time -v "$@" 2>&1 >/dev/null | awk -F': ' '/Maximum resident set size/ {print $2}')
    if [ -n "$v" ]; then
      out="$out$v
"
    fi
    i=$((i+1))
  done
  printf '%s' "$out" | sort -g | tail -1
}

# scale_of: the argument a benchmark gets at each scale. A program that does not
# appear here is run without one.
scale_of() {
  case "$1" in
    bench_fib)        if [ "$2" = long ]; then echo 44;        else echo 32;        fi ;;
    bench_loop)       if [ "$2" = long ]; then echo 1000000;   else echo 20000;     fi ;;
    bench_oop)        if [ "$2" = long ]; then echo 500000;    else echo 2000;      fi ;;
    bench_alloc)      if [ "$2" = long ]; then echo 800000000; else echo 20000000;  fi ;;
    bench_string)     if [ "$2" = long ]; then echo 16000000;  else echo 200000;    fi ;;
    bench_string_cjk) if [ "$2" = long ]; then echo 4000000;   else echo 200000;    fi ;;
    bench_invoke)     if [ "$2" = long ]; then echo 200000000; else echo 20000000;  fi ;;
    *) echo "" ;;
  esac
}

# table: one scale's worth of rows. The long table also shows peak RSS, because
# the long runs are where an allocation difference becomes a memory difference.
table() {
  which=$1
  if [ "$which" = long ]; then
    printf '\n%-20s %10s %10s %10s %12s\n' program teyru java teyru-java rss
  else
    printf '%-20s %10s %10s %10s\n' program teyru java teyru-java
  fi
  for f in examples/bench_*.teyru; do
    [ -f "$f" ] || continue
    name=$(basename "$f" .teyru)
    scale=$(scale_of "$name" "$which")
    exe="$TMP/$name.teyru.exe"
    if ! $BIN build -O2 -o "$exe" "$f" >"$TMP/$name.build.log" 2>&1; then
      printf '%-20s %10s\n' "$name" "!build"
      continue
    fi
    if [ -n "$scale" ]; then
      t=$(best "$exe" "$scale")
    else
      t=$(best "$exe")
    fi
    j=""
    missing=""
    if [ "$JAVA" = "1" ] && command -v java >/dev/null 2>&1 && [ -f "examples/$name.java" ]; then
      if javac -d "$TMP" "examples/$name.java" 2>/dev/null && [ -f "$TMP/$name.class" ]; then
        if [ -n "$scale" ]; then
          j=$(best java -cp "$TMP" "$name" "$scale")
        else
          j=$(best java -cp "$TMP" "$name")
        fi
      else
        # A Java file that did not produce a class named after the file: the row
        # would otherwise print "-", which reads as "not measured rather than
        # measured", and a benchmark that loses is exactly the one whose column
        # must not disappear. Say so instead.
        missing="!no-class"
      fi
    fi
    if [ -n "$missing" ]; then
      printf '%-20s %9ss %9s %10s\n' "$name" "$t" "$missing" "$missing"
      continue
    fi
    ratio="-"
    if [ -n "$j" ]; then
      if [ -n "$scale" ]; then
        to=$(answer_of "$exe" "$scale")
        jo=$(answer_of java -cp "$TMP" "$name" "$scale")
      else
        to=$(answer_of "$exe")
        jo=$(answer_of java -cp "$TMP" "$name")
      fi
      if [ "$to" != "$jo" ]; then
        ratio="!(out)"
      else
        ratio=$(awk -v a="$j" -v b="$t" 'BEGIN{ if (b>0) printf "%.2fx", a/b; else print "-" }')
      fi
    fi
    if [ "$which" = long ]; then
      if [ -n "$scale" ]; then
        r=$(worst_rss "$exe" "$scale")
      else
        r=$(worst_rss "$exe")
      fi
      printf '%-20s %9ss %9ss %10s %10skB\n' "$name" "$t" "${j:--}" "$ratio" "${r:-?}"
    else
      printf '%-20s %9ss %9ss %10s\n' "$name" "$t" "${j:--}" "$ratio"
    fi
  done
}

if [ "${LONG_ONLY:-0}" != "1" ]; then
  table short
fi
if [ "${SHORT_ONLY:-0}" != "1" ]; then
  table long
fi

# The three non-program rows of the README table. The JVM side of the size row
# is the installed JDK runtime, which is a property of the measuring machine
# rather than of the program, so it is not measured here.
cat > "$TMP/hello.teyru" <<'EOF'
class Hello {
  public static void main(String[] args) {
    System.out.println("Hello, Teyru!")
  }
}
EOF

# run100 runs the program it is given 100 times, so `best` returns the best of
# $RUNS batches of 100 runs (a single process start is too short to time).
# This row is the one the README quotes, so it prints the byte count rather than
# a rounded KB: the number moves with almost every landing, and a rounded one
# cannot be compared against the previous measurement.
cat > "$TMP/run100.sh" <<'EOF'
#!/bin/sh
i=0
while [ "$i" -lt 100 ]; do
  "$@" >/dev/null 2>&1
  i=$((i+1))
done
EOF
chmod +x "$TMP/run100.sh"

$BIN build -O2 -o "$TMP/hello" "$TMP/hello.teyru" >/dev/null
startup=$(best "$TMP/run100.sh" "$TMP/hello")
size=$(wc -c < "$TMP/hello" | tr -d ' ')
rss=$(worst_rss "$TMP/hello")

jstartup=""
jrss=""
if [ "$JAVA" = "1" ] && command -v java >/dev/null 2>&1; then
  cat > "$TMP/Hello.java" <<'EOF'
public class Hello {
  public static void main(String[] args) {
    System.out.println("Hello, Teyru!");
  }
}
EOF
  if javac -d "$TMP" "$TMP/Hello.java" 2>/dev/null; then
    jstartup=$(best "$TMP/run100.sh" java -cp "$TMP" Hello)
    jrss=$(worst_rss java -cp "$TMP" Hello)
  fi
fi

jstartup_cell="-"
if [ -n "$jstartup" ]; then
  jstartup_cell="${jstartup}s"
fi
jrss_cell="-"
if [ -n "$jrss" ]; then
  jrss_cell="${jrss}kB"
fi

printf '\n'
printf '%-20s %10s %10s %10s\n' metric teyru java teyru-java
printf '%-20s %10s %10s %10s\n' startup-100x "${startup}s" "$jstartup_cell" "$(fmt_ratio "$jstartup" "$startup")"
printf '%-20s %10s %10s %10s\n' hello-size "${size}B" - -
printf '%-20s %10s %10s %10s\n' peak-rss "${rss:-?}kB" "$jrss_cell" "$(fmt_ratio "$jrss" "$rss")"

# The load on the way out, beside the line on the way in: this one includes the
# compiles this script just did, so when the two disagree it is the entering line
# that says whether the box was quiet while the programs were timed.
printf '# load average (1m 5m 15m) leaving the tables: %s\n' "$(loadavg)"
