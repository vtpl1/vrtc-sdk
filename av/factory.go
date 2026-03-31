package av

import (
	"context"
)

// DemuxerFactory opens and returns a DemuxCloser for the given stream.
// sourceID identifies the source (e.g. an RTSP URL, a camera ID, or a file path).
// The caller is responsible for calling Close() on the returned DemuxCloser when done.
type DemuxerFactory func(ctx context.Context, sourceID string) (DemuxCloser, error)

// DemuxerRemover tears down a previously created demuxer and deregisters it from any
// internal registry. It must be called after Close() has been called on the DemuxCloser.
// sourceID must match the value used when the demuxer was created.
type DemuxerRemover func(ctx context.Context, sourceID string) error

// MuxerFactory opens and returns a MuxCloser for the given stream and consumer.
// consumerID identifies the downstream sink
// (e.g. a recording session, a subscriber connection, or an output URL).
// The caller is responsible for calling Close() on the returned MuxCloser when done.
type MuxerFactory func(ctx context.Context, consumerID string) (MuxCloser, error)

// MuxerRemover tears down a previously created muxer and deregisters it from any
// internal registry. It must be called after Close() has been called on the MuxCloser.
// consumerID must match the values used when the muxer was created.
type MuxerRemover func(ctx context.Context, consumerID string) error

// RelayRemover tears down a relay and deregisters it from any internal registry.
// Unlike DemuxerRemover, which operates at the demuxer level, RelayRemover operates
// at the relay-entity level and may encompass additional cleanup (e.g. removing all
// associated consumers before deregistering the relay).
// sourceID must match the value used when the relay was created.
type RelayRemover func(ctx context.Context, sourceID string) error

// ConsumerRemover tears down a consumer and deregisters it from any internal registry.
// consumerID must be globally unique across all relays; it is not scoped per relay.
// It must be called after the associated MuxCloser has been closed.
type ConsumerRemover func(ctx context.Context, consumerID string) error
