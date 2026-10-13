# Multi-stage build for SS7 3EAI Gateway
# Based on Alpine Linux for minimal image size

# Stage 1: Build
FROM golang:1.24-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    gcc \
    musl-dev \
    linux-headers

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application. VERSION is passed by the release workflow (the tag); local builds
# fall back to git describe, then to "dev".
ARG VERSION=""
RUN V="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
    CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo \
    -ldflags="-w -s -X main.AppVersion=${V}" \
    -o ss7-gateway \
    ./cmd/gateway

# Licence texts (and NOTICE files) of every third-party module compiled into the binary, for
# redistribution of the image (Apache-2.0 §4(d), MIT/BSD notice clauses, MPL-2.0 source offer).
# The gateway's own packages are skipped (its LICENSE/NOTICE are copied below); the trailing
# slash keeps the Apache-2.0 client module, which is compiled in, in the saved set.
RUN go install github.com/google/go-licenses/v2@v2.0.1 && \
    go-licenses save ./cmd/gateway --save_path=/build/third_party_licenses \
        --ignore github.com/3eAI-Labs/ss7-3eai-gateway/

# Stage 2: Runtime
FROM alpine:3.19

LABEL org.opencontainers.image.title="ss7-3eai-gateway" \
      org.opencontainers.image.vendor="3eAI Labs Ltd" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/3eAI-Labs/ss7-3eai-gateway"

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata

# Create non-root user
RUN addgroup -g 1000 ss7 && \
    adduser -D -u 1000 -G ss7 ss7

# Create data directories
RUN mkdir -p /var/lib/ss7-gateway && \
    chown -R ss7:ss7 /var/lib/ss7-gateway

# Copy binary from builder
COPY --from=builder /build/ss7-gateway /usr/local/bin/

# Licences: the gateway's own, and those of the modules compiled into it
COPY LICENSE NOTICE /usr/share/licenses/ss7-3eai-gateway/
COPY --from=builder /build/third_party_licenses /usr/share/licenses/ss7-3eai-gateway/third_party/

# Copy default configuration
COPY config.example.yaml /etc/ss7-gateway/config.yaml

# Set ownership
RUN chown -R ss7:ss7 /etc/ss7-gateway

# Switch to non-root user
USER ss7

# Expose ports
# 2905: M3UA (SCTP)
# 8080: REST API
EXPOSE 2905 8080

# Set default environment variables
ENV LOG_LEVEL=info
ENV CONFIG_FILE=/etc/ss7-gateway/config.yaml

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/api/v1/health || exit 1

# Run the gateway
ENTRYPOINT ["/usr/local/bin/ss7-gateway"]
CMD ["-config", "/etc/ss7-gateway/config.yaml"]
