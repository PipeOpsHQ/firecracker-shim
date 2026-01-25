// fcctl is the debug and inspection CLI for the Firecracker CRI runtime.
//
// It provides commands to:
// - List and inspect running VMs/sandboxes
// - Check pool status and health
// - Debug vsock connections
// - View metrics and latencies
// - Troubleshoot stuck pods
//
// Usage:
//
//	fcctl list                    # List all sandboxes
//	fcctl inspect <sandbox-id>    # Show sandbox details
//	fcctl pool status             # Show VM pool status
//	fcctl metrics                 # Show runtime metrics
//	fcctl logs <sandbox-id>       # Stream sandbox logs
//	fcctl exec <sandbox-id> <cmd> # Execute command in VM
//	fcctl health                  # Check runtime health
//
// Build: go build -o fcctl ./cmd/fcctl
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	version        = "0.1.0"
	defaultRunDir  = "/run/fc-cri"
	metricsAddress = "http://localhost:9090/metrics"
)

// CLI holds the CLI state
type CLI struct {
	runDir         string
	metricsAddress string
	verbose        bool
	output         string // "table", "json", "wide"
}

func main() {
	cli := &CLI{
		runDir:         getEnvOrDefault("FC_CRI_RUN_DIR", defaultRunDir),
		metricsAddress: getEnvOrDefault("FC_CRI_METRICS_ADDRESS", metricsAddress),
		output:         "table",
	}

	if len(os.Args) < 2 {
		cli.printUsage()
		os.Exit(1)
	}

	// Parse global flags
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-v", "--verbose":
			cli.verbose = true
			args = args[1:]
		case "-o", "--output":
			if len(args) < 2 {
				fatal("--output requires a value")
			}
			cli.output = args[1]
			args = args[2:]
		case "--run-dir":
			if len(args) < 2 {
				fatal("--run-dir requires a value")
			}
			cli.runDir = args[1]
			args = args[2:]
		case "-h", "--help":
			cli.printUsage()
			os.Exit(0)
		case "--version":
			fmt.Printf("fcctl version %s\n", version)
			os.Exit(0)
		default:
			fatal("unknown flag: %s", args[0])
		}
	}

	if len(args) == 0 {
		cli.printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	var err error
	switch cmd {
	case "list", "ls":
		err = cli.cmdList(ctx, cmdArgs)
	case "inspect", "get":
		err = cli.cmdInspect(ctx, cmdArgs)
	case "pool":
		err = cli.cmdPool(ctx, cmdArgs)
	case "metrics":
		err = cli.cmdMetrics(ctx, cmdArgs)
	case "logs":
		err = cli.cmdLogs(ctx, cmdArgs)
	case "exec":
		err = cli.cmdExec(ctx, cmdArgs)
	case "health":
		err = cli.cmdHealth(ctx, cmdArgs)
	case "kill":
		err = cli.cmdKill(ctx, cmdArgs)
	case "cleanup":
		err = cli.cmdCleanup(ctx, cmdArgs)
	case "version":
		fmt.Printf("fcctl version %s\n", version)
	case "help":
		cli.printUsage()
	default:
		fatal("unknown command: %s", cmd)
	}

	if err != nil {
		fatal("%v", err)
	}
}

func (cli *CLI) printUsage() {
	fmt.Println(`fcctl - Firecracker CRI Runtime Debug Tool

Usage:
  fcctl [flags] <command> [args]

Commands:
  list, ls              List all sandboxes/VMs
  inspect <id>          Show detailed sandbox information
  pool [status|warm|drain]  Manage VM pool
  metrics               Show runtime metrics
  logs <id> [-f]        Show/stream sandbox logs
  exec <id> <cmd>       Execute command in VM via agent
  health                Check runtime health
  kill <id>             Force kill a sandbox VM
  cleanup               Clean up orphaned resources
  version               Show version
  help                  Show this help

Flags:
  -v, --verbose         Enable verbose output
  -o, --output <fmt>    Output format: table, json, wide (default: table)
  --run-dir <path>      Runtime directory (default: /run/fc-cri)
  -h, --help            Show help
  --version             Show version

Environment:
  FC_CRI_RUN_DIR        Runtime directory
  FC_CRI_METRICS_ADDRESS Metrics endpoint address

Examples:
  fcctl list
  fcctl inspect fc-1234567890
  fcctl pool status
  fcctl metrics
  fcctl logs fc-1234567890 -f
  fcctl exec fc-1234567890 cat /etc/os-release
  fcctl health
  fcctl cleanup --dry-run
`)
}

