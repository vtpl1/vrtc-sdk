package mp4_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
	"github.com/vtpl1/vrtc-sdk/av/format/mp4"
)

// ── codec fixtures ────────────────────────────────────────────────────────────

// minimalAVCRecord is a synthetic AVCDecoderConfigurationRecord for a 320×240
// H.264 Baseline stream, shared with the fmp4 unit tests.
var minimalAVCRecord = []byte{
	0x01,
	0x42, 0x00, 0x1E,
	0xFF,
	0xE1,
	0x00, 0x0F,
	0x67, 0x42, 0x00, 0x1E,
	0xAC, 0xD9, 0x40, 0xA0,
	0x3D, 0xA1, 0x00, 0x00,
	0x03, 0x00, 0x00,
	0x01,
	0x00, 0x04,
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

// minimalAAC is a 2-byte AudioSpecificConfig for AAC-LC 44100 Hz stereo.
// ObjectType=2 (LC), SampleRateIndex=4 (44100), ChannelConfig=2 (stereo).
var minimalAAC = []byte{0x12, 0x10}

func makeAACCodec(t *testing.T) aacparser.CodecData {
	t.Helper()

	c, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(minimalAAC)
	if err != nil {
		t.Fatalf("aacparser: %v", err)
	}

	return c
}

// ── format registry ───────────────────────────────────────────────────────────

// formatSpec describes one container format with factories for its muxer
// and demuxer. Both factories accept *bytes.Reader (which satisfies both
// io.Reader and io.ReadSeeker, as required by the respective constructors).
type formatSpec struct {
	name       string
	newMuxer   func(w io.Writer) av.MuxCloser
	newDemuxer func(r *bytes.Reader) av.DemuxCloser
}

// allFormats lists the supported container formats in a canonical order.
// Tests iterate over this slice to generate all source×destination combinations.
var allFormats = []formatSpec{
	{
		name:       "fmp4",
		newMuxer:   func(w io.Writer) av.MuxCloser { return fmp4.NewMuxer(w) },
		newDemuxer: func(r *bytes.Reader) av.DemuxCloser { return fmp4.NewDemuxer(r) },
	},
	{
		name:       "mp4",
		newMuxer:   func(w io.Writer) av.MuxCloser { return mp4.NewMuxer(w) },
		newDemuxer: func(r *bytes.Reader) av.DemuxCloser { return mp4.NewDemuxer(r) },
	},
}

// ── pipeline helpers ──────────────────────────────────────────────────────────

// muxFmt muxes packets using the given format spec and returns the raw bytes.
func muxFmt(t *testing.T, f formatSpec, streams []av.Stream, pkts []av.Packet) []byte {
	t.Helper()

	var buf bytes.Buffer

	m := f.newMuxer(&buf)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("[%s] WriteHeader: %v", f.name, err)
	}

	for i, pkt := range pkts {
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("[%s] WritePacket[%d]: %v", f.name, i, err)
		}
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("[%s] WriteTrailer: %v", f.name, err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("[%s] MuxClose: %v", f.name, err)
	}

	return buf.Bytes()
}

func TestMP4Moov_NoMvex(t *testing.T) {
	t.Parallel()

	// Non-fragmented MP4 must NOT contain an mvex box.
	h264 := makeH264Codec(t)
	data := muxFmt(t, allFormats[1], []av.Stream{{Idx: 0, Codec: h264}}, nil)

	if bytes.Contains(data, []byte("mvex")) {
		t.Error("non-fragmented mp4 moov must not contain mvex")
	}
}

func TestMP4Moov_ContainsAvcC(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	data := muxFmt(t, allFormats[1], []av.Stream{{Idx: 0, Codec: h264}}, nil)

	if !bytes.Contains(data, []byte("avcC")) {
		t.Error("mp4 moov does not contain avcC box")
	}
}

func TestMP4Moov_ContainsEsds(t *testing.T) {
	t.Parallel()

	aac := makeAACCodec(t)
	data := muxFmt(t, allFormats[1], []av.Stream{{Idx: 0, Codec: aac}}, nil)

	if !bytes.Contains(data, []byte("esds")) {
		t.Error("mp4 moov does not contain esds box")
	}
}

// ── cross-format helpers ─────────────────────────────────────────────────────

// avcc wraps a raw NALU in a 4-byte big-endian length prefix (AVCC format).
func avcc(nalu ...byte) []byte {
	out := make([]byte, 4+len(nalu))
	binary.BigEndian.PutUint32(out, uint32(len(nalu)))
	copy(out[4:], nalu)

	return out
}

