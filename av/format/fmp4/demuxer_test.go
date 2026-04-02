package fmp4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// avcc wraps raw NALU bytes in a single-NALU AVCC container
// (4-byte big-endian length prefix + NALU data).
func avcc(nalu ...byte) []byte {
	out := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(out, uint32(len(nalu)))
	copy(out[4:], nalu)

	return out
}

// compile-time check: *fmp4.Demuxer satisfies av.DemuxCloser.
var _ av.DemuxCloser = (*fmp4.Demuxer)(nil)

// ── helpers ───────────────────────────────────────────────────────────────────

// muxToBytes writes a complete fMP4 stream for the given streams and packets,
// returning the raw bytes.
func muxToBytes(t *testing.T, streams []av.Stream, pkts []av.Packet) []byte {
	t.Helper()

	var buf bytes.Buffer
	mux := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("mux WriteHeader: %v", err)
	}

	for _, pkt := range pkts {
		if err := mux.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("mux WritePacket: %v", err)
		}
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("mux WriteTrailer: %v", err)
	}

	return buf.Bytes()
}

// readAllPackets drains a Demuxer until io.EOF and returns the packets.
func readAllPackets(t *testing.T, dmx *fmp4.Demuxer) []av.Packet {
	t.Helper()

	ctx := context.Background()
	var pkts []av.Packet

	for {
		pkt, err := dmx.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}

		pkts = append(pkts, pkt)
	}

	return pkts
}

// ── GetCodecs tests ───────────────────────────────────────────────────────────

func TestDemuxer_GetCodecs_H264(t *testing.T) {
	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}
	data := muxToBytes(t, streams, nil) // no packets, just the init segment

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))
	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 stream, got %d", len(got))
	}

	if got[0].Codec.Type() != av.H264 {
		t.Errorf("want H264 codec, got %v", got[0].Codec.Type())
	}
}

func TestDemuxer_GetCodecs_AAC(t *testing.T) {
	aac := makeAACCodec(t)
	streams := []av.Stream{{Idx: 0, Codec: aac}}
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))
	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 stream, got %d", len(got))
	}

	if got[0].Codec.Type() != av.AAC {
		t.Errorf("want AAC codec, got %v", got[0].Codec.Type())
	}
}

func TestDemuxer_GetCodecs_FLAC(t *testing.T) {
	flac := pcm.NewFLACCodecData(av.PCM_MULAW, 8000, av.ChMono)
	streams := []av.Stream{{Idx: 0, Codec: flac}}
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))
	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 stream, got %d", len(got))
	}

	if got[0].Codec.Type() != av.FLAC {
		t.Errorf("want FLAC codec, got %v", got[0].Codec.Type())
	}
}

func TestDemuxer_GetCodecs_MultiStream(t *testing.T) {
	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))
	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 streams, got %d", len(got))
	}

	if got[0].Codec.Type() != av.H264 {
		t.Errorf("stream 0: want H264, got %v", got[0].Codec.Type())
	}

	if got[1].Codec.Type() != av.AAC {
		t.Errorf("stream 1: want AAC, got %v", got[1].Codec.Type())
	}
}

func TestDemuxer_GetCodecs_NoMoov(t *testing.T) {
	dmx := fmp4.NewDemuxer(bytes.NewReader([]byte{}))
	_, err := dmx.GetCodecs(context.Background())

	if !errors.Is(err, fmp4.ErrNoMoovBox) {
		t.Errorf("want ErrNoMoovBox, got %v", err)
	}
}

// ── ReadPacket / round-trip tests ─────────────────────────────────────────────

func TestDemuxer_ReadPacket_VideoOnlyRoundTrip(t *testing.T) {
	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Three frames: keyframe, non-keyframe, keyframe.
	// The muxer flushes on the second keyframe; WriteTrailer flushes the rest.
	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: frameDur, Data: avcc(0x01, 0x02, 0x03), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: frameDur, Duration: frameDur, Data: avcc(0x04, 0x05, 0x06), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 2 * frameDur, Duration: frameDur, Data: avcc(0x07, 0x08, 0x09), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 1 || got[0].Codec.Type() != av.H264 {
		t.Fatalf("unexpected streams: %v", got)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	for i, want := range inPkts {
		got := outPkts[i]

		if got.KeyFrame != want.KeyFrame {
			t.Errorf("pkt %d: KeyFrame want %v got %v", i, want.KeyFrame, got.KeyFrame)
		}

		if got.DTS != want.DTS {
			t.Errorf("pkt %d: DTS want %v got %v", i, want.DTS, got.DTS)
		}

		if got.Duration != want.Duration {
			t.Errorf("pkt %d: Duration want %v got %v", i, want.Duration, got.Duration)
		}

		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("pkt %d: Data want %v got %v", i, want.Data, got.Data)
		}
	}
}

