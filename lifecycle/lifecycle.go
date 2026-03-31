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
