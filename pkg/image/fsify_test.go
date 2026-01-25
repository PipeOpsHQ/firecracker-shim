package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestNewFsifyConverter(t *testing.T) {
	tmpDir := t.TempDir()
	config := DefaultFsifyConfig()
	config.OutputDir = filepath.Join(tmpDir, "output")
	config.TempDir = filepath.Join(tmpDir, "temp")

	log := logrus.NewEntry(logrus.New())
	f, err := NewFsifyConverter(config, log)
	if err != nil {
		t.Fatalf("NewFsifyConverter failed: %v", err)
	}

	if f == nil {
		t.Fatal("Returned nil converter")
	}

	// Check directories created
	if _, err := os.Stat(config.OutputDir); os.IsNotExist(err) {
		t.Errorf("OutputDir not created")
	}
	if _, err := os.Stat(config.TempDir); os.IsNotExist(err) {
		t.Errorf("TempDir not created")
	}
}

func TestNormalizeRef(t *testing.T) {
	f := &FsifyConverter{}

	tests := []struct {
		input    string
		expected string
	}{
		{"nginx", "library/nginx:latest"},
		{"nginx:1.19", "library/nginx:1.19"},
		{"library/nginx", "library/nginx:latest"},
		{"quay.io/coreos/etcd", "quay.io/coreos/etcd:latest"},
		{"my.reg:5000/repo/img:tag", "my.reg:5000/repo/img:tag"},
		{"ubuntu@sha256:12345", "library/ubuntu@sha256:12345"},
	}

	for _, tt := range tests {
		result := f.normalizeRef(tt.input)
		if result != tt.expected {
			t.Errorf("normalizeRef(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	f := &FsifyConverter{}

	tests := []struct {
		input    string
		expected string
	}{
		{"library/nginx:latest", "nginx-latest"},
		{"docker.io/library/alpine:3.14", "alpine-3.14"},
		{"my-registry.com/image:v1", "my-registry.com-image-v1"},
	}

	for _, tt := range tests {
		result := f.sanitizeName(tt.input)
		if result != tt.expected {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestGetDigest(t *testing.T) {
	d1 := GetDigest("nginx:latest")
	d2 := GetDigest("nginx:latest")
	d3 := GetDigest("alpine:latest")

	if d1 != d2 {
		t.Errorf("GetDigest should be deterministic")
	}
	if d1 == d3 {
		t.Errorf("GetDigest collision")
	}
	if len(d1) != 12 {
		t.Errorf("GetDigest length = %d, want 12", len(d1))
	}
}

func TestCachePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	config := DefaultFsifyConfig()
	config.OutputDir = tmpDir

	log := logrus.NewEntry(logrus.New())
	f, err := NewFsifyConverter(config, log)
	if err != nil {
		t.Fatalf("NewFsifyConverter failed: %v", err)
	}

	// Manually inject an entry into cache
	ref := "library/nginx:latest"
	imgPath := filepath.Join(tmpDir, "nginx.img")

	// Create dummy image file so validation passes
	if err := os.WriteFile(imgPath, []byte("test data"), 0644); err != nil {
		t.Fatalf("Failed to create dummy image: %v", err)
	}

	f.cache[ref] = &ConvertedImage{
		Reference:   ref,
		Digest:      "sha256:1234",
		RootfsPath:  imgPath,
		SizeBytes:   1024,
		Filesystem:  "ext4",
		ConvertedAt: time.Now(),
	}

	// Save cache
	f.saveCache()

	// Verify cache file exists
	cacheFile := filepath.Join(tmpDir, "cache.json")
	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		t.Fatalf("Cache file not created")
	}

	// Create new converter to load cache
	f2, err := NewFsifyConverter(config, log)
	if err != nil {
		t.Fatalf("NewFsifyConverter failed: %v", err)
	}

	if _, ok := f2.cache[ref]; !ok {
		t.Errorf("Failed to load cached entry")
	}

	if f2.cache[ref].Digest != "sha256:1234" {
		t.Errorf("Loaded cache mismatch")
	}
}

func TestCalculateSize(t *testing.T) {
	tmpDir := t.TempDir()

	// Create 1MB file
	f1 := filepath.Join(tmpDir, "file1")
	data := make([]byte, 1024*1024)
	if err := os.WriteFile(f1, data, 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	f := &FsifyConverter{}
	size, err := f.calculateSize(tmpDir)
	if err != nil {
		t.Fatalf("calculateSize failed: %v", err)
	}

	// 1MB + directory overhead -> expecting at least 1MB
	if size < 1 {
		t.Errorf("Size too small: %d", size)
	}
}