func TestDemuxer_ReadPacket_AudioOnlyRoundTrip(t *testing.T) {
	// 20ms maps exactly to 882 ticks at 44100 Hz (20*44100/1000=882 integer).
	const frameDur = 20 * time.Millisecond

	aac := makeAACCodec(t)
	streams := []av.Stream{{Idx: 0, Codec: aac}}

	inPkts := []av.Packet{
		{Idx: 0, DTS: 0, Duration: frameDur, Data: []byte{0xAA, 0xBB}, CodecType: av.AAC},
		{Idx: 0, DTS: frameDur, Duration: frameDur, Data: []byte{0xCC, 0xDD}, CodecType: av.AAC},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	for i, want := range inPkts {
		got := outPkts[i]

		if got.DTS != want.DTS {
			t.Errorf("pkt %d: DTS want %v got %v", i, want.DTS, got.DTS)
		}

		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("pkt %d: Data want %v got %v", i, want.Data, got.Data)
		}
	}
}

func TestDemuxer_ReadPacket_VideoAndAudioRoundTrip(t *testing.T) {
	const vidDur = 33 * time.Millisecond
	const audDur = 21 * time.Millisecond

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	// Two video keyframes + non-key + audio packets.
	// Fragment 1 is flushed at second video keyframe:
	//   contains [video@0(key), audio@0, video@33ms(non-key)].
	// WriteTrailer flushes fragment 2: [video@66ms(key)].
	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 1, DTS: 0, Duration: audDur, Data: []byte{0xA1}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: avcc(0x02), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 2 * vidDur, Duration: vidDur, Data: avcc(0x03), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))
	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 streams, got %d", len(got))
	}

	outPkts := readAllPackets(t, dmx)

	// 4 input packets → 4 output packets (possibly reordered by DTS within each fragment).
	if len(outPkts) != 4 {
		t.Fatalf("want 4 packets, got %d", len(outPkts))
	}

	// Verify DTS ordering: demuxer sorts by DTS within each fragment.
	for i := 1; i < len(outPkts); i++ {
		if outPkts[i].DTS < outPkts[i-1].DTS {
			t.Errorf("DTS not non-decreasing at index %d: %v < %v", i, outPkts[i].DTS, outPkts[i-1].DTS)
		}
	}

	// Verify all expected payloads appear. Video packets are AVCC (4-byte
	// length prefix + NALU), audio packets are raw bytes.
	wantPayloads := [][]byte{avcc(0x01), {0xA1}, avcc(0x02), avcc(0x03)}
	foundCount := 0

	for _, want := range wantPayloads {
		for _, pkt := range outPkts {
			if bytes.Equal(pkt.Data, want) {
				foundCount++

				break
			}
		}
	}

	if foundCount != len(wantPayloads) {
		t.Errorf("expected %d matching payloads, found %d", len(wantPayloads), foundCount)
	}
}

func TestDemuxer_ReadPacket_PTSOffset(t *testing.T) {
	const frameDur = 33 * time.Millisecond
	const ptsOff = 66 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, PTSOffset: ptsOff, Duration: frameDur, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: frameDur, PTSOffset: ptsOff, Duration: frameDur, Data: avcc(0x02), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 2 * frameDur, Duration: frameDur, Data: avcc(0x03), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != 3 {
		t.Fatalf("want 3 packets, got %d", len(outPkts))
	}

	// PTSOffset should survive the round-trip.
	if outPkts[0].PTSOffset != ptsOff {
		t.Errorf("pkt 0 PTSOffset: want %v, got %v", ptsOff, outPkts[0].PTSOffset)
	}

	if outPkts[1].PTSOffset != ptsOff {
		t.Errorf("pkt 1 PTSOffset: want %v, got %v", ptsOff, outPkts[1].PTSOffset)
	}
}

func TestDemuxer_ReadPacket_KeyFrameFlags(t *testing.T) {
	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: frameDur, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: frameDur, Duration: frameDur, Data: avcc(0x02), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 2 * frameDur, Duration: frameDur, Data: avcc(0x03), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)
	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != 3 {
		t.Fatalf("want 3 packets, got %d", len(outPkts))
	}

	wantKey := []bool{true, false, true}

	for i, want := range wantKey {
		if outPkts[i].KeyFrame != want {
			t.Errorf("pkt %d: KeyFrame want %v, got %v", i, want, outPkts[i].KeyFrame)
		}
	}
}

