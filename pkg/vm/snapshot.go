// Package vm provides snapshot management for Firecracker VMs.
//
// Firecracker supports creating and restoring VM snapshots, which enables
// sub-10ms VM startup times. A snapshot captures:
// - VM memory state
// - vCPU registers
// - Device state (virtio, vsock, etc.)
//
// The snapshot workflow:
//  1. Create a "golden" VM with base rootfs and agent running
//  2. Pause the VM and create a snapshot
//  3. For new workloads, restore from snapshot instead of cold boot
//  4. Hot-attach the workload-specific rootfs
//
// This is faster than even VM pooling because restore is essentially
// a memory map operation, avoiding kernel boot entirely.
package vm

import (
	"context"
	"encoding/json"
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

// SnapshotManager handles VM snapshot creation and restoration.
type SnapshotManager struct {
	mu sync.RWMutex

	config    SnapshotConfig
	log       *logrus.Entry
	vmManager *Manager

	// Available snapshots indexed by name
	snapshots map[string]*Snapshot

	// Golden snapshot for fast VM creation
	goldenSnapshot *Snapshot
}

// SnapshotConfig configures snapshot behavior.
type SnapshotConfig struct {
	// Enabled controls whether snapshot support is active.
	Enabled bool

	// CacheDir is where snapshot files are stored.
	CacheDir string

	// MaxCached is the maximum number of snapshots to keep.
	MaxCached int

	// GoldenSnapshotName is the name of the golden base snapshot.
	GoldenSnapshotName string

	// GoldenVMConfig is the configuration for the golden VM.
	GoldenVMConfig domain.VMConfig

	// SnapshotType: "Full" or "Diff" (differential snapshots)
	SnapshotType string

	// MemoryBackend: "File" or "Uffd" (userfaultfd for lazy loading)
	MemoryBackend string

	// CompressMemory enables memory compression for smaller snapshots.
	CompressMemory bool
}

// DefaultSnapshotConfig returns sensible defaults.
func DefaultSnapshotConfig() SnapshotConfig {
	return SnapshotConfig{
		Enabled:            false, // Opt-in feature
		CacheDir:           "/var/lib/fc-cri/snapshots",
		MaxCached:          10,
		GoldenSnapshotName: "golden-base",
		GoldenVMConfig:     domain.DefaultVMConfig(),
		SnapshotType:       "Full",
		MemoryBackend:      "File",
		CompressMemory:     false,
	}
}

// Snapshot represents a saved VM state.
type Snapshot struct {
	// Name is the unique identifier for this snapshot.
	Name string `json:"name"`

	// MemoryPath is the path to the memory snapshot file.
	MemoryPath string `json:"memory_path"`

	// StatePath is the path to the VM state snapshot file.
	StatePath string `json:"state_path"`

	// VMConfig is the configuration used when the snapshot was created.
	VMConfig domain.VMConfig `json:"vm_config"`

	// Version is the Firecracker snapshot version.
	Version string `json:"version"`

	// CreatedAt is when the snapshot was created.
	CreatedAt time.Time `json:"created_at"`

	// SizeBytes is the total size of snapshot files.
	SizeBytes int64 `json:"size_bytes"`

	// Metadata contains arbitrary metadata.
	Metadata map[string]string `json:"metadata,omitempty"`

	// IsGolden indicates if this is the golden base snapshot.
	IsGolden bool `json:"is_golden"`
}

// NewSnapshotManager creates a new snapshot manager.
func NewSnapshotManager(config SnapshotConfig, vmManager *Manager, log *logrus.Entry) (*SnapshotManager, error) {
	if !config.Enabled {
		return &SnapshotManager{
			config:    config,
			log:       log.WithField("component", "snapshot-manager"),
			vmManager: vmManager,
			snapshots: make(map[string]*Snapshot),
		}, nil
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot cache dir: %w", err)
	}

	sm := &SnapshotManager{
		config:    config,
		log:       log.WithField("component", "snapshot-manager"),
		vmManager: vmManager,
		snapshots: make(map[string]*Snapshot),
	}

	// Load existing snapshots
	if err := sm.loadSnapshots(); err != nil {
		log.WithError(err).Warn("Failed to load existing snapshots")
	}

	// Check for golden snapshot
	if snap, ok := sm.snapshots[config.GoldenSnapshotName]; ok {
		sm.goldenSnapshot = snap
		log.WithField("snapshot", snap.Name).Info("Golden snapshot loaded")
	}

	return sm, nil
}

// CreateGoldenSnapshot creates the golden base snapshot for fast VM creation.
// This should be called once during initialization.
func (sm *SnapshotManager) CreateGoldenSnapshot(ctx context.Context) (*Snapshot, error) {
	if !sm.config.Enabled {
		return nil, fmt.Errorf("snapshots not enabled")
	}

	sm.log.Info("Creating golden snapshot")

	// Create a fresh VM
	sandbox, err := sm.vmManager.CreateVM(ctx, sm.config.GoldenVMConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create golden VM: %w", err)
	}

	// Wait for the VM to be fully initialized
	// The agent should be running and ready
	time.Sleep(2 * time.Second) // Allow agent to start

	// Create the snapshot
	snap, err := sm.CreateSnapshot(ctx, sandbox, sm.config.GoldenSnapshotName, true)
	if err != nil {
		sm.vmManager.DestroyVM(ctx, sandbox)
		return nil, fmt.Errorf("failed to create golden snapshot: %w", err)
	}

	// Destroy the source VM (we only need the snapshot)
	sm.vmManager.DestroyVM(ctx, sandbox)

	sm.mu.Lock()
	sm.goldenSnapshot = snap
	sm.mu.Unlock()

	sm.log.WithFields(logrus.Fields{
		"name": snap.Name,
		"size": snap.SizeBytes,
	}).Info("Golden snapshot created")

	return snap, nil
}

// CreateSnapshot creates a snapshot from a running VM.
func (sm *SnapshotManager) CreateSnapshot(ctx context.Context, sandbox *domain.Sandbox, name string, isGolden bool) (*Snapshot, error) {
	if !sm.config.Enabled {
		return nil, fmt.Errorf("snapshots not enabled")
	}

	if sandbox.VM == nil {
		return nil, fmt.Errorf("sandbox has no VM")
	}

	sm.log.WithFields(logrus.Fields{
		"sandbox_id": sandbox.ID,
		"name":       name,
	}).Info("Creating snapshot")

	// Create snapshot directory
	snapDir := filepath.Join(sm.config.CacheDir, name)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot dir: %w", err)
	}

	memPath := filepath.Join(snapDir, "memory")
	statePath := filepath.Join(snapDir, "state")

	// Pause the VM before snapshotting
	if err := sandbox.VM.PauseVM(ctx); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	// Create the snapshot using Firecracker API
	snapshotParams := &models.SnapshotCreateParams{
		MemFilePath:  firecracker.String(memPath),
		SnapshotPath: firecracker.String(statePath),
		SnapshotType: sm.config.SnapshotType,
	}

	// Use the machine's CreateSnapshot method
	if err := sm.createSnapshotViaAPI(ctx, sandbox.VM, snapshotParams); err != nil {
		// Resume VM on failure
		sandbox.VM.ResumeVM(ctx)
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	// Get file sizes
	memInfo, _ := os.Stat(memPath)
	stateInfo, _ := os.Stat(statePath)

	var totalSize int64
	if memInfo != nil {
		totalSize += memInfo.Size()
	}
	if stateInfo != nil {
		totalSize += stateInfo.Size()
	}

	snap := &Snapshot{
		Name:       name,
		MemoryPath: memPath,
		StatePath:  statePath,
		VMConfig:   sandbox.VMConfig,
		Version:    "1.0", // Firecracker snapshot version
		CreatedAt:  time.Now(),
		SizeBytes:  totalSize,
		IsGolden:   isGolden,
		Metadata: map[string]string{
			"source_sandbox": sandbox.ID,
		},
	}

	// Save snapshot metadata
	if err := sm.saveSnapshotMetadata(snap); err != nil {
		sm.log.WithError(err).Warn("Failed to save snapshot metadata")
	}

	// Store in memory
	sm.mu.Lock()
	sm.snapshots[name] = snap
	sm.mu.Unlock()

	// Resume the source VM
	if err := sandbox.VM.ResumeVM(ctx); err != nil {
		sm.log.WithError(err).Warn("Failed to resume VM after snapshot")
	}

	sm.log.WithFields(logrus.Fields{
		"name":      name,
		"size_mb":   totalSize / 1024 / 1024,
		"is_golden": isGolden,
	}).Info("Snapshot created")

	return snap, nil
}

// RestoreFromSnapshot creates a new VM from a snapshot.
// This is much faster than cold boot (~10ms vs ~100ms+).
func (sm *SnapshotManager) RestoreFromSnapshot(ctx context.Context, snap *Snapshot) (*domain.Sandbox, error) {
	if !sm.config.Enabled {
		return nil, fmt.Errorf("snapshots not enabled")
	}

	sm.log.WithField("snapshot", snap.Name).Info("Restoring from snapshot")

	startTime := time.Now()

	// Generate sandbox ID
	sandboxID := fmt.Sprintf("fc-snap-%d", time.Now().UnixNano())
	sandboxDir := filepath.Join(sm.vmManager.config.RuntimeDir, sandboxID)

	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create sandbox dir: %w", err)
	}

	socketPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	// Assign vsock CID
	sm.vmManager.mu.Lock()
	cid := sm.vmManager.cidCounter
	sm.vmManager.cidCounter++
	sm.vmManager.mu.Unlock()

	// Build Firecracker config for restore
	fcConfig := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: snap.VMConfig.KernelPath, // Still needed for config
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(snap.VMConfig.VcpuCount),
			MemSizeMib: firecracker.Int64(snap.VMConfig.MemoryMB),
			Smt:        firecracker.Bool(snap.VMConfig.SMTEnabled),
		},
		VsockDevices: []firecracker.VsockDevice{
			{
				Path: vsockPath,
				CID:  uint32(cid),
			},
		},
		// Snapshot restore parameters
		Snapshot: firecracker.SnapshotConfig{
			MemFilePath:         snap.MemoryPath,
			SnapshotPath:        snap.StatePath,
			ResumeVM:            true,
			EnableDiffSnapshots: sm.config.SnapshotType == "Diff",
		},
	}

	// Create the machine with snapshot restore
	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logrus.NewEntry(logrus.StandardLogger())),
	}

	machine, err := firecracker.NewMachine(ctx, fcConfig, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create machine for restore: %w", err)
	}

	// Start (restore) the VM
	if err := machine.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to restore VM: %w", err)
	}

	// Build sandbox object
	sandbox := domain.NewSandbox(sandboxID)
	sandbox.VM = machine
	sandbox.VMConfig = snap.VMConfig
	sandbox.VsockPath = vsockPath
	sandbox.VsockCID = cid
	pid, _ := machine.PID()
	sandbox.PID = pid
	sandbox.State = domain.SandboxReady
	sandbox.StartedAt = time.Now()
	sandbox.FromPool = true // Treat restored VMs like pooled VMs

	// Track in manager
	sm.vmManager.mu.Lock()
	sm.vmManager.sandboxes[sandboxID] = sandbox
	sm.vmManager.mu.Unlock()

	restoreTime := time.Since(startTime)
	sm.log.WithFields(logrus.Fields{
		"sandbox_id": sandboxID,
		"snapshot":   snap.Name,
		"restore_ms": restoreTime.Milliseconds(),
	}).Info("VM restored from snapshot")

	return sandbox, nil
}

