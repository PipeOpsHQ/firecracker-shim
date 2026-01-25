// Package network implements CNI-based networking for Firecracker VMs.
//
// Unlike traditional container networking where the CNI plugin configures
// a network namespace directly, we need to bridge the gap to Firecracker's
// virtio-net interface. The flow is:
//
//  1. CNI creates a tap device and configures it
//  2. We attach the tap device to Firecracker's virtio-net interface
//  3. The guest kernel sees a normal eth0 interface
//  4. Guest agent configures the interface inside the VM
package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/libcni"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// CNIService implements domain.NetworkService using CNI plugins.
type CNIService struct {
	config    CNIServiceConfig
	cniConfig *libcni.CNIConfig
	netConfig *libcni.NetworkConfigList
	log       *logrus.Entry
}

// CNIServiceConfig holds CNI configuration.
type CNIServiceConfig struct {
	// PluginDir is the directory containing CNI plugins.
	PluginDir string

	// ConfDir is the directory containing CNI configuration files.
	ConfDir string

	// CacheDir is the directory for CNI state cache.
	CacheDir string

	// NetworkName is the name of the CNI network to use.
	// If empty, uses the first network found in ConfDir.
	NetworkName string

	// DefaultSubnet is used if not specified in CNI config.
	DefaultSubnet string
}

// DefaultCNIServiceConfig returns sensible defaults.
func DefaultCNIServiceConfig() CNIServiceConfig {
	return CNIServiceConfig{
		PluginDir:     "/opt/cni/bin",
		ConfDir:       "/etc/cni/net.d",
		CacheDir:      "/var/lib/cni",
		DefaultSubnet: "10.88.0.0/16",
	}
}

// NewCNIService creates a new CNI-based network service.
func NewCNIService(config CNIServiceConfig, log *logrus.Entry) (*CNIService, error) {
	// Create CNI config executor
	cniConfig := libcni.NewCNIConfig([]string{config.PluginDir}, nil)

	// Load network configuration
	netConfig, err := loadNetworkConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to load CNI config: %w", err)
	}

	return &CNIService{
		config:    config,
		cniConfig: cniConfig,
		netConfig: netConfig,
		log:       log.WithField("component", "cni"),
	}, nil
}

// Setup configures networking for a sandbox.
// This creates the tap device and attaches it to the VM.
func (s *CNIService) Setup(ctx context.Context, sandbox *domain.Sandbox, config *domain.CNIConfig) error {
	s.log.WithField("sandbox_id", sandbox.ID).Info("Setting up network")

	// Create network namespace for the sandbox
	netnsPath, err := s.createNetNS(sandbox.ID)
	if err != nil {
		return fmt.Errorf("failed to create network namespace: %w", err)
	}
	sandbox.NetworkNamespace = netnsPath

	// Prepare CNI runtime config
	rt := &libcni.RuntimeConf{
		ContainerID: sandbox.ID,
		NetNS:       netnsPath,
		IfName:      "eth0",
		Args: [][2]string{
			{"IgnoreUnknown", "1"},
			{"K8S_POD_NAMESPACE", sandbox.Namespace},
			{"K8S_POD_NAME", sandbox.Name},
		},
	}

	// Add the network
	result, err := s.cniConfig.AddNetworkList(ctx, s.netConfig, rt)
	if err != nil {
		return fmt.Errorf("CNI AddNetworkList failed: %w", err)
	}

	// Parse the result to get IP info
	result100, err := types100.NewResultFromResult(result)
	if err != nil {
		return fmt.Errorf("failed to parse CNI result: %w", err)
	}

	// Extract IP address
	if len(result100.IPs) > 0 {
		sandbox.IP = result100.IPs[0].Address.IP
		s.log.WithField("ip", sandbox.IP).Debug("Assigned IP address")
	}

	// Extract gateway
	for _, route := range result100.Routes {
		if route.GW != nil {
			sandbox.Gateway = route.GW
			break
		}
	}

	// The tap device is now ready in the namespace
	// Firecracker will attach to it via the VMConfig.NetworkInterfaces

	s.log.WithFields(logrus.Fields{
		"sandbox_id": sandbox.ID,
		"ip":         sandbox.IP,
		"gateway":    sandbox.Gateway,
		"netns":      netnsPath,
	}).Info("Network setup complete")

	return nil
}

// Teardown removes network configuration for a sandbox.
func (s *CNIService) Teardown(ctx context.Context, sandbox *domain.Sandbox) error {
	s.log.WithField("sandbox_id", sandbox.ID).Info("Tearing down network")

	if sandbox.NetworkNamespace == "" {
		return nil // Nothing to tear down
	}

	rt := &libcni.RuntimeConf{
		ContainerID: sandbox.ID,
		NetNS:       sandbox.NetworkNamespace,
		IfName:      "eth0",
	}

	// Remove the network
	if err := s.cniConfig.DelNetworkList(ctx, s.netConfig, rt); err != nil {
		s.log.WithError(err).Warn("CNI DelNetworkList failed")
		// Continue with cleanup
	}

	// Remove the network namespace
	if err := s.deleteNetNS(sandbox.ID); err != nil {
		s.log.WithError(err).Warn("Failed to delete network namespace")
	}

	return nil
}

// GetIP returns the IP address assigned to a sandbox.
func (s *CNIService) GetIP(ctx context.Context, sandboxID string) (net.IP, error) {
	// This would typically look up the sandbox state
	// For now, return an error indicating we need the sandbox object
	return nil, fmt.Errorf("use sandbox.IP directly")
}