// =============================================================================
// List Command
// =============================================================================

type SandboxInfo struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
	VCPUs     int       `json:"vcpus"`
	MemoryMB  int       `json:"memory_mb"`
	IP        string    `json:"ip,omitempty"`
	Uptime    string    `json:"uptime"`
	SocketOK  bool      `json:"socket_ok"`
}

func (cli *CLI) cmdList(ctx context.Context, args []string) error {
	sandboxes, err := cli.discoverSandboxes()
	if err != nil {
		return fmt.Errorf("failed to discover sandboxes: %w", err)
	}

	if cli.output == "json" {
		return json.NewEncoder(os.Stdout).Encode(sandboxes)
	}

	if len(sandboxes) == 0 {
		fmt.Println("No sandboxes found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if cli.output == "wide" {
		fmt.Fprintln(w, "ID\tSTATE\tPID\tVCPUs\tMEMORY\tIP\tUPTIME\tSOCKET")
	} else {
		fmt.Fprintln(w, "ID\tSTATE\tPID\tUPTIME\tSOCKET")
	}

	for _, sb := range sandboxes {
		socketStatus := "✗"
		if sb.SocketOK {
			socketStatus = "✓"
		}

		if cli.output == "wide" {
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%dMB\t%s\t%s\t%s\n",
				sb.ID, sb.State, sb.PID, sb.VCPUs, sb.MemoryMB, sb.IP, sb.Uptime, socketStatus)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
				sb.ID, sb.State, sb.PID, sb.Uptime, socketStatus)
		}
	}
	w.Flush()

	fmt.Printf("\nTotal: %d sandbox(es)\n", len(sandboxes))
	return nil
}

func (cli *CLI) discoverSandboxes() ([]SandboxInfo, error) {
	entries, err := os.ReadDir(cli.runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sandboxes []SandboxInfo
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "fc-") {
			continue
		}

		sb := cli.getSandboxInfo(entry.Name())
		sandboxes = append(sandboxes, sb)
	}

	// Sort by creation time
	sort.Slice(sandboxes, func(i, j int) bool {
		return sandboxes[i].CreatedAt.After(sandboxes[j].CreatedAt)
	})

	return sandboxes, nil
}

func (cli *CLI) getSandboxInfo(id string) SandboxInfo {
	sandboxDir := filepath.Join(cli.runDir, id)
	socketPath := filepath.Join(sandboxDir, "firecracker.sock")

	info := SandboxInfo{
		ID:    id,
		State: "unknown",
	}

	// Check socket exists
	if _, err := os.Stat(socketPath); err == nil {
		info.SocketOK = true
	}

	// Try to get state from Firecracker API
	if info.SocketOK {
		if state, err := cli.getVMState(socketPath); err == nil {
			info.State = state.State
			info.VCPUs = state.VCPUs
			info.MemoryMB = state.MemoryMB
		}
	}

	// Get PID from pid file or process lookup
	pidFile := filepath.Join(sandboxDir, "firecracker.pid")
	if data, err := os.ReadFile(pidFile); err == nil {
		fmt.Sscanf(string(data), "%d", &info.PID)
	}

	// Check if process is running
	if info.PID > 0 {
		if process, err := os.FindProcess(info.PID); err == nil {
			if err := process.Signal(syscall.Signal(0)); err == nil {
				if info.State == "unknown" {
					info.State = "running"
				}
			} else {
				info.State = "dead"
			}
		}
	}

	// Get directory creation time for uptime
	if stat, err := os.Stat(sandboxDir); err == nil {
		info.CreatedAt = stat.ModTime()
		info.Uptime = formatDuration(time.Since(info.CreatedAt))
	}

	return info
}

