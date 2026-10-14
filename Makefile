.PHONY: all build clean test lint docker-build docker-push run fmt vet test-client licenses export-check ci-local ci-local-dirty test-e2e

# Build variables
BINARY_NAME=ss7-gateway
VERSION?=$(shell git describe --tags --always --dirty)
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.AppVersion=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

# Docker variables
DOCKER_REPO?=ghcr.io/3eai-labs
DOCKER_IMAGE=$(DOCKER_REPO)/ss7-3eai-gateway
DOCKER_TAG?=$(VERSION)

all: fmt vet build

build:
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	@CGO_ENABLED=1 go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/gateway

build-linux:
	@echo "Building $(BINARY_NAME) for Linux..."
	@CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/gateway

clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@go clean

test:
	@echo "Running tests..."
	@go test -v -race -coverprofile=coverage.out ./...

test-coverage:
	@echo "Generating coverage report..."
	@go test -v -race -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

lint:
	@echo "Running linters..."
	@golangci-lint run ./...

fmt:
	@echo "Formatting code..."
	@go fmt ./...

vet:
	@echo "Vetting code..."
	@go vet ./...

tidy:
	@echo "Tidying modules..."
	@go mod tidy

download:
	@echo "Downloading dependencies..."
	@go mod download

docker-build:
	@echo "Building Docker image $(DOCKER_IMAGE):$(DOCKER_TAG)..."
	@docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	@docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest

docker-push:
	@echo "Pushing Docker image $(DOCKER_IMAGE):$(DOCKER_TAG)..."
	@docker push $(DOCKER_IMAGE):$(DOCKER_TAG)
	@docker push $(DOCKER_IMAGE):latest

docker-run:
	@echo "Running Docker container..."
	@docker run -it --rm \
		-p 2905:2905 \
		-p 8080:8080 \
		-v $(PWD)/config.yaml:/etc/ss7-gateway/config.yaml \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

run:
	@echo "Running $(BINARY_NAME)..."
	@go run ./cmd/gateway -config config.yaml -log-level info

run-debug:
	@echo "Running $(BINARY_NAME) in debug mode..."
	@go run ./cmd/gateway -config config.yaml -log-level debug

install-tools:
	@echo "Installing development tools..."
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@go install golang.org/x/tools/cmd/goimports@latest

# E2E: full SS7 wire round trip against an in-process HLR peer.
# Drives NATS → MAP encode → TCAP BEGIN → SCCP UDT → M3UA → SCTP loopback
# → ss7peer (HLR sim) → ReturnResult → reverse path → NATS response.
# Build tag keeps it out of `go test ./...`; SCTP requires Linux kernel
# support, so this target is gated to ubuntu CI runners (see .github/workflows/ci.yml).
#
# The GitHub Actions gate (ci.yml, internal.yml) run locally through nektos/act —
# the Actions allowance is the binding constraint. See scripts/ci-local.sh.
ci-local:
	@scripts/ci-local.sh

ci-local-dirty:
	@scripts/ci-local.sh --dirty

# With -race: the M3UA library whose own races kept it off is gone
# (internal/m3ua/wire), and running with it found a race of ours that the
# unit tests did not reach.
test-e2e:
	@echo "Running E2E SS7 integration test..."
	@go test -tags integration -race -count=1 -timeout 600s ./cmd/gateway/ ./internal/m3ua/

# Client module (Apache-2.0): its own go.mod, so `go test ./...` at the root skips it.
test-client:
	@cd client && go vet ./... && go test -race -count=1 ./...

# Licence headers and dependency licences (ADR-007).
licenses:
	@scripts/check-licenses.sh

# Dry-run both public exports (allowlist + leak guard). Writes to a temp dir and removes it.
export-check:
	@d="$$(mktemp -d)"; trap 'rm -rf "$$d"' EXIT; \
	scripts/export-public.sh client "$$d/client" && \
	EXPORT_ALLOW_LOCAL_REPLACE=1 scripts/export-public.sh server "$$d/server"

build-load-simulator:
	@echo "Building NATS load simulator..."
	@cd test/integration && go build -o nats-load-simulator nats_load_simulator.go

test-load-simulator:
	@echo "Running NATS load test (60 seconds, both mode, 10 msg/s)..."
	@cd test/integration && ./nats-load-simulator -mode both -rate 10 -duration 60

test-load-simulator-stress:
	@echo "Running NATS stress test (300 seconds, both mode, 100 msg/s)..."
	@cd test/integration && ./nats-load-simulator -mode both -rate 100 -duration 300

test-all: test build-load-simulator test-load-simulator test-e2e
	@echo "All tests completed!"

help:
	@echo "Available targets:"
	@echo "  build              - Build the binary"
	@echo "  build-linux        - Build Linux binary"
	@echo "  clean              - Remove build artifacts"
	@echo "  test               - Run unit tests"
	@echo "  test-coverage      - Run tests with coverage report"
	@echo "  test-e2e           - Run E2E SS7 round trip (in-process HLR peer)"
	@echo "  build-load-simulator    - Build NATS load simulator"
	@echo "  test-load-simulator     - Run NATS load test (60s, 10 msg/s)"
	@echo "  test-load-simulator-stress - NATS stress test (300s, 100 msg/s)"
	@echo "  test-all           - Run all tests"
	@echo "  lint               - Run linters"
	@echo "  fmt                - Format code"
	@echo "  vet                - Vet code"
	@echo "  tidy               - Tidy modules"
	@echo "  download           - Download dependencies"
	@echo "  docker-build       - Build Docker image"
	@echo "  docker-push        - Push Docker image"
	@echo "  docker-run         - Run Docker container"
	@echo "  run                - Run the gateway locally"
	@echo "  run-debug          - Run the gateway in debug mode"
	@echo "  install-tools      - Install development tools"
	@echo "  help               - Show this help message"
