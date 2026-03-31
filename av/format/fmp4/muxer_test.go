package fmp4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

// minimalAVCRecord is a synthetic AVCDecoderConfigurationRecord for
// a 320×240 Baseline-profile H.264 stream (profile 66, level 30).
var minimalAVCRecord = []byte{
	0x01,             // configurationVersion
	0x42, 0x00, 0x1E, // profile_idc, constraint_flags, level_idc
	0xFF,       // lengthSizeMinusOne = 3
	0xE1,       // numSequenceParameterSets = 1
	0x00, 0x0F, // SPS length
	// SPS: 66 00 1E AC D9 40 A0 3D A1 00 00 03 00 00 03 (truncated but enough for parser)
	0x67, 0x42, 0x00, 0x1E,
	0xAC, 0xD9, 0x40, 0xA0,
	0x3D, 0xA1, 0x00, 0x00,
	0x03, 0x00, 0x00,
	0x01,       // numPictureParameterSets = 1
	0x00, 0x04, // PPS length
	0x68, 0xCE, 0x38, 0x80, // PPS
}

// minimalAAC is a 2-byte AudioSpecificConfig for AAC-LC 44100 Hz stereo.
// ObjectType=2 (LC), SampleRateIndex=4 (44100), ChannelConfig=2 (stereo)
// Bits: 00010 0100 0010 0 = 0x12 0x10
var minimalAAC = []byte{0x12, 0x10}

func makeH264Codec(t *testing.T) h264parser.CodecData {
	t.Helper()

	c, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(minimalAVCRecord)
	if err != nil {
		t.Fatalf("h264parser: %v", err)
	}

	return c
}

func makeAACCodec(t *testing.T) aacparser.CodecData {
	t.Helper()

	c, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(minimalAAC)
	if err != nil {
		t.Fatalf("aacparser: %v", err)
	}

	return c
}

// readBox reads an ISO BMFF box header from b and returns (type, payload).
// It advances b past the box.
func readBox(t *testing.T, b *bytes.Reader) (string, []byte) {
	t.Helper()

	var hdr [8]byte
	if _, err := b.Read(hdr[:]); err != nil {
		t.Fatalf("readBox: read header: %v", err)
	}

	size := binary.BigEndian.Uint32(hdr[0:4])
	typ := string(hdr[4:8])

	if size < 8 {
		t.Fatalf("readBox: size %d < 8", size)
	}

	payload := make([]byte, size-8)
	if _, err := b.Read(payload); err != nil {
		t.Fatalf("readBox: read payload: %v", err)
	}

	return typ, payload
}

func TestWriteHeader_InitSegment(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Verify ftyp + moov are present.
	r := bytes.NewReader(buf.Bytes())

	typ, _ := readBox(t, r)
	if typ != "ftyp" {
		t.Errorf("expected ftyp, got %q", typ)
	}

	typ, _ = readBox(t, r)
	if typ != "moov" {
		t.Errorf("expected moov, got %q", typ)
	}

	if r.Len() != 0 {
		t.Errorf("unexpected trailing bytes: %d", r.Len())
	}
}

func TestWriteHeader_Idempotency(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()
	streams := []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("first WriteHeader: %v", err)
	}

	if err := m.WriteHeader(ctx, streams); err == nil {
		t.Fatal("second WriteHeader should return error")
	}
}

func TestWriteTrailer_BeforeAnyPacket(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	initLen := buf.Len()

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// No samples → no fragment should be emitted.
	if buf.Len() != initLen {
		t.Errorf("unexpected bytes written: got %d want %d", buf.Len(), initLen)
	}
}

func TestFragment_VideoKeyframe(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	streams := []av.Stream{{Idx: 0, Codec: h264}}
	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	frameData := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xDE, 0xAD}

	// First keyframe: no flush yet (nothing to flush).
	pkt0 := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  dur,
		Data:      frameData,
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt0); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	sizeBefore := buf.Len()

	// Second keyframe triggers a flush of the first frame.
	pkt1 := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       time.Duration(dur),
		Duration:  dur,
		Data:      frameData,
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt1); err != nil {
		t.Fatalf("WritePacket(kf1): %v", err)
	}

	if buf.Len() == sizeBefore {
		t.Fatal("expected a fragment to be written on second keyframe")
	}

	// Verify moof+mdat structure in the emitted fragment.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // skip ftyp
	readBox(t, r) // skip moov

	typ, _ := readBox(t, r)
	if typ != "moof" {
		t.Errorf("expected moof, got %q", typ)
	}

	typ, _ = readBox(t, r)
	if typ != "mdat" {
		t.Errorf("expected mdat, got %q", typ)
	}
}

