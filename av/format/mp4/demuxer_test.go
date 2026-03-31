package mp4_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/mp4"
)

// closingReadSeeker implements io.ReadSeeker and io.Closer with an onClose hook.
type closingReadSeeker struct {
	r       *bytes.Reader
	onClose func()
}

func (c *closingReadSeeker) Read(p []byte) (int, error)                { return c.r.Read(p) }
func (c *closingReadSeeker) Seek(off int64, whence int) (int64, error) { return c.r.Seek(off, whence) }
func (c *closingReadSeeker) Close() error                              { c.onClose(); return nil }

// demuxFmt demuxes all packets from the given container bytes using the format spec.
// Returns the detected streams and the full packet list.
func demuxFmt(t *testing.T, f formatSpec, data []byte) ([]av.Stream, []av.Packet) {
	t.Helper()

	ctx := context.Background()
	d := f.newDemuxer(bytes.NewReader(data))

	defer func() {
		if err := d.Close(); err != nil {
			t.Errorf("[%s] DemuxClose: %v", f.name, err)
		}
	}()

	streams, err := d.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("[%s] GetCodecs: %v", f.name, err)
	}

	var pkts []av.Packet

	for {
		pkt, err := d.ReadPacket(ctx)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("[%s] ReadPacket: %v", f.name, err)
		}

		pkts = append(pkts, pkt)
	}

	return streams, pkts
}

// ── mp4 demuxer lifecycle tests ───────────────────────────────────────────────

func TestMP4Demuxer_GetCodecs_NoMoov(t *testing.T) {
	t.Parallel()

	d := mp4.NewDemuxer(bytes.NewReader([]byte{}))
	_, err := d.GetCodecs(context.Background())

	if !errors.Is(err, mp4.ErrNoMoovBox) {
		t.Errorf("want ErrNoMoovBox, got %v", err)
	}
}

func TestMP4Demuxer_ReadPacket_EOF(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	// Mux a file with no packets (empty mdat).
	data := muxFmt(t, allFormats[1], []av.Stream{{Idx: 0, Codec: h264}}, nil)

	d := mp4.NewDemuxer(bytes.NewReader(data))

	if _, err := d.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	_, err := d.ReadPacket(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("want io.EOF, got %v", err)
	}
}

func TestMP4Demuxer_ReadPacket_CancelledContext(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	data := muxFmt(t, allFormats[1], []av.Stream{{Idx: 0, Codec: h264}}, nil)

	d := mp4.NewDemuxer(bytes.NewReader(data))

	if _, err := d.GetCodecs(context.Background()); err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.ReadPacket(ctx)
	if err == nil {
		t.Error("want error from cancelled context, got nil")
	}
}

func TestMP4Demuxer_Close_ClosesUnderlying(t *testing.T) {
	t.Parallel()

	closed := false
	cr := &closingReadSeeker{
		r:       bytes.NewReader([]byte{}),
		onClose: func() { closed = true },
	}

	d := mp4.NewDemuxer(cr)
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !closed {
		t.Error("underlying reader was not closed")
	}
}

func TestMP4Demuxer_Close_NonCloserReader(t *testing.T) {
	t.Parallel()

	d := mp4.NewDemuxer(bytes.NewReader([]byte{}))
	if err := d.Close(); err != nil {
		t.Errorf("Close on non-Closer: want nil, got %v", err)
	}
}

// TestMP4Demuxer_GetCodecs_MultiCodec verifies that a file with both H.264
// and AAC tracks reports both streams after GetCodecs.
func TestMP4Demuxer_GetCodecs_MultiCodec(t *testing.T) {
	t.Parallel()

	streams := []av.Stream{
		{Idx: 0, Codec: makeH264Codec(t)},
		{Idx: 1, Codec: makeAACCodec(t)},
	}

	data := muxFmt(t, allFormats[1], streams, nil)

	d := mp4.NewDemuxer(bytes.NewReader(data))

	got, err := d.GetCodecs(context.Background())
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

// TestMP4Demuxer_KeyFrameFlags verifies that keyframe information is preserved
// in a standalone mp4→mp4 round trip.
func TestMP4Demuxer_KeyFrameFlags(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 2 * vidDur, Duration: vidDur, Data: []byte{0x65, 0x03}, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("want %d packets, got %d", len(inPkts), len(outPkts))
	}

	wantKeys := []bool{true, false, true}

	for i, wantKey := range wantKeys {
		if outPkts[i].KeyFrame != wantKey {
			t.Errorf("pkt[%d] KeyFrame: want %v, got %v", i, wantKey, outPkts[i].KeyFrame)
		}
	}
}

