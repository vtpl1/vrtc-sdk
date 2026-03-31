package grpc

import (
	"context"
	"fmt"
	"sync"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
	"google.golang.org/grpc"
)

// ClientMuxer implements av.MuxCloser by pushing packets to a remote gRPC server
// via the PushStream client-streaming RPC.
type ClientMuxer struct {
	conn          *grpc.ClientConn
	sourceID      string
	stream        pb.AVTransportService_PushStreamClient
	cancel        context.CancelFunc
	mu            sync.Mutex
	headerWritten bool
	closed        bool
}

// NewClientMuxer creates a new ClientMuxer for the given source.
func NewClientMuxer(conn *grpc.ClientConn, sourceID string) *ClientMuxer {
	return &ClientMuxer{
		conn:     conn,
		sourceID: sourceID,
	}
}

// NewClientMuxerFactory returns an av.MuxerFactory that creates ClientMuxers.
// The consumerID argument to the factory is used as the sourceID.
func NewClientMuxerFactory(conn *grpc.ClientConn) av.MuxerFactory {
	return func(_ context.Context, consumerID string) (av.MuxCloser, error) {
		return NewClientMuxer(conn, consumerID), nil
	}
}

// WriteHeader opens the PushStream RPC and sends the initial StreamHeader.
// Must be called exactly once before any WritePacket.
func (m *ClientMuxer) WriteHeader(ctx context.Context, streams []av.Stream) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return errClientMuxerClosed
	}

	if m.headerWritten {
		return errHeaderAlreadyWritten
	}

	infos := marshalStreams(streams)

	sctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	client := pb.NewAVTransportServiceClient(m.conn)

	stream, err := client.PushStream(sctx)
	if err != nil {
		cancel()
		m.cancel = nil

		return fmt.Errorf("grpc: open PushStream: %w", err)
	}

	m.stream = stream
	m.headerWritten = true

	return stream.Send(&pb.PushStreamRequest{
		SourceId: m.sourceID,
		Payload:  &pb.PushStreamRequest_Header{Header: &pb.StreamHeader{Streams: infos}},
	})
}

// WritePacket sends a single AV packet over the gRPC stream.
func (m *ClientMuxer) WritePacket(_ context.Context, pkt av.Packet) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.headerWritten {
		return errNoHeader
	}

	return m.stream.Send(&pb.PushStreamRequest{
		SourceId: m.sourceID,
		Payload:  &pb.PushStreamRequest_Packet{Packet: marshalPacket(pkt)},
	})
}

// WriteTrailer sends the trailer message and closes the client-streaming RPC.
func (m *ClientMuxer) WriteTrailer(_ context.Context, upstreamError error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.headerWritten {
		return errTrailerNoHeader
	}

	if m.closed {
		return errTrailerCalledTwice
	}

	m.closed = true

	errStr := ""
	if upstreamError != nil {
		errStr = upstreamError.Error()
	}

	if err := m.stream.Send(&pb.PushStreamRequest{
		SourceId: m.sourceID,
		Payload:  &pb.PushStreamRequest_Trailer{Trailer: &pb.StreamTrailer{Error: errStr}},
	}); err != nil {
		return fmt.Errorf("grpc: send trailer: %w", err)
	}

	resp, err := m.stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("grpc: close stream: %w", err)
	}

	if resp.GetError() != "" {
		return fmt.Errorf("%w: %s", errRemoteError, resp.GetError())
	}

	return nil
}

// Close cancels the underlying gRPC stream if still open.
func (m *ClientMuxer) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	return nil
}
