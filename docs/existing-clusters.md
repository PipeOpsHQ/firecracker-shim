# Running on Existing Clusters

This guide explains how to install and configure `firecracker-shim` on an existing Kubernetes cluster (e.g., EKS, GKE, bare metal).

## Prerequisites

Before proceeding, ensure your worker nodes meet these requirements:

1.  **Virtualization Support**: Nodes must support KVM.
    *   **Bare Metal**: Enabled in BIOS.
    *   **Cloud VMs**: Must support nested virtualization (e.g., AWS `.metal` instances, GCP nested virtualization).
    *   Check with: `ls -la /dev/kvm`
2.  **Container Runtime**: Nodes must use **containerd** (v1.6+).
    *   Docker shim (dockershim) is not supported.
    *   CRI-O is conceptually compatible but this guide focuses on containerd configuration.
3.  **OS**: Linux kernel 4.14+ (recommended 5.10+).

---

## Installation Methods

Choose one of the following methods to install the runtime components on your nodes.

### Method 1: DaemonSet Installer (Recommended)

We provide a DaemonSet that runs a privileged pod on every node to install the binaries, configuration, and assets.

**1. Apply the Installer Manifest**

```bash
kubectl apply -f https://raw.githubusercontent.com/PipeOpsHQ/firecracker-shim/main/deploy/installer/daemonset.yaml
```

**2. Wait for Installation**

The installer pod will:
1.  Check for KVM support (and sleep if missing).
2.  Copy binaries (`containerd-shim-fc-v2`, `fc-agent`, `firecracker`) to `/usr/local/bin`.
3.  Install the kernel and rootfs to `/var/lib/fc-cri`.
4.  Update containerd configuration.
5.  **Restart containerd** (Note: This may cause a momentary disruption to pod scheduling on the node).

Check the logs to verify success:

```bash
kubectl -n kube-system logs -l app=firecracker-shim-installer
```

### Method 2: Manual Installation (Ansible/User Data)

If you prefer to manage nodes via configuration management (Ansible, Chef, Terraform user_data), follow these steps for each node.

**1. Install Binaries**

Download the latest release and extract to `/usr/local/bin`:

```bash
VERSION="v0.1.0"
wget https://github.com/PipeOpsHQ/firecracker-shim/releases/download/${VERSION}/firecracker-shim-linux-amd64.tar.gz
tar xf firecracker-shim-linux-amd64.tar.gz -C /usr/local/bin/
```

**2. Install Assets**

You need the guest kernel and base rootfs.

```bash
mkdir -p /var/lib/fc-cri/rootfs
# Download or build these assets (see Build Guide)
cp vmlinux /var/lib/fc-cri/
cp base.ext4 /var/lib/fc-cri/rootfs/
```

**3. Configure Runtime**

Create the config file:

```bash
mkdir -p /etc/fc-cri
cp config.toml /etc/fc-cri/
```

**4. Configure containerd**

Add the runtime plugin configuration. If your containerd supports `config.d`:

```bash
# /etc/containerd/config.d/firecracker.toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker]
  runtime_type = "io.containerd.firecracker.v2"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker.options]
    ConfigPath = "/etc/fc-cri/config.toml"
```

Then restart containerd:

```bash
systemctl restart containerd
```

---

## Cluster Configuration

Once the nodes have the software installed, tell Kubernetes how to use it.

### 1. Create RuntimeClass

This defines the `firecracker` runtime class.

```yaml
# runtime-class.yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: firecracker
handler: firecracker  # Must match the name in containerd config
overhead:
  podFixed:
    memory: "64Mi"  # Overhead reservation per VM
    cpu: "100m"
scheduling:
  nodeSelector:
    fc-cri.io/enabled: "true" # Optional: restrict to specific nodes
```

```bash
kubectl apply -f runtime-class.yaml
```

### 2. Label Nodes

If you are using the `nodeSelector` in the RuntimeClass, label the compatible nodes:

```bash
kubectl label node <node-name> fc-cri.io/enabled=true
```

---

## Running Workloads

Deploy a pod using the `runtimeClassName`.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: secure-nginx
spec:
  runtimeClassName: firecracker
  containers:
    - name: nginx
      image: nginx:alpine
```

```bash
kubectl apply -f pod.yaml
```

Verify it is running:

```bash
kubectl get pod secure-nginx -o wide
```

You can also use `fcctl` on the node to verify the VM is running:

```bash
# On the worker node
sudo fcctl list
```

---

## Troubleshooting

### "RuntimeHandler not found" Error

**Symptom**: Pod stays in `ContainerCreating` with error `RuntimeHandler "firecracker" not supported`.

**Fix**:
1.  Verify containerd config has the `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.firecracker]` section.
2.  Verify `systemctl restart containerd` was run.
3.  Check `containerd` logs: `journalctl -u containerd -f`.

### "KVM not available"

**Symptom**: `fcctl health` shows KVM error or pod fails to start.

**Fix**:
1.  Check permissions: `ls -la /dev/kvm`.
2.  Ensure the user running the process (usually root for containerd) has access.
3.  On cloud instances, ensure you are using a supported instance type (e.g., AWS `i3.metal`).

### Image Pull Errors

**Symptom**: Pod fails with image pull or unpack errors.

**Fix**:
The shim relies on `fsify` and `skopeo` to convert images. Ensure these tools are installed and in the PATH if they are not bundled in your installer image.
