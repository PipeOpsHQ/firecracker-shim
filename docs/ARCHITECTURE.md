# fc-cri Architecture & Design Document

## Executive Summary

**fc-cri** is a custom CRI (Container Runtime Interface) runtime that runs containers inside Firecracker microVMs. It's designed as a lightweight alternative to Kata Containers, targeting significantly lower resource overhead while maintaining VM-level isolation.

### Goals

- **Memory**: 64-128MB per VM (vs 160MB+ for Kata)
- **Cold Start**: <150ms (vs 500ms+ for Kata)
- **Warm Start**: <50ms using VM pooling
- **Simplicity**: Direct Firecracker integration, minimal abstraction layers

### Non-Goals

- Full Kata compatibility
- Support for multiple hypervisors (Firecracker only)
- Windows containers

---

## Background & Motivation

### Why Not Kata Containers?

Kata Containers is the established solution for running containers in VMs on Kubernetes. However, it has significant overhead:

| Resource              | Kata Containers                     | Target for fc-cri |
| --------------------- | ----------------------------------- | ----------------- |
| Memory baseline       | 160MB+                              | 64-128MB          |
| Guest agent           | ~50MB (kata-agent)                  | ~2-3MB (fc-agent) |
| Cold start time       | 500-800ms                           | <150ms            |
| Warm start time       | N/A                                 | <50ms             |
| Supported hypervisors | QEMU, Cloud-Hypervisor, Firecracker | Firecracker only  |

Kata's overhead comes from:

1. **Generic VMM abstraction** - Supports multiple hypervisors, adding complexity
2. **Full-featured guest agent** - kata-agent is feature-rich but heavy
3. **No VM pooling** - Every pod starts a fresh VM
4. **Complex storage** - Multiple layers of abstraction for image handling

### Why Firecracker?

Firecracker is AWS's microVM hypervisor, used in Lambda and Fargate. Key properties:

- **Minimal attack surface** - Purpose-built for multi-tenant isolation
- **Fast boot** - <125ms to kernel boot
- **Low memory** - ~5MB VMM overhead
- **Simple API** - REST/socket API for VM management
- **No legacy support** - No BIOS, no PCI, no USB = smaller kernel

### Use Cases

1. **Multi-tenant SaaS** - Run untrusted customer code with VM isolation
2. **CI/CD pipelines** - Isolated build environments
3. **Serverless platforms** - Fast-starting isolated execution environments
4. **Compliance workloads** - When container isolation isn't sufficient

---

## Architecture Overview

### System Context

