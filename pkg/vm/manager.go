// Package vm implements Firecracker VM lifecycle management.
package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// Manager implements domain.VMManager for Firecracker VMs.
type Manager struct {
	mu sync.RWMutex

	config     ManagerConfig
	log        *logrus.Entry
	sandboxes  map[string]*domain.Sandbox
	cidCounter uint32 // For generating unique vsock CIDs
}

// ManagerConfig holds configuration for the VM manager.
type ManagerConfig struct {
	// FirecrackerBinary is the path to the firecracker binary.
	FirecrackerBinary string

	// RuntimeDir is the directory for runtime state (sockets, etc.).
	RuntimeDir string

	// DefaultKernelPath is the default kernel to use.
	DefaultKernelPath string

	// DefaultKernelArgs are the default kernel boot arguments.
	DefaultKernelArgs string

	// JailerBinary is the path to the jailer binary (optional).
	JailerBinary string

	// EnableJailer controls whether to use the jailer.
	EnableJailer bool
}

// DefaultManagerConfig returns a sensible default configuration.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		FirecrackerBinary: "/usr/bin/firecracker",
		RuntimeDir:        "/run/fc-cri",
		DefaultKernelPath: "/var/lib/fc-cri/vmlinux",
		DefaultKernelArgs: "console=ttyS0 reboot=k panic=1 pci=off quiet",
		JailerBinary:      "/usr/bin/jailer",
		EnableJailer:      false, // Start simple, add jailer later
	}
}

// NewManager creates a new VM manager.
func NewManager(config ManagerConfig, log *logrus.Entry) (*Manager, error) {
	// Ensure runtime directory exists
	if err := os.MkdirAll(config.RuntimeDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create runtime dir: %w", err)
	}

	return &Manager{
		config:     config,
		log:        log.WithField("component", "vm-manager"),
		sandboxes:  make(map[string]*domain.Sandbox),
		cidCounter: 3, // CIDs start at 3 (0=hypervisor, 1=reserved, 2=host)
	}, nil
}

// CreateVM creates and starts a new Firecracker microVM.
func (m *Manager) CreateVM(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error) {
	// Generate unique sandbox ID
	sandboxID := generateID()
	sandbox := domain.NewSandbox(sandboxID)

	m.log.WithField("sandbox_id", sandboxID).Info("Creating VM")

	// Assign vsock CID
	m.mu.Lock()
	sandbox.VsockCID = m.cidCounter
	m.cidCounter++
	m.mu.Unlock()

	// Setup paths
	sandboxDir := filepath.Join(m.config.RuntimeDir, sandboxID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create sandbox dir: %w", err)
	}

	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	sandbox.VsockPath = vsockPath

	// Apply defaults
	if config.KernelPath == "" {
		config.KernelPath = m.config.DefaultKernelPath
	}
	if config.KernelArgs == "" {
		config.KernelArgs = m.config.DefaultKernelArgs
	}

	// Build Firecracker configuration
	fcConfig := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: config.KernelPath,
		KernelArgs:      config.KernelArgs,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(config.VcpuCount),
			MemSizeMib: firecracker.Int64(config.MemoryMB),
			Smt:        firecracker.Bool(config.SMTEnabled),
		},
		// Vsock for guest-host communication
		VsockDevices: []firecracker.VsockDevice{
			{
				Path: vsockPath,
				CID:  uint32(sandbox.VsockCID),
			},
		},
	}

	// Add root drive if specified
	if config.RootDrive.PathOnHost != "" {
		fcConfig.Drives = []models.Drive{
			{
				DriveID:      firecracker.String(config.RootDrive.DriveID),
				PathOnHost:   firecracker.String(config.RootDrive.PathOnHost),
				IsRootDevice: firecracker.Bool(config.RootDrive.IsRoot),
				IsReadOnly:   firecracker.Bool(config.RootDrive.IsReadOnly),
			},
		}
	}

	// Create the machine
	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logrus.NewEntry(logrus.StandardLogger())),
	}

	machine, err := firecracker.NewMachine(ctx, fcConfig, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create machine: %w", err)
	}

	// Start the VM
	if err := machine.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start machine: %w", err)
	}

	// Update sandbox state
	sandbox.VM = machine
	sandbox.VMConfig = config
	pid, _ := machine.PID()
	sandbox.PID = pid
	sandbox.State = domain.SandboxReady
	sandbox.StartedAt = time.Now()

	// Store sandbox
	m.mu.Lock()
	m.sandboxes[sandboxID] = sandbox
	m.mu.Unlock()

	m.log.WithFields(logrus.Fields{
		"sandbox_id": sandboxID,
		"pid":        sandbox.PID,
		"cid":        sandbox.VsockCID,
	}).Info("VM started successfully")

	return sandbox, nil
}

