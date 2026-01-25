// Package vm provides VM pool management for pre-warming Firecracker VMs.
package vm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
)

// Pool implements domain.VMPool for pre-warming Firecracker VMs.
// This is critical for achieving <50ms container start times.
//
// The pool maintains a set of ready-to-use VMs that can be immediately
// assigned to new pod sandboxes. When a VM is acquired, it's customized
// for the specific workload (rootfs attached, networking configured).
type Pool struct {
	mu sync.Mutex

	manager *Manager
	config  PoolConfig
	log     *logrus.Entry

	// Pool of ready VMs
	available chan *domain.Sandbox

	// Tracking
	inUse map[string]*domain.Sandbox

	// Statistics
	stats poolStats

	// Lifecycle
	ctx     context.Context
	cancel  context.CancelFunc
	warmSem *semaphore.Weighted // Limit concurrent warming
	closed  bool
}

type poolStats struct {
	totalServed int64
	poolHits    int64
	poolMisses  int64
}

// PoolConfig configures the VM pool behavior.
type PoolConfig struct {
	// MaxSize is the maximum number of pre-warmed VMs to maintain.
	MaxSize int

	// MinSize is the minimum number of VMs to keep warm.
	MinSize int

	// MaxIdleTime is how long a VM can sit idle before being destroyed.
	MaxIdleTime time.Duration

	// WarmConcurrency limits how many VMs can be created simultaneously.
	WarmConcurrency int

	// DefaultVMConfig is the configuration for pre-warmed VMs.
	DefaultVMConfig domain.VMConfig

	// ReplenishInterval is how often to check and refill the pool.
	ReplenishInterval time.Duration
}

// DefaultPoolConfig returns sensible defaults for the pool.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxSize:           10,
		MinSize:           3,
		MaxIdleTime:       5 * time.Minute,
		WarmConcurrency:   2,
		DefaultVMConfig:   domain.DefaultVMConfig(),
		ReplenishInterval: 10 * time.Second,
	}
}

// NewPool creates a new VM pool.
func NewPool(manager *Manager, config PoolConfig, log *logrus.Entry) (*Pool, error) {
	ctx, cancel := context.WithCancel(context.Background())

	pool := &Pool{
		manager:   manager,
		config:    config,
		log:       log.WithField("component", "vm-pool"),
		available: make(chan *domain.Sandbox, config.MaxSize),
		inUse:     make(map[string]*domain.Sandbox),
		ctx:       ctx,
		cancel:    cancel,
		warmSem:   semaphore.NewWeighted(int64(config.WarmConcurrency)),
	}

	// Start background workers
	go pool.replenishLoop()
	go pool.cleanupLoop()

	return pool, nil
}

// Acquire gets a pre-warmed VM from the pool, or creates a new one if empty.
// This is the hot path - needs to be fast.
func (p *Pool) Acquire(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error) {
	atomic.AddInt64(&p.stats.totalServed, 1)

	// Try to get from pool first (non-blocking)
	select {
	case sandbox := <-p.available:
		atomic.AddInt64(&p.stats.poolHits, 1)
		p.log.WithField("sandbox_id", sandbox.ID).Debug("Acquired VM from pool")

		// Mark as in-use
		p.mu.Lock()
		sandbox.FromPool = true
		p.inUse[sandbox.ID] = sandbox
		p.mu.Unlock()

		// Customize the VM for this workload
		if err := p.customizeVM(ctx, sandbox, config); err != nil {
			// Failed to customize, destroy and create fresh
			_ = p.manager.DestroyVM(ctx, sandbox)
			return p.createFresh(ctx, config)
		}

		return sandbox, nil

	default:
		// Pool empty, create fresh
		atomic.AddInt64(&p.stats.poolMisses, 1)
		p.log.Debug("Pool empty, creating fresh VM")
		return p.createFresh(ctx, config)
	}
}

// Release returns a VM to the pool or destroys it.
func (p *Pool) Release(ctx context.Context, sandbox *domain.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.inUse, sandbox.ID)

	// Check if pool is full or VM is too old
	poolSize := len(p.available)
	vmAge := time.Since(sandbox.CreatedAt)

	if poolSize >= p.config.MaxSize || vmAge > p.config.MaxIdleTime {
		p.log.WithFields(logrus.Fields{
			"sandbox_id": sandbox.ID,
			"pool_size":  poolSize,
			"vm_age":     vmAge,
		}).Debug("Destroying VM instead of returning to pool")
		return p.manager.DestroyVM(ctx, sandbox)
	}

	// Reset the VM state for reuse
	if err := p.resetVM(ctx, sandbox); err != nil {
		p.log.WithError(err).Warn("Failed to reset VM, destroying")
		return p.manager.DestroyVM(ctx, sandbox)
	}

	// Return to pool
	sandbox.PooledAt = time.Now()
	select {
	case p.available <- sandbox:
		p.log.WithField("sandbox_id", sandbox.ID).Debug("Returned VM to pool")
	default:
		// Pool full (race condition), destroy
		_ = p.manager.DestroyVM(ctx, sandbox)
	}

	return nil
}

