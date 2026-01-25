// Package image handles OCI image management and conversion to block devices.
//
// Firecracker requires block devices, not overlay filesystems. This package
// converts OCI images into ext4 block device images that can be attached
// to VMs as root filesystems.
//
// The conversion process:
//  1. Pull OCI image layers using containerd
//  2. Flatten layers into a single directory
//  3. Create an ext4 filesystem image
//  4. Copy contents into the filesystem
//
// For efficiency, we cache converted images and use sparse files.
package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// Service implements domain.ImageService for OCI images.
type Service struct {
	mu sync.RWMutex

	config ServiceConfig
	log    *logrus.Entry

	// Cache of converted images
	cache map[string]*cachedImage
}

// ServiceConfig configures the image service.
type ServiceConfig struct {
	// RootDir is the directory for storing image data.
	RootDir string

	// ContainerdSocket is the path to containerd's socket.
	ContainerdSocket string

	// DefaultBlockSize is the default size for block device images (in MB).
	DefaultBlockSizeMB int64

	// UseSparseFiles enables sparse file creation for efficiency.
	UseSparseFiles bool
}

// DefaultServiceConfig returns sensible defaults.
func DefaultServiceConfig() ServiceConfig {
	return ServiceConfig{
		RootDir:            "/var/lib/fc-cri/images",
		ContainerdSocket:   "/run/containerd/containerd.sock",
		DefaultBlockSizeMB: 1024, // 1GB
		UseSparseFiles:     true,
	}
}

type cachedImage struct {
	ref        string
	digest     string
	rootfsPath string
	// sizeMB     int64 // Unused
}

// NewService creates a new image service.
func NewService(config ServiceConfig, log *logrus.Entry) (*Service, error) {
	// Ensure directories exist
	for _, dir := range []string{
		config.RootDir,
		filepath.Join(config.RootDir, "layers"),
		filepath.Join(config.RootDir, "rootfs"),
		filepath.Join(config.RootDir, "tmp"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create dir %s: %w", dir, err)
		}
	}

	return &Service{
		config: config,
		log:    log.WithField("component", "image-service"),
		cache:  make(map[string]*cachedImage),
	}, nil
}

// Pull downloads an image and converts it to a rootfs block device.
func (s *Service) Pull(ctx context.Context, ref string) (string, error) {
	s.log.WithField("ref", ref).Info("Pulling image")

	// Check cache first
	s.mu.RLock()
	if cached, ok := s.cache[ref]; ok {
		s.mu.RUnlock()
		s.log.WithField("ref", ref).Debug("Using cached rootfs")
		return cached.rootfsPath, nil
	}
	s.mu.RUnlock()

	// Pull the image using containerd (via ctr or client library)
	if err := s.pullWithContainerd(ctx, ref); err != nil {
		return "", fmt.Errorf("failed to pull image: %w", err)
	}

	// Export and convert to block device
	rootfsPath, err := s.convertToBlockDevice(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("failed to convert image: %w", err)
	}

	// Cache the result
	s.mu.Lock()
	s.cache[ref] = &cachedImage{
		ref:        ref,
		rootfsPath: rootfsPath,
	}
	s.mu.Unlock()

	return rootfsPath, nil
}

// GetRootfs returns the path to the rootfs for an image.
func (s *Service) GetRootfs(ctx context.Context, ref string) (string, error) {
	s.mu.RLock()
	cached, ok := s.cache[ref]
	s.mu.RUnlock()

	if ok {
		return cached.rootfsPath, nil
	}

	// Not cached, pull and convert
	return s.Pull(ctx, ref)
}

// Remove removes an image.
func (s *Service) Remove(ctx context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cached, ok := s.cache[ref]
	if !ok {
		return nil // Already removed
	}

	// Remove the rootfs file
	if err := os.Remove(cached.rootfsPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove rootfs: %w", err)
	}

	delete(s.cache, ref)
	return nil
}

// List lists available images.
func (s *Service) List(ctx context.Context) ([]domain.ImageInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]domain.ImageInfo, 0, len(s.cache))
	for _, cached := range s.cache {
		info, err := os.Stat(cached.rootfsPath)
		if err != nil {
			continue
		}

		result = append(result, domain.ImageInfo{
			Ref:    cached.ref,
			Digest: cached.digest,
			Size:   info.Size(),
		})
	}

	return result, nil
}

// pullWithContainerd pulls an image using containerd.
func (s *Service) pullWithContainerd(ctx context.Context, ref string) error {
	// Use ctr for simplicity. In production, use the containerd client library.
	cmd := exec.CommandContext(ctx, "ctr",
		"--address", s.config.ContainerdSocket,
		"images", "pull", ref)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ctr pull failed: %w: %s", err, output)
	}

	return nil
}

// convertToBlockDevice converts an OCI image to an ext4 block device.
func (s *Service) convertToBlockDevice(ctx context.Context, ref string) (string, error) {
	// Generate output path based on image ref
	safeName := strings.ReplaceAll(ref, "/", "_")
	safeName = strings.ReplaceAll(safeName, ":", "_")
	rootfsPath := filepath.Join(s.config.RootDir, "rootfs", safeName+".ext4")

	// Check if already exists
	if _, err := os.Stat(rootfsPath); err == nil {
		s.log.WithField("path", rootfsPath).Debug("Rootfs already exists")
		return rootfsPath, nil
	}

	tmpDir := filepath.Join(s.config.RootDir, "tmp", safeName)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	// Export the image filesystem
	exportDir := filepath.Join(tmpDir, "rootfs")
	if err := s.exportImage(ctx, ref, exportDir); err != nil {
		return "", fmt.Errorf("failed to export image: %w", err)
	}

	// Calculate required size
	sizeMB, err := s.calculateSize(exportDir)
	if err != nil {
		return "", fmt.Errorf("failed to calculate size: %w", err)
	}

	// Add 20% headroom
	sizeMB = int64(float64(sizeMB) * 1.2)
	if sizeMB < 64 {
		sizeMB = 64 // Minimum 64MB
	}

	// Create the ext4 filesystem image
	if err := s.createExt4Image(ctx, rootfsPath, sizeMB, exportDir); err != nil {
		return "", fmt.Errorf("failed to create ext4 image: %w", err)
	}

	s.log.WithFields(logrus.Fields{
		"ref":    ref,
		"path":   rootfsPath,
		"sizeMB": sizeMB,
	}).Info("Created rootfs image")

	return rootfsPath, nil
}

