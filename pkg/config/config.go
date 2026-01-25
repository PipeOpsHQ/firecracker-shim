// Package config provides centralized configuration management for the Firecracker CRI runtime.
//
// Configuration can be loaded from:
// - TOML configuration file (default: /etc/fc-cri/config.toml)
// - Environment variables (prefixed with FC_CRI_)
// - Command-line flags (for overrides)
//
// Configuration is organized into sections matching the domain components:
// - Runtime: General runtime settings
// - VM: Default VM configuration
// - Pool: VM pool settings
// - Network: CNI configuration
// - Image: Image service settings
// - Agent: Guest agent settings
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Config holds all configuration for the Firecracker CRI runtime.
type Config struct {
	// Runtime configuration
	Runtime RuntimeConfig `toml:"runtime"`

	// VM configuration defaults
	VM VMConfig `toml:"vm"`

	// VM pool configuration
	Pool PoolConfig `toml:"pool"`

	// Network configuration
	Network NetworkConfig `toml:"network"`

	// Image service configuration
	Image ImageConfig `toml:"image"`

	// Agent configuration
	Agent AgentConfig `toml:"agent"`

	// Metrics configuration
	Metrics MetricsConfig `toml:"metrics"`

	// Logging configuration
	Log LogConfig `toml:"log"`
}

// RuntimeConfig holds general runtime settings.
type RuntimeConfig struct {
	// RuntimeDir is the directory for runtime state (sockets, etc.).
	RuntimeDir string `toml:"runtime_dir"`

	// FirecrackerBinary is the path to the firecracker binary.
	FirecrackerBinary string `toml:"firecracker_binary"`

	// JailerBinary is the path to the jailer binary.
	JailerBinary string `toml:"jailer_binary"`

	// EnableJailer controls whether to use the jailer for security isolation.
	EnableJailer bool `toml:"enable_jailer"`

	// ShutdownTimeout is how long to wait for graceful shutdown.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout"`

	// ContainerdSocket is the path to containerd's socket.
	ContainerdSocket string `toml:"containerd_socket"`
}

// VMConfig holds default VM configuration.
type VMConfig struct {
	// KernelPath is the path to the kernel image.
	KernelPath string `toml:"kernel_path"`

	// KernelArgs are the default kernel boot arguments.
	KernelArgs string `toml:"kernel_args"`

	// InitrdPath is the optional path to an initrd.
	InitrdPath string `toml:"initrd_path"`

	// DefaultVcpuCount is the default number of vCPUs per VM.
	DefaultVcpuCount int64 `toml:"default_vcpu_count"`

	// DefaultMemoryMB is the default memory size in MB.
	DefaultMemoryMB int64 `toml:"default_memory_mb"`

	// MinMemoryMB is the minimum memory size in MB.
	MinMemoryMB int64 `toml:"min_memory_mb"`

	// MaxMemoryMB is the maximum memory size in MB.
	MaxMemoryMB int64 `toml:"max_memory_mb"`

	// EnableSMT controls whether simultaneous multithreading is enabled.
	EnableSMT bool `toml:"enable_smt"`

	// BaseRootfsPath is the path to the base rootfs used for pooled VMs.
	BaseRootfsPath string `toml:"base_rootfs_path"`

	// VsockEnabled controls whether vsock is enabled for guest communication.
	VsockEnabled bool `toml:"vsock_enabled"`
}

// PoolConfig holds VM pool configuration.
type PoolConfig struct {
	// Enabled controls whether VM pooling is enabled.
	Enabled bool `toml:"enabled"`

	// MaxSize is the maximum number of pre-warmed VMs.
	MaxSize int `toml:"max_size"`

	// MinSize is the minimum number of VMs to keep warm.
	MinSize int `toml:"min_size"`

	// MaxIdleTime is how long a VM can sit idle before being destroyed.
	MaxIdleTime time.Duration `toml:"max_idle_time"`

	// WarmConcurrency limits how many VMs can be created simultaneously.
	WarmConcurrency int `toml:"warm_concurrency"`

	// ReplenishInterval is how often to check and refill the pool.
	ReplenishInterval time.Duration `toml:"replenish_interval"`

	// PrewarmOnStart controls whether to pre-warm the pool on startup.
	PrewarmOnStart bool `toml:"prewarm_on_start"`
}