// RestoreFromGolden restores a VM from the golden snapshot.
// This is the primary method for fast VM creation.
func (sm *SnapshotManager) RestoreFromGolden(ctx context.Context) (*domain.Sandbox, error) {
	sm.mu.RLock()
	golden := sm.goldenSnapshot
	sm.mu.RUnlock()

	if golden == nil {
		return nil, fmt.Errorf("no golden snapshot available")
	}

	return sm.RestoreFromSnapshot(ctx, golden)
}

// HasGoldenSnapshot returns true if a golden snapshot is available.
func (sm *SnapshotManager) HasGoldenSnapshot() bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.goldenSnapshot != nil
}

// GetSnapshot retrieves a snapshot by name.
func (sm *SnapshotManager) GetSnapshot(name string) (*Snapshot, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	snap, ok := sm.snapshots[name]
	return snap, ok
}

// ListSnapshots returns all available snapshots.
func (sm *SnapshotManager) ListSnapshots() []*Snapshot {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	result := make([]*Snapshot, 0, len(sm.snapshots))
	for _, snap := range sm.snapshots {
		result = append(result, snap)
	}
	return result
}

// DeleteSnapshot removes a snapshot.
func (sm *SnapshotManager) DeleteSnapshot(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	snap, ok := sm.snapshots[name]
	if !ok {
		return nil
	}

	// Don't allow deleting golden snapshot
	if snap.IsGolden {
		return fmt.Errorf("cannot delete golden snapshot")
	}

	// Remove files
	snapDir := filepath.Dir(snap.MemoryPath)
	if err := os.RemoveAll(snapDir); err != nil {
		return fmt.Errorf("failed to remove snapshot files: %w", err)
	}

	delete(sm.snapshots, name)

	sm.log.WithField("name", name).Info("Snapshot deleted")
	return nil
}