// ── new demuxer round-trip tests ─────────────────────────────────────────────

func TestMP4Demuxer_VideoOnly_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: avcc(0x65, 0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: avcc(0x41, 0x02), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: avcc(0x41, 0x03), CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 3 * vidDur, Duration: vidDur, Data: avcc(0x65, 0x04), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 4 * vidDur, Duration: vidDur, Data: avcc(0x41, 0x05), CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	wantKeys := []bool{true, false, false, true, false}

	for i := range inPkts {
		if outPkts[i].KeyFrame != wantKeys[i] {
			t.Errorf("pkt[%d] KeyFrame: want %v, got %v", i, wantKeys[i], outPkts[i].KeyFrame)
		}

		if outPkts[i].DTS != inPkts[i].DTS {
			t.Errorf("pkt[%d] DTS: want %v, got %v", i, inPkts[i].DTS, outPkts[i].DTS)
		}

		if outPkts[i].Duration != inPkts[i].Duration {
			t.Errorf("pkt[%d] Duration: want %v, got %v", i, inPkts[i].Duration, outPkts[i].Duration)
		}

		if !bytes.Equal(outPkts[i].Data, inPkts[i].Data) {
			t.Errorf("pkt[%d] Data: want %x, got %x", i, inPkts[i].Data, outPkts[i].Data)
		}
	}
}

func TestMP4Demuxer_AudioOnly_RoundTrip(t *testing.T) {
	t.Parallel()

	aac := makeAACCodec(t)
	streams := []av.Stream{{Idx: 0, Codec: aac}}

	inPkts := make([]av.Packet, 5)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			DTS:       time.Duration(i) * audDur,
			Duration:  audDur,
			Data:      []byte{0xDE, 0xAD, byte(i), 0x01, 0x02},
			CodecType: av.AAC,
		}
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	for i := range inPkts {
		if outPkts[i].DTS != inPkts[i].DTS {
			t.Errorf("pkt[%d] DTS: want %v, got %v", i, inPkts[i].DTS, outPkts[i].DTS)
		}

		if outPkts[i].Duration != inPkts[i].Duration {
			t.Errorf("pkt[%d] Duration: want %v, got %v", i, inPkts[i].Duration, outPkts[i].Duration)
		}

		if !bytes.Equal(outPkts[i].Data, inPkts[i].Data) {
			t.Errorf("pkt[%d] Data: want %x, got %x", i, inPkts[i].Data, outPkts[i].Data)
		}
	}
}

func TestMP4Demuxer_VideoAndAudio_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: avcc(0x65, 0x01), CodecType: av.H264},
		{Idx: 1, DTS: 0, Duration: audDur, Data: []byte{0xAA, 0x01}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: avcc(0x41, 0x02), CodecType: av.H264},
		{Idx: 1, DTS: audDur, Duration: audDur, Data: []byte{0xAA, 0x02}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: avcc(0x41, 0x03), CodecType: av.H264},
		{Idx: 1, DTS: 2 * audDur, Duration: audDur, Data: []byte{0xAA, 0x03}, CodecType: av.AAC},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	// Group by stream index for order-independent comparison.
	wantByIdx := map[uint16][]av.Packet{}
	gotByIdx := map[uint16][]av.Packet{}

	for _, p := range inPkts {
		wantByIdx[p.Idx] = append(wantByIdx[p.Idx], p)
	}

	for _, p := range outPkts {
		gotByIdx[p.Idx] = append(gotByIdx[p.Idx], p)
	}

	for idx, wantPkts := range wantByIdx {
		gotPkts := gotByIdx[idx]
		if len(gotPkts) != len(wantPkts) {
			t.Errorf("stream %d: want %d packets, got %d", idx, len(wantPkts), len(gotPkts))

			continue
		}

		for i, want := range wantPkts {
			got := gotPkts[i]
			if !bytes.Equal(got.Data, want.Data) {
				t.Errorf("stream %d pkt[%d] Data: want %x, got %x", idx, i, want.Data, got.Data)
			}
		}
	}
}

