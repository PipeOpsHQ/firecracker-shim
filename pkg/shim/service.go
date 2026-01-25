// Package shim implements the containerd v2 shim interface for Firecracker.
//
// This is the main entry point that containerd launches. The shim acts as
// a bridge between containerd's task API and our Firecracker VM management.
//
// Architecture:
//
//	containerd -> ttrpc -> shim (this) -> firecracker-go-sdk -> Firecracker VMM
//	                                   -> vsock -> fc-agent -> runc -> container
package shim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/pipeops/firecracker-cri/pkg/agent"
	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/pipeops/firecracker-cri/pkg/vm"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// shimID is used by containerd to identify this runtime.
	shimID = "io.containerd.firecracker.v2"

	// vsockAgentPort is the port the guest agent listens on.
	vsockAgentPort = 1024
)

// Service implements the containerd task service for Firecracker.
type Service struct {
	mu sync.Mutex

	// Shim identity
	id        string
	namespace string
	bundle    string

	// Core components
	vmManager   *vm.Manager
	vmPool      *vm.Pool
	agentClient *agent.Client

	// Current sandbox (one sandbox per shim instance)
	sandbox *domain.Sandbox

	// Task state
	processes map[string]*processState

	// Event publishing
	events    chan interface{}
	publisher shim.Publisher

	// Lifecycle
	ctx      context.Context
	cancel   context.CancelFunc
	shutdown func()

	log *logrus.Entry
}

// processState tracks the state of a process (init or exec).
type processState struct {
	id          string
	containerID string
	pid         int
	exitStatus  int
	exitedAt    time.Time
	stdin       string
	stdout      string
	stderr      string
	terminal    bool
}

// New creates a new Firecracker shim service.
// This is called by containerd when launching the shim.
func New(ctx context.Context, id string, publisher shim.Publisher, shutdown func()) (shim.Shim, error) {
	ns, _ := namespaces.Namespace(ctx)

	log := logrus.NewEntry(logrus.StandardLogger()).WithFields(logrus.Fields{
		"namespace": ns,
		"id":        id,
	})
	log.Info("Creating new Firecracker shim")

	ctx, cancel := context.WithCancel(ctx)

	// Initialize VM manager
	vmConfig := vm.DefaultManagerConfig()
	vmManager, err := vm.NewManager(vmConfig, log)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create VM manager: %w", err)
	}

	// Initialize VM pool
	poolConfig := vm.DefaultPoolConfig()
	vmPool, err := vm.NewPool(vmManager, poolConfig, log)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create VM pool: %w", err)
	}

	s := &Service{
		id:        id,
		namespace: ns,
		vmManager: vmManager,
		vmPool:    vmPool,
		processes: make(map[string]*processState),
		events:    make(chan interface{}, 128),
		publisher: publisher,
		ctx:       ctx,
		cancel:    cancel,
		shutdown:  shutdown,
		log:       log,
	}

	// Start event forwarding
	go s.forwardEvents()

	return s, nil
}

// StartShim is called to start the shim as a new process.
// It returns the address that containerd should use to connect.
func (s *Service) StartShim(ctx context.Context, opts shim.StartOpts) (string, error) {
	// The shim runs as its own process. containerd calls this to get the
	// ttrpc socket address to communicate with us.

	s.bundle = opts.Address // In newer containerd, bundle is in different field

	// Create socket in bundle directory
	socketPath := filepath.Join(filepath.Dir(opts.Address), "shim.sock")

	s.log.WithField("socket", socketPath).Info("Starting shim")

	// In a real implementation, you'd fork here and have the child
	// create the ttrpc server. For simplicity, we assume we're already
	// the child process.

	return socketPath, nil
}

// Cleanup is called after the shim exits to clean up resources.
func (s *Service) Cleanup(ctx context.Context) (*taskAPI.DeleteResponse, error) {
	s.log.Info("Cleanup called")

	// Destroy the sandbox VM
	if s.sandbox != nil {
		if err := s.vmManager.DestroyVM(ctx, s.sandbox); err != nil {
			s.log.WithError(err).Warn("Error destroying sandbox during cleanup")
		}
	}

	return &taskAPI.DeleteResponse{
		ExitedAt:   timestamppb.Now(),
		ExitStatus: 0,
	}, nil
}

// =============================================================================
// TaskService Implementation
// =============================================================================

