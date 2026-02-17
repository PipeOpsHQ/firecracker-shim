# Makefile for fc-cri (Firecracker CRI Runtime)

BINARY_DIR := bin
INSTALL_DIR := /usr/local/bin
CONFIG_DIR := /etc/fc-cri
LIB_DIR := /var/lib/fc-cri
FSIFY_VERSION := v0.0.7

# Go build settings
GO := go
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Binaries
SHIM_BINARY := $(BINARY_DIR)/containerd-shim-fc-v2
AGENT_BINARY := $(BINARY_DIR)/fc-agent
FCCTL_BINARY := $(BINARY_DIR)/fcctl

# Detect package manager
PKG_MANAGER := $(shell \
	if command -v pacman > /dev/null 2>&1; then echo "pacman"; \
	elif command -v apt-get > /dev/null 2>&1; then echo "apt"; \
	elif command -v dnf > /dev/null 2>&1; then echo "dnf"; \
	elif command -v brew > /dev/null 2>&1; then echo "brew"; \
	else echo "unknown"; fi)

.PHONY: all build shim agent fcctl install clean test lint proto deps fsify

all: build

build: shim agent fcctl

# Build the containerd shim
shim:
	@echo "Building containerd-shim-fc-v2..."
	@mkdir -p $(BINARY_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(SHIM_BINARY) ./cmd/containerd-shim-fc-v2

# Build the guest agent (static binary for VM)
agent:
	@echo "Building fc-agent..."
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(AGENT_BINARY) ./cmd/fc-agent

# Build the debug CLI
fcctl:
	@echo "Building fcctl..."
	@mkdir -p $(BINARY_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(FCCTL_BINARY) ./cmd/fcctl

# Install dependencies (fsify, skopeo, umoci)
deps:
	@echo "Detected package manager: $(PKG_MANAGER)"
	@echo "Installing dependencies..."
	@# --- skopeo ---
	@which skopeo > /dev/null 2>&1 || ( \
		echo "Installing skopeo..." && \
		if [ "$(PKG_MANAGER)" = "pacman" ]; then sudo pacman -S --noconfirm skopeo; \
		elif [ "$(PKG_MANAGER)" = "apt" ]; then sudo apt-get install -y skopeo; \
		elif [ "$(PKG_MANAGER)" = "dnf" ]; then sudo dnf install -y skopeo; \
		elif [ "$(PKG_MANAGER)" = "brew" ]; then brew install skopeo; \
		else echo "ERROR: Could not detect package manager. Install skopeo manually." && exit 1; fi \
	)
	@# --- umoci ---
	@which umoci > /dev/null 2>&1 || ( \
		echo "Installing umoci..." && \
		if [ "$(PKG_MANAGER)" = "pacman" ]; then \
			echo "umoci not in official repos, installing from AUR or Go..." && \
			if command -v yay > /dev/null 2>&1; then yay -S --noconfirm umoci; \
			elif command -v paru > /dev/null 2>&1; then paru -S --noconfirm umoci; \
			else GO111MODULE=on go install github.com/opencontainers/umoci/cmd/umoci@latest; fi; \
		elif [ "$(PKG_MANAGER)" = "apt" ]; then sudo apt-get install -y umoci; \
		elif [ "$(PKG_MANAGER)" = "dnf" ]; then sudo dnf install -y umoci; \
		elif [ "$(PKG_MANAGER)" = "brew" ]; then GO111MODULE=on go install github.com/opencontainers/umoci/cmd/umoci@latest; \
		else GO111MODULE=on go install github.com/opencontainers/umoci/cmd/umoci@latest; fi \
	)
	@# --- containerd ---
	@which containerd > /dev/null 2>&1 || ( \
		echo "Installing containerd..." && \
		if [ "$(PKG_MANAGER)" = "pacman" ]; then sudo pacman -S --noconfirm containerd; \
		elif [ "$(PKG_MANAGER)" = "apt" ]; then sudo apt-get install -y containerd; \
		elif [ "$(PKG_MANAGER)" = "dnf" ]; then sudo dnf install -y containerd; \
		else echo "ERROR: Install containerd manually." && exit 1; fi \
	)
	@# --- runc ---
	@which runc > /dev/null 2>&1 || ( \
		echo "Installing runc..." && \
		if [ "$(PKG_MANAGER)" = "pacman" ]; then sudo pacman -S --noconfirm runc; \
		elif [ "$(PKG_MANAGER)" = "apt" ]; then sudo apt-get install -y runc; \
		elif [ "$(PKG_MANAGER)" = "dnf" ]; then sudo dnf install -y runc; \
		else echo "ERROR: Install runc manually." && exit 1; fi \
	)
	@# --- CNI plugins ---
	@test -d /opt/cni/bin/ || ( \
		echo "Installing CNI plugins..." && \
		if [ "$(PKG_MANAGER)" = "pacman" ]; then sudo pacman -S --noconfirm cni-plugins; \
		elif [ "$(PKG_MANAGER)" = "apt" ]; then sudo apt-get install -y containernetworking-plugins; \
		elif [ "$(PKG_MANAGER)" = "dnf" ]; then sudo dnf install -y containernetworking-plugins; \
		else echo "ERROR: Install CNI plugins manually." && exit 1; fi \
	)
	@# --- fsify ---
	@which fsify > /dev/null 2>&1 || $(MAKE) fsify
	@echo "Dependencies installed!"

# Install fsify for OCI to block device conversion
fsify:
	@echo "Installing fsify $(FSIFY_VERSION)..."
	@mkdir -p /tmp/fsify-install
	@cd /tmp/fsify-install && \
		curl -sLO https://github.com/volantvm/fsify/releases/download/$(FSIFY_VERSION)/fsify-linux-amd64 && \
		chmod +x fsify-linux-amd64 && \
		sudo mv fsify-linux-amd64 $(INSTALL_DIR)/fsify
	@rm -rf /tmp/fsify-install
	@echo "fsify installed to $(INSTALL_DIR)/fsify"

# Install binaries and configuration
install: build deps
	@echo "Installing fc-cri..."
	# Create directories
	sudo mkdir -p $(INSTALL_DIR)
	sudo mkdir -p $(CONFIG_DIR)
	sudo mkdir -p $(LIB_DIR)/images
	sudo mkdir -p $(LIB_DIR)/rootfs
	sudo mkdir -p /run/fc-cri

	# Install binaries
	sudo install -m 755 $(SHIM_BINARY) $(INSTALL_DIR)/
	sudo install -m 755 $(AGENT_BINARY) $(LIB_DIR)/
	sudo install -m 755 $(FCCTL_BINARY) $(INSTALL_DIR)/

	# Install default configuration
	sudo install -m 644 config/fc-cri.toml $(CONFIG_DIR)/config.toml
	sudo install -m 644 config/containerd-fc.toml /etc/containerd/config.d/firecracker.toml

	@echo "Installation complete!"
	@echo "Next steps:"
	@echo "  1. Build and install the kernel: cd kernel && ./build.sh"
	@echo "  2. Create a base rootfs: make rootfs"
	@echo "  3. Restart containerd: sudo systemctl restart containerd"

# Uninstall
uninstall:
	@echo "Uninstalling fc-cri..."
	sudo rm -f $(INSTALL_DIR)/containerd-shim-fc-v2
	sudo rm -f $(INSTALL_DIR)/fcctl
	sudo rm -f $(LIB_DIR)/fc-agent
	sudo rm -f $(CONFIG_DIR)/config.toml
	sudo rm -f /etc/containerd/config.d/firecracker.toml

# Build kernel
kernel:
	@echo "Building minimal kernel..."
	cd kernel && bash build.sh

# Create base rootfs
rootfs:
	@echo "Creating base rootfs..."
	sudo ./scripts/create-rootfs.sh

# Convert an OCI image to rootfs using fsify
# Usage: make convert-image IMAGE=nginx:latest
convert-image:
	@if [ -z "$(IMAGE)" ]; then echo "Usage: make convert-image IMAGE=nginx:latest"; exit 1; fi
	@echo "Converting $(IMAGE) to rootfs..."
	sudo fsify -v -fs ext4 -s 50 -o $(LIB_DIR)/rootfs/$(subst /,-,$(subst :,-,$(IMAGE))).img $(IMAGE)

# Run tests
test:
	@echo "Running tests..."
	$(GO) test -v -race ./...

# Run linter
lint:
	@echo "Running linter..."
	golangci-lint run ./...

# Format code
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...
	gofumpt -w .

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BINARY_DIR)
	rm -rf /tmp/kernel-build

# Development helpers
dev-install: build
	sudo install -m 755 $(SHIM_BINARY) $(INSTALL_DIR)/
	sudo systemctl restart containerd

# Run integration tests
integration-test:
	@echo "Running integration tests..."
	./scripts/integration-test.sh

# Generate protobuf code (if needed)
proto:
	@echo "Generating protobuf code..."
	protoc --go_out=. --go-grpc_out=. api/agent.proto

# Build documentation site
site:
	@echo "Building documentation site..."
	pip install mkdocs-material
	cp README.md docs/index.md
	mkdocs build

# Serve documentation site locally
site-serve:
	@echo "Serving documentation site..."
	pip install mkdocs-material
	cp README.md docs/index.md
	mkdocs serve

# Show help
help:
	@echo "fc-cri Makefile"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all            - Build all binaries (default)"
	@echo "  build          - Build shim, agent, and fcctl"
	@echo "  shim           - Build containerd shim"
	@echo "  agent          - Build guest agent"
	@echo "  fcctl          - Build debug CLI"
	@echo "  install        - Install binaries and configuration"
	@echo "  uninstall      - Remove installed files"
	@echo "  deps           - Install dependencies (fsify, skopeo, umoci)"
	@echo "  fsify          - Install fsify binary"
	@echo "  kernel         - Build minimal Linux kernel"
	@echo "  rootfs         - Create base rootfs image"
	@echo "  convert-image  - Convert OCI image to rootfs (IMAGE=nginx:latest)"
	@echo "  test           - Run unit tests"
	@echo "  lint           - Run linter"
	@echo "  fmt            - Format code"
	@echo "  clean          - Remove build artifacts"
	@echo "  site           - Build documentation site"
	@echo "  site-serve     - Serve documentation site locally"
	@echo "  dev-install    - Quick install for development"
	@echo "  help           - Show this help"
