// Package image provides fsify integration for converting OCI images to block devices.
//
// fsify (github.com/volantvm/fsify) converts Docker/OCI container images into
// bootable filesystem images suitable for Firecracker microVMs. This integration
// provides both a CLI wrapper and native Go implementation of the core logic.
//
// The conversion process:
//  1. Pull OCI image using skopeo
//  2. Unpack layers using umoci
//  3. Calculate required disk size
//  4. Create filesystem image (ext4, xfs, or btrfs)
//  5. Mount and copy rootfs contents
//  6. Optionally create squashfs for caching
package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// FsifyConverter converts OCI images to Firecracker-compatible block devices.
type FsifyConverter struct {
	mu sync.RWMutex

	config FsifyConfig
	log    *logrus.Entry

	// Cache of converted images: imageRef -> ConvertedImage
	cache map[string]*ConvertedImage

	// In-progress conversions to prevent duplicate work
	inProgress map[string]chan struct{}
}

// FsifyConfig configures the fsify converter.
type FsifyConfig struct {
	// OutputDir is where converted images are stored.
	OutputDir string

	// TempDir is used for intermediate files during conversion.
	TempDir string

	// Filesystem type: "ext4", "xfs", or "btrfs"
	Filesystem string

	// SizeBufferMB is extra space (MB) added to images.
	SizeBufferMB int64

	// Preallocate disk space instead of sparse files.
	Preallocate bool

	// DualOutput generates both ext4 and squashfs images.
	DualOutput bool

	// UseFsifyCLI shells out to fsify binary if available.
	UseFsifyCLI bool

	// FsifyBinary is the path to fsify binary.
	FsifyBinary string

	// SkopeoPath is the path to skopeo binary.
	SkopeoPath string

	// UmociPath is the path to umoci binary.
	UmociPath string

	// DefaultRegistry is used when no registry is specified.
	DefaultRegistry string

	// InsecureRegistries allows HTTP for these registries.
	InsecureRegistries []string
}

// DefaultFsifyConfig returns sensible defaults.
func DefaultFsifyConfig() FsifyConfig {
	return FsifyConfig{
		OutputDir:       "/var/lib/fc-cri/images/rootfs",
		TempDir:         "/var/lib/fc-cri/images/tmp",
		Filesystem:      "ext4",
		SizeBufferMB:    50,
		Preallocate:     false,
		DualOutput:      false,
		UseFsifyCLI:     true,
		FsifyBinary:     "/usr/local/bin/fsify",
		SkopeoPath:      "/usr/bin/skopeo",
		UmociPath:       "/usr/bin/umoci",
		DefaultRegistry: "docker.io",
	}
}

// ConvertedImage represents a successfully converted image.
type ConvertedImage struct {
	// Reference is the original image reference (e.g., "nginx:latest")
	Reference string `json:"reference"`

	// Digest is the image digest
	Digest string `json:"digest"`

	// RootfsPath is the path to the ext4/xfs/btrfs block device image.
	RootfsPath string `json:"rootfs_path"`

	// SquashfsPath is the path to squashfs image (if DualOutput enabled).
	SquashfsPath string `json:"squashfs_path,omitempty"`

	// SizeBytes is the size of the rootfs image.
	SizeBytes int64 `json:"size_bytes"`

	// Filesystem type used.
	Filesystem string `json:"filesystem"`

	// OCIConfig contains the original OCI config (entrypoint, cmd, env, etc.)
	OCIConfig *OCIImageConfig `json:"oci_config,omitempty"`

	// ConvertedAt is when the conversion completed.
	ConvertedAt time.Time `json:"converted_at"`
}

// OCIImageConfig holds relevant OCI image configuration.
type OCIImageConfig struct {
	Entrypoint   []string            `json:"entrypoint,omitempty"`
	Cmd          []string            `json:"cmd,omitempty"`
	Env          []string            `json:"env,omitempty"`
	WorkingDir   string              `json:"working_dir,omitempty"`
	User         string              `json:"user,omitempty"`
	Labels       map[string]string   `json:"labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"exposed_ports,omitempty"`
}

