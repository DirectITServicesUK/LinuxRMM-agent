# RMM Agent Makefile
#
# Build and distribution targets for the RMM agent.
# Supports static binary builds with embedded version information.
#
# Usage:
#   make build          - Build the agent binary
#   make test           - Run tests
#   make lint           - Run linters
#   make clean          - Remove build artifacts
#   make install        - Run the installation script
#   make dist-package   - Create distribution tarball

# Version information (can be overridden)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Build settings
BINARY_NAME = rmm-agent
BUILD_DIR = build
DIST_DIR = dist

# Go build flags for static binary with version info
# -trimpath removes build machine paths from binaries for cleaner logs
GOFLAGS = -trimpath
LDFLAGS = -ldflags "-s -w \
	-X github.com/doughall/linuxrmm/agent/internal/version.Version=$(VERSION) \
	-X github.com/doughall/linuxrmm/agent/internal/version.Commit=$(COMMIT) \
	-X github.com/doughall/linuxrmm/agent/internal/version.BuildTime=$(BUILD_TIME)"

# CGO disabled for static binary
export CGO_ENABLED=0

# Default target
.DEFAULT_GOAL := build

# Ensure build directories exist
$(BUILD_DIR) $(DIST_DIR):
	mkdir -p $@

# Build the agent binary
.PHONY: build
build: $(BUILD_DIR)
	go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/rmm-agent
	@echo "Built $(BUILD_DIR)/$(BINARY_NAME)"

# Build for specific Linux targets
.PHONY: build-linux-amd64
build-linux-amd64: $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/rmm-agent
	@echo "Built $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64"

.PHONY: build-linux-arm64
build-linux-arm64: $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/rmm-agent
	@echo "Built $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64"

# Build all Linux architectures
.PHONY: build-all
build-all: build-linux-amd64 build-linux-arm64

# Build the helper binary
.PHONY: build-helper
build-helper: $(BUILD_DIR)
	go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/rmm-agent-helper ./cmd/rmm-agent-helper
	@echo "Built $(BUILD_DIR)/rmm-agent-helper"

# Build helper for specific Linux targets
.PHONY: build-helper-linux-amd64
build-helper-linux-amd64: $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/rmm-agent-helper-linux-amd64 ./cmd/rmm-agent-helper
	@echo "Built $(BUILD_DIR)/rmm-agent-helper-linux-amd64"

.PHONY: build-helper-linux-arm64
build-helper-linux-arm64: $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) $(LDFLAGS) -o $(BUILD_DIR)/rmm-agent-helper-linux-arm64 ./cmd/rmm-agent-helper
	@echo "Built $(BUILD_DIR)/rmm-agent-helper-linux-arm64"

# Build all binaries (agent + helper)
.PHONY: build-all-binaries
build-all-binaries: build-linux-amd64 build-linux-arm64 build-helper-linux-amd64 build-helper-linux-arm64

# Run tests
.PHONY: test
test:
	go test -v -race ./...

# Run tests with coverage
.PHONY: test-coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run linters
.PHONY: lint
lint:
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed" && exit 1)
	golangci-lint run ./...

# Format code
.PHONY: fmt
fmt:
	go fmt ./...

# Verify dependencies
.PHONY: verify
verify:
	go mod verify
	go mod tidy
	go vet ./...

# Clean build artifacts
.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(DIST_DIR)
	rm -f coverage.out coverage.html
	@echo "Cleaned build artifacts"

# Install using the installation script
# NOTE: Requires sudo and --group/--server-url arguments
.PHONY: install
install: build
	@echo "Running installation script..."
	@echo "Usage: sudo make install GROUP=<group> SERVER_URL=<url>"
	@if [ -z "$(GROUP)" ] || [ -z "$(SERVER_URL)" ]; then \
		echo "Error: GROUP and SERVER_URL are required"; \
		echo "Example: sudo make install GROUP=production SERVER_URL=https://rmm.example.com"; \
		exit 1; \
	fi
	./scripts/install.sh --group=$(GROUP) --server-url=$(SERVER_URL) --binary=$(BUILD_DIR)/$(BINARY_NAME)

# Create distribution package (tarball with binary, scripts, service file)
.PHONY: dist-package
dist-package: build-all-binaries $(DIST_DIR)
	@echo "Creating distribution package..."
	# Create package directory structure
	mkdir -p $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)
	# Copy files
	cp $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp $(BUILD_DIR)/rmm-agent-helper-linux-amd64 $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp $(BUILD_DIR)/rmm-agent-helper-linux-arm64 $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp scripts/install.sh $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp systemd/rmm-agent.service $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	cp configs/config.yaml.example $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/
	# Create README for the package
	echo "# RMM Agent $(VERSION)" > $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/README.txt
	echo "" >> $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/README.txt
	echo "Installation:" >> $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/README.txt
	echo "  sudo ./install.sh --group=<GROUP> --server-url=<URL>" >> $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)/README.txt
	# Create tarball
	cd $(DIST_DIR) && tar -czvf $(BINARY_NAME)-$(VERSION).tar.gz $(BINARY_NAME)-$(VERSION)
	rm -rf $(DIST_DIR)/$(BINARY_NAME)-$(VERSION)
	@echo "Created $(DIST_DIR)/$(BINARY_NAME)-$(VERSION).tar.gz"

# Version information
.PHONY: version
version:
	@echo "Version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build Time: $(BUILD_TIME)"

# Help
.PHONY: help
help:
	@echo "RMM Agent Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build          - Build the agent binary (default)"
	@echo "  build-all      - Build for all Linux architectures"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage report"
	@echo "  lint           - Run golangci-lint"
	@echo "  fmt            - Format code with go fmt"
	@echo "  verify         - Verify module dependencies"
	@echo "  clean          - Remove build artifacts"
	@echo "  install        - Install using install.sh (requires sudo)"
	@echo "  dist-package   - Create distribution tarball"
	@echo "  version        - Show version information"
	@echo ""
	@echo "Variables:"
	@echo "  VERSION    - Version string (default: git describe)"
	@echo "  GROUP      - Agent group (for install target)"
	@echo "  SERVER_URL - Server URL (for install target)"
