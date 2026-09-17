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

# best_rss: runs the program $RUNS times and prints the smallest peak RSS in kB
# (/usr/bin/time -v is GNU time; without it the measurement is skipped)
best_rss() {
  [ -x /usr/bin/time ] || return 0
  i=0
  out=""
  while [ "$i" -lt "$RUNS" ]; do
    v=$(/usr/bin/time -v timeout "$LIMIT" "$@" 2>&1 >/dev/null | awk -F': ' '/Maximum resident set size/ {print $2}')
    if [ -n "$v" ]; then
      out="$out$v
"
    fi
    i=$((i+1))
  done
  printf '%s' "$out" | sort -g | head -1
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
      ratio=$(awk -v a="$j" -v b="$t" 'BEGIN{ if (b>0) printf "%.2fx", a/b; else print "-" }')
    fi
    if [ "$which" = long ]; then
      if [ -n "$scale" ]; then
        r=$(best_rss "$exe" "$scale")
      else
        r=$(best_rss "$exe")
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
size=$(wc -c < "$TMP/hello" | awk '{printf "%.1f", $1/1024}')
rss=$(best_rss "$TMP/hello")

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
    jrss=$(best_rss java -cp "$TMP" Hello)
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
printf '%-20s %10s %10s %10s\n' hello-size "${size}KB" - -
printf '%-20s %10s %10s %10s\n' peak-rss "${rss:-?}kB" "$jrss_cell" "$(fmt_ratio "$jrss" "$rss")"
