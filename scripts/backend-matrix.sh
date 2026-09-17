#!/bin/sh
# The back-end matrix: every program in tests/programs, built and run in six
# cells, compared with the suite's expectation for it and with each other.
#
#   { C back end + clang, C back end + gcc, LLVM back end } x { -O0, -O2 }
#
# This is W8's test matrix (fix plan §W8 item 1). The repository has no CI: the
# one workflow runs when a release is published, and it calls this through
# `make backend-matrix` like every other check, so the matrix is written to be
# run by a person as well as by the workflow.
#
#   sh scripts/backend-matrix.sh                  # every program, every cell
#   ONLY='t19[0-9]_' sh scripts/backend-matrix.sh # programs whose name matches
#   CELLS='O2-clang O2-llvm' sh scripts/backend-matrix.sh
#   JOBS=8 sh scripts/backend-matrix.sh           # programs built at a time
#   OUT=$HOME/tywork/matrix-2026-09-17 sh scripts/backend-matrix.sh
#   BIN=/path/to/teyru sh scripts/backend-matrix.sh  # instead of this tree's
#   STRICT=1 sh scripts/backend-matrix.sh         # see "what it answers" below
#
# What it answers, and what it does not
# -------------------------------------
# Two different questions, kept apart in the report, because the answer to one
# is a back-end bug and the answer to the other is somebody else's:
#
#   * do the cells agree with each other? A program whose answer depends on
#     which back end compiled it is W8's question, and the report lists every
#     one of them with the cells, the bytes and the construct.
#   * does each cell agree with tests/programs/<name>.expected (and .exit and
#     .experr)? A program that is wrong the same way in every cell is one
#     defect, not two back ends disagreeing; the suite's own drivers (go test
#     and tests/run.sh) already report it, and this matrix counts it so that a
#     divergence can be told from it. It does not fail the run by default
#     (STRICT=1 makes it fail, for when the matrix is the only thing running).
#
# A build that ends in a diagnostic the driver named -- a TY- code, such as the
# LLVM back end's TY-INT-0100 for a feature it does not lower -- is a named
# limit rather than a miscompile: it is counted as `refused` for that cell and
# listed. A build that fails any other way is a finding.
#
# The record
# ----------
# One line per program and cell in <out>/results.tsv:
#
#   prog cell state exit want stdout stderr note outsum errsum
#
# with the compiler's own output in runs/<prog>.<cell>.log and the program's
# stdout and stderr in runs/<prog>.<cell>.out and .err, so a divergence can be
# read rather than guessed at. The report is <out>/report.txt and is printed.
# Nothing is deleted from <out> except what a previous run of this script put
# there, so two runs can be compared by keeping the older directory.
#
# A divergence is accounted for in scripts/backend-matrix-allow.txt, one
# `<program> <work item> <reason>` per line, the way tests/known-failures.txt
# accounts for a failing case: a listed program that stops diverging fails the
# run, so an entry cannot outlive the divergence it describes.
set -u
cd "$(dirname "$0")/.."
SELF=$(pwd)/scripts/backend-matrix.sh

DIR=${DIR:-tests/programs}
CELLS=${CELLS:-O0-clang O0-gcc O0-llvm O2-clang O2-gcc O2-llvm}
JOBS=${JOBS:-4}
ONLY=${ONLY:-}
BUILD_TIMEOUT=${BUILD_TIMEOUT:-900}
RUN_TIMEOUT=${RUN_TIMEOUT:-120}
KEEP=${KEEP:-0}
STRICT=${STRICT:-0}
ALLOW=${ALLOW:-scripts/backend-matrix-allow.txt}
KNOWN=${KNOWN:-tests/known-failures.txt}

# A private temporary directory, because the compiler writes the generated
# runtime sources into $TMPDIR and two builds sharing one have been observed to
# compile each other's copy of the runtime. It is set once here and passed down
# to every build.
MATRIX_TMP_ROOT=${MATRIX_TMP_ROOT:-${HOME:-/tmp}/tywork/tmp}
mkdir -p "$MATRIX_TMP_ROOT" 2>/dev/null || MATRIX_TMP_ROOT=${TMPDIR:-/tmp}
TMPDIR=${TMPDIR:-$(mktemp -d "$MATRIX_TMP_ROOT/backend-matrix.XXXXXX")}
export TMPDIR