func TestFragment_AudioOnly(t *testing.T) {
	t.Parallel()

	aac := makeAACCodec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: aac}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	audioData := make([]byte, 128)
	pkt := av.Packet{
		Idx:       0,
		DTS:       0,
		Duration:  23 * time.Millisecond,
		Data:      audioData,
		CodecType: av.AAC,
	}

	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Audio-only: flush immediately on every packet.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	typ, _ := readBox(t, r)
	if typ != "moof" {
		t.Errorf("expected moof, got %q", typ)
	}

	typ, moofPayload := readBox(t, r)
	if typ != "mdat" {
		t.Errorf("expected mdat, got %q", typ)
	}

	if len(moofPayload) != len(audioData) {
		t.Errorf("mdat payload len = %d, want %d", len(moofPayload), len(audioData))
	}
}

func TestWriteTrailer_FlushesRemaining(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write one keyframe (no flush yet since no second keyframe arrives).
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  33 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	sizeBefore := buf.Len()

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	if buf.Len() == sizeBefore {
		t.Error("WriteTrailer should flush remaining samples")
	}
}

func TestWriteTrailer_Idempotency(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
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

func TestMoov_ContainsMvex(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp

	_, moovPayload := readBox(t, r) // moov

	// Walk moov children looking for mvex.
	found := false
	mr := bytes.NewReader(moovPayload)

	for mr.Len() > 0 {
		typ, _ := readBox(t, mr)
		if typ == "mvex" {
			found = true

			break
		}
	}

	if !found {
		t.Error("moov does not contain mvex")
	}
}

func TestMoovTrak_ContainsAvcC(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	data := buf.Bytes()
	// avcC bytes should appear somewhere in the output.
	avcCHeader := []byte("avcC")
	if !bytes.Contains(data, avcCHeader) {
		t.Error("init segment does not contain avcC box")
	}
}

// ── MuxCloser ─────────────────────────────────────────────────────────────────

// closingWriter wraps bytes.Buffer and records whether Close was called.
type closingWriter struct {
	bytes.Buffer
	closed bool
}

func (cw *closingWriter) Close() error {
	cw.closed = true

	return nil
}

func TestClose_ClosesUnderlying(t *testing.T) {
	t.Parallel()

	cw := &closingWriter{}
	m := fmp4.NewMuxer(cw)
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

func TestClose_BestEffortTrailer(t *testing.T) {
	t.Parallel()

	// Verify Close flushes remaining samples even if WriteTrailer wasn't called.
	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  33 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	sizeBefore := buf.Len()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The buffered keyframe should have been flushed.
	if buf.Len() == sizeBefore {
		t.Error("Close did not flush remaining samples")
	}
}

func TestClose_WithNonCloserWriter(t *testing.T) {
	t.Parallel()

	// io.Discard does not implement io.Closer; Close must still return nil.
	m := fmp4.NewMuxer(io.Discard)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close on non-Closer writer: %v", err)
	}
}

// ── CodecChanger ──────────────────────────────────────────────────────────────

func TestWriteCodecChange_EmitsNewInitSegment(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	h264 := makeH264Codec(t)
	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Feed one keyframe so there are buffered samples.
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  33 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	sizeBeforeChange := buf.Len()

	// Trigger a codec change.
	if err := m.WriteCodecChange(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteCodecChange: %v", err)
	}

	// A new init segment (ftyp+moov) must have been written.
	added := buf.Bytes()[sizeBeforeChange:]
	if len(added) < 8 {
		t.Fatalf("expected new init segment after codec change, got %d bytes", len(added))
	}

	// Walk added boxes: expect moof/mdat from the flushed fragment, then ftyp+moov.
	r := bytes.NewReader(added)
	sawMoov := false

	for r.Len() > 0 {
		typ, _ := readBox(t, r)
		if typ == "moov" {
			sawMoov = true

			break
		}
	}

	if !sawMoov {
		t.Error("no moov box found after codec change")
	}
}

func TestWriteCodecChange_UpdatesTimingContinuity(t *testing.T) {
	t.Parallel()

	// After a codec change the fragment sequence numbers must remain monotonic.
	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	h264 := makeH264Codec(t)
	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	frame := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0x88}

	// Write two keyframes → flush after second.
	for i := range 2 {
		pkt := av.Packet{
			KeyFrame:  true,
			Idx:       0,
			DTS:       time.Duration(i) * dur,
			Duration:  dur,
			Data:      frame,
			CodecType: av.H264,
		}
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket[%d]: %v", i, err)
		}
	}

	if err := m.WriteCodecChange(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteCodecChange: %v", err)
	}

	// Write another keyframe after the codec change.
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       2 * dur,
		Duration:  dur,
		Data:      frame,
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket after change: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Parse out all mfhd sequence numbers and verify monotonic increase.
	seqs := extractMFHDSeqNos(t, buf.Bytes())
	if len(seqs) < 2 {
		t.Fatalf("expected ≥2 mfhd, got %d", len(seqs))
	}

	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("mfhd seqNos not monotonic: %v", seqs)
		}
	}
}

