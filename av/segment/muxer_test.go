package segment_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/relayhub"
	"github.com/vtpl1/vrtc-sdk/av/segment"
)

// minimalAVCRecord is a synthetic AVCDecoderConfigurationRecord for
// a 320x240 Baseline-profile H.264 stream (profile 66, level 30).
var minimalAVCRecord = []byte{
	0x01,             // configurationVersion
	0x42, 0x00, 0x1E, // profile_idc, constraint_flags, level_idc
	0xFF,       // lengthSizeMinusOne = 3
	0xE1,       // numSequenceParameterSets = 1
	0x00, 0x0F, // SPS length
	0x67, 0x42, 0x00, 0x1E,
	0xAC, 0xD9, 0x40, 0xA0,
	0x3D, 0xA1, 0x00, 0x00,
	0x03, 0x00, 0x00,
	0x01,       // numPictureParameterSets = 1
	0x00, 0x04, // PPS length
	0x68, 0xCE, 0x38, 0x80,
}

func makeH264Codec(t *testing.T) h264parser.CodecData {
	t.Helper()

	c, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(minimalAVCRecord)
	if err != nil {
		t.Fatalf("h264parser: %v", err)
	}

	return c
}

func makeStreams(t *testing.T) []av.Stream {
	t.Helper()

	return []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}
}

func makeKeyframe(dts time.Duration) av.Packet {
	return av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       dts,
		Duration:  33 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xDE, 0xAD},
		CodecType: av.H264,
	}
}

func TestNewSegmentMuxer_CreatesFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "seg.mp4")

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	if mux.BytesWritten() != 0 {
		t.Errorf("BytesWritten before any write = %d, want 0", mux.BytesWritten())
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewSegmentMuxer_InvalidPath(t *testing.T) {
	t.Parallel()

	_, err := segment.NewSegmentMuxer(
		filepath.Join(t.TempDir(), "no", "such", "dir", "seg.mp4"),
		time.Now(), segment.ProfileSSD, 0, 0, nil, nil,
	)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestSegmentMuxer_FullLifecycle(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "lifecycle.mp4")
	startTime := time.Now().UTC()

	var mu sync.Mutex

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		mu.Lock()
		gotInfo = info
		mu.Unlock()
	}

	mux, err := segment.NewSegmentMuxer(path, startTime, segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()
	streams := makeStreams(t)

	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write two keyframes to trigger a fragment flush.
	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(33*time.Millisecond)); err != nil {
		t.Fatalf("WritePacket(kf1): %v", err)
	}

	if mux.BytesWritten() == 0 {
		t.Error("expected BytesWritten > 0 after writing packets")
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	info := gotInfo
	mu.Unlock()

	if info.Path != path {
		t.Errorf("CloseInfo.Path = %q, want %q", info.Path, path)
	}

	if info.Start != startTime {
		t.Errorf("CloseInfo.Start = %v, want %v", info.Start, startTime)
	}

	if info.End.Before(startTime) {
		t.Error("CloseInfo.End should be after Start")
	}

	if info.SizeBytes <= 0 {
		t.Errorf("CloseInfo.SizeBytes = %d, want > 0", info.SizeBytes)
	}

	// The file is a valid fMP4, so ValidationError should be nil.
	if info.ValidationError != nil {
		t.Errorf("CloseInfo.ValidationError = %v, want nil", info.ValidationError)
	}
}

func TestSegmentMuxer_CloseAfterWriteTrailerFinalizes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "trailer-first.mp4")
	startTime := time.Now().UTC()

	var mu sync.Mutex
	closeCalls := 0
	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		mu.Lock()
		closeCalls++
		gotInfo = info
		mu.Unlock()
	}

	mux, err := segment.NewSegmentMuxer(path, startTime, segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()
	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(33*time.Millisecond)); err != nil {
		t.Fatalf("WritePacket(kf1): %v", err)
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	info := gotInfo
	gotCalls := closeCalls
	mu.Unlock()

	if gotCalls != 1 {
		t.Fatalf("onClose calls = %d, want 1", gotCalls)
	}

	if info.Path != path {
		t.Errorf("CloseInfo.Path = %q, want %q", info.Path, path)
	}

	if info.Start != startTime {
		t.Errorf("CloseInfo.Start = %v, want %v", info.Start, startTime)
	}

	if info.End.Before(startTime) {
		t.Error("CloseInfo.End should be after Start")
	}

	if info.SizeBytes <= 0 {
		t.Errorf("CloseInfo.SizeBytes = %d, want > 0", info.SizeBytes)
	}

	if info.ValidationError != nil {
		t.Errorf("CloseInfo.ValidationError = %v, want nil", info.ValidationError)
	}
}

func TestSegmentMuxer_AnalyticsFlags_NoAnalytics(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "noanalytics.mp4")

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()

	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Packets without analytics.
	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if gotInfo.HasMotion {
		t.Error("expected HasMotion=false")
	}

	if gotInfo.HasObjects {
		t.Error("expected HasObjects=false")
	}
}

func TestSegmentMuxer_AnalyticsFlags_MotionOnly(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "motion.mp4")

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()

	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Packet with analytics but no objects -> motion only.
	pkt := makeKeyframe(0)
	pkt.Analytics = &av.FrameAnalytics{}

	if err := mux.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !gotInfo.HasMotion {
		t.Error("expected HasMotion=true")
	}

	if gotInfo.HasObjects {
		t.Error("expected HasObjects=false")
	}
}