// ── Close tests ───────────────────────────────────────────────────────────────

func TestDemuxer_Close_ClosesUnderlying(t *testing.T) {
	closed := false

	rc := &closingReader{
		r:       bytes.NewReader([]byte{}),
		onClose: func() { closed = true },
	}

	dmx := fmp4.NewDemuxer(rc)
	if err := dmx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !closed {
		t.Error("underlying reader was not closed")
	}
}

func TestDemuxer_Close_NonCloserReader(t *testing.T) {
	// bytes.Reader does not implement io.Closer; Close must still return nil.
	dmx := fmp4.NewDemuxer(bytes.NewReader([]byte{}))
	if err := dmx.Close(); err != nil {
		t.Errorf("Close on non-Closer: want nil, got %v", err)
	}
}

// ── EOF behaviour ─────────────────────────────────────────────────────────────

func TestDemuxer_ReadPacket_EOF(t *testing.T) {
	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}
	data := muxToBytes(t, streams, nil) // init segment only, no fragments

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	_, err := dmx.ReadPacket(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF, got %v", err)
	}
}

// ── Context cancellation ──────────────────────────────────────────────────────

func TestDemuxer_ReadPacket_CancelledContext(t *testing.T) {
	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := dmx.ReadPacket(ctx)
	if err == nil {
		t.Error("want error from cancelled context, got nil")
	}
}

// ── closingReader helper ──────────────────────────────────────────────────────

type closingReader struct {
	r       io.Reader
	onClose func()
}

func (c *closingReader) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *closingReader) Close() error {
	c.onClose()

	return nil
}

// ── Analytics round-trip tests ────────────────────────────────────────────────

func TestDemuxer_Analytics_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	fa := &av.FrameAnalytics{
		SiteID: 1, ChannelID: 2, VehicleCount: 3,
		Objects: []*av.Detection{{X: 10, Y: 20, W: 100, H: 200, ClassID: 1, Confidence: 90}},
	}

	inPkts := []av.Packet{
		{
			Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond,
			Data: avcc(0x65, 0x88), CodecType: av.H264, Analytics: fa,
		},
		{
			Idx: 0, KeyFrame: true, DTS: 33 * time.Millisecond, Duration: 33 * time.Millisecond,
			Data: avcc(0x65, 0x99), CodecType: av.H264,
		},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != 2 {
		t.Fatalf("want 2 packets, got %d", len(outPkts))
	}

	// The first packet should carry the Analytics data.
	if outPkts[0].Analytics == nil {
		t.Fatal("pkt 0: expected Analytics, got nil")
	}

	wantJSON, _ := json.Marshal(fa)
	gotJSON, _ := json.Marshal(outPkts[0].Analytics)

	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("Analytics mismatch:\n got  %s\n want %s", gotJSON, wantJSON)
	}

	// The second packet should have no analytics.
	if outPkts[1].Analytics != nil {
		t.Errorf("pkt 1: expected nil Analytics, got %v", outPkts[1].Analytics)
	}
}

func TestDemuxer_Analytics_NonMatchingScheme(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Build a complete fMP4 stream using FragmentWriter, then manually replace
	// the emsg scheme URI. We mux a packet with analytics so an emsg is produced,
	// then tamper with the scheme in the raw bytes.
	fa := &av.FrameAnalytics{SiteID: 99, ChannelID: 42, VehicleCount: 7}

	inPkts := []av.Packet{
		{
			Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond,
			Data: avcc(0x65, 0x88), CodecType: av.H264, Analytics: fa,
		},
		{
			Idx: 0, KeyFrame: true, DTS: 33 * time.Millisecond, Duration: 33 * time.Millisecond,
			Data: avcc(0x65, 0x99), CodecType: av.H264,
		},
	}

	raw := muxToBytes(t, streams, inPkts)

	// Replace the valid scheme URI with a different one of equal length.
	// "urn:vtpl:analytics:1" → "urn:test:analytics:1"
	original := []byte("urn:vtpl:analytics:1")
	replacement := []byte("urn:test:analytics:1")

	idx := bytes.Index(raw, original)
	if idx < 0 {
		t.Fatal("could not find emsg scheme URI in raw data")
	}

	copy(raw[idx:], replacement)

	dmx := fmp4.NewDemuxer(bytes.NewReader(raw))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	for i, pkt := range outPkts {
		if pkt.Analytics != nil {
			t.Errorf("pkt %d: expected nil Analytics with non-matching scheme, got %v", i, pkt.Analytics)
		}
	}
}

