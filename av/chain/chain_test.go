package chain_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/chain"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// minimalAVCRecord is a synthetic AVCDecoderConfigurationRecord for
// a 320x240 Baseline-profile H.264 stream.
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

const frameDur = 33 * time.Millisecond

// makeSegment creates a complete fMP4 segment in memory with nPackets
// video keyframes, each with the given duration. Returns raw bytes.
func makeSegment(t *testing.T, nPackets int) []byte {
	t.Helper()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	streams := []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	for i := range nPackets {
		pkt := av.Packet{
			KeyFrame:  true,
			Idx:       0,
			DTS:       time.Duration(i) * frameDur,
			Duration:  frameDur,
			Data:      []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xDE, 0xAD},
			CodecType: av.H264,
		}

		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket(%d): %v", i, err)
		}
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	return buf.Bytes()
}

// demuxerFromBytes creates an fmp4.Demuxer over raw segment bytes.
func demuxerFromBytes(data []byte) av.DemuxCloser {
	return fmp4.NewDemuxer(bytes.NewReader(data))
}

// readAllPackets reads all packets from a DemuxCloser until io.EOF.
func readAllPackets(t *testing.T, dmx av.DemuxCloser) []av.Packet {
	t.Helper()

	ctx := context.Background()
	var pkts []av.Packet

	for {
		pkt, err := dmx.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			return pkts
		}

		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}

		pkts = append(pkts, pkt)
	}
}

// emptySource is a SegmentSource that returns io.EOF immediately.
type emptySource struct{}

func (emptySource) Next(_ context.Context) (av.DemuxCloser, error) {
	return nil, io.EOF
}

// bytesSource yields fmp4 demuxers from pre-built byte slices.
type bytesSource struct {
	segments [][]byte
	idx      int
}

func (s *bytesSource) Next(_ context.Context) (av.DemuxCloser, error) {
	if s.idx >= len(s.segments) {
		return nil, io.EOF
	}

	data := s.segments[s.idx]
	s.idx++

	return demuxerFromBytes(data), nil
}

// errSource returns a fixed error on Next.
type errSource struct {
	err error
}

func (s *errSource) Next(_ context.Context) (av.DemuxCloser, error) {
	return nil, s.err
}

func TestChainingDemuxer_SingleSegment(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 3)
	first := demuxerFromBytes(seg)

	cd := chain.NewChainingDemuxer(first, emptySource{})
	ctx := context.Background()

	streams, err := cd.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(streams) == 0 {
		t.Fatal("expected at least one stream")
	}

	pkts := readAllPackets(t, cd)

	if len(pkts) != 3 {
		t.Fatalf("expected 3 packets, got %d", len(pkts))
	}

	// Verify DTS is monotonic.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].DTS < pkts[i-1].DTS {
			t.Errorf("DTS not monotonic: pkt[%d].DTS=%v < pkt[%d].DTS=%v",
				i, pkts[i].DTS, i-1, pkts[i-1].DTS)
		}
	}

	if err := cd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestChainingDemuxer_TwoSegments_DTSMonotonic(t *testing.T) {
	t.Parallel()

	seg1 := makeSegment(t, 2) // DTS 0, 33ms
	seg2 := makeSegment(t, 2) // DTS 0, 33ms (original)

	first := demuxerFromBytes(seg1)
	src := &bytesSource{segments: [][]byte{seg2}}

	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	pkts := readAllPackets(t, cd)

	if len(pkts) != 4 {
		t.Fatalf("expected 4 packets, got %d", len(pkts))
	}

	// Verify all DTS are monotonically non-decreasing.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].DTS < pkts[i-1].DTS {
			t.Errorf("DTS not monotonic: pkt[%d].DTS=%v < pkt[%d].DTS=%v",
				i, pkts[i].DTS, i-1, pkts[i-1].DTS)
		}
	}

	// Second segment packets should have DTS >= lastEnd of first segment.
	// First segment: 2 packets at 0, 33ms with 33ms duration → lastEnd = 66ms.
	lastEndSeg1 := pkts[1].DTS + pkts[1].Duration

	if pkts[2].DTS < lastEndSeg1 {
		t.Errorf("second segment first packet DTS=%v should be >= %v", pkts[2].DTS, lastEndSeg1)
	}

	if err := cd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestChainingDemuxer_ThreeSegments(t *testing.T) {
	t.Parallel()

	seg1 := makeSegment(t, 2)
	seg2 := makeSegment(t, 3)
	seg3 := makeSegment(t, 2)

	first := demuxerFromBytes(seg1)
	src := &bytesSource{segments: [][]byte{seg2, seg3}}

	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	pkts := readAllPackets(t, cd)

	if len(pkts) != 7 {
		t.Fatalf("expected 7 packets (2+3+2), got %d", len(pkts))
	}

	// Verify monotonic DTS across all three segments.
	for i := 1; i < len(pkts); i++ {
		if pkts[i].DTS < pkts[i-1].DTS {
			t.Errorf("DTS not monotonic at pkt[%d]: %v < %v",
				i, pkts[i].DTS, pkts[i-1].DTS)
		}
	}

	if err := cd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestChainingDemuxer_SourceError(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 1)
	first := demuxerFromBytes(seg)

	testErr := errors.New("source failure")
	src := &errSource{err: testErr}

	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read the single packet from the first segment.
	if _, err := cd.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket(first): %v", err)
	}

	// Next ReadPacket should trigger source.Next and propagate the error.
	_, err := cd.ReadPacket(ctx)
	if !errors.Is(err, testErr) {
		t.Fatalf("expected source error, got: %v", err)
	}
}

