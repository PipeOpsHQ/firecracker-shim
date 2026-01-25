// Package domain defines the core domain model for the Firecracker CRI runtime.
// Following domain-driven design principles, these types represent the ubiquitous
// language of our bounded context: microVM-based container isolation.
package domain

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
)

// =============================================================================
// Core Domain Entities
// =============================================================================

// SandboxState represents the lifecycle state of a pod sandbox (microVM).
type SandboxState int

const (
	SandboxUnknown SandboxState = iota
	SandboxPending              // VM is being created
	SandboxReady                // VM is running and ready
	SandboxStopped              // VM has been stopped
)

func (s SandboxState) String() string {
	switch s {
	case SandboxPending:
		return "pending"
	case SandboxReady:
		return "ready"
	case SandboxStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ContainerState represents the lifecycle state of a container within a sandbox.
type ContainerState int

const (
	ContainerUnknown ContainerState = iota
	ContainerCreated                // Container is created but not started
	ContainerRunning                // Container is actively running
	ContainerExited                 // Container has exited
)

func (s ContainerState) String() string {
	switch s {
	case ContainerCreated:
		return "created"
	case ContainerRunning:
		return "running"
	case ContainerExited:
		return "exited"
	default:
		return "unknown"
	}
}

// Sandbox represents a pod sandbox - the microVM that hosts containers.
// This is the aggregate root for the sandbox bounded context.
type Sandbox struct {
	mu sync.RWMutex

	// Identity
	ID        string            // Unique sandbox identifier
	Name      string            // Human-readable name
	Namespace string            // Kubernetes namespace
	Labels    map[string]string // Pod labels
	Annotations map[string]string

	// VM State
	State       SandboxState
	VM          *firecracker.Machine // The actual Firecracker VM
	VMConfig    VMConfig             // VM configuration used
	PID         int                  // VMM process ID

	// Communication
	VsockPath   string    // Unix socket for vsock
	VsockCID    uint32    // Guest context ID
	AgentConn   net.Conn  // Connection to guest agent

	// Networking
	NetworkNamespace string
	IP               net.IP
	Gateway          net.IP

	// Storage
	RootfsPath  string // Path to rootfs block device

	// Containers within this sandbox
	Containers map[string]*Container

	// Lifecycle timestamps
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time

	// Metadata for pool management
	PooledAt    time.Time // When this VM was added to pool (if pre-warmed)
	FromPool    bool      // Whether this sandbox came from the pool
}

// NewSandbox creates a new sandbox with the given ID.
func NewSandbox(id string) *Sandbox {
	return &Sandbox{
		ID:          id,
		State:       SandboxPending,
		Labels:      make(map[string]string),
		Annotations: make(map[string]string),
		Containers:  make(map[string]*Container),
		CreatedAt:   time.Now(),
	}
}

// AddContainer adds a container to this sandbox.
func (s *Sandbox) AddContainer(c *Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.SandboxID = s.ID
	s.Containers[c.ID] = c
}

// GetContainer retrieves a container by ID.
func (s *Sandbox) GetContainer(id string) (*Container, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.Containers[id]
	return c, ok
}

// RemoveContainer removes a container from this sandbox.
func (s *Sandbox) RemoveContainer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Containers, id)
}

// Container represents a container running inside a sandbox (microVM).
type Container struct {
	mu sync.RWMutex

	// Identity
	ID        string // Unique container identifier
	SandboxID string // Parent sandbox ID
	Name      string
	Image     string
	ImageRef  string // Resolved image reference (digest)

	// State
	State      ContainerState
	PID        int   // Process ID inside the VM
	ExitCode   int32

	// Configuration
	Command    []string
	Args       []string
	Env        []string
	WorkingDir string
	Mounts     []Mount

	// Resource limits
	Resources ResourceConfig

	// Lifecycle timestamps
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	// Logs
	LogPath    string
}

// NewContainer creates a new container with the given ID.
func NewContainer(id string) *Container {
	return &Container{
		ID:        id,
		State:     ContainerCreated,
		CreatedAt: time.Now(),
		Mounts:    make([]Mount, 0),
		Env:       make([]string, 0),
	}
}

// =============================================================================
// Value Objects
// =============================================================================

// VMConfig holds the configuration for creating a Firecracker VM.
// This is a value object - immutable once created.
type VMConfig struct {
	// Compute
	VcpuCount  int64
	MemoryMB   int64
	SMTEnabled bool

	// Boot
	KernelPath string
	KernelArgs string
	InitrdPath string // Optional

	// Storage
	RootDrive DriveConfig

	// Network
	NetworkMode string // "cni" or "none"
	CNIConfig   *CNIConfig

	// Vsock
	VsockEnabled bool
	VsockCID     uint32

	// Advanced
	JailerEnabled bool
	JailerConfig  *JailerConfig
}

// DefaultVMConfig returns a minimal VM configuration.
func DefaultVMConfig() VMConfig {
	return VMConfig{
		VcpuCount:    1,
		MemoryMB:     128,
		SMTEnabled:   false,
		KernelArgs:   "console=ttyS0 reboot=k panic=1 pci=off quiet",
		VsockEnabled: true,
		NetworkMode:  "cni",
	}
}