// ── Multi-fragment DTS monotonic test ─────────────────────────────────────────

func TestDemuxer_MultiFragment_DTS_Monotonic(t *testing.T) {
	t.Parallel()

	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// 3 GOPs: keyframe + 2 P-frames each = 9 packets total.
	// The muxer flushes a fragment on each new keyframe after the first.
	var inPkts []av.Packet

	for gop := range 3 {
		for frame := range 3 {
			idx := gop*3 + frame
			inPkts = append(inPkts, av.Packet{
				Idx:       0,
				KeyFrame:  frame == 0,
				DTS:       time.Duration(idx) * frameDur,
				Duration:  frameDur,
				Data:      avcc(byte(idx + 1)),
				CodecType: av.H264,
			})
		}
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	for i := 1; i < len(outPkts); i++ {
		if outPkts[i].DTS < outPkts[i-1].DTS {
			t.Errorf("DTS not monotonic at index %d: %v < %v", i, outPkts[i].DTS, outPkts[i-1].DTS)
		}
	}
}

// ── Duration preserved test ───────────────────────────────────────────────────

func TestDemuxer_Duration_Preserved(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	durations := []time.Duration{33 * time.Millisecond, 40 * time.Millisecond, 17 * time.Millisecond}

	var dts time.Duration

	inPkts := make([]av.Packet, len(durations))

	for i, dur := range durations {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i == 0 || i == 2, // keyframes at 0 and 2 to force a flush
			DTS:       dts,
			Duration:  dur,
			Data:      avcc(byte(i + 1)),
			CodecType: av.H264,
		}

		dts += dur
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	for i, want := range durations {
		got := outPkts[i].Duration
		diff := got - want
		if diff < 0 {
			diff = -diff
		}

		if diff > time.Millisecond {
			t.Errorf("pkt %d: Duration want %v, got %v (diff %v)", i, want, got, diff)
		}
	}
}

// ── Many packets test ─────────────────────────────────────────────────────────

func TestDemuxer_ManyPackets(t *testing.T) {
	t.Parallel()

	const (
		total    = 500
		gopSize  = 30
		frameDur = 33 * time.Millisecond
	)

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := make([]av.Packet, total)

	for i := range total {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i%gopSize == 0,
			DTS:       time.Duration(i) * frameDur,
			Duration:  frameDur,
			Data:      avcc(byte(i%256), byte(i/256)),
			CodecType: av.H264,
		}
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != total {
		t.Fatalf("want %d packets, got %d", total, len(outPkts))
	}
}

// ── Large payload test ────────────────────────────────────────────────────────

func TestDemuxer_LargePayload(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Build a 1MB AVCC payload: 4-byte length prefix + 1MB NALU.
	const naluSize = 1 << 20
	nalu := make([]byte, naluSize)

	for i := range nalu {
		nalu[i] = byte(i % 251) // deterministic pattern using a prime
	}

	bigData := make([]byte, 4+naluSize)
	binary.BigEndian.PutUint32(bigData, naluSize)
	copy(bigData[4:], nalu)

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond, Data: bigData, CodecType: av.H264},
		// Second keyframe forces flush of the first.
		{Idx: 0, KeyFrame: true, DTS: 33 * time.Millisecond, Duration: 33 * time.Millisecond, Data: avcc(0x01), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != 2 {
		t.Fatalf("want 2 packets, got %d", len(outPkts))
	}

	if !bytes.Equal(outPkts[0].Data, bigData) {
		t.Errorf("large payload mismatch: got len %d, want len %d", len(outPkts[0].Data), len(bigData))
	}
}

// ── Empty mdat test ───────────────────────────────────────────────────────────

func TestDemuxer_EmptyMdat(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// WriteHeader + WriteTrailer with no packets → init segment only.
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want 1 stream, got %d", len(got))
	}

	if got[0].Codec.Type() != av.H264 {
		t.Errorf("want H264, got %v", got[0].Codec.Type())
	}

	// ReadPacket should immediately return EOF.
	_, err = dmx.ReadPacket(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF on empty mdat, got %v", err)
	}
}

// ── Multiple GetCodecs calls test ─────────────────────────────────────────────

