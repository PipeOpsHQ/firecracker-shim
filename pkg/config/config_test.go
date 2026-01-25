package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.VM.DefaultVcpuCount != 1 {
		t.Errorf("Default DefaultVcpuCount = %d, want 1", cfg.VM.DefaultVcpuCount)
	}
	if cfg.VM.DefaultMemoryMB != 128 {
		t.Errorf("Default DefaultMemoryMB = %d, want 128", cfg.VM.DefaultMemoryMB)
	}
	if cfg.Pool.Enabled != true {
		t.Errorf("Default Pool.Enabled = %v, want true", cfg.Pool.Enabled)
	}
	if cfg.Network.NetworkMode != "cni" {
		t.Errorf("Default Network.NetworkMode = %s, want cni", cfg.Network.NetworkMode)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Default Log.Level = %s, want info", cfg.Log.Level)
	}
}

func TestLoadFromFile(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.toml")

	content := `
[runtime]
runtime_dir = "/tmp/fc-cri"
enable_jailer = true

[vm]
default_vcpu_count = 4
default_memory_mb = 1024
kernel_args = "console=ttyS0 reboot=k"

[pool]
enabled = false
max_size = 20

[network]
network_mode = "none"

[log]
level = "debug"
`
	if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Load the config
	cfg, err := LoadFromFile(configFile)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	// Verify values
	if cfg.Runtime.RuntimeDir != "/tmp/fc-cri" {
		t.Errorf("RuntimeDir = %s, want /tmp/fc-cri", cfg.Runtime.RuntimeDir)
	}
	if !cfg.Runtime.EnableJailer {
		t.Errorf("EnableJailer = false, want true")
	}
	if cfg.VM.DefaultVcpuCount != 4 {
		t.Errorf("DefaultVcpuCount = %d, want 4", cfg.VM.DefaultVcpuCount)
	}
	if cfg.VM.DefaultMemoryMB != 1024 {
		t.Errorf("DefaultMemoryMB = %d, want 1024", cfg.VM.DefaultMemoryMB)
	}
	if cfg.VM.KernelArgs != "console=ttyS0 reboot=k" {
		t.Errorf("KernelArgs = %s, want console=ttyS0 reboot=k", cfg.VM.KernelArgs)
	}
	if cfg.Pool.Enabled {
		t.Errorf("Pool.Enabled = true, want false")
	}
	if cfg.Pool.MaxSize != 20 {
		t.Errorf("Pool.MaxSize = %d, want 20", cfg.Pool.MaxSize)
	}
	if cfg.Network.NetworkMode != "none" {
		t.Errorf("NetworkMode = %s, want none", cfg.Network.NetworkMode)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level = %s, want debug", cfg.Log.Level)
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("FC_CRI_RUNTIME_DIR", "/env/runtime")
	os.Setenv("FC_CRI_VM_DEFAULT_VCPU_COUNT", "8")
	os.Setenv("FC_CRI_POOL_ENABLED", "false")
	os.Setenv("FC_CRI_SHUTDOWN_TIMEOUT", "1m")
	defer func() {
		os.Unsetenv("FC_CRI_RUNTIME_DIR")
		os.Unsetenv("FC_CRI_VM_DEFAULT_VCPU_COUNT")
		os.Unsetenv("FC_CRI_POOL_ENABLED")
		os.Unsetenv("FC_CRI_SHUTDOWN_TIMEOUT")
	}()

	cfg := Default()
	LoadFromEnv(cfg)

	if cfg.Runtime.RuntimeDir != "/env/runtime" {
		t.Errorf("RuntimeDir = %s, want /env/runtime", cfg.Runtime.RuntimeDir)
	}
	if cfg.VM.DefaultVcpuCount != 8 {
		t.Errorf("DefaultVcpuCount = %d, want 8", cfg.VM.DefaultVcpuCount)
	}
	if cfg.Pool.Enabled {
		t.Errorf("Pool.Enabled = true, want false")
	}
	if cfg.Runtime.ShutdownTimeout != 1*time.Minute {
		t.Errorf("ShutdownTimeout = %s, want 1m", cfg.Runtime.ShutdownTimeout)
	}
}

func TestValidate(t *testing.T) {
	// Create required directories for validation
	tmpDir := t.TempDir()
	runtimeDir := filepath.Join(tmpDir, "runtime")
	rootDir := filepath.Join(tmpDir, "images")
	binFile := filepath.Join(tmpDir, "firecracker")
	kernelFile := filepath.Join(tmpDir, "vmlinux")

	os.MkdirAll(runtimeDir, 0755)
	os.MkdirAll(rootDir, 0755)
	os.WriteFile(binFile, []byte("fake binary"), 0755)
	os.WriteFile(kernelFile, []byte("fake kernel"), 0644)

	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name: "Valid config",
			modify: func(c *Config) {
				c.Runtime.RuntimeDir = runtimeDir
				c.Image.RootDir = rootDir
				c.Runtime.FirecrackerBinary = binFile
				c.VM.KernelPath = kernelFile
			},
			wantErr: false,
		},
		{
			name: "Invalid binary",
			modify: func(c *Config) {
				c.Runtime.FirecrackerBinary = "/non/existent/binary"
			},
			wantErr: true,
		},
		{
			name: "Invalid kernel",
			modify: func(c *Config) {
				c.VM.KernelPath = "/non/existent/kernel"
			},
			wantErr: true,
		},
		{
			name: "Invalid memory range",
			modify: func(c *Config) {
				c.VM.DefaultMemoryMB = 32 // Less than min
			},
			wantErr: true,
		},
		{
			name: "Invalid pool config",
			modify: func(c *Config) {
				c.Pool.MinSize = 10
				c.Pool.MaxSize = 5 // Min > Max
			},
			wantErr: true,
		},
		{
			name: "Invalid network mode",
			modify: func(c *Config) {
				c.Network.NetworkMode = "invalid"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			// Set valid base paths first
			cfg.Runtime.RuntimeDir = runtimeDir
			cfg.Image.RootDir = rootDir
			cfg.Runtime.FirecrackerBinary = binFile
			cfg.VM.KernelPath = kernelFile

			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyToLogger(t *testing.T) {
	log := logrus.New()
	cfg := Default()

	// Test Debug level
	cfg.Log.Level = "debug"
	cfg.ApplyToLogger(log)
	if log.Level != logrus.DebugLevel {
		t.Errorf("Logger level = %v, want DebugLevel", log.Level)
	}

	// Test JSON format
	cfg.Log.Format = "json"
	cfg.ApplyToLogger(log)
	if _, ok := log.Formatter.(*logrus.JSONFormatter); !ok {
		t.Errorf("Logger formatter is not JSONFormatter")
	}
}
