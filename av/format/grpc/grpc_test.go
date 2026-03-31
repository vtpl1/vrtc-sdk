package grpc_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	avgrpc "github.com/vtpl1/vrtc-sdk/av/format/grpc"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

// testH264CodecData returns a minimal but valid H264 CodecData for testing.
// Uses a minimal SPS+PPS that produces a valid AVCDecoderConfRecord.
func testH264CodecData(t *testing.T) h264parser.CodecData {
	t.Helper()
	// Minimal SPS for baseline profile, 320x240
	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps, pps)
	if err != nil {
		t.Fatalf("create test H264 codec data: %v", err)
	}
	return cd
}

func testStreams(t *testing.T) []av.Stream {
	t.Helper()
	return []av.Stream{
		{Idx: 0, Codec: testH264CodecData(t)},
	}
}

func testPacket() av.Packet {
	return av.Packet{
		KeyFrame:        true,
		IsDiscontinuity: false,
		Idx:             0,
		CodecType:       av.H264,
		FrameID:         42,
		DTS:             100 * time.Millisecond,
		PTSOffset:       33 * time.Millisecond,
		Duration:        33 * time.Millisecond,
		WallClockTime:   time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		Data:            []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x84, 0x00},
		Analytics: &av.FrameAnalytics{
			SiteID:    1,
			ChannelID: 2,
			FramePTS:  42,
			RefWidth:  320,
			RefHeight: 240,
			Objects: []*av.Detection{
				{X: 10, Y: 20, W: 30, H: 40, ClassID: 0, Confidence: 95, TrackID: 1},
			},
		},
	}
}

// setupTestServer creates an in-process gRPC server with bufconn for testing.
func setupTestServer(t *testing.T, srv *avgrpc.Server) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	gs := grpc.NewServer()
	pb.RegisterAVTransportServiceServer(gs, srv)

	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Logf("grpc serve error: %v", err)
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough://bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}

	return conn, func() {
		conn.Close()
		gs.GracefulStop()
		lis.Close()
	}
}

func TestPushStream(t *testing.T) {
	// Channel to receive the demuxer created by the push handler.
	dmxCh := make(chan av.DemuxCloser, 1)

	srv := avgrpc.NewServer(
		func(_ context.Context, _ string, dmx av.DemuxCloser) {
			dmxCh <- dmx
		},
		nil,
	)

	conn, cleanup := setupTestServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streams := testStreams(t)
	pkt := testPacket()

	// Client side: push packets.
	mux := avgrpc.NewClientMuxer(conn, "test-source")
	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := mux.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Server side: read from demuxer.
	var dmx av.DemuxCloser
	select {
	case dmx = <-dmxCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for push handler")
	}

	codecs, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}
	if len(codecs) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(codecs))
	}
	if codecs[0].Codec.Type() != av.H264 {
		t.Fatalf("expected H264, got %s", codecs[0].Codec.Type())
	}

	got, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if got.FrameID != pkt.FrameID {
		t.Errorf("FrameID: got %d, want %d", got.FrameID, pkt.FrameID)
	}
	if got.DTS != pkt.DTS {
		t.Errorf("DTS: got %v, want %v", got.DTS, pkt.DTS)
	}
	if !got.KeyFrame {
		t.Error("expected KeyFrame=true")
	}
	if got.Analytics == nil {
		t.Fatal("expected analytics, got nil")
	}
	if got.Analytics.SiteID != 1 {
		t.Errorf("Analytics.SiteID: got %d, want 1", got.Analytics.SiteID)
	}
	if len(got.Analytics.Objects) != 1 {
		t.Fatalf("expected 1 detection, got %d", len(got.Analytics.Objects))
	}

	// Close the muxer (sends trailer).
	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Demuxer should get EOF.
	_, err = dmx.ReadPacket(ctx)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after trailer, got %v", err)
	}
}

func TestPullStream(t *testing.T) {
	streams := testStreams(t)
	pkt := testPacket()

	// The pull handler writes header + packet + trailer to the muxer.
	srv := avgrpc.NewServer(
		nil,
		func(ctx context.Context, _, _ string, mux av.MuxCloser) error {
			if err := mux.WriteHeader(ctx, streams); err != nil {
				return err
			}
			if err := mux.WritePacket(ctx, pkt); err != nil {
				return err
			}
			return mux.WriteTrailer(ctx, nil)
		},
	)

	conn, cleanup := setupTestServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Client side: pull packets.
	dmx := avgrpc.NewClientDemuxer(conn, "test-source", "test-consumer")
	defer dmx.Close()

	codecs, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}
	if len(codecs) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(codecs))
	}
	if codecs[0].Codec.Type() != av.H264 {
		t.Fatalf("expected H264, got %s", codecs[0].Codec.Type())
	}

	got, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if got.FrameID != pkt.FrameID {
		t.Errorf("FrameID: got %d, want %d", got.FrameID, pkt.FrameID)
	}
	if got.DTS != pkt.DTS {
		t.Errorf("DTS: got %v, want %v", got.DTS, pkt.DTS)
	}
	if !got.KeyFrame {
		t.Error("expected KeyFrame=true")
	}
	if got.Analytics == nil {
		t.Fatal("expected analytics, got nil")
	}
	if len(got.Analytics.Objects) != 1 {
		t.Fatalf("expected 1 detection, got %d", len(got.Analytics.Objects))
	}

	// Next read should get EOF from trailer.
	_, err = dmx.ReadPacket(ctx)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after trailer, got %v", err)
	}
}