func TestChainingDemuxer_ContextCancellation(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 1)
	first := demuxerFromBytes(seg)

	// Source blocks until context is cancelled.
	src := &funcSource{fn: func(ctx context.Context) (av.DemuxCloser, error) {
		<-ctx.Done()

		return nil, ctx.Err()
	}}

	cd := chain.NewChainingDemuxer(first, src)
	ctx, cancel := context.WithCancel(context.Background())

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Drain the first segment.
	if _, err := cd.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	// Cancel and verify ReadPacket returns context error.
	cancel()

	_, err := cd.ReadPacket(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestChainingDemuxer_Close_NilCur(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 1)
	first := demuxerFromBytes(seg)

	cd := chain.NewChainingDemuxer(first, emptySource{})
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Drain to EOF so cur becomes nil.
	readAllPackets(t, cd)

	// Close on nil cur should not panic.
	if err := cd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestChainingDemuxer_Close_ActiveCur(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 3)
	first := demuxerFromBytes(seg)

	cd := chain.NewChainingDemuxer(first, emptySource{})
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read one packet but don't drain — cur is still active.
	if _, err := cd.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if err := cd.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSliceSource_Empty(t *testing.T) {
	t.Parallel()

	src := chain.SliceSource(nil, func(_ context.Context, _ string) (av.DemuxCloser, error) {
		t.Fatal("open should not be called")

		return nil, nil
	})

	_, err := src.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got: %v", err)
	}
}

func TestSliceSource_ThreeElements(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 1)
	ids := []string{"a", "b", "c"}

	var opened []string

	src := chain.SliceSource(ids, func(_ context.Context, id string) (av.DemuxCloser, error) {
		opened = append(opened, id)

		return demuxerFromBytes(seg), nil
	})

	ctx := context.Background()

	for range 3 {
		dmx, err := src.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}

		_ = dmx.Close()
	}

	// Fourth call should return EOF.
	_, err := src.Next(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF after 3 elements, got: %v", err)
	}

	if len(opened) != 3 || opened[0] != "a" || opened[1] != "b" || opened[2] != "c" {
		t.Errorf("opened = %v, want [a b c]", opened)
	}
}

func TestSliceSource_OpenError(t *testing.T) {
	t.Parallel()

	testErr := errors.New("open failed")

	src := chain.SliceSource([]string{"bad"}, func(_ context.Context, _ string) (av.DemuxCloser, error) {
		return nil, testErr
	})

	_, err := src.Next(context.Background())
	if !errors.Is(err, testErr) {
		t.Fatalf("expected open error, got: %v", err)
	}
}

