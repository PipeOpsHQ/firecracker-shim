// Package vm provides jailer integration for production security isolation.
//
// The Firecracker jailer provides defense-in-depth by:
// - Running each VM in a chroot environment
// - Dropping privileges to a non-root user/group
// - Setting up cgroups for resource control
// - Limiting the filesystem view to only required files
// - Applying seccomp filters
//
// In production, always enable the jailer for untrusted workloads.
// The jailer adds ~5ms to VM startup but provides significant security benefits.
//
// Jailer directory structure:
//
//	/srv/jailer/firecracker/<id>/root/
//	├── dev/
//	│   ├── kvm
//	│   ├── net/tun
//	│   ├── null
//	│   ├── urandom
//	│   └── vsock (if enabled)
//	├── run/
//	│   └── firecracker.socket
//	├── kernel -> /var/lib/fc-cri/vmlinux (bind mount)
//	└── rootfs.ext4 -> /path/to/rootfs (bind mount or copy)
package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// JailerManager manages jailed Firecracker instances.
type JailerManager struct {
	mu sync.Mutex

	config JailerConfig
	log    *logrus.Entry

	// Track jailed VMs for cleanup
	jailedVMs map[string]*JailedVM
}

// JailerConfig configures the jailer.
type JailerConfig struct {
	// Enabled controls whether the jailer is used.
	Enabled bool

	// JailerBinary is the path to the jailer binary.
	JailerBinary string

	// FirecrackerBinary is the path to the firecracker binary.
	FirecrackerBinary string

	// ChrootBaseDir is the base directory for chroot environments.
	ChrootBaseDir string

	// UID is the user ID to run Firecracker as.
	UID int

	// GID is the group ID.
	GID int

	// NumaNode is the NUMA node to pin the VM to (-1 for no pinning).
	NumaNode int

	// CgroupVersion is the cgroup version: "1" or "2".
	CgroupVersion string

	// CgroupParent is the parent cgroup for VM cgroups.
	CgroupParent string

	// NetNS is the network namespace path (empty for new namespace).
	NetNS string

	// Daemonize controls whether the jailer daemonizes.
	Daemonize bool

	// SeccompLevel sets the seccomp filter level: 0=disabled, 1=basic, 2=advanced.
	SeccompLevel int

	// ResourceLimits contains default resource limits.
	ResourceLimits JailerResourceLimits
}

// JailerResourceLimits defines resource constraints for jailed VMs.
type JailerResourceLimits struct {
	// MaxOpenFiles is the RLIMIT_NOFILE limit.
	MaxOpenFiles uint64

	// MaxProcesses is the RLIMIT_NPROC limit.
	MaxProcesses uint64

	// MaxMemoryBytes is the memory limit (0 for unlimited).
	MaxMemoryBytes uint64

	// CPUWeight is the CPU weight for cgroup (1-10000).
	CPUWeight uint64

	// CPUQuota is the CPU quota in microseconds per period.
	CPUQuota int64

	// CPUPeriod is the CPU period in microseconds.
	CPUPeriod int64
}

// DefaultJailerConfig returns sensible defaults.
func DefaultJailerConfig() JailerConfig {
	return JailerConfig{
		Enabled:           false, // Opt-in for production
		JailerBinary:      "/usr/bin/jailer",
		FirecrackerBinary: "/usr/bin/firecracker",
		ChrootBaseDir:     "/srv/jailer",
		UID:               1000,
		GID:               1000,
		NumaNode:          -1,
		CgroupVersion:     "2",
		CgroupParent:      "fc-cri.slice",
		Daemonize:         true,
		SeccompLevel:      2,
		ResourceLimits: JailerResourceLimits{
			MaxOpenFiles: 2048,
			MaxProcesses: 100,
			CPUWeight:    100,
			CPUPeriod:    100000, // 100ms
		},
	}
}

// JailedVM represents a VM running inside a jail.
type JailedVM struct {
	// ID is the unique identifier (same as sandbox ID).
	ID string

	// ChrootDir is the chroot directory for this VM.
	ChrootDir string

	// SocketPath is the path to the API socket inside the chroot.
	SocketPath string

	// PID is the jailer process ID.
	PID int

	// CgroupPath is the cgroup for this VM.
	CgroupPath string

	// Config is the jailer configuration used.
	Config JailerConfig
}