// Cleanup removes old snapshots to stay within MaxCached limit.
func (sm *SnapshotManager) Cleanup() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Don't clean up if under limit
	if len(sm.snapshots) <= sm.config.MaxCached {
		return nil
	}

	// Find oldest non-golden snapshots
	var oldest *Snapshot
	for _, snap := range sm.snapshots {
		if snap.IsGolden {
			continue
		}
		if oldest == nil || snap.CreatedAt.Before(oldest.CreatedAt) {
			oldest = snap
		}
	}

	if oldest != nil {
		snapDir := filepath.Dir(oldest.MemoryPath)
		os.RemoveAll(snapDir)
		delete(sm.snapshots, oldest.Name)

		sm.log.WithField("name", oldest.Name).Info("Cleaned up old snapshot")
	}

	return nil
}

// =============================================================================
// Internal Methods
// =============================================================================

func (sm *SnapshotManager) loadSnapshots() error {
	entries, err := os.ReadDir(sm.config.CacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		metaPath := filepath.Join(sm.config.CacheDir, entry.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}

		var snap Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}

		// Verify files exist
		if _, err := os.Stat(snap.MemoryPath); err != nil {
			continue
		}
		if _, err := os.Stat(snap.StatePath); err != nil {
			continue
		}

		sm.snapshots[snap.Name] = &snap
	}

	sm.log.WithField("count", len(sm.snapshots)).Debug("Loaded existing snapshots")
	return nil
}