```
┌─────────────────────────────────────────────────────────────────────────┐
│                            Kubernetes Cluster                            │
│                                                                          │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                           Control Plane                           │   │
│  │                                                                   │   │
│  │   ┌─────────────┐   ┌─────────────┐   ┌─────────────────────┐   │   │
│  │   │ API Server  │   │ Scheduler   │   │ Controller Manager  │   │   │
│  │   └─────────────┘   └─────────────┘   └─────────────────────┘   │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                                    │                                     │
│                                    │ Schedule Pod with                   │
│                                    │ runtimeClassName: firecracker       │
│                                    ▼                                     │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                          Worker Node                              │   │
│  │                                                                   │   │
│  │   ┌─────────────────────────────────────────────────────────┐   │   │
│  │   │                       kubelet                            │   │   │
│  │   │                          │                               │   │   │
│  │   │                     CRI (gRPC)                           │   │   │
│  │   │                          │                               │   │   │
│  │   │                          ▼                               │   │   │
│  │   │   ┌─────────────────────────────────────────────────┐   │   │   │
│  │   │   │                  containerd                      │   │   │   │
│  │   │   │                       │                          │   │   │   │
│  │   │   │              Runtime Selection                   │   │   │   │
│  │   │   │                       │                          │   │   │   │
│  │   │   │         ┌─────────────┴─────────────┐           │   │   │   │
│  │   │   │         │                           │           │   │   │   │
│  │   │   │         ▼                           ▼           │   │   │   │
│  │   │   │   ┌──────────┐              ┌─────────────┐    │   │   │   │
│  │   │   │   │   runc   │              │  fc-cri     │    │   │   │   │
│  │   │   │   │ (default)│              │   shim      │    │   │   │   │
│  │   │   │   └──────────┘              └─────────────┘    │   │   │   │
│  │   │   │                                    │           │   │   │   │
│  │   │   └────────────────────────────────────┼───────────┘   │   │   │
│  │   │                                        │               │   │   │
│  │   └────────────────────────────────────────┼───────────────┘   │   │
│  │                                            │                   │   │
│  │                                            ▼                   │   │
│  │   ┌────────────────────────────────────────────────────────┐   │   │
│  │   │                   Firecracker VMM                       │   │   │
│  │   │  ┌──────────────────────────────────────────────────┐  │   │   │
│  │   │  │                    microVM                        │  │   │   │
│  │   │  │                                                   │  │   │   │
│  │   │  │   ┌─────────────┐      ┌──────────────────────┐  │  │   │   │
│  │   │  │   │  fc-agent   │◄────►│  Container (runc)    │  │  │   │   │
│  │   │  │   └─────────────┘      └──────────────────────┘  │  │   │   │
│  │   │  │          ▲                                        │  │   │   │
│  │   │  └──────────┼────────────────────────────────────────┘  │   │   │
│  │   │             │ vsock                                      │   │   │
│  │   │             ▼                                            │   │   │
│  │   │      ┌─────────────┐                                     │   │   │
│  │   │      │  VM Pool    │ (pre-warmed VMs)                    │   │   │
│  │   │      └─────────────┘                                     │   │   │
│  │   └────────────────────────────────────────────────────────┘   │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### Component Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                              fc-cri                                      │
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────────┐ │
│  │                    containerd-shim-fc-v2                            │ │
│  │                                                                     │ │
│  │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐ │ │
│  │  │   Shim Service   │  │   VM Manager     │  │   Agent Client   │ │ │
│  │  │                  │  │                  │  │                  │ │ │
│  │  │ - Task lifecycle │  │ - Create/Stop VM │  │ - JSON-RPC/vsock │ │ │
│  │  │ - Event publish  │  │ - Snapshot mgmt  │  │ - Container ops  │ │ │
│  │  │ - State tracking │  │ - Resource cfg   │  │ - Exec/Attach    │ │ │
│  │  └────────┬─────────┘  └────────┬─────────┘  └────────┬─────────┘ │ │
│  │           │                     │                     │           │ │
│  │           └─────────────────────┼─────────────────────┘           │ │
│  │                                 │                                  │ │
│  │  ┌──────────────────────────────┴───────────────────────────────┐ │ │
│  │  │                         VM Pool                               │ │ │
│  │  │                                                               │ │ │
│  │  │  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐│ │ │
│  │  │  │ Warm VM │ │ Warm VM │ │ Warm VM │ │ Warm VM │ │ Warm VM ││ │ │
│  │  │  └─────────┘ └─────────┘ └─────────┘ └─────────┘ └─────────┘│ │ │
│  │  │                                                               │ │ │
│  │  │  - Acquire() → O(1) VM retrieval                             │ │ │
│  │  │  - Release() → Return to pool or destroy                     │ │ │
│  │  │  - Auto-replenish background goroutine                       │ │ │
│  │  │  - Idle cleanup (configurable max idle time)                 │ │ │
│  │  └───────────────────────────────────────────────────────────────┘ │ │
│  │                                                                     │ │
│  │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐ │ │
│  │  │  Network (CNI)   │  │  Image Service   │  │  Config Manager  │ │ │
│  │  │                  │  │                  │  │                  │ │ │
│  │  │ - TAP devices    │  │ - OCI pull       │  │ - TOML config    │ │ │
│  │  │ - Bridge setup   │  │ - Layer flatten  │  │ - Defaults       │ │ │
│  │  │ - IP assignment  │  │ - ext4 creation  │  │ - Validation     │ │ │
│  │  └──────────────────┘  └──────────────────┘  └──────────────────┘ │ │
│  └────────────────────────────────────────────────────────────────────┘ │
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────────┐ │
│  │                          fc-agent (in VM)                           │ │
│  │                                                                     │ │
│  │  ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐ │ │
│  │  │   vsock Server   │  │  Container Mgr   │  │   Stats/Cgroups  │ │ │
│  │  │                  │  │                  │  │                  │ │ │
│  │  │ - JSON-RPC proto │  │ - runc create    │  │ - CPU usage      │ │ │
│  │  │ - Request router │  │ - runc start     │  │ - Memory usage   │ │ │
│  │  │ - Conn handling  │  │ - runc exec      │  │ - I/O stats      │ │ │
│  │  └──────────────────┘  └──────────────────┘  └──────────────────┘ │ │
│  └────────────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## Domain Model

Following domain-driven design principles, the core domain is modeled around these key concepts:

### Aggregate Roots

#### Sandbox (Pod Sandbox / microVM)

The `Sandbox` is the aggregate root representing a Firecracker microVM that hosts one or more containers.

```go
type Sandbox struct {
    // Identity
    ID        string
    Name      string
    Namespace string

    // VM State
    State       SandboxState  // Pending → Ready → Stopped
    VM          *firecracker.Machine
    VMConfig    VMConfig

    // Communication
    VsockPath   string
    VsockCID    uint32
    AgentConn   net.Conn

    // Networking
    NetworkNamespace string
    IP               net.IP

    // Containers within this sandbox
    Containers map[string]*Container
}
```

#### Container

A `Container` represents a container running inside a Sandbox (microVM).

```go
type Container struct {
    ID        string
    SandboxID string
    Name      string
    Image     string

    State      ContainerState  // Created → Running → Exited
    PID        int
    ExitCode   int32

    // Configuration
    Command    []string
    Env        []string
    Mounts     []Mount
    Resources  ResourceConfig
}
```

### Value Objects

#### VMConfig

Immutable configuration for creating a Firecracker VM.

```go
type VMConfig struct {
    VcpuCount    int64   // Default: 1
    MemoryMB     int64   // Default: 128
    KernelPath   string
    KernelArgs   string
    RootDrive    DriveConfig
    NetworkMode  string  // "cni" or "none"
    VsockEnabled bool
}
```

### Domain Services

| Service          | Responsibility                             |
| ---------------- | ------------------------------------------ |
| `VMManager`      | Create, stop, destroy Firecracker VMs      |
| `VMPool`         | Pre-warm VMs for fast acquisition          |
| `AgentClient`    | Communicate with guest agent via vsock     |
| `NetworkService` | CNI-based network setup and teardown       |
| `ImageService`   | OCI image pull and block device conversion |

---

## Key Design Decisions

### 1. containerd Shim v2 (Not Full CRI Server)

**Decision**: Implement a containerd shim rather than a standalone CRI server.

**Rationale**:

- containerd handles CRI protocol, image management, and storage
- Shim only handles runtime-specific logic
- Less code to maintain, better integration
- containerd manages shim lifecycle automatically

**Trade-off**: Tightly coupled to containerd (can't use with CRI-O directly)

### 2. VM Pooling for Fast Starts

**Decision**: Pre-warm VMs and maintain a pool of ready-to-use instances.

**Rationale**:

- VM creation is the slowest part (~150ms even with Firecracker)
- Pool provides O(1) VM acquisition
- Enables <50ms pod start times

**Implementation**:

```go
type Pool struct {
    available chan *Sandbox  // Ready VMs
    inUse     map[string]*Sandbox
    config    PoolConfig
}