// NewFsifyConverter creates a new fsify-based image converter.
func NewFsifyConverter(config FsifyConfig, log *logrus.Entry) (*FsifyConverter, error) {
	// Ensure directories exist
	for _, dir := range []string{config.OutputDir, config.TempDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Check for required binaries
	if config.UseFsifyCLI {
		if _, err := os.Stat(config.FsifyBinary); os.IsNotExist(err) {
			log.Warn("fsify binary not found, falling back to native implementation")
			config.UseFsifyCLI = false
		}
	}

	converter := &FsifyConverter{
		config:     config,
		log:        log.WithField("component", "fsify-converter"),
		cache:      make(map[string]*ConvertedImage),
		inProgress: make(map[string]chan struct{}),
	}

	// Load existing cache from disk
	converter.loadCache()

	return converter, nil
}

// Convert converts an OCI image to a block device image.
// Returns the path to the converted rootfs image.
func (f *FsifyConverter) Convert(ctx context.Context, imageRef string) (*ConvertedImage, error) {
	// Normalize the image reference
	normalizedRef := f.normalizeRef(imageRef)

	f.log.WithField("image", normalizedRef).Info("Converting image to rootfs")

	// Check cache first
	f.mu.RLock()
	if cached, ok := f.cache[normalizedRef]; ok {
		// Verify the file still exists
		if _, err := os.Stat(cached.RootfsPath); err == nil {
			f.mu.RUnlock()
			f.log.WithField("image", normalizedRef).Debug("Using cached rootfs")
			return cached, nil
		}
	}
	f.mu.RUnlock()

	// Check if conversion is already in progress
	f.mu.Lock()
	if progress, ok := f.inProgress[normalizedRef]; ok {
		f.mu.Unlock()
		// Wait for existing conversion
		select {
		case <-progress:
			// Conversion finished, check cache
			f.mu.RLock()
			cached := f.cache[normalizedRef]
			f.mu.RUnlock()
			if cached != nil {
				return cached, nil
			}
			return nil, fmt.Errorf("conversion failed")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Mark conversion as in-progress
	progress := make(chan struct{})
	f.inProgress[normalizedRef] = progress
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.inProgress, normalizedRef)
		close(progress)
		f.mu.Unlock()
	}()

	// Perform the conversion
	var result *ConvertedImage
	var err error

	if f.config.UseFsifyCLI {
		result, err = f.convertWithCLI(ctx, normalizedRef)
	} else {
		result, err = f.convertNative(ctx, normalizedRef)
	}

	if err != nil {
		return nil, err
	}

	// Cache the result
	f.mu.Lock()
	f.cache[normalizedRef] = result
	f.mu.Unlock()

	// Persist cache to disk
	f.saveCache()

	return result, nil
}

// convertWithCLI uses the fsify CLI tool for conversion.
func (f *FsifyConverter) convertWithCLI(ctx context.Context, imageRef string) (*ConvertedImage, error) {
	outputPath := f.getOutputPath(imageRef)

	args := []string{
		"-o", outputPath,
		"-fs", f.config.Filesystem,
		"-s", fmt.Sprintf("%d", f.config.SizeBufferMB),
	}

	if f.config.Preallocate {
		args = append(args, "--preallocate")
	}

	if f.config.DualOutput {
		args = append(args, "--dual-output")
	}

	args = append(args, imageRef)

	f.log.WithFields(logrus.Fields{
		"binary": f.config.FsifyBinary,
		"args":   args,
	}).Debug("Running fsify CLI")

	cmd := exec.CommandContext(ctx, f.config.FsifyBinary, args...)
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("fsify failed: %w: %s", err, output)
	}

	// Verify the output exists
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("fsify completed but output not found: %w", err)
	}

	result := &ConvertedImage{
		Reference:   imageRef,
		RootfsPath:  outputPath,
		SizeBytes:   info.Size(),
		Filesystem:  f.config.Filesystem,
		ConvertedAt: time.Now(),
	}

	// Check for squashfs output
	if f.config.DualOutput {
		squashfsPath := strings.TrimSuffix(outputPath, ".img") + ".squashfs"
		if _, err := os.Stat(squashfsPath); err == nil {
			result.SquashfsPath = squashfsPath
		}
	}

	// Try to extract OCI config
	result.OCIConfig = f.extractOCIConfig(outputPath)

	return result, nil
}