// NetworkConfig holds CNI configuration.
type NetworkConfig struct {
	// NetworkMode is the network mode: "cni" or "none".
	NetworkMode string `toml:"network_mode"`

	// CNIPluginDir is the directory containing CNI plugins.
	CNIPluginDir string `toml:"cni_plugin_dir"`

	// CNIConfDir is the directory containing CNI configuration files.
	CNIConfDir string `toml:"cni_conf_dir"`

	// CNICacheDir is the directory for CNI state cache.
	CNICacheDir string `toml:"cni_cache_dir"`

	// DefaultNetworkName is the default CNI network to use.
	DefaultNetworkName string `toml:"default_network_name"`

	// DefaultSubnet is used if not specified in CNI config.
	DefaultSubnet string `toml:"default_subnet"`
}

// ImageConfig holds image service configuration.
type ImageConfig struct {
	// RootDir is the directory for storing image data.
	RootDir string `toml:"root_dir"`

	// DefaultBlockSizeMB is the default size for block device images.
	DefaultBlockSizeMB int64 `toml:"default_block_size_mb"`

	// UseSparseFiles enables sparse file creation for efficiency.
	UseSparseFiles bool `toml:"use_sparse_files"`

	// CacheEnabled enables image caching.
	CacheEnabled bool `toml:"cache_enabled"`

	// CacheMaxSizeMB is the maximum cache size in MB.
	CacheMaxSizeMB int64 `toml:"cache_max_size_mb"`
}

// AgentConfig holds guest agent configuration.
type AgentConfig struct {
	// VsockPort is the port the guest agent listens on.
	VsockPort uint32 `toml:"vsock_port"`

	// ConnectTimeout is how long to wait for agent connection.
	ConnectTimeout time.Duration `toml:"connect_timeout"`

	// DialRetries is the number of connection retries.
	DialRetries int `toml:"dial_retries"`

	// DialRetryInterval is the interval between retries.
	DialRetryInterval time.Duration `toml:"dial_retry_interval"`

	// CommandTimeout is the default timeout for agent commands.
	CommandTimeout time.Duration `toml:"command_timeout"`
}

// MetricsConfig holds metrics configuration.
type MetricsConfig struct {
	// Enabled controls whether metrics are enabled.
	Enabled bool `toml:"enabled"`

	// Address is the address to listen on for metrics.
	Address string `toml:"address"`

	// Path is the HTTP path for metrics endpoint.
	Path string `toml:"path"`
}