# ---------------------------------------------------------------- one cell
#
# The child mode: one cell of one program. It is a separate run of this script
# so that the cells can be run in parallel with nothing but xargs, and it is
# handed what it needs in the environment. It always exits 0: what a program
# did to itself (a crash, a timeout, a build failure) is a result to record,
# not an error in the matrix. A cell that recorded nothing is the error, and
# the parent notices it.
if [ -n "${MATRIX_CHILD:-}" ]; then
  prog=$MATRIX_PROG
  case $MATRIX_CELL in
    O0-clang) opt=-O0; backend=c;    cc=clang ;;
    O0-gcc)   opt=-O0; backend=c;    cc=gcc ;;
    O2-clang) opt=-O2; backend=c;    cc=clang ;;
    O2-gcc)   opt=-O2; backend=c;    cc=gcc ;;
    O0-llvm)  opt=-O0; backend=llvm; cc= ;;
    O2-llvm)  opt=-O2; backend=llvm; cc= ;;
    *) echo "backend-matrix: unknown cell $MATRIX_CELL" >&2; exit 2 ;;
  esac
  runs=$MATRIX_W/runs
  p=$runs/$prog.$MATRIX_CELL
  exe=$MATRIX_W/exe/$prog.$MATRIX_CELL
  ccarg=""
  [ -z "$cc" ] || ccarg="--cc $cc"

  # shellcheck disable=SC2086
  timeout -k 5 "$MATRIX_BUILD_TIMEOUT" \
    "$MATRIX_BIN" build "$opt" --backend "$backend" $ccarg -o "$exe" \
    "$MATRIX_DIR/$prog.teyru" >"$p.log" 2>&1
  bstat=$?

  if [ "$bstat" -ne 0 ]; then
    # A named refusal is the driver saying which feature it cannot lower, and
    # it is reported by that name rather than as an unknown build failure.
    note=$(sed -n 's/.*\(TY-[A-Z][A-Z]*-[0-9][0-9]*\).*/\1/p' "$p.log" | head -1)
    if [ -z "$note" ]; then
      if [ "$bstat" -eq 124 ] || [ "$bstat" -eq 137 ]; then
        note="build timeout"
      else
        note=$(sed -n '1p' "$p.log" | cut -c1-120)
        [ -n "$note" ] || note="build failed with status $bstat"
      fi
    fi
    printf '%s\t%s\trefused\t-\t-\t-\t-\t%s\t-\t-\n' \
      "$prog" "$MATRIX_CELL" "$note" >"$p.tsv"
    exit 0
  fi

  args=""
  [ -f "$MATRIX_DIR/$prog.args" ] && args=$(cat "$MATRIX_DIR/$prog.args")
  # shellcheck disable=SC2086
  timeout -k 5 "$MATRIX_RUN_TIMEOUT" "$exe" $args >"$p.out" 2>"$p.err"
  rstat=$?

  want=0
  [ -f "$MATRIX_DIR/$prog.exit" ] && want=$(cat "$MATRIX_DIR/$prog.exit")

  stdout=diff
  cmp -s "$MATRIX_DIR/$prog.expected" "$p.out" && stdout=ok
  if [ -f "$MATRIX_DIR/$prog.experr" ]; then
    stderr=diff
    cmp -s "$MATRIX_DIR/$prog.experr" "$p.err" && stderr=ok
  elif [ -s "$p.err" ]; then
    # tests/run.sh ignores stderr when there is no .experr and the harness in
    # the compiler repository fails on it: the matrix records which happened
    # rather than pick a side, and a cell that differs from another cell here
    # is reported like any other difference.
    stderr=extra
  else
    stderr=none
  fi

  if [ "$rstat" -eq 124 ] || [ "$rstat" -eq 137 ]; then
    state=run-timeout
  elif [ "$rstat" -ge 128 ]; then
    state=crash
  else
    state=ran
  fi

  note=-
  [ "$state" = crash ] && note="signal $((rstat - 128))"
  [ "$state" = run-timeout ] && note="run timeout"
  outsum=$(cksum <"$p.out" | tr -s ' ' ':')
  errsum=$(cksum <"$p.err" | tr -s ' ' ':')
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$prog" "$MATRIX_CELL" "$state" "$rstat" "$want" "$stdout" "$stderr" \
    "$note" "$outsum" "$errsum" >"$p.tsv"

  [ "$MATRIX_KEEP" = "1" ] || rm -f "$exe"
  exit 0