type VMState struct {
	State    string `json:"state"`
	VCPUs    int    `json:"vcpu_count"`
	MemoryMB int    `json:"mem_size_mib"`
}

func (cli *CLI) getVMState(socketPath string) (*VMState, error) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get("http://localhost/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var state VMState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, err
	}

	return &state, nil
}

// =============================================================================
// Inspect Command
// =============================================================================

type DetailedSandboxInfo struct {
	SandboxInfo
	SocketPath string            `json:"socket_path"`
	VsockPath  string            `json:"vsock_path"`
	VsockCID   uint32            `json:"vsock_cid"`
	Drives     []DriveInfo       `json:"drives,omitempty"`
	Network    *NetworkInfo      `json:"network,omitempty"`
	Agent      *AgentInfo        `json:"agent,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type DriveInfo struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
	IsRoot   bool   `json:"is_root"`
}

type NetworkInfo struct {
	IP        string `json:"ip"`
	Gateway   string `json:"gateway"`
	Interface string `json:"interface"`
	Namespace string `json:"namespace"`
}

type AgentInfo struct {
	Connected bool   `json:"connected"`
	Version   string `json:"version,omitempty"`
	Latency   string `json:"latency,omitempty"`
}

func (cli *CLI) cmdInspect(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fcctl inspect <sandbox-id>")
	}

	id := args[0]
	sandboxDir := filepath.Join(cli.runDir, id)

	if _, err := os.Stat(sandboxDir); os.IsNotExist(err) {
		return fmt.Errorf("sandbox not found: %s", id)
	}

	info := DetailedSandboxInfo{
		SandboxInfo: cli.getSandboxInfo(id),
		SocketPath:  filepath.Join(sandboxDir, "firecracker.sock"),
		VsockPath:   filepath.Join(sandboxDir, "vsock.sock"),
	}

	// Read metadata if exists
	metaPath := filepath.Join(sandboxDir, "metadata.json")
	if data, err := os.ReadFile(metaPath); err == nil {
		json.Unmarshal(data, &info.Metadata)
	}

	// Test agent connection
	info.Agent = cli.testAgentConnection(info.VsockPath)

	if cli.output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	// Table output
	fmt.Println("=== Sandbox Information ===")
	fmt.Printf("ID:          %s\n", info.ID)
	fmt.Printf("State:       %s\n", info.State)
	fmt.Printf("PID:         %d\n", info.PID)
	fmt.Printf("Uptime:      %s\n", info.Uptime)
	fmt.Printf("vCPUs:       %d\n", info.VCPUs)
	fmt.Printf("Memory:      %d MB\n", info.MemoryMB)
	fmt.Println()

	fmt.Println("=== Paths ===")
	fmt.Printf("Socket:      %s (%s)\n", info.SocketPath, boolToStatus(info.SocketOK))
	fmt.Printf("Vsock:       %s\n", info.VsockPath)
	fmt.Println()

	if info.Agent != nil {
		fmt.Println("=== Agent ===")
		fmt.Printf("Connected:   %s\n", boolToStatus(info.Agent.Connected))
		if info.Agent.Latency != "" {
			fmt.Printf("Latency:     %s\n", info.Agent.Latency)
		}
	}

	if info.Network != nil {
		fmt.Println()
		fmt.Println("=== Network ===")
		fmt.Printf("IP:          %s\n", info.Network.IP)
		fmt.Printf("Gateway:     %s\n", info.Network.Gateway)
		fmt.Printf("Interface:   %s\n", info.Network.Interface)
	}

	return nil
}

func (cli *CLI) testAgentConnection(vsockPath string) *AgentInfo {
	info := &AgentInfo{Connected: false}

	// Try to connect to vsock and send ping
	conn, err := net.DialTimeout("unix", vsockPath, 2*time.Second)
	if err != nil {
		return info
	}
	defer conn.Close()

	start := time.Now()

	// Send ping request
	req := map[string]interface{}{
		"id":     1,
		"method": "ping",
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return info
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var resp map[string]interface{}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return info
	}

	info.Connected = true
	info.Latency = time.Since(start).String()

	return info
}

// =============================================================================
// Pool Command
// =============================================================================

type PoolStatus struct {
	Available   int     `json:"available"`
	InUse       int     `json:"in_use"`
	MaxSize     int     `json:"max_size"`
	TotalServed int64   `json:"total_served"`
	HitRate     float64 `json:"hit_rate"`
	PoolHits    int64   `json:"pool_hits"`
	PoolMisses  int64   `json:"pool_misses"`
}

func (cli *CLI) cmdPool(ctx context.Context, args []string) error {
	subCmd := "status"
	if len(args) > 0 {
		subCmd = args[0]
	}

	switch subCmd {
	case "status":
		return cli.cmdPoolStatus(ctx)
	case "warm":
		return cli.cmdPoolWarm(ctx, args[1:])
	case "drain":
		return cli.cmdPoolDrain(ctx)
	default:
		return fmt.Errorf("unknown pool command: %s", subCmd)
	}
}

func (cli *CLI) cmdPoolStatus(ctx context.Context) error {
	// Try to get pool stats from metrics endpoint
	resp, err := http.Get(cli.metricsAddress)
	if err != nil {
		return fmt.Errorf("cannot connect to metrics endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	metrics := string(body)

	status := PoolStatus{}

	// Parse Prometheus metrics
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "fc_cri_pool_available ") {
			fmt.Sscanf(line, "fc_cri_pool_available %d", &status.Available)
		} else if strings.HasPrefix(line, "fc_cri_pool_in_use ") {
			fmt.Sscanf(line, "fc_cri_pool_in_use %d", &status.InUse)
		} else if strings.HasPrefix(line, "fc_cri_pool_max_size ") {
			fmt.Sscanf(line, "fc_cri_pool_max_size %d", &status.MaxSize)
		} else if strings.HasPrefix(line, "fc_cri_pool_hits_total ") {
			fmt.Sscanf(line, "fc_cri_pool_hits_total %d", &status.PoolHits)
		} else if strings.HasPrefix(line, "fc_cri_pool_misses_total ") {
			fmt.Sscanf(line, "fc_cri_pool_misses_total %d", &status.PoolMisses)
		} else if strings.HasPrefix(line, "fc_cri_pool_hit_rate ") {
			fmt.Sscanf(line, "fc_cri_pool_hit_rate %f", &status.HitRate)
		}
	}

	if cli.output == "json" {
		return json.NewEncoder(os.Stdout).Encode(status)
	}

	fmt.Println("=== VM Pool Status ===")
	fmt.Printf("Available:    %d\n", status.Available)
	fmt.Printf("In Use:       %d\n", status.InUse)
	fmt.Printf("Max Size:     %d\n", status.MaxSize)
	fmt.Printf("Hit Rate:     %.1f%%\n", status.HitRate)
	fmt.Printf("Pool Hits:    %d\n", status.PoolHits)
	fmt.Printf("Pool Misses:  %d\n", status.PoolMisses)

	// Visual bar
	if status.MaxSize > 0 {
		fmt.Println()
		usedPct := float64(status.InUse) / float64(status.MaxSize) * 100
		availPct := float64(status.Available) / float64(status.MaxSize) * 100
		fmt.Printf("Pool: [%s%s%s] %d/%d\n",
			strings.Repeat("█", int(usedPct/5)),
			strings.Repeat("░", int(availPct/5)),
			strings.Repeat(" ", 20-int(usedPct/5)-int(availPct/5)),
			status.InUse+status.Available, status.MaxSize)
		fmt.Printf("       %s In Use  %s Available  %s Empty\n", "█", "░", " ")
	}

	return nil
}

func (cli *CLI) cmdPoolWarm(ctx context.Context, args []string) error {
	count := 1
	if len(args) > 0 {
		var err error
		count, err = strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid count: %s", args[0])
		}
	}

	fmt.Printf("Warming pool with %d VM(s)...\n", count)
	fmt.Println("Note: This requires the runtime to be running and is not yet implemented in fcctl.")
	fmt.Println("Use the runtime's pool configuration to manage warming.")

	return nil
}

func (cli *CLI) cmdPoolDrain(ctx context.Context) error {
	fmt.Println("Draining pool...")
	fmt.Println("Note: This requires the runtime to be running and is not yet implemented in fcctl.")
	return nil
}

// =============================================================================
// Metrics Command
// =============================================================================

func (cli *CLI) cmdMetrics(ctx context.Context, args []string) error {
	resp, err := http.Get(cli.metricsAddress)
	if err != nil {
		return fmt.Errorf("cannot connect to metrics endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if cli.output == "json" {
		// Convert Prometheus format to JSON
		metrics := parsePrometheusMetrics(string(body))
		return json.NewEncoder(os.Stdout).Encode(metrics)
	}

	// Pretty print key metrics
	metrics := string(body)

	fmt.Println("=== Firecracker CRI Metrics ===")
	fmt.Println()

	// Extract and display key metrics
	sections := []struct {
		title   string
		metrics []string
	}{
		{
			title: "VM Pool",
			metrics: []string{
				"fc_cri_pool_available",
				"fc_cri_pool_in_use",
				"fc_cri_pool_hit_rate",
			},
		},
		{
			title: "Latencies (ms)",
			metrics: []string{
				"fc_cri_create_latency_p50_ms",
				"fc_cri_create_latency_p95_ms",
				"fc_cri_create_latency_p99_ms",
				"fc_cri_start_latency_p50_ms",
				"fc_cri_start_latency_p95_ms",
			},
		},
		{
			title: "Counters",
			metrics: []string{
				"fc_cri_vms_created_total",
				"fc_cri_vms_destroyed_total",
				"fc_cri_containers_total",
				"fc_cri_containers_active",
			},
		},
		{
			title: "Resources",
			metrics: []string{
				"fc_cri_total_memory_mb",
				"fc_cri_total_vcpus",
			},
		},
		{
			title: "Errors",
			metrics: []string{
				"fc_cri_vm_create_errors_total",
				"fc_cri_container_errors_total",
				"fc_cri_agent_connect_errors_total",
			},
		},
	}

	for _, section := range sections {
		fmt.Printf("--- %s ---\n", section.title)
		for _, metricName := range section.metrics {
			value := extractMetricValue(metrics, metricName)
			displayName := strings.TrimPrefix(metricName, "fc_cri_")
			displayName = strings.ReplaceAll(displayName, "_", " ")
			fmt.Printf("  %-30s %s\n", displayName+":", value)
		}
		fmt.Println()
	}

	return nil
}

func parsePrometheusMetrics(body string) map[string]interface{} {
	result := make(map[string]interface{})
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

func extractMetricValue(metrics, name string) string {
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, name+" ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	return "N/A"
}

// =============================================================================
// Logs Command
// =============================================================================

func (cli *CLI) cmdLogs(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fcctl logs <sandbox-id> [-f]")
	}

	id := args[0]
	follow := false
	if len(args) > 1 && (args[1] == "-f" || args[1] == "--follow") {
		follow = true
	}

	sandboxDir := filepath.Join(cli.runDir, id)
	logFile := filepath.Join(sandboxDir, "firecracker.log")

	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		// Try alternate log location
		logFile = filepath.Join(sandboxDir, "vmm.log")
		if _, err := os.Stat(logFile); os.IsNotExist(err) {
			return fmt.Errorf("no log file found for sandbox %s", id)
		}
	}

	if follow {
		return cli.tailFile(ctx, logFile)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		return fmt.Errorf("failed to read log file: %w", err)
	}

	fmt.Print(string(data))
	return nil
}

func (cli *CLI) tailFile(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	// Seek to end
	file.Seek(0, io.SeekEnd)

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n, err := file.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err == io.EOF {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

// =============================================================================
// Exec Command
// =============================================================================

func (cli *CLI) cmdExec(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: fcctl exec <sandbox-id> <command> [args...]")
	}

	id := args[0]
	cmd := args[1:]

	sandboxDir := filepath.Join(cli.runDir, id)
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")

	if _, err := os.Stat(vsockPath); os.IsNotExist(err) {
		return fmt.Errorf("vsock not found for sandbox %s", id)
	}

	conn, err := net.DialTimeout("unix", vsockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to agent: %w", err)
	}
	defer conn.Close()

	// Send exec_sync request
	req := map[string]interface{}{
		"id":     1,
		"method": "exec_sync",
		"params": map[string]interface{}{
			"id":      "fcctl-exec",
			"cmd":     cmd,
			"timeout": 30,
		},
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(35 * time.Second))
	var resp struct {
		Result struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("agent error: %s", resp.Error.Message)
	}

	if resp.Result.Stdout != "" {
		fmt.Print(resp.Result.Stdout)
	}
	if resp.Result.Stderr != "" {
		fmt.Fprint(os.Stderr, resp.Result.Stderr)
	}

	if resp.Result.ExitCode != 0 {
		os.Exit(resp.Result.ExitCode)
	}

	return nil
}

// =============================================================================
// Health Command
// =============================================================================

type HealthStatus struct {
	Healthy    bool              `json:"healthy"`
	Components map[string]string `json:"components"`
	Issues     []string          `json:"issues,omitempty"`
	CheckedAt  time.Time         `json:"checked_at"`
}

func (cli *CLI) cmdHealth(ctx context.Context, args []string) error {
	status := HealthStatus{
		Healthy:    true,
		Components: make(map[string]string),
		CheckedAt:  time.Now(),
	}

	// Check runtime directory
	if _, err := os.Stat(cli.runDir); err != nil {
		status.Components["runtime_dir"] = "missing"
		status.Issues = append(status.Issues, fmt.Sprintf("Runtime directory missing: %s", cli.runDir))
		status.Healthy = false
	} else {
		status.Components["runtime_dir"] = "ok"
	}

	// Check /dev/kvm
	if _, err := os.Stat("/dev/kvm"); err != nil {
		status.Components["kvm"] = "missing"
		status.Issues = append(status.Issues, "/dev/kvm not available")
		status.Healthy = false
	} else {
		status.Components["kvm"] = "ok"
	}

	// Check firecracker binary
	if _, err := os.Stat("/usr/bin/firecracker"); err != nil {
		status.Components["firecracker"] = "missing"
		status.Issues = append(status.Issues, "firecracker binary not found")
		status.Healthy = false
	} else {
		status.Components["firecracker"] = "ok"
	}

	// Check metrics endpoint
	resp, err := http.Get(cli.metricsAddress)
	if err != nil {
		status.Components["metrics"] = "unavailable"
		status.Issues = append(status.Issues, "Metrics endpoint not responding")
	} else {
		resp.Body.Close()
		status.Components["metrics"] = "ok"
	}

	// Check kernel
	if _, err := os.Stat("/var/lib/fc-cri/vmlinux"); err != nil {
		status.Components["kernel"] = "missing"
		status.Issues = append(status.Issues, "Kernel not found at /var/lib/fc-cri/vmlinux")
		status.Healthy = false
	} else {
		status.Components["kernel"] = "ok"
	}

	// Check base rootfs
	if _, err := os.Stat("/var/lib/fc-cri/rootfs/base.ext4"); err != nil {
		status.Components["rootfs"] = "missing"
		status.Issues = append(status.Issues, "Base rootfs not found")
	} else {
		status.Components["rootfs"] = "ok"
	}

	if cli.output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Print status
	if status.Healthy {
		fmt.Println("✓ Runtime is healthy")
	} else {
		fmt.Println("✗ Runtime has issues")
	}
	fmt.Println()

	fmt.Println("Components:")
	for name, state := range status.Components {
		icon := "✓"
		if state != "ok" {
			icon = "✗"
		}
		fmt.Printf("  %s %-20s %s\n", icon, name, state)
	}

	if len(status.Issues) > 0 {
		fmt.Println()
		fmt.Println("Issues:")
		for _, issue := range status.Issues {
			fmt.Printf("  - %s\n", issue)
		}
	}

	return nil
}

// =============================================================================
// Kill Command
// =============================================================================

func (cli *CLI) cmdKill(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fcctl kill <sandbox-id>")
	}

	id := args[0]
	sandboxDir := filepath.Join(cli.runDir, id)

	if _, err := os.Stat(sandboxDir); os.IsNotExist(err) {
		return fmt.Errorf("sandbox not found: %s", id)
	}

	info := cli.getSandboxInfo(id)

	if info.PID > 0 {
		fmt.Printf("Killing sandbox %s (PID %d)...\n", id, info.PID)
		process, err := os.FindProcess(info.PID)
		if err != nil {
			return fmt.Errorf("failed to find process: %w", err)
		}

		if err := process.Kill(); err != nil {
			return fmt.Errorf("failed to kill process: %w", err)
		}

		fmt.Println("Process killed")
	} else {
		fmt.Println("No running process found for sandbox")
	}

	return nil
}

// =============================================================================
// Cleanup Command
// =============================================================================

func (cli *CLI) cmdCleanup(ctx context.Context, args []string) error {
	dryRun := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-n" {
			dryRun = true
		}
	}

	fmt.Println("Scanning for orphaned resources...")

	sandboxes, err := cli.discoverSandboxes()
	if err != nil {
		return err
	}

	var orphaned []SandboxInfo
	for _, sb := range sandboxes {
		if sb.State == "dead" || sb.State == "unknown" {
			orphaned = append(orphaned, sb)
		}
	}

	if len(orphaned) == 0 {
		fmt.Println("No orphaned resources found")
		return nil
	}

	fmt.Printf("Found %d orphaned sandbox(es):\n", len(orphaned))
	for _, sb := range orphaned {
		fmt.Printf("  - %s (state: %s, pid: %d)\n", sb.ID, sb.State, sb.PID)
	}

	if dryRun {
		fmt.Println("\nDry run - no changes made")
		return nil
	}

	fmt.Println()
	fmt.Print("Clean up these resources? [y/N] ")

	var response string
	fmt.Scanln(&response)
	if response != "y" && response != "Y" {
		fmt.Println("Aborted")
		return nil
	}

	for _, sb := range orphaned {
		sandboxDir := filepath.Join(cli.runDir, sb.ID)

		// Kill process if still running
		if sb.PID > 0 {
			if process, err := os.FindProcess(sb.PID); err == nil {
				process.Kill()
			}
		}

		// Remove directory
		if err := os.RemoveAll(sandboxDir); err != nil {
			fmt.Printf("  Failed to remove %s: %v\n", sb.ID, err)
		} else {
			fmt.Printf("  Removed %s\n", sb.ID)
		}
	}

	fmt.Println("Cleanup complete")
	return nil
}

// =============================================================================
// Helper Functions
// =============================================================================

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func boolToStatus(b bool) string {
	if b {
		return "✓"
	}
	return "✗"
}
