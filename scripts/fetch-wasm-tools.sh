#!/usr/bin/env bash
# Put a sha256-verified wasm-tools release tarball at <dest>, from the cache
# if <dest> is already there and still verifies, from the GitHub release
# otherwise. Used by tier1-gate.yml's two "Fetch wasm-tools" steps (cleat#3693),
# which sit between actions/cache/restore and actions/cache/save.
#
# WHY THIS EXISTS. The install used to be one `curl --retry 5 --retry-delay 2`
# and nothing else. On 2026-11-08 `develop` c67b11f3's `Tier 1 Gate -- rest
# shard 3/4` failed four attempts, two of them in that step: six 504s from the
# release asset in about ten seconds, then exit 22. The retry window was shorter
# than the outage, and a required check went red for a reason that said nothing
# about the code under test.
#
# WHAT IT DOES.
#
#   1. If <dest> exists, check it against WASM_TOOLS_SHA256. Good: done, with
#      no network at all.
#   2. If it exists and FAILS the check, say so as a warning, delete it, and
#      go on to 3. It is not used and it does not fail the job. The cache key
#      the workflow uses is immutable once saved, so failing here would make
#      every later run on that key fail until someone deleted the entry by hand
#      -- the "permanently unmergeable" shape tier1-gate.yml's header records
#      for a setup-go cache key. The warning carries the delete command.
#   3. Download with a bounded retry window (below) to <dest>.partial, check
#      it against the SAME digest, and only then move it to <dest>. A download
#      that fails the check is a hard failure, not a retry: a 200 with the
#      wrong bytes is not a transient outage, and it is the case the pin
#      exists to refuse.
#
# TO A FILE, NOT PIPED INTO tar, one attempt at a time. A retried pipe hands tar
# the first attempt's bytes followed by a complete second copy (ci.yml's
# "Install lint tools" has the measurement); here every attempt starts a fresh
# file and the check runs on the whole of it.
#
# THE RETRY BOUND. No new attempt starts once 240 s have elapsed since the
# script began, and one attempt is itself bounded by --connect-timeout 15 and
# --max-time 60, so the script returns within about 300 s of starting whatever
# the server does. Between attempts it sleeps 5, 10, 20, 40, 60, 60 ... s, and
# it does not sleep if the sleep alone would pass the 240 s mark. The old
# window was ~10 s.
#
# EXIT: 0 a verified tarball is at <dest>; 1 it could not be obtained or
# failed the check; 2 a usage error or, under --self-test, a tool the test
# needs is missing (a check that could not run is not a pass).
#
# Environment:
#   WASM_TOOLS_VERSION  required, e.g. 1.255.0
#   WASM_TOOLS_SHA256   required, the pinned digest of the x86_64-linux tarball
#   WASM_TOOLS_URL      optional; overrides the release URL. A test seam: the
#                       workflow never sets it.
#   WASM_TOOLS_FETCH_BUDGET_S, WASM_TOOLS_FETCH_FIRST_DELAY_S,
#   WASM_TOOLS_FETCH_MAX_TIME_S
#                       optional; the 240, 5 and 60 above, so the self-test can
#                       exercise the give-up path and a stalled server in
#                       seconds. The workflow never sets them.
#
# Usage:
#   scripts/fetch-wasm-tools.sh <dest.tar.gz>
#   scripts/fetch-wasm-tools.sh --self-test

set -euo pipefail

verify() {
  # $1 = file. Output is left visible: "<file>: OK" or "FAILED" is the log line.
  echo "${WASM_TOOLS_SHA256}  $1" | sha256sum -c -
}

fetch() {
  local dest="${1:?usage: fetch-wasm-tools.sh <dest.tar.gz>}"
  : "${WASM_TOOLS_VERSION:?WASM_TOOLS_VERSION is required}"
  : "${WASM_TOOLS_SHA256:?WASM_TOOLS_SHA256 is required}"
  local url="${WASM_TOOLS_URL:-https://github.com/bytecodealliance/wasm-tools/releases/download/v${WASM_TOOLS_VERSION}/wasm-tools-${WASM_TOOLS_VERSION}-x86_64-linux.tar.gz}"
  local budget="${WASM_TOOLS_FETCH_BUDGET_S:-240}"
  local delay="${WASM_TOOLS_FETCH_FIRST_DELAY_S:-5}"
  local max_time="${WASM_TOOLS_FETCH_MAX_TIME_S:-60}"
  local part="${dest}.partial"

  mkdir -p "$(dirname "$dest")"
  # Expanded now, not at exit: $part is a local and is gone (so `set -u` would
  # fail the script) by the time an EXIT trap quoted with '' runs.
  # shellcheck disable=SC2064  # expanding now is the point
  trap "rm -f '$part'" EXIT

  if [ -e "$dest" ]; then
    if verify "$dest"; then
      echo "wasm-tools ${WASM_TOOLS_VERSION}: using the cached tarball (sha256 verified, no download)"
      return 0
    fi
    echo "::warning::wasm-tools: the cached tarball at ${dest} failed its sha256 check and was NOT used; downloading the release instead. The cache entry is immutable, so every run will do this until it is deleted: gh cache delete <key> --repo <owner>/<repo>"
    rm -f "$dest"
  fi

  local attempt=0
  while :; do
    attempt=$((attempt + 1))
    rm -f "$part"
    if curl -sSLf --connect-timeout 15 --max-time "$max_time" -o "$part" "$url"; then
      break
    fi
    if [ $((SECONDS + delay)) -ge "$budget" ]; then
      echo "::error::wasm-tools: could not download ${url} after ${attempt} attempt(s) in ${SECONDS}s (retry budget ${budget}s)"
      return 1
    fi
    echo "wasm-tools: attempt ${attempt} failed after ${SECONDS}s; retrying in ${delay}s"
    sleep "$delay"
    delay=$((delay * 2))
    [ "$delay" -le 60 ] || delay=60
  done

  if ! verify "$part"; then
    echo "::error::wasm-tools: the downloaded tarball does not match the pinned sha256; refusing it (not retried: wrong bytes are not an outage)"
    return 1
  fi
  mv "$part" "$dest"
  echo "wasm-tools ${WASM_TOOLS_VERSION}: downloaded in ${attempt} attempt(s), sha256 verified"
}