fi

# ---------------------------------------------------------------- the parent
[ -d "$DIR" ] || { echo "backend-matrix: no $DIR" >&2; exit 2; }

if [ -n "${OUT:-}" ]; then
  W=$OUT
  mkdir -p "$W" || exit 2
  if [ -e "$W/.backend-matrix" ]; then
    rm -rf "$W/runs" "$W/exe" "$W/results.tsv" "$W/report.txt" "$W/divergent.tsv"
  elif [ -n "$(ls -A "$W" 2>/dev/null)" ]; then
    echo "backend-matrix: $W is not empty and was not written by this script" >&2
    echo "                (point OUT= at a new directory, or remove it yourself)" >&2
    exit 2
  fi
else
  W=$(mktemp -d "$MATRIX_TMP_ROOT/backend-matrix-out.XXXXXX")
fi
: >"$W/.backend-matrix"
mkdir -p "$W/runs" "$W/exe"

# A missing account file means nothing is accounted for, which is the safe
# reading; awk is handed an empty file so that it does not warn about this one.
if [ ! -f "$ALLOW" ]; then
  echo "backend-matrix: no $ALLOW -- no divergence is accounted for"
  ALLOW=$W/allow-empty.txt
  : >"$ALLOW"
fi

BIN=${BIN:-}
BIN_TEMP=
if [ -z "$BIN" ]; then
  # Built from the tree being run, into the record's directory, so that the
  # cells are this checkout's cells and not those of a binary that happened to
  # be lying around (the same paragraph as in bench.sh).
  BIN=$W/teyru
  BIN_TEMP=$BIN
  go build -trimpath -o "$BIN" ./cmd/teyru || exit 2
fi
[ -x "$BIN" ] && [ ! -d "$BIN" ] || { echo "backend-matrix: $BIN is not an executable" >&2; exit 2; }