func TestMP4Demuxer_PTSOffset_Preserved(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	ptsOff := 66 * time.Millisecond // 5940 ticks @ 90000 Hz — exact

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, PTSOffset: ptsOff, Data: avcc(0x65, 0x01), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, PTSOffset: 0, Data: avcc(0x41, 0x02), CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, PTSOffset: ptsOff, Data: avcc(0x41, 0x03), CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	for i := range inPkts {
		if outPkts[i].PTSOffset != inPkts[i].PTSOffset {
			t.Errorf("pkt[%d] PTSOffset: want %v, got %v", i, inPkts[i].PTSOffset, outPkts[i].PTSOffset)
		}
	}
}

func TestMP4Demuxer_ManyPackets(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	const numPkts = 500

	inPkts := make([]av.Packet, numPkts)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i%30 == 0,
			DTS:       time.Duration(i) * vidDur,
			Duration:  vidDur,
			Data:      avcc(0x65, byte(i>>8), byte(i)),
			CodecType: av.H264,
		}
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != numPkts {
		t.Fatalf("packet count: want %d, got %d", numPkts, len(outPkts))
	}
}

func TestMP4Demuxer_LargePayload(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Build a 1 MB AVCC-formatted NALU.
	naluBody := make([]byte, 1<<20)
	naluBody[0] = 0x65 // IDR NALU type
	for i := 1; i < len(naluBody); i++ {
		naluBody[i] = byte(i)
	}

	largeData := avcc(naluBody...)

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: largeData, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != 1 {
		t.Fatalf("packet count: want 1, got %d", len(outPkts))
	}

	if !bytes.Equal(outPkts[0].Data, largeData) {
		t.Errorf("Data mismatch: lengths want %d, got %d", len(largeData), len(outPkts[0].Data))
	}
}

func TestMP4Demuxer_ConstantDuration_SttsRunLength(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	const numPkts = 10

	inPkts := make([]av.Packet, numPkts)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i == 0,
			DTS:       time.Duration(i) * vidDur,
			Duration:  vidDur,
			Data:      avcc(0x65, byte(i)),
			CodecType: av.H264,
		}
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != numPkts {
		t.Fatalf("packet count: want %d, got %d", numPkts, len(outPkts))
	}

	for i := range inPkts {
		if outPkts[i].Duration != vidDur {
			t.Errorf("pkt[%d] Duration: want %v, got %v", i, vidDur, outPkts[i].Duration)
		}
	}
}

func TestMP4Demuxer_VariableDuration(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	durations := []time.Duration{
		33 * time.Millisecond,
		40 * time.Millisecond,
		17 * time.Millisecond,
		50 * time.Millisecond,
		33 * time.Millisecond,
	}

	inPkts := make([]av.Packet, len(durations))

	var dts time.Duration

	for i, dur := range durations {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i == 0,
			DTS:       dts,
			Duration:  dur,
			Data:      avcc(0x65, byte(i)),
			CodecType: av.H264,
		}
		dts += dur
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	const tolerance = time.Millisecond

	for i, want := range inPkts {
		diff := outPkts[i].Duration - want.Duration
		if diff < -tolerance || diff > tolerance {
			t.Errorf("pkt[%d] Duration: want %v, got %v (diff %v)", i, want.Duration, outPkts[i].Duration, diff)
		}
	}
}

func TestMP4Demuxer_AllKeyframes(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := make([]av.Packet, 5)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  true,
			DTS:       time.Duration(i) * vidDur,
			Duration:  vidDur,
			Data:      avcc(0x65, byte(i)),
			CodecType: av.H264,
		}
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	for i, p := range outPkts {
		if !p.KeyFrame {
			t.Errorf("pkt[%d] KeyFrame: want true, got false", i)
		}
	}
}

func TestMP4Demuxer_MultipleGetCodecs(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	data := muxFmt(t, allFormats[1], streams, nil)

	d := mp4.NewDemuxer(bytes.NewReader(data))
	defer d.Close()

	ctx := context.Background()

	got1, err := d.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("first GetCodecs: %v", err)
	}

	got2, err := d.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("second GetCodecs: %v", err)
	}

	if len(got1) != len(got2) {
		t.Fatalf("stream count mismatch: first %d, second %d", len(got1), len(got2))
	}

	for i := range got1 {
		if got1[i].Codec.Type() != got2[i].Codec.Type() {
			t.Errorf("stream %d codec: first %v, second %v", i, got1[i].Codec.Type(), got2[i].Codec.Type())
		}
	}
}
