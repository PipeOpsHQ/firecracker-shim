#!/bin/bash
# Create a minimal base rootfs for Firecracker VMs
#
# This script creates an ext4 filesystem image containing:
# - Alpine Linux base system
# - runc for container execution
# - fc-agent for host communication
#
# Usage: ./create-rootfs.sh [output_path] [size_mb]

set -euo pipefail

OUTPUT_PATH="${1:-/var/lib/fc-cri/rootfs/base.ext4}"
SIZE_MB="${2:-256}"
ALPINE_VERSION="3.19"
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"

WORK_DIR=$(mktemp -d)
MOUNT_DIR="$WORK_DIR/mnt"
ROOTFS_DIR="$WORK_DIR/rootfs"

cleanup() {
    echo "Cleaning up..."
    if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
        sudo umount "$MOUNT_DIR" || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

echo "=== Creating Firecracker Base Rootfs ==="
echo "Output: $OUTPUT_PATH"
echo "Size: ${SIZE_MB}MB"

# Create directories
mkdir -p "$MOUNT_DIR" "$ROOTFS_DIR"
mkdir -p "$(dirname "$OUTPUT_PATH")"

# Create sparse ext4 image
echo "Creating ext4 filesystem image..."
dd if=/dev/zero of="$OUTPUT_PATH" bs=1M count=0 seek="$SIZE_MB" 2>/dev/null
mkfs.ext4 -F -L rootfs -O ^metadata_csum,^64bit "$OUTPUT_PATH"

# Mount the image
sudo mount -o loop "$OUTPUT_PATH" "$MOUNT_DIR"

# Download Alpine minirootfs
echo "Downloading Alpine Linux minirootfs..."
ALPINE_TAR="alpine-minirootfs-${ALPINE_VERSION}.0-x86_64.tar.gz"
if [[ ! -f "/tmp/$ALPINE_TAR" ]]; then
    wget -q -O "/tmp/$ALPINE_TAR" \
        "${ALPINE_MIRROR}/v${ALPINE_VERSION}/releases/x86_64/$ALPINE_TAR"
fi

# Extract to mount point
echo "Extracting Alpine rootfs..."
sudo tar xzf "/tmp/$ALPINE_TAR" -C "$MOUNT_DIR"

# Configure the rootfs
echo "Configuring rootfs..."

# Set up resolv.conf
sudo bash -c "cat > $MOUNT_DIR/etc/resolv.conf" <<EOF
nameserver 8.8.8.8
nameserver 8.8.4.4
EOF

# Set up init system (OpenRC)
sudo bash -c "cat > $MOUNT_DIR/etc/inittab" <<EOF
# /etc/inittab - Minimal init configuration for Firecracker
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default

# Start the fc-agent
::respawn:/usr/local/bin/fc-agent

# Console
ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100

# Shutdown
::shutdown:/sbin/openrc shutdown
EOF

# Set up network interface
sudo bash -c "cat > $MOUNT_DIR/etc/network/interfaces" <<EOF
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp
EOF

# Set hostname
echo "fc-vm" | sudo tee "$MOUNT_DIR/etc/hostname" > /dev/null

# Install required packages using chroot
echo "Installing packages..."
sudo chroot "$MOUNT_DIR" /bin/sh -c "
    # Update package index
    apk update

    # Install essential packages
    apk add --no-cache \
        runc \
        iptables \
        ip6tables \
        iproute2 \
        util-linux \
        ca-certificates \
        busybox-extras

    # Clean up
    rm -rf /var/cache/apk/*
"

# Copy fc-agent binary
FC_AGENT_PATH="${FC_AGENT_PATH:-./bin/fc-agent}"
if [[ -f "$FC_AGENT_PATH" ]]; then
    echo "Installing fc-agent..."
    sudo install -m 755 "$FC_AGENT_PATH" "$MOUNT_DIR/usr/local/bin/fc-agent"
else
    echo "Warning: fc-agent not found at $FC_AGENT_PATH"
    echo "Build it with: make agent"
fi

# Create required directories
sudo mkdir -p "$MOUNT_DIR/run/runc"
sudo mkdir -p "$MOUNT_DIR/run/fc-agent"
sudo mkdir -p "$MOUNT_DIR/var/lib/containers"

# Set up vsock module loading
sudo bash -c "cat > $MOUNT_DIR/etc/modules-load.d/vsock.conf" <<EOF
vsock
vmw_vsock_virtio_transport
vmw_vsock_virtio_transport_common
EOF

# Create a simple startup script
sudo bash -c "cat > $MOUNT_DIR/etc/local.d/fc-init.start" <<'EOF'
#!/bin/sh
# Firecracker VM initialization script

# Mount cgroups v2
if [ ! -d /sys/fs/cgroup/cgroup.controllers ]; then
    mount -t cgroup2 none /sys/fs/cgroup
fi

# Set up networking if not already done
if ! ip addr show eth0 | grep -q "inet "; then
    udhcpc -i eth0 -q -n
fi

# Signal readiness
echo "fc-init: VM ready" > /dev/ttyS0
EOF
sudo chmod +x "$MOUNT_DIR/etc/local.d/fc-init.start"

# Enable local scripts at boot
sudo chroot "$MOUNT_DIR" /sbin/rc-update add local default 2>/dev/null || true

# Unmount
sync
sudo umount "$MOUNT_DIR"

# Show result
ACTUAL_SIZE=$(du -h "$OUTPUT_PATH" | cut -f1)
echo ""
echo "=== Rootfs created successfully ==="
echo "Path: $OUTPUT_PATH"
echo "Size: $ACTUAL_SIZE (sparse: ${SIZE_MB}MB)"
echo ""
echo "Contents:"
echo "  - Alpine Linux ${ALPINE_VERSION}"
echo "  - runc container runtime"
echo "  - fc-agent (if built)"
echo "  - Network utilities"