// StopVM gracefully stops a VM.
func (m *Manager) StopVM(ctx context.Context, sandbox *domain.Sandbox) error {
	m.log.WithField("sandbox_id", sandbox.ID).Info("Stopping VM")

	if sandbox.VM == nil {
		return fmt.Errorf("sandbox %s has no VM", sandbox.ID)
	}

	// Try graceful shutdown first
	if err := sandbox.VM.Shutdown(ctx); err != nil {
		m.log.WithError(err).Warn("Graceful shutdown failed, forcing stop")
		_ = sandbox.VM.StopVMM()
	}

	// Wait for process exit
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := sandbox.VM.Wait(waitCtx); err != nil {
		m.log.WithError(err).Warn("Wait for VM exit failed")
	}

	sandbox.State = domain.SandboxStopped
	sandbox.FinishedAt = time.Now()

	return nil
}

// DestroyVM forcefully terminates a VM and cleans up resources.
func (m *Manager) DestroyVM(ctx context.Context, sandbox *domain.Sandbox) error {
	m.log.WithField("sandbox_id", sandbox.ID).Info("Destroying VM")

	// Stop the VM if running
	if sandbox.State == domain.SandboxReady {
		if err := m.StopVM(ctx, sandbox); err != nil {
			m.log.WithError(err).Warn("Error stopping VM during destroy")
		}
	}

	// Close agent connection if open
	if sandbox.AgentConn != nil {
		sandbox.AgentConn.Close()
	}

	// Clean up sandbox directory
	sandboxDir := filepath.Join(m.config.RuntimeDir, sandbox.ID)
	if err := os.RemoveAll(sandboxDir); err != nil {
		m.log.WithError(err).Warn("Failed to clean up sandbox directory")
	}

	// Remove from tracking
	m.mu.Lock()
	delete(m.sandboxes, sandbox.ID)
	m.mu.Unlock()

	return nil
}

// PauseVM suspends VM execution.
func (m *Manager) PauseVM(ctx context.Context, sandbox *domain.Sandbox) error {
	if sandbox.VM == nil {
		return fmt.Errorf("sandbox %s has no VM", sandbox.ID)
	}
	return sandbox.VM.PauseVM(ctx)
}

// ResumeVM resumes a paused VM.
func (m *Manager) ResumeVM(ctx context.Context, sandbox *domain.Sandbox) error {
	if sandbox.VM == nil {
		return fmt.Errorf("sandbox %s has no VM", sandbox.ID)
	}
	return sandbox.VM.ResumeVM(ctx)
}

// GetSandbox retrieves a sandbox by ID.
func (m *Manager) GetSandbox(id string) (*domain.Sandbox, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sandboxes[id]
	return s, ok
}

// ListSandboxes returns all managed sandboxes.
func (m *Manager) ListSandboxes() []*domain.Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*domain.Sandbox, 0, len(m.sandboxes))
	for _, s := range m.sandboxes {
		result = append(result, s)
	}
	return result
}

// generateID creates a unique identifier.
func generateID() string {
	// In production, use uuid or similar
	return fmt.Sprintf("fc-%d", time.Now().UnixNano())
}
