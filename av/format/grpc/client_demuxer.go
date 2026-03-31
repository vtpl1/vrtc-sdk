package grpc

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
	"google.golang.org/grpc"
)

// Compile-time interface checks.
var (
	_ av.DemuxCloser = (*ClientDemuxer)(nil)
	_ av.Pauser      = (*ClientDemuxer)(nil)
	_ av.TimeSeeker  = (*ClientDemuxer)(nil)
)

// ClientDemuxer implements av.DemuxCloser by pulling packets from a remote gRPC
// server via the PullStream server-streaming RPC. It also implements av.Pauser
// and av.TimeSeeker, forwarding control operations as unary RPCs.
type ClientDemuxer struct {
	conn       *grpc.ClientConn
	sourceID   string
	consumerID string
	stream     pb.AVTransportService_PullStreamClient
	codecs     []av.Stream
	cancel     context.CancelFunc
	paused     atomic.Bool
}

// NewClientDemuxer creates a new ClientDemuxer for the given source.
func NewClientDemuxer(conn *grpc.ClientConn, sourceID, consumerID string) *ClientDemuxer {
	return &ClientDemuxer{
		conn:       conn,
		sourceID:   sourceID,
		consumerID: consumerID,
	}
}

// NewClientDemuxerFactory returns an av.DemuxerFactory that creates ClientDemuxers.
// Each call uses sourceID as-is; consumerID is the fixed value passed to the factory.
func NewClientDemuxerFactory(conn *grpc.ClientConn, consumerID string) av.DemuxerFactory {
	return func(_ context.Context, sourceID string) (av.DemuxCloser, error) {
		return NewClientDemuxer(conn, sourceID, consumerID), nil
	}
}

// GetCodecs opens the PullStream RPC and reads the initial StreamHeader from the server.
func (d *ClientDemuxer) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	if d.codecs != nil {
		return d.codecs, nil
	}

	sctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel

	client := pb.NewAVTransportServiceClient(d.conn)

	stream, err := client.PullStream(sctx, &pb.PullStreamRequest{
		SourceId:   d.sourceID,
		ConsumerId: d.consumerID,
	})
	if err != nil {
		cancel()

		return nil, fmt.Errorf("grpc: open PullStream: %w", err)
	}

	d.stream = stream

	resp, err := stream.Recv()
	if err != nil {
		cancel()

		return nil, fmt.Errorf("grpc: receive header: %w", err)
	}

	hdr, ok := resp.GetPayload().(*pb.PullStreamResponse_Header)
	if !ok {
		cancel()

		return nil, fmt.Errorf("%w: got %T", errUnexpectedHeader, resp.GetPayload())
	}

	d.codecs, err = unmarshalStreams(hdr.Header.GetStreams())
	if err != nil {
		cancel()

		return nil, fmt.Errorf("grpc: unmarshal streams: %w", err)
	}

	return d.codecs, nil
}

// ReadPacket reads the next AV packet from the server stream.
// Returns io.EOF when the stream ends. The caller's ctx is honored: if it is
// cancelled before the next message arrives, ReadPacket returns the context error.
func (d *ClientDemuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if d.stream == nil {
		return av.Packet{}, errReadBeforeGetCodecs
	}

	// Recv blocks on the stream's internal context (from GetCodecs). To also
	// honor the per-call ctx we race Recv against ctx.Done.
	type recvResult struct {
		resp *pb.PullStreamResponse
		err  error
	}

	ch := make(chan recvResult, 1)

	go func() {
		resp, err := d.stream.Recv()
		ch <- recvResult{resp, err}
	}()

	select {
	case <-ctx.Done():
		return av.Packet{}, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return av.Packet{}, r.err
		}

		return d.decodeResponse(r.resp)
	}
}

// Pause implements av.Pauser. Sends a PauseStream RPC to the server.
func (d *ClientDemuxer) Pause(ctx context.Context) error {
	client := pb.NewAVTransportServiceClient(d.conn)

	_, err := client.PauseStream(ctx, &pb.PauseStreamRequest{SourceId: d.sourceID})
	if err != nil {
		return fmt.Errorf("grpc: pause: %w", err)
	}

	d.paused.Store(true)

	return nil
}

// Resume implements av.Pauser. Sends a ResumeStream RPC to the server.
func (d *ClientDemuxer) Resume(ctx context.Context) error {
	client := pb.NewAVTransportServiceClient(d.conn)

	_, err := client.ResumeStream(ctx, &pb.ResumeStreamRequest{SourceId: d.sourceID})
	if err != nil {
		return fmt.Errorf("grpc: resume: %w", err)
	}

	d.paused.Store(false)

	return nil
}

// IsPaused implements av.Pauser.
func (d *ClientDemuxer) IsPaused() bool {
	return d.paused.Load()
}

// SeekToTime implements av.TimeSeeker. Sends a SeekStream RPC to the server.
// The returned duration is the actual position the server landed on.
func (d *ClientDemuxer) SeekToTime(ctx context.Context, pos time.Duration) (time.Duration, error) {
	client := pb.NewAVTransportServiceClient(d.conn)

	resp, err := client.SeekStream(ctx, &pb.SeekStreamRequest{
		SourceId:   d.sourceID,
		PositionNs: int64(pos),
	})
	if err != nil {
		return 0, fmt.Errorf("grpc: seek: %w", err)
	}

	return time.Duration(resp.GetActualPositionNs()), nil
}

// Close cancels the pull stream.
func (d *ClientDemuxer) Close() error {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}

	return nil
}

func (d *ClientDemuxer) decodeResponse(resp *pb.PullStreamResponse) (av.Packet, error) {
	switch p := resp.GetPayload().(type) {
	case *pb.PullStreamResponse_Packet:
		return unmarshalPacket(p.Packet)
	case *pb.PullStreamResponse_Trailer:
		if p.Trailer.GetError() != "" {
			return av.Packet{}, fmt.Errorf("%w: %s", errRemoteError, p.Trailer.GetError())
		}

		return av.Packet{}, io.EOF
	default:
		return av.Packet{}, fmt.Errorf("%w: %T", errUnexpectedPayload, resp.GetPayload())
	}
}