// LogConfig holds logging configuration.
type LogConfig struct {
	// Level is the log level: debug, info, warn, error.
	Level string `toml:"level"`

	// Format is the log format: text, json.
	Format string `toml:"format"`

	// File is the optional log file path.
	File string `toml:"file"`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Runtime: RuntimeConfig{
			RuntimeDir:        "/run/fc-cri",
			FirecrackerBinary: "/usr/bin/firecracker",
			JailerBinary:      "/usr/bin/jailer",
			EnableJailer:      false,
			ShutdownTimeout:   30 * time.Second,
			ContainerdSocket:  "/run/containerd/containerd.sock",
		},
		VM: VMConfig{
			KernelPath:       "/var/lib/fc-cri/vmlinux",
			KernelArgs:       "console=ttyS0 reboot=k panic=1 pci=off quiet",
			DefaultVcpuCount: 1,
			DefaultMemoryMB:  128,
			MinMemoryMB:      64,
			MaxMemoryMB:      8192,
			EnableSMT:        false,
			BaseRootfsPath:   "/var/lib/fc-cri/rootfs/base.ext4",
			VsockEnabled:     true,
		},
		Pool: PoolConfig{
			Enabled:           true,
			MaxSize:           10,
			MinSize:           3,
			MaxIdleTime:       5 * time.Minute,
			WarmConcurrency:   2,
			ReplenishInterval: 10 * time.Second,
			PrewarmOnStart:    true,
		},
		Network: NetworkConfig{
			NetworkMode:        "cni",
			CNIPluginDir:       "/opt/cni/bin",
			CNIConfDir:         "/etc/cni/net.d",
			CNICacheDir:        "/var/lib/cni",
			DefaultNetworkName: "fc-net",
			DefaultSubnet:      "10.88.0.0/16",
		},
		Image: ImageConfig{
			RootDir:            "/var/lib/fc-cri/images",
			DefaultBlockSizeMB: 1024,
			UseSparseFiles:     true,
			CacheEnabled:       true,
			CacheMaxSizeMB:     10240,
		},
		Agent: AgentConfig{
			VsockPort:         1024,
			ConnectTimeout:    30 * time.Second,
			DialRetries:       30,
			DialRetryInterval: 100 * time.Millisecond,
			CommandTimeout:    60 * time.Second,
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Address: ":9090",
			Path:    "/metrics",
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// LoadFromFile loads configuration from a TOML file.
func LoadFromFile(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return defaults if file doesn't exist
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := parseTOML(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return cfg, nil
}

// LoadFromEnv loads configuration from environment variables.
// Environment variables are prefixed with FC_CRI_ and use underscores.
// Example: FC_CRI_VM_DEFAULT_MEMORY_MB=256
func LoadFromEnv(cfg *Config) {
	// Runtime
	loadEnvString(&cfg.Runtime.RuntimeDir, "FC_CRI_RUNTIME_DIR")
	loadEnvString(&cfg.Runtime.FirecrackerBinary, "FC_CRI_FIRECRACKER_BINARY")
	loadEnvString(&cfg.Runtime.JailerBinary, "FC_CRI_JAILER_BINARY")
	loadEnvBool(&cfg.Runtime.EnableJailer, "FC_CRI_ENABLE_JAILER")
	loadEnvDuration(&cfg.Runtime.ShutdownTimeout, "FC_CRI_SHUTDOWN_TIMEOUT")

	// VM
	loadEnvString(&cfg.VM.KernelPath, "FC_CRI_VM_KERNEL_PATH")
	loadEnvString(&cfg.VM.KernelArgs, "FC_CRI_VM_KERNEL_ARGS")
	loadEnvInt64(&cfg.VM.DefaultVcpuCount, "FC_CRI_VM_DEFAULT_VCPU_COUNT")
	loadEnvInt64(&cfg.VM.DefaultMemoryMB, "FC_CRI_VM_DEFAULT_MEMORY_MB")
	loadEnvInt64(&cfg.VM.MinMemoryMB, "FC_CRI_VM_MIN_MEMORY_MB")
	loadEnvInt64(&cfg.VM.MaxMemoryMB, "FC_CRI_VM_MAX_MEMORY_MB")
	loadEnvBool(&cfg.VM.EnableSMT, "FC_CRI_VM_ENABLE_SMT")

	// Pool
	loadEnvBool(&cfg.Pool.Enabled, "FC_CRI_POOL_ENABLED")
	loadEnvInt(&cfg.Pool.MaxSize, "FC_CRI_POOL_MAX_SIZE")
	loadEnvInt(&cfg.Pool.MinSize, "FC_CRI_POOL_MIN_SIZE")
	loadEnvDuration(&cfg.Pool.MaxIdleTime, "FC_CRI_POOL_MAX_IDLE_TIME")
	loadEnvInt(&cfg.Pool.WarmConcurrency, "FC_CRI_POOL_WARM_CONCURRENCY")

	// Network
	loadEnvString(&cfg.Network.NetworkMode, "FC_CRI_NETWORK_MODE")
	loadEnvString(&cfg.Network.CNIPluginDir, "FC_CRI_CNI_PLUGIN_DIR")
	loadEnvString(&cfg.Network.CNIConfDir, "FC_CRI_CNI_CONF_DIR")
	loadEnvString(&cfg.Network.DefaultSubnet, "FC_CRI_DEFAULT_SUBNET")

	// Image
	loadEnvString(&cfg.Image.RootDir, "FC_CRI_IMAGE_ROOT_DIR")
	loadEnvInt64(&cfg.Image.DefaultBlockSizeMB, "FC_CRI_IMAGE_DEFAULT_BLOCK_SIZE_MB")
	loadEnvBool(&cfg.Image.UseSparseFiles, "FC_CRI_IMAGE_USE_SPARSE_FILES")

	// Metrics
	loadEnvBool(&cfg.Metrics.Enabled, "FC_CRI_METRICS_ENABLED")
	loadEnvString(&cfg.Metrics.Address, "FC_CRI_METRICS_ADDRESS")

	// Logging
	loadEnvString(&cfg.Log.Level, "FC_CRI_LOG_LEVEL")
	loadEnvString(&cfg.Log.Format, "FC_CRI_LOG_FORMAT")
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	// Check required paths exist or can be created
	for _, dir := range []string{
		c.Runtime.RuntimeDir,
		c.Image.RootDir,
	} {
		if err := ensureDir(dir); err != nil {
			return fmt.Errorf("failed to ensure directory %s: %w", dir, err)
		}
	}

	// Validate binaries exist
	for _, bin := range []string{
		c.Runtime.FirecrackerBinary,
	} {
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("binary not found: %s", bin)
		}
	}

	// Validate kernel exists
	if _, err := os.Stat(c.VM.KernelPath); err != nil {
		return fmt.Errorf("kernel not found: %s", c.VM.KernelPath)
	}

	// Validate memory limits
	if c.VM.MinMemoryMB > c.VM.MaxMemoryMB {
		return fmt.Errorf("min_memory_mb (%d) > max_memory_mb (%d)", c.VM.MinMemoryMB, c.VM.MaxMemoryMB)
	}
	if c.VM.DefaultMemoryMB < c.VM.MinMemoryMB || c.VM.DefaultMemoryMB > c.VM.MaxMemoryMB {
		return fmt.Errorf("default_memory_mb (%d) not in range [%d, %d]",
			c.VM.DefaultMemoryMB, c.VM.MinMemoryMB, c.VM.MaxMemoryMB)
	}

	// Validate pool settings
	if c.Pool.Enabled {
		if c.Pool.MinSize > c.Pool.MaxSize {
			return fmt.Errorf("pool min_size (%d) > max_size (%d)", c.Pool.MinSize, c.Pool.MaxSize)
		}
	}

	// Validate network mode
	validModes := map[string]bool{"cni": true, "none": true}
	if !validModes[c.Network.NetworkMode] {
		return fmt.Errorf("invalid network_mode: %s (must be 'cni' or 'none')", c.Network.NetworkMode)
	}

	// Validate log level
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Log.Level] {
		return fmt.Errorf("invalid log level: %s", c.Log.Level)
	}

	return nil
}

// ApplyToLogger applies logging configuration.
func (c *Config) ApplyToLogger(log *logrus.Logger) {
	// Set level
	switch c.Log.Level {
	case "debug":
		log.SetLevel(logrus.DebugLevel)
	case "info":
		log.SetLevel(logrus.InfoLevel)
	case "warn":
		log.SetLevel(logrus.WarnLevel)
	case "error":
		log.SetLevel(logrus.ErrorLevel)
	default:
		log.SetLevel(logrus.InfoLevel)
	}

	// Set format
	switch c.Log.Format {
	case "json":
		log.SetFormatter(&logrus.JSONFormatter{})
	default:
		log.SetFormatter(&logrus.TextFormatter{
			FullTimestamp: true,
		})
	}

	// Set output file if specified
	if c.Log.File != "" {
		dir := filepath.Dir(c.Log.File)
		if err := os.MkdirAll(dir, 0755); err == nil {
			if f, err := os.OpenFile(c.Log.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				log.SetOutput(f)
			}
		}
	}
}

// =============================================================================
// Helper Functions
// =============================================================================

func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func loadEnvString(target *string, key string) {
	if val := os.Getenv(key); val != "" {
		*target = val
	}
}

func loadEnvBool(target *bool, key string) {
	if val := os.Getenv(key); val != "" {
		*target = val == "true" || val == "1" || val == "yes"
	}
}

func loadEnvInt(target *int, key string) {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			*target = i
		}
	}
}

