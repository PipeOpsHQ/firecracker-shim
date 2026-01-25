# firecracker-shim

A **containerd runtime (shim v2)** that runs Kubernetes pod sandboxes inside Firecracker microVMs.

**Firecracker isolation, Kubernetes-native, without Kata's complexity.**

## What This Is

This project provides a containerd shim that creates Firecracker microVMs for pod sandboxes. Kubernetes uses it via **RuntimeClass + containerd**—no changes to kubelet or CRI required.

```
Kubernetes → kubelet → containerd (CRI) → containerd-shim-fc-v2 → Firecracker VM
                                                                        ↓
                                                                   fc-agent → runc → container
```

### Security Model

- **1 VM per pod sandbox** (matches Kata semantics)
- Each pod runs in a dedicated microVM with its own kernel
- Containers within the pod share the VM (standard pod networking)
- VM-level isolation protects host from untrusted workloads

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│  Kubernetes Node                                                          │
│                                                                           │
│  ┌─────────────┐     ┌──────────────────────────────────────────────┐    │
│  │   kubelet   │────▶│               containerd                      │    │
│  └─────────────┘     │                   │                           │    │
│                      │         ┌─────────┴─────────┐                 │    │
│                      │         ▼                   ▼                 │    │
│                      │    CRI Plugin          Shim Manager           │    │
│                      └─────────┬───────────────────┬─────────────────┘    │
│                                │                   │                      │
│                                │     ┌─────────────┴─────────────┐        │
│                                │     │  containerd-shim-fc-v2    │        │
│                                │     │  (one per pod sandbox)    │        │
│                                │     └─────────────┬─────────────┘        │
│                                │                   │                      │
│  ┌─────────────────────────────┼───────────────────┼───────────────────┐  │
│  │  VM Pool                    │                   │                   │  │
│  │  ┌───────┐ ┌───────┐        │     ┌─────────────▼─────────────┐     │  │
│  │  │ warm  │ │ warm  │ ◀──────┘     │      Firecracker VMM      │     │  │
│  │  │  VM   │ │  VM   │              │            │               │     │  │
│  │  └───────┘ └───────┘              │     ┌──────┴──────┐       │     │  │
│  └───────────────────────────────────│     │             │       │─────┘  │
│                                      │     │  microVM    │       │        │
│                                      │     │             │       │        │
│                                      │     │  ┌───────┐  │       │        │
│                                      │     │  │fc-agen│◀─┼─vsock─┤        │
│                                      │     │  └───┬───┘  │       │        │
│                                      │     │      │      │       │        │
│                                      │     │  ┌───▼───┐  │       │        │
│                                      │     │  │ runc  │  │       │        │
│                                      │     │  │  │    │  │       │        │
│                                      │     │  │ ┌▼──┐ │  │       │        │
│                                      │     │  │ │app│ │  │       │        │
│                                      │     │  │ └───┘ │  │       │        │
│                                      │     │  └───────┘  │       │        │
│                                      │     └─────────────┘       │        │
│                                      └───────────────────────────┘        │
└──────────────────────────────────────────────────────────────────────────┘
```

## Features

| Feature                 | Description                                 |
| ----------------------- | ------------------------------------------- |
| **VM Pooling**          | Pre-warmed VMs for fast acquisition (<50ms) |
| **Minimal Footprint**   | 64-128MB memory per VM                      |
| **vsock Communication** | Efficient host↔guest via virtio-vsock       |
| **CNI Networking**      | Standard CNI plugin support                 |
| **Jailer Support**      | Optional privilege isolation                |
| **Prometheus Metrics**  | Pool stats, latencies, errors               |
| **Debug CLI**           | `fcctl` for inspection and troubleshooting  |

## Non-Goals

- **Not a CRI implementation**: We're a containerd shim, not a replacement for containerd/CRI
- **Not cross-platform**: Linux only, requires KVM
- **No nested virtualization**: Requires bare-metal or VM with nested virt enabled
- **Snapshots are best-effort**: Host-kernel sensitive, not guaranteed portable

## Quick Start

### Prerequisites

```bash
# Check KVM support
ls -la /dev/kvm

# Check vsock support
ls -la /dev/vhost-vsock

# Required: containerd 1.7+
containerd --version
```

### Installation

```bash
# Clone
git clone https://github.com/pipeops/firecracker-cri
cd firecracker-cri

# Install dependencies (fsify, skopeo, umoci)
make deps

# Build binaries
make build

# Build or download kernel (takes ~10 min to build)
make kernel

# Create base rootfs
make rootfs

