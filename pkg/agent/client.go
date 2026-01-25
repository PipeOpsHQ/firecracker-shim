// Package agent provides the host-side client for communicating with the
// guest agent running inside Firecracker VMs via vsock.
//
// The communication protocol is simple JSON-RPC over vsock, designed to be
// lightweight and easy to implement in any language.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/pipeops/firecracker-cri/pkg/domain"
	"github.com/sirupsen/logrus"
)

// Client implements domain.AgentClient for communicating with the guest agent.
type Client struct {
	mu sync.Mutex

	conn      net.Conn
	encoder   *json.Encoder
	decoder   *json.Decoder
	requestID uint64

	log *logrus.Entry
}

// NewClient creates a new agent client.
func NewClient(log *logrus.Entry) *Client {
	return &Client{
		log: log.WithField("component", "agent-client"),
	}
}

// Connect establishes a connection to the guest agent via vsock.
func (c *Client) Connect(ctx context.Context, vsockPath string, cid uint32, port uint32) error {
	c.log.WithFields(logrus.Fields{
		"vsock_path": vsockPath,
		"cid":        cid,
		"port":       port,
	}).Info("Connecting to guest agent")

	// Connect to the vsock Unix socket that Firecracker exposes
	var conn net.Conn
	vsockConn, err := vsock.Dial(cid, port, &vsock.Config{})
	if err != nil {
		// Fallback: try Unix socket directly if vsock package fails
		conn, err = net.DialTimeout("unix", vsockPath, 30*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to vsock: %w", err)
		}
	} else {
		conn = vsockConn
	}

	c.mu.Lock()
	c.conn = conn
	c.encoder = json.NewEncoder(conn)
	c.decoder = json.NewDecoder(conn)
	c.mu.Unlock()

	// Wait for agent to be ready
	if err := c.waitForReady(ctx); err != nil {
		conn.Close()
		return fmt.Errorf("agent not ready: %w", err)
	}

	c.log.Info("Connected to guest agent")
	return nil
}

// Close terminates the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// CreateContainer creates a container inside the VM.
func (c *Client) CreateContainer(ctx context.Context, spec *domain.ContainerSpec) error {
	req := &Request{
		Method: "create_container",
		Params: map[string]interface{}{
			"id":       spec.ID,
			"bundle":   spec.BundlePath,
			"stdin":    spec.Stdin,
			"stdout":   spec.Stdout,
			"stderr":   spec.Stderr,
			"terminal": spec.Terminal,
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("create_container failed: %s", resp.Error.Message)
	}

	return nil
}

// StartContainer starts a created container.
func (c *Client) StartContainer(ctx context.Context, containerID string) (int, error) {
	req := &Request{
		Method: "start_container",
		Params: map[string]interface{}{
			"id": containerID,
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return 0, err
	}

	if resp.Error != nil {
		return 0, fmt.Errorf("start_container failed: %s", resp.Error.Message)
	}

	// Extract PID from result
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("invalid response format")
	}

	pid, _ := result["pid"].(float64)
	return int(pid), nil
}

// StopContainer stops a running container.
func (c *Client) StopContainer(ctx context.Context, containerID string, timeout time.Duration) error {
	req := &Request{
		Method: "stop_container",
		Params: map[string]interface{}{
			"id":      containerID,
			"timeout": int(timeout.Seconds()),
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("stop_container failed: %s", resp.Error.Message)
	}

	return nil
}

// RemoveContainer removes a container.
func (c *Client) RemoveContainer(ctx context.Context, containerID string) error {
	req := &Request{
		Method: "remove_container",
		Params: map[string]interface{}{
			"id": containerID,
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return err
	}

	if resp.Error != nil {
		return fmt.Errorf("remove_container failed: %s", resp.Error.Message)
	}

	return nil
}

// ExecSync executes a command synchronously.
func (c *Client) ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (*domain.ExecResult, error) {
	req := &Request{
		Method: "exec_sync",
		Params: map[string]interface{}{
			"id":      containerID,
			"cmd":     cmd,
			"timeout": int(timeout.Seconds()),
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("exec_sync failed: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response format")
	}

	exitCode, _ := result["exit_code"].(float64)
	stdout, _ := result["stdout"].(string)
	stderr, _ := result["stderr"].(string)

	return &domain.ExecResult{
		ExitCode: int32(exitCode),
		Stdout:   []byte(stdout),
		Stderr:   []byte(stderr),
	}, nil
}

// GetContainerStats retrieves container resource usage.
func (c *Client) GetContainerStats(ctx context.Context, containerID string) (*domain.ContainerStats, error) {
	req := &Request{
		Method: "get_stats",
		Params: map[string]interface{}{
			"id": containerID,
		},
	}

	resp, err := c.call(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("get_stats failed: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response format")
	}

	cpuUsage, _ := result["cpu_usage"].(float64)
	memUsage, _ := result["memory_usage"].(float64)
	readBytes, _ := result["read_bytes"].(float64)
	writeBytes, _ := result["write_bytes"].(float64)

	return &domain.ContainerStats{
		CPUUsage:    uint64(cpuUsage),
		MemoryUsage: uint64(memUsage),
		ReadBytes:   uint64(readBytes),
		WriteBytes:  uint64(writeBytes),
	}, nil
}

// =============================================================================
// Protocol Types
// =============================================================================

// Request is a JSON-RPC request.
type Request struct {
	ID     uint64                 `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// Response is a JSON-RPC response.
type Response struct {
	ID     uint64         `json:"id"`
	Result interface{}    `json:"result,omitempty"`
	Error  *ResponseError `json:"error,omitempty"`
}

// ResponseError represents an error in a response.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// =============================================================================
// Internal Methods
// =============================================================================

func (c *Client) call(ctx context.Context, req *Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	// Assign request ID
	req.ID = atomic.AddUint64(&c.requestID, 1)

	// Set deadline from context
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
		defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	}

	// Send request
	if err := c.encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	var resp Response
	if err := c.decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Verify response ID matches
	if resp.ID != req.ID {
		return nil, fmt.Errorf("response ID mismatch: expected %d, got %d", req.ID, resp.ID)
	}

	return &resp, nil
}

func (c *Client) waitForReady(ctx context.Context) error {
	// Send a ping and wait for response
	req := &Request{
		Method: "ping",
	}

	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := c.call(ctx, req)
		if err == nil && resp.Error == nil {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for agent")
}
