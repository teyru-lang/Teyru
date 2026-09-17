#!/bin/sh
#
# check-notices.sh -- make THIRD-PARTY-NOTICES.md answer to the tree.
#
# The notices file makes claims about *this* repository: which files the runtime
# is, what version of OpenSSL the TLS layer demands, what the Windows target
# links, where the time zone data comes from, which licence files exist. A claim
# in a file is worth nothing unless something fails when the tree stops agreeing
# with it, so every one of those is read back out of the file it describes.
#
#   scripts/check-notices.sh           check; exit non-zero on any difference
#   scripts/check-notices.sh --write   regenerate the generated block in place
#
# `make notices` is the first form. The release workflow runs it before a
# release is published, which is why it takes no arguments, needs no network,
# and writes nothing in check mode. There is no CI job in this repository by
# owner decision; this script is what that job would have run.
#
# POSIX shell, grep, sed and awk only, and LC_ALL=C so the file list (a glob)
# and everything compared against it do not depend on the caller's locale.

set -u

LC_ALL=C
export LC_ALL

cd "$(dirname "$0")/.." || exit 1

SRC=internal/runtime/src
NOTICES=THIRD-PARTY-NOTICES.md
BEGIN='<!-- BEGIN GENERATED: internal/runtime/src -->'
END='<!-- END GENERATED: internal/runtime/src -->'

case "${1:-}" in
""|--write) ;;
*) echo "usage: $0 [--write]" >&2; exit 2 ;;
esac
WRITE=no
[ "${1:-}" = "--write" ] && WRITE=yes

bad=0
problem() {
	bad=$((bad + 1))
	echo "check-notices: $*" >&2
}

# --- 1. the runtime file list ------------------------------------------------
#
# The generated block is a glob of the directory: adding a file without
# regenerating it changes the list, and the list is what this compares.