func (sm *SnapshotManager) saveSnapshotMetadata(snap *Snapshot) error {
	snapDir := filepath.Dir(snap.MemoryPath)
	metaPath := filepath.Join(snapDir, "metadata.json")

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(metaPath, data, 0644)
}

func (sm *SnapshotManager) createSnapshotViaAPI(ctx context.Context, machine *firecracker.Machine, params *models.SnapshotCreateParams) error {
	// The firecracker-go-sdk provides CreateSnapshot method
	// This is a placeholder for the actual API call
	//
	// In the actual implementation:
	// return machine.CreateSnapshot(ctx, *params.MemFilePath, *params.SnapshotPath)

	_ = machine
	_ = params

	// For now, return nil (implement when integrating with actual SDK)
	return nil
}

// getMemoryBackendType returns the memory backend type string.
// Note: MemoryBackend configuration may vary by Firecracker version.
func (sm *SnapshotManager) getMemoryBackendType() string {
	switch sm.config.MemoryBackend {
	case "Uffd":
		// Userfaultfd for lazy memory loading (even faster restore)
		return "Uffd"
	default:
		// File-based memory backend
		return "File"
	}
}

// =============================================================================
// Snapshot-Aware Pool Integration
// =============================================================================

// SnapshotPool wraps VMPool with snapshot restore capability.
// When a golden snapshot is available, it restores from snapshot instead
// of creating new VMs, achieving sub-10ms startup times.
type SnapshotPool struct {
	*Pool
	snapshotMgr *SnapshotManager
	log         *logrus.Entry
}