func (p *Pool) Acquire(ctx context.Context, config VMConfig) (*Sandbox, error) {
    select {
    case sandbox := <-p.available:
        // Got pre-warmed VM - customize and return
        return p.customizeVM(sandbox, config)
    default:
        // Pool empty - create fresh
        return p.manager.CreateVM(ctx, config)
    }
}
```

**Configuration**:

```toml
[pool]
enabled = true
size = 5          # VMs to keep ready
min_size = 2      # Minimum to maintain
max_idle_time = "5m"
```

### 3. Minimal Guest Agent

**Decision**: Build a custom minimal agent instead of using kata-agent.

**Rationale**:

- kata-agent is ~50MB, ours is ~2-3MB
- Simple JSON-RPC protocol over vsock
- Only implements what we need
- Static binary, no runtime dependencies

**Protocol**:

```json
// Request
{"id": 1, "method": "create_container", "params": {"id": "abc", "bundle": "/..."}}

// Response
{"id": 1, "result": {"status": "created"}}
```

**Supported Methods**:

- `ping` - Health check
- `create_container` - Create container via runc
- `start_container` - Start container, return PID
- `stop_container` - Stop with timeout, then SIGKILL
- `remove_container` - Delete container
- `exec_sync` - Synchronous exec
- `get_stats` - Cgroup statistics

### 4. Block Device Storage (Not Overlayfs)

**Decision**: Convert OCI images to ext4 block devices.

**Rationale**:

- Firecracker doesn't support filesystem sharing (no 9p, no virtiofs)
- Block devices (virtio-blk) are fast and simple
- Sparse files minimize disk usage

**Flow**:

```
OCI Image → Pull layers → Flatten → Create ext4 → Attach to VM as /dev/vda
```

**Future Optimization**: Use device mapper thin provisioning for copy-on-write efficiency.

### 5. Minimal Kernel Configuration

**Decision**: Build a custom minimal kernel (~5MB uncompressed).

**Rationale**:

- Stock kernels are 30-50MB
- We only need: virtio, vsock, ext4, cgroups, namespaces, netfilter
- Faster boot, smaller memory footprint

**Trade-offs**:

- **Reduced Compatibility**: Missing drivers for XFS, ZFS, SCTP, and specialized hardware.
- **Mitigation**: Users can supply their own kernel via `config.toml` (see Operations Guide).

**Key Config**:

```
CONFIG_VIRTIO_MMIO=y      # Firecracker uses MMIO, not PCI
CONFIG_VIRTIO_BLK=y       # Block devices
CONFIG_VIRTIO_NET=y       # Networking
CONFIG_VIRTIO_VSOCKETS=y  # Host communication
CONFIG_EXT4_FS=y          # Rootfs
CONFIG_OVERLAY_FS=y       # Container layers
CONFIG_CGROUPS=y          # Resource limits
CONFIG_NAMESPACES=y       # Container isolation
CONFIG_PCI=n              # Not needed
CONFIG_USB_SUPPORT=n      # Not needed
CONFIG_SOUND=n            # Not needed
```

---

## Implementation Status

### Completed Components

| Component            | Description                                                              |
| -------------------- | ------------------------------------------------------------------------ |
| **Domain Model**     | Core entities, value objects, and service interfaces.                    |
| **VM Manager**       | Lifecycle management (create, stop, destroy) using firecracker-go-sdk.   |
| **VM Pool**          | Pre-warming, acquisition, release, and auto-replenishment logic.         |
| **Shim Service**     | Implementation of containerd shim v2 task API.                           |
| **Agent Client**     | Host-side JSON-RPC client for guest communication.                       |
| **Guest Agent**      | Minimal static binary handling vsock communication and runc integration. |
| **CNI Network**      | Network namespace management and CNI plugin invocation.                  |
| **Image Service**    | OCI image pull and conversion to ext4 block devices (fsify).             |
| **Hot-Attach**       | Dynamic attachment of workload rootfs to pooled VMs.                     |
| **Snapshot Restore** | Fast VM restoration from memory snapshots.                               |
| **Jailer**           | Production security hardening (chroot, cgroups, seccomp).                |
| **Metrics**          | Prometheus metrics for pool stats, latencies, and errors.                |
| **CLI Tool**         | `fcctl` for inspection and debugging.                                    |

### Future Work

| Component     | Priority | Notes                                                                                          |
| ------------- | -------- | ---------------------------------------------------------------------------------------------- |
| **Devmapper** | High     | Alternative storage backend for thin provisioning.                                             |
| **ARM64**     | Medium   | Support for Graviton/Ampere instances.                                                         |
| **GPU**       | Low      | Passthrough support for ML workloads.                                                          |
| **PVM**       | Low      | Pagetable-based Virtual Machine (Research). Support for running without nested virtualization. |

---

## File Structure

```
firecracker-cri/
├── cmd/
│   ├── containerd-shim-fc-v2/
│   │   └── main.go              # Shim entry point
│   └── fc-agent/
│       └── main.go              # Guest agent (static binary)
│
├── pkg/
│   ├── domain/
│   │   └── types.go             # Core domain model
│   ├── vm/
│   │   ├── manager.go           # VM lifecycle management
│   │   └── pool.go              # Pre-warming pool
│   ├── shim/
│   │   └── service.go           # containerd shim v2 service
│   ├── agent/
│   │   └── client.go            # Guest agent client
│   ├── network/
│   │   └── cni.go               # CNI integration
│   └── image/
│       └── rootfs.go            # OCI to block device
│
├── kernel/
│   ├── config-minimal           # Kernel configuration
│   └── build.sh                 # Kernel build script
│
├── config/
│   ├── fc-cri.toml              # Runtime configuration
│   └── containerd-fc.toml       # containerd integration
│
├── deploy/
│   └── kubernetes/
│       ├── runtime-class.yaml   # Kubernetes RuntimeClass
│       └── example-pod.yaml     # Usage examples
│
├── scripts/
│   └── create-rootfs.sh         # Base rootfs creation
│
├── docs/
│   └── ARCHITECTURE.md          # This document
│
├── go.mod
├── Makefile
└── README.md
```

---

## Getting Started

### Prerequisites

```bash
# Required
- Linux with KVM support (check: ls /dev/kvm)
- containerd 1.6+
- Go 1.22+
- Root access for installation

