# Security & Trust

Security is the primary driver for using Firecracker. This document outlines our threat model, security boundaries, and trust mechanisms.

## Threat Model

### The Goal
The primary security goal of `firecracker-shim` is to **protect the host infrastructure** (and by extension, other tenants) from untrusted or malicious workloads running inside Kubernetes pods.

### Security Boundaries

1.  **Hardware Virtualization (Strong)**
    *   **Boundary**: The KVM hypervisor.
    *   **Protection**: Prevents code running in the guest (kernel or user space) from accessing host memory or executing code on the host. This is the strongest layer of defense.

2.  **Jailer (Defense-in-Depth)**
    *   **Boundary**: Chroot, Cgroups, Seccomp on the host.
    *   **Protection**: Even if a process escapes the KVM boundary (e.g., via a QEMU/Firecracker vulnerability), the Firecracker process itself is trapped in a restrictive chroot, has dropped capabilities, and is limited by seccomp filters.

3.  **Pod Boundary**
    *   **Scope**: 1 MicroVM = 1 Pod.
    *   **Protection**: Containers *within* the same pod share the guest kernel and network namespace. They are **not** isolated from each other via virtualization. Malicious code in one container can potentially compromise others in the *same* pod, but cannot breach the VM to reach the host.

### What We Do Not Protect Against
*   **Side-Channel Attacks**: While Firecracker mitigates many speculative execution attacks (Spectre/Meltdown), complete immunity depends on host hardware and kernel patches.
*   **Intra-Pod Attacks**: We do not provide hard isolation between sidecars and app containers in the same pod.

---

## Secure Defaults

We ship with "secure by default" configurations:

*   **Jailer Enabled**: The shim expects to run Firecracker via the `jailer` binary.
*   **Seccomp Filters**: We apply Firecracker's strict seccomp filters to the VMM process.
*   **Dropped Privileges**: The VMM runs as a non-root user (`uid: 1000`) inside the jail.
*   **Network Isolation**: VMs are connected via TAP devices; they cannot sniff traffic from other VMs on the bridge unless specifically configured (promiscuous mode is off).

## Artifact Provenance & Trust

When you install `firecracker-shim`, you are running binary code on your cluster. Here is how we build trust:

### 1. Binaries
*   **Source**: Built from this repository using GitHub Actions.
*   **Reproducibility**: Builds are run in standard runners. We aim for reproducible builds (future roadmap).
*   **Checksums**: Every release includes a `checksums.txt` containing SHA256 hashes of all artifacts.

### 2. Installer Image
*   **Source**: `ghcr.io/pipeopshq/firecracker-shim-installer`
*   **Contents**:
    *   `firecracker-shim` binaries (built by us).
    *   `firecracker` and `jailer` binaries (downloaded directly from [AWS Firecracker releases](https://github.com/firecracker-microvm/firecracker/releases)).
    *   `vmlinux` kernel (built by us or sourced from AWS examples).
    *   `base.ext4` rootfs (built via `scripts/create-rootfs.sh` using Alpine Linux).

### 3. Release Signing
*   We publish SHA256 checksums for all release artifacts.
*   *Future*: We plan to sign releases using **Cosign** / Sigstore for verifiable supply chain security.

## DaemonSet Installer Security

The installer runs as a **Privileged Pod** because it must:
1.  Write executable binaries to the host (`/usr/local/bin`).
2.  Restart the system service (`containerd`).

**Risk Mitigation:**
*   **Transparency**: The installer script is simple bash (`deploy/installer/install.sh`). You can audit it.
*   **Minimal Base**: We use `alpine` as the base image.
*   **Least Privilege (Planned)**: We are exploring ways to reduce privileges, but host modification inherently requires root access.

## Reporting Vulnerabilities

If you find a security issue, please **DO NOT** open a public issue.
Email **security@pipeops.io** with details. We will respond within 48 hours.
