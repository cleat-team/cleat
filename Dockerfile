# =============================================================================
# Stage 1: Build the cleat-worker binary
# =============================================================================
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Pre-cache dependencies for better layer reuse.
# The cleat/ submodule is a separate Go module (replace directive)
# and must be present for go mod download to resolve.
COPY go.mod go.sum ./
COPY cleat/go.mod cleat/go.sum cleat/
RUN go mod download

# Copy the full source tree (includes cmd/cleat-worker/web/dist/ for //go:embed)
COPY . .

# Build a fully static binary (no libc dependency)
RUN CGO_ENABLED=0 go build -o /cleat-worker ./cmd/cleat-worker

# =============================================================================
# Stage 2: Minimal runtime image
# =============================================================================
FROM alpine:3.20

# ca-certificates: HTTPS outbound from durable HTTP calls
# wget:            used by docker-compose healthcheck
RUN apk add --no-cache ca-certificates wget

# Create a non-root user
RUN addgroup -S cleat && adduser -S -G cleat cleat

COPY --from=builder /cleat-worker /usr/local/bin/cleat-worker
COPY --from=builder /app/migrations /migrations

USER cleat

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/cleat-worker"]