# Optional
- Docker (for building rootfs)
- crictl (for testing)
```

### Build & Install

```bash
# Clone the repository
git clone https://github.com/pipeops/firecracker-cri.git
cd firecracker-cri

# Build binaries
make build

# Build kernel (one-time, ~10 minutes)
make kernel

# Create base rootfs
make rootfs

# Install
sudo make install

# Restart containerd
sudo systemctl restart containerd
```

### Test with Kubernetes

```bash
# Apply RuntimeClass
kubectl apply -f deploy/kubernetes/runtime-class.yaml

# Label node as fc-cri enabled
kubectl label node <node-name> fc-cri.io/enabled=true

# Run a test pod
kubectl apply -f deploy/kubernetes/example-pod.yaml

# Check pod status
kubectl get pod secure-workload

# Verify it's running in a Firecracker VM
kubectl describe pod secure-workload | grep -A5 "Events"
```

---

## Performance Tuning

### Pool Sizing

```toml
[pool]
size = 10        # For high-throughput: increase
min_size = 5     # For consistent latency: increase
max_idle_time = "10m"  # For cost savings: decrease
```

### Memory Optimization

```toml
[vm]
memory_mb = 64   # Minimum for small workloads
# memory_mb = 128  # Default, good for most
# memory_mb = 256  # For memory-heavy apps
```

### Kernel Args

```toml
kernel_args = "console=ttyS0 reboot=k panic=1 pci=off quiet loglevel=0"
# Add 'quiet loglevel=0' for faster boot (less console output)
```

---

## Security Considerations

### VM Isolation

- Each pod runs in a separate Firecracker microVM
- Hardware-level isolation via KVM
- Separate kernel, memory space, and filesystem

### Jailer (Production)

Enable the jailer for additional security:

```toml
[jailer]
enabled = true
uid = 1000
gid = 1000
chroot_base_dir = "/srv/jailer"
```

The jailer provides:

- Chroot isolation for Firecracker process
- Seccomp filtering
- Cgroup enforcement
- Dropped privileges

### Network Isolation

- Each VM gets its own network namespace
- CNI handles network policy enforcement
- No shared network stack with host

---

## Comparison with Alternatives

| Feature          | fc-cri      | Kata Containers | gVisor            |
| ---------------- | ----------- | --------------- | ----------------- |
| Isolation        | VM          | VM              | Syscall filtering |
| Memory overhead  | 64-128MB    | 160MB+          | 50-100MB          |
| Cold start       | <150ms      | 500ms+          | <100ms            |
| Compatibility    | High        | High            | Medium            |
| Hypervisor       | Firecracker | Multiple        | None              |
| Complexity       | Low         | High            | Medium            |
| Production ready | In progress | Yes             | Yes               |

---

## Contributing

1. Fork the repository
2. Create a feature branch
3. Write tests
4. Submit a pull request

### Code Style

- Follow Go conventions
- Domain-driven design principles
- Clear separation of concerns
- Comprehensive error handling

---

## References

- [Firecracker](https://firecracker-microvm.github.io/)
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk)
- [containerd Shim v2](https://github.com/containerd/containerd/blob/main/runtime/v2/README.md)
- [Kata Containers](https://katacontainers.io/)
- [CRI Specification](https://github.com/kubernetes/cri-api)

---

## License

Apache 2.0