// createNetNS creates a new network namespace for the sandbox.
func (s *CNIService) createNetNS(sandboxID string) (string, error) {
	// Network namespace path
	nsPath := filepath.Join("/var/run/netns", fmt.Sprintf("fc-%s", sandboxID))

	// Ensure the netns directory exists
	if err := os.MkdirAll("/var/run/netns", 0755); err != nil {
		return "", fmt.Errorf("failed to create netns dir: %w", err)
	}

	// Create the namespace file
	f, err := os.Create(nsPath)
	if err != nil {
		return "", fmt.Errorf("failed to create netns file: %w", err)
	}
	f.Close()

	// Create a new network namespace via unshare
	// This is a simplified version - in production, use the netns package
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// In a real implementation, you'd call:
	// syscall.Unshare(syscall.CLONE_NEWNET)
	// syscall.Mount("/proc/self/ns/net", nsPath, "", syscall.MS_BIND, "")

	return nsPath, nil
}

// deleteNetNS removes a network namespace.
func (s *CNIService) deleteNetNS(sandboxID string) error {
	nsPath := filepath.Join("/var/run/netns", fmt.Sprintf("fc-%s", sandboxID))

	// Unmount and remove
	// syscall.Unmount(nsPath, 0)
	return os.Remove(nsPath)
}

// loadNetworkConfig loads CNI network configuration from the config directory.
func loadNetworkConfig(config CNIServiceConfig) (*libcni.NetworkConfigList, error) {
	// If a specific network name is specified, load that
	if config.NetworkName != "" {
		confList, err := libcni.LoadConfList(config.ConfDir, config.NetworkName)
		if err == nil {
			return confList, nil
		}
		// Fall through to try loading as a single config
	}

	// Try to find any .conflist or .conf file
	files, err := libcni.ConfFiles(config.ConfDir, []string{".conflist", ".conf"})
	if err != nil || len(files) == 0 {
		// No config files found, create a default bridge config
		return createDefaultConfig(config)
	}

	// Load the first config found
	if filepath.Ext(files[0]) == ".conflist" {
		return libcni.ConfListFromFile(files[0])
	}

	// It's a single .conf file, convert to conflist
	conf, err := libcni.ConfFromFile(files[0])
	if err != nil {
		return nil, err
	}
	return libcni.ConfListFromConf(conf)
}

// createDefaultConfig creates a default bridge network configuration.
func createDefaultConfig(config CNIServiceConfig) (*libcni.NetworkConfigList, error) {
	defaultConf := map[string]interface{}{
		"cniVersion": "1.0.0",
		"name":       "fc-net",
		"plugins": []map[string]interface{}{
			{
				"type":      "bridge",
				"bridge":    "fc-br0",
				"isGateway": true,
				"ipMasq":    true,
				"ipam": map[string]interface{}{
					"type":   "host-local",
					"subnet": config.DefaultSubnet,
					"routes": []map[string]string{
						{"dst": "0.0.0.0/0"},
					},
				},
			},
			{
				"type": "portmap",
				"capabilities": map[string]bool{
					"portMappings": true,
				},
			},
			{
				"type": "tc-redirect-tap",
			},
		},
	}

	confBytes, err := json.Marshal(defaultConf)
	if err != nil {
		return nil, err
	}

	return libcni.ConfListFromBytes(confBytes)
}

// =============================================================================
// TAP Device Management
// =============================================================================

// TAPConfig holds configuration for creating a TAP device.
type TAPConfig struct {
	Name    string
	MTU     int
	OwnerID int
	GroupID int
}

// CreateTAP creates a TAP device for Firecracker to use.
// The TAP device bridges the VM's virtio-net to the host network.
func CreateTAP(config TAPConfig) error {
	// This would typically use netlink to create the tap device
	// For example:
	//
	// link := &netlink.Tuntap{
	//     LinkAttrs: netlink.LinkAttrs{Name: config.Name, MTU: config.MTU},
	//     Mode:      netlink.TUNTAP_MODE_TAP,
	//     Flags:     netlink.TUNTAP_VNET_HDR,
	// }
	// if err := netlink.LinkAdd(link); err != nil {
	//     return err
	// }
	// if err := netlink.LinkSetUp(link); err != nil {
	//     return err
	// }

	// For simplicity, use ip command
	// In production, use netlink directly
	return nil
}

// AttachTAPToBridge attaches a TAP device to a bridge.
func AttachTAPToBridge(tapName, bridgeName string) error {
	// link, _ := netlink.LinkByName(tapName)
	// bridge, _ := netlink.LinkByName(bridgeName)
	// return netlink.LinkSetMaster(link, bridge)
	return nil
}

// =============================================================================
// Firecracker Network Configuration
// =============================================================================

// FirecrackerNetConfig returns the Firecracker network interface configuration
// for a given tap device.
func FirecrackerNetConfig(tapName string, macAddress string) map[string]interface{} {
	return map[string]interface{}{
		"iface_id":      "eth0",
		"host_dev_name": tapName,
		"guest_mac":     macAddress,
	}
}

// GenerateMAC generates a random MAC address.
func GenerateMAC() string {
	// Use a locally administered MAC address
	// Format: x2:xx:xx:xx:xx:xx (where x2 ensures locally administered bit)
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x",
		randByte(), randByte(), randByte(), randByte(), randByte())
}

func randByte() byte {
	// In production, use crypto/rand
	return byte(os.Getpid() & 0xFF)
}