// State returns the state of a process.
func (s *Service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	proc, ok := s.processes[r.ExecID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", r.ExecID)
	}

	var exitedAt *timestamppb.Timestamp
	if !proc.exitedAt.IsZero() {
		exitedAt = timestamppb.New(proc.exitedAt)
	}

	return &taskAPI.StateResponse{
		ID:         proc.id,
		Bundle:     s.bundle,
		Pid:        uint32(proc.pid),
		Status:     s.processStatus(proc),
		Stdin:      proc.stdin,
		Stdout:     proc.stdout,
		Stderr:     proc.stderr,
		Terminal:   proc.terminal,
		ExitStatus: uint32(proc.exitStatus),
		ExitedAt:   exitedAt,
	}, nil
}

// Create creates a new task (container).
func (s *Service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	s.log.WithFields(logrus.Fields{
		"id":     r.ID,
		"bundle": r.Bundle,
	}).Info("Creating task")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Create or acquire a VM for this task
	vmConfig := domain.DefaultVMConfig()

	// The rootfs comes from the bundle
	if len(r.Rootfs) > 0 {
		vmConfig.RootDrive = domain.DriveConfig{
			DriveID:    "rootfs",
			PathOnHost: r.Rootfs[0].Source,
			IsRoot:     true,
			IsReadOnly: false,
		}
	}

	// Acquire VM from pool (fast path) or create new
	sandbox, err := s.vmPool.Acquire(ctx, vmConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire VM: %w", err)
	}
	s.sandbox = sandbox
	s.bundle = r.Bundle

	// Connect to the guest agent
	s.agentClient = agent.NewClient(s.log)
	if err := s.agentClient.Connect(ctx, sandbox.VsockPath, sandbox.VsockCID, vsockAgentPort); err != nil {
		return nil, fmt.Errorf("failed to connect to agent: %w", err)
	}

	// Create the container inside the VM
	containerSpec := &domain.ContainerSpec{
		ID:         r.ID,
		BundlePath: r.Bundle,
		Stdin:      r.Stdin != "",
		Stdout:     r.Stdout != "",
		Stderr:     r.Stderr != "",
		Terminal:   r.Terminal,
	}
	if err := s.agentClient.CreateContainer(ctx, containerSpec); err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Track the init process
	proc := &processState{
		id:          r.ID,
		containerID: r.ID,
		stdin:       r.Stdin,
		stdout:      r.Stdout,
		stderr:      r.Stderr,
		terminal:    r.Terminal,
	}
	s.processes[r.ID] = proc

	return &taskAPI.CreateTaskResponse{
		Pid: uint32(sandbox.PID),
	}, nil
}

// Start starts a created task.
func (s *Service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	s.log.WithFields(logrus.Fields{
		"id":      r.ID,
		"exec_id": r.ExecID,
	}).Info("Starting task")

	s.mu.Lock()
	defer s.mu.Unlock()

	procID := r.ID
	if r.ExecID != "" {
		procID = r.ExecID
	}

	proc, ok := s.processes[procID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", procID)
	}

	// Start the container via the agent
	pid, err := s.agentClient.StartContainer(ctx, proc.containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}
	proc.pid = pid

	return &taskAPI.StartResponse{
		Pid: uint32(pid),
	}, nil
}

// Delete removes a task.
func (s *Service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.log.WithFields(logrus.Fields{
		"id":      r.ID,
		"exec_id": r.ExecID,
	}).Info("Deleting task")

	s.mu.Lock()
	defer s.mu.Unlock()

	procID := r.ID
	if r.ExecID != "" {
		procID = r.ExecID
	}

	proc, ok := s.processes[procID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", procID)
	}

	// Remove the container via the agent
	if s.agentClient != nil {
		if err := s.agentClient.RemoveContainer(ctx, proc.containerID); err != nil {
			s.log.WithError(err).Warn("Error removing container")
		}
	}

	// Clean up process state
	delete(s.processes, procID)

	// If this is the init process, release the VM
	if r.ExecID == "" && s.sandbox != nil {
		if err := s.vmPool.Release(ctx, s.sandbox); err != nil {
			s.log.WithError(err).Warn("Error releasing VM to pool")
		}
		s.sandbox = nil
	}

	var exitedAt *timestamppb.Timestamp
	if !proc.exitedAt.IsZero() {
		exitedAt = timestamppb.New(proc.exitedAt)
	}

	return &taskAPI.DeleteResponse{
		Pid:        uint32(proc.pid),
		ExitStatus: uint32(proc.exitStatus),
		ExitedAt:   exitedAt,
	}, nil
}

