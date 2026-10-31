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
# two alternatives are not from a recorded instance -- they are the same class
# (a connection that did not complete) and are here because a pattern limited to
# the exact strings above would miss the next variant by construction.
#
# A BARE `unexpected EOF` WAS HERE AND WAS WRONG. It is the Go compiler's own
# message for a truncated file ("syntax error: unexpected EOF, expected }"), so a
# real compile error ran three times: three `docker build`s and 30s of sleep
# (cleat#3644 review, measured). Do not add a phrase a compiler, a linter or a
# test can print; anchor it to the fetch it belongs to.
#
# WHAT PART OF THE LOG DECIDES. Only what comes after the LAST
# `go mod download: ok after N attempt(s)` line the Dockerfile prints (that RUN
# retries internally), and then only its last RETRY_TRANSIENT_WINDOW (40) lines.
#
# The marker is what does the work. A fetch error the Dockerfile recovered from
# stays in the build log after the build went on, and a later ordinary failure --
# a compile error -- would be read as transient because of it. A window alone does
# not stop that: in a real BuildKit log the recovered error is 22 lines from the
# end, because between it and the end sit the next COPY, the failing RUN and
# BuildKit's error summary with its Dockerfile excerpt (cleat#3644 review, built
# and measured; an earlier version of this comment and of the self-test claimed
# the window covered it, on a fixture padded with 60 lines no real build has).
# The window stays for the other direction: the callers print `tail -25`/`-30`
# of this log on failure, so the cause is in that tail.
#
# Without the marker -- compose, or any command that is not this Dockerfile --
# the window is all there is, and a recovered error inside it counts.
#
# Source it, or run `scripts/lib/retry-transient.sh --self-test`.

TRANSIENT_RE='could not fetch content descriptor .* from remote: not found|stream error: stream ID [0-9]+; INTERNAL_ERROR|connection reset by peer|TLS handshake timeout|dial tcp .*i/o timeout'

TRANSIENT_MARKER_RE='go mod download: ok after [0-9]+ attempt'

# The part of LOG that decides: after its last marker, if it has one, then the
# last WINDOW lines of that.
_retry_transient_deciding_text() {
  local log=$1 window=$2 last
  last=$(grep -nE -- "$TRANSIENT_MARKER_RE" "$log" | tail -n 1 | cut -d: -f1)
  if [[ -n "$last" ]]; then
    tail -n +"$((last + 1))" "$log" | tail -n "$window"
  else
    tail -n "$window" "$log"
  fi
}

retry_transient() {
  local log=$1
  shift
  local attempts=${RETRY_TRANSIENT_ATTEMPTS:-3}
  local delay=${RETRY_TRANSIENT_DELAY:-10}
  local window=${RETRY_TRANSIENT_WINDOW:-40}
  local n=1 rc
  while :; do
    "$@" >"$log" 2>&1
    rc=$?
    if (( rc == 0 )); then
      return 0
    fi
    if (( n >= attempts )) || ! _retry_transient_deciding_text "$log" "$window" | grep -Eq -- "$TRANSIENT_RE"; then
      return "$rc"
    fi
    echo "==> transient upstream fetch failure on attempt $n of $attempts (exit $rc); retrying in $((delay * n))s:" >&2
    _retry_transient_deciding_text "$log" "$window" | grep -Em1 -- "$TRANSIENT_RE" | cut -c1-300 >&2
    sleep $((delay * n))
    n=$((n + 1))
  done
}

# ---- self-test --------------------------------------------------------------

