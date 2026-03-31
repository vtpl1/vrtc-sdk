package relayhub

import "errors"

var (
	ErrRelayNotFound          = errors.New("producer not found")
	ErrRelayDemuxFactory      = errors.New("producer demux factory")
	ErrConsumerMuxFactory     = errors.New("consumer mux factory")
	ErrRelayHubClosing        = errors.New("stream manager closing")
	ErrRelayClosing           = errors.New("producer closing")
	ErrConsumerClosing        = errors.New("consumer closing")
	ErrRelayLastError         = errors.New("producer last error")
	ErrConsumerAlreadyExists  = errors.New("consumer already exists")
	ErrCodecsNotAvailable     = errors.New("codecs not available")
	ErrDroppingPacket         = errors.New("dropping packet")
	ErrRelayHubNotStartedYet  = errors.New("stream manager not started yet")
	ErrRelayNotStartedYet     = errors.New("producer not started yet")
	ErrMuxerWritePacket       = errors.New("muxer write packet")
	ErrMuxerWriteHeader       = errors.New("muxer write header")
	ErrMuxerWriteCodecChange  = errors.New("muxer write codec change")
	ErrRelayHubAlreadyStarted = errors.New("stream manager already started")
	ErrRelayAlreadyStarted    = errors.New("producer already started")
	ErrConsumerAlreadyStarted = errors.New("consumer already started")
)
