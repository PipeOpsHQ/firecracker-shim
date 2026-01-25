// Package metrics provides Prometheus-compatible metrics for the Firecracker CRI runtime.
//
// Metrics are exposed via a /metrics HTTP endpoint and can be scraped by Prometheus.
// Key metrics include:
// - VM pool statistics (available, in-use, hits, misses)
// - Container operation latencies (create, start, stop, delete)
// - VM lifecycle events
// - Resource utilization
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Collector collects and exposes runtime metrics.
type Collector struct {
	mu sync.RWMutex

	// VM Pool metrics
	poolAvailable   int64
	poolInUse       int64
	poolHits        int64
	poolMisses      int64
	poolMaxSize     int64
	poolWarmingTime []float64 // Recent warming times in ms

	// Operation latencies (in milliseconds)
	createLatencies []float64
	startLatencies  []float64
	stopLatencies   []float64
	deleteLatencies []float64

	// Counters
	totalVMsCreated   int64
	totalVMsDestroyed int64
	totalContainers   int64
	activeContainers  int64

	// Error counters
	vmCreateErrors     int64
	vmDestroyErrors    int64
	containerErrors    int64
	agentConnectErrors int64

	// Resource metrics
	totalMemoryMB int64
	totalVCPUs    int64

	log *logrus.Entry
}

// NewCollector creates a new metrics collector.
func NewCollector(log *logrus.Entry) *Collector {
	return &Collector{
		log:             log.WithField("component", "metrics"),
		createLatencies: make([]float64, 0, 100),
		startLatencies:  make([]float64, 0, 100),
		stopLatencies:   make([]float64, 0, 100),
		deleteLatencies: make([]float64, 0, 100),
		poolWarmingTime: make([]float64, 0, 100),
	}
}

// =============================================================================
// VM Pool Metrics
// =============================================================================

// SetPoolStats updates VM pool statistics.
func (c *Collector) SetPoolStats(available, inUse, maxSize int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.poolAvailable = available
	c.poolInUse = inUse
	c.poolMaxSize = maxSize
}

// RecordPoolHit records a successful pool acquisition.
func (c *Collector) RecordPoolHit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.poolHits++
}

// RecordPoolMiss records a pool miss (new VM created).
func (c *Collector) RecordPoolMiss() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.poolMisses++
}

// RecordPoolWarmTime records the time to warm a VM in the pool.
func (c *Collector) RecordPoolWarmTime(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.poolWarmingTime = appendWithLimit(c.poolWarmingTime, float64(duration.Milliseconds()), 100)
}

// =============================================================================
// Operation Latency Metrics
// =============================================================================

// Timer helps measure operation latencies.
type Timer struct {
	start     time.Time
	collector *Collector
	operation string
}

// StartTimer starts a timer for an operation.
func (c *Collector) StartTimer(operation string) *Timer {
	return &Timer{
		start:     time.Now(),
		collector: c,
		operation: operation,
	}
}

// Stop stops the timer and records the latency.
func (t *Timer) Stop() time.Duration {
	duration := time.Since(t.start)
	t.collector.recordLatency(t.operation, duration)
	return duration
}

func (c *Collector) recordLatency(operation string, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ms := float64(duration.Milliseconds())

	switch operation {
	case "create":
		c.createLatencies = appendWithLimit(c.createLatencies, ms, 100)
	case "start":
		c.startLatencies = appendWithLimit(c.startLatencies, ms, 100)
	case "stop":
		c.stopLatencies = appendWithLimit(c.stopLatencies, ms, 100)
	case "delete":
		c.deleteLatencies = appendWithLimit(c.deleteLatencies, ms, 100)
	}
}

// =============================================================================
// Counter Metrics
// =============================================================================

// RecordVMCreated increments the VM creation counter.
func (c *Collector) RecordVMCreated(memoryMB, vcpus int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalVMsCreated++
	c.totalMemoryMB += memoryMB
	c.totalVCPUs += vcpus
}

// RecordVMDestroyed increments the VM destruction counter.
func (c *Collector) RecordVMDestroyed(memoryMB, vcpus int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalVMsDestroyed++
	c.totalMemoryMB -= memoryMB
	c.totalVCPUs -= vcpus
	if c.totalMemoryMB < 0 {
		c.totalMemoryMB = 0
	}
	if c.totalVCPUs < 0 {
		c.totalVCPUs = 0
	}
}

// RecordContainerCreated increments the container counter.
func (c *Collector) RecordContainerCreated() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalContainers++
	c.activeContainers++
}

// RecordContainerDestroyed decrements the active container counter.
func (c *Collector) RecordContainerDestroyed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeContainers--
	if c.activeContainers < 0 {
		c.activeContainers = 0
	}
}

// =============================================================================
// Error Metrics
// =============================================================================

// RecordVMCreateError records a VM creation error.
func (c *Collector) RecordVMCreateError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vmCreateErrors++
}

