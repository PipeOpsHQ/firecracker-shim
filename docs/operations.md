# firecracker-shim Operations Guide

This guide covers the deployment, configuration, troubleshooting, and maintenance of the `firecracker-shim` runtime in production environments.

## Architecture Overview

The runtime operates as a containerd shim (`io.containerd.firecracker.v2`). When Kubernetes creates a pod, the following happens:

1. **kubelet** calls **containerd** (via CRI) to create a PodSandbox.
2. **containerd** spawns a new instance of **containerd-shim-fc-v2**.
3. The shim acquires a **Firecracker microVM** (from pool or new).
4. Inside the VM, **fc-agent** starts and listens on vsock.
5. Container operations (create, start, exec) are proxied to the agent.

## Deployment Requirements

### Host Requirements

- **OS**: Linux (kernel 4.14+)
- **Virtualization**: KVM enabled (`/dev/kvm` accessible)
- **vsock**: `vhost_vsock` kernel module loaded
- **containerd**: Version 1.7+
- **CNI Plugins**: Standard plugins installed (`bridge`, `ptp`, `host-local`, etc.)

### Sizing Recommendations

| Component           | CPU              | Memory               | Disk        |
| ------------------- | ---------------- | -------------------- | ----------- |
| **Shim process**    | < 1% core        | ~10MB                | Negligible  |
| **Firecracker VMM** | < 1% core (idle) | ~5MB                 | Negligible  |
| **Guest VM**        | 1+ vCPU          | 64MB+ (configurable) | Rootfs size |

**Recommended Host Config:**

- Enable hugepages for better VM memory performance (optional)
- Use `mq-deadline` or `none` I/O scheduler for backing files

## Configuration

Configuration is loaded from `/etc/fc-cri/config.toml`.

### VM Sizing

Adjust based on your workload needs:

```toml
[vm]
# Default vCPUs per VM
vcpu_count = 2

# Default memory per VM in MB
memory_mb = 256

# Minimum memory (if requested via pod resources)
min_memory_mb = 64

# Maximum memory cap
max_memory_mb = 4096
```

### VM Pool Tuning

The pool significantly reduces cold start latency. Tuning depends on your pod churn rate.

```toml
[pool]
enabled = true

# Max VMs to keep warm (memory cost: ~size * memory_mb)
max_size = 20

# Min VMs to always have ready
min_size = 5

# Concurrency for warming (limit to avoid CPU spikes)
warm_concurrency = 4
```

### Networking

The runtime supports standard CNI. The default setup uses a bridge.

**Important**: Ensure the subnet doesn't overlap with your host or pod network.

```toml
[network]
# Default subnet if not using CNI config
default_subnet = "10.88.0.0/16"
```

### Security (Jailer)

For production, **always enable the jailer**.

```toml
[runtime]
enable_jailer = true
jailer_binary = "/usr/bin/jailer"

# UID/GID to run Firecracker as (must exist)
uid = 1000
gid = 1000
```

**Prerequisites for Jailer:**

- User `1000:1000` exists
- `/srv/jailer` directory exists and is owned by `root:root`
- Cgroup v2 is recommended

## Troubleshooting

### Tools

The `fcctl` CLI is your primary troubleshooting tool.

```bash
# Check runtime health
sudo fcctl health

# List all sandboxes
sudo fcctl list

# Inspect specific sandbox
sudo fcctl inspect <sandbox-id>
```

### Common Issues

#### 1. Pods stuck in `ContainerCreating`

**Symptoms**: Pod status stays in `ContainerCreating` for >30s.

**Checks**:

1. Check runtime health: `fcctl health`
2. Check shim logs: `journalctl -u containerd` or `/var/lib/containerd/io.containerd.runtime.v2.task/.../log`
3. Verify VM started: `fcctl list`
4. Check agent connection: `fcctl inspect <id>`

**Possible Causes**:

- **KVM missing**: Ensure `/dev/kvm` exists and is accessible.
- **Kernel/Rootfs missing**: Verify `/var/lib/fc-cri/vmlinux` exists.
- **vsock failure**: Ensure `vhost_vsock` module is loaded.

#### 2. Network Connectivity Issues