// funcSource implements chain.SegmentSource using a function.
type funcSource struct {
	fn func(ctx context.Context) (av.DemuxCloser, error)
}

func (f *funcSource) Next(ctx context.Context) (av.DemuxCloser, error) {
	return f.fn(ctx)
}

// ── Seek tests ──────────────────────────────────────────────────────────────

// seekableSource implements chain.SeekableSegmentSource for testing.
type seekableSource struct {
	bytesSource
	segments map[string][]byte // wall-clock key → segment bytes
}

func (s *seekableSource) OpenAt(_ context.Context, ts time.Time) (av.DemuxCloser, error) {
	key := ts.Format(time.RFC3339)

	data, ok := s.segments[key]
	if !ok {
		return nil, io.EOF
	}

	return fmp4.NewDemuxer(bytes.NewReader(data)), nil
}

// minimalAVCRecordLevel40 is a synthetic AVCDecoderConfigurationRecord for
// a 320x240 Baseline-profile H.264 stream at level 4.0. The different level
// produces a different RFC 6381 Tag() ("avc1.420028" vs "avc1.42001E"),
// which streamsChanged detects as a codec change.
var minimalAVCRecordLevel40 = []byte{
	0x01,             // configurationVersion
	0x42, 0x00, 0x28, // profile_idc=66 (Baseline), constraint_flags, level_idc=40 (4.0)
	0xFF,       // lengthSizeMinusOne = 3
	0xE1,       // numSequenceParameterSets = 1
	0x00, 0x0F, // SPS length
	0x67, 0x42, 0x00, 0x28,
	0xAC, 0xD9, 0x40, 0xA0,
	0x3D, 0xA1, 0x00, 0x00,
	0x03, 0x00, 0x00,
	0x01,       // numPictureParameterSets = 1
	0x00, 0x04, // PPS length
	0x68, 0xCE, 0x38, 0x80,
}

func makeH264Level40Codec(t *testing.T) h264parser.CodecData {
	t.Helper()

	c, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(minimalAVCRecordLevel40)
	if err != nil {
		t.Fatalf("h264parser level40: %v", err)
	}

	return c
}

// makeSegmentWithCodec creates a segment using the given codec data.
func makeSegmentWithCodec(t *testing.T, nPackets int, codec av.CodecData) []byte {
	t.Helper()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	streams := []av.Stream{{Idx: 0, Codec: codec}}

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	for i := range nPackets {
		pkt := av.Packet{
			KeyFrame:  true,
			Idx:       0,
			DTS:       time.Duration(i) * frameDur,
			Duration:  frameDur,
			Data:      []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xDE, 0xAD},
			CodecType: av.H264,
		}

		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket(%d): %v", i, err)
		}
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	return buf.Bytes()
}

// ── Codec change detection tests ──────────────────────────────────────────

func TestChainingDemuxer_CodecChange_AtSegmentBoundary(t *testing.T) {
	t.Parallel()

	// Segment 1: Baseline profile H.264
	seg1 := makeSegment(t, 2)
	// Segment 2: High profile H.264 — different Tag()
	seg2 := makeSegmentWithCodec(t, 2, makeH264Level40Codec(t))

	src := &bytesSource{segments: [][]byte{seg2}}
	first := demuxerFromBytes(seg1)
	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read all packets from both segments.
	var allPkts []av.Packet
	for {
		pkt, err := cd.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		allPkts = append(allPkts, pkt)
	}

	// Expect 4 total packets (2 from each segment).
	if len(allPkts) != 4 {
		t.Fatalf("expected 4 packets, got %d", len(allPkts))
	}

	// The first packet from the second segment (index 2) should carry NewCodecs.
	if allPkts[2].NewCodecs == nil {
		t.Fatal("expected NewCodecs on first packet of second segment (codec changed)")
	}

	if len(allPkts[2].NewCodecs) != 1 {
		t.Fatalf("expected 1 stream in NewCodecs, got %d", len(allPkts[2].NewCodecs))
	}

	// Packets 0, 1, 3 should NOT carry NewCodecs.
	for _, idx := range []int{0, 1, 3} {
		if allPkts[idx].NewCodecs != nil {
			t.Errorf("packet %d should not have NewCodecs", idx)
		}
	}
}

