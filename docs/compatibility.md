# Compatibility Matrix

This document tracks the compatibility of `firecracker-shim` with various upstream components and environment versions.

## Core Components

| Component               | Tested Versions | Minimum Required | Notes                                                    |
| :---------------------- | :-------------- | :--------------- | :------------------------------------------------------- |
| **Kubernetes**          | 1.24 - 1.29     | 1.24+            | Requires `RuntimeClass` support.                         |
| **containerd**          | 1.7.0+          | 1.7.0+           | Shim v2 API is stable since 1.6, but we validate on 1.7. |
| **Firecracker**         | v1.6.0          | v1.3.0           | Uses `firecracker-go-sdk` compatibility.                 |
| **Linux Kernel (Host)** | 5.10, 6.1       | 4.14             | Requires KVM and `vhost_vsock` module.                   |

## CNI Plugins

We support standard CNI plugins via the bridge setup.

| Plugin          | Status         | Notes                                                    |
| :-------------- | :------------- | :------------------------------------------------------- |
| **bridge**      | [Supported]    | Default configuration.                                   |
| **ptp**         | [Supported]    | Point-to-point setup works well.                         |
| **flannel**     | [Supported]    | Standard overlay works via bridge.                       |
| **calico**      | [Supported]    | Requires standard CNI config (not eBPF mode).            |
| **aws-vpc-cni** | [Experimental] | Requires specific interface handling inside VM.          |
| **cilium**      | [Experimental] | eBPF acceleration features are not passed through to VM. |

## Guest Kernels

The guest kernel running inside the microVM must support virtio drivers.

| Kernel Source        | Version | Status       | Notes                                                |
| :------------------- | :------ | :----------- | :--------------------------------------------------- |
| **PipeOps Minimal**  | 6.1.x   | [Default]    | ~5MB, optimized for speed. No module support.        |
| **AWS Firecracker**  | 5.10.x  | [Compatible] | Official AWS kernel.                                 |
| **Ubuntu / Generic** | 5.x+    | [Heavy]      | Works but increases boot time significantly (~1-2s). |

## Architecture

| Arch                | Status    | Notes                                                    |
| :------------------ | :-------- | :------------------------------------------------------- |
| **AMD64 (x86_64)**  | [Stable]  | Primary development platform.                            |
| **ARM64 (aarch64)** | [Planned] | Requires different kernel/rootfs and Firecracker binary. |
