package lifecycle

import "context"

// SignalStopper can asynchronously signal a stop without waiting for completion.
// SignalStop returns true on the first call and false on subsequent calls,
// allowing safe concurrent use.
type SignalStopper interface {
	SignalStop() bool
}

// Stopper extends SignalStopper with blocking shutdown: WaitStop blocks until
// all resources have been released, and Stop is a convenience wrapper that
// calls SignalStop followed by WaitStop.
type Stopper interface {
	SignalStopper
	WaitStop() error
	Stop() error
}

// StartStopper is the full lifecycle interface for service components that must
// be started before use and cleanly stopped when done.
type StartStopper interface {
	// Start initialises the component and begins background processing.
	// The provided context governs the component's lifetime; cancelling it
	// is equivalent to calling SignalStop.
	Start(ctx context.Context) error
	Stopper
}