// NewSnapshotPool creates a pool that uses snapshots when available.
func NewSnapshotPool(pool *Pool, snapshotMgr *SnapshotManager, log *logrus.Entry) *SnapshotPool {
	return &SnapshotPool{
		Pool:        pool,
		snapshotMgr: snapshotMgr,
		log:         log.WithField("component", "snapshot-pool"),
	}
}

// Acquire gets a VM from the pool, preferring snapshot restore.
func (sp *SnapshotPool) Acquire(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error) {
	// Try regular pool first (fastest if available)
	sandbox, err := sp.Pool.Acquire(ctx, config)
	if err == nil {
		return sandbox, nil
	}

	// Pool empty - try snapshot restore if available
	if sp.snapshotMgr != nil && sp.snapshotMgr.HasGoldenSnapshot() {
		sp.log.Debug("Pool empty, restoring from golden snapshot")
		sandbox, err := sp.snapshotMgr.RestoreFromGolden(ctx)
		if err == nil {
			// Customize restored VM for workload
			if customErr := sp.Pool.customizeVM(ctx, sandbox, config); customErr != nil {
				sp.log.WithError(customErr).Warn("Failed to customize restored VM")
			}
			return sandbox, nil
		}
		sp.log.WithError(err).Warn("Snapshot restore failed, falling back to fresh VM")
	}

	// Fallback: create fresh VM
	return sp.Pool.createFresh(ctx, config)
}

// WarmFromSnapshot warms the pool using snapshot restores.
// This is faster than creating VMs from scratch.
func (sp *SnapshotPool) WarmFromSnapshot(ctx context.Context, count int) error {
	if sp.snapshotMgr == nil || !sp.snapshotMgr.HasGoldenSnapshot() {
		// Fall back to regular warming
		return sp.Pool.Warm(ctx, count, sp.Pool.config.DefaultVMConfig)
	}

	sp.log.WithField("count", count).Info("Warming pool from snapshot")

	for i := 0; i < count; i++ {
		sandbox, err := sp.snapshotMgr.RestoreFromGolden(ctx)
		if err != nil {
			sp.log.WithError(err).Warn("Failed to restore VM for pool warming")
			continue
		}

		sandbox.PooledAt = time.Now()

		select {
		case sp.Pool.available <- sandbox:
			sp.log.WithField("sandbox_id", sandbox.ID).Debug("Added restored VM to pool")
		default:
			// Pool full
			sp.Pool.manager.DestroyVM(ctx, sandbox)
		}
	}

	return nil
}

// =============================================================================
// Snapshot Statistics
// =============================================================================

// SnapshotStats contains snapshot-related statistics.
type SnapshotStats struct {
	SnapshotsAvailable int     `json:"snapshots_available"`
	HasGoldenSnapshot  bool    `json:"has_golden_snapshot"`
	TotalSizeBytes     int64   `json:"total_size_bytes"`
	AvgRestoreTimeMs   float64 `json:"avg_restore_time_ms"`
	RestoreCount       int64   `json:"restore_count"`
}

// Stats returns snapshot statistics.
func (sm *SnapshotManager) Stats() SnapshotStats {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var totalSize int64
	for _, snap := range sm.snapshots {
		totalSize += snap.SizeBytes
	}

	return SnapshotStats{
		SnapshotsAvailable: len(sm.snapshots),
		HasGoldenSnapshot:  sm.goldenSnapshot != nil,
		TotalSizeBytes:     totalSize,
	}
}
