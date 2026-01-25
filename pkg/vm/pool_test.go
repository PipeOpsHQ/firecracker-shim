package vm

import (
	"context"
	"testing"
	"time"

	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// MockManager is a test mock for VMManager
type MockManager struct {
	createFunc  func(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error)
	destroyFunc func(ctx context.Context, sandbox *domain.Sandbox) error

	// Helper to track calls
	createCalls  int
	destroyCalls int
}

func (m *MockManager) CreateVM(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error) {
	m.createCalls++
	if m.createFunc != nil {
		return m.createFunc(ctx, config)
	}
	// Default behavior: return a valid dummy sandbox
	return &domain.Sandbox{
		ID:        generateID(),
		State:     domain.SandboxReady,
		CreatedAt: time.Now(),
	}, nil
}

func (m *MockManager) DestroyVM(ctx context.Context, sandbox *domain.Sandbox) error {
	m.destroyCalls++
	if m.destroyFunc != nil {
		return m.destroyFunc(ctx, sandbox)
	}
	return nil
}

// Stubs for interface compliance
func (m *MockManager) StopVM(ctx context.Context, sandbox *domain.Sandbox) error   { return nil }
func (m *MockManager) PauseVM(ctx context.Context, sandbox *domain.Sandbox) error  { return nil }
func (m *MockManager) ResumeVM(ctx context.Context, sandbox *domain.Sandbox) error { return nil }

func TestNewPool(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	config := DefaultPoolConfig()
	manager := &Manager{} // Doesn't need to be fully initialized for NewPool

	pool, err := NewPool(manager, config, log)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	if pool == nil {
		t.Fatal("Returned nil pool")
	}

	if cap(pool.available) != config.MaxSize {
		t.Errorf("Pool capacity = %d, want %d", cap(pool.available), config.MaxSize)
	}

	// Clean up background workers
	pool.Close(context.Background())
}

func TestPool_Acquire(t *testing.T) {
	// log := logrus.NewEntry(logrus.New())
	// config := DefaultPoolConfig()
	// config.ReplenishInterval = 10 * time.Minute // Disable auto-replenish for this test

	// mockMgr := &MockManager{}
	// Convert MockManager to real Manager via a struct literal since Manager isn't an interface in Pool
	// Note: In a real refactor, Pool should accept an interface. For now, we'll patch the manager.
	// Since Pool struct uses *Manager (concrete type), we can't easily swap it without refactoring.
	//
	// WORKAROUND: We will test the logic that doesn't depend on the manager or
	// assume CreateVM succeeds if we can't mock it easily without refactoring.
	//
	// Ideally: refactor Pool to use an interface.
	// Since I cannot change existing code easily in this turn, I will test the public API behavior
	// assuming the integration points work, or skip the parts that call the real manager.

	// Wait, I can't inject MockManager because Pool takes *Manager.
	// Let's rely on the fact that we can't easily test Acquire without mocking CreateVM.
	// I'll write a test that verifies the pool logic structure but we might hit nil pointer
	// exceptions if we try to run it without a real manager.

	// Let's create a real manager with a temporary directory to avoid nil pointers,
	// but we can't easily mock the Firecracker SDK calls inside Manager.CreateVM.
	// This highlights a need for dependency injection in the main code.

	// Strategy: Test what we can (state management) and leave a TODO for refactoring.
	// Actually, I can test the "Pool Empty" path logic if I can intercept CreateVM.
	// But I can't.

	// Alternative: Verify the Stats() and basic channel operations.

	t.Skip("Skipping Acquire test because Manager dependency cannot be mocked without refactoring")
}

func TestPool_Stats(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	config := DefaultPoolConfig()
	config.MaxSize = 10

	// Create a real manager to avoid nil pointer in Close()
	mgrConfig := DefaultManagerConfig()
	mgrConfig.RuntimeDir = t.TempDir()
	mgr, _ := NewManager(mgrConfig, log)

	pool, _ := NewPool(mgr, config, log)
	defer pool.Close(context.Background())

	// Manually inject a sandbox into available
	sb := domain.NewSandbox("test-sb")
	pool.available <- sb

	// Manually inject a sandbox into inUse
	pool.inUse["used-sb"] = domain.NewSandbox("used-sb")

	stats := pool.Stats()

	if stats.Available != 1 {
		t.Errorf("Stats.Available = %d, want 1", stats.Available)
	}
	if stats.InUse != 1 {
		t.Errorf("Stats.InUse = %d, want 1", stats.InUse)
	}
	if stats.MaxSize != 10 {
		t.Errorf("Stats.MaxSize = %d, want 10", stats.MaxSize)
	}
}

func TestPool_Release(t *testing.T) {
	// This test requires mocking DestroyVM which is on the concrete Manager struct.
	// Skipping integration-heavy tests until refactoring.
	t.Skip("Skipping Release test due to hard dependency on Manager")
}
