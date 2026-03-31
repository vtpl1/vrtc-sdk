package mp4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/format/mp4"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// readBoxType reads an ISO BMFF box header from r and returns the 4-char box
// type, advancing r past the entire box.
func readBoxType(t *testing.T, r *bytes.Reader) string {
	t.Helper()

	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatalf("readBoxType: %v", err)
	}

	size := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	typ := string(hdr[4:8])

	if size < 8 {
		t.Fatalf("readBoxType: box size %d < 8", size)
	}

	payload := make([]byte, size-8)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("readBoxType: read payload for %q: %v", typ, err)
	}

	return typ
}

// closingWriter is a bytes.Buffer that records whether Close was called.
type closingWriter struct {
	bytes.Buffer
	closed bool
}

func (cw *closingWriter) Close() error {
	cw.closed = true
	return nil
}

// All durations are exact ms multiples so they round-trip through
// FMP4/MP4 (timescale units) without rounding error.
const (
	vidDur = 33 * time.Millisecond // 2970 ticks @ 90000 Hz
	audDur = 20 * time.Millisecond // 882 ticks  @ 44100 Hz
)

// ── mp4 package lifecycle tests ───────────────────────────────────────────────

func TestMP4Muxer_WriteHeader_Idempotency(t *testing.T) {
	t.Parallel()

	m := mp4.NewMuxer(io.Discard)
	ctx := context.Background()
	streams := []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("first WriteHeader: %v", err)
	}

	if err := m.WriteHeader(ctx, streams); err == nil {
		t.Fatal("second WriteHeader should return error")
	}
}

func TestMP4Muxer_WriteTrailer_Idempotency(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := mp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("first WriteTrailer: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err == nil {
		t.Fatal("second WriteTrailer should return error")
	}
}

func TestMP4Muxer_WritePacket_BeforeWriteHeader(t *testing.T) {
	t.Parallel()

	m := mp4.NewMuxer(io.Discard)
	pkt := av.Packet{Idx: 0, KeyFrame: true, Data: []byte{0x65}, CodecType: av.H264}

	if err := m.WritePacket(context.Background(), pkt); err == nil {
		t.Fatal("WritePacket before WriteHeader should return error")
	}
}

func TestMP4Muxer_WritePacket_AfterWriteTrailer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := mp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	pkt := av.Packet{Idx: 0, KeyFrame: true, Data: []byte{0x65}, CodecType: av.H264}
	if err := m.WritePacket(ctx, pkt); err == nil {
		t.Fatal("WritePacket after WriteTrailer should return error")
	}
}

func TestMP4Muxer_Close_ClosesUnderlying(t *testing.T) {
	t.Parallel()

	cw := &closingWriter{}
	m := mp4.NewMuxer(cw)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !cw.closed {
		t.Error("Close did not call Close() on the underlying writer")
	}
}

func TestMP4Muxer_Close_NonCloserWriter(t *testing.T) {
	t.Parallel()

	m := mp4.NewMuxer(io.Discard)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close on non-Closer writer: %v", err)
	}
}

func TestMP4Muxer_Output_HasFtypAndMoovBeforeMdat(t *testing.T) {
	t.Parallel()

	// Verify moov-first (fast-start) layout: ftyp then moov then mdat.
	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts) // allFormats[1] = mp4
	r := bytes.NewReader(data)

	typ1 := readBoxType(t, r)
	if typ1 != "ftyp" {
		t.Errorf("box 0: want ftyp, got %q", typ1)
	}

	typ2 := readBoxType(t, r)
	if typ2 != "moov" {
		t.Errorf("box 1: want moov, got %q", typ2)
	}

	if r.Len() > 0 {
		typ3 := readBoxType(t, r)
		if typ3 != "mdat" {
			t.Errorf("box 2: want mdat, got %q", typ3)
		}
	}
}

// ── new round-trip and structural tests ──────────────────────────────────────

func TestMP4Muxer_VideoOnly_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0xAA, 0xBB}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 3 * vidDur, Duration: vidDur, Data: []byte{0x65, 0xCC, 0xDD}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 4 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x03}, CodecType: av.H264},
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

