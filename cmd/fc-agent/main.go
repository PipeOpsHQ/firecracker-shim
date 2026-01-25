// fc-agent is the minimal guest agent that runs inside Firecracker VMs.
//
// This agent is designed to be as small and fast as possible:
// - Static binary (~2-3MB)
// - No runtime dependencies
// - Minimal memory footprint
//
// It communicates with the host via vsock and manages containers using runc.
//
// Build: CGO_ENABLED=0 go build -ldflags="-s -w" -o fc-agent ./cmd/fc-agent
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
)

const (
	vsockPort     = 1024
	runcBinary    = "/usr/bin/runc"
	containerRoot = "/run/fc-agent/containers"
)

// Agent manages containers inside the VM.
type Agent struct {
	mu         sync.RWMutex
	containers map[string]*Container
	log        *Logger
}

// Container represents a managed container.
type Container struct {
	ID      string
	Bundle  string
	PID     int
	Status  string
	Created time.Time
}

// Logger is a simple structured logger.
type Logger struct {
	prefix string
}

func (l *Logger) Info(msg string, fields ...interface{}) {
	l.log("INFO", msg, fields...)
}

func (l *Logger) Error(msg string, fields ...interface{}) {
	l.log("ERROR", msg, fields...)
}

func (l *Logger) log(level, msg string, fields ...interface{}) {
	fmt.Fprintf(os.Stderr, "[%s] %s %s %v\n", level, l.prefix, msg, fields)
}

func main() {
	log := &Logger{prefix: "fc-agent"}
	log.Info("Starting fc-agent")

	// Ensure required directories exist
	for _, dir := range []string{containerRoot, "/run/runc"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Error("Failed to create directory", "dir", dir, "error", err)
			os.Exit(1)
		}
	}

	// Create agent
	agent := &Agent{
		containers: make(map[string]*Container),
		log:        log,
	}

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Start vsock listener
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigCh
		log.Info("Received shutdown signal")
		cancel()
	}()

	if err := agent.serve(ctx); err != nil && ctx.Err() == nil {
		log.Error("Server error", "error", err)
		os.Exit(1)
	}
}

func (a *Agent) serve(ctx context.Context) error {
	// Listen on vsock
	listener, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		return fmt.Errorf("failed to listen on vsock: %w", err)
	}
	defer listener.Close()

	a.log.Info("Listening on vsock", "port", vsockPort)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Accept connection
		conn, err := listener.Accept()
		if err != nil {
			a.log.Error("Accept error", "error", err)
			continue
		}

		go a.handleConnection(ctx, conn)
	}
}

func (a *Agent) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var req Request
		if err := decoder.Decode(&req); err != nil {
			if err == io.EOF {
				return
			}
			a.log.Error("Decode error", "error", err)
			return
		}

		resp := a.handleRequest(&req)
		if err := encoder.Encode(resp); err != nil {
			a.log.Error("Encode error", "error", err)
			return
		}
	}
}

func (a *Agent) handleRequest(req *Request) *Response {
	resp := &Response{ID: req.ID}

	switch req.Method {
	case "ping":
		resp.Result = map[string]string{"status": "ok"}

	case "create_container":
		if err := a.createContainer(req.Params); err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = map[string]string{"status": "created"}
		}

	case "start_container":
		pid, err := a.startContainer(req.Params)
		if err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = map[string]interface{}{"pid": pid}
		}

	case "stop_container":
		if err := a.stopContainer(req.Params); err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = map[string]string{"status": "stopped"}
		}

	case "remove_container":
		if err := a.removeContainer(req.Params); err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = map[string]string{"status": "removed"}
		}

	case "exec_sync":
		result, err := a.execSync(req.Params)
		if err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = result
		}

	case "get_stats":
		stats, err := a.getStats(req.Params)
		if err != nil {
			resp.Error = &ResponseError{Code: 1, Message: err.Error()}
		} else {
			resp.Result = stats
		}

	default:
		resp.Error = &ResponseError{Code: -32601, Message: "Method not found"}
	}

	return resp
}

// =============================================================================
// Container Operations
// =============================================================================

func (a *Agent) createContainer(params map[string]interface{}) error {
	id, _ := params["id"].(string)
	bundle, _ := params["bundle"].(string)

	if id == "" {
		return fmt.Errorf("container ID required")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.containers[id]; exists {
		return fmt.Errorf("container %s already exists", id)
	}

	// Create container directory
	containerDir := filepath.Join(containerRoot, id)
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		return fmt.Errorf("failed to create container dir: %w", err)
	}

	// Run runc create
	cmd := exec.Command(runcBinary, "create",
		"--bundle", bundle,
		"--pid-file", filepath.Join(containerDir, "pid"),
		id)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("runc create failed: %w: %s", err, output)
	}

	a.containers[id] = &Container{
		ID:      id,
		Bundle:  bundle,
		Status:  "created",
		Created: time.Now(),
	}

	a.log.Info("Container created", "id", id)
	return nil
}

