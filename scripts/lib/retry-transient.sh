#!/usr/bin/env bash
#
# retry_transient LOGFILE COMMAND [ARG...]
#
# Runs COMMAND with stdout and stderr in LOGFILE. If it fails AND the log shows a
# transient upstream-fetch error, runs it again, up to RETRY_TRANSIENT_ATTEMPTS
# times (default 3) with RETRY_TRANSIENT_DELAY * attempt seconds between them
# (default 10). Returns the last attempt's exit status; LOGFILE holds the last
# attempt's output, so a caller that tails it on failure is unchanged.
#
# cleat#3586. The scenario scripts run `docker build` and `docker compose up -d`
# once, and each pulls from somewhere that is not ours: the Go module proxy, a
# registry, a registry mirror. Five recorded failures, all before any workflow
# ran:
#
#   go mod download   stream error: stream ID N; INTERNAL_ERROR     (twice)
#   go mod download   read tcp ...: connection reset by peer
#   compose up        could not fetch content descriptor sha256:... (application/
#                     vnd.in-toto+json) from remote: not found      (twice)
#
# The registry one is not a missing image. The digests named in cleat#3586's
# comments are the CURRENT attestation-layer digests of mirror.gcr.io's
# library/postgres:16 and library/mysql:8.4 (measured 2026-10-31: the mirror
# serves both), so "not found" was the mirror failing to serve a blob that
# exists, which a second attempt can get past.
#
# RETRIED ONLY ON A MATCH, not on every failure. A compile error, a bad compose
# file or a failing `--verify-backend` is a finding about this checkout, and
# running it three times would triple the time to learn it and bury the first
# attempt's output. TRANSIENT_RE below is the whole list; the five recorded
# strings are in --self-test as known-positives, and a compile error is the
# known-negative.
#
# THE LIST GROWS ON EVIDENCE. An unseen transport error fails this run exactly as
# it did before; add its text to --self-test first, then to TRANSIENT_RE. The last
# three alternatives are not from a recorded instance -- they are the same class
# (a connection that did not complete) and are here because a pattern limited to
# the exact strings above would miss the next variant by construction.
#
# Source it, or run `scripts/lib/retry-transient.sh --self-test`.

TRANSIENT_RE='could not fetch content descriptor .* from remote: not found|stream error: stream ID [0-9]+; INTERNAL_ERROR|connection reset by peer|TLS handshake timeout|dial tcp .*i/o timeout|unexpected EOF'

retry_transient() {
  local log=$1
  shift
  local attempts=${RETRY_TRANSIENT_ATTEMPTS:-3}
  local delay=${RETRY_TRANSIENT_DELAY:-10}
  local n=1 rc
  while :; do
    "$@" >"$log" 2>&1
    rc=$?
    if (( rc == 0 )); then
      return 0
    fi
    if (( n >= attempts )) || ! grep -Eq -- "$TRANSIENT_RE" "$log"; then
      return "$rc"
    fi
    echo "==> transient upstream fetch failure on attempt $n of $attempts (exit $rc); retrying in $((delay * n))s:" >&2
    grep -Em1 -- "$TRANSIENT_RE" "$log" | cut -c1-300 >&2
    sleep $((delay * n))
    n=$((n + 1))
  done
}

# ---- self-test --------------------------------------------------------------