// NewJailerManager creates a new jailer manager.
func NewJailerManager(config JailerConfig, log *logrus.Entry) (*JailerManager, error) {
	if !config.Enabled {
		return &JailerManager{
			config:    config,
			log:       log.WithField("component", "jailer"),
			jailedVMs: make(map[string]*JailedVM),
		}, nil
	}

	// Verify jailer binary exists
	if _, err := os.Stat(config.JailerBinary); err != nil {
		return nil, fmt.Errorf("jailer binary not found: %s", config.JailerBinary)
	}

	// Verify firecracker binary exists
	if _, err := os.Stat(config.FirecrackerBinary); err != nil {
		return nil, fmt.Errorf("firecracker binary not found: %s", config.FirecrackerBinary)
	}

	// Create base chroot directory
	if err := os.MkdirAll(config.ChrootBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create chroot base dir: %w", err)
	}

	// Create cgroup parent if using cgroups v2
	if config.CgroupVersion == "2" {
		cgroupPath := filepath.Join("/sys/fs/cgroup", config.CgroupParent)
		if err := os.MkdirAll(cgroupPath, 0755); err != nil {
			log.WithError(err).Warn("Failed to create cgroup parent")
		}
	}

	return &JailerManager{
		config:    config,
		log:       log.WithField("component", "jailer"),
		jailedVMs: make(map[string]*JailedVM),
	}, nil
}

// CreateJailedVM creates a new jailed Firecracker VM.
func (jm *JailerManager) CreateJailedVM(ctx context.Context, sandboxID string, vmConfig domain.VMConfig) (*JailedVM, *firecracker.Config, error) {
	if !jm.config.Enabled {
		return nil, nil, fmt.Errorf("jailer not enabled")
	}

	jm.log.WithField("sandbox_id", sandboxID).Info("Creating jailed VM")

	// Create chroot directory structure
	chrootDir := filepath.Join(jm.config.ChrootBaseDir, "firecracker", sandboxID, "root")
	if err := jm.setupChrootDir(chrootDir); err != nil {
		return nil, nil, fmt.Errorf("failed to setup chroot: %w", err)
	}

	// Setup device nodes
	if err := jm.setupDevices(chrootDir); err != nil {
		jm.cleanupChroot(chrootDir)
		return nil, nil, fmt.Errorf("failed to setup devices: %w", err)
	}

	// Bind mount kernel
	kernelDest := filepath.Join(chrootDir, "kernel")
	if err := jm.bindMount(vmConfig.KernelPath, kernelDest); err != nil {
		jm.cleanupChroot(chrootDir)
		return nil, nil, fmt.Errorf("failed to bind mount kernel: %w", err)
	}

	// Bind mount or copy rootfs
	if vmConfig.RootDrive.PathOnHost != "" {
		rootfsDest := filepath.Join(chrootDir, "rootfs.ext4")
		if err := jm.bindMount(vmConfig.RootDrive.PathOnHost, rootfsDest); err != nil {
			jm.cleanupChroot(chrootDir)
			return nil, nil, fmt.Errorf("failed to bind mount rootfs: %w", err)
		}
	}

	// Create the jailed VM object
	jailedVM := &JailedVM{
		ID:         sandboxID,
		ChrootDir:  chrootDir,
		SocketPath: filepath.Join(chrootDir, "run", "firecracker.socket"),
		Config:     jm.config,
	}

	// Setup cgroup
	if err := jm.setupCgroup(jailedVM); err != nil {
		jm.cleanupChroot(chrootDir)
		return nil, nil, fmt.Errorf("failed to setup cgroup: %w", err)
	}

	// Build Firecracker config for jailed execution
	fcConfig := jm.buildJailedConfig(jailedVM, vmConfig)

	// Track the jailed VM
	jm.mu.Lock()
	jm.jailedVMs[sandboxID] = jailedVM
	jm.mu.Unlock()

	jm.log.WithFields(logrus.Fields{
		"sandbox_id": sandboxID,
		"chroot":     chrootDir,
	}).Info("Jailed VM environment prepared")

	return jailedVM, &fcConfig, nil
}

