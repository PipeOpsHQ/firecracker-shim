# Comparison: Daemon vs. Shim Architecture

The Firecracker ecosystem offers two main approaches to running microVMs in Kubernetes: the **Daemon Model** (used by AWS's `firecracker-containerd`) and the **Shim Model** (used by `firecracker-shim`).

This guide explains the architectural differences, trade-offs, and why we chose the Shim model for this project.

---

## At a Glance

| Feature             | Daemon (`firecracker-containerd`)            | Shim (`firecracker-shim`)                      |
| :------------------ | :------------------------------------------- | :--------------------------------------------- |
| **Architecture**    | Centralized Monolith                         | Decentralized Processes (1 per pod)            |
| **Compatibility**   | **Custom**: Often needs patched `containerd` | **Standard**: Works with upstream `containerd` |
| **Blast Radius**    | **High**: Daemon crash affects all VMs       | **Low**: Shim crash affects only one pod       |
| **Storage**         | Complex Device Mapper (Required)             | Simple File-backed (ext4)                      |
| **Memory Overhead** | **Lowest** (Shared runtime)                  | **Low** (~10MB overhead per pod)               |
| **Debugging**       | Difficult (Shared logs, opaque state)        | Native (Process tree, `kill`, `ps`)            |
| **Setup**           | High Complexity (Custom LVM/storage)         | Plug-and-Play (Binary + Config)                |
| **Best For**        | Hyperscale Lambda-like density               | Kubernetes Pods, CI Runners, SaaS              |

---

## 1. The Daemon Model (AWS)

AWS built `firecracker-containerd` with a focus on extreme density (thousands of functions per node). To achieve this, they centralized management into a single long-running daemon.

### Why use a Daemon?

- **Memory Efficiency**: By sharing the Go runtime across all management threads, the per-VM overhead is negligible (kilobytes).
- **Centralized Storage**: It acts as the authority for Device Mapper (devmapper), preventing race conditions when managing block devices at scale.
- **Resilience**: It runs independently of `containerd`, so `containerd` upgrades don't affect running VMs.

### The Drawbacks

- **Specialized Binaries**: It relies on a control plugin compiled _into_ containerd, often requiring you to replace your system's standard `containerd` with a specialized build.
- **Single Point of Failure**: If the daemon crashes, deadlocks, or is killed by OOM, **every microVM on the node is orphaned or lost**.
- **Operational Complexity**: It requires a specific storage setup (LVM thin pools, devmapper) that is difficult to configure and debug compared to standard overlay filesystems.
- **Debugging**: Identifying why _one_ specific pod failed involves sifting through the logs of a daemon handling hundreds of concurrent operations.

---

## 2. The Shim Model (Us)

`firecracker-shim` adopts the **Containerd Shim v2** architecture, which has become the industry standard for runtimes like `runc`, `gvisor`, and `kata-containers`.

### Why use a Shim?

- **Isolation (Blast Radius)**: Each pod gets its own shim process. If a shim crashes due to a bug or memory issue, **only that specific pod fails**. The rest of the node remains stable.
- **Kubernetes Native Debugging**: The process tree reflects the pod structure.
  ```
  containerd
   └─ containerd-shim-fc-v2 (Pod A)
       └─ firecracker (VM A)
   └─ containerd-shim-fc-v2 (Pod B)
       └─ firecracker (VM B)
  ```
  You can identify, trace, or kill individual pods using standard Linux tools (`ps`, `top`, `kill`).
- **Simplicity**: We replaced complex device mapper requirements with simple file-backed images (`.ext4`). This allows the runtime to work on any Linux machine without specialized storage configuration.
- **Compatibility**: Because we implement the standard Shim v2 API, we work with unmodified, upstream `containerd` (v1.7+). No need to replace your existing container runtime binaries.

### The Drawbacks

- **Slightly Higher Memory**: Each shim process has its own Go runtime overhead (~10-15MB). On a node with 100 pods, this uses ~1.5GB of RAM for management. We consider this an acceptable trade-off for the stability and simplicity gained.

---

## Conclusion

We built `firecracker-shim` because we believe the **Shim Model** is superior for general-purpose Kubernetes workloads.

- **Choose the Daemon** (`firecracker-containerd`) if you are building a Lambda clone with 5,000+ tiny functions per node and possess a dedicated storage engineering team.
- **Choose the Shim** (`firecracker-shim`) if you want secure, isolated Kubernetes pods with standard operational tooling, high reliability, and easy setup.
