// Package lifecycle provides interfaces and helpers for managing the start/stop
// lifecycle of long-running service components.
//
// The core interfaces (SignalStopper, Stopper, StartStopper) establish a
// conventional two-phase shutdown model: signal first, wait separately. This
// allows callers to fan out shutdown across multiple components before blocking
// on any of them.
package lifecycle

import (
	"os"
	"os/signal"
	"syscall"
)

// WaitForTerminationRequest handles termination signals to gracefully shut down the server.
func WaitForTerminationRequest(errChan <-chan error) {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
		return
	case <-errChan:
		return
	}
}