// GetJailerArgs returns the command-line arguments for the jailer.
func (jm *JailerManager) GetJailerArgs(jailedVM *JailedVM, vmConfig domain.VMConfig) []string {
	args := []string{
		"--id", jailedVM.ID,
		"--exec-file", jm.config.FirecrackerBinary,
		"--uid", strconv.Itoa(jm.config.UID),
		"--gid", strconv.Itoa(jm.config.GID),
		"--chroot-base-dir", jm.config.ChrootBaseDir,
	}

	// NUMA pinning
	if jm.config.NumaNode >= 0 {
		args = append(args, "--numa-node", strconv.Itoa(jm.config.NumaNode))
	}

	// Cgroup configuration
	if jm.config.CgroupVersion == "2" {
		args = append(args, "--cgroup-version", "2")
	}
	if jm.config.CgroupParent != "" {
		args = append(args, "--parent-cgroup", jm.config.CgroupParent)
	}

	// Network namespace
	if jm.config.NetNS != "" {
		args = append(args, "--netns", jm.config.NetNS)
	}

	// Daemonize
	if jm.config.Daemonize {
		args = append(args, "--daemonize")
	}

	// Add separator for Firecracker args
	args = append(args, "--")

	// Firecracker arguments
	args = append(args,
		"--api-sock", "/run/firecracker.socket",
	)

	// Seccomp
	if jm.config.SeccompLevel > 0 {
		args = append(args, "--seccomp-level", strconv.Itoa(jm.config.SeccompLevel))
	}

	return args
}

// StartJailedVM starts the jailer with Firecracker.
func (jm *JailerManager) StartJailedVM(ctx context.Context, jailedVM *JailedVM, vmConfig domain.VMConfig) error {
	args := jm.GetJailerArgs(jailedVM, vmConfig)

	jm.log.WithFields(logrus.Fields{
		"sandbox_id": jailedVM.ID,
		"args":       args,
	}).Debug("Starting jailer")

	cmd := exec.CommandContext(ctx, jm.config.JailerBinary, args...)

	// Set resource limits
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Create new session to properly daemonize
		Setsid: true,
	}

	// Capture output for debugging
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jailer failed: %w: %s", err, output)
	}

	// If daemonized, we need to find the PID
	// The jailer writes the PID to a file
	pidFile := filepath.Join(jailedVM.ChrootDir, "..", "firecracker.pid")
	if data, err := os.ReadFile(pidFile); err == nil {
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		jailedVM.PID = pid
	}

	jm.log.WithFields(logrus.Fields{
		"sandbox_id": jailedVM.ID,
		"pid":        jailedVM.PID,
	}).Info("Jailed VM started")

	return nil
}

// DestroyJailedVM destroys a jailed VM and cleans up resources.
func (jm *JailerManager) DestroyJailedVM(ctx context.Context, sandboxID string) error {
	jm.mu.Lock()
	jailedVM, ok := jm.jailedVMs[sandboxID]
	if ok {
		delete(jm.jailedVMs, sandboxID)
	}
	jm.mu.Unlock()

	if !ok {
		return nil
	}

	jm.log.WithField("sandbox_id", sandboxID).Info("Destroying jailed VM")

	// Kill the jailer process if running
	if jailedVM.PID > 0 {
		process, err := os.FindProcess(jailedVM.PID)
		if err == nil {
			process.Kill()
			process.Wait()
		}
	}

	// Remove cgroup
	if jailedVM.CgroupPath != "" {
		os.RemoveAll(jailedVM.CgroupPath)
	}

	// Cleanup chroot
	if err := jm.cleanupChroot(jailedVM.ChrootDir); err != nil {
		jm.log.WithError(err).Warn("Failed to cleanup chroot")
	}

	return nil
}

// =============================================================================
// Internal Methods
// =============================================================================