func loadEnvInt64(target *int64, key string) {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			*target = i
		}
	}
}

func loadEnvDuration(target *time.Duration, key string) {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			*target = d
		}
	}
}

// parseTOML is a simple TOML parser for our specific config format.
// For production, use a proper TOML library like github.com/BurntSushi/toml
func parseTOML(data []byte, cfg *Config) error {
	lines := strings.Split(string(data), "\n")
	currentSection := ""

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Section header
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.Trim(line, "[]")
			continue
		}

		// Key-value pair
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove quotes from string values
		value = strings.Trim(value, `"'`)

		// Apply value based on section and key
		applyConfigValue(cfg, currentSection, key, value)
	}

	return nil
}

func applyConfigValue(cfg *Config, section, key, value string) {
	switch section {
	case "runtime":
		switch key {
		case "runtime_dir":
			cfg.Runtime.RuntimeDir = value
		case "firecracker_binary":
			cfg.Runtime.FirecrackerBinary = value
		case "jailer_binary":
			cfg.Runtime.JailerBinary = value
		case "enable_jailer":
			cfg.Runtime.EnableJailer = value == "true"
		case "shutdown_timeout":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Runtime.ShutdownTimeout = d
			}
		}

	case "vm":
		switch key {
		case "kernel_path":
			cfg.VM.KernelPath = value
		case "kernel_args":
			cfg.VM.KernelArgs = value
		case "default_vcpu_count":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.VM.DefaultVcpuCount = i
			}
		case "default_memory_mb":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.VM.DefaultMemoryMB = i
			}
		case "min_memory_mb":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.VM.MinMemoryMB = i
			}
		case "max_memory_mb":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.VM.MaxMemoryMB = i
			}
		case "enable_smt":
			cfg.VM.EnableSMT = value == "true"
		case "base_rootfs_path":
			cfg.VM.BaseRootfsPath = value
		case "vsock_enabled":
			cfg.VM.VsockEnabled = value == "true"
		}

	case "pool":
		switch key {
		case "enabled":
			cfg.Pool.Enabled = value == "true"
		case "max_size":
			if i, err := strconv.Atoi(value); err == nil {
				cfg.Pool.MaxSize = i
			}
		case "min_size":
			if i, err := strconv.Atoi(value); err == nil {
				cfg.Pool.MinSize = i
			}
		case "max_idle_time":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Pool.MaxIdleTime = d
			}
		case "warm_concurrency":
			if i, err := strconv.Atoi(value); err == nil {
				cfg.Pool.WarmConcurrency = i
			}
		case "replenish_interval":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Pool.ReplenishInterval = d
			}
		case "prewarm_on_start":
			cfg.Pool.PrewarmOnStart = value == "true"
		}

	case "network":
		switch key {
		case "network_mode":
			cfg.Network.NetworkMode = value
		case "cni_plugin_dir":
			cfg.Network.CNIPluginDir = value
		case "cni_conf_dir":
			cfg.Network.CNIConfDir = value
		case "cni_cache_dir":
			cfg.Network.CNICacheDir = value
		case "default_network_name":
			cfg.Network.DefaultNetworkName = value
		case "default_subnet":
			cfg.Network.DefaultSubnet = value
		}

	case "image":
		switch key {
		case "root_dir":
			cfg.Image.RootDir = value
		case "default_block_size_mb":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.Image.DefaultBlockSizeMB = i
			}
		case "use_sparse_files":
			cfg.Image.UseSparseFiles = value == "true"
		case "cache_enabled":
			cfg.Image.CacheEnabled = value == "true"
		case "cache_max_size_mb":
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				cfg.Image.CacheMaxSizeMB = i
			}
		}

	case "agent":
		switch key {
		case "vsock_port":
			if i, err := strconv.ParseUint(value, 10, 32); err == nil {
				cfg.Agent.VsockPort = uint32(i)
			}
		case "connect_timeout":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Agent.ConnectTimeout = d
			}
		case "dial_retries":
			if i, err := strconv.Atoi(value); err == nil {
				cfg.Agent.DialRetries = i
			}
		case "dial_retry_interval":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Agent.DialRetryInterval = d
			}
		case "command_timeout":
			if d, err := time.ParseDuration(value); err == nil {
				cfg.Agent.CommandTimeout = d
			}
		}

	case "metrics":
		switch key {
		case "enabled":
			cfg.Metrics.Enabled = value == "true"
		case "address":
			cfg.Metrics.Address = value
		case "path":
			cfg.Metrics.Path = value
		}

	case "log":
		switch key {
		case "level":
			cfg.Log.Level = value
		case "format":
			cfg.Log.Format = value
		case "file":
			cfg.Log.File = value
		}
	}
}