// convertNative implements the conversion logic natively in Go.
func (f *FsifyConverter) convertNative(ctx context.Context, imageRef string) (*ConvertedImage, error) {
	f.log.WithField("image", imageRef).Info("Converting image (native)")

	outputPath := f.getOutputPath(imageRef)
	tempDir := filepath.Join(f.config.TempDir, f.sanitizeName(imageRef))

	// Cleanup temp dir
	defer os.RemoveAll(tempDir)

	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Step 1: Pull image with skopeo
	ociDir := filepath.Join(tempDir, "oci")
	if err := f.pullImage(ctx, imageRef, ociDir); err != nil {
		return nil, fmt.Errorf("failed to pull image: %w", err)
	}

	// Step 2: Unpack with umoci
	rootfsDir := filepath.Join(tempDir, "rootfs")
	if err := f.unpackImage(ctx, ociDir, rootfsDir); err != nil {
		return nil, fmt.Errorf("failed to unpack image: %w", err)
	}

	// Step 3: Extract OCI config
	ociConfig := f.extractOCIConfigFromDir(ociDir)

	// Embed OCI config in rootfs
	if ociConfig != nil {
		_ = f.embedOCIConfig(rootfsDir, ociConfig)
	}

	// Step 4: Calculate required size
	sizeMB, err := f.calculateSize(rootfsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate size: %w", err)
	}
	sizeMB += f.config.SizeBufferMB

	// Step 5: Create filesystem image
	if err := f.createFilesystemImage(ctx, outputPath, sizeMB, rootfsDir); err != nil {
		return nil, fmt.Errorf("failed to create filesystem: %w", err)
	}

	// Get final size
	info, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat output: %w", err)
	}

	result := &ConvertedImage{
		Reference:   imageRef,
		RootfsPath:  outputPath,
		SizeBytes:   info.Size(),
		Filesystem:  f.config.Filesystem,
		OCIConfig:   ociConfig,
		ConvertedAt: time.Now(),
	}

	// Step 6: Create squashfs if dual output
	if f.config.DualOutput {
		squashfsPath := strings.TrimSuffix(outputPath, ".img") + ".squashfs"
		if err := f.createSquashfs(ctx, rootfsDir, squashfsPath); err != nil {
			f.log.WithError(err).Warn("Failed to create squashfs")
		} else {
			result.SquashfsPath = squashfsPath
		}
	}

	f.log.WithFields(logrus.Fields{
		"image":   imageRef,
		"output":  outputPath,
		"size_mb": sizeMB,
	}).Info("Image conversion complete")

	return result, nil
}

// pullImage pulls an OCI image using skopeo.
func (f *FsifyConverter) pullImage(ctx context.Context, imageRef, destDir string) error {
	// Normalize to docker:// format for skopeo
	srcRef := imageRef
	if !strings.Contains(srcRef, "://") {
		srcRef = "docker://" + srcRef
	}

	destRef := "oci:" + destDir + ":latest"

	args := []string{"copy", srcRef, destRef}

	// Check for insecure registry
	for _, insecure := range f.config.InsecureRegistries {
		if strings.Contains(imageRef, insecure) {
			args = append([]string{"--src-tls-verify=false"}, args...)
			break
		}
	}

	f.log.WithFields(logrus.Fields{
		"src":  srcRef,
		"dest": destRef,
	}).Debug("Pulling image with skopeo")

	cmd := exec.CommandContext(ctx, f.config.SkopeoPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("skopeo copy failed: %w: %s", err, output)
	}

	return nil
}

// unpackImage unpacks an OCI image using umoci.
func (f *FsifyConverter) unpackImage(ctx context.Context, ociDir, destDir string) error {
	args := []string{
		"unpack",
		"--image", ociDir + ":latest",
		destDir,
	}

	f.log.WithField("dest", destDir).Debug("Unpacking image with umoci")

	cmd := exec.CommandContext(ctx, f.config.UmociPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umoci unpack failed: %w: %s", err, output)
	}

	return nil
}

// calculateSize calculates the size of a directory in MB.
func (f *FsifyConverter) calculateSize(dir string) (int64, error) {
	// Use du for accuracy
	cmd := exec.Command("du", "-sm", dir)
	output, err := cmd.Output()
	if err != nil {
		// Fallback to manual calculation
		var size int64
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
		return (size / 1024 / 1024) + 1, nil
	}

	var sizeMB int64
	_, _ = fmt.Sscanf(string(output), "%d", &sizeMB)
	return sizeMB, nil
}

