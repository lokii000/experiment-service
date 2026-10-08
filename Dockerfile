
# Stage 1: Build Go application
FROM golang:1.27.1-alpine3.24 AS builder

WORKDIR /src

# Copy Go dependency files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy application source code
COPY cmd ./cmd
COPY internal ./internal

# Build Go binary with PostgreSQL support
RUN CGO_ENABLED=0 GOOS=linux \
    go build -tags pgx -trimpath -ldflags="-s -w" \
    -o /out/api ./cmd/api


# Stage 2: Production runtime
FROM alpine:3.24

# Install CA certificates and create non-root user
RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S app -G app

WORKDIR /app

# Copy compiled binary
COPY --from=builder --chown=app:app \
    /out/api /app/api

# Copy initial experiment configuration
COPY --chown=app:app \
    config/experiments.json /app/config/experiments.json

# Application environment
ENV PORT=8080 \
    CONFIG_PATH=/app/config/experiments.json

# Run as non-root
USER app

EXPOSE 8080

# Container health check
HEALTHCHECK --interval=30s \
    --timeout=3s \
    --start-period=10s \
    --retries=3 \
    CMD wget -q -O /dev/null \
    http://127.0.0.1:8080/healthz || exit 1

# Start application
ENTRYPOINT ["/app/api"]
