package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mockStartStopper struct{}

func (mockStartStopper) Start(_ context.Context) error { return nil }
func (mockStartStopper) SignalStop() bool              { return true }
func (mockStartStopper) WaitStop() error               { return nil }
func (mockStartStopper) Stop() error                   { return nil }

func TestStartStopperInterfaceConformance(t *testing.T) {
	t.Parallel()

	var _ StartStopper = (*mockStartStopper)(nil)
}

func TestWaitForTerminationRequestReturnsOnErrChanValue(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	done := make(chan struct{})

	go func() {
		WaitForTerminationRequest(errCh)
		close(done)
	}()

	errCh <- errors.New("stop requested")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTerminationRequest did not return after errChan value")
	}
}

func TestWaitForTerminationRequestReturnsOnErrChanClose(t *testing.T) {
	t.Parallel()

	errCh := make(chan error)
	done := make(chan struct{})

	go func() {
		WaitForTerminationRequest(errCh)
		close(done)
	}()

	close(errCh)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForTerminationRequest did not return after errChan close")
	}
}