# Install
sudo make install

# Restart containerd
sudo systemctl restart containerd
```

### Verify Installation

```bash
# Check health
sudo fcctl health

# List sandboxes (should be empty initially)
sudo fcctl list
```

### Usage with Kubernetes

1. **Apply RuntimeClass:**

```yaml
# runtime-class.yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: firecracker
handler: firecracker
overhead:
  podFixed:
    memory: "64Mi"
    cpu: "100m"
scheduling:
  nodeSelector:
    fc-cri.io/enabled: "true" # Label your fc-cri nodes
```

```bash
kubectl apply -f runtime-class.yaml
```

2. **Deploy a pod:**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: isolated-workload
spec:
  runtimeClassName: firecracker
  containers:
    - name: app
      image: nginx:alpine
      resources:
        requests:
          memory: "64Mi"
          cpu: "100m"
```

3. **Verify it's running in a VM:**

```bash
kubectl get pod isolated-workload -o wide
sudo fcctl list
sudo fcctl inspect <sandbox-id>
```

## Configuration

### Runtime Configuration

```toml
# /etc/fc-cri/config.toml

[runtime]
runtime_dir = "/run/fc-cri"
firecracker_binary = "/usr/bin/firecracker"
shutdown_timeout = "30s"

[vm]
kernel_path = "/var/lib/fc-cri/vmlinux"
kernel_args = "console=ttyS0 reboot=k panic=1 pci=off quiet"
default_vcpu_count = 1
default_memory_mb = 128
min_memory_mb = 64
max_memory_mb = 8192

[pool]
enabled = true
max_size = 10
min_size = 3
max_idle_time = "5m"
warm_concurrency = 2

[network]
network_mode = "cni"
cni_plugin_dir = "/opt/cni/bin"
cni_conf_dir = "/etc/cni/net.d"

[agent]
vsock_port = 1024
connect_timeout = "30s"

[metrics]
enabled = true
address = ":9090"
path = "/metrics"

[log]
level = "info"  # debug, info, warn, error
format = "text" # text, json
```

### containerd Configuration

```toml
# /etc/containerd/config.d/firecracker.toml

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker]
  runtime_type = "io.containerd.firecracker.v2"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker.options]
  ConfigPath = "/etc/fc-cri/config.toml"
```

## Components

### containerd-shim-fc-v2

The main shim binary launched by containerd for each pod sandbox.

```
/usr/local/bin/containerd-shim-fc-v2
```

Responsibilities:

- Implements containerd shim v2 protocol
- Manages Firecracker VM lifecycle
- Communicates with fc-agent via vsock
- Handles container create/start/stop/delete

### fc-agent

Minimal agent running inside the microVM (~2-3MB static binary).

```
/usr/local/bin/fc-agent  (inside VM at /usr/local/bin/fc-agent)
```

Responsibilities:

- Listens on vsock port 1024
- Executes container operations via runc
- Streams stdout/stderr
- Reports container stats

### fcctl

Debug and inspection CLI.

```bash
# List all sandboxes
fcctl list

# Detailed inspection
fcctl inspect fc-1234567890

# Pool status
fcctl pool status

# View metrics
fcctl metrics

# Stream logs
fcctl logs fc-1234567890 -f

# Execute command in VM
fcctl exec fc-1234567890 cat /etc/os-release

# Health check
fcctl health

# Clean up orphaned resources
fcctl cleanup --dry-run
```

## Performance

**Test Environment**: 4 vCPU, 8GB RAM, Intel Xeon, ext4, containerd 1.7

| Metric                        | Value    | Notes               |
| ----------------------------- | -------- | ------------------- |
| Cold start (create → running) | ~150ms   | Excludes image pull |
| Warm start (from pool)        | <50ms    | Pre-warmed VM       |
| Memory per VM                 | 64-128MB | Depends on workload |
| Agent binary size             | ~2.5MB   | Static binary       |
| Kernel size                   | ~5MB     | Minimal config      |

**Pool Hit Rates** (after warm-up):

- Steady workload: 85-95% hit rate
- Bursty workload: 60-80% hit rate

## Networking

### CNI Integration

We support standard CNI plugins. The default configuration uses bridge networking:

```json
{
  "cniVersion": "1.0.0",
  "name": "fc-net",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "fc-br0",
      "isGateway": true,
      "ipMasq": true,
      "ipam": {
        "type": "host-local",
        "subnet": "10.88.0.0/16"
      }
    },
    {
      "type": "portmap",
      "capabilities": { "portMappings": true }
    }
  ]
}
```

### Network Flow

