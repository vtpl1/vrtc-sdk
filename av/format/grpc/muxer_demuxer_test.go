package grpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const internalBufSize = 1024 * 1024

func internalTestH264CodecData(t *testing.T) h264parser.CodecData {
	t.Helper()

	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps, pps)
	if err != nil {
		t.Fatalf("create test H264 codec data: %v", err)
	}

	return cd
}

func internalTestStreams(t *testing.T) []av.Stream {
	t.Helper()

	return []av.Stream{{Idx: 0, Codec: internalTestH264CodecData(t)}}
}

func setupInternalTestServer(
	t *testing.T,
	register func(gs *ggrpc.Server),
) (*ggrpc.ClientConn, func()) {
	t.Helper()

	lis := bufconn.Listen(internalBufSize)
	gs := ggrpc.NewServer()
	register(gs)

	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Logf("grpc serve error: %v", err)
		}
	}()

	conn, err := ggrpc.NewClient(
		"passthrough://bufconn",
		ggrpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		ggrpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		gs.GracefulStop()
		_ = lis.Close()
	}

	return conn, cleanup
}

func TestClientMuxer_WritePacketBeforeHeader(t *testing.T) {
	mux := NewClientMuxer(nil, "source")

	err := mux.WritePacket(context.Background(), av.Packet{})
	if !errors.Is(err, errNoHeader) {
		t.Fatalf("WritePacket error = %v, want errNoHeader", err)
	}
}

func TestClientMuxer_WriteTrailerBeforeHeader(t *testing.T) {
	mux := NewClientMuxer(nil, "source")

	err := mux.WriteTrailer(context.Background(), nil)
	if !errors.Is(err, errTrailerNoHeader) {
		t.Fatalf("WriteTrailer error = %v, want errTrailerNoHeader", err)
	}
}

func TestClientMuxer_WriteHeaderAfterClose(t *testing.T) {
	mux := NewClientMuxer(nil, "source")
	_ = mux.Close()

	err := mux.WriteHeader(context.Background(), nil)
	if !errors.Is(err, errClientMuxerClosed) {
		t.Fatalf("WriteHeader error = %v, want errClientMuxerClosed", err)
	}
}

func TestClientMuxer_WriteHeaderTwice(t *testing.T) {
	srv := NewServer(nil, nil)
	conn, cleanup := setupInternalTestServer(t, func(gs *ggrpc.Server) {
		pb.RegisterAVTransportServiceServer(gs, srv)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mux := NewClientMuxer(conn, "source")
	if err := mux.WriteHeader(ctx, internalTestStreams(t)); err != nil {
		t.Fatalf("first WriteHeader: %v", err)
	}

	err := mux.WriteHeader(ctx, internalTestStreams(t))
	if !errors.Is(err, errHeaderAlreadyWritten) {
		t.Fatalf("second WriteHeader error = %v, want errHeaderAlreadyWritten", err)
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer cleanup: %v", err)
	}
}

func TestClientMuxer_WriteTrailerTwice(t *testing.T) {
	srv := NewServer(nil, nil)
	conn, cleanup := setupInternalTestServer(t, func(gs *ggrpc.Server) {
		pb.RegisterAVTransportServiceServer(gs, srv)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mux := NewClientMuxer(conn, "source")
	if err := mux.WriteHeader(ctx, internalTestStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("first WriteTrailer: %v", err)
	}

	err := mux.WriteTrailer(ctx, nil)
	if !errors.Is(err, errTrailerCalledTwice) {
		t.Fatalf("second WriteTrailer error = %v, want errTrailerCalledTwice", err)
	}
}

func TestClientDemuxer_ReadPacketBeforeGetCodecs(t *testing.T) {
	dmx := NewClientDemuxer(nil, "source", "consumer")

	_, err := dmx.ReadPacket(context.Background())
	if !errors.Is(err, errReadBeforeGetCodecs) {
		t.Fatalf("ReadPacket error = %v, want errReadBeforeGetCodecs", err)
	}
}

type malformedPullServer struct {
	pb.UnimplementedAVTransportServiceServer
}

func (s *malformedPullServer) PushStream(pb.AVTransportService_PushStreamServer) error {
	return status.Error(12, "unimplemented")
}

func (s *malformedPullServer) PullStream(
	_ *pb.PullStreamRequest,
	stream pb.AVTransportService_PullStreamServer,
) error {
	return stream.Send(&pb.PullStreamResponse{
		Payload: &pb.PullStreamResponse_Packet{Packet: marshalPacket(av.Packet{Idx: 0, CodecType: av.H264})},
	})
}

func TestClientDemuxer_GetCodecs_UnexpectedHeader(t *testing.T) {
	conn, cleanup := setupInternalTestServer(t, func(gs *ggrpc.Server) {
		pb.RegisterAVTransportServiceServer(gs, &malformedPullServer{})
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dmx := NewClientDemuxer(conn, "source", "consumer")
	_, err := dmx.GetCodecs(ctx)
	if !errors.Is(err, errUnexpectedHeader) {
		t.Fatalf("GetCodecs error = %v, want errUnexpectedHeader", err)
	}
}

func TestClientDemuxer_ReadPacket_RemoteTrailerError(t *testing.T) {
	srv := NewServer(nil, func(ctx context.Context, _, _ string, mux av.MuxCloser) error {
		if err := mux.WriteHeader(ctx, internalTestStreams(t)); err != nil {
			return err
		}

		return mux.WriteTrailer(ctx, errors.New("upstream boom"))
	})
	conn, cleanup := setupInternalTestServer(t, func(gs *ggrpc.Server) {
		pb.RegisterAVTransportServiceServer(gs, srv)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dmx := NewClientDemuxer(conn, "source", "consumer")
	defer dmx.Close()

	if _, err := dmx.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	_, err := dmx.ReadPacket(ctx)
	if !errors.Is(err, errRemoteError) {
		t.Fatalf("ReadPacket error = %v, want errRemoteError", err)
	}
	if !strings.Contains(err.Error(), "upstream boom") {
		t.Fatalf("ReadPacket error = %q, expected upstream error message", err.Error())
	}
}

func TestClientDemuxer_ReadPacket_ContextCanceled(t *testing.T) {
	srv := NewServer(nil, func(ctx context.Context, _, _ string, mux av.MuxCloser) error {
		if err := mux.WriteHeader(ctx, internalTestStreams(t)); err != nil {
			return err
		}

		<-ctx.Done()
		return nil
	})
	conn, cleanup := setupInternalTestServer(t, func(gs *ggrpc.Server) {
		pb.RegisterAVTransportServiceServer(gs, srv)
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dmx := NewClientDemuxer(conn, "source", "consumer")
	defer dmx.Close()

	if _, err := dmx.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	readCtx, readCancel := context.WithCancel(context.Background())
	readCancel()

	_, err := dmx.ReadPacket(readCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadPacket error = %v, want context.Canceled", err)
	}
}