_retry_transient_self_test() {
  local work failures=0 ROOT
  ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
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
  # The compiler's own message for a truncated file contains "unexpected EOF".
  # An earlier TRANSIENT_RE matched that bare phrase and ran a real compile error
  # three times (found in review of cleat#3644); "undefined: nope" above could
  # not see it because it does not contain the phrase.
  _case "compile error: go's own 'unexpected EOF' (truncated file)" 7 1 \
    './main.go:5:1: syntax error: unexpected EOF, expected }' 99

  echo "a recovered go mod download flake, in a REAL BuildKit log, is not a reason to retry what fails after it"
  # The log below is real: a probe image whose RUN is the Dockerfile's `go mod
  # download` retry, verbatim, over a stub `go` that fails that step once with
  # cleat#3582's exact text and then succeeds, followed by a build that fails to
  # compile; `docker build --progress=plain`, 2026-10-31. The recovered flake is
  # 22 lines from the end -- inside the window -- because between it and the end
  # sit the COPY step, the failing RUN, and BuildKit's error summary with its
  # Dockerfile excerpt. An earlier version of this case padded the gap with 60
  # filler lines, which is a gap no real build has (found in review of #3644).
  local real_log
  real_log=$(cat <<'REAL_BUILDKIT_LOG'
#0 building with "orbstack" instance using docker driver

#1 [internal] load build definition from Dockerfile.probe
#1 transferring dockerfile: 588B done
#1 DONE 0.0s

#2 [internal] load metadata for mirror.gcr.io/library/busybox:latest
#2 DONE 0.5s

#3 [internal] load .dockerignore
#3 transferring context: 2B done
#3 DONE 0.0s

#4 [1/8] FROM mirror.gcr.io/library/busybox:latest@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4
#4 resolve mirror.gcr.io/library/busybox:latest@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4 0.0s done
#4 CACHED

#5 [internal] load build context
#5 transferring context: 870B done
#5 DONE 0.0s

#6 [2/8] COPY stub/go /usr/local/bin/go
#6 DONE 0.1s

#7 [3/8] WORKDIR /app
#7 DONE 0.0s

#8 [4/8] COPY go.mod go.sum ./
#8 DONE 0.0s

#9 [5/8] COPY cleat/go.mod cleat/go.sum cleat/
#9 DONE 0.1s

#10 [6/8] RUN attempt=1;     until go mod download; do       if [ "$attempt" -ge 5 ]; then exit 1; fi;       echo "go mod download failed (attempt $attempt of 5); retrying in $((attempt * 5))s" >&2;       sleep $((attempt * 5));       attempt=$((attempt + 1));     done;     echo "go mod download: ok after $attempt attempt(s)"
#10 0.226 go: golang.org/x/text@v0.43.0: read "https://proxy.golang.org/golang.org/x/text/@v/v0.43.0.zip": stream error: stream ID 357; INTERNAL_ERROR; received from peer
#10 0.226 go mod download failed (attempt 1 of 5); retrying in 5s
#10 5.231 go mod download: ok after 2 attempt(s)
#10 DONE 5.3s

#11 [7/8] COPY . .
#11 DONE 0.1s

#12 [8/8] RUN CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker
#12 0.138 ./cmd/cleat-worker/main.go:12:2: undefined: nope
#12 ERROR: process "/bin/sh -c CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker" did not complete successfully: exit code: 1
------
 > [8/8] RUN CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker:
0.138 ./cmd/cleat-worker/main.go:12:2: undefined: nope
------
Dockerfile.probe:15
--------------------
  13 |         echo "go mod download: ok after $attempt attempt(s)"
  14 |     COPY . .
  15 | >>> RUN CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker
  16 |     
--------------------
ERROR: failed to build: failed to solve: process "/bin/sh -c CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker" did not complete successfully: exit code: 1
REAL_BUILDKIT_LOG
)
  _case "real log: recovered flake, marker, then a compile error" 7 1 "$real_log" 99
  # The same log with a transient error AFTER the marker is still retried: the
  # marker narrows the decision, it does not switch the helper off.
  local after_marker
  after_marker=$(printf '%s\n' "$real_log" | sed -n '1,/go mod download: ok after/p'; echo '#12 0.4 go: github.com/x/y@v1.0.0: read "https://proxy.golang.org/x.zip": connection reset by peer')
  _case "a transient error after the marker is still retried" 0 2 "$after_marker" 1
  # The marker is the Dockerfile's, and the helper reads it: if the two drift the
  # case above goes back to retrying, so check they are the same string.
  # shellcheck disable=SC2016  # the Dockerfile's text has a literal $attempt
  if grep -qF 'echo "go mod download: ok after $attempt attempt(s)"' "$ROOT/Dockerfile" \
     && printf '%s\n' 'go mod download: ok after 2 attempt(s)' | grep -Eq -- "$TRANSIENT_MARKER_RE"; then
    printf '  ok    %-52s\n' "the Dockerfile's marker and TRANSIENT_MARKER_RE agree"
  else
    printf '  FAIL  %-52s\n' "the Dockerfile's marker and TRANSIENT_MARKER_RE agree" >&2
    failures=$((failures + 1))
  fi

  echo "window: only the tail of the log is read"
  local noise early_flake
  noise=$(for i in $(seq 1 60); do echo "#12 [builder 7/9] step output line $i"; done)
  early_flake='go: golang.org/x/text@v0.43.0: read "https://proxy.golang.org/x.zip": stream error: stream ID 357; INTERNAL_ERROR; received from peer'
  _case "the transient error is at the end of a long log" 0 2 \
    "$noise"$'\n'"$early_flake" 1

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