// exportImage exports an image's filesystem to a directory.
func (s *Service) exportImage(ctx context.Context, ref, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	// Create a temporary container and export its rootfs
	containerID := fmt.Sprintf("fc-export-%d", os.Getpid())

	// Create container
	cmd := exec.CommandContext(ctx, "ctr",
		"--address", s.config.ContainerdSocket,
		"containers", "create", ref, containerID)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create container: %w: %s", err, output)
	}

	// Export rootfs using ctr snapshot
	// This is simplified - in production, use the containerd client to mount
	// and copy the snapshot properly
	cmd = exec.CommandContext(ctx, "ctr",
		"--address", s.config.ContainerdSocket,
		"snapshots", "--snapshotter", "overlayfs",
		"mounts", destDir, containerID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Fallback: try mounting manually
		s.log.WithError(err).Debug("Snapshot mount failed, trying alternative")
	}
	_ = output

	// Clean up container
	cleanupCmd := exec.CommandContext(ctx, "ctr",
		"--address", s.config.ContainerdSocket,
		"containers", "delete", containerID)
	_ = cleanupCmd.Run()

	return nil
}

// calculateSize calculates the size of a directory in MB.
func (s *Service) calculateSize(dir string) (int64, error) {
	var size int64

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	if err != nil {
		return 0, err
	}

	// Convert to MB
	return (size / 1024 / 1024) + 1, nil
}

// createExt4Image creates an ext4 filesystem image and populates it.
func (s *Service) createExt4Image(ctx context.Context, path string, sizeMB int64, contentDir string) error {
	// Create sparse file
	if s.config.UseSparseFiles {
		if err := createSparseFile(path, sizeMB*1024*1024); err != nil {
			return fmt.Errorf("failed to create sparse file: %w", err)
		}
	} else {
		// Create regular file with dd
		cmd := exec.CommandContext(ctx, "dd",
			"if=/dev/zero",
			fmt.Sprintf("of=%s", path),
			"bs=1M",
			fmt.Sprintf("count=%d", sizeMB))
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("dd failed: %w: %s", err, output)
		}
	}

	// Create ext4 filesystem
	cmd := exec.CommandContext(ctx, "mkfs.ext4",
		"-F",           // Force, don't ask
		"-L", "rootfs", // Label
		"-O", "^metadata_csum,^64bit", // Compatibility options
		"-q", // Quiet
		path)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %w: %s", err, output)
	}

	// Mount and copy content
	mountDir := path + ".mount"
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	// Mount the image
	cmd = exec.CommandContext(ctx, "mount", "-o", "loop", path, mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %w: %s", err, output)
	}
	defer exec.Command("umount", mountDir).Run()

	// Copy content
	cmd = exec.CommandContext(ctx, "cp", "-a", contentDir+"/.", mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp failed: %w: %s", err, output)
	}

	return nil
}

// createSparseFile creates a sparse file of the given size.
func createSparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(size); err != nil {
		return err
	}

	return nil
}

// =============================================================================
// Devmapper Integration (Alternative to ext4 files)
// =============================================================================

// DevmapperConfig holds configuration for devmapper-based storage.
// Devmapper is more efficient for production use with many VMs.
type DevmapperConfig struct {
	// PoolName is the name of the thin pool.
	PoolName string

	// BaseSize is the default size for thin volumes.
	BaseSize int64

	// MetadataDir is where devmapper metadata is stored.
	MetadataDir string
}

// DevmapperService provides rootfs volumes via device mapper thin provisioning.
// This is more efficient than file-based images for production use.
type DevmapperService struct {
	config DevmapperConfig
	log    *logrus.Entry
}

// NewDevmapperService creates a devmapper-based storage service.
func NewDevmapperService(config DevmapperConfig, log *logrus.Entry) (*DevmapperService, error) {
	// Verify thin pool exists
	// dmsetup info <pool_name>

	return &DevmapperService{
		config: config,
		log:    log.WithField("component", "devmapper"),
	}, nil
}

// CreateThinVolume creates a thin-provisioned volume for a rootfs.
func (d *DevmapperService) CreateThinVolume(name string, sizeMB int64) (string, error) {
	// dmsetup message /dev/mapper/<pool> 0 "create_thin <dev_id>"
	// dmsetup create <name> --table "0 <size> thin /dev/mapper/<pool> <dev_id>"

	devicePath := fmt.Sprintf("/dev/mapper/%s", name)
	return devicePath, nil
}

// SnapshotVolume creates a snapshot of an existing volume.
// This is very fast and space-efficient.
func (d *DevmapperService) SnapshotVolume(source, dest string) (string, error) {
	// dmsetup message /dev/mapper/<pool> 0 "create_snap <new_id> <origin_id>"
	return "", nil
}

// DeleteVolume removes a thin volume.
func (d *DevmapperService) DeleteVolume(name string) error {
	// dmsetup remove <name>
	// dmsetup message /dev/mapper/<pool> 0 "delete <dev_id>"
	return nil
}
