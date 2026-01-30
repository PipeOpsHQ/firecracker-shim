# Frequently Asked Questions

## Comparisons

### How is this different from `firecracker-containerd`?

AWS's `firecracker-containerd` uses a **daemon-centric** architecture designed for hyperscale density (thousands of microVMs per node). It acts as a central manager for all VMs and often requires complex storage setups (Device Mapper) and sometimes a patched `containerd`.

**firecracker-shim** uses the modern **Shim v2** architecture:

- **Standard Integration**: We work with unmodified, upstream `containerd`.
- **Failure Isolation**: Each pod has its own shim process. If a shim crashes, only that specific pod is affected—not the entire node.
- **Operational Simplicity**: We use simple file-backed images (`.ext4`) and standard CNI networking. You don't need to be a storage engineer to run it.

**Choose `firecracker-containerd`** if you are building an AWS Lambda competitor with thousands of functions per server.
**Choose `firecracker-shim`** if you want secure Kubernetes pods with the ease of use of standard containers.

### How is this different from Kata Containers?

Kata Containers is a mature project that supports multiple hypervisors (QEMU, Cloud Hypervisor, Firecracker). While powerful, this flexibility creates abstraction layers that add weight.

**firecracker-shim** is **opinionated and optimized**:

- **Single Focus**: We only support Firecracker. No abstraction layers for QEMU.
- **Lighter**: Our per-pod memory overhead is ~64MB vs Kata's ~160MB+.
- **Simpler**: Our agent is a static ~2MB binary, not a complex systemd-based guest image.

### How is this different from gVisor?

gVisor uses **syscall emulation** (a kernel written in Go running in userspace) to isolate containers. Firecracker uses **hardware virtualization** (KVM).

- **Security**: Both provide excellent isolation. Hardware virtualization (KVM) is generally considered the "gold standard" boundary.
- **Performance**: gVisor can be faster for some operations but slower for syscall-heavy workloads (network/IO). Firecracker behaves like a real (albeit minimal) Linux server.
- **Compatibility**: Firecracker runs a real Linux kernel. gVisor emulates it. If your app relies on obscure kernel features, it's more likely to work in Firecracker (provided the kernel has the modules).

---

## Infrastructure

### Can I run this on AWS EC2, GCP, or Azure?

**Yes, but with requirements.**
Firecracker requires KVM (Kernel-based Virtual Machine). Since cloud instances are already VMs, you need **Nested Virtualization**.

- **AWS**: You typically need **Bare Metal** instances (e.g., `c5.metal`, `m5.metal`). Standard instances (like `t3.medium`) do **not** support nested KVM.
- **GCP**: You must explicitly enable nested virtualization when creating the VM image or instance.
- **Azure**: The Dv3 and Ev3 series generally support nested virtualization.

### Can I run without KVM for testing?

**Technically yes, via QEMU emulation.**
It is possible to run Firecracker inside a QEMU VM using software emulation (TCG) on non-metal cloud instances. This allows you to test the setup, but **performance will be extremely slow**. This is strictly for development/CI and not suitable for running workloads.

### Does it support GPUs?

**Not yet.**
Firecracker's design philosophy prioritizes security and minimalism, so PCI passthrough support is limited compared to QEMU. We are monitoring upstream Firecracker features for GPU support possibilities.

### Do you support ARM64?

**Planned.**
Firecracker supports ARM64, and our architecture supports it. We plan to add ARM64 build targets in the v0.2 roadmap.

### What is PVM (Pagetable-based Virtual Machine)?

**PVM** is an emerging virtualization framework that allows running secure containers (like Firecracker) without hardware-assisted nested virtualization.

- **Why it matters**: Currently, running Firecracker inside a cloud VM requires **Nested Virtualization**. Many cloud providers disable this or only offer it on expensive Bare Metal instances.
- **How it works**: PVM decouples the secure container from hardware virtualization requirements by using a modified host kernel and a Position-Independent Executable (PIE) guest kernel.
- **Status in firecracker-shim**: Researching. Implementing PVM would enable our shim to run on any standard cloud instance, significantly reducing infrastructure costs.

#### Requirements for PVM

- **Patched Host Kernel**: The host must run a Linux kernel with the PVM RFC patchset (introducing the `kvm-pvm` vendor module).
- **PIE Guest Kernel**: The guest OS must use a Position-Independent Executable (PIE) kernel that supports running in hardware Ring 3.
- **x86_64 Architecture**: Current PVM research is specific to x86_64 and requires Shadow Paging.

#### Known Limitations

- **Performance Overhead**: PVM relies on **Shadow Paging**, which can lead to performance degradation in workloads that frequently modify page tables (e.g., high process fork rates). Long-running services like Java apps typically perform well.
- **Feature Gaps**: Direct SMAP/SMEP is not supported (requires emulation via PKU/NX). LDT and full PMU virtualization are currently not implemented in the initial PVM specifications.
- **Instruction Emulation**: Certain privileged instructions must be emulated in software since the guest runs in hardware Ring 3.

---

## Operations

### Why is my pod stuck in `ContainerCreating`?

This usually means the shim failed to initialize the VM.

1.  **Check KVM**: Ensure `/dev/kvm` exists and is writable by the container runtime user (usually root).
2.  **Check Logs**: Run `sudo fcctl logs <sandbox-id>`.
3.  **Check Config**: Ensure `/var/lib/fc-cri/vmlinux` and `/var/lib/fc-cri/rootfs/base.ext4` exist.

### Can I use my own Linux Kernel?

**Yes.**
The default kernel is minimal (~5MB) for speed. If you need specific modules (e.g., for specific networking protocols or filesystems like XFS), you can provide your own kernel.
See [Using a Custom Kernel](operations.md#advanced-using-a-custom-kernel) in the Operations Guide.

### How do updates work?

The shim is stateless.

1.  Update the binaries on the host (or update the DaemonSet).
2.  Restart `containerd`.
3.  **Existing pods** continue running with the _old_ shim process.
4.  **New pods** will use the updated shim.
    To fully upgrade, you must drain the node or delete/recreate the pods.
