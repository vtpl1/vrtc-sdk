package relayhub

import "errors"

// Sentinel errors returned by RelayHub, Relay, and Consumer operations.
// Callers should use errors.Is for matching; errors are often wrapped with
// additional context.
var (
	// ErrRelayNotFound is returned when no relay with the given sourceID exists.
	ErrRelayNotFound = errors.New("producer not found")

	// ErrRelayDemuxFactory wraps errors from the DemuxerFactory callback.
	ErrRelayDemuxFactory = errors.New("producer demux factory")

	// ErrConsumerMuxFactory wraps errors from the MuxerFactory callback.
	ErrConsumerMuxFactory = errors.New("consumer mux factory")

	// ErrRelayHubClosing is returned by Consume when Stop or SignalStop has been called.
	ErrRelayHubClosing = errors.New("relay hub closing")

	// ErrRelayClosing is returned when an operation targets a relay that is shutting down.
	ErrRelayClosing = errors.New("producer closing")

	// ErrConsumerClosing is returned when Start is called after Close on a Consumer.
	ErrConsumerClosing = errors.New("consumer closing")

	// ErrRelayLastError wraps the demuxer error that caused the relay to fail on a
	// previous run. Returned by Consume when the relay's demuxer previously failed.
	ErrRelayLastError = errors.New("producer last error")

	// ErrConsumerAlreadyExists is returned when a consumer with the same ID is
	// already registered on the relay.
	ErrConsumerAlreadyExists = errors.New("consumer already exists")

	// ErrCodecsNotAvailable is returned when codec headers could not be obtained
	// from the demuxer (e.g. empty stream list).
	ErrCodecsNotAvailable = errors.New("codecs not available")

	// ErrDroppingPacket is returned by WritePacketLeaky when the consumer's packet
	// buffer is full and the packet must be discarded.
	ErrDroppingPacket = errors.New("dropping packet")

	// ErrRelayHubNotStartedYet is returned by Consume when Start has not been called.
	ErrRelayHubNotStartedYet = errors.New("relay hub not started yet")

	// ErrRelayNotStartedYet is returned when AddConsumer is called before Start.
	ErrRelayNotStartedYet = errors.New("producer not started yet")

	// ErrMuxerWritePacket wraps errors from Muxer.WritePacket.
	ErrMuxerWritePacket = errors.New("muxer write packet")

	// ErrMuxerWriteHeader wraps errors from Muxer.WriteHeader.
	ErrMuxerWriteHeader = errors.New("muxer write header")

	// ErrMuxerWriteCodecChange wraps errors from CodecChanger.WriteCodecChange.
	ErrMuxerWriteCodecChange = errors.New("muxer write codec change")

	// ErrRelayHubAlreadyStarted is returned when Start is called more than once on a RelayHub.
	ErrRelayHubAlreadyStarted = errors.New("relay hub already started")

	// ErrRelayAlreadyStarted is returned when Start is called more than once on a Relay.
	ErrRelayAlreadyStarted = errors.New("producer already started")

	// ErrConsumerAlreadyStarted is returned when Start is called more than once on a Consumer.
	ErrConsumerAlreadyStarted = errors.New("consumer already started")

	// ErrMuxerRotate is a sentinel error that a MuxCloser may return from
	// WritePacket to signal that the current segment is full and the consumer
	// should close this muxer and open a new one via the MuxerFactory.
	// The packet that triggered the rotation is re-delivered to the new muxer.
	ErrMuxerRotate = errors.New("muxer rotate")
)
