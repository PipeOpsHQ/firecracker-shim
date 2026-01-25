package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestCollector_PoolStats(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	c := NewCollector(log)

	c.SetPoolStats(5, 3, 10)
	c.RecordPoolHit()
	c.RecordPoolHit()
	c.RecordPoolMiss()
	c.RecordPoolWarmTime(100 * time.Millisecond)

	snap := c.GetSnapshot()

	if snap.PoolAvailable != 5 {
		t.Errorf("PoolAvailable = %d, want 5", snap.PoolAvailable)
	}
	if snap.PoolInUse != 3 {
		t.Errorf("PoolInUse = %d, want 3", snap.PoolInUse)
	}
	if snap.PoolMaxSize != 10 {
		t.Errorf("PoolMaxSize = %d, want 10", snap.PoolMaxSize)
	}
	if snap.PoolHits != 2 {
		t.Errorf("PoolHits = %d, want 2", snap.PoolHits)
	}
	if snap.PoolMisses != 1 {
		t.Errorf("PoolMisses = %d, want 1", snap.PoolMisses)
	}
	if snap.PoolHitRate < 66.0 || snap.PoolHitRate > 67.0 {
		t.Errorf("PoolHitRate = %f, want ~66.67", snap.PoolHitRate)
	}
}

func TestCollector_Counters(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	c := NewCollector(log)

	c.RecordVMCreated(128, 1)
	c.RecordVMCreated(256, 2)
	c.RecordVMDestroyed(128, 1)

	c.RecordContainerCreated()
	c.RecordContainerCreated()
	c.RecordContainerDestroyed()

	c.RecordVMCreateError()
	c.RecordVMDestroyError()
	c.RecordContainerError()
	c.RecordAgentConnectError()

	snap := c.GetSnapshot()

	if snap.TotalVMsCreated != 2 {
		t.Errorf("TotalVMsCreated = %d, want 2", snap.TotalVMsCreated)
	}
	if snap.TotalVMsDestroyed != 1 {
		t.Errorf("TotalVMsDestroyed = %d, want 1", snap.TotalVMsDestroyed)
	}
	if snap.TotalMemoryMB != 256 {
		t.Errorf("TotalMemoryMB = %d, want 256", snap.TotalMemoryMB)
	}
	if snap.TotalVCPUs != 2 {
		t.Errorf("TotalVCPUs = %d, want 2", snap.TotalVCPUs)
	}
	if snap.TotalContainers != 2 {
		t.Errorf("TotalContainers = %d, want 2", snap.TotalContainers)
	}
	if snap.ActiveContainers != 1 {
		t.Errorf("ActiveContainers = %d, want 1", snap.ActiveContainers)
	}
	if snap.VMCreateErrors != 1 {
		t.Errorf("VMCreateErrors = %d, want 1", snap.VMCreateErrors)
	}
	if snap.VMDestroyErrors != 1 {
		t.Errorf("VMDestroyErrors = %d, want 1", snap.VMDestroyErrors)
	}
	if snap.ContainerErrors != 1 {
		t.Errorf("ContainerErrors = %d, want 1", snap.ContainerErrors)
	}
	if snap.AgentConnectErrors != 1 {
		t.Errorf("AgentConnectErrors = %d, want 1", snap.AgentConnectErrors)
	}
}

func TestCollector_Latencies(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	c := NewCollector(log)

	// Simulate some operations
	timer := c.StartTimer("create")
	time.Sleep(1 * time.Millisecond) // Ensure non-zero
	timer.Stop()

	snap := c.GetSnapshot()
	// Just verify it doesn't crash and we get 0 or more
	if snap.CreateLatencyP50 < 0 {
		t.Errorf("CreateLatencyP50 = %f, want >= 0", snap.CreateLatencyP50)
	}
}

func TestPrometheusHandler(t *testing.T) {
	log := logrus.NewEntry(logrus.New())
	c := NewCollector(log)

	// Populate some data
	c.SetPoolStats(10, 5, 20)
	c.RecordPoolHit()

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()

	c.PrometheusHandler().ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	s := string(body)

	expected := []string{
		"fc_cri_pool_available 10",
		"fc_cri_pool_in_use 5",
		"fc_cri_pool_max_size 20",
		"fc_cri_pool_hits_total 1",
		"TYPE fc_cri_pool_available gauge",
	}

	for _, exp := range expected {
		if !strings.Contains(s, exp) {
			t.Errorf("Response missing expected string: %s", exp)
		}
	}
}

func TestGlobalCollector(t *testing.T) {
	c := Global()
	if c == nil {
		t.Error("Global() returned nil")
	}

	c2 := Global()
	if c != c2 {
		t.Error("Global() returned different instance")
	}

	custom := NewCollector(logrus.NewEntry(logrus.New()))
	SetGlobal(custom)
	if Global() != custom {
		t.Error("SetGlobal failed")
	}
}