_retry_transient_self_test() {
  local work failures=0
  work=$(mktemp -d) || { echo "UNMEASURED: no scratch directory" >&2; return 2; }
  # shellcheck disable=SC2064  # $work is expanded now on purpose
  trap "rm -rf '$work'" RETURN

  # A command that fails with $1 on its first $2 runs, then succeeds. Its run
  # count goes in $work/count, which is what proves "retried" rather than "passed".
  cat >"$work/flaky.sh" <<'EOF'
#!/usr/bin/env bash
n=$(( $(cat "$COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$COUNT"
if (( n <= $2 )); then
  echo "$1" >&2
  exit "${FAIL_RC:-1}"
fi
echo "ok"
EOF
  chmod +x "$work/flaky.sh"
  export COUNT="$work/count"

  _case() { # name want_rc want_runs message fail_times [attempts]
    local name=$1 want_rc=$2 want_runs=$3 msg=$4 times=$5 attempts=${6:-3} rc runs said
    rm -f "$COUNT"
    said=$(RETRY_TRANSIENT_DELAY=0 RETRY_TRANSIENT_ATTEMPTS=$attempts FAIL_RC=7 \
      retry_transient "$work/log" "$work/flaky.sh" "$msg" "$times" 2>&1 >/dev/null; echo "rc=$?")
    rc=${said##*rc=}
    runs=$(cat "$COUNT" 2>/dev/null || echo 0)
    # Assert the TEXT as well as the status: with the retry disabled a
    # transient case still ends in a status, just the wrong one.
    local want_text=no
    (( want_runs > 1 )) && want_text=yes
    local got_text=no
    [[ "$said" == *"retrying in"* ]] && got_text=yes
    if [[ "$rc" == "$want_rc" && "$runs" == "$want_runs" && "$got_text" == "$want_text" ]]; then
      printf '  ok    %-52s rc=%s runs=%s\n' "$name" "$rc" "$runs"
    else
      printf '  FAIL  %-52s rc=%s (want %s) runs=%s (want %s) retry-notice=%s (want %s)\n' \
        "$name" "$rc" "$want_rc" "$runs" "$want_runs" "$got_text" "$want_text" >&2
      failures=$((failures + 1))
    fi
  }

  echo "known-positives: the five failures recorded on cleat#3586 (retried once, then pass)"
  _case "go proxy: x/tools stream error (#3576)" 0 2 \
    'go: golang.org/x/tools@v0.52.0: read "https://proxy.golang.org/golang.org/x/tools/@v/v0.52.0.zip": stream error: stream ID 301; INTERNAL_ERROR; received from peer' 1
  _case "go proxy: x/text stream error (#3582)" 0 2 \
    'go: golang.org/x/text@v0.43.0: read "https://proxy.golang.org/golang.org/x/text/@v/v0.43.0.zip": stream error: stream ID 357; INTERNAL_ERROR; received from peer' 1
  _case "go proxy: wasmtime-go connection reset (#3616)" 0 2 \
    'go: github.com/bytecodealliance/wasmtime-go/v48@v48.0.0: read "https://proxy.golang.org/github.com/bytecodealliance/wasmtime-go/v48/@v/v48.0.0.zip": read tcp 172.17.0.3:44884->34.3.0.27:443: read: connection reset by peer' 1
  _case "registry: postgres in-toto descriptor (#3585)" 0 2 \
    'failed to copy: httpReadSeeker: failed open: could not fetch content descriptor sha256:34abe417644f81fc14fdf86ef1466a6dbfd2a03aba5770c9b76015b1328e0123 (application/vnd.in-toto+json) from remote: not found' 1
  _case "registry: mysql in-toto descriptor (#3599)" 0 2 \
    'failed to copy: httpReadSeeker: failed open: could not fetch content descriptor sha256:f95cea338c517fd21f1294c91255bdd1b63d3186bacd703382e0de03131d54ef (application/vnd.in-toto+json) from remote: not found' 1

  echo "known-negatives: a finding about the checkout is NOT retried, and keeps its status"
  _case "compile error" 7 1 './cmd/cleat-worker/main.go:12:2: undefined: nope' 99
  _case "bad compose file (a plain 'not found')" 7 1 'service "cleat-worker" refers to undefined volume: not found' 99
  _case "--verify-backend refusing a no-wasmtime binary" 7 1 'wasmtime backend unavailable' 99

  echo "bounds"
  _case "transient every time: gives up after 3, keeps rc" 7 3 'connection reset by peer' 99
  _case "RETRY_TRANSIENT_ATTEMPTS=1 means no retry" 7 1 'connection reset by peer' 99 1
  _case "first attempt passes: one run, no notice" 0 1 'unused' 0

  if (( failures > 0 )); then
    echo "retry-transient self-test: $failures case(s) failed" >&2
    return 1
  fi
  echo "retry-transient self-test: all cases held"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-}" in
    --self-test) _retry_transient_self_test; exit $? ;;
    *) echo "usage: $0 --self-test   (or: source $0; retry_transient LOG CMD...)" >&2; exit 2 ;;
  esac
fi
