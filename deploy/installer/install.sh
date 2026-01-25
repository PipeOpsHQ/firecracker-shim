#!/bin/bash
set -euo pipefail

# Configuration
HOST_ROOT="${HOST_ROOT:-/host}"
BIN_DIR="${HOST_ROOT}/usr/local/bin"
CONFIG_DIR="${HOST_ROOT}/etc/fc-cri"
LIB_DIR="${HOST_ROOT}/var/lib/fc-cri"
CONTAINERD_CONF_DIR="${HOST_ROOT}/etc/containerd/config.d"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

log() {
    echo -e "${GREEN}[installer]${NC} $1"
}

error() {
    echo -e "${RED}[installer] Error:${NC} $1"
    exit 1
}

# Check if running privileged
if [ "$(id -u)" -ne 0 ]; then
    error "Must run as root"
fi

# 1. Check KVM availability
log "Checking KVM availability..."
if [ ! -e "${HOST_ROOT}/dev/kvm" ]; then
    error "/dev/kvm not found on host. Please enable KVM."
fi

# 2. Check containerd
log "Checking containerd..."
if [ ! -d "${HOST_ROOT}/etc/containerd" ]; then
    error "containerd configuration directory not found at /etc/containerd"
fi

# 3. Install Binaries
log "Installing binaries to ${BIN_DIR}..."
mkdir -p "${BIN_DIR}"

for bin in containerd-shim-fc-v2 fc-agent fcctl firecracker jailer; do
    if [ -f "/binaries/${bin}" ]; then
        cp "/binaries/${bin}" "${BIN_DIR}/"
        chmod +x "${BIN_DIR}/${bin}"
        log "  Installed ${bin}"
    else
        log "  Warning: ${bin} not found in installer image"
    fi
done

# 4. Install Configuration
log "Installing configuration to ${CONFIG_DIR}..."
mkdir -p "${CONFIG_DIR}"
if [ -f "/config/config.toml" ]; then
    cp "/config/config.toml" "${CONFIG_DIR}/"
else
    log "  Warning: config.toml not found, creating default"
fi

# 5. Install Assets (Kernel/Rootfs)
log "Installing assets to ${LIB_DIR}..."
mkdir -p "${LIB_DIR}/rootfs"
mkdir -p "${LIB_DIR}/images"

if [ -f "/assets/vmlinux" ]; then
    cp "/assets/vmlinux" "${LIB_DIR}/"
    log "  Installed vmlinux"
fi

if [ -f "/assets/base.ext4" ]; then
    cp "/assets/base.ext4" "${LIB_DIR}/rootfs/"
    log "  Installed base.ext4"
fi

# 6. Configure containerd
log "Configuring containerd..."
if [ -d "${CONTAINERD_CONF_DIR}" ]; then
    log "  Detected config.d support"
    if [ -f "/config/containerd-fc.toml" ]; then
        cp "/config/containerd-fc.toml" "${CONTAINERD_CONF_DIR}/firecracker.toml"
        log "  Installed firecracker.toml config drop-in"
    fi
else
    log "  Warning: ${CONTAINERD_CONF_DIR} not found."
    log "  Manual configuration of /etc/containerd/config.toml required."
fi

# 7. Restart containerd
if [ "${RESTART_CONTAINERD:-true}" = "true" ]; then
    log "Restarting containerd on host..."

    # Try nsenter to access host systemd
    if command -v nsenter >/dev/null; then
        # Access host namespace to run systemctl
        nsenter --target 1 --mount --uts --ipc --net --pid systemctl restart containerd || \
        log "  Warning: Failed to restart containerd via nsenter"
    else
        log "  Warning: nsenter not found, cannot restart containerd"
    fi
else
    log "Skipping containerd restart (RESTART_CONTAINERD=${RESTART_CONTAINERD})"
fi

log "Installation successful!"

# Sleep to keep DaemonSet pod running
sleep infinity