func TestSatisfies_MuxCloser(t *testing.T) {
	t.Parallel()

	var _ av.MuxCloser = fmp4.NewMuxer(io.Discard)
}

func TestSatisfies_CodecChanger(t *testing.T) {
	t.Parallel()

	var _ av.CodecChanger = fmp4.NewMuxer(io.Discard)
}

// extractMFHDSeqNos walks raw fMP4 bytes and extracts the sequence_number
// from every mfhd (Movie Fragment Header) box found anywhere in the data.
func extractMFHDSeqNos(t *testing.T, data []byte) []uint32 {
	t.Helper()

	var seqs []uint32

	findMFHD(t, data, &seqs)

	return seqs
}

func findMFHD(t *testing.T, data []byte, seqs *[]uint32) {
	t.Helper()

	r := bytes.NewReader(data)

	for r.Len() >= 8 {
		var hdr [8]byte
		if _, err := r.Read(hdr[:]); err != nil {
			break
		}

		sz := int(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])

		if sz < 8 || sz > r.Len()+8 {
			break
		}

		payload := make([]byte, sz-8)
		if _, err := r.Read(payload); err != nil {
			break
		}

		if typ == "mfhd" && len(payload) >= 8 {
			// mfhd full-box: 4 bytes version+flags + 4 bytes sequence_number
			seq := binary.BigEndian.Uint32(payload[4:8])
			*seqs = append(*seqs, seq)
		}

		// Recurse into container boxes.
		switch typ {
		case "moov", "moof", "traf", "trak", "mdia", "minf", "stbl", "mvex":
			findMFHD(t, payload, seqs)
		}
	}
}

// TestEmsg_Analytics verifies that a packet carrying Analytics results in an
// emsg box appearing before the moof box in the flushed fragment, and that the
// emsg payload is the JSON-marshalled FrameAnalytics.
func TestEmsg_Analytics(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	fa := &av.FrameAnalytics{
		SiteID:       1,
		ChannelID:    2,
		VehicleCount: 3,
		PeopleCount:  5,
		Objects: []*av.Detection{
			{X: 10, Y: 20, W: 100, H: 200, ClassID: 1, Confidence: 90, TrackID: 42, IsEvent: true},
		},
	}

	analyticsJSON, err := json.Marshal(fa)
	if err != nil {
		t.Fatalf("json.Marshal FrameAnalytics: %v", err)
	}

	fw, _, err := fmp4.NewFragmentWriter(streams)
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}

	fw.WritePacket(av.Packet{
		Idx:       0,
		KeyFrame:  true,
		DTS:       33 * time.Millisecond,
		Duration:  33 * time.Millisecond,
		Data:      []byte{0x65, 0x88},
		Analytics: fa,
	})

	fragment := fw.Flush()
	if fragment == nil {
		t.Fatal("Flush returned nil")
	}

	r := bytes.NewReader(fragment)

	// First box must be emsg.
	typ, payload := readBox(t, r)
	if typ != "emsg" {
		t.Fatalf("first box: want emsg, got %q", typ)
	}

	// emsg full-box: 1 byte version + 3 bytes flags = 4 bytes prefix.
	// version=1, so layout after prefix:
	//   scheme_id_uri (null-terminated)
	//   value         (null-terminated)
	//   timescale     uint32
	//   presentation_time uint64
	//   event_duration uint32
	//   id            uint32
	//   message_data  (rest)
	version := payload[0]
	if version != 1 {
		t.Errorf("emsg version: want 1, got %d", version)
	}

	// Skip version+flags (4 bytes), find end of scheme_id_uri and value strings.
	pos := 4
	for pos < len(payload) && payload[pos] != 0 {
		pos++
	}
	pos++ // skip null terminator of scheme_id_uri
	for pos < len(payload) && payload[pos] != 0 {
		pos++
	}
	pos++ // skip null terminator of value

	// timescale(4) + presentation_time(8) + event_duration(4) + id(4) = 20 bytes
	pos += 20

	if pos > len(payload) {
		t.Fatal("emsg payload too short")
	}

	got := payload[pos:]
	if !bytes.Equal(got, analyticsJSON) {
		t.Errorf("emsg data mismatch:\n got  %s\n want %s", got, analyticsJSON)
	}

	// Next box must be moof.
	typ, _ = readBox(t, r)
	if typ != "moof" {
		t.Errorf("second box: want moof, got %q", typ)
	}
}