func TestChainingDemuxer_NoCodecChange_SameCodec(t *testing.T) {
	t.Parallel()

	// Both segments use the same codec — no NewCodecs should appear.
	seg1 := makeSegment(t, 2)
	seg2 := makeSegment(t, 2)

	src := &bytesSource{segments: [][]byte{seg2}}
	first := demuxerFromBytes(seg1)
	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	for {
		pkt, err := cd.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if pkt.NewCodecs != nil {
			t.Fatal("unexpected NewCodecs when segments have the same codec")
		}
	}
}

func TestChainingDemuxer_Seek_CodecChange(t *testing.T) {
	t.Parallel()

	// Segment 1: Baseline. Segment 2: High profile.
	seg1 := makeSegment(t, 3)
	seg2 := makeSegmentWithCodec(t, 3, makeH264Level40Codec(t))

	wallTime := time.Date(2026, 4, 2, 14, 0, 0, 0, time.UTC)

	src := &seekableSource{
		segments: map[string][]byte{
			wallTime.Format(time.RFC3339): seg2,
		},
	}

	first := demuxerFromBytes(seg1)
	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read one packet from seg1 to establish prevStreams baseline.
	if _, err := cd.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	// Seek to seg2 (different codec).
	if err := cd.Seek(ctx, wallTime, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	// First packet after seek should carry NewCodecs.
	pkt, err := cd.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket after seek: %v", err)
	}

	if pkt.NewCodecs == nil {
		t.Fatal("expected NewCodecs on first packet after seeking to different codec")
	}
}

func TestChainingDemuxer_Seek_NoCodecChange(t *testing.T) {
	t.Parallel()

	// Both segments use the same codec — seek should NOT emit NewCodecs.
	seg1 := makeSegment(t, 3)
	seg2 := makeSegment(t, 3)

	wallTime := time.Date(2026, 4, 2, 14, 0, 0, 0, time.UTC)

	src := &seekableSource{
		segments: map[string][]byte{
			wallTime.Format(time.RFC3339): seg2,
		},
	}

	first := demuxerFromBytes(seg1)
	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if _, err := cd.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if err := cd.Seek(ctx, wallTime, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	pkt, err := cd.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket after seek: %v", err)
	}

	if pkt.NewCodecs != nil {
		t.Fatal("unexpected NewCodecs when seeking to same codec")
	}
}

func TestChainingDemuxer_Seek_NotSeekable(t *testing.T) {
	t.Parallel()

	seg := makeSegment(t, 3)
	first := demuxerFromBytes(seg)

	// emptySource does not implement SeekableSegmentSource.
	cd := chain.NewChainingDemuxer(first, emptySource{})
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	err := cd.Seek(ctx, time.Now(), 0)
	if err == nil {
		t.Fatal("Seek should fail on non-seekable source")
	}
}

func TestChainingDemuxer_Seek_SeekableSource(t *testing.T) {
	t.Parallel()

	seg1 := makeSegment(t, 3)
	seg2 := makeSegment(t, 5)

	wallTime := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)

	src := &seekableSource{
		segments: map[string][]byte{
			wallTime.Format(time.RFC3339): seg2,
		},
	}

	first := demuxerFromBytes(seg1)
	cd := chain.NewChainingDemuxer(first, src)
	ctx := context.Background()

	if _, err := cd.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read one packet from the first segment.
	pkt, err := cd.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if pkt.DTS != 0 {
		t.Errorf("first packet DTS want 0, got %v", pkt.DTS)
	}

	// Seek to the second segment.
	if err := cd.Seek(ctx, wallTime, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	// After seek, we should be reading from seg2 (which has 5 packets).
	var pkts []av.Packet

	for {
		p, err := cd.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("ReadPacket after seek: %v", err)
		}

		pkts = append(pkts, p)
	}

	if len(pkts) != 5 {
		t.Fatalf("expected 5 packets from sought segment, got %d", len(pkts))
	}
}
