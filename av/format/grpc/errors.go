package grpc

import "errors"

var (
	errClientMuxerClosed         = errors.New("grpc: client muxer already closed")
	errHeaderAlreadyWritten      = errors.New("grpc: WriteHeader called twice")
	errNoHeader                  = errors.New("grpc: WritePacket called before WriteHeader")
	errTrailerNoHeader           = errors.New("grpc: WriteTrailer called before WriteHeader")
	errTrailerCalledTwice        = errors.New("grpc: WriteTrailer called twice")
	errReadBeforeGetCodecs       = errors.New("grpc: ReadPacket called before GetCodecs")
	errH264MissingConfig         = errors.New("grpc: H264 stream missing avcC config record")
	errH265MissingConfig         = errors.New("grpc: H265 stream missing hvcC config record")
	errAACMissingConfig          = errors.New("grpc: AAC stream missing MPEG4AudioConfig bytes")
	errPacketDurationUnsupported = errors.New(
		"grpc: PacketDuration not supported for generic codec",
	)
	errPullNotSupported      = errors.New("grpc: pull not supported")
	errServerMuxerClosed     = errors.New("grpc: server muxer closed")
	errServerMuxerTrailerDup = errors.New("grpc: WriteTrailer called twice")
	errUnexpectedHeader      = errors.New("grpc: expected StreamHeader")
	errUnexpectedPayload     = errors.New("grpc: unexpected payload type")
	errRemoteError           = errors.New("grpc: remote error")
	errRemoteTrailer         = errors.New("grpc: remote trailer error")
	errPushHeaderExpected    = errors.New("grpc: first PushStream message must be a header")
	errNoPushedSource        = errors.New("grpc: no pushed source")
	errSourceIDInUse         = errors.New("grpc: source ID already has an active push stream")
	errIdxOverflow           = errors.New("grpc: stream index exceeds uint16 range")
	errPauseNotSupported     = errors.New("grpc: pause/resume not supported")
	errSeekNotSupported      = errors.New("grpc: seek not supported")
)