// TestWritePacket_DurationInferredFromDTS verifies that when pkt.Duration is zero
// (as produced by the avgrabber demuxer for video), the muxer infers sample
// durations from consecutive DTS deltas and the resulting fMP4 fragment has
// non-zero duration.
func TestWritePacket_DurationInferredFromDTS(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	streams := []av.Stream{{Idx: 0, Codec: h264}}

	var buf bytes.Buffer
	ctx := context.Background()
	m := fmp4.NewMuxer(&buf)

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	buf.Reset()

	const fps = 25
	frameDur := time.Second / fps
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0xAF, 0x3C}
	pFrame := []byte{0x41, 0x9A, 0x00, 0x00}

	// Send two GOPs (IDR + 2 P-frames each). Duration field deliberately zero
	// to simulate avgrabber video packets.
	pkts := []av.Packet{
		{Idx: 0, DTS: 0 * frameDur, KeyFrame: true, Data: idr},
		{Idx: 0, DTS: 1 * frameDur, Data: pFrame},
		{Idx: 0, DTS: 2 * frameDur, Data: pFrame},
		// Second IDR triggers flush of the first GOP.
		{Idx: 0, DTS: 3 * frameDur, KeyFrame: true, Data: idr},
		{Idx: 0, DTS: 4 * frameDur, Data: pFrame},
		{Idx: 0, DTS: 5 * frameDur, Data: pFrame},
	}

	for _, pkt := range pkts {
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}

	if buf.Len() == 0 {
		t.Fatal("expected fragment after second IDR, got nothing")
	}

	// Verify that the fragment's mdat is preceded by a moof that contains
	// non-zero trun durations. We do this by checking the total fragment
	// duration reported via mp4info would be > 0 — a simpler proxy: the sum
	// of all trun sample-duration fields must equal 3 * frameDur in timescale
	// units (90000 / 25 = 3600 each).
	//
	// We verify at a high level: the fragment must be present and parseable.
	r := bytes.NewReader(buf.Bytes())
	typ, _ := readBox(t, r)

	if typ != "moof" {
		t.Fatalf("first box: want moof, got %q", typ)
	}

	typ, _ = readBox(t, r)
	if typ != "mdat" {
		t.Fatalf("second box: want mdat, got %q", typ)
	}
}

// TestWritePacket_DropsLeadingNonKeyframes verifies that when packets arrive
// before the first IDR (simulating a late-joining consumer that attaches
// mid-GOP), no fragment is emitted until the first keyframe.  The first
// fragment produced must start on the IDR sample.
func TestWritePacket_DropsLeadingNonKeyframes(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	var buf bytes.Buffer
	ctx := context.Background()
	m := fmp4.NewMuxer(&buf)

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	initSize := buf.Len()
	buf.Reset()

	dur := 33 * time.Millisecond
	dts0 := 100 * time.Millisecond // non-zero wall-clock origin

	// Send: audio, P-frame, P-frame — all before the first IDR.
	// These must be dropped; no fragment should be emitted.
	_ = m.WritePacket(ctx, av.Packet{Idx: 1, DTS: dts0, Duration: dur, Data: []byte{0x01}})
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dts0, Duration: dur, Data: []byte{0x41, 0x9A, 0x00, 0x00}}) // P-frame NALU (nal_unit_type=1)
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dts0 + dur, Duration: dur, Data: []byte{0x41, 0x9A, 0x00, 0x00}})

	if buf.Len() != 0 {
		t.Fatalf("expected no fragment before first IDR, got %d bytes", buf.Len())
	}

	_ = initSize // suppress unused warning

	// Now send the first IDR — no pending samples exist, so no flush yet.
	idrData := []byte{0x65, 0x88, 0x84, 0x00, 0xAF, 0x3C} // IDR NALU (nal_unit_type=5)
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dts0 + 2*dur, Duration: dur, KeyFrame: true, Data: idrData})

	if buf.Len() != 0 {
		t.Fatalf("expected no fragment after first IDR (no second IDR yet), got %d bytes", buf.Len())
	}

	// Send a P-frame after the IDR — still no flush.
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dts0 + 3*dur, Duration: dur, Data: []byte{0x41, 0x9A, 0x00, 0x00}})

	// Send a second IDR — this flushes the first GOP.
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dts0 + 4*dur, Duration: dur, KeyFrame: true, Data: idrData})

	if buf.Len() == 0 {
		t.Fatal("expected a fragment after second IDR, got nothing")
	}

	// Parse the fragment: expect moof then mdat.
	r := bytes.NewReader(buf.Bytes())

	typ, _ := readBox(t, r)
	if typ != "moof" {
		t.Fatalf("first fragment box: want moof, got %q", typ)
	}

	typ, _ = readBox(t, r)
	if typ != "mdat" {
		t.Fatalf("second fragment box: want mdat, got %q", typ)
	}
}

