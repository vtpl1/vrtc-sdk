package pva

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// sliceDemuxer feeds packets from a slice, then returns io.EOF.
type sliceDemuxer struct {
	packets []av.Packet
	idx     int
}

func (d *sliceDemuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	return []av.Stream{{Idx: 0}}, nil
}

func (d *sliceDemuxer) ReadPacket(_ context.Context) (av.Packet, error) {
	if d.idx >= len(d.packets) {
		return av.Packet{}, io.EOF
	}

	pkt := d.packets[d.idx]
	d.idx++

	return pkt, nil
}

func (d *sliceDemuxer) Close() error { return nil }

func TestWallClockStamper_StampsZeroWallClock(t *testing.T) {
	t.Parallel()

	segStart := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	inner := &sliceDemuxer{packets: []av.Packet{
		{DTS: 0, CodecType: av.H264, KeyFrame: true},
		{DTS: 33 * time.Millisecond, CodecType: av.H264},
		{DTS: 66 * time.Millisecond, CodecType: av.H264},
	}}

	stamper := NewWallClockStampingDemuxer(inner, segStart)
	ctx := context.Background()

	pkt, err := stamper.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("pkt 0: %v", err)
	}

	if !pkt.WallClockTime.Equal(segStart) {
		t.Fatalf("pkt 0: WallClockTime = %v, want %v", pkt.WallClockTime, segStart)
	}

	pkt, err = stamper.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("pkt 1: %v", err)
	}

	want := segStart.Add(33 * time.Millisecond)
	if !pkt.WallClockTime.Equal(want) {
		t.Fatalf("pkt 1: WallClockTime = %v, want %v", pkt.WallClockTime, want)
	}

	pkt, err = stamper.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("pkt 2: %v", err)
	}

	want = segStart.Add(66 * time.Millisecond)
	if !pkt.WallClockTime.Equal(want) {
		t.Fatalf("pkt 2: WallClockTime = %v, want %v", pkt.WallClockTime, want)
	}
}

func TestWallClockStamper_PreservesExisting(t *testing.T) {
	t.Parallel()

	segStart := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	existingWC := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	inner := &sliceDemuxer{packets: []av.Packet{
		{DTS: 0, CodecType: av.H264, WallClockTime: existingWC},
	}}

	stamper := NewWallClockStampingDemuxer(inner, segStart)

	pkt, err := stamper.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if !pkt.WallClockTime.Equal(existingWC) {
		t.Fatalf("WallClockTime = %v, want existing %v", pkt.WallClockTime, existingWC)
	}
}

func TestWallClockStamper_NonZeroBaseDTS(t *testing.T) {
	t.Parallel()

	segStart := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	// Simulates intra-segment seek: first packet has DTS=2s instead of 0.
	inner := &sliceDemuxer{packets: []av.Packet{
		{DTS: 2 * time.Second, CodecType: av.H264, KeyFrame: true},
		{DTS: 2*time.Second + 33*time.Millisecond, CodecType: av.H264},
	}}

	stamper := NewWallClockStampingDemuxer(inner, segStart)
	ctx := context.Background()

	// First packet: baseDTS becomes 2s, WallClockTime = segStart + (2s - 2s) = segStart.
	pkt, err := stamper.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("pkt 0: %v", err)
	}

	if !pkt.WallClockTime.Equal(segStart) {
		t.Fatalf("pkt 0: WallClockTime = %v, want %v", pkt.WallClockTime, segStart)
	}

	// Second packet: WallClockTime = segStart + (2.033s - 2s) = segStart + 33ms.
	pkt, err = stamper.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("pkt 1: %v", err)
	}

	want := segStart.Add(33 * time.Millisecond)
	if !pkt.WallClockTime.Equal(want) {
		t.Fatalf("pkt 1: WallClockTime = %v, want %v", pkt.WallClockTime, want)
	}
}

func TestWallClockStamper_ZeroSegmentStart(t *testing.T) {
	t.Parallel()

	inner := &sliceDemuxer{packets: []av.Packet{
		{DTS: 0, CodecType: av.H264},
	}}

	// Zero segment start: should not stamp.
	stamper := NewWallClockStampingDemuxer(inner, time.Time{})

	pkt, err := stamper.ReadPacket(context.Background())
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if !pkt.WallClockTime.IsZero() {
		t.Fatalf("WallClockTime should remain zero when segmentStart is zero, got %v", pkt.WallClockTime)
	}
}

func TestWallClockStamper_EOF(t *testing.T) {
	t.Parallel()

	inner := &sliceDemuxer{packets: nil}
	stamper := NewWallClockStampingDemuxer(inner, time.Now())

	_, err := stamper.ReadPacket(context.Background())
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestWallClockStamper_GetCodecs(t *testing.T) {
	t.Parallel()

	inner := &sliceDemuxer{}
	stamper := NewWallClockStampingDemuxer(inner, time.Now())

	streams, err := stamper.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(streams) != 1 {
		t.Fatalf("got %d streams, want 1", len(streams))
	}
}

func TestWallClockStamper_MixedAudioVideo(t *testing.T) {
	t.Parallel()

	segStart := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	inner := &sliceDemuxer{packets: []av.Packet{
		{DTS: 0, CodecType: av.H264, KeyFrame: true},
		{DTS: 0, CodecType: av.AAC, Idx: 1},
		{DTS: 33 * time.Millisecond, CodecType: av.H264},
	}}

	stamper := NewWallClockStampingDemuxer(inner, segStart)
	ctx := context.Background()

	// Video packet.
	pkt, _ := stamper.ReadPacket(ctx)
	if !pkt.WallClockTime.Equal(segStart) {
		t.Fatalf("video pkt: WallClockTime = %v, want %v", pkt.WallClockTime, segStart)
	}

	// Audio packet (also gets stamped — WallClockTime applies to all tracks).
	pkt, _ = stamper.ReadPacket(ctx)
	if !pkt.WallClockTime.Equal(segStart) {
		t.Fatalf("audio pkt: WallClockTime = %v, want %v", pkt.WallClockTime, segStart)
	}

	// Second video packet.
	pkt, _ = stamper.ReadPacket(ctx)
	want := segStart.Add(33 * time.Millisecond)
	if !pkt.WallClockTime.Equal(want) {
		t.Fatalf("video pkt 2: WallClockTime = %v, want %v", pkt.WallClockTime, want)
	}
}
