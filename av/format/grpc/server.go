package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

const defaultPacketBufSize = 50

// PushHandler is called when a new source connects via PushStream.
// The handler receives the sourceID and a DemuxCloser it can wire into a RelayHub.
// The callback must return promptly; the demuxer's ReadPacket loop runs concurrently.
type PushHandler func(ctx context.Context, sourceID string, dmx av.DemuxCloser)

// PullHandler is called when a consumer subscribes via PullStream.
// The handler receives source/consumer IDs and a MuxCloser it must wire into a RelayHub
// consumer. The handler blocks until the consumer is done (i.e., until WriteTrailer is
// called on the muxer or the context is cancelled). Return nil on clean shutdown.
type PullHandler func(ctx context.Context, sourceID, consumerID string, mux av.MuxCloser) error

// PauseHandler is called when a client sends a PauseStream or ResumeStream RPC.
// pause is true for pause, false for resume.
type PauseHandler func(ctx context.Context, sourceID string, pause bool) error

// SeekHandler is called when a client sends a SeekStream RPC.
// Returns the actual position landed on.
type SeekHandler func(ctx context.Context, sourceID string, pos time.Duration) (time.Duration, error)

// Server implements the AVTransportService gRPC server.
type Server struct {
	pb.UnimplementedAVTransportServiceServer

	onPush  PushHandler
	onPull  PullHandler
	onPause PauseHandler
	onSeek  SeekHandler

	mu       sync.RWMutex
	demuxers map[string]*ServerDemuxer
}

// ServerOption configures optional Server handlers.
type ServerOption func(*Server)

// WithPauseHandler sets the handler for PauseStream/ResumeStream RPCs.
func WithPauseHandler(h PauseHandler) ServerOption {
	return func(s *Server) { s.onPause = h }
}

// WithSeekHandler sets the handler for SeekStream RPCs.
func WithSeekHandler(h SeekHandler) ServerOption {
	return func(s *Server) { s.onSeek = h }
}

// NewServer creates a new AVTransport gRPC server.
// onPush is called for each PushStream connection (may be nil if push is not needed).
// onPull is called for each PullStream connection (may be nil if pull is not needed).
func NewServer(onPush PushHandler, onPull PullHandler, opts ...ServerOption) *Server {
	s := &Server{
		onPush:   onPush,
		onPull:   onPull,
		demuxers: make(map[string]*ServerDemuxer),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// DemuxerFactory returns an av.DemuxerFactory that looks up ServerDemuxers by sourceID.
// This is useful for wiring pushed sources into a RelayHub.
func (s *Server) DemuxerFactory() av.DemuxerFactory {
	return func(_ context.Context, sourceID string) (av.DemuxCloser, error) {
		s.mu.RLock()
		dmx, ok := s.demuxers[sourceID]
		s.mu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("%w: %q", errNoPushedSource, sourceID)
		}

		return dmx, nil
	}
}

// PushStream handles an incoming push stream from a remote edge client.
func (s *Server) PushStream(stream pb.AVTransportService_PushStreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}

	sourceID := first.GetSourceId()

	hdr, ok := first.GetPayload().(*pb.PushStreamRequest_Header)
	if !ok {
		return fmt.Errorf("%w: got %T", errPushHeaderExpected, first.GetPayload())
	}

	streams, err := unmarshalStreams(hdr.Header.GetStreams())
	if err != nil {
		return fmt.Errorf("grpc: unmarshal header: %w", err)
	}

	dmx := newServerDemuxer(sourceID, defaultPacketBufSize)
	dmx.setCodecsAndSignal(streams)

	// Register, rejecting if sourceID is already active.
	s.mu.Lock()
	if _, exists := s.demuxers[sourceID]; exists {
		s.mu.Unlock()

		return fmt.Errorf("%w: %q", errSourceIDInUse, sourceID)
	}

	s.demuxers[sourceID] = dmx
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only remove our own registration.
		if s.demuxers[sourceID] == dmx {
			delete(s.demuxers, sourceID)
		}
		s.mu.Unlock()
		close(dmx.packets)
	}()

	if s.onPush != nil {
		s.onPush(stream.Context(), sourceID, dmx)
	}

	return s.pushReadLoop(stream, dmx)
}