// createFilesystemImage creates the filesystem image.
func (f *FsifyConverter) createFilesystemImage(ctx context.Context, outputPath string, sizeMB int64, contentDir string) error {
	sizeBytes := sizeMB * 1024 * 1024

	// Create the image file
	if f.config.Preallocate {
		// Use fallocate for preallocation
		cmd := exec.CommandContext(ctx, "fallocate", "-l", fmt.Sprintf("%d", sizeBytes), outputPath)
		if err := cmd.Run(); err != nil {
			// Fallback to dd
			cmd = exec.CommandContext(ctx, "dd",
				"if=/dev/zero",
				"of="+outputPath,
				"bs=1M",
				fmt.Sprintf("count=%d", sizeMB))
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("dd failed: %w: %s", err, output)
			}
		}
	} else {
		// Sparse file
		file, err := os.Create(outputPath)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		if err := file.Truncate(sizeBytes); err != nil {
			file.Close()
			return fmt.Errorf("failed to truncate: %w", err)
		}
		file.Close()
	}

	// Create filesystem
	mkfsCmd := "mkfs." + f.config.Filesystem
	mkfsArgs := []string{"-F", "-L", "rootfs"}

	switch f.config.Filesystem {
	case "ext4":
		mkfsArgs = append(mkfsArgs, "-O", "^metadata_csum,^64bit", "-q")
	case "xfs":
		mkfsArgs = []string{"-L", "rootfs", "-f"}
	case "btrfs":
		mkfsArgs = []string{"-L", "rootfs", "-f"}
	}

	mkfsArgs = append(mkfsArgs, outputPath)

	cmd := exec.CommandContext(ctx, mkfsCmd, mkfsArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs failed: %w: %s", err, output)
	}

	// Mount and copy content
	mountDir := outputPath + ".mount"
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	// Mount
	cmd = exec.CommandContext(ctx, "mount", "-o", "loop", outputPath, mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %w: %s", err, output)
	}

	// Ensure unmount on exit
	defer func() {
		_ = exec.Command("umount", mountDir).Run()
	}()

	// The umoci unpack creates a bundle structure, rootfs is inside
	sourceDir := filepath.Join(contentDir, "rootfs")
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		sourceDir = contentDir // Fallback to direct content dir
	}

	// Copy content
	cmd = exec.CommandContext(ctx, "cp", "-a", sourceDir+"/.", mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp failed: %w: %s", err, output)
	}

	// Sync before unmount
	_ = exec.Command("sync").Run()

	return nil
}

// createSquashfs creates a squashfs image for caching.
func (f *FsifyConverter) createSquashfs(ctx context.Context, contentDir, outputPath string) error {
	sourceDir := filepath.Join(contentDir, "rootfs")
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		sourceDir = contentDir
	}

	cmd := exec.CommandContext(ctx, "mksquashfs",
		sourceDir, outputPath,
		"-comp", "zstd",
		"-Xcompression-level", "19",
		"-noappend")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mksquashfs failed: %w: %s", err, output)
	}

	return nil
}

// extractOCIConfig reads OCI config from /etc/fsify-entrypoint in a mounted image.
func (f *FsifyConverter) extractOCIConfig(imagePath string) *OCIImageConfig {
	// Mount the image temporarily
	mountDir := imagePath + ".extract"
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return nil
	}
	defer os.RemoveAll(mountDir)

	cmd := exec.Command("mount", "-o", "loop,ro", imagePath, mountDir)
	if err := cmd.Run(); err != nil {
		return nil
	}
	defer func() { _ = exec.Command("umount", mountDir).Run() }()

	configPath := filepath.Join(mountDir, "etc", "fsify-entrypoint")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}

	var config OCIImageConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil
	}

	return &config
}

// extractOCIConfigFromDir extracts OCI config from an OCI directory.
func (f *FsifyConverter) extractOCIConfigFromDir(ociDir string) *OCIImageConfig {
	// Read the index.json to find the manifest
	indexPath := filepath.Join(ociDir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return nil
	}

	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil || len(index.Manifests) == 0 {
		return nil
	}

	// Parse digest to get blob path
	manifestDigest := index.Manifests[0].Digest
	parts := strings.SplitN(manifestDigest, ":", 2)
	if len(parts) != 2 {
		return nil
	}

	manifestPath := filepath.Join(ociDir, "blobs", parts[0], parts[1])
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}

	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil
	}

	// Parse config blob
	parts = strings.SplitN(manifest.Config.Digest, ":", 2)
	if len(parts) != 2 {
		return nil
	}

	configBlobPath := filepath.Join(ociDir, "blobs", parts[0], parts[1])
	configData, err := os.ReadFile(configBlobPath)
	if err != nil {
		return nil
	}

	var ociConfig struct {
		Config struct {
			Entrypoint   []string            `json:"Entrypoint"`
			Cmd          []string            `json:"Cmd"`
			Env          []string            `json:"Env"`
			WorkingDir   string              `json:"WorkingDir"`
			User         string              `json:"User"`
			Labels       map[string]string   `json:"Labels"`
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configData, &ociConfig); err != nil {
		return nil
	}

	return &OCIImageConfig{
		Entrypoint:   ociConfig.Config.Entrypoint,
		Cmd:          ociConfig.Config.Cmd,
		Env:          ociConfig.Config.Env,
		WorkingDir:   ociConfig.Config.WorkingDir,
		User:         ociConfig.Config.User,
		Labels:       ociConfig.Config.Labels,
		ExposedPorts: ociConfig.Config.ExposedPorts,
	}
}

