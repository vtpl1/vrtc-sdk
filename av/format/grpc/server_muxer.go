package grpc

import (
	"context"
	"sync"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

// ServerMuxer implements av.MuxCloser on the server side for a pull subscriber.
// The PullStream handler creates one of these and the RelayHub consumer drives it.
type ServerMuxer struct {
	stream        pb.AVTransportService_PullStreamServer
	mu            sync.Mutex
	headerWritten bool
	closed        bool
}

func newServerMuxer(stream pb.AVTransportService_PullStreamServer) *ServerMuxer {
	return &ServerMuxer{stream: stream}
}

// WriteHeader sends the StreamHeader to the pull client.
// Must be called exactly once before any WritePacket.
func (m *ServerMuxer) WriteHeader(_ context.Context, streams []av.Stream) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errServerMuxerClosed
	}

	if m.headerWritten {
		return errHeaderAlreadyWritten
	}

	m.headerWritten = true

	return m.stream.Send(&pb.PullStreamResponse{
		Payload: &pb.PullStreamResponse_Header{
			Header: &pb.StreamHeader{Streams: marshalStreams(streams)},
		},
	})
}

// WritePacket sends a single AV packet to the pull client.
func (m *ServerMuxer) WritePacket(_ context.Context, pkt av.Packet) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errServerMuxerClosed
	}

	if !m.headerWritten {
		return errNoHeader
	}

	return m.stream.Send(&pb.PullStreamResponse{
		Payload: &pb.PullStreamResponse_Packet{Packet: marshalPacket(pkt)},
	})
}

// WriteTrailer sends the trailer to the pull client, signalling end of stream.
func (m *ServerMuxer) WriteTrailer(_ context.Context, upstreamError error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errServerMuxerTrailerDup
	}

	m.closed = true

	errStr := ""
	if upstreamError != nil {
		errStr = upstreamError.Error()
	}

	return m.stream.Send(&pb.PullStreamResponse{
		Payload: &pb.PullStreamResponse_Trailer{Trailer: &pb.StreamTrailer{Error: errStr}},
	})
}

// Close is a no-op; the gRPC server stream lifecycle is controlled by the handler return.
func (m *ServerMuxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true

	return nil
}