// DriveConfig represents a block device configuration.
type DriveConfig struct {
	DriveID    string
	PathOnHost string
	IsReadOnly bool
	IsRoot     bool
	CacheType  string // "Unsafe" or "Writeback"
}

// CNIConfig holds CNI-specific configuration.
type CNIConfig struct {
	NetworkName string
	IfName      string
	BinDir      string
	ConfDir     string
	CacheDir    string
}

// JailerConfig holds jailer configuration for privilege isolation.
type JailerConfig struct {
	UID           int
	GID           int
	ChrootBaseDir string
	ExecFile      string
	NetNS         string
}

// Mount represents a filesystem mount inside a container.
type Mount struct {
	Source      string
	Destination string
	Type        string
	Options     []string
	ReadOnly    bool
}

// ResourceConfig represents resource limits for a container.
type ResourceConfig struct {
	CPUShares     int64
	CPUQuota      int64
	CPUPeriod     int64
	MemoryLimitMB int64
	OOMScoreAdj   int
}

// =============================================================================
// Domain Services Interfaces
// =============================================================================

// VMManager defines the interface for managing Firecracker VMs.
// This is the primary domain service for VM lifecycle operations.
type VMManager interface {
	// CreateVM creates and starts a new Firecracker VM.
	CreateVM(ctx context.Context, config VMConfig) (*Sandbox, error)

	// StopVM gracefully stops a VM.
	StopVM(ctx context.Context, sandbox *Sandbox) error

	// DestroyVM forcefully terminates a VM and cleans up resources.
	DestroyVM(ctx context.Context, sandbox *Sandbox) error

	// PauseVM suspends VM execution.
	PauseVM(ctx context.Context, sandbox *Sandbox) error

	// ResumeVM resumes a paused VM.
	ResumeVM(ctx context.Context, sandbox *Sandbox) error
}

// VMPool defines the interface for pre-warming VMs.
type VMPool interface {
	// Acquire gets a pre-warmed VM from the pool, or creates a new one if empty.
	Acquire(ctx context.Context, config VMConfig) (*Sandbox, error)

	// Release returns a VM to the pool (or destroys it if pool is full).
	Release(ctx context.Context, sandbox *Sandbox) error

	// Warm adds pre-warmed VMs to the pool.
	Warm(ctx context.Context, count int, config VMConfig) error

	// Stats returns pool statistics.
	Stats() PoolStats

	// Close shuts down the pool and all VMs.
	Close(ctx context.Context) error
}

// PoolStats contains VM pool statistics.
type PoolStats struct {
	Available   int
	InUse       int
	MaxSize     int
	TotalServed int64
	PoolHits    int64
	PoolMisses  int64
}

// AgentClient defines the interface for communicating with the guest agent.
type AgentClient interface {
	// Connect establishes a connection to the guest agent.
	Connect(ctx context.Context, vsockPath string, cid uint32, port uint32) error

	// Close terminates the connection.
	Close() error

	// CreateContainer creates a container inside the VM.
	CreateContainer(ctx context.Context, spec *ContainerSpec) error

	// StartContainer starts a created container.
	StartContainer(ctx context.Context, containerID string) (int, error)

	// StopContainer stops a running container.
	StopContainer(ctx context.Context, containerID string, timeout time.Duration) error

	// RemoveContainer removes a container.
	RemoveContainer(ctx context.Context, containerID string) error

	// ExecSync executes a command synchronously.
	ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (*ExecResult, error)

	// GetContainerStats retrieves container resource usage.
	GetContainerStats(ctx context.Context, containerID string) (*ContainerStats, error)
}

// ContainerSpec is the specification for creating a container.
type ContainerSpec struct {
	ID         string
	BundlePath string
	Stdin      bool
	Stdout     bool
	Stderr     bool
	Terminal   bool
}

// ExecResult holds the result of a synchronous exec.
type ExecResult struct {
	ExitCode int32
	Stdout   []byte
	Stderr   []byte
}

// ContainerStats holds container resource usage statistics.
type ContainerStats struct {
	CPUUsage    uint64 // nanoseconds
	MemoryUsage uint64 // bytes
	ReadBytes   uint64
	WriteBytes  uint64
}

// ImageService defines the interface for managing container images.
type ImageService interface {
	// Pull downloads an image and converts it to a rootfs.
	Pull(ctx context.Context, ref string) (string, error)

	// GetRootfs returns the path to the rootfs for an image.
	GetRootfs(ctx context.Context, ref string) (string, error)

	// Remove removes an image.
	Remove(ctx context.Context, ref string) error

	// List lists available images.
	List(ctx context.Context) ([]ImageInfo, error)
}

// ImageInfo contains information about an image.
type ImageInfo struct {
	Ref       string
	Digest    string
	Size      int64
	CreatedAt time.Time
}

// NetworkService defines the interface for network management.
type NetworkService interface {
	// Setup configures networking for a sandbox.
	Setup(ctx context.Context, sandbox *Sandbox, config *CNIConfig) error

	// Teardown removes network configuration.
	Teardown(ctx context.Context, sandbox *Sandbox) error

	// GetIP returns the IP address assigned to a sandbox.
	GetIP(ctx context.Context, sandboxID string) (net.IP, error)
}
