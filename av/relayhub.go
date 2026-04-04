package av

import (
	"context"
	"time"

	"github.com/vtpl1/vrtc-sdk/lifecycle"
)

// StreamInfo describes a single audio or video stream within a relay.
type StreamInfo struct {
	Idx        uint16    `json:"idx"`
	CodecType  CodecType `json:"codecType"`
	Width      int       `json:"width,omitempty"`
	Height     int       `json:"height,omitempty"`
	SampleRate int       `json:"sampleRate,omitempty"`
}

// RelayStats is a point-in-time snapshot of a single relay's metrics.
type RelayStats struct {
	ID             string    `json:"id"`
	ConsumerCount  int       `json:"consumerCount"`
	PacketsRead    uint64    `json:"packetsRead"`
	BytesRead      uint64    `json:"bytesRead"`
	KeyFrames      uint64    `json:"keyFrames"`
	DroppedPackets uint64    `json:"droppedPackets"`
	StartedAt      time.Time `json:"startedAt"`
	LastPacketAt   time.Time `json:"lastPacketAt"`
	LastError      string    `json:"lastError,omitempty"`

	// Stream metadata from the relay's current codec headers.
	Streams    []StreamInfo `json:"streams,omitempty"`
	ActualFPS  float64      `json:"actualFps"`
	BitrateBps float64      `json:"bitrateBps"`

	// Consumer-aggregated counters.
	Rotations uint64 `json:"rotations"` // total muxer rotations across all consumers
	Skips     uint64 `json:"skips"`     // total keyframe-recovery skips across all consumers
}

// ConsumeOptions configures a consumer attachment to a relay.
type ConsumeOptions struct {
	// ConsumerID uniquely identifies the consumer within the relay.
	// If empty, the implementation assigns a generated ID.
	ConsumerID string

	// MuxerFactory opens the downstream muxer for this consumer.
	MuxerFactory MuxerFactory

	// MuxerRemover is called after the muxer is closed to deregister it.
	// May be nil if no deregistration is needed.
	MuxerRemover MuxerRemover

	// ErrChan, if non-nil, receives asynchronous write errors from the consumer's
	// muxer. The channel should be buffered to avoid blocking the write loop.
	ErrChan chan<- error
}

// ConsumerHandle represents an attached consumer.
//
// Close detaches the consumer from its relay and closes the underlying
// muxer. Close is safe to call multiple times.
type ConsumerHandle interface {
	ID() string
	Close(ctx context.Context) error
}