gen_runtime() {
	n=0
	entries=
	for f in "$SRC"/*; do
		[ -f "$f" ] || continue
		n=$((n + 1))
		entries="$entries
- \`${f##*/}\`"
	done
	printf '執行期目前有 %d 個檔案：\n%s\n' "$n" "$entries"
}

gen=$(mktemp) || exit 1
have=$(mktemp) || exit 1
trap 'rm -f "$gen" "$have"' EXIT
gen_runtime >"$gen"

awk -v b="$BEGIN" -v e="$END" '
	$0 == b { inblock = 1; next }
	$0 == e { inblock = 0; next }
	inblock { print }' "$NOTICES" >"$have"

if [ "$WRITE" = yes ]; then
	tmp=$(mktemp) || exit 1
	if awk -v b="$BEGIN" -v e="$END" -v gen="$gen" '
		$0 == b { print; while ((getline l < gen) > 0) print l; skip = 1; next }
		$0 == e { skip = 0 }
		!skip { print }' "$NOTICES" >"$tmp"; then
		mv "$tmp" "$NOTICES"
		echo "check-notices: regenerated the runtime list in $NOTICES"
	else
		rm -f "$tmp"
		problem "could not rewrite $NOTICES"
	fi
	exit $bad
fi

if ! cmp -s "$have" "$gen"; then
	problem "$SRC and the generated list in $NOTICES disagree; run scripts/check-notices.sh --write"
	diff -u "$have" "$gen" >&2
fi

# --- 2. the OpenSSL version floor -------------------------------------------
#
# The floor lives in one place in the source, as a preprocessor #error, and is
# quoted in the notices. Bumping one without the other is the kind of drift a
# reader cannot see, so it is a failure here.

floor=$(sed -n 's/.*OPENSSL_VERSION_NUMBER *< *\(0x[0-9A-Fa-f]*L\).*/\1/p' "$SRC/tyrt_tls.c" | head -1)
if [ -z "$floor" ]; then
	problem "$SRC/tyrt_tls.c no longer states an OPENSSL_VERSION_NUMBER floor"
elif ! grep -qF -- "$floor" "$NOTICES"; then
	problem "$SRC/tyrt_tls.c requires $floor and $NOTICES does not quote it"
fi

# --- 3. what a TLS build links ----------------------------------------------

for lib in -lssl -lcrypto; do
	grep -qF -- "$lib" internal/driver/driver.go ||
		problem "internal/driver/driver.go no longer links $lib for the TLS targets"
	grep -qF -- "$lib" "$NOTICES" ||
		problem "$NOTICES does not mention $lib"
done

# --- 4. the Windows target's link mode --------------------------------------
#
# The notices say the windows/amd64 target statically links the mingw-w64
# runtime into the artifact -- that sentence is why there is a row for it, and
# it is what makes distributing a .exe an act of distributing that runtime. The
# claim is checked both ways: while the target passes -static the notices have
# to say so, and if -static goes away the sentence has to go with it.
#
# Only the target's ldflags line is read. The block has prose comments that
# mention -static too, and a substring test over the whole block would pass on
# the comment after the flag was deleted.

winflags=$(awk '/"windows\/amd64": \{/,/^\t\},/' internal/driver/driver.go |
	sed -n 's/.*ldflags: \[\]string{\(.*\)}.*/\1/p')
if [ -z "$winflags" ]; then
	problem "internal/driver/driver.go has no windows/amd64 ldflags to read"
elif printf '%s\n' "$winflags" | grep -qF -- '"-static"'; then
	grep -qF -- "靜態連結進產物" "$NOTICES" ||
		problem "the windows/amd64 target passes -static and $NOTICES does not say so"
	grep -qF -- "mingw-w64" "$NOTICES" ||
		problem "windows/amd64 statically links the mingw-w64 runtime and $NOTICES has no row for it"
else
	grep -qF -- "靜態連結進產物" "$NOTICES" &&
		problem "$NOTICES says the mingw-w64 runtime is statically linked into the artifact; windows/amd64 no longer passes -static"
fi

# --- 5. where the time zone data comes from ---------------------------------
#
# Not vendored: the host ships it, the library reads it. That is a claim about
# both files, so both are read.

tz=lib/46_timezone.teyru
if [ ! -f "$tz" ]; then
	problem "$tz is gone; the notices still describe reading the host's tzdata"
else
	grep -qF -- "/usr/share/zoneinfo" "$tz" ||
		problem "$tz no longer reads /usr/share/zoneinfo"
	grep -qF -- "TZDIR" "$tz" ||
		problem "$tz no longer honours TZDIR"
	for s in tzdata /usr/share/zoneinfo TZDIR; do
		grep -qF -- "$s" "$NOTICES" ||
			problem "$NOTICES no longer states where the tzdata comes from (missing \"$s\")"
	done
fi

# --- 6. the Unicode data files, once they are here -------------------------
#
# W5 brings Unicode 15.0's data files into the repository. This section of the
# notices has to be right on both sides of that landing, so the check is
# conditional: a data file that is in the tree has to be named in the notices,
# and while there is none there is nothing to disagree about.
#
# The names are the four the plan enumerates, which is exactly the four the
# notices names. A file the generator brings under another name is covered by
# the notices' own wording ("and the other files of the same distribution");
# this check deliberately does not list a file nobody has landed yet, because
# then it would fail on the landing that is nobody's mistake.

for name in UnicodeData.txt SpecialCasing.txt CaseFolding.txt PropList.txt; do
	found=$(find . -name "$name" -not -path './.git/*' -print -quit)
	[ -n "$found" ] || continue
	grep -qF -- "$name" "$NOTICES" ||
		problem "$found is in the tree and $NOTICES does not name $name"
done

# --- 7. the files the notices point at --------------------------------------
#
# The editor support is described in its own section because it is not in this
# repository any more (08dd6ca split it out); the rest of the rows are about
# files that are here, so both facts are checked.

for f in LICENSE LICENSE-CLASSPATH-EXCEPTION-2.0 scripts/bench.sh; do
	[ -e "$f" ] || problem "$f is named in $NOTICES but is not in the tree"
	grep -qF -- "$f" "$NOTICES" ||
		problem "$NOTICES no longer points at $f"
done

if [ -e editors ]; then
	problem "editors/ is back in this repository and $NOTICES describes it as teyru-lang/editors"
fi

# --- 8. the compiler's own dependency ---------------------------------------
#
# The notices open by saying go.mod has no external module dependency. It is one
# line to check and it is the claim the rest of the file is short because of.

grep -q '^require' go.mod &&
	problem "go.mod has a require line and $NOTICES says the compiler depends on the standard library alone"

# ---------------------------------------------------------------------------

if [ "$bad" -gt 0 ]; then
	echo "check-notices: $bad problem(s)" >&2
	exit 1
fi
echo "check-notices: $NOTICES agrees with the tree"
