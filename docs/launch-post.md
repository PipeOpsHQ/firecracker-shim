# Introducing firecracker-shim: Kubernetes-native Firecracker, Simplified

Today we're releasing **firecracker-shim** (fc-cri), a lightweight container runtime that brings Firecracker microVM isolation to Kubernetes without the complexity of existing solutions.

## The Problem: "Secure" Containers Are Hard

If you want stronger isolation than standard Linux containers (cgroups/namespaces), you typically have two choices:

1. **Kata Containers**: Powerful, enterprise-ready, but heavy. It supports QEMU, Cloud Hypervisor, Firecracker, and more—but that flexibility brings significant architectural complexity and operational overhead (160MB+ memory per pod).
2. **Firecracker directly**: Lean and fast, but hard to integrate. You have to build your own orchestration, networking, and image handling.

We wanted the **leanness of Firecracker** with the **simplicity of a standard runc-like shim**.

## What We Built

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

### 🚀 Speed & Efficiency

- **<150ms Cold Starts**: From pod creation to running container.
- **<50ms Warm Starts**: Pre-warmed VM pool for instant provisioning.
- **64MB Overhead**: Run thousands of secure pods on a single node.

### 🛡️ Real Isolation

Each pod gets its own kernel. If a container breaks out, it’s trapped in the microVM, protecting the host and other tenants.

### 🔌 Kubernetes Native

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
   git clone https://github.com/pipeops/firecracker-cri
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

Check out the code on [GitHub](https://github.com/pipeops/firecracker-cri) and let us know what you think!