// Warm adds pre-warmed VMs to the pool.
func (p *Pool) Warm(ctx context.Context, count int, config domain.VMConfig) error {
	p.log.WithField("count", count).Info("Warming VM pool")

	var wg sync.WaitGroup
	errChan := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Respect concurrency limit
			if err := p.warmSem.Acquire(ctx, 1); err != nil {
				errChan <- err
				return
			}
			defer p.warmSem.Release(1)

			sandbox, err := p.manager.CreateVM(ctx, config)
			if err != nil {
				errChan <- err
				return
			}

			sandbox.PooledAt = time.Now()

			select {
			case p.available <- sandbox:
				p.log.WithField("sandbox_id", sandbox.ID).Debug("Added warmed VM to pool")
			default:
				// Pool full
				_ = p.manager.DestroyVM(ctx, sandbox)
			}
		}()
	}

	wg.Wait()
	close(errChan)

	// Collect errors
	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to warm %d VMs", len(errs))
	}

	return nil
}

// Stats returns pool statistics.
func (p *Pool) Stats() domain.PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return domain.PoolStats{
		Available:   len(p.available),
		InUse:       len(p.inUse),
		MaxSize:     p.config.MaxSize,
		TotalServed: atomic.LoadInt64(&p.stats.totalServed),
		PoolHits:    atomic.LoadInt64(&p.stats.poolHits),
		PoolMisses:  atomic.LoadInt64(&p.stats.poolMisses),
	}
}

// Close shuts down the pool and all VMs.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	p.cancel() // Stop background loops

	p.log.Info("Closing VM pool")

	// Destroy all available VMs
	close(p.available)
	for sandbox := range p.available {
		if err := p.manager.DestroyVM(ctx, sandbox); err != nil {
			p.log.WithError(err).Warn("Error destroying pooled VM")
		}
	}

	// Destroy in-use VMs
	p.mu.Lock()
	for _, sandbox := range p.inUse {
		if err := p.manager.DestroyVM(ctx, sandbox); err != nil {
			p.log.WithError(err).Warn("Error destroying in-use VM")
		}
	}
	p.mu.Unlock()

	return nil
}

// createFresh creates a new VM outside the pool.
func (p *Pool) createFresh(ctx context.Context, config domain.VMConfig) (*domain.Sandbox, error) {
	sandbox, err := p.manager.CreateVM(ctx, config)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.inUse[sandbox.ID] = sandbox
	p.mu.Unlock()

	return sandbox, nil
}

// customizeVM applies workload-specific configuration to a pooled VM.
// This includes attaching the actual rootfs, configuring networking, etc.
func (p *Pool) customizeVM(ctx context.Context, sandbox *domain.Sandbox, config domain.VMConfig) error {
	// In a real implementation, you would:
	// 1. Hot-attach the actual rootfs block device
	// 2. Configure networking via the agent
	// 3. Apply any workload-specific settings

	// For now, just update the config
	sandbox.VMConfig = config
	return nil
}

// resetVM resets a VM for reuse in the pool.
func (p *Pool) resetVM(ctx context.Context, sandbox *domain.Sandbox) error {
	// In a real implementation, you would:
	// 1. Kill all processes inside the VM
	// 2. Detach workload-specific drives
	// 3. Reset networking
	// 4. Clear any state

	// Reset container map
	sandbox.Containers = make(map[string]*domain.Container)

	return nil
}

// replenishLoop maintains the minimum pool size.
func (p *Pool) replenishLoop() {
	ticker := time.NewTicker(p.config.ReplenishInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.replenish()
		}
	}
}

func (p *Pool) replenish() {
	currentSize := len(p.available)

	if currentSize < p.config.MinSize {
		needed := p.config.MinSize - currentSize
		p.log.WithFields(logrus.Fields{
			"current": currentSize,
			"min":     p.config.MinSize,
			"needed":  needed,
		}).Debug("Replenishing pool")

		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		defer cancel()

		_ = p.Warm(ctx, needed, p.config.DefaultVMConfig)
	}
}

// cleanupLoop removes idle VMs that have been in the pool too long.
func (p *Pool) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.cleanupIdle()
		}
	}
}

func (p *Pool) cleanupIdle() {
	// Drain and re-add non-expired VMs
	var keep []*domain.Sandbox

	for {
		select {
		case sandbox := <-p.available:
			if time.Since(sandbox.PooledAt) > p.config.MaxIdleTime {
				p.log.WithFields(logrus.Fields{
					"sandbox_id": sandbox.ID,
					"idle_time":  time.Since(sandbox.PooledAt),
				}).Debug("Removing idle VM from pool")

				ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
				_ = p.manager.DestroyVM(ctx, sandbox)
				cancel()
			} else {
				keep = append(keep, sandbox)
			}
		default:
			// Pool drained
			goto refill
		}
	}

refill:
	// Put non-expired VMs back
	for _, sandbox := range keep {
		select {
		case p.available <- sandbox:
		default:
			// Pool somehow full, destroy
			ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
			_ = p.manager.DestroyVM(ctx, sandbox)
			cancel()
		}
	}
}
