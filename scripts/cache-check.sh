#!/bin/sh
# The standard library cache, checked rather than assumed (W13).
#
# A cache's failure mode is not a slow build, it is a wrong program, so this
# script does not measure how fast the second build is: it checks that the
# second build *reuses* the standard library, that reusing it produces the same
# executable as building it from scratch, that a different optimisation level
# does not reuse it, that a different program does, and that a cache with
# something wrong in it is not read at all. Each check says what it proves.
#
# Usage: scripts/cache-check.sh [compiler]
set -e

CC=${1:-clang}
TMP=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMP/teyru-cache-check.XXXXXX")
CACHE="$WORK/cache"
trap 'rm -rf "$WORK"' EXIT

BIN=${BIN:-./teyru}
if [ ! -x "$BIN" ]; then
  echo "cache-check: $BIN is not built (run: make build)" >&2
  exit 2
fi

export TEYRU_CACHE_DIR="$CACHE"
export TMPDIR="$TMP"

cat > "$WORK/hello.teyru" <<'EOF'
class Main {
  public static void main(String[] args) {
    System.out.println("hello, world")
  }
}
EOF
cat > "$WORK/other.teyru" <<'EOF'
class Main {
  public static void main(String[] args) {
    System.out.println("a second program")
  }
}
EOF

fail=0
say() { printf '%s\n' "$*"; }
check() {
  if [ "$2" = "$3" ]; then
    say "ok   $1"
  else
    say "FAIL $1: $2, want $3"
    fail=1
  fi
}

# ---- 1. a cold build fills the cache, and the program runs
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-cold" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-cold")
check "a -O0 build runs the program" "$out" "hello, world"
entries=$(find "$CACHE" -name prelude.o | wc -l | tr -d ' ')
check "the cold build cached the standard library" "$entries" "1"

# ---- 2. the second build reuses it: the object is not rewritten
obj=$(find "$CACHE" -name prelude.o | head -1)
before=$(stat -c '%Y:%s' "$obj")
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-warm" "$WORK/hello.teyru" >/dev/null
after=$(stat -c '%Y:%s' "$obj")
check "the warm build reused the cached object" "$after" "$before"
cmp -s "$WORK/hello-cold" "$WORK/hello-warm" && same=yes || same=no
check "the warm build produced the same executable" "$same" "yes"

# ---- 3. a different program reuses it too: that is what the cache is for
"$BIN" build -O0 --cc "$CC" -o "$WORK/other" "$WORK/other.teyru" >/dev/null
after=$(stat -c '%Y:%s' "$obj")
check "a different program reused the same object" "$after" "$before"
out=$("$WORK/other")
check "the other program runs" "$out" "a second program"

# ---- 4. a different optimisation level does not reuse it
"$BIN" build -O1 --cc "$CC" -o "$WORK/hello-o1" "$WORK/hello.teyru" >/dev/null
n=$(find "$CACHE" -name prelude.o | wc -l | tr -d ' ')
check "another optimisation level is another entry" "$n" "2"

# ---- 5. a wrong object in the cache is not reused: deleting it rebuilds it
rm -f "$obj"
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-again" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-again")
check "a build with its cached object removed still works" "$out" "hello, world"
n=$(find "$CACHE" -name prelude.o | wc -l | tr -d ' ')
check "and rebuilt it" "$n" "2"

# ---- 6. the cache off: the same program, built the long way
TEYRU_NOCACHE=1 "$BIN" build -O0 --cc "$CC" -o "$WORK/hello-nocache" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-nocache")
check "TEYRU_NOCACHE builds and runs the same program" "$out" "hello, world"

# ---- 7. the debug build and the release build are the same program
"$BIN" build -O2 --cc "$CC" -o "$WORK/hello-release" "$WORK/hello.teyru" >/dev/null
a=$("$WORK/hello-cold")
b=$("$WORK/hello-release")
check "the split build and the whole-program build agree" "$a" "$b"

# ---- 8. a damaged artifact is not read
#
# This is the check the rest of the script exists for: a cache that answers a
# build with half an object does not fail loudly, it fails as a program that
# does something else, or as a link error at best. An entry records the size and
# the hash of every file it holds, so damage of either shape is a miss.
obj=$(find "$CACHE" -name prelude.o | head -1)
size=$(stat -c '%s' "$obj")
# One block, not one byte at a time: an object is megabytes.
dd if="$obj" of="$WORK/half.o" bs=$((size / 2)) count=1 status=none
mv "$WORK/half.o" "$obj"
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-truncated" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-truncated")
check "a truncated object is refused and the build is redone" "$out" "hello, world"
check "and the artifact is whole again" "$(stat -c '%s' "$obj")" "$size"

# The same length with one bit different is the case a size check would pass and
# a linker would accept; only the recorded hash says no.
mid=$((size / 2))
byte=$(dd if="$obj" bs=1 skip=$mid count=1 status=none | od -An -tu1 | tr -d ' ')
flipped=$(printf '\\%03o' $((byte ^ 255)))
printf "$flipped" | dd of="$obj" bs=1 seek=$mid count=1 conv=notrunc status=none
cp "$obj" "$WORK/flipped.o"
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-flipped" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-flipped")
check "a corrupted object is refused and the build is redone" "$out" "hello, world"
cmp -s "$obj" "$WORK/flipped.o" && same=yes || same=no
check "and the damaged file was replaced rather than kept" "$same" "no"

# ---- 9. an entry with no record of itself is not read either: the manifest is
# what says these are the files a build wrote, so files without one are files of
# unknown provenance -- a directory restored from a backup, or a layout this
# compiler did not write.
find "$CACHE" -name manifest -delete
"$BIN" build -O0 --cc "$CC" -o "$WORK/hello-nomanifest" "$WORK/hello.teyru" >/dev/null
out=$("$WORK/hello-nomanifest")
check "an entry with no manifest is not read" "$out" "hello, world"
# The build that read no manifest records what it wrote, so the entry it used
# has one again; the other optimisation level's entry stays unrecorded until a
# build of that level runs, which is what "not read" means for it too.
n=$(find "$CACHE" -name manifest | wc -l | tr -d ' ')
check "and the build wrote the manifest for the entry it used" "$n" "1"

if [ "$fail" -ne 0 ]; then
  say "cache-check: failed"
  exit 1
fi
say "cache-check: ok"