programs=""
for src in "$DIR"/*.teyru; do
  [ -f "$src" ] || continue
  name=$(basename "$src" .teyru)
  if [ -n "$ONLY" ]; then
    printf '%s\n' "$name" | grep -Eq -- "$ONLY" || continue
  fi
  programs="$programs $name"
done
[ -n "$programs" ] || { echo "backend-matrix: no programs under $DIR" >&2; exit 2; }
count=$(printf '%s\n' $programs | wc -l | tr -d ' ')

: >"$W/tasks.txt"
for prog in $programs; do
  for cell in $CELLS; do
    printf '%s %s\n' "$cell" "$prog" >>"$W/tasks.txt"
  done
done
ncells=$(printf '%s\n' $CELLS | wc -l | tr -d ' ')

load() {
  if [ -r /proc/loadavg ]; then
    awk '{printf "%s %s %s", $1, $2, $3}' /proc/loadavg
  else
    echo "unknown"
  fi
}
load_start=$(load)

echo "backend-matrix: $count programs x $ncells cells = $((count * ncells)) builds"
echo "                cells:    $CELLS"
echo "                compiler: $BIN"
echo "                record:   $W"
echo "                jobs:     $JOBS, load at start: $load_start"
echo

start=$(date +%s)
export MATRIX_SELF=$SELF MATRIX_DIR=$DIR MATRIX_BIN=$BIN MATRIX_W=$W \
  MATRIX_RUN_TIMEOUT=$RUN_TIMEOUT MATRIX_BUILD_TIMEOUT=$BUILD_TIMEOUT \
  MATRIX_KEEP=$KEEP
# The cell and the program are the two arguments xargs splits each line of the
# task list into; everything else comes from the environment above.
xargs -P "$JOBS" -n 2 sh -c 'MATRIX_CELL=$1 MATRIX_PROG=$2 MATRIX_CHILD=1 sh "$MATRIX_SELF"' sh \
  <"$W/tasks.txt"
xargsstat=$?
[ "$xargsstat" -eq 0 ] || echo "backend-matrix: xargs exited $xargsstat" >&2

# A cell that recorded nothing is a bug in this script, not a result.
missing=0
for prog in $programs; do
  for cell in $CELLS; do
    [ -f "$W/runs/$prog.$cell.tsv" ] || { echo "backend-matrix: no record for $prog $cell" >&2; missing=$((missing + 1)); }
  done
done
[ "$missing" -eq 0 ] || exit 2

cat "$W"/runs/*.tsv >"$W/results.tsv"

# ---------------------------------------------------------------- the report
#
# The classification, the counts and the lists. cksum groups the cells that
# produced the same bytes and cmp confirms it below, so a listed divergence is
# never a guess.
elapsed=$(( $(date +%s) - start ))
awk -F'\t' -v W="$W" -v cells="$CELLS" -v known="$KNOWN" -v allow="$ALLOW" \
  -v strict="$STRICT" -v loadavg="$load_start" -v jobs="$JOBS" \
  -v count="$count" -v elapsed="$elapsed" '
function rank(c,   a,o,b) {
  split(c, a, "-")
  o = (a[1] == "O0") ? 1 : 2
  b = (a[2] == "clang") ? 1 : ((a[2] == "gcc") ? 2 : 3)
  return o * 10 + b
}
# sortstr sorts an array of strings in place (the report is read by people and
# compared between runs, so its order cannot depend on the hash order awk uses)
function sortstr(arr, n,   i, j, t) {
  for (i = 2; i <= n; i++) {
    t = arr[i]
    for (j = i - 1; j >= 1 && arr[j] > t; j--) arr[j + 1] = arr[j]
    arr[j + 1] = t
  }
}
function loadwork(f, arr,   line, n, flds, i, reason) {
  while ((getline line < f) > 0) {
    sub(/#.*/, "", line)
    n = split(line, flds, /[ \t]+/)
    if (n < 2 || flds[1] == "") continue
    reason = ""
    for (i = 3; i <= n; i++) reason = reason (i > 3 ? " " : "") flds[i]
    arr[flds[1]] = flds[2] " (" reason ")"
  }
  close(f)
}
BEGIN {
  loadwork(known, kf)
  loadwork(allow, al)
  nc = split(cells, celllist, /[ \t]+/)
  for (i = 1; i <= nc; i++) perm[celllist[i]] = 1
  # canonical cell order, whatever order the caller listed them in
  for (i = 1; i <= nc; i++) {
    for (j = i + 1; j <= nc; j++) {
      if (rank(celllist[j]) < rank(celllist[i])) { t = celllist[i]; celllist[i] = celllist[j]; celllist[j] = t }
    }
  }
  printf "backend-matrix: %s programs x %d cells, %ss wall, jobs %s, load at start %s\n", count, nc, elapsed, jobs, loadavg
  print "cells: " cells
  print ""
}
{
  prog = $1
  if (!(prog in seen)) { seen[prog] = 1; order[++np] = prog }
  cell = $2
  if (!(cell in perm)) next
  key = prog SUBSEP cell
  state[key] = $3; exitc[key] = $4; want[key] = $5
  sout[key] = $6; serr[key] = $7; note[key] = $8
  outsum[key] = $9; errsum[key] = $10
  if ($3 == "refused") {
    refused[cell]++
    if ($8 ~ /^TY-/) named[cell]++; else other[cell]++
  } else {
    ran[cell]++
    if ($6 == "ok" && $4 == $5 && ($7 == "ok" || $7 == "none")) met[cell]++; else unmet[cell]++
    if ($7 == "extra") extra[cell]++
    if ($3 == "crash") crash[cell]++
    if ($3 == "run-timeout") to[cell]++
  }
}
END {
  # ------------------------------------------------------------ per cell
  print "== per cell (a cell is one optimisation level of one back end)"
  printf "%-9s %6s %6s %6s %6s %6s %6s %6s %6s\n", "cell", "ran", "met", "unmet", "ref", "named", "other", "crash", "timeout"
  for (i = 1; i <= nc; i++) {
    c = celllist[i]
    printf "%-9s %6d %6d %6d %6d %6d %6d %6d %6d\n", c, ran[c] + 0, met[c] + 0, unmet[c] + 0,
      refused[c] + 0, named[c] + 0, other[c] + 0, crash[c] + 0, to[c] + 0
  }
  print ""
  print "  ran     built and run; met/unmet is the suite expectation holding or not"
  print "  ref     the build was refused; named is a TY- diagnostic (the driver saying"
  print "          what it cannot lower), other is a build failure of any other kind"
  print "  crash   ran but died on a signal; timeout ran past RUN_TIMEOUT"
  print ""

  # ------------------------------------------------------------ per program
  for (p = 1; p <= np; p++) {
    prog = order[p]
    nran = 0; nref = 0; nbuildfail = 0; nnamed = 0; nsig = 0; badc = 0
    delete sig
    for (i = 1; i <= nc; i++) {
      c = celllist[i]
      k = prog SUBSEP c
      if (state[k] == "refused") {
        nref++
        if (note[k] ~ /^TY-/) nnamed++; else nbuildfail++
        continue
      }
      nran++
      s = state[k] ":" exitc[k] ":" outsum[k] ":" errsum[k]
      siglist[k] = s
      if (!(s in sig)) { sig[s] = c; nsig++ }
      if (sout[k] != "ok" || exitc[k] != want[k] || (serr[k] != "ok" && serr[k] != "none")) badc = 1
    }
    # A cell that could not build the program at all while another cell built
    # and ran it is a disagreement about the program, so it belongs with the
    # divergences: the report shows which cell failed and with what.
    if (nran == 0) class = (nbuildfail == 0) ? "refused" : "buildfail"
    else if (nsig > 1) class = "diverge"
    else if (nbuildfail > 0) class = "diverge"
    else class = badc ? ((prog in kf) ? "known" : "expect") : "ok"
    klass[prog] = class
    nclass[class]++
    if (class == "diverge" || class == "buildfail") {
      if (prog in al) acct[prog] = 1; else unacct[++nun] = prog
    }
    # a program some cell refused by name is reported whether or not that is
    # all that happened to it: a named refusal is a limit of that back end, and
    # the list of them is what the plan asks for
    if (nnamed > 0) ncap++
  }

  print "== programs where the cells disagree with each other (" nclass["diverge"] + 0 ")"
  print "  This is W8\047s question: the answer depends on which back end compiled it."
  for (p = 1; p <= np; p++) {
    prog = order[p]
    if (klass[prog] != "diverge") continue
    printf "  DIVERGE %s%s\n", prog, (prog in al) ? "  [" al[prog] "]" : ""
    delete shown
    for (i = 1; i <= nc; i++) {
      c = celllist[i]; k = prog SUBSEP c
      printf "    %-9s %-11s exit %-4s stdout %-4s stderr %-5s %s\n", c, state[k], exitc[k], sout[k], serr[k], note[k]
      if (state[k] == "refused") continue
      s = siglist[k]
      if (!(s in shown)) { shown[s] = c; printf "    %-9s -> %s\n", c, s }
      else printf "    %-9s -> same as %s\n", c, shown[s]
      print prog "\t" c "\t" s > (W "/divergent.tsv")
    }
  }
  print ""

  print "== programs the same in every cell that ran, and not what the suite expects (" nclass["expect"] + 0 ")"
  print "  One defect rather than two back ends disagreeing; the suite\047s drivers own these."
  for (p = 1; p <= np; p++) if (klass[order[p]] == "expect") print "  " order[p]
  print ""

  print "== known failures that failed here as tests/known-failures.txt says they would (" nclass["known"] + 0 ")"
  for (p = 1; p <= np; p++) if (klass[order[p]] == "known") print "  " order[p]
  print ""

  print "== refused by a named diagnostic in at least one cell (" ncap + 0 ")"
  for (p = 1; p <= np; p++) {
    prog = order[p]
    notes = ""
    named_here = 0
    for (i = 1; i <= nc; i++) {
      c = celllist[i]; k = prog SUBSEP c
      if (state[k] == "refused" && note[k] ~ /^TY-/) { notes = notes (notes == "" ? "" : " ") c ":" note[k]; named_here = 1 }
    }
    if (!named_here) continue
    printf "  %-40s %s\n", prog, notes
  }
  print ""

  nbuild = 0
  for (p = 1; p <= np; p++) if (klass[order[p]] == "buildfail") nbuild++
  print "== build failures that are not a named refusal (" nbuild ")"
  for (p = 1; p <= np; p++) {
    prog = order[p]
    if (klass[prog] != "buildfail") continue
    for (i = 1; i <= nc; i++) {
      c = celllist[i]; k = prog SUBSEP c
      if (state[k] == "refused" && note[k] !~ /^TY-/) printf "  %-40s %-9s %s\n", prog, c, note[k]
    }
  }
  print ""

  printf "== agreed with the suite in every cell that ran (%d)\n", nclass["ok"] + 0
  printf "== classifications: %d diverge, %d buildfail, %d refused-everywhere, %d known, %d expect, %d ok\n\n",
    nclass["diverge"] + 0, nclass["buildfail"] + 0, nclass["refused"] + 0, nclass["known"] + 0,
    nclass["expect"] + 0, nclass["ok"] + 0

  # accounting: a stale entry is a failure of the run, the way it is in
  # tests/known-failures.txt -- the entry cannot outlive what it describes
  for (prog in al) {
    if (!(prog in klass)) { entry[++ne] = prog "  (not in the suite)"; }
    else if (klass[prog] != "diverge" && klass[prog] != "buildfail") entry[++ne] = prog "  (" al[prog] ")";
  }
  sortstr(entry, ne)

  fatal = nun + ne
  if (strict) fatal += nclass["expect"] + 0
  printf "verdict: %s\n", (fatal == 0) ? "ok" : "failed"
  if (nun > 0) {
    print "  " nun " finding(s) not accounted for in " allow ":"
    for (i = 1; i <= nun; i++) print "    " unacct[i]
    print "  fix it, or add `<program> <work item> <reason>` to that file."
  }
  if (ne > 0) {
    print "  " ne " stale allowlist entry/entries -- delete them:"
    for (i = 1; i <= ne; i++) print "    " entry[i]
  }
  if (strict && nclass["expect"] > 0)
    print "  STRICT=1: " nclass["expect"] " program(s) that every cell got wrong"
  exit (fatal == 0) ? 0 : 1
}
' "$W"/runs/*.tsv >"$W/report.txt"
verdict=$?
cat "$W/report.txt"