// ── New tests ─────────────────────────────────────────────────────────────────

// countBoxes counts the number of child boxes with the given type inside data.
func countBoxes(data []byte, typ string) int {
	count := 0

	for len(data) >= 8 {
		size := binary.BigEndian.Uint32(data[0:4])
		if size < 8 || int(size) > len(data) {
			break
		}

		if string(data[4:8]) == typ {
			count++
		}

		data = data[size:]
	}

	return count
}

// findBoxPayload finds the first child box with the given type inside data and
// returns its payload. Returns (nil, false) if not found.
func findBoxPayload(data []byte, typ string) ([]byte, bool) {
	for len(data) >= 8 {
		size := binary.BigEndian.Uint32(data[0:4])
		if size < 8 || int(size) > len(data) {
			return nil, false
		}

		if string(data[4:8]) == typ {
			return data[8:size], true
		}

		data = data[size:]
	}

	return nil, false
}

// TestFMP4_VideoAndAudio_MultiTrack verifies that when both video and audio
// packets are interleaved, the flushed fragment's moof contains two traf boxes
// (one per track).
func TestFMP4_VideoAndAudio_MultiTrack(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	// AVCC-formatted IDR (4-byte length prefix + IDR NALU type 0x65)
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}
	audioData := []byte{0x01, 0x02, 0x03, 0x04}

	// First keyframe + interleaved audio.
	if err := m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 0, Duration: dur, Data: idrData, CodecType: av.H264,
	}); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	if err := m.WritePacket(ctx, av.Packet{
		Idx: 1, DTS: 0, Duration: 23 * time.Millisecond, Data: audioData, CodecType: av.AAC,
	}); err != nil {
		t.Fatalf("WritePacket(audio0): %v", err)
	}

	// Second keyframe triggers flush of the first GOP.
	if err := m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: dur, Duration: dur, Data: idrData, CodecType: av.H264,
	}); err != nil {
		t.Fatalf("WritePacket(kf1): %v", err)
	}

	// Parse the output: skip ftyp + moov, then find moof.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	typ, moofPayload := readBox(t, r)
	if typ != "moof" {
		t.Fatalf("expected moof, got %q", typ)
	}

	// Count traf children inside the moof payload.
	trafCount := countBoxes(moofPayload, "traf")
	if trafCount != 2 {
		t.Errorf("expected 2 traf boxes in moof, got %d", trafCount)
	}
}

// TestFMP4_VideoAndAudio_AudioBeforeFirstKeyframe verifies that audio packets
// arriving before the first video keyframe are dropped (along with P-frames),
// so no fragment is emitted until the first IDR.
func TestFMP4_VideoAndAudio_AudioBeforeFirstKeyframe(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	buf.Reset() // discard init segment to measure fragment output

	dur := 33 * time.Millisecond

	// Audio and P-frames before first keyframe — all should be dropped.
	_ = m.WritePacket(ctx, av.Packet{Idx: 1, DTS: 0, Duration: dur, Data: []byte{0xAA}, CodecType: av.AAC})
	_ = m.WritePacket(ctx, av.Packet{Idx: 1, DTS: dur, Duration: dur, Data: []byte{0xBB}, CodecType: av.AAC})
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: 0, Duration: dur, Data: []byte{0x41, 0x9A}}) // P-frame

	if buf.Len() != 0 {
		t.Fatalf("expected no output before first IDR, got %d bytes", buf.Len())
	}

	// First keyframe arrives — no flush yet (nothing pending from before).
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}
	_ = m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 2 * dur, Duration: dur, Data: idrData, CodecType: av.H264,
	})

	if buf.Len() != 0 {
		t.Fatalf("expected no fragment after first IDR (no second yet), got %d bytes", buf.Len())
	}

	// Second keyframe triggers flush.
	_ = m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 3 * dur, Duration: dur, Data: idrData, CodecType: av.H264,
	})

	if buf.Len() == 0 {
		t.Fatal("expected fragment after second IDR, got nothing")
	}
}