func TestServerDemuxerPauser(t *testing.T) {
	dmxCh := make(chan av.DemuxCloser, 1)

	srv := avgrpc.NewServer(
		func(_ context.Context, _ string, dmx av.DemuxCloser) {
			dmxCh <- dmx
		},
		nil,
	)

	conn, cleanup := setupTestServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streams := testStreams(t)
	pkt := testPacket()

	mux := avgrpc.NewClientMuxer(conn, "pause-test")
	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	var dmx av.DemuxCloser
	select {
	case dmx = <-dmxCh:
	case <-ctx.Done():
		t.Fatal("timeout waiting for push handler")
	}

	// Type-assert Pauser.
	pauser, ok := dmx.(av.Pauser)
	if !ok {
		t.Fatal("ServerDemuxer does not implement av.Pauser")
	}

	if pauser.IsPaused() {
		t.Error("should not be paused initially")
	}

	// Pause, then send a packet.
	if err := pauser.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !pauser.IsPaused() {
		t.Error("should be paused after Pause()")
	}

	if err := mux.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// ReadPacket should block while paused. Verify with a short timeout.
	shortCtx, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer shortCancel()

	_, err := dmx.ReadPacket(shortCtx)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded while paused, got %v", err)
	}

	// Resume and read should succeed.
	if err := pauser.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if pauser.IsPaused() {
		t.Error("should not be paused after Resume()")
	}

	got, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket after resume: %v", err)
	}
	if got.FrameID != pkt.FrameID {
		t.Errorf("FrameID: got %d, want %d", got.FrameID, pkt.FrameID)
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}
}

func TestClientDemuxerPauserAndSeeker(t *testing.T) {
	streams := testStreams(t)
	pkt := testPacket()

	// Track pause/seek calls.
	var pauseCalled, resumeCalled bool
	var seekPos time.Duration

	srv := avgrpc.NewServer(
		nil,
		func(ctx context.Context, _, _ string, mux av.MuxCloser) error {
			if err := mux.WriteHeader(ctx, streams); err != nil {
				return err
			}
			if err := mux.WritePacket(ctx, pkt); err != nil {
				return err
			}
			return mux.WriteTrailer(ctx, nil)
		},
		avgrpc.WithPauseHandler(func(_ context.Context, _ string, pause bool) error {
			if pause {
				pauseCalled = true
			} else {
				resumeCalled = true
			}
			return nil
		}),
		avgrpc.WithSeekHandler(func(_ context.Context, _ string, pos time.Duration) (time.Duration, error) {
			seekPos = pos
			// Simulate keyframe alignment: round down to nearest second.
			return pos.Truncate(time.Second), nil
		}),
	)

	conn, cleanup := setupTestServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dmx := avgrpc.NewClientDemuxer(conn, "ctrl-test", "consumer-1")
	defer dmx.Close()

	if _, err := dmx.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Use interface type for type assertions.
	var demuxer av.DemuxCloser = dmx

	// Test Pauser.
	pauser, ok := demuxer.(av.Pauser)
	if !ok {
		t.Fatal("ClientDemuxer does not implement av.Pauser")
	}

	if err := pauser.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !pauseCalled {
		t.Error("server pause handler not called")
	}
	if !pauser.IsPaused() {
		t.Error("IsPaused should be true after Pause")
	}

	if err := pauser.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !resumeCalled {
		t.Error("server resume handler not called")
	}
	if pauser.IsPaused() {
		t.Error("IsPaused should be false after Resume")
	}

	// Test TimeSeeker.
	seeker, ok := demuxer.(av.TimeSeeker)
	if !ok {
		t.Fatal("ClientDemuxer does not implement av.TimeSeeker")
	}

	actual, err := seeker.SeekToTime(ctx, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("SeekToTime: %v", err)
	}
	if seekPos != 1500*time.Millisecond {
		t.Errorf("server saw pos %v, want %v", seekPos, 1500*time.Millisecond)
	}
	if actual != 1*time.Second {
		t.Errorf("actual position: got %v, want %v", actual, 1*time.Second)
	}
}

func TestCodecMarshalRoundTrip(t *testing.T) {
	streams := testStreams(t)

	infos := avgrpc.MarshalStreamsForTest(streams)

	got, err := avgrpc.UnmarshalStreamsForTest(infos)
	if err != nil {
		t.Fatalf("unmarshalStreams: %v", err)
	}

	if len(got) != len(streams) {
		t.Fatalf("stream count: got %d, want %d", len(got), len(streams))
	}

	for i := range streams {
		if got[i].Idx != streams[i].Idx {
			t.Errorf("stream[%d].Idx: got %d, want %d", i, got[i].Idx, streams[i].Idx)
		}
		if got[i].Codec.Type() != streams[i].Codec.Type() {
			t.Errorf("stream[%d].Codec.Type: got %s, want %s", i, got[i].Codec.Type(), streams[i].Codec.Type())
		}
	}

	// Verify H264-specific properties survived round-trip.
	origVCD := streams[0].Codec.(av.VideoCodecData)
	gotVCD := got[0].Codec.(av.VideoCodecData)
	if gotVCD.Width() != origVCD.Width() || gotVCD.Height() != origVCD.Height() {
		t.Errorf("H264 dimensions: got %dx%d, want %dx%d",
			gotVCD.Width(), gotVCD.Height(), origVCD.Width(), origVCD.Height())
	}
}
