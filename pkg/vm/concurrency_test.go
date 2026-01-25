package vm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// TestManager_Concurrency verifies thread safety of the VM Manager.
// It spawns multiple goroutines accessing the same sandbox simultaneously.
func TestManager_Concurrency(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	config := DefaultManagerConfig()
	config.RuntimeDir = t.TempDir()

	mgr, err := NewManager(config, log)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Create a dummy sandbox
	sbID := "race-test-sandbox"
	sandbox := domain.NewSandbox(sbID)
	// We don't set sandbox.VM, so methods will return "has no VM" error,
	// which is fine - we are testing the locking mechanism around the map/struct.

	// Manually inject into manager (since CreateVM does too much real work)
	mgr.mu.Lock()
	mgr.sandboxes[sbID] = sandbox
	mgr.mu.Unlock()

	var wg sync.WaitGroup
	ctx := context.Background()
	concurrency := 20

	// Goroutines for GetSandbox (Read lock)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, ok := mgr.GetSandbox(sbID)
				if !ok {
					// It might be deleted by the Destroy routine, that's expected
				}
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	// Goroutines for State Transitions (Write/Mutex locks)
	// These will fail with "no VM" but will exercise the locking logic
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = mgr.StopVM(ctx, sandbox)
				_ = mgr.PauseVM(ctx, sandbox)
				_ = mgr.ResumeVM(ctx, sandbox)
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	// Goroutines for ListSandboxes (Read lock on map)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = mgr.ListSandboxes()
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// Final cleanup test (Write lock on map + mutex)
	_ = mgr.DestroyVM(ctx, sandbox)
}