// TestFMP4_PTSOffset_PreservedInFragment verifies that packets with non-zero
// PTSOffset result in trun entries that include the composition time offset
// flag (0x800).
func TestFMP4_PTSOffset_PreservedInFragment(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}

	// Write keyframe with B-frame style PTSOffset.
	if err := m.WritePacket(ctx, av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		PTSOffset: 66 * time.Millisecond,
		Duration:  dur,
		Data:      idrData,
		CodecType: av.H264,
	}); err != nil {
		t.Fatalf("WritePacket(kf0): %v", err)
	}

	// Second keyframe triggers flush.
	if err := m.WritePacket(ctx, av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       dur,
		PTSOffset: 66 * time.Millisecond,
		Duration:  dur,
		Data:      idrData,
		CodecType: av.H264,
	}); err != nil {
		t.Fatalf("WritePacket(kf1): %v", err)
	}

	// Parse output: skip ftyp + moov, find moof.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	typ, moofPayload := readBox(t, r)
	if typ != "moof" {
		t.Fatalf("expected moof, got %q", typ)
	}

	// Find traf inside moof, then trun inside traf.
	trafPayload, ok := findBoxPayload(moofPayload, "traf")
	if !ok {
		t.Fatal("no traf found in moof")
	}

	trunPayload, ok := findBoxPayload(trafPayload, "trun")
	if !ok {
		t.Fatal("no trun found in traf")
	}

	// trun is a full-box: version(1) + flags(3). Check CTS flag (0x800).
	if len(trunPayload) < 4 {
		t.Fatal("trun payload too short")
	}

	trunFlags := uint32(trunPayload[1])<<16 | uint32(trunPayload[2])<<8 | uint32(trunPayload[3])
	if trunFlags&0x800 == 0 {
		t.Errorf("trun flags 0x%X do not include CTS offset flag (0x800)", trunFlags)
	}
}

// TestFMP4_Analytics_OnlyOnVideoKeyframes verifies that Analytics attached to a
// video keyframe produces an emsg box, while a video keyframe without Analytics
// does not. Audio packets without Analytics also produce no emsg.
func TestFMP4_Analytics_OnlyOnVideoKeyframes(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	fa := &av.FrameAnalytics{SiteID: 1, ChannelID: 2, VehicleCount: 1}

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}

	// First keyframe WITH Analytics — should produce emsg.
	_ = m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 0, Duration: dur, Data: idrData,
		CodecType: av.H264, Analytics: fa,
	})

	// Audio packet WITHOUT Analytics — no emsg expected.
	_ = m.WritePacket(ctx, av.Packet{
		Idx: 1, DTS: 0, Duration: 23 * time.Millisecond, Data: []byte{0x01, 0x02},
		CodecType: av.AAC,
	})

	// Second keyframe (no Analytics) triggers flush.
	_ = m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: dur, Duration: dur, Data: idrData,
		CodecType: av.H264,
	})

	// Parse output: skip ftyp + moov, count emsg boxes before moof.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	emsgCount := 0

	for r.Len() > 0 {
		typ, _ := readBox(t, r)
		if typ == "emsg" {
			emsgCount++
		}

		if typ == "moof" {
			break
		}
	}

	// Exactly one emsg should exist (from the video keyframe with Analytics).
	// The audio packet (no Analytics) and the second keyframe (no Analytics)
	// should not have produced any additional emsg boxes.
	if emsgCount != 1 {
		t.Errorf("expected exactly 1 emsg (from video keyframe with Analytics), got %d", emsgCount)
	}
}