self_test() {
  local tool
  for tool in python3 curl sha256sum; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "UNMEASURED: ${tool} is not installed, so the self-test cannot run." >&2
      echo "This is a failure of the check, not a finding about the script." >&2
      exit 2
    fi
  done

  # work and server_pid are globals on purpose: the EXIT trap runs after this
  # function has returned, when its locals no longer exist.
  local self failures=0 cases=0
  work="" server_pid=""
  self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
  work="$(mktemp -d)"
  stop_server() {
    if [ -n "$server_pid" ]; then
      kill "$server_pid" 2>/dev/null || true
      wait "$server_pid" 2>/dev/null || true
      server_pid=""
    fi
  }
  # shellcheck disable=SC2317  # invoked through the trap
  cleanup() {
    stop_server
    rm -rf "$work"
  }
  trap cleanup EXIT

  # A scripted release server: the first $2 requests get a 504, the rest get
  # $1's bytes; a negative $2 means it accepts the connection and then says
# nothing (the "Read timed out" shape of a half-dead release endpoint). It
# counts requests in $4 so a case can say how many times the
  # network was touched, which is the quantity the cache exists to reduce.
  cat >"$work/server.py" <<'PY'
import http.server, sys, time

payload = open(sys.argv[1], "rb").read()
fail_first = int(sys.argv[2])
portfile, hitsfile = sys.argv[3], sys.argv[4]
hits = 0


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global hits
        hits += 1
        with open(hitsfile, "w") as f:
            f.write(str(hits))
        if fail_first < 0:
            time.sleep(120)
            return
        if hits <= fail_first:
            self.send_response(504)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True


server = Server(("127.0.0.1", 0), Handler)
with open(portfile, "w") as f:
    f.write(str(server.server_port))
server.serve_forever()
PY

  printf 'the real tarball, as far as this test is concerned\n' >"$work/good.tgz"
  printf 'a different file under the same name\n' >"$work/evil.tgz"
  local good_sha
  good_sha="$(sha256sum "$work/good.tgz" | cut -d' ' -f1)"

  # start_server <payload> <fail_first>; sets $url and $hits_file
  start_server() {
    stop_server
    rm -f "$work/port" "$work/hits"
    python3 -I "$work/server.py" "$1" "$2" "$work/port" "$work/hits" &
    server_pid=$!
    local i=0
    while [ ! -s "$work/port" ]; do
      i=$((i + 1))
      if [ "$i" -gt 100 ]; then
        echo "UNMEASURED: the test server did not start" >&2
        exit 2
      fi
      sleep 0.1
    done
    url="http://127.0.0.1:$(cat "$work/port")/wasm-tools.tar.gz"
    hits_file="$work/hits"
  }
  server_hits() { if [ -f "$hits_file" ]; then cat "$hits_file"; else echo 0; fi; }

  # run_case <name> <want-exit> <budget> <first-delay> [max-time]; sets $rc $out
  # $elapsed.
  # The server answers in milliseconds, so a run that outlives its budget by 15 s
  # has lost its bound; it is killed and reported as rc=124 rather than left to
  # hang the self-test (and the Lint job) until something else times out.
  run_case() {
    local started=$SECONDS limit=$(($3 + 15)) pid timedout=no
    set +e
    env WASM_TOOLS_VERSION=0.0.0 WASM_TOOLS_SHA256="$good_sha" WASM_TOOLS_URL="$url" \
      WASM_TOOLS_FETCH_BUDGET_S="$3" WASM_TOOLS_FETCH_FIRST_DELAY_S="$4" \
      WASM_TOOLS_FETCH_MAX_TIME_S="${5:-60}" \
      bash "$self" "$work/cache/wasm-tools.tar.gz" >"$work/case.out" 2>&1 &
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
      if [ $((SECONDS - started)) -ge "$limit" ]; then
        kill "$pid" 2>/dev/null
        timedout=yes
        break
      fi
      sleep 0.2
    done
    wait "$pid" 2>/dev/null
    rc=$?
    set -e
    [ "$timedout" = no ] || rc=124
    out="$(cat "$work/case.out")"
    elapsed=$((SECONDS - started))
  }

  # expect <name> <condition-result: 0|1> <detail>
  expect() {
    cases=$((cases + 1))
    if [ "$2" -eq 0 ]; then
      echo "PASS  $1  [$3]"
    else
      echo "FAIL  $1  [$3]"
      failures=$((failures + 1))
    fi
  }
  has() { case "$out" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
  dest_sha() { if [ -f "$work/cache/wasm-tools.tar.gz" ]; then sha256sum "$work/cache/wasm-tools.tar.gz" | cut -d' ' -f1; else echo none; fi; }
  reset_cache() { rm -rf "$work/cache"; mkdir -p "$work/cache"; }

  # 1. cold cache: the release is fetched, verified, and left at <dest>.
  reset_cache; start_server "$work/good.tgz" 0
  run_case cold 0 30 1
  expect "cold cache downloads, verifies and keeps the tarball" \
    "$([ $rc -eq 0 ] && [ "$(dest_sha)" = "$good_sha" ] && [ "$(server_hits)" = 1 ] && [ ! -e "$work/cache/wasm-tools.tar.gz.partial" ] && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) dest_sha_ok=$([ "$(dest_sha)" = "$good_sha" ] && echo yes || echo no)"

  # 2. warm cache: no request is made at all.
  reset_cache; cp "$work/good.tgz" "$work/cache/wasm-tools.tar.gz"; start_server "$work/good.tgz" 0
  run_case warm 0 30 1
  expect "a verified cached tarball is used with NO network request" \
    "$([ $rc -eq 0 ] && [ "$(server_hits)" = 0 ] && has "using the cached tarball" && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits)"

  # 3. a bad cache entry: refused loudly, replaced from the release, not fatal.
  reset_cache; cp "$work/evil.tgz" "$work/cache/wasm-tools.tar.gz"; start_server "$work/good.tgz" 0
  run_case badcache 0 30 1
  expect "a cache entry that fails sha256 is refused, warned about, and replaced" \
    "$([ $rc -eq 0 ] && [ "$(dest_sha)" = "$good_sha" ] && [ "$(server_hits)" = 1 ] && has "::warning::" && has "was NOT used" && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) dest_sha_ok=$([ "$(dest_sha)" = "$good_sha" ] && echo yes || echo no) warned=$(has '::warning::' && echo yes || echo no)"

  # 4. a bad cache entry AND a release serving wrong bytes: fail, leave nothing.
  reset_cache; cp "$work/evil.tgz" "$work/cache/wasm-tools.tar.gz"; start_server "$work/evil.tgz" 0
  run_case badboth 1 30 1
  expect "wrong bytes from the release are refused and never left at <dest>" \
    "$([ $rc -eq 1 ] && [ "$(dest_sha)" = none ] && has "does not match the pinned sha256" && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) dest=$(dest_sha | cut -c1-8)"

  # 5. a short outage: three 504s, then 200, inside the budget.
  reset_cache; start_server "$work/good.tgz" 3
  run_case flap 0 30 1
  expect "three 504s then success is ridden out (the old window was five retries at 2 s)" \
    "$([ $rc -eq 0 ] && [ "$(server_hits)" = 4 ] && [ "$(dest_sha)" = "$good_sha" ] && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) elapsed=${elapsed}s"

  # 6. an outage longer than the budget: gives up, bounded, says so.
  reset_cache; start_server "$work/good.tgz" 1000000
  run_case down 1 6 1
  # >= 2 attempts, not 3: $SECONDS is whole seconds, so a run that starts late
  # in a second reads one second further on and may legitimately stop a try early.
  expect "a permanent outage gives up inside the budget and leaves no file" \
    "$([ $rc -eq 1 ] && [ "$(server_hits)" -ge 2 ] && [ "$elapsed" -le 12 ] && [ "$(dest_sha)" = none ] && has "could not download" && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) elapsed=${elapsed}s budget=6s"

  # 7. a server that accepts and never answers: each attempt is cut off by
  # --max-time, the loop still honours the budget, and nothing is left at <dest>.
  # Without --max-time curl would wait on the stalled connection for as long as
  # the server likes, past the budget and past the watchdog (rc=124).
  reset_cache; start_server "$work/good.tgz" -1
  run_case stall 1 5 1 2
  expect "a stalled server is cut off per attempt and the step still ends inside its bound" \
    "$([ $rc -eq 1 ] && [ "$(server_hits)" -ge 2 ] && [ "$elapsed" -le 12 ] && [ "$(dest_sha)" = none ] && has "could not download" && echo 0 || echo 1)" \
    "exit=$rc hits=$(server_hits) elapsed=${elapsed}s budget=5s max_time=2s"

  echo "${cases} cases, ${failures} failed"
  [ "$failures" -eq 0 ]
}

case "${1:-}" in
  --self-test) self_test ;;
  "" | -h | --help)
    sed -n '2,/^set -euo/p' "$0" | sed '$d' >&2
    exit 2
    ;;
  *) fetch "$1" ;;
esac
