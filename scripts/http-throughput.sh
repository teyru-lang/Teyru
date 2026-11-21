#!/bin/sh
# scripts/http-throughput.sh - HTTP throughput for examples/webapp, measured
# rather than claimed.
#
#   sh scripts/http-throughput.sh                    # build, serve, measure
#   AB='ab -n 50000 -c 16' sh scripts/http-throughput.sh
#   PORT=9090 sh scripts/http-throughput.sh
#
# Why this script exists. The documentation used to say HTTP throughput "was
# never written down as a number": the web framework was there, the example
# served, and no one had put a request count against it. A number is only worth
# having if the measurement can be repeated, so it lives here, with the load
# average printed on both sides of it: this machine is shared, and a loopback
# request count taken while something else is compiling measures the scheduler.
#
# What it reports. Requests completed over the wall clock `ab` measures, which
# for a single keeps-alive-free client is one connection per request. `-k`
# changes what is measured (connection reuse), so it is not in the default. The
# number is only meaningful beside the load average and beside the compiler and
# machine it ran on, and both are printed.
#
# The server's own build is separate and timed: a `teyru build` of the example is
# the part of a deployment that a request count does not show.

set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
PORT=${PORT:-8080}
N=${N:-50000}
C=${C:-16}
AB=${AB:-"ab -n $N -c $C"}
TEYRU=${TEYRU:-"$root/teyru"}

if [ ! -x "$TEYRU" ]; then
  echo "http-throughput: no compiler at $TEYRU (make build first)" >&2
  exit 2
fi
if ! command -v ab >/dev/null 2>&1; then
  echo "http-throughput: ab is not installed (apache2-utils / httpd-tools)" >&2
  exit 2
fi

load() {
  awk '{printf "%s %s %s", $1, $2, $3}' /proc/loadavg 2>/dev/null || echo "?"
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/teyru-http.XXXXXX")
trap 'kill "${pid:-0}" 2>/dev/null || true; rm -rf "$tmp"' EXIT

echo "== environment"
echo "compiler: $("$TEYRU" version 2>/dev/null | head -1)"
echo "machine:  $(uname -srm)"
echo "load:     $(load) (before the build)"

echo
echo "== build examples/webapp"
start=$(date +%s%N)
(cd "$root/examples/webapp" && "$TEYRU" build -O2 -o "$tmp/webapp" ./...)
end=$(date +%s%N)
echo "build:    $(( (end - start) / 1000000 )) ms"
echo "binary:   $(wc -c < "$tmp/webapp") bytes"

echo
echo "== serve and measure"
(cd "$root/examples/webapp" && "$tmp/webapp" --port="$PORT" >"$tmp/server.log" 2>&1) &
pid=$!
# Readiness is a request that answers, not a line of the server's stdout: stdout
# to a pipe is block-buffered, so "listening on ..." sits in the C buffer until
# the process exits, and a script that waited for it would always time out.
i=0
ready=0
while [ "$i" -lt 300 ]; do
  if curl -fsS -m 1 -o /dev/null "http://127.0.0.1:$PORT/todos" 2>/dev/null; then
    ready=1
    break
  fi
  i=$((i + 1))
  sleep 0.1
done
if [ "$ready" -ne 1 ]; then
  echo "http-throughput: the server never answered:" >&2
  sed -n '1,10p' "$tmp/server.log" >&2
  exit 1
fi

echo "load:     $(load) (before the measurement)"
echo "command:  $AB http://127.0.0.1:$PORT/todos"
$AB "http://127.0.0.1:$PORT/todos" >"$tmp/ab.log" 2>&1 || true
echo "load:     $(load) (after the measurement)"
grep -E "^(Requests per second|Time per request|Complete requests|Failed requests|Non-2xx responses)" "$tmp/ab.log" || sed -n '1,20p' "$tmp/ab.log"

echo
echo "== caveats"
echo "- loopback traffic measures this machine's scheduler as much as the server;"
echo "  the load averages above are printed so a busy machine is visible."
echo "- one connection per request unless -k is used, and -k measures different work."