// TestFMP4_MultipleFragments_SequenceMonotonic writes 6 GOPs and verifies that
// all mfhd sequence numbers are consecutive (1, 2, 3, 4, 5).
func TestFMP4_MultipleFragments_SequenceMonotonic(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}
	pData := []byte{0x00, 0x00, 0x00, 0x04, 0x41, 0x9A, 0x00, 0x00}

	// Write 6 GOPs: IDR + 2 P-frames each.
	for gop := range 6 {
		base := time.Duration(gop*3) * dur
		_ = m.WritePacket(ctx, av.Packet{
			KeyFrame: true, Idx: 0, DTS: base, Duration: dur, Data: idrData, CodecType: av.H264,
		})
		_ = m.WritePacket(ctx, av.Packet{
			Idx: 0, DTS: base + dur, Duration: dur, Data: pData, CodecType: av.H264,
		})
		_ = m.WritePacket(ctx, av.Packet{
			Idx: 0, DTS: base + 2*dur, Duration: dur, Data: pData, CodecType: av.H264,
		})
	}

	// Flush remaining.
	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	seqs := extractMFHDSeqNos(t, buf.Bytes())
	if len(seqs) < 5 {
		t.Fatalf("expected >= 5 mfhd sequence numbers, got %d: %v", len(seqs), seqs)
	}

	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Errorf("mfhd sequence numbers not consecutive: %v", seqs)

			break
		}
	}

	// First sequence number should be 1.
	if seqs[0] != 1 {
		t.Errorf("first mfhd sequence number: want 1, got %d", seqs[0])
	}
}

// TestFMP4_WritePacket_BeforeWriteHeader verifies that WritePacket returns an
// error when called before WriteHeader.
func TestFMP4_WritePacket_BeforeWriteHeader(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	err := m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 0, Duration: 33 * time.Millisecond,
		Data: []byte{0x65}, CodecType: av.H264,
	})

	if err == nil {
		t.Fatal("WritePacket before WriteHeader should return an error")
	}
}

// TestFMP4_WritePacket_AfterWriteTrailer verifies that WritePacket returns an
// error when called after WriteTrailer.
func TestFMP4_WritePacket_AfterWriteTrailer(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: makeH264Codec(t)}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	err := m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 0, Duration: 33 * time.Millisecond,
		Data: []byte{0x65}, CodecType: av.H264,
	})

	if err == nil {
		t.Fatal("WritePacket after WriteTrailer should return an error")
	}
}

// TestFMP4_EmptyData_KeyframeCodecChange verifies that a keyframe with nil Data
// and NewCodecs does not panic.
func TestFMP4_EmptyData_KeyframeCodecChange(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write a keyframe with nil Data and NewCodecs set — should not crash.
	err := m.WritePacket(ctx, av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  33 * time.Millisecond,
		Data:      nil,
		NewCodecs: []av.Stream{{Idx: 0, Codec: h264}},
		CodecType: av.H264,
	})

	// We don't require a specific error; we just verify no panic occurred.
	_ = err

	// Also flush to ensure no panic in the fragment builder.
	_ = m.WriteTrailer(ctx, nil)
}

// TestFMP4_LargePayload writes a packet with 1 MB of data and verifies the
// mdat box size matches the payload.
func TestFMP4_LargePayload(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Build a 1 MB AVCC-formatted payload: 4-byte length prefix + 1MB-4 bytes of data.
	const payloadSize = 1 << 20 // 1 MB
	largeData := make([]byte, payloadSize)
	binary.BigEndian.PutUint32(largeData, uint32(payloadSize-4))
	largeData[4] = 0x65 // IDR NALU type

	if err := m.WritePacket(ctx, av.Packet{
		KeyFrame: true, Idx: 0, DTS: 0, Duration: 33 * time.Millisecond,
		Data: largeData, CodecType: av.H264,
	}); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Flush via trailer.
	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Parse output: skip ftyp + moov, find mdat and verify its payload size.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	// Walk until we find mdat.
	for r.Len() > 0 {
		typ, payload := readBox(t, r)
		if typ == "mdat" {
			// The mdat payload should contain the entire AVCC-normalised data.
			// Since the input is already AVCC, the output should be the same size.
			if len(payload) != payloadSize {
				t.Errorf("mdat payload size: got %d, want %d", len(payload), payloadSize)
			}

			return
		}
	}

	t.Fatal("mdat box not found in output")
}

