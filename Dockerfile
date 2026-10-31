# =============================================================================
# Stage 1: Build the cleat-worker binary
# =============================================================================
#
# Debian, not Alpine, and CGO on rather than off. Both are forced by the same
# thing: the wasmtime backend (engine/backend_wasmtime.go) is `//go:build cgo`,
# and wasmtime-go ships a prebuilt libwasmtime.a linked against glibc. Under
# musl it fails with `undefined reference to fstat64` / `ftruncate64` -- the
# LFS symbols glibc exports and musl does not.
#
# This file used to build with CGO_ENABLED=0 and describe the result as "a
# fully static binary (no libc dependency)". That was true, and it silently
# compiled out the primary backend: every container ran wazero and logged
# "wasmtime backend unavailable". wazero cannot interrupt a guest that never
# calls into the host, so a workflow with a 2-second budget ran for 2m35s and
# was reported as a success. See IMPROVEMENT-PLAN.md 2.28.
#
FROM mirror.gcr.io/library/golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Pre-cache dependencies for better layer reuse.
# The cleat/ submodule is a separate Go module (replace directive)
# and must be present for go mod download to resolve.
COPY go.mod go.sum ./
COPY cleat/go.mod cleat/go.sum cleat/
# Retried, because this is the one step in the build that depends on a server
# we do not run, and it has failed on a transport error three times with the
# code under test never reached (cleat#3586): `stream error ... INTERNAL_ERROR`
# twice and `connection reset by peer` once, each from proxy.golang.org, each on
# a different module. Every caller of this Dockerfile gets the retry -- the
# scenario scripts, `make cluster-up`, the Go tests that build the image -- which a
# wrapper around `docker build` in one script would not give them.
#
# Not retried on a match of the error text, unlike scripts/lib/retry-transient.sh:
# `go mod download` does one thing, so any failure of it is a fetch failure, and
# a text filter here could only turn an unfamiliar wording into a failed build.
# Five attempts, 5s apart growing to 20s, then the last error is the build's.
RUN attempt=1; \
    until go mod download; do \
      if [ "$attempt" -ge 5 ]; then exit 1; fi; \
      echo "go mod download failed (attempt $attempt of 5); retrying in $((attempt * 5))s" >&2; \
      sleep $((attempt * 5)); \
      attempt=$((attempt + 1)); \
    done

# Copy the full source tree (includes cmd/cleat-worker/web/dist/ for //go:embed)
COPY . .

RUN CGO_ENABLED=1 go build -o /cleat-worker ./cmd/cleat-worker

# Fail the build here rather than ship an image that silently falls back. The
# binary must actually contain the wasmtime backend -- if a future change turns
# CGO off again, or moves to a musl base, this is where it stops.
RUN /cleat-worker --verify-backend

# =============================================================================
# Stage 2: Runtime image
# =============================================================================
FROM mirror.gcr.io/library/debian:bookworm-slim

# ca-certificates: HTTPS outbound from durable HTTP calls
# wget:            used by docker-compose healthcheck
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates wget \
    && rm -rf /var/lib/apt/lists/*

# Create a non-root user
RUN groupadd -r cleat && useradd -r -g cleat cleat

COPY --from=builder /cleat-worker /usr/local/bin/cleat-worker
COPY --from=builder /app/migrations /migrations

USER cleat

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/cleat-worker"]