// PullStream handles a pull subscription from a remote consumer.
func (s *Server) PullStream(
	req *pb.PullStreamRequest,
	stream pb.AVTransportService_PullStreamServer,
) error {
	if s.onPull == nil {
		return errPullNotSupported
	}

	mux := newServerMuxer(stream)

	return s.onPull(stream.Context(), req.GetSourceId(), req.GetConsumerId(), mux)
}

// PauseStream handles a pause request from a remote client.
func (s *Server) PauseStream(
	ctx context.Context,
	req *pb.PauseStreamRequest,
) (*pb.PauseStreamResponse, error) {
	if s.onPause == nil {
		return nil, errPauseNotSupported
	}

	if err := s.onPause(ctx, req.GetSourceId(), true); err != nil {
		return nil, err
	}

	return &pb.PauseStreamResponse{}, nil
}

// ResumeStream handles a resume request from a remote client.
func (s *Server) ResumeStream(
	ctx context.Context,
	req *pb.ResumeStreamRequest,
) (*pb.ResumeStreamResponse, error) {
	if s.onPause == nil {
		return nil, errPauseNotSupported
	}

	if err := s.onPause(ctx, req.GetSourceId(), false); err != nil {
		return nil, err
	}

	return &pb.ResumeStreamResponse{}, nil
}

// SeekStream handles a seek request from a remote client.
func (s *Server) SeekStream(
	ctx context.Context,
	req *pb.SeekStreamRequest,
) (*pb.SeekStreamResponse, error) {
	if s.onSeek == nil {
		return nil, errSeekNotSupported
	}

	actual, err := s.onSeek(ctx, req.GetSourceId(), time.Duration(req.GetPositionNs()))
	if err != nil {
		return nil, err
	}

	return &pb.SeekStreamResponse{ActualPositionNs: int64(actual)}, nil
}

// pushReadLoop reads packets from the client and feeds them into the demuxer channel.
func (s *Server) pushReadLoop(
	stream pb.AVTransportService_PushStreamServer,
	dmx *ServerDemuxer,
) error {
	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				dmx.setError(io.EOF)

				return stream.SendAndClose(&pb.PushStreamResponse{})
			}

			dmx.setError(recvErr)

			return recvErr
		}

		switch p := msg.GetPayload().(type) {
		case *pb.PushStreamRequest_Packet:
			pkt, unmarshalErr := unmarshalPacket(p.Packet)
			if unmarshalErr != nil {
				dmx.setError(unmarshalErr)

				return unmarshalErr
			}

			select {
			case <-stream.Context().Done():
				dmx.setError(stream.Context().Err())

				return stream.Context().Err()
			case dmx.packets <- pkt:
			}
		case *pb.PushStreamRequest_Trailer:
			if p.Trailer.GetError() != "" {
				dmx.setError(fmt.Errorf("%w: %s", errRemoteTrailer, p.Trailer.GetError()))
			} else {
				dmx.setError(io.EOF)
			}

			return stream.SendAndClose(&pb.PushStreamResponse{})
		case *pb.PushStreamRequest_Header:
			newStreams, unmarshalErr := unmarshalStreams(p.Header.GetStreams())
			if unmarshalErr != nil {
				dmx.setError(fmt.Errorf("grpc: unmarshal codec change: %w", unmarshalErr))

				return unmarshalErr
			}

			dmx.updateCodecs(newStreams)
		default:
			err := fmt.Errorf("%w: %T", errUnexpectedPayload, msg.GetPayload())
			dmx.setError(err)

			return err
		}
	}
}
