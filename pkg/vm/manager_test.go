package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

func TestNewManager(t *testing.T) {
	tmpDir := t.TempDir()
	log := logrus.NewEntry(logrus.New())

	config := DefaultManagerConfig()
	config.RuntimeDir = tmpDir

	mgr, err := NewManager(config, log)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	if mgr == nil {
		t.Fatal("Returned nil manager")
	}

	if mgr.cidCounter != 3 {
		t.Errorf("Initial CID counter = %d, want 3", mgr.cidCounter)
	}
}

func TestManager_GetSandbox(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	config := DefaultManagerConfig()
	config.RuntimeDir = t.TempDir()

	mgr, err := NewManager(config, log)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Add a dummy sandbox
	sb := domain.NewSandbox("test-id")
	mgr.sandboxes["test-id"] = sb

	// Test existing
	got, ok := mgr.GetSandbox("test-id")
	if !ok {
		t.Error("GetSandbox failed for existing ID")
	}
	if got != sb {
		t.Error("GetSandbox returned wrong object")
	}

	// Test non-existent
	_, ok = mgr.GetSandbox("missing")
	if ok {
		t.Error("GetSandbox succeeded for missing ID")
	}
}

func TestManager_ListSandboxes(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	config := DefaultManagerConfig()
	config.RuntimeDir = t.TempDir()

	mgr, err := NewManager(config, log)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	mgr.sandboxes["sb1"] = domain.NewSandbox("sb1")
	mgr.sandboxes["sb2"] = domain.NewSandbox("sb2")

	list := mgr.ListSandboxes()
	if len(list) != 2 {
		t.Errorf("ListSandboxes count = %d, want 2", len(list))
	}
}

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	time.Sleep(1 * time.Nanosecond) // Ensure time difference
	id2 := generateID()

	if id1 == "" {
		t.Error("Generated empty ID")
	}
	if id1 == id2 {
		t.Error("Generated duplicate IDs")
	}
}

// Note: CreateVM and DestroyVM require mocking the Firecracker SDK,
// which is complex due to the external dependency.
// For now, we test the state management logic.

func TestManager_DestroyVM_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	log := logrus.NewEntry(logrus.New())

	config := DefaultManagerConfig()
	config.RuntimeDir = tmpDir

	mgr, err := NewManager(config, log)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Create a sandbox directory structure to verify cleanup
	sbID := "test-cleanup"
	sbDir := filepath.Join(tmpDir, sbID)
	_ = os.MkdirAll(sbDir, 0755)

	sb := domain.NewSandbox(sbID)
	sb.State = domain.SandboxStopped // Already stopped to skip StopVM logic which needs VM
	mgr.sandboxes[sbID] = sb

	// Call DestroyVM
	ctx := context.Background()
	err = mgr.DestroyVM(ctx, sb)
	if err != nil {
		t.Errorf("DestroyVM failed: %v", err)
	}

	// Verify directory removed
	if _, err := os.Stat(sbDir); !os.IsNotExist(err) {
		t.Error("Sandbox directory was not removed")
	}

	// Verify removed from map
	if _, ok := mgr.sandboxes[sbID]; ok {
		t.Error("Sandbox was not removed from manager map")
	}
}
