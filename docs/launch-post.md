# Introducing firecracker-shim: Firecracker for Kubernetes, Simplified

Today we're releasing **firecracker-shim** (fc-cri), a lightweight container runtime that brings Firecracker microVM isolation to Kubernetes without the complexity of existing solutions.

## Why Another Runtime?

If you want stronger isolation than standard Linux containers (cgroups/namespaces), you typically look at two options: **Kata Containers** or **firecracker-containerd**. Both are excellent, but we found them operationally complex for our needs.

### vs. firecracker-containerd

AWS's `firecracker-containerd` pioneered this space, but it uses a **daemon-based architecture** that sits between containerd and the VMs. This adds complexity to debugging, image handling (often requiring custom snapshotters), and networking setup.

**firecracker-shim** takes a different approach:

- **Shim v2 Architecture**: No middleman daemon. containerd talks directly to our shim, which talks directly to Firecracker.
- **Standard OCI Images**: No complex device mapper setup. We convert standard Docker images to ext4 block devices on the fly.
- **Standard Networking**: We use standard CNI plugins via a bridge, so your existing Calico/Flannel/AWS-VPC-CNI just works.

### vs. Kata Containers

Kata is a powerful, multi-hypervisor runtime (QEMU, Cloud Hypervisor, etc.). That flexibility comes with abstraction overhead (~160MB+ memory per pod vs our ~64MB) and a larger architectural footprint.

We built **firecracker-shim** to be:

1.  **Single-purpose**: Optimized solely for Firecracker.
2.  **Lean**: Minimal agent (~2MB), minimal kernel, minimal overhead.
3.  **Fast**: Sub-50ms warm starts via VM pooling.

## Architecture

**firecracker-shim** is a purpose-built containerd shim (v2) that maps Kubernetes Pods 1:1 to Firecracker microVMs.

```
Kubernetes → kubelet → containerd → firecracker-shim → Firecracker VM
                                                            ↓
                                                       fc-agent → runc → container
```

It’s designed with a "less is more" philosophy:

- **No proxy sidecars**: Direct communication via vsock.
- **No complex agents**: A minimal 2MB static agent inside the VM.
- **No multi-hypervisor abstraction**: Optimized solely for Firecracker.

## Key Features

### Speed & Efficiency

- **<150ms Cold Starts**: From pod creation to running container.
- **<50ms Warm Starts**: Pre-warmed VM pool for instant provisioning.
- **64MB Overhead**: Run thousands of secure pods on a single node.

### Real Isolation

Each pod gets its own kernel. If a container breaks out, it’s trapped in the microVM, protecting the host and other tenants.

### Kubernetes Native

It works out of the box with standard Kubernetes networking (CNI) and storage. Use it with a simple RuntimeClass:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: secure-workload
spec:
  runtimeClassName: firecracker
  containers:
    - name: app
      image: nginx:alpine
```

## How It Works

1. **VM Pooling**: We maintain a pool of "paused" microVMs booted with a minimal kernel and agent.
2. **Hot Plug**: When a pod is scheduled, we grab a VM and hot-attach the container's root filesystem (converted from the OCI image).
3. **CNI Bridge**: We wire the VM's tap device to the CNI network, making the pod indistinguishable from a regular container on the network.

## Getting Started

You can try it today on any Linux machine with KVM support.

1. **Install**:

   ```bash
   git clone https://github.com/PipeOpsHQ/firecracker-shim
   make install
   ```

2. **Configure Kubernetes**:

   ```bash
   kubectl apply -f deploy/kubernetes/runtime-class.yaml
   ```

3. **Run**:
   ```bash
   kubectl apply -f deploy/kubernetes/example-pod.yaml
   ```

## What's Next?

This is an initial release (v0.1) but it's already feature-rich. We've implemented:

- **Snapshot Support**: Sub-10ms restore times for lightning-fast scaling.
- **Production Hardening**: Full jailer integration with chroot and cgroups isolation.

Our roadmap for v0.2 includes:

- **Multi-arch Support**: ARM64 builds for running on Graviton/Ampere.
- **Conformance**: Passing 100% of the Kubernetes e2e suite.

Check out the code on [GitHub](https://github.com/PipeOpsHQ/firecracker-shim) and let us know what you think!