// Kill sends a signal to a task.
func (s *Service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	s.log.WithFields(logrus.Fields{
		"id":     r.ID,
		"signal": r.Signal,
	}).Info("Killing task")

	s.mu.Lock()
	defer s.mu.Unlock()

	procID := r.ID
	if r.ExecID != "" {
		procID = r.ExecID
	}

	proc, ok := s.processes[procID]
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", procID)
	}

	// Send signal via the agent
	timeout := 30 * time.Second
	if err := s.agentClient.StopContainer(ctx, proc.containerID, timeout); err != nil {
		return nil, fmt.Errorf("failed to kill container: %w", err)
	}

	return &emptypb.Empty{}, nil
}

// Exec creates an additional process inside a container.
func (s *Service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	s.log.WithFields(logrus.Fields{
		"id":      r.ID,
		"exec_id": r.ExecID,
	}).Info("Exec in task")

	// TODO: Implement exec via agent
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Pids returns all pids inside a container.
func (s *Service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pids []*task.ProcessInfo
	for _, proc := range s.processes {
		if proc.containerID == r.ID {
			pids = append(pids, &task.ProcessInfo{
				Pid: uint32(proc.pid),
			})
		}
	}

	return &taskAPI.PidsResponse{Processes: pids}, nil
}

// Pause pauses a container.
func (s *Service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	if s.sandbox == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "no sandbox")
	}

	if err := s.vmManager.PauseVM(ctx, s.sandbox); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	return &emptypb.Empty{}, nil
}

// Resume resumes a paused container.
func (s *Service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	if s.sandbox == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "no sandbox")
	}

	if err := s.vmManager.ResumeVM(ctx, s.sandbox); err != nil {
		return nil, fmt.Errorf("failed to resume VM: %w", err)
	}

	return &emptypb.Empty{}, nil
}

// Checkpoint creates a checkpoint of a container.
func (s *Service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	// TODO: Implement using Firecracker snapshots
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Update updates a running container.
func (s *Service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	// TODO: Implement resource updates via balloon/hotplug
	return nil, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Wait waits for a process to exit.
func (s *Service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	s.mu.Lock()
	procID := r.ID
	if r.ExecID != "" {
		procID = r.ExecID
	}
	proc, ok := s.processes[procID]
	s.mu.Unlock()

	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s not found", procID)
	}

	// In a real implementation, you'd wait on a channel here
	// For now, just return current state if exited
	if !proc.exitedAt.IsZero() {
		return &taskAPI.WaitResponse{
			ExitStatus: uint32(proc.exitStatus),
			ExitedAt:   timestamppb.New(proc.exitedAt),
		}, nil
	}

	// Block until context cancelled or process exits
	<-ctx.Done()
	return nil, ctx.Err()
}

// Stats returns resource usage statistics.
func (s *Service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.agentClient == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "no agent connection")
	}

	stats, err := s.agentClient.GetContainerStats(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats: %w", err)
	}

	// Convert to containerd stats format
	// This is simplified - real implementation would use cgroups metrics
	_ = stats // TODO: Convert stats

	return &taskAPI.StatsResponse{}, nil
}

// Connect returns shim information.
func (s *Service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	var pid uint32
	if s.sandbox != nil {
		pid = uint32(s.sandbox.PID)
	}

	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: pid,
		Version: "v2",
	}, nil
}

// Shutdown shuts down the shim.
func (s *Service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	s.log.Info("Shutdown requested")

	s.cancel()

	if s.vmPool != nil {
		s.vmPool.Close(ctx)
	}

	if s.shutdown != nil {
		s.shutdown()
	}

	return &emptypb.Empty{}, nil
}

// ResizePty resizes the terminal.
func (s *Service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	// TODO: Implement PTY resize via agent
	return &emptypb.Empty{}, nil
}

// CloseIO closes the I/O streams for a process.
func (s *Service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	// TODO: Implement I/O close via agent
	return &emptypb.Empty{}, nil
}

// =============================================================================
// Helper Methods
// =============================================================================

func (s *Service) processStatus(proc *processState) task.Status {
	if !proc.exitedAt.IsZero() {
		return task.Status_STOPPED
	}
	if proc.pid > 0 {
		return task.Status_RUNNING
	}
	return task.Status_CREATED
}

func (s *Service) forwardEvents() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case e := <-s.events:
			if err := s.publisher.Publish(s.ctx, getTopic(e), e); err != nil {
				s.log.WithError(err).Warn("Failed to publish event")
			}
		}
	}
}

func getTopic(e interface{}) string {
	switch e.(type) {
	default:
		return "/tasks/unknown"
	}
}