**Symptoms**: Container cannot reach external network or other pods.

**Checks**:

1. Check CNI bridge: `ip addr show fc-br0`
2. Check VM IP: `fcctl inspect <id>`
3. Test from inside: `fcctl exec <id> ping 8.8.8.8`

**Possible Causes**:

- **IP exhaustion**: Check CNI subnet size vs number of pods.
- **Firewall**: Ensure `iptables` allows forwarding on `fc-br0`.

#### 3. High Host Memory Usage

**Symptoms**: Host OOM killer active.

**Checks**:

1. Check pool size: `fcctl pool status`
2. Check active VMs: `fcctl list | wc -l`

**Mitigation**:

- Reduce pool size.
- Reduce default VM memory in `config.toml`.
- Enable memory overcommitment (ensure swap is available).

## Monitoring

### Prometheus Metrics

The runtime exposes metrics at `:9090/metrics`.

**Key Metrics to Alert On:**

| Metric                              | Condition | Severity | Description                     |
| ----------------------------------- | --------- | -------- | ------------------------------- |
| `fc_cri_vm_create_errors_total`     | rate > 0  | High     | VM creation failing             |
| `fc_cri_agent_connect_errors_total` | rate > 0  | High     | Agent unreachable               |
| `fc_cri_pool_available`             | == 0      | Warning  | Pool exhausted (latency impact) |
| `fc_cri_start_latency_p95_ms`       | > 500ms   | Warning  | Slow startup                    |

### Logging

Logs are written to stdout (captured by containerd) or a file.

**Log Levels**:

- `info`: Normal operations (VM start/stop)
- `warn`: Retryable errors, resource pressure
- `error`: Operational failures requiring attention
- `debug`: Detailed trace (verbose!)

Change level via environment variable for specific pods:

```yaml
env:
  - name: FC_CRI_LOG_LEVEL
    value: "debug"
```

## Upgrades

1. **Drain node**: `kubectl drain <node> --ignore-daemonsets`
2. **Stop containerd**: `systemctl stop containerd`
3. **Install new binaries**: `make install`
4. **Update config**: Check `config.toml` for new options.
5. **Start containerd**: `systemctl start containerd`
6. **Uncordon node**: `kubectl uncordon <node>`

**Note**: Upgrading the shim binary does _not_ affect running VMs. Only new pods will use the new shim version.

## Disaster Recovery

### Cleaning Orphaned Resources

If the shim crashes hard, it might leave VMs running or files on disk.

```bash
# Dry run cleanup
sudo fcctl cleanup --dry-run

# Force cleanup
sudo fcctl cleanup
```

### recovering from Bad State

If the runtime is completely stuck:

1. Stop containerd: `systemctl stop containerd`
2. Kill all Firecracker processes: `pkill firecracker`
3. Remove runtime state: `rm -rf /run/fc-cri/*`
4. Start containerd: `systemctl start containerd`

_Warning: This will kill all running pods on the node._

## Advanced: Using a Custom Kernel

The default minimal kernel (~5MB) is optimized for speed and supports standard container workloads. However, it lacks support for advanced filesystems (XFS, ZFS), complex networking protocols (SCTP), or specific hardware drivers.

If your workload fails due to missing kernel features, you can swap in a standard kernel.

**Steps:**

1.  **Obtain a Kernel**: Compile your own or download an AWS Firecracker-compatible kernel (e.g., from the [Firecracker CI artifacts](https://github.com/firecracker-microvm/firecracker/releases) or Amazon Linux 2 kernel).
2.  **Place on Host**: Copy the `vmlinux` file to `/var/lib/fc-cri/custom-vmlinux`.
3.  **Update Config**:
    Edit `/etc/fc-cri/config.toml`:
    ```toml
    [vm]
    kernel_path = "/var/lib/fc-cri/custom-vmlinux"
    # Ensure boot args match your kernel requirements
    kernel_args = "console=ttyS0 reboot=k panic=1 pci=off quiet"
    ```
4.  **Restart**: New pods will use the new kernel immediately. Existing pods (and pooled VMs) must be recycled.
    ```bash
    # Clear the pool
    sudo systemctl restart containerd
    ```
