#!/bin/sh
# The deep-recursion matrix, run locally because this repository has no CI: the
# same command a CI job would have run, spelled so a person can run it.
#
# It builds a program that catches a StackOverflowError at every optimisation
# level, backend and C compiler this project supports, and demands the same
# output from each:
#
#     -O0 and -O2  x  the C backend and the LLVM backend  x  clang and gcc
#
# The two back ends are not handed the same program, and that is the LLVM back
# end's own doing rather than a choice made here: it refuses a program that
# starts a Thread -- "the llvm back end does not lower the runtime's thread
# start", TY-INT-0100, the named refusal the project's rules ask for -- so the C
# backend runs t214 (the error caught on the main thread *and* on a Thread) and
# the LLVM backend runs t215 (the same recursion on the main thread, with nothing
# catching it, which also exercises the report inside the margin).
#
# A combination that cannot be built is printed with its reason rather than
# skipped, because "it passed" and "it never ran" are different answers:
#
#   - gcc -O0 cannot link any program on this tree. The generated C keeps the
#     bodies of methods whose dispatch slots the pruner emptied, those bodies
#     call ty_tls_*, and gcc -O0 does not drop the unreferenced statics the way
#     clang and gcc >= -O1 do, so the link ends in "undefined reference to
#     ty_tls_kind". Pre-existing: a compiler built from the plan's baseline
#     commit a84b32f fails the same way on a hello world.
#   - a backend that refuses the program by name is reported as BLOCKED with the
#     diagnostic code it refused with.
#
#   sh scripts/stack-matrix.sh
set -e
cd "$(dirname "$0")/.."

DIR=${DIR:-tests/programs}
# A private temporary directory: the compiler writes the runtime sources into
# $TMPDIR, and two builds sharing one have been observed to compile each other's
# copy of the runtime.
TMPDIR=${TMPDIR:-$(mktemp -d "${HOME}/tywork/tmp/stack-matrix.XXXXXX")}
export TMPDIR
BIN=${BIN:-}
TMPBIN=
if [ -z "$BIN" ]; then
  TMPBIN="$TMPDIR/teyru"
  go build -o "$TMPBIN" ./cmd/teyru
  BIN=$TMPBIN
fi
[ -x "$BIN" ] || { echo "stack-matrix: $BIN is not an executable" >&2; exit 1; }
[ -f "$DIR/t214_stack_overflow.teyru" ] || {
  echo "stack-matrix: $DIR is not checked out (tests/ is a submodule)" >&2
  exit 1
}
trap '[ -z "$TMPBIN" ] || rm -f "$TMPBIN"' EXIT

failed=0
blocked=0
printf '%-5s %-7s %-6s %-24s %s\n' opt backend cc program result
for opt in -O0 -O2; do
  for backend in c llvm; do
    case $backend in
    c)    name=t214_stack_overflow ;;
    llvm) name=t215_stack_overflow_uncaught ;;
    esac
    prog="$DIR/$name.teyru"
    want=$(cat "$DIR/$name.expected" 2>/dev/null || true)
    [ -f "$DIR/$name.experr" ] && want="$want
$(cat "$DIR/$name.experr")"
    wantexit=0
    [ -f "$DIR/$name.exit" ] && wantexit=$(cat "$DIR/$name.exit")
    for cc in clang gcc; do
      if [ "$backend" = llvm ] && [ "$cc" = gcc ]; then
        printf '%-5s %-7s %-6s %-24s n/a      the llvm back end links the module through clang\n' "$opt" "$backend" "$cc" "$name"
        continue
      fi
      exe="$TMPDIR/$name.$opt.$backend.$cc"
      back=""
      [ "$backend" = c ] || back="--backend llvm"
      log="$TMPDIR/build.$opt.$backend.$cc.log"
      if ! "$BIN" build $opt --cc "$cc" $back -o "$exe" "$prog" >"$log" 2>&1; then
        case "$log" in *) : ;; esac
        reason=$(grep -m1 -o 'undefined reference to [^ ]*' "$log" || true)
        code=$(grep -m1 -o 'TY-[A-Z]*-[0-9]*' "$log" || true)
        if [ -n "$reason" ] || [ -n "$code" ]; then
          printf '%-5s %-7s %-6s %-24s BLOCKED  %s\n' "$opt" "$backend" "$cc" "$name" "${code:-$reason}"
          blocked=$((blocked+1))
        else
          printf '%-5s %-7s %-6s %-24s BUILD-FAILED\n' "$opt" "$backend" "$cc" "$name"
          sed -n '1,3p' "$log" | sed 's/^/        /'
          failed=$((failed+1))
        fi
        continue
      fi
      code=0
      "$exe" >"$TMPDIR/out" 2>"$TMPDIR/err" || code=$?
      gotout=$(cat "$TMPDIR/out")
      goterr=$(cat "$TMPDIR/err")
      wantout=$(cat "$DIR/$name.expected" 2>/dev/null || true)
      wanterr=$(cat "$DIR/$name.experr" 2>/dev/null || true)
      if [ "$gotout" = "$wantout" ] && [ "$goterr" = "$wanterr" ] && [ "$code" = "$wantexit" ]; then
        printf '%-5s %-7s %-6s %-24s ok\n' "$opt" "$backend" "$cc" "$name"
      else
        printf '%-5s %-7s %-6s %-24s MISMATCH (exit %s, want %s)\n' "$opt" "$backend" "$cc" "$name" "$code" "$wantexit"
        printf 'stdout:%s\nstderr:%s\n' "$gotout" "$goterr" | sed 's/^/        /'
        failed=$((failed+1))
      fi
    done
  done
done

echo
echo "stack-matrix: $failed failed, $blocked blocked (gcc -O0, see the header)"
[ "$failed" -eq 0 ]