func TestDemuxer_MultipleGetCodecs(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}
	data := muxToBytes(t, streams, nil)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	got1, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("first GetCodecs: %v", err)
	}

	// Second call: moov was already consumed, so we expect either the same
	// cached result or an error (no second moov). The demuxer reads until it
	// finds a moov; with no more data it should return ErrNoMoovBox.
	got2, err2 := dmx.GetCodecs(context.Background())

	if err2 != nil {
		// Acceptable: no second moov found.
		if !errors.Is(err2, fmp4.ErrNoMoovBox) {
			t.Fatalf("second GetCodecs: unexpected error %v", err2)
		}

		return
	}

	// If it succeeded, verify it returns the same streams.
	if len(got2) != len(got1) {
		t.Errorf("second GetCodecs returned %d streams, want %d", len(got2), len(got1))
	}
}

// ── ReadPacket before GetCodecs test ──────────────────────────────────────────

func TestDemuxer_ReadPacketBeforeGetCodecs(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 33 * time.Millisecond, Duration: 33 * time.Millisecond, Data: avcc(0x02), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	// Skip GetCodecs, call ReadPacket directly.
	// The demuxer should encounter the moov box in ReadPacket and handle it
	// (as a mid-stream codec change) or return an error.
	ctx := context.Background()
	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		// It is acceptable to get an error if the demuxer requires GetCodecs first.
		// Verify it is not a panic or unexpected error type.
		t.Logf("ReadPacket before GetCodecs returned error (acceptable): %v", err)

		return
	}

	// If it worked, verify we got a valid packet.
	if pkt.CodecType != av.H264 {
		t.Errorf("want H264 codec type, got %v", pkt.CodecType)
	}
}

// ── Video and audio interleaved DTS order test ────────────────────────────────

func TestDemuxer_VideoAndAudio_InterleavedDTS(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	// Video at DTS 0, 33, 66ms and audio at DTS 10, 30, 50ms.
	// Two keyframes to force at least one flush.
	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 1, DTS: 10 * time.Millisecond, Duration: 20 * time.Millisecond, Data: []byte{0xA1}, CodecType: av.AAC},
		{Idx: 1, DTS: 30 * time.Millisecond, Duration: 20 * time.Millisecond, Data: []byte{0xA2}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: 33 * time.Millisecond, Duration: 33 * time.Millisecond, Data: avcc(0x02), CodecType: av.H264},
		{Idx: 1, DTS: 50 * time.Millisecond, Duration: 20 * time.Millisecond, Data: []byte{0xA3}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: true, DTS: 66 * time.Millisecond, Duration: 33 * time.Millisecond, Data: avcc(0x03), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, inPkts)

	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	got, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("want 2 streams, got %d", len(got))
	}

	outPkts := readAllPackets(t, dmx)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	// Verify DTS ordering: packets must come out in non-decreasing DTS order.
	for i := 1; i < len(outPkts); i++ {
		if outPkts[i].DTS < outPkts[i-1].DTS {
			t.Errorf("DTS not non-decreasing at index %d: %v < %v", i, outPkts[i].DTS, outPkts[i-1].DTS)
		}
	}

	// Verify both track indices appear in the output.
	trackSeen := map[uint16]bool{}

	for _, pkt := range outPkts {
		trackSeen[pkt.Idx] = true
	}

	if !trackSeen[0] {
		t.Error("no video packets (Idx=0) in output")
	}

	if !trackSeen[1] {
		t.Error("no audio packets (Idx=1) in output")
	}
}

// ── SeekToKeyframe tests ────────────────────────────────────────────────────

func TestDemuxer_SeekToKeyframe_NotSeekable(t *testing.T) {
	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}
	data := muxToBytes(t, streams, []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: 33 * time.Millisecond, Data: avcc(0x01), CodecType: av.H264},
	})

	// Use a non-seekable reader (plain bytes.NewBuffer, not bytes.NewReader).
	dmx := fmp4.NewDemuxer(bytes.NewBuffer(data))

	if _, err := dmx.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	err := dmx.SeekToKeyframe(0)
	if !errors.Is(err, fmp4.ErrNotSeekable) {
		t.Errorf("want ErrNotSeekable, got %v", err)
	}
}

