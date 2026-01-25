// containerd-shim-fc-v2 is the containerd shim for Firecracker.
//
// This binary is launched by containerd when a task using the "firecracker"
// runtime is created. It manages the lifecycle of Firecracker VMs and
// communicates with containerd via ttrpc.
//
// Naming convention: containerd-shim-{runtime}-v2
// This maps to the runtime type: io.containerd.{runtime}.v2
//
// Build: go build -o containerd-shim-fc-v2 ./cmd/containerd-shim-fc-v2
package main

import (
	"context"
	"os"

	"github.com/containerd/containerd/runtime/v2/shim"
	fcshim "github.com/pipeops/firecracker-cri/pkg/shim"
	"github.com/sirupsen/logrus"
)

func main() {
	// Configure logging
	logrus.SetLevel(logrus.InfoLevel)
	if os.Getenv("FC_CRI_LOG_LEVEL") == "debug" {
		logrus.SetLevel(logrus.DebugLevel)
	}

	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Run the shim
	// shim.Run handles:
	// - Parsing command line arguments (start, delete, etc.)
	// - Setting up ttrpc server
	// - Signal handling
	// - Daemonization
	shim.Run(
		"io.containerd.firecracker.v2",
		shimManager,
	)
}

// shimManager creates new shim instances.
func shimManager(ctx context.Context, id string, publisher shim.Publisher, shutdown func()) (shim.Shim, error) {
	return fcshim.New(ctx, id, publisher, shutdown)
}