func TestSegmentMuxer_AnalyticsFlags_WithObjects(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "objects.mp4")

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()

	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	pkt := makeKeyframe(0)
	pkt.Analytics = &av.FrameAnalytics{
		Objects: []*av.Detection{{ClassID: 1, Confidence: 90}},
	}

	if err := mux.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !gotInfo.HasMotion {
		t.Error("expected HasMotion=true")
	}

	if !gotInfo.HasObjects {
		t.Error("expected HasObjects=true")
	}
}

func TestSegmentMuxer_CloseWithoutOnClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nocallback.mp4")

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()

	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Close with nil onClose should not panic.
	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentMuxer_WithRingBuffer(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ring.mp4")
	ring := segment.NewRingBuffer(10 * time.Second)

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, ring, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()

	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := mux.WritePacket(ctx, makeKeyframe(33*time.Millisecond)); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Ring buffer should have received fragments from the tee writer.
	if ring.Len() == 0 {
		t.Error("expected ring buffer to have fragments after writes")
	}

	if ring.SizeBytes() == 0 {
		t.Error("expected ring buffer SizeBytes > 0")
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if gotInfo.SizeBytes <= 0 {
		t.Errorf("CloseInfo.SizeBytes = %d, want > 0", gotInfo.SizeBytes)
	}

	if gotInfo.ValidationError != nil {
		t.Errorf("CloseInfo.ValidationError = %v, want nil", gotInfo.ValidationError)
	}
}

func TestSegmentMuxer_RingBufferStoresCompleteFragments(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ring-fragments.mp4")
	ring := segment.NewRingBuffer(10 * time.Second)

	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, ring, nil)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()
	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	t0 := time.Now().UTC()
	pkt0 := makeKeyframe(0)
	pkt0.WallClockTime = t0

	if err := mux.WritePacket(ctx, pkt0); err != nil {
		t.Fatalf("WritePacket(pkt0): %v", err)
	}

	pkt1 := makeKeyframe(33 * time.Millisecond)
	pkt1.WallClockTime = t0.Add(33 * time.Millisecond)

	if err := mux.WritePacket(ctx, pkt1); err != nil {
		t.Fatalf("WritePacket(pkt1): %v", err)
	}

	frags := ring.ReadFrom(0)
	if len(frags) != 1 {
		t.Fatalf("expected 1 completed fragment before Close, got %d", len(frags))
	}

	if frags[0].DTS != 0 {
		t.Fatalf("fragment DTS: got %v, want 0", frags[0].DTS)
	}

	if frags[0].Duration != 33*time.Millisecond {
		t.Fatalf("fragment Duration: got %v, want 33ms", frags[0].Duration)
	}

	if !frags[0].KeyFrame {
		t.Fatal("expected fragment to be marked as keyframe")
	}

	if len(frags[0].Data) == 0 {
		t.Fatal("expected fragment bytes to be captured")
	}

	if !frags[0].Timestamp.Equal(t0) {
		t.Fatalf("fragment Timestamp: got %v, want %v", frags[0].Timestamp, t0)
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	frags = ring.ReadFrom(33 * time.Millisecond)
	if len(frags) != 1 {
		t.Fatalf("expected trailing fragment after Close, got %d", len(frags))
	}

	if frags[0].DTS != 33*time.Millisecond {
		t.Fatalf("trailing fragment DTS: got %v, want 33ms", frags[0].DTS)
	}
}

func TestSegmentMuxer_ValidationError_EmptyFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.mp4")

	var gotInfo segment.SegmentCloseInfo

	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	// Create the muxer but close without writing anything meaningful.
	// The fmp4.Muxer will not write header/trailer, resulting in an
	// invalid or empty file.
	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	// Close immediately without writing header — the file will be empty or
	// contain only what fmp4.Muxer writes on Close (WriteTrailer with no header is a no-op).
	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if gotInfo.ValidationError == nil {
		t.Error("expected ValidationError for empty/invalid segment")
	}
}

func TestSegmentMuxer_SizeRotation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.mp4")

	var gotInfo segment.SegmentCloseInfo
	onClose := func(info segment.SegmentCloseInfo) {
		gotInfo = info
	}

	// Set maxSegmentBytes to 1 byte — any keyframe after the first should rotate.
	maxBytes := int64(1)
	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, maxBytes, 0, nil, onClose)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()
	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// First keyframe writes ftyp+moov+first moof+mdat → BytesWritten > 1.
	if err := mux.WritePacket(ctx, makeKeyframe(0)); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	t.Logf("BytesWritten after kf0: %d", mux.BytesWritten())

	// Second keyframe should trigger ErrMuxerRotate since we're over 1 byte.
	err = mux.WritePacket(ctx, makeKeyframe(33*time.Millisecond))
	if err == nil {
		t.Fatal("expected ErrMuxerRotate, got nil")
	}
	if !errors.Is(err, relayhub.ErrMuxerRotate) {
		t.Fatalf("expected ErrMuxerRotate, got %v", err)
	}

	// Close the muxer — should finalize normally.
	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if gotInfo.SizeBytes <= 0 {
		t.Errorf("expected SizeBytes > 0, got %d", gotInfo.SizeBytes)
	}
}

func TestSegmentMuxer_NoRotationWhenDisabled(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "norotate.mp4")

	// maxSegmentBytes=0 means no rotation.
	mux, err := segment.NewSegmentMuxer(path, time.Now(), segment.ProfileSSD, 0, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewSegmentMuxer: %v", err)
	}

	ctx := context.Background()
	if err := mux.WriteHeader(ctx, makeStreams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write many keyframes — none should trigger rotation.
	for i := range 10 {
		if err := mux.WritePacket(ctx, makeKeyframe(time.Duration(i)*33*time.Millisecond)); err != nil {
			t.Fatalf("WritePacket(%d): %v", i, err)
		}
	}

	if err := mux.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