echo
echo "backend-matrix: record in $W"
echo "  results.tsv    one line per program and cell"
echo "  report.txt     the report above"
echo "  runs/          each cell's compiler output, its stdout and its stderr"
echo "  divergent.tsv  the divergences, for a diff or a script"
[ -z "$BIN_TEMP" ] || rm -f "$BIN_TEMP"

# ---------------------------------------------------------------- the bytes
#
# A report that says "these two cells differ" is a report nobody can act on, so
# the divergences are printed as a diff against the first cell of each group.
# Nothing here decides anything: the classification is above, and this only
# shows what it classified.
if [ -s "$W/divergent.tsv" ]; then
  echo
  echo "== the divergences, byte for byte (the first cell of each group is the reference)"
  prev=""; refout=""; referr=""
  while IFS="$(printf '\t')" read -r prog cell _sig; do
    if [ "$prog" != "$prev" ]; then
      echo
      echo "-- $prog"
      prev=$prog
      refout=$W/runs/$prog.$cell.out
      referr=$W/runs/$prog.$cell.err
      [ -f "$refout" ] || refout=""
      continue
    fi
    if [ -n "$refout" ] && [ -f "$W/runs/$prog.$cell.out" ] && ! cmp -s "$refout" "$W/runs/$prog.$cell.out"; then
      echo "   $cell stdout:"
      diff -u "$refout" "$W/runs/$prog.$cell.out" | sed -n '3,14p' | sed 's/^/     /'
    fi
    if [ -n "$referr" ] && [ -f "$W/runs/$prog.$cell.err" ] && ! cmp -s "$referr" "$W/runs/$prog.$cell.err"; then
      echo "   $cell stderr:"
      diff -u "$referr" "$W/runs/$prog.$cell.err" | sed -n '3,14p' | sed 's/^/     /'
    fi
  done <"$W/divergent.tsv"
fi
exit "$verdict"