1. CNI creates tap device and assigns IP
2. Firecracker attaches tap to virtio-net
3. Guest kernel sees eth0 interface
4. fc-agent configures interface via DHCP or static

## Image Handling

We use [fsify](https://github.com/volantvm/fsify) to convert OCI images to ext4 block devices:

```bash
# Manual conversion
make convert-image IMAGE=nginx:latest

# Programmatic (via FsifyConverter)
converter.Convert(ctx, "nginx:latest")
```

### Root Filesystem Strategy

1. **Image pull**: Host pulls OCI image (via containerd/skopeo)
2. **Convert**: fsify creates ext4 block device from layers
3. **Attach**: Block device attached to VM as virtio-blk
4. **Mount**: fc-agent mounts as rootfs

This keeps the security boundary clear—images are processed on host, only the final rootfs enters the VM.

## Troubleshooting

### Pod stuck in ContainerCreating

```bash
# Check sandbox status
sudo fcctl list
sudo fcctl inspect <sandbox-id>

# Check agent connectivity
sudo fcctl exec <sandbox-id> echo "ping"

# View logs
sudo fcctl logs <sandbox-id> -f

# Check metrics for errors
sudo fcctl metrics | grep error
```

### VM fails to start

```bash
# Check KVM access
ls -la /dev/kvm

# Check kernel exists
ls -la /var/lib/fc-cri/vmlinux

# Check rootfs exists
ls -la /var/lib/fc-cri/rootfs/base.ext4

# Run health check
sudo fcctl health
```

### Networking issues

```bash
# Check CNI plugins
ls /opt/cni/bin/

# Check CNI config
cat /etc/cni/net.d/*.conflist

# Check bridge
ip addr show fc-br0

# Test from inside VM
sudo fcctl exec <sandbox-id> ip addr
sudo fcctl exec <sandbox-id> ping -c 1 8.8.8.8
```

### Clean up orphaned VMs

```bash
# List all VMs
sudo fcctl list

# Find orphaned (dead/unknown state)
sudo fcctl cleanup --dry-run

# Clean up
sudo fcctl cleanup
```

## Development

### Project Structure

```
firecracker-cri/
├── cmd/
│   ├── containerd-shim-fc-v2/  # Shim binary
│   ├── fc-agent/                # Guest agent
│   └── fcctl/                   # Debug CLI
├── pkg/
│   ├── domain/                  # Core types
│   ├── vm/                      # VM management
│   │   ├── manager.go          # Lifecycle
│   │   ├── pool.go             # Pre-warming
│   │   ├── snapshot.go         # Fast restore
│   │   ├── hotplug.go          # Drive attach
│   │   └── jailer.go           # Security
│   ├── shim/                    # Shim implementation
│   ├── agent/                   # Agent client
│   ├── network/                 # CNI integration
│   ├── image/                   # OCI → rootfs
│   ├── config/                  # Configuration
│   └── metrics/                 # Prometheus
├── kernel/                      # Kernel build
├── scripts/                     # Helper scripts
├── config/                      # Default configs
└── deploy/kubernetes/           # K8s manifests
```

### Building

```bash
# Build all
make build

# Build individual components
make shim
make agent

# Cross-compile for Linux (from macOS)
GOOS=linux make build

# Run tests
make test

# Run integration tests
sudo ./scripts/integration-test.sh -v
```

### Testing

```bash
# Unit tests
go test ./...

# Integration tests (requires root, KVM)
sudo ./scripts/integration-test.sh

# Test specific component
sudo ./scripts/integration-test.sh -t kernel_boot
```

## Roadmap

### v0.1 (MVP)

- [x] containerd shim v2 (CreateTask/Start/Stop/Delete)
- [x] 1 VM per pod sandbox
- [x] vsock agent (exec, mount, signal)
- [x] CNI networking (bridge + host-local)
- [x] RuntimeClass + docs

### v0.2

- [x] VM pooling
- [x] Metrics + fcctl
- [ ] Resilience (restart, cleanup)
- [ ] CI/CD pipeline

### v0.3

- [x] Snapshot support (optional)
- [ ] Multi-arch (arm64)
- [ ] Conformance tests

## Contributing

1. Fork the repository
2. Create a feature branch
3. Write tests for new functionality
4. Submit a pull request

## License

Apache 2.0

## Acknowledgments

- [Firecracker](https://firecracker-microvm.github.io/) - The microVM engine
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk) - Go SDK
- [containerd](https://containerd.io/) - Container runtime
- [fsify](https://github.com/volantvm/fsify) - OCI to rootfs conversion