// RelayHub coordinates a set of relays (demuxers) and consumers (muxers).
// A single RelayHub may host multiple relays; each relay can serve
// multiple consumers simultaneously.
//
// # Lifecycle
//
// A RelayHub must be started before consumers can be attached:
//
//	hub := relayhub.New(demuxerFactory, demuxerRemover)
//	if err := hub.Start(ctx); err != nil { /* handle */ }
//	defer hub.Stop()
//
//	handle, err := hub.Consume(ctx, "camera-1", av.ConsumeOptions{
//		ConsumerID:   "recorder-a",
//		MuxerFactory: muxFactory,
//		MuxerRemover: muxRemover,
//		ErrChan:      errCh,
//	})
//	if err != nil { /* handle */ }
//	defer handle.Close(ctx)
//
// # Relay management
//
// Relays are created on-demand: the first Consume call for a given
// sourceID opens a demuxer via the DemuxerFactory supplied to the
// implementation constructor. A relay remains alive as long as at least
// one consumer is attached; idle relays (zero consumers) are reclaimed
// automatically by a background ticker (within ~1 s).
//
// # Delivery policy
//
// The delivery mode depends on the active consumer count per relay:
//   - 1 consumer  → blocking write: back-pressure propagates to ReadPacket;
//     no packets are dropped as long as the consumer keeps up.
//   - 2+ consumers → leaky write: a slow consumer drops frames rather than
//     stalling the pipeline for the others.
//
// # Concurrency
//
// All methods are safe to call concurrently from multiple goroutines.
type RelayHub interface {
	// GetRelayStats returns a point-in-time snapshot of all active relays.
	GetRelayStats(ctx context.Context) []RelayStats

	// GetRelayStatsByID returns the stats for a single relay identified by
	// sourceID. Returns the stats and true if the relay exists, or a zero
	// value and false if not. This is O(1) — a single map lookup plus the
	// Stats() call on that relay — so prefer it over GetRelayStats when
	// only one relay's metrics are needed.
	GetRelayStatsByID(ctx context.Context, sourceID string) (RelayStats, bool)

	// ListRelayIDs returns the sourceIDs of all active relays. This is a
	// lightweight O(n) key-only scan — no per-relay Stats() is computed.
	ListRelayIDs(ctx context.Context) []string

	// GetActiveRelayCount returns the number of relays currently managed
	// by this RelayHub. A relay is considered active from the moment its
	// first consumer is registered until all its consumers have been removed and
	// the background cleanup ticker has reclaimed it (within ~1 s).
	GetActiveRelayCount(ctx context.Context) int

	// Consume attaches a new consumer to the named relay and returns a handle
	// that can later detach it. If no relay with sourceID exists, one is
	// created automatically using the DemuxerFactory supplied to the constructor.
	//
	// Consume blocks until the relay's initial codec headers are available
	// (i.e. GetCodecs has returned), then delivers a WriteHeader to the muxer.
	// It retries transparently if the relay is still starting or is
	// transiently closing.
	//
	// Errors:
	//   - ErrRelayHubNotStartedYet  if Start has not been called.
	//   - ErrRelayHubClosing        if Stop or SignalStop has been called.
	//   - ErrConsumerAlreadyExists  if opts.ConsumerID is already registered
	//     on sourceID.
	//   - ErrRelayLastError (wrapped) if the relay's demuxer previously failed.
	//   - ctx.Err()                 if the context is cancelled while waiting.
	//
	// opts.ErrChan, if non-nil, receives asynchronous write errors from the
	// consumer's muxer (e.g. ErrMuxerWritePacket). The channel should be buffered
	// to avoid blocking the consumer's write loop.
	Consume(ctx context.Context, sourceID string, opts ConsumeOptions) (ConsumerHandle, error)

	// PauseRelay requests the named relay's demuxer to suspend packet
	// delivery. The request is forwarded only if the underlying DemuxCloser
	// implements av.Pauser; otherwise PauseRelay is a no-op.
	//
	// Errors:
	//   - ErrRelayNotFound      if no relay with sourceID exists.
	//   - ErrRelayClosing       if the relay is shutting down.
	//   - ErrRelayNotStartedYet if the relay goroutine has not yet begun.
	PauseRelay(ctx context.Context, sourceID string) error

	// ResumeRelay requests the named relay's demuxer to resume packet
	// delivery after a previous PauseRelay call. Like PauseRelay, it is
	// a no-op when the demuxer does not implement av.Pauser.
	//
	// Errors:
	//   - ErrRelayNotFound      if no relay with sourceID exists.
	//   - ErrRelayClosing       if the relay is shutting down.
	//   - ErrRelayNotStartedYet if the relay goroutine has not yet begun.
	ResumeRelay(ctx context.Context, sourceID string) error

	// StartStopper embeds the full Start / Stop lifecycle.
	//
	// Start launches the background goroutine that manages relay creation,
	// idle cleanup, and context propagation. It must be called exactly once
	// before Consume; subsequent calls return ErrRelayHubAlreadyStarted.
	//
	// Stop signals shutdown (cancels the internal context) and blocks until all
	// relays and their consumers have exited cleanly. Calling Stop multiple
	// times is safe; all calls after the first return nil immediately.
	lifecycle.StartStopper
}