// demuxAll reads all streams and packets from a demuxer built from raw bytes.
func demuxAll(t *testing.T, f formatSpec, data []byte) ([]av.Stream, []av.Packet) {
	t.Helper()

	dmx := f.newDemuxer(bytes.NewReader(data))

	streams, err := dmx.GetCodecs(context.Background())
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	var pkts []av.Packet
	for {
		pkt, err := dmx.ReadPacket(context.Background())
		if err != nil {
			break
		}

		pkts = append(pkts, pkt)
	}

	dmx.Close()

	return streams, pkts
}

// ── cross-format round-trip tests ────────────────────────────────────────────

func TestCrossFormat_VideoRoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Build 5 video packets in AVCC format.
	var pkts []av.Packet
	for i := range 5 {
		pkt := av.Packet{
			KeyFrame:  i == 0,
			Idx:       0,
			CodecType: av.H264,
			DTS:       time.Duration(i) * 40 * time.Millisecond,
			Duration:  40 * time.Millisecond,
			Data:      avcc(0x65, byte(i), 0xAB, 0xCD), // synthetic IDR NALU
		}
		pkts = append(pkts, pkt)
	}

	for _, src := range allFormats {
		for _, dst := range allFormats {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				t.Parallel()

				// First round-trip: mux → demux.
				data1 := muxFmt(t, src, streams, pkts)
				_, got1 := demuxAll(t, src, data1)

				// Second round-trip: re-mux with dst format → demux again.
				data2 := muxFmt(t, dst, streams, got1)
				_, got2 := demuxAll(t, dst, data2)

				if len(got2) != len(pkts) {
					t.Fatalf("want %d packets, got %d", len(pkts), len(got2))
				}

				for i, want := range pkts {
					got := got2[i]
					if got.KeyFrame != want.KeyFrame {
						t.Errorf("pkt[%d] KeyFrame: want %v, got %v", i, want.KeyFrame, got.KeyFrame)
					}

					if got.DTS != want.DTS {
						t.Errorf("pkt[%d] DTS: want %v, got %v", i, want.DTS, got.DTS)
					}

					if got.Duration != want.Duration {
						t.Errorf("pkt[%d] Duration: want %v, got %v", i, want.Duration, got.Duration)
					}

					if !bytes.Equal(got.Data, want.Data) {
						t.Errorf("pkt[%d] Data mismatch: want %x, got %x", i, want.Data, got.Data)
					}
				}
			})
		}
	}
}

func TestCrossFormat_AudioRoundTrip(t *testing.T) {
	t.Parallel()

	aac := makeAACCodec(t)
	streams := []av.Stream{{Idx: 0, Codec: aac}}

	// Build 5 AAC audio packets.
	var pkts []av.Packet
	for i := range 5 {
		pkt := av.Packet{
			Idx:       0,
			CodecType: av.AAC,
			DTS:       time.Duration(i) * 23 * time.Millisecond,
			Duration:  23 * time.Millisecond,
			Data:      []byte{0xDE, 0xAD, byte(i), 0x01, 0x02},
		}
		pkts = append(pkts, pkt)
	}

	for _, src := range allFormats {
		for _, dst := range allFormats {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				t.Parallel()

				data1 := muxFmt(t, src, streams, pkts)
				_, got1 := demuxAll(t, src, data1)

				data2 := muxFmt(t, dst, streams, got1)
				_, got2 := demuxAll(t, dst, data2)

				if len(got2) != len(pkts) {
					t.Fatalf("want %d packets, got %d", len(pkts), len(got2))
				}

				const tol = time.Millisecond // AAC 44100 Hz quantisation

				for i, want := range pkts {
					got := got2[i]

					if diff := got.DTS - want.DTS; diff < -tol || diff > tol {
						t.Errorf("pkt[%d] DTS: want %v, got %v (diff %v)", i, want.DTS, got.DTS, diff)
					}

					if diff := got.Duration - want.Duration; diff < -tol || diff > tol {
						t.Errorf("pkt[%d] Duration: want %v, got %v (diff %v)", i, want.Duration, got.Duration, diff)
					}

					if !bytes.Equal(got.Data, want.Data) {
						t.Errorf("pkt[%d] Data mismatch: want %x, got %x", i, want.Data, got.Data)
					}
				}
			})
		}
	}
}

