# Security Policy

## Supported Versions

Only the latest release is currently supported with security updates.

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1.0 | :x:                |

## Reporting a Vulnerability

We take the security of this project seriously. If you discover a vulnerability, please report it privately.

**Do NOT open a public GitHub issue.**

Please email **security@pipeops.io** with a description of the vulnerability. We will acknowledge your report within 48 hours and provide a timeline for triage and resolution.

## Threat Model

### Scope
`firecracker-shim` is designed to protect the **host infrastructure** from untrusted container workloads. It does this by wrapping each Kubernetes Pod in a Firecracker microVM.

### Guarantees
*   **Host Isolation**: A compromise of the container kernel should not lead to a compromise of the host kernel (VM boundary).
*   **Tenant Isolation**: Pods are isolated from each other by the hypervisor.

### Non-Guarantees
*   **Intra-Pod Isolation**: Containers *within the same pod* share the microVM kernel and network namespace. They are not isolated from each other.
*   **Side-Channel Attacks**: While Firecracker mitigates many speculative execution attacks (Spectre/Meltdown), we rely on the upstream Firecracker VMM for these guarantees.

### Installer Security
The provided DaemonSet installer runs as a **privileged container** to modify host binaries and configuration.
*   **Trust**: You must trust the `ghcr.io/pipeopshq/firecracker-shim-installer` image.
*   **Audit**: Verify the checksums of the release artifacts against the `checksums.txt` provided in the GitHub Release.

## Hardening Recommendations

For production deployments, we strongly recommend:

1.  **Enable Jailer**: Ensure `enable_jailer = true` in `config.toml`. This runs the VMM in a chroot with dropped privileges.
2.  **Seccomp**: Use high seccomp levels (level 2) in the configuration.
3.  **Kernel**: Use a hardened guest kernel (latest stable).
4.  **Network**: Use restrictive CNI network policies.
