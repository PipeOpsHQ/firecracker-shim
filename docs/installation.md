# Installation Guide

This guide covers the installation of `firecracker-shim` in various environments.

## Prerequisites

Before installing, ensure your environment meets the following requirements:

### Hardware / VM

- **Architecture**: x86_64 (AMD64)
- **Virtualization**: KVM support is **required**.
  - **Bare Metal**: Enable VT-x/AMD-V in BIOS.
  - **Cloud (AWS)**: Use `.metal` instances (e.g., `c5.metal`) OR instances with nested virtualization enabled.
  - **Local (Linux)**: Verify with `kvm-ok` or check `ls -la /dev/kvm`.

### Software

- **OS**: Linux (Kernel 4.14+, recommended 5.10+)
- **Container Runtime**: `containerd` 1.7+
- **Kubernetes**: 1.24+ (if using with K8s)

---

## Installation Methods

### Method 1: Kubernetes DaemonSet (Recommended)

The easiest way to install on a Kubernetes cluster. This deploys a privileged pod to every node that installs the necessary binaries and configuration.

1.  **Deploy the Installer**:

    ```bash
    kubectl apply -f https://github.com/PipeOpsHQ/firecracker-shim/releases/latest/download/firecracker-shim-installer.yaml
    ```

2.  **Verify Installation**:
    Check the logs of the installer pods:

    ```bash
    kubectl -n kube-system logs -l app=firecracker-shim-installer
    ```

    You should see "Installation successful!".

3.  **Install RuntimeClass**:
    Apply the RuntimeClass definition to allow pods to request this runtime.
    ```yaml
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
        fc-cri.io/enabled: "true"
    ```
    Save as `runtime-class.yaml` and apply:
    ```bash
    kubectl apply -f runtime-class.yaml
    ```

### Method 2: Manual Installation (Linux Host)

For development or single-node setups without Kubernetes.

1.  **Download Release**:
    Get the latest release from [GitHub Releases](https://github.com/PipeOpsHQ/firecracker-shim/releases).

    ```bash
    VERSION="v0.1.0"
    wget https://github.com/PipeOpsHQ/firecracker-shim/releases/download/${VERSION}/firecracker-shim_${VERSION}_linux_amd64.tar.gz
    tar xf firecracker-shim_${VERSION}_linux_amd64.tar.gz
    ```

2.  **Install Binaries**:

    ```bash
    sudo cp containerd-shim-fc-v2 /usr/local/bin/
    sudo cp fc-agent /usr/local/bin/
    sudo cp fcctl /usr/local/bin/
    ```

3.  **Install Assets (Kernel & Rootfs)**:
    You need a compatible Linux kernel and a base rootfs for the VM.

    _Development/Test Assets:_

    ```bash
    sudo mkdir -p /var/lib/fc-cri/rootfs

    # Download test assets (example URLs - replace with your build or trusted source)
    # wget -O /var/lib/fc-cri/vmlinux https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux-5.10.186
    # wget -O /var/lib/fc-cri/rootfs/base.ext4 https://...
    ```

    _Alternatively, build them using `make kernel` and `make rootfs` in the repo._

4.  **Install Config**:

    ```bash
    sudo mkdir -p /etc/fc-cri
    sudo cp config.toml /etc/fc-cri/
    ```

5.  **Configure containerd**:
    Add the runtime plugin to `/etc/containerd/config.toml`:
    ```toml
    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker]
      runtime_type = "io.containerd.firecracker.v2"
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker.options]
        ConfigPath = "/etc/fc-cri/config.toml"
    ```
    Restart containerd:
    ```bash
    sudo systemctl restart containerd
    ```

---

## Verifying Installation

### 1. Check Component Health

Use the `fcctl` tool to verify the runtime components.

```bash
sudo fcctl health
```

Expected output:

```
[OK] Runtime is healthy
Components:
  [OK]  runtime_dir          ok
  [OK]  kvm                  ok
  [OK]  firecracker          ok
  [OK]  kernel               ok
  [OK]  rootfs               ok
```

### 2. Run a Test Pod

Create a pod using the `firecracker` runtime class.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-fc
spec:
  runtimeClassName: firecracker
  containers:
    - name: nginx
      image: nginx:alpine
```

```bash
kubectl apply -f pod.yaml
kubectl get pod test-fc -o wide
```

If the pod reaches `Running` state, congratulations! You are running a container inside a Firecracker microVM.

### 3. Inspect the VM

On the worker node where the pod is running:

```bash
sudo fcctl list
sudo fcctl inspect <sandbox-id>
```

---

## Uninstalling

### DaemonSet Uninstall

```bash
kubectl delete -f https://github.com/PipeOpsHQ/firecracker-shim/releases/latest/download/firecracker-shim-installer.yaml
```

_Note: This removes the installer, but binaries may persist on nodes depending on cleanup policy. To fully clean nodes, you may need to run a cleanup script._

### Manual Uninstall

```bash
# Remove config
sudo rm /etc/containerd/config.d/firecracker.toml
sudo systemctl restart containerd

# Remove binaries
sudo rm /usr/local/bin/containerd-shim-fc-v2
sudo rm /usr/local/bin/fc-agent
sudo rm /usr/local/bin/fcctl

# Remove assets
sudo rm -rf /var/lib/fc-cri
sudo rm -rf /etc/fc-cri
```