func TestCrossFormat_VideoAndAudio_RoundTrip(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	aac := makeAACCodec(t)
	streams := []av.Stream{
		{Idx: 0, Codec: h264},
		{Idx: 1, Codec: aac},
	}

	// Interleave video (idx 0) and audio (idx 1) packets.
	pkts := []av.Packet{
		{KeyFrame: true, Idx: 0, CodecType: av.H264, DTS: 0, Duration: 40 * time.Millisecond, Data: avcc(0x65, 0x00)},
		{Idx: 1, CodecType: av.AAC, DTS: 0, Duration: 23 * time.Millisecond, Data: []byte{0xAA, 0x01}},
		{Idx: 0, CodecType: av.H264, DTS: 40 * time.Millisecond, Duration: 40 * time.Millisecond, Data: avcc(0x41, 0x01)},
		{Idx: 1, CodecType: av.AAC, DTS: 23 * time.Millisecond, Duration: 23 * time.Millisecond, Data: []byte{0xAA, 0x02}},
		{Idx: 0, CodecType: av.H264, DTS: 80 * time.Millisecond, Duration: 40 * time.Millisecond, Data: avcc(0x41, 0x02)},
		{Idx: 1, CodecType: av.AAC, DTS: 46 * time.Millisecond, Duration: 23 * time.Millisecond, Data: []byte{0xAA, 0x03}},
	}

	for _, src := range allFormats {
		for _, dst := range allFormats {
			t.Run(src.name+"_to_"+dst.name, func(t *testing.T) {
				t.Parallel()

				data1 := muxFmt(t, src, streams, pkts)
				_, got1 := demuxAll(t, src, data1)

				data2 := muxFmt(t, dst, streams, got1)
				_, got2 := demuxAll(t, dst, data2)

				if len(got2) != len(pkts) {
					t.Fatalf("want %d packets, got %d", len(pkts), len(got2))
				}

				// Group packets by stream index for order-independent comparison,
				// since demuxers may reorder interleaved tracks by DTS.
				wantByIdx := map[uint16][]av.Packet{}
				gotByIdx := map[uint16][]av.Packet{}

				for _, p := range pkts {
					wantByIdx[p.Idx] = append(wantByIdx[p.Idx], p)
				}

				for _, p := range got2 {
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
							t.Errorf("stream %d pkt[%d] Data mismatch: want %x, got %x", idx, i, want.Data, got.Data)
						}
					}
				}
			})
		}
	}
}

func TestCrossFormat_FMP4ContainsMvex(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// fmp4 is the first entry in allFormats.
	fmp4Fmt := allFormats[0]
	data := muxFmt(t, fmp4Fmt, streams, nil)

	if !bytes.Contains(data, []byte("mvex")) {
		t.Error("fragmented mp4 moov must contain mvex box")
	}
}

func TestCrossFormat_AllFormats_ContainAvcC(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	for _, f := range allFormats {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			data := muxFmt(t, f, streams, nil)

			if !bytes.Contains(data, []byte("avcC")) {
				t.Errorf("[%s] output does not contain avcC box", f.name)
			}
		})
	}
}

func TestCrossFormat_DurationPreserved(t *testing.T) {
	t.Parallel()

	h264 := makeH264Codec(t)
	streams := []av.Stream{{Idx: 0, Codec: h264}}

	// Packets with specific ms-level durations.
	durations := []time.Duration{
		33 * time.Millisecond,
		40 * time.Millisecond,
		17 * time.Millisecond,
		50 * time.Millisecond,
	}

	var pkts []av.Packet

	var dts time.Duration

	for i, dur := range durations {
		pkt := av.Packet{
			KeyFrame:  i == 0,
			Idx:       0,
			CodecType: av.H264,
			DTS:       dts,
			Duration:  dur,
			Data:      avcc(0x65, byte(i)),
		}
		pkts = append(pkts, pkt)
		dts += dur
	}

	for _, f := range allFormats {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()

			data := muxFmt(t, f, streams, pkts)
			_, got := demuxAll(t, f, data)

			if len(got) != len(pkts) {
				t.Fatalf("want %d packets, got %d", len(pkts), len(got))
			}

			const tolerance = time.Millisecond

			for i, want := range pkts {
				diff := got[i].Duration - want.Duration
				if diff < -tolerance || diff > tolerance {
					t.Errorf("pkt[%d] Duration: want %v, got %v (diff %v)", i, want.Duration, got[i].Duration, diff)
				}
			}
		})
	}
}