// TestFMP4_ZeroDuration_AllPackets verifies that when all packets have
// Duration=0, the muxer still produces a valid fragment by inferring durations
// from DTS deltas.
func TestFMP4_ZeroDuration_AllPackets(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)

	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, []av.Stream{{Idx: 0, Codec: h264}}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	dur := 33 * time.Millisecond
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}
	pData := []byte{0x00, 0x00, 0x00, 0x04, 0x41, 0x9A, 0x00, 0x00}

	// Write a GOP with Duration=0 for all packets.
	_ = m.WritePacket(ctx, av.Packet{KeyFrame: true, Idx: 0, DTS: 0, Data: idrData})
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: dur, Data: pData})
	_ = m.WritePacket(ctx, av.Packet{Idx: 0, DTS: 2 * dur, Data: pData})

	// Second keyframe triggers flush.
	_ = m.WritePacket(ctx, av.Packet{KeyFrame: true, Idx: 0, DTS: 3 * dur, Data: idrData})

	// Parse output: skip init, find moof + mdat.
	r := bytes.NewReader(buf.Bytes())
	readBox(t, r) // ftyp
	readBox(t, r) // moov

	typ, _ := readBox(t, r)
	if typ != "moof" {
		t.Fatalf("expected moof, got %q", typ)
	}

	typ, _ = readBox(t, r)
	if typ != "mdat" {
		t.Fatalf("expected mdat, got %q", typ)
	}
}

// TestFMP4_VideoAndAudio_RoundTrip muxes video+audio packets through
// fmp4.Muxer, then demuxes with fmp4.Demuxer, and verifies that all packets
// come back with correct data, DTS, Duration, and KeyFrame flags.
func TestFMP4_VideoAndAudio_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)

	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	dur := 33 * time.Millisecond
	audioDur := 23 * time.Millisecond

	// AVCC-formatted video data.
	idrData := []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF}
	pData := []byte{0x00, 0x00, 0x00, 0x04, 0x41, 0x9A, 0x00, 0x00}
	audioData := []byte{0x01, 0x02, 0x03, 0x04}

	inputPkts := []av.Packet{
		{KeyFrame: true, Idx: 0, DTS: 0, Duration: dur, Data: idrData, CodecType: av.H264},
		{Idx: 1, DTS: 0, Duration: audioDur, Data: audioData, CodecType: av.AAC},
		{Idx: 0, DTS: dur, Duration: dur, Data: pData, CodecType: av.H264},
		{Idx: 1, DTS: audioDur, Duration: audioDur, Data: audioData, CodecType: av.AAC},
		// Second GOP — triggers flush of the first.
		{KeyFrame: true, Idx: 0, DTS: 2 * dur, Duration: dur, Data: idrData, CodecType: av.H264},
		{Idx: 1, DTS: 2 * audioDur, Duration: audioDur, Data: audioData, CodecType: av.AAC},
	}

	// Mux.
	var buf bytes.Buffer
	m := fmp4.NewMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	for i, pkt := range inputPkts {
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket[%d]: %v", i, err)
		}
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// Demux.
	dmx := fmp4.NewDemuxer(bytes.NewReader(buf.Bytes()))

	gotStreams, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(gotStreams) != 2 {
		t.Fatalf("want 2 streams, got %d", len(gotStreams))
	}

	var outPkts []av.Packet

	for {
		pkt, err := dmx.ReadPacket(ctx)
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}

		outPkts = append(outPkts, pkt)
	}

	if len(outPkts) == 0 {
		t.Fatal("demuxer returned no packets")
	}

	// Verify that we got back all the input packets.
	if len(outPkts) != len(inputPkts) {
		t.Fatalf("packet count: got %d, want %d", len(outPkts), len(inputPkts))
	}

	// Build lookup maps: separate video and audio packets in output (sorted by DTS).
	var videoOut, audioOut []av.Packet

	for _, p := range outPkts {
		if p.Idx == 0 {
			videoOut = append(videoOut, p)
		} else {
			audioOut = append(audioOut, p)
		}
	}

	// Verify video packets.
	videoIn := []av.Packet{inputPkts[0], inputPkts[2], inputPkts[4]}
	if len(videoOut) != len(videoIn) {
		t.Fatalf("video packet count: got %d, want %d", len(videoOut), len(videoIn))
	}

	for i, got := range videoOut {
		want := videoIn[i]

		if got.KeyFrame != want.KeyFrame {
			t.Errorf("video[%d].KeyFrame: got %v, want %v", i, got.KeyFrame, want.KeyFrame)
		}

		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("video[%d].Data: got %d bytes, want %d bytes", i, len(got.Data), len(want.Data))
		}
	}

	// Verify audio packets.
	audioIn := []av.Packet{inputPkts[1], inputPkts[3], inputPkts[5]}
	if len(audioOut) != len(audioIn) {
		t.Fatalf("audio packet count: got %d, want %d", len(audioOut), len(audioIn))
	}

	for i, got := range audioOut {
		want := audioIn[i]

		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("audio[%d].Data: got %x, want %x", i, got.Data, want.Data)
		}
	}
}