func (a *Agent) startContainer(params map[string]interface{}) (int, error) {
	id, _ := params["id"].(string)
	if id == "" {
		return 0, fmt.Errorf("container ID required")
	}

	a.mu.Lock()
	container, exists := a.containers[id]
	a.mu.Unlock()

	if !exists {
		return 0, fmt.Errorf("container %s not found", id)
	}

	// Run runc start
	cmd := exec.Command(runcBinary, "start", id)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("runc start failed: %w: %s", err, output)
	}

	// Read PID
	pidFile := filepath.Join(containerRoot, id, "pid")
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("failed to read pid file: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(pidData), "%d", &pid); err != nil {
		return 0, fmt.Errorf("failed to parse pid: %w", err)
	}

	a.mu.Lock()
	container.PID = pid
	container.Status = "running"
	a.mu.Unlock()

	a.log.Info("Container started", "id", id, "pid", pid)
	return pid, nil
}

func (a *Agent) stopContainer(params map[string]interface{}) error {
	id, _ := params["id"].(string)
	timeout, _ := params["timeout"].(float64)
	if timeout == 0 {
		timeout = 10
	}

	if id == "" {
		return fmt.Errorf("container ID required")
	}

	// Try graceful stop with SIGTERM
	cmd := exec.Command(runcBinary, "kill", id, "SIGTERM")
	_ = cmd.Run()

	// Wait for container to stop
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(deadline) {
		state, _ := a.getContainerState(id)
		if state == "stopped" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill if still running
	cmd = exec.Command(runcBinary, "kill", id, "SIGKILL")
	_ = cmd.Run()

	a.mu.Lock()
	if container, exists := a.containers[id]; exists {
		container.Status = "stopped"
	}
	a.mu.Unlock()

	a.log.Info("Container stopped", "id", id)
	return nil
}

func (a *Agent) removeContainer(params map[string]interface{}) error {
	id, _ := params["id"].(string)
	if id == "" {
		return fmt.Errorf("container ID required")
	}

	// Run runc delete
	cmd := exec.Command(runcBinary, "delete", "--force", id)
	_ = cmd.Run() // Ignore errors

	// Clean up container directory
	containerDir := filepath.Join(containerRoot, id)
	os.RemoveAll(containerDir)

	a.mu.Lock()
	delete(a.containers, id)
	a.mu.Unlock()

	a.log.Info("Container removed", "id", id)
	return nil
}

func (a *Agent) execSync(params map[string]interface{}) (map[string]interface{}, error) {
	id, _ := params["id"].(string)
	cmdArgs, _ := params["cmd"].([]interface{})
	timeout, _ := params["timeout"].(float64)
	if timeout == 0 {
		timeout = 30
	}

	if id == "" || len(cmdArgs) == 0 {
		return nil, fmt.Errorf("container ID and command required")
	}

	// Convert command args
	args := make([]string, len(cmdArgs))
	for i, arg := range cmdArgs {
		args[i], _ = arg.(string)
	}

	// Build runc exec command
	execArgs := []string{"exec", id}
	execArgs = append(execArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, runcBinary, execArgs...)
	stdout, err := cmd.Output()

	var stderr []byte
	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			stderr = exitErr.Stderr
		} else {
			return nil, fmt.Errorf("exec failed: %w", err)
		}
	}

	return map[string]interface{}{
		"exit_code": exitCode,
		"stdout":    string(stdout),
		"stderr":    string(stderr),
	}, nil
}

func (a *Agent) getStats(params map[string]interface{}) (map[string]interface{}, error) {
	id, _ := params["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("container ID required")
	}

	// Read cgroup stats
	// This is simplified - real implementation would read from cgroup fs

	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/system.slice/runc-%s.scope", id)

	// CPU usage
	cpuUsage := readCgroupValue(filepath.Join(cgroupPath, "cpu.stat"), "usage_usec")

	// Memory usage
	memUsage := readCgroupValue(filepath.Join(cgroupPath, "memory.current"), "")

	return map[string]interface{}{
		"cpu_usage":    cpuUsage,
		"memory_usage": memUsage,
		"read_bytes":   0,
		"write_bytes":  0,
	}, nil
}

func (a *Agent) getContainerState(id string) (string, error) {
	cmd := exec.Command(runcBinary, "state", id)
	output, err := cmd.Output()
	if err != nil {
		return "unknown", err
	}

	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &state); err != nil {
		return "unknown", err
	}

	return state.Status, nil
}

func readCgroupValue(path, key string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	if key == "" {
		var val uint64
		_, _ = fmt.Sscanf(string(data), "%d", &val)
		return val
	}

	// Parse key-value format
	var val uint64
	_, _ = fmt.Sscanf(string(data), key+" %d", &val)
	return val
}

// =============================================================================
// Protocol Types
// =============================================================================

type Request struct {
	ID     uint64                 `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type Response struct {
	ID     uint64         `json:"id"`
	Result interface{}    `json:"result,omitempty"`
	Error  *ResponseError `json:"error,omitempty"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
