#!/bin/bash
# Build minimal Linux kernel for Firecracker
#
# Usage: ./build.sh [kernel_version]
# Example: ./build.sh 6.6.10

set -euo pipefail

KERNEL_VERSION="${1:-6.6.10}"
KERNEL_MAJOR=$(echo "$KERNEL_VERSION" | cut -d. -f1)
BUILD_DIR="/tmp/kernel-build"
OUTPUT_DIR="${OUTPUT_DIR:-/var/lib/fc-cri}"

echo "=== Building Linux Kernel $KERNEL_VERSION for Firecracker ==="

# Create build directory
mkdir -p "$BUILD_DIR"
cd "$BUILD_DIR"

# Download kernel source
if [[ ! -f "linux-$KERNEL_VERSION.tar.xz" ]]; then
    echo "Downloading kernel source..."
    wget "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_MAJOR}.x/linux-$KERNEL_VERSION.tar.xz"
fi

# Extract
if [[ ! -d "linux-$KERNEL_VERSION" ]]; then
    echo "Extracting kernel source..."
    tar xf "linux-$KERNEL_VERSION.tar.xz"
fi

cd "linux-$KERNEL_VERSION"

# Copy minimal config
SCRIPT_DIR="$(dirname "$(realpath "$0")")"
cp "$SCRIPT_DIR/config-minimal" .config

# Update config with defaults
echo "Configuring kernel..."
make olddefconfig

# Build vmlinux
echo "Building kernel (this may take a while)..."
make vmlinux -j"$(nproc)"

# Check size
VMLINUX_SIZE=$(stat -c%s vmlinux)
VMLINUX_SIZE_MB=$((VMLINUX_SIZE / 1024 / 1024))
echo "Kernel size: ${VMLINUX_SIZE_MB}MB"

# Copy to output
mkdir -p "$OUTPUT_DIR"
cp vmlinux "$OUTPUT_DIR/vmlinux"
echo "Kernel installed to $OUTPUT_DIR/vmlinux"

# Optionally strip (reduces size further but removes symbols)
if command -v strip &> /dev/null; then
    cp vmlinux "$OUTPUT_DIR/vmlinux-stripped"
    strip --strip-all "$OUTPUT_DIR/vmlinux-stripped"
    STRIPPED_SIZE=$(stat -c%s "$OUTPUT_DIR/vmlinux-stripped")
    STRIPPED_SIZE_MB=$((STRIPPED_SIZE / 1024 / 1024))
    echo "Stripped kernel size: ${STRIPPED_SIZE_MB}MB"
fi

echo "=== Kernel build complete ==="