// RecordVMDestroyError records a VM destruction error.
func (c *Collector) RecordVMDestroyError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vmDestroyErrors++
}

// RecordContainerError records a container operation error.
func (c *Collector) RecordContainerError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.containerErrors++
}

// RecordAgentConnectError records an agent connection error.
func (c *Collector) RecordAgentConnectError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentConnectErrors++
}

// =============================================================================
// Metrics Export
// =============================================================================

// Snapshot returns a point-in-time snapshot of all metrics.
type Snapshot struct {
	// Pool
	PoolAvailable int64   `json:"pool_available"`
	PoolInUse     int64   `json:"pool_in_use"`
	PoolMaxSize   int64   `json:"pool_max_size"`
	PoolHits      int64   `json:"pool_hits"`
	PoolMisses    int64   `json:"pool_misses"`
	PoolHitRate   float64 `json:"pool_hit_rate"`

	// Latencies (p50, p95, p99 in ms)
	CreateLatencyP50 float64 `json:"create_latency_p50_ms"`
	CreateLatencyP95 float64 `json:"create_latency_p95_ms"`
	CreateLatencyP99 float64 `json:"create_latency_p99_ms"`
	StartLatencyP50  float64 `json:"start_latency_p50_ms"`
	StartLatencyP95  float64 `json:"start_latency_p95_ms"`
	StartLatencyP99  float64 `json:"start_latency_p99_ms"`

	// Counters
	TotalVMsCreated   int64 `json:"total_vms_created"`
	TotalVMsDestroyed int64 `json:"total_vms_destroyed"`
	TotalContainers   int64 `json:"total_containers"`
	ActiveContainers  int64 `json:"active_containers"`

	// Resources
	TotalMemoryMB int64 `json:"total_memory_mb"`
	TotalVCPUs    int64 `json:"total_vcpus"`

	// Errors
	VMCreateErrors     int64 `json:"vm_create_errors"`
	VMDestroyErrors    int64 `json:"vm_destroy_errors"`
	ContainerErrors    int64 `json:"container_errors"`
	AgentConnectErrors int64 `json:"agent_connect_errors"`
}

// GetSnapshot returns a snapshot of current metrics.
func (c *Collector) GetSnapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	hitRate := float64(0)
	total := c.poolHits + c.poolMisses
	if total > 0 {
		hitRate = float64(c.poolHits) / float64(total) * 100
	}

	return Snapshot{
		PoolAvailable: c.poolAvailable,
		PoolInUse:     c.poolInUse,
		PoolMaxSize:   c.poolMaxSize,
		PoolHits:      c.poolHits,
		PoolMisses:    c.poolMisses,
		PoolHitRate:   hitRate,

		CreateLatencyP50: percentile(c.createLatencies, 0.50),
		CreateLatencyP95: percentile(c.createLatencies, 0.95),
		CreateLatencyP99: percentile(c.createLatencies, 0.99),
		StartLatencyP50:  percentile(c.startLatencies, 0.50),
		StartLatencyP95:  percentile(c.startLatencies, 0.95),
		StartLatencyP99:  percentile(c.startLatencies, 0.99),

		TotalVMsCreated:   c.totalVMsCreated,
		TotalVMsDestroyed: c.totalVMsDestroyed,
		TotalContainers:   c.totalContainers,
		ActiveContainers:  c.activeContainers,

		TotalMemoryMB: c.totalMemoryMB,
		TotalVCPUs:    c.totalVCPUs,

		VMCreateErrors:     c.vmCreateErrors,
		VMDestroyErrors:    c.vmDestroyErrors,
		ContainerErrors:    c.containerErrors,
		AgentConnectErrors: c.agentConnectErrors,
	}
}