func TestDemuxer_SeekToKeyframe_MoofScan(t *testing.T) {
	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Create 6 keyframes → 6 fragments (each keyframe triggers a flush).
	var pkts []av.Packet
	for i := range 6 {
		pkts = append(pkts, av.Packet{
			Idx:       0,
			KeyFrame:  true,
			DTS:       time.Duration(i) * frameDur,
			Duration:  frameDur,
			Data:      avcc(byte(i + 1)),
			CodecType: av.H264,
		})
	}

	data := muxToBytes(t, streams, pkts)

	// Use bytes.NewReader (implements io.ReadSeeker).
	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	ctx := context.Background()
	if _, err := dmx.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Seek to a time that falls in the 4th fragment (DTS ≈ 3*33ms = 99ms).
	target := 3 * frameDur
	if err := dmx.SeekToKeyframe(target); err != nil {
		t.Fatalf("SeekToKeyframe: %v", err)
	}

	// The first packet after seek should have DTS <= target.
	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket after seek: %v", err)
	}

	if pkt.DTS > target {
		t.Errorf("first packet DTS %v > target %v", pkt.DTS, target)
	}

	if !pkt.KeyFrame {
		t.Error("first packet after seek should be a keyframe")
	}
}

func TestDemuxer_SeekToKeyframe_SeekToStart(t *testing.T) {
	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	pkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: frameDur, Data: avcc(0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: frameDur, Duration: frameDur, Data: avcc(0x02), CodecType: av.H264},
	}

	data := muxToBytes(t, streams, pkts)
	dmx := fmp4.NewDemuxer(bytes.NewReader(data))

	ctx := context.Background()
	if _, err := dmx.GetCodecs(ctx); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	// Read past first packet.
	if _, err := dmx.ReadPacket(ctx); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	// Seek back to the start.
	if err := dmx.SeekToKeyframe(0); err != nil {
		t.Fatalf("SeekToKeyframe(0): %v", err)
	}

	pkt, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket after seek: %v", err)
	}

	if pkt.DTS != 0 {
		t.Errorf("expected DTS=0 after seeking to start, got %v", pkt.DTS)
	}
}

// ── sidx tests ──────────────────────────────────────────────────────────────

func TestMuxer_FragIndex(t *testing.T) {
	const frameDur = 33 * time.Millisecond

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	var buf bytes.Buffer
	mux := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write 3 keyframes → 3 fragments (first two flushed on keyframe, last on trailer).
	for i := range 3 {
		pkt := av.Packet{
			Idx:       0,
			KeyFrame:  true,
			DTS:       time.Duration(i) * frameDur,
			Duration:  frameDur,
			Data:      avcc(byte(i + 1)),
			CodecType: av.H264,
		}
		if err := mux.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket(%d): %v", i, err)
		}
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	idx := mux.FragIndex()
	if len(idx) < 2 {
		t.Fatalf("want at least 2 fragment index entries, got %d", len(idx))
	}

	// First entry should start at PTS 0.
	if idx[0].PTS != 0 {
		t.Errorf("first fragment PTS want 0, got %v", idx[0].PTS)
	}

	// All entries should have positive size.
	for i, e := range idx {
		if e.Size <= 0 {
			t.Errorf("fragment %d: size %d <= 0", i, e.Size)
		}
	}
}

func TestBuildSidx_RoundTrip(t *testing.T) {
	entries := []fmp4.FragmentIndex{
		{PTS: 0, Duration: 2 * time.Second, Offset: 100, Size: 5000, StartsWithSAP: true},
		{PTS: 2 * time.Second, Duration: 2 * time.Second, Offset: 5100, Size: 4800, StartsWithSAP: true},
		{PTS: 4 * time.Second, Duration: 2 * time.Second, Offset: 9900, Size: 5200, StartsWithSAP: false},
	}

	sidxData := fmp4.BuildSidx(entries, 90000)
	if sidxData == nil {
		t.Fatal("BuildSidx returned nil")
	}

	// Verify it's a valid box: first 4 bytes = size, next 4 = "sidx".
	if len(sidxData) < 8 {
		t.Fatalf("sidx box too small: %d bytes", len(sidxData))
	}

	size := binary.BigEndian.Uint32(sidxData[0:4])
	typ := string(sidxData[4:8])

	if typ != "sidx" {
		t.Errorf("box type want 'sidx', got %q", typ)
	}

	if int(size) != len(sidxData) {
		t.Errorf("box size %d != actual %d", size, len(sidxData))
	}
}

func TestBuildSidx_Empty(t *testing.T) {
	sidx := fmp4.BuildSidx(nil, 90000)
	if sidx != nil {
		t.Errorf("BuildSidx(nil) should return nil, got %d bytes", len(sidx))
	}
}