func TestMP4Muxer_AudioOnly_RoundTrip(t *testing.T) {
	t.Parallel()

	aac := makeAACCodec(t)
	streams := []av.Stream{{Idx: 0, Codec: aac}}

	inPkts := make([]av.Packet, 5)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			DTS:       time.Duration(i) * audDur,
			Duration:  audDur,
			Data:      []byte{0xFF, byte(i), 0x01, 0x02},
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

func TestMP4Muxer_VideoAndAudio_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 1, DTS: 0, Duration: audDur, Data: []byte{0xFF, 0x01}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 1, DTS: audDur, Duration: audDur, Data: []byte{0xFF, 0x02}, CodecType: av.AAC},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x03}, CodecType: av.H264},
		{Idx: 1, DTS: 2 * audDur, Duration: audDur, Data: []byte{0xFF, 0x03}, CodecType: av.AAC},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != len(inPkts) {
		t.Fatalf("packet count: want %d, got %d", len(inPkts), len(outPkts))
	}

	// Count packets per stream index.
	vidCount, audCount := 0, 0

	for _, p := range outPkts {
		switch p.Idx {
		case 0:
			vidCount++
		case 1:
			audCount++
		default:
			t.Errorf("unexpected stream index %d", p.Idx)
		}
	}

	if vidCount != 3 {
		t.Errorf("video packet count: want 3, got %d", vidCount)
	}

	if audCount != 3 {
		t.Errorf("audio packet count: want 3, got %d", audCount)
	}
}

func TestMP4Muxer_MoovContainsStts(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x03}, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)

	if !bytes.Contains(data, []byte("stts")) {
		t.Error("mp4 output does not contain stts box")
	}
}

func TestMP4Muxer_MoovContainsStss(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x03}, CodecType: av.H264},
		{Idx: 0, KeyFrame: true, DTS: 3 * vidDur, Duration: vidDur, Data: []byte{0x65, 0x04}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 4 * vidDur, Duration: vidDur, Data: []byte{0x41, 0x05}, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)

	if !bytes.Contains(data, []byte("stss")) {
		t.Error("mp4 output does not contain stss box (sync sample table)")
	}
}

func TestMP4Muxer_MoovContainsStco(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, Data: []byte{0x41, 0x02}, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)

	// stco or co64 are both valid chunk offset boxes.
	if !bytes.Contains(data, []byte("stco")) && !bytes.Contains(data, []byte("co64")) {
		t.Error("mp4 output does not contain stco or co64 box (chunk offset table)")
	}
}

func TestMP4Muxer_PTSOffset_Preserved(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	ptsOff := 66 * time.Millisecond // 5940 ticks @ 90000 Hz — exact

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, PTSOffset: ptsOff, Data: []byte{0x65, 0x01}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: vidDur, Duration: vidDur, PTSOffset: 0, Data: []byte{0x41, 0x02}, CodecType: av.H264},
		{Idx: 0, KeyFrame: false, DTS: 2 * vidDur, Duration: vidDur, PTSOffset: ptsOff, Data: []byte{0x41, 0x03}, CodecType: av.H264},
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

func TestMP4Muxer_ZeroPackets_ValidFile(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	data := muxFmt(t, allFormats[1], streams, nil)

	if len(data) == 0 {
		t.Fatal("output is empty")
	}

	r := bytes.NewReader(data)

	typ1 := readBoxType(t, r)
	if typ1 != "ftyp" {
		t.Errorf("box 0: want ftyp, got %q", typ1)
	}

	typ2 := readBoxType(t, r)
	if typ2 != "moov" {
		t.Errorf("box 1: want moov, got %q", typ2)
	}
}

func TestMP4Muxer_LargePayload(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	largeData := make([]byte, 1<<20) // 1 MB
	largeData[0] = 0x65              // IDR NALU type
	for i := 1; i < len(largeData); i++ {
		largeData[i] = byte(i)
	}

	inPkts := []av.Packet{
		{Idx: 0, KeyFrame: true, DTS: 0, Duration: vidDur, Data: largeData, CodecType: av.H264},
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)

	// The mdat box must be large enough to hold the 1 MB payload.
	// Find the mdat box and check its size.
	mdatIdx := bytes.Index(data, []byte("mdat"))
	if mdatIdx < 4 {
		t.Fatal("mdat box not found in output")
	}

	mdatSize := binary.BigEndian.Uint32(data[mdatIdx-4 : mdatIdx])
	// mdat size includes the 8-byte header, so payload must be at least 1 MB.
	if mdatSize < uint32(len(largeData))+8 {
		t.Errorf("mdat size %d too small for 1 MB payload", mdatSize)
	}
}

func TestMP4Muxer_ManySamples(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	const numPkts = 1000

	inPkts := make([]av.Packet, numPkts)
	for i := range inPkts {
		inPkts[i] = av.Packet{
			Idx:       0,
			KeyFrame:  i%30 == 0, // keyframe every 30 packets
			DTS:       time.Duration(i) * vidDur,
			Duration:  vidDur,
			Data:      []byte{0x65, byte(i >> 8), byte(i)},
			CodecType: av.H264,
		}
	}

	data := muxFmt(t, allFormats[1], streams, inPkts)
	_, outPkts := demuxFmt(t, allFormats[1], data)

	if len(outPkts) != numPkts {
		t.Fatalf("packet count: want %d, got %d", numPkts, len(outPkts))
	}
}