func (jm *JailerManager) setupChrootDir(chrootDir string) error {
	// Create directory structure
	dirs := []string{
		chrootDir,
		filepath.Join(chrootDir, "dev"),
		filepath.Join(chrootDir, "dev", "net"),
		filepath.Join(chrootDir, "run"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}

	// Set ownership
	for _, dir := range dirs {
		if err := os.Chown(dir, jm.config.UID, jm.config.GID); err != nil {
			jm.log.WithError(err).Warn("Failed to chown directory")
		}
	}

	return nil
}

func (jm *JailerManager) setupDevices(chrootDir string) error {
	devices := []struct {
		path  string
		mode  uint32
		major uint32
		minor uint32
	}{
		// /dev/null
		{filepath.Join(chrootDir, "dev", "null"), syscall.S_IFCHR | 0666, 1, 3},
		// /dev/zero
		{filepath.Join(chrootDir, "dev", "zero"), syscall.S_IFCHR | 0666, 1, 5},
		// /dev/urandom
		{filepath.Join(chrootDir, "dev", "urandom"), syscall.S_IFCHR | 0666, 1, 9},
		// /dev/kvm
		{filepath.Join(chrootDir, "dev", "kvm"), syscall.S_IFCHR | 0660, 10, 232},
		// /dev/net/tun
		{filepath.Join(chrootDir, "dev", "net", "tun"), syscall.S_IFCHR | 0660, 10, 200},
	}

	for _, dev := range devices {
		// Remove if exists
		os.Remove(dev.path)

		// Create device node
		devNum := int(dev.major<<8 | dev.minor)
		if err := syscall.Mknod(dev.path, dev.mode, devNum); err != nil {
			// Try bind mount as fallback (for unprivileged setup)
			srcPath := strings.TrimPrefix(dev.path, chrootDir)
			if err := jm.bindMount(srcPath, dev.path); err != nil {
				jm.log.WithFields(logrus.Fields{
					"path":  dev.path,
					"error": err,
				}).Warn("Failed to create device node")
			}
		}

		// Set ownership
		os.Chown(dev.path, jm.config.UID, jm.config.GID)
	}

	return nil
}

func (jm *JailerManager) bindMount(src, dst string) error {
	// Create destination file/directory
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("source not found: %s", src)
	}

	if srcInfo.IsDir() {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
	} else {
		// Create parent directory
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		// Create empty file
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		f.Close()
	}

	// Bind mount using mount command (cross-platform)
	cmd := exec.Command("mount", "--bind", src, dst)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bind mount failed: %w: %s", err, output)
	}

	return nil
}

func (jm *JailerManager) setupCgroup(jailedVM *JailedVM) error {
	if jm.config.CgroupVersion == "2" {
		return jm.setupCgroupV2(jailedVM)
	}
	return jm.setupCgroupV1(jailedVM)
}

func (jm *JailerManager) setupCgroupV2(jailedVM *JailedVM) error {
	cgroupPath := filepath.Join("/sys/fs/cgroup", jm.config.CgroupParent, jailedVM.ID)

	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create cgroup: %w", err)
	}

	jailedVM.CgroupPath = cgroupPath

	// Configure CPU limits
	limits := jm.config.ResourceLimits

	if limits.CPUWeight > 0 {
		os.WriteFile(filepath.Join(cgroupPath, "cpu.weight"),
			[]byte(strconv.FormatUint(limits.CPUWeight, 10)), 0644)
	}

	if limits.CPUQuota > 0 && limits.CPUPeriod > 0 {
		// Format: $MAX $PERIOD
		quota := fmt.Sprintf("%d %d", limits.CPUQuota, limits.CPUPeriod)
		os.WriteFile(filepath.Join(cgroupPath, "cpu.max"), []byte(quota), 0644)
	}

	// Configure memory limits
	if limits.MaxMemoryBytes > 0 {
		os.WriteFile(filepath.Join(cgroupPath, "memory.max"),
			[]byte(strconv.FormatUint(limits.MaxMemoryBytes, 10)), 0644)
	}

	// Enable controllers
	os.WriteFile(filepath.Join(cgroupPath, "cgroup.subtree_control"),
		[]byte("+cpu +memory +io"), 0644)

	return nil
}

