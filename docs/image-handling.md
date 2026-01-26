# Image Handling

One of the key differentiators of `firecracker-shim` is how it handles container images. Instead of requiring complex setups like Device Mapper (devmapper) or overlayfs inside the guest, we convert standard OCI (Docker) images into simple ext4 block devices on the host.

This approach simplifies operations but comes with specific trade-offs.

## How It Works

1.  **Pull**: When a pod is scheduled, `containerd` pulls the image layers to the host as usual.
2.  **Convert**: The shim (via `fsify`) takes these layers and merges them into a single directory structure.
3.  **Build**: It creates a sparse `.ext4` filesystem file and copies the merged content into it.
4.  **Attach**: This file is attached to the Firecracker microVM as a read-only block device (`/dev/vdb`).
5.  **Mount**: The in-guest agent mounts this device as the container's root filesystem.

## Caching Strategy

Conversion takes time (seconds for large images). To mitigate this, we implement a **Host-Side Conversion Cache**.

*   **Cache Location**: `/var/lib/fc-cri/images/cache/`
*   **Key**: Image Digest (SHA256)
*   **Behavior**:
    *   First run: Pull -> Convert -> Cache -> Run (~seconds)
    *   Subsequent runs: Cache Hit -> Run (<100ms)

The cache is shared across all pods on the node.

## Supported Features

| Feature | Status | Notes |
| :--- | :--- | :--- |
| **Standard Images** | Supported | Debian, Alpine, Ubuntu, Distroless work out of the box. |
| **Large Images** | Supported | Tested up to 10GB. Conversion time scales with size. |
| **Whiteouts (.wh)** | Supported | Correctly handles file deletion in upper layers. |
| **Symlinks** | Supported | Preserves standard symlink behavior. |
| **User/Group Ownership** | Supported | Preserves UID/GID from the image. |
| **File Capabilities** | Supported | `setcap` bits are preserved in the ext4 image. |

## Limitations & Constraints

### 1. Startup Latency (Cold)
The *first* time an image is used on a node, there is a conversion penalty.
*   **Alpine (5MB)**: < 100ms
*   **Ubuntu (30MB)**: ~500ms
*   **Heavy App (1GB)**: ~2-5 seconds

**Mitigation**: Pre-pull images or use the VM pool (which doesn't solve image conversion but speeds up VM boot).

### 2. Disk Usage
We create a full flattened copy of the image. While we use **sparse files** (only allocating used blocks), this consumes more disk space than overlayfs which shares layers between images.
*   **Mitigation**: Run a periodic cleanup script to prune `/var/lib/fc-cri/images/cache/`.

### 3. Read-Only Rootfs
By default, the container's root filesystem is mounted **Read-Only** for security.
*   **Writes**: Use `emptyDir` volumes or standard Kubernetes volumes for writable paths.
*   **Overlay**: We do *not* currently overlay a writable tmpfs on top of the rootfs inside the guest (to keep the agent simple). This enforces immutable infrastructure patterns.

## Security Guarantees

*   **Host Processing**: Image parsing and conversion happen on the host. Malicious filesystem structures are processed by the host kernel/tools.
*   **Isolation**: The resulting ext4 image is exposed to the guest as a block device. The guest kernel parses the ext4 filesystem.
*   **Integrity**: We verify image digests before conversion (relies on containerd).

## Troubleshooting

**Symptoms**: "Image unpack failed" or "No space left on device".

**Checks**:
1.  **Disk Space**: Ensure `/var/lib/fc-cri` has sufficient space.
2.  **Permissions**: Ensure the shim has write access to the cache dir.
3.  **Logs**: Check shim logs for `fsify` errors.

```bash
# Clear the cache manually if corrupted
sudo rm -rf /var/lib/fc-cri/images/cache/*
```