// embedOCIConfig writes OCI config to /etc/fsify-entrypoint in the rootfs.
func (f *FsifyConverter) embedOCIConfig(rootfsDir string, config *OCIImageConfig) error {
	targetDir := filepath.Join(rootfsDir, "rootfs", "etc")
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		targetDir = filepath.Join(rootfsDir, "etc")
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	configPath := filepath.Join(targetDir, "fsify-entrypoint")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}

// getOutputPath generates the output path for an image.
func (f *FsifyConverter) getOutputPath(imageRef string) string {
	safeName := f.sanitizeName(imageRef)
	return filepath.Join(f.config.OutputDir, safeName+".img")
}

// sanitizeName converts an image reference to a safe filename.
func (f *FsifyConverter) sanitizeName(imageRef string) string {
	// Remove registry prefix for common registries
	name := imageRef
	for _, prefix := range []string{"docker.io/", "library/"} {
		name = strings.TrimPrefix(name, prefix)
	}

	// Replace unsafe characters
	replacer := strings.NewReplacer(
		"/", "-",
		":", "-",
		"@", "-",
	)
	return replacer.Replace(name)
}

// normalizeRef normalizes an image reference.
func (f *FsifyConverter) normalizeRef(imageRef string) string {
	// Add default tag if missing
	if !strings.Contains(imageRef, ":") && !strings.Contains(imageRef, "@") {
		imageRef = imageRef + ":latest"
	}

	// Add default registry for library images
	if !strings.Contains(imageRef, "/") {
		imageRef = "library/" + imageRef
	}

	return imageRef
}

// Delete removes a converted image from cache and disk.
func (f *FsifyConverter) Delete(imageRef string) error {
	normalizedRef := f.normalizeRef(imageRef)

	f.mu.Lock()
	defer f.mu.Unlock()

	cached, ok := f.cache[normalizedRef]
	if !ok {
		return nil
	}

	// Remove files
	os.Remove(cached.RootfsPath)
	if cached.SquashfsPath != "" {
		os.Remove(cached.SquashfsPath)
	}

	delete(f.cache, normalizedRef)
	f.saveCache()

	return nil
}

// List returns all cached images.
func (f *FsifyConverter) List() []*ConvertedImage {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make([]*ConvertedImage, 0, len(f.cache))
	for _, img := range f.cache {
		result = append(result, img)
	}
	return result
}

// cacheFilePath returns the path to the cache index file.
func (f *FsifyConverter) cacheFilePath() string {
	return filepath.Join(f.config.OutputDir, "cache.json")
}

// loadCache loads the cache from disk.
func (f *FsifyConverter) loadCache() {
	data, err := os.ReadFile(f.cacheFilePath())
	if err != nil {
		return
	}

	var cache map[string]*ConvertedImage
	if err := json.Unmarshal(data, &cache); err != nil {
		return
	}

	// Validate each entry still exists
	for ref, img := range cache {
		if _, err := os.Stat(img.RootfsPath); err == nil {
			f.cache[ref] = img
		}
	}
}

// saveCache persists the cache to disk.
func (f *FsifyConverter) saveCache() {
	data, err := json.MarshalIndent(f.cache, "", "  ")
	if err != nil {
		f.log.WithError(err).Warn("Failed to marshal cache")
		return
	}

	if err := os.WriteFile(f.cacheFilePath(), data, 0644); err != nil {
		f.log.WithError(err).Warn("Failed to write cache")
	}
}

// GetDigest returns a hash of the image reference for deduplication.
func GetDigest(imageRef string) string {
	h := sha256.New()
	h.Write([]byte(imageRef))
	return hex.EncodeToString(h.Sum(nil))[:12]
}