func (jm *JailerManager) setupCgroupV1(jailedVM *JailedVM) error {
	// Create cgroups in each controller
	controllers := []string{"cpu", "memory", "devices", "pids"}

	for _, ctrl := range controllers {
		cgroupPath := filepath.Join("/sys/fs/cgroup", ctrl, jm.config.CgroupParent, jailedVM.ID)
		if err := os.MkdirAll(cgroupPath, 0755); err != nil {
			continue
		}

		limits := jm.config.ResourceLimits

		switch ctrl {
		case "cpu":
			if limits.CPUQuota > 0 {
				os.WriteFile(filepath.Join(cgroupPath, "cpu.cfs_quota_us"),
					[]byte(strconv.FormatInt(limits.CPUQuota, 10)), 0644)
			}
			if limits.CPUPeriod > 0 {
				os.WriteFile(filepath.Join(cgroupPath, "cpu.cfs_period_us"),
					[]byte(strconv.FormatInt(limits.CPUPeriod, 10)), 0644)
			}

		case "memory":
			if limits.MaxMemoryBytes > 0 {
				os.WriteFile(filepath.Join(cgroupPath, "memory.limit_in_bytes"),
					[]byte(strconv.FormatUint(limits.MaxMemoryBytes, 10)), 0644)
			}

		case "pids":
			if limits.MaxProcesses > 0 {
				os.WriteFile(filepath.Join(cgroupPath, "pids.max"),
					[]byte(strconv.FormatUint(limits.MaxProcesses, 10)), 0644)
			}
		}
	}

	jailedVM.CgroupPath = filepath.Join("/sys/fs/cgroup/cpu", jm.config.CgroupParent, jailedVM.ID)
	return nil
}

func (jm *JailerManager) cleanupChroot(chrootDir string) error {
	// Unmount any bind mounts first
	mounts := []string{
		filepath.Join(chrootDir, "kernel"),
		filepath.Join(chrootDir, "rootfs.ext4"),
		filepath.Join(chrootDir, "dev", "kvm"),
		filepath.Join(chrootDir, "dev", "net", "tun"),
		filepath.Join(chrootDir, "dev", "null"),
		filepath.Join(chrootDir, "dev", "zero"),
		filepath.Join(chrootDir, "dev", "urandom"),
	}

	for _, mount := range mounts {
		syscall.Unmount(mount, 0)
	}

	// Remove the entire chroot tree
	// Go up one level to remove the ID directory too
	parentDir := filepath.Dir(chrootDir)
	return os.RemoveAll(parentDir)
}

func (jm *JailerManager) buildJailedConfig(jailedVM *JailedVM, vmConfig domain.VMConfig) firecracker.Config {
	// Paths are relative to chroot
	return firecracker.Config{
		SocketPath:      jailedVM.SocketPath,
		KernelImagePath: "/kernel",
		KernelArgs:      vmConfig.KernelArgs,
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String("rootfs"),
				PathOnHost:   firecracker.String("/rootfs.ext4"),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(vmConfig.RootDrive.IsReadOnly),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(vmConfig.VcpuCount),
			MemSizeMib: firecracker.Int64(vmConfig.MemoryMB),
			Smt:        firecracker.Bool(vmConfig.SMTEnabled),
		},
	}
}

// =============================================================================
// Utility Functions
// =============================================================================

// CheckJailerPrerequisites verifies the system is ready for jailed VMs.
func CheckJailerPrerequisites(config JailerConfig) error {
	var errors []string

	// Check jailer binary
	if _, err := os.Stat(config.JailerBinary); err != nil {
		errors = append(errors, fmt.Sprintf("jailer binary not found: %s", config.JailerBinary))
	}

	// Check firecracker binary
	if _, err := os.Stat(config.FirecrackerBinary); err != nil {
		errors = append(errors, fmt.Sprintf("firecracker binary not found: %s", config.FirecrackerBinary))
	}

	// Check /dev/kvm
	if _, err := os.Stat("/dev/kvm"); err != nil {
		errors = append(errors, "/dev/kvm not available")
	}

	// Check user exists
	// This is a simplified check - in production, verify with getpwuid
	if config.UID < 0 || config.UID > 65534 {
		errors = append(errors, fmt.Sprintf("invalid UID: %d", config.UID))
	}

	// Check cgroup mount
	if config.CgroupVersion == "2" {
		if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
			errors = append(errors, "cgroups v2 not mounted")
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("jailer prerequisites not met:\n  - %s", strings.Join(errors, "\n  - "))
	}

	return nil
}

// GetJailedSocketPath returns the API socket path for a jailed VM.
// This accounts for the chroot and is useful for connecting to the VM.
func GetJailedSocketPath(baseDir, sandboxID string) string {
	return filepath.Join(baseDir, "firecracker", sandboxID, "root", "run", "firecracker.socket")
}