// PrometheusHandler returns an HTTP handler for Prometheus metrics.
func (c *Collector) PrometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snap := c.GetSnapshot()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")

		// Pool metrics
		writeMetric(w, "fc_cri_pool_available", "gauge", "Number of VMs available in pool", snap.PoolAvailable)
		writeMetric(w, "fc_cri_pool_in_use", "gauge", "Number of VMs currently in use", snap.PoolInUse)
		writeMetric(w, "fc_cri_pool_max_size", "gauge", "Maximum pool size", snap.PoolMaxSize)
		writeMetric(w, "fc_cri_pool_hits_total", "counter", "Total pool hits", snap.PoolHits)
		writeMetric(w, "fc_cri_pool_misses_total", "counter", "Total pool misses", snap.PoolMisses)
		writeMetricFloat(w, "fc_cri_pool_hit_rate", "gauge", "Pool hit rate percentage", snap.PoolHitRate)

		// Latency metrics
		writeMetricFloat(w, "fc_cri_create_latency_p50_ms", "gauge", "Container create latency p50", snap.CreateLatencyP50)
		writeMetricFloat(w, "fc_cri_create_latency_p95_ms", "gauge", "Container create latency p95", snap.CreateLatencyP95)
		writeMetricFloat(w, "fc_cri_create_latency_p99_ms", "gauge", "Container create latency p99", snap.CreateLatencyP99)
		writeMetricFloat(w, "fc_cri_start_latency_p50_ms", "gauge", "Container start latency p50", snap.StartLatencyP50)
		writeMetricFloat(w, "fc_cri_start_latency_p95_ms", "gauge", "Container start latency p95", snap.StartLatencyP95)
		writeMetricFloat(w, "fc_cri_start_latency_p99_ms", "gauge", "Container start latency p99", snap.StartLatencyP99)

		// Counter metrics
		writeMetric(w, "fc_cri_vms_created_total", "counter", "Total VMs created", snap.TotalVMsCreated)
		writeMetric(w, "fc_cri_vms_destroyed_total", "counter", "Total VMs destroyed", snap.TotalVMsDestroyed)
		writeMetric(w, "fc_cri_containers_total", "counter", "Total containers created", snap.TotalContainers)
		writeMetric(w, "fc_cri_containers_active", "gauge", "Active containers", snap.ActiveContainers)

		// Resource metrics
		writeMetric(w, "fc_cri_total_memory_mb", "gauge", "Total memory allocated to VMs (MB)", snap.TotalMemoryMB)
		writeMetric(w, "fc_cri_total_vcpus", "gauge", "Total vCPUs allocated to VMs", snap.TotalVCPUs)

		// Error metrics
		writeMetric(w, "fc_cri_vm_create_errors_total", "counter", "Total VM creation errors", snap.VMCreateErrors)
		writeMetric(w, "fc_cri_vm_destroy_errors_total", "counter", "Total VM destruction errors", snap.VMDestroyErrors)
		writeMetric(w, "fc_cri_container_errors_total", "counter", "Total container errors", snap.ContainerErrors)
		writeMetric(w, "fc_cri_agent_connect_errors_total", "counter", "Total agent connection errors", snap.AgentConnectErrors)
	})
}

// =============================================================================
// Helpers
// =============================================================================

func writeMetric(w http.ResponseWriter, name, metricType, help string, value int64) {
	_, _ = w.Write([]byte("# HELP " + name + " " + help + "\n"))
	_, _ = w.Write([]byte("# TYPE " + name + " " + metricType + "\n"))
	_, _ = w.Write([]byte(name + " " + itoa(value) + "\n"))
}

func writeMetricFloat(w http.ResponseWriter, name, metricType, help string, value float64) {
	_, _ = w.Write([]byte("# HELP " + name + " " + help + "\n"))
	_, _ = w.Write([]byte("# TYPE " + name + " " + metricType + "\n"))
	_, _ = w.Write([]byte(name + " " + ftoa(value) + "\n"))
}

func itoa(i int64) string {
	return string(appendInt(nil, i))
}

func ftoa(f float64) string {
	return string(appendFloat(nil, f))
}

func appendInt(b []byte, i int64) []byte {
	if i == 0 {
		return append(b, '0')
	}
	if i < 0 {
		b = append(b, '-')
		i = -i
	}
	var tmp [20]byte
	j := 20
	for i > 0 {
		j--
		tmp[j] = byte('0' + i%10)
		i /= 10
	}
	return append(b, tmp[j:]...)
}

func appendFloat(b []byte, f float64) []byte {
	// Simple float formatting with 2 decimal places
	i := int64(f * 100)
	whole := i / 100
	frac := i % 100
	if frac < 0 {
		frac = -frac
	}
	b = appendInt(b, whole)
	b = append(b, '.')
	if frac < 10 {
		b = append(b, '0')
	}
	b = appendInt(b, frac)
	return b
}

func appendWithLimit(slice []float64, value float64, limit int) []float64 {
	if len(slice) >= limit {
		// Remove oldest (first) element
		slice = slice[1:]
	}
	return append(slice, value)
}

func percentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return 0
	}

	// Make a copy and sort
	sorted := make([]float64, len(data))
	copy(sorted, data)

	// Simple insertion sort (good enough for small arrays)
	for i := 1; i < len(sorted); i++ {
		j := i
		for j > 0 && sorted[j-1] > sorted[j] {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
			j--
		}
	}

	// Calculate percentile index
	index := int(float64(len(sorted)-1) * p)
	return sorted[index]
}

// =============================================================================
// Global Collector (convenience)
// =============================================================================

var globalCollector *Collector
var globalOnce sync.Once

// Global returns the global metrics collector.
func Global() *Collector {
	globalOnce.Do(func() {
		globalCollector = NewCollector(logrus.NewEntry(logrus.StandardLogger()))
	})
	return globalCollector
}

// SetGlobal sets the global metrics collector.
func SetGlobal(c *Collector) {
	globalCollector = c
}
