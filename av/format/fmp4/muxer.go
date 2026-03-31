// Package fmp4 implements a Fragmented MP4 (ISO 14496-12) muxer that satisfies
// the av.Muxer interface. Fragments are emitted on each video keyframe; if only
// audio tracks are present, a fragment is flushed on each packet.
package fmp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

var (
	// ErrHeaderAlreadyWritten is returned if WriteHeader is called more than once.
	ErrHeaderAlreadyWritten = errors.New("fmp4: WriteHeader already called")
	// ErrTrailerAlreadyWritten is returned if WriteTrailer is called more than once.
	ErrTrailerAlreadyWritten = errors.New("fmp4: WriteTrailer already called")
	// ErrUnsupportedCodec is returned for codec types with no fMP4 mapping.
	ErrUnsupportedCodec = errors.New("fmp4: unsupported codec")
	// ErrHeaderNotWritten is returned if WritePacket is called before WriteHeader.
	ErrHeaderNotWritten = errors.New("fmp4: WriteHeader not called")
)

// trunFlags selects which per-sample fields appear in a trun box.
const (
	trunFlagDataOffset      = 0x000001
	trunFlagSampleDuration  = 0x000100
	trunFlagSampleSize      = 0x000200
	trunFlagSampleFlags     = 0x000400
	trunFlagSampleCTSOffset = 0x000800

	// videoTrunFlags includes all four per-sample fields (used for video tracks).
	videoTrunFlags = trunFlagDataOffset | trunFlagSampleDuration |
		trunFlagSampleSize | trunFlagSampleFlags | trunFlagSampleCTSOffset

	// audioTrunFlags omits flags and CTS (audio has no B-frames).
	audioTrunFlags = trunFlagDataOffset | trunFlagSampleDuration | trunFlagSampleSize
)

// Sample flags encoding (ISO 14496-12 §8.8.8.1).
const (
	sampleFlagsKeyframe    = uint32(0x02000000) // depends_on = 2 (no other), non_sync = 0
	sampleFlagsNonKeyframe = uint32(0x01010000) // depends_on = 1, non_sync = 1
)

// sample holds one compressed media sample buffered for the current fragment.
type sample struct {
	duration           uint32
	size               uint32
	flags              uint32
	ptsOffset          int32 // composition-time offset in timescale units (B-frame support)
	data               []byte
	extra              []byte // optional analytics payload (JSON) from av.Packet.Analytics
	presentationTimeMS int64  // absolute presentation time in milliseconds for emsg
	dts                int64  // decode time in timescale units; used to back-fill preceding sample duration
}

// trackState maintains per-track muxing state.
type trackState struct {
	streamIdx uint16       // av.Stream.Idx this track maps to
	id        uint32       // 1-based MP4 track ID
	codec     av.CodecData // codec descriptor from WriteHeader
	timescale uint32       // clock ticks per second for timestamps
	samples   []sample     // samples accumulated for the current fragment
	baseTime  int64        // baseMediaDecodeTime of the first sample in this fragment
	nextTime  int64        // running decode time (updated after each fragment flush)
	hasVideo  bool         // true if this is a video track
}

// Muxer serialises Packets into a Fragmented MP4 byte stream.
// Use NewMuxer to construct.
type Muxer struct {
	w        io.Writer
	tracks   []*trackState          // ordered by writeHeader stream order
	trackMap map[uint16]*trackState // keyed by av.Stream.Idx
	seqNum   uint32
	emsgID   uint32 // monotonically increasing emsg event id
	written      bool // WriteHeader has been called
	closed       bool // WriteTrailer has been called
	writerClosed bool // Close has been called

	// dtsOffset is the DTS of the very first packet received; it is subtracted
	// from all subsequent timestamps so the output timeline starts at zero.
	// This normalises camera wall-clock timestamps to a player-friendly origin.
	dtsOffset    time.Duration
	dtsOffsetSet bool

	// waitingForKeyframe is true when the muxer has a video track but has not
	// yet seen its first IDR. All packets (video and audio) are dropped until
	// the first video keyframe arrives, guaranteeing that every fragment emitted
	// starts on a sync sample. This covers the common late-join scenario where a
	// new consumer attaches mid-GOP and would otherwise receive leading P-frames.
	waitingForKeyframe bool
}

// NewMuxer returns a Muxer that writes fMP4 data to w.
func NewMuxer(w io.Writer) *Muxer {
	return &Muxer{
		w:        w,
		trackMap: make(map[uint16]*trackState),
	}
}

// WriteHeader writes the fMP4 initialisation segment (ftyp + moov) to the
// underlying writer. It must be called exactly once before WritePacket.
func (m *Muxer) WriteHeader(_ context.Context, streams []av.Stream) error {
	if m.written {
		return ErrHeaderAlreadyWritten
	}

	nextTrackID := uint32(1)

	for _, s := range streams {
		ts, err := newTrackState(s, nextTrackID)
		if errors.Is(err, ErrUnsupportedCodec) {
			continue // silently skip codecs fMP4 cannot represent
		}

		if err != nil {
			return err
		}

		m.tracks = append(m.tracks, ts)
		m.trackMap[s.Idx] = ts
		nextTrackID++
	}

	if len(m.tracks) == 0 {
		return ErrUnsupportedCodec
	}

	init := buildInitSegment(m.tracks)
	if _, err := m.w.Write(init); err != nil {
		return err
	}

	m.written = true
	m.waitingForKeyframe = m.hasVideoTracks()

	return nil
}

// WritePacket buffers a sample and emits a fragment whenever a video keyframe
// is received (or immediately for audio-only streams).
func (m *Muxer) WritePacket(_ context.Context, pkt av.Packet) error {
	if !m.written {
		return ErrHeaderNotWritten
	}

	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	ts := m.trackMap[pkt.Idx]
	if ts == nil {
		return nil // unknown stream index – skip gracefully
	}

	// Drop all packets until the first video IDR so that every emitted fragment
	// starts on a sync sample. This handles late-joining consumers that attach
	// mid-GOP: without this guard the first flushed fragment would begin with
	// non-reference frames, which MSE and most players reject.
	if m.waitingForKeyframe {
		if !ts.hasVideo || !pkt.KeyFrame {
			return nil
		}

		m.waitingForKeyframe = false
	}

	// Normalise timestamps to a zero-based origin on the first packet so that
	// camera wall-clock DTS values don't produce huge or negative tfdt times.
	if !m.dtsOffsetSet {
		m.dtsOffset = pkt.DTS
		m.dtsOffsetSet = true
	}

	pkt.DTS -= m.dtsOffset

	// A video keyframe triggers a fragment flush of all pending samples.
	if ts.hasVideo && pkt.KeyFrame && m.hasAnySamples() {
		if err := m.flushFragment(); err != nil {
			return err
		}
	}

	newDTS := dtsToTimescale(pkt.DTS, ts.timescale)

	if len(ts.samples) == 0 {
		ts.baseTime = newDTS
	} else {
		// Back-fill the previous sample's duration from the DTS delta when the
		// demuxer did not supply one (pkt.Duration == 0 for most video sources).
		prev := &ts.samples[len(ts.samples)-1]
		if prev.duration == 0 && newDTS > prev.dts {
			prev.duration = uint32(newDTS - prev.dts)
		}
	}

	ts.samples = append(ts.samples, makeSample(pkt, ts))

	// Audio-only: flush every packet so latency stays low.
	if !m.hasVideoTracks() {
		return m.flushFragment()
	}

	return nil
}

// WriteTrailer flushes any buffered samples and signals end of stream.
// A non-nil upstreamError is recorded but does not affect the flush.
func (m *Muxer) WriteTrailer(_ context.Context, _ error) error {
	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	m.closed = true

	if m.hasAnySamples() {
		return m.flushFragment()
	}

	return nil
}

// Close implements av.MuxCloser. It flushes any remaining data (best-effort),
// then closes the underlying writer if it implements io.Closer.
// Safe to call multiple times.
func (m *Muxer) Close() error {
	if !m.closed {
		_ = m.WriteTrailer(context.Background(), nil)
	}

	if m.writerClosed {
		return nil
	}

	m.writerClosed = true

	if c, ok := m.w.(io.Closer); ok {
		return c.Close()
	}

	return nil
}

// WriteCodecChange implements av.CodecChanger. It flushes the current fragment,
// updates the codec for each listed stream, then writes a new init segment so
// downstream decoders can reinitialise. Only the streams listed in changed are
// updated; all others remain as declared in WriteHeader.
func (m *Muxer) WriteCodecChange(_ context.Context, changed []av.Stream) error {
	if !m.written || m.closed {
		return nil
	}

	// Flush pending samples before rewriting codec state.
	if m.hasAnySamples() {
		if err := m.flushFragment(); err != nil {
			return err
		}
	}

	// Update each changed stream's trackState in place, preserving timing.
	for _, s := range changed {
		old := m.trackMap[s.Idx]
		if old == nil {
			continue
		}

		fresh, err := newTrackState(s, old.id)
		if err != nil {
			return err
		}

		fresh.nextTime = old.nextTime

		for i, t := range m.tracks {
			if t.streamIdx == s.Idx {
				m.tracks[i] = fresh

				break
			}
		}

		m.trackMap[s.Idx] = fresh
	}

	// Emit a new init segment so decoders can pick up the new codec parameters.
	_, err := m.w.Write(buildInitSegment(m.tracks))

	return err
}

// ── internal helpers ─────────────────────────────────────────────────────────

func newTrackState(s av.Stream, id uint32) (*trackState, error) {
	var ts uint32

	isVideo := false

	switch c := s.Codec.(type) {
	case h264parser.CodecData:
		ts = c.TimeScale()
		isVideo = true
	case h265parser.CodecData:
		ts = c.TimeScale()
		isVideo = true
	case aacparser.CodecData:
		ts = uint32(c.SampleRate())
	case pcm.FLACCodecData:
		ts = uint32(c.SampleRate())
	default:
		return nil, ErrUnsupportedCodec
	}

	return &trackState{
		streamIdx: s.Idx,
		id:        id,
		codec:     s.Codec,
		timescale: ts,
		samples:   nil,
		baseTime:  0,
		nextTime:  0,
		hasVideo:  isVideo,
	}, nil
}

func makeSample(pkt av.Packet, ts *trackState) sample {
	dur := durationToTimescale(pkt.Duration, ts.timescale)
	cts := int32(0)

	if pkt.PTSOffset != 0 {
		cts = int32(durationToTimescale(pkt.PTSOffset, ts.timescale))
	}

	flags := uint32(0)

	var data []byte

	if ts.hasVideo {
		if pkt.KeyFrame {
			flags = sampleFlagsKeyframe
		} else {
			flags = sampleFlagsNonKeyframe
		}

		data = normalizeVideoToAVCC(pkt.Data)
	} else {
		data = make([]byte, len(pkt.Data))
		copy(data, pkt.Data)
	}

	var extra []byte

	if pkt.Analytics != nil {
		if b, err := json.Marshal(pkt.Analytics); err == nil {
			extra = b
		}
	}

	return sample{
		duration:           uint32(dur),
		size:               uint32(len(data)),
		flags:              flags,
		ptsOffset:          cts,
		data:               data,
		extra:              extra,
		presentationTimeMS: (pkt.DTS + pkt.PTSOffset).Milliseconds(),
		dts:                dtsToTimescale(pkt.DTS, ts.timescale),
	}
}

// normalizeVideoToAVCC ensures H.264/H.265 sample data is in AVCC format
// (4-byte big-endian length prefix per NALU), as required by ISO 14496-15.
// All pipeline demuxers already produce AVCC, so this is a defensive pass-through.
// It handles non-AVCC input (Annex-B, raw NALU) for robustness.
func normalizeVideoToAVCC(data []byte) []byte {
	nalus, typ := parser.SplitNALUs(data)
	switch typ {
	case parser.NALUAvcc:
		out := make([]byte, len(data))
		copy(out, data)

		return out
	case parser.NALUAnnexb:
		return h264parser.AnnexBToAVCC(nalus)
	case parser.NALURaw:
		fallthrough
	default:
		// Raw single NALU — prepend 4-byte BE length.
		out := make([]byte, 4+len(data))
		binary.BigEndian.PutUint32(out, uint32(len(data)))
		copy(out[4:], data)

		return out
	}
}

func (m *Muxer) hasAnySamples() bool {
	for _, ts := range m.tracks {
		if len(ts.samples) > 0 {
			return true
		}
	}

	return false
}

func (m *Muxer) hasVideoTracks() bool {
	for _, ts := range m.tracks {
		if ts.hasVideo {
			return true
		}
	}

	return false
}

func (m *Muxer) flushFragment() error {
	m.seqNum++

	active := make([]*trackState, 0, len(m.tracks))

	for _, ts := range m.tracks {
		if len(ts.samples) > 0 {
			active = append(active, ts)
		}
	}

	// Patch the last sample's duration for each active track.
	// The back-fill in WritePacket covers all but the final sample of each
	// fragment (there is no subsequent packet to trigger it). Carry forward
	// the preceding sample's duration as the best available estimate; for a
	// constant-frame-rate stream this is exact.
	for _, ts := range active {
		last := &ts.samples[len(ts.samples)-1]
		if last.duration == 0 && len(ts.samples) >= 2 {
			last.duration = ts.samples[len(ts.samples)-2].duration
		}
	}

	if len(active) == 0 {
		return nil
	}

	// Compute total mdat payload size to derive data offsets in trun.
	dataOffsets := make([]uint32, len(active))
	moofSize := estimateMoofSize(active)
	offset := moofSize + 8 // 8 = mdat box header

	for i, ts := range active {
		dataOffsets[i] = offset
		for _, s := range ts.samples {
			offset += s.size
		}
	}

	emsgs, nextID := collectEmsg(active, m.emsgID)
	m.emsgID = nextID

	moof := buildMoof(active, m.seqNum, dataOffsets)
	mdat := buildMdat(active)

	if len(emsgs) > 0 {
		if _, err := m.w.Write(emsgs); err != nil {
			return err
		}
	}

	if _, err := m.w.Write(moof); err != nil {
		return err
	}

	if _, err := m.w.Write(mdat); err != nil {
		return err
	}

	// Advance nextTime and clear sample buffers.
	for _, ts := range active {
		for _, s := range ts.samples {
			ts.nextTime += int64(s.duration)
		}

		ts.samples = ts.samples[:0]
	}

	return nil
}

// dtsToTimescale converts a time.Duration DTS into timescale ticks.
func dtsToTimescale(d time.Duration, timescale uint32) int64 {
	return int64(d) * int64(timescale) / int64(time.Second)
}

// durationToTimescale converts a time.Duration into timescale ticks.
func durationToTimescale(d time.Duration, timescale uint32) int64 {
	return int64(d) * int64(timescale) / int64(time.Second)
}

// ── fMP4 box builders ────────────────────────────────────────────────────────

func buildInitSegment(tracks []*trackState) []byte {
	var b bytes.Buffer
	b.Write(buildFtyp())
	b.Write(buildMoov(tracks))

	return b.Bytes()
}

// buildFtyp builds an ftyp box compatible with ISOBMFF and common players.
func buildFtyp() []byte {
	var p bytes.Buffer
	p.WriteString("isom")       // major_brand
	writeUint32(&p, 0x00000200) // minor_version = 512
	p.WriteString("isom")       // compatible_brand
	p.WriteString("iso5")
	p.WriteString("avc1")
	p.WriteString("mp41")

	return makeBox("ftyp", p.Bytes())
}

func buildMoov(tracks []*trackState) []byte {
	var p bytes.Buffer
	p.Write(buildMvhd(len(tracks)))

	for _, ts := range tracks {
		p.Write(buildTrak(ts))
	}

	p.Write(buildMvex(tracks))

	return makeBox("moov", p.Bytes())
}

func buildMvhd(numTracks int) []byte {
	var p bytes.Buffer

	writeUint32(&p, 0)          // creation_time
	writeUint32(&p, 0)          // modification_time
	writeUint32(&p, 1000)       // timescale (movie-level, ms)
	writeUint32(&p, 0)          // duration = 0 (live)
	writeUint32(&p, 0x00010000) // rate = 1.0
	writeUint16(&p, 0x0100)     // volume = 1.0
	writeZeros(&p, 10)          // reserved
	// identity matrix
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x40000000)
	writeZeros(&p, 24) // pre_defined
	writeUint32(&p, uint32(numTracks+1))

	return makeFullBox("mvhd", 0, 0, p.Bytes())
}

func buildTrak(ts *trackState) []byte {
	var p bytes.Buffer
	p.Write(buildTkhd(ts))
	p.Write(buildMdia(ts))

	return makeBox("trak", p.Bytes())
}

func buildTkhd(ts *trackState) []byte {
	var p bytes.Buffer
	// flags: 0x3 = track_enabled | track_in_movie
	writeUint32(&p, 0)     // creation_time
	writeUint32(&p, 0)     // modification_time
	writeUint32(&p, ts.id) // track_ID
	writeUint32(&p, 0)     // reserved
	writeUint32(&p, 0)     // duration = 0 (live)
	writeZeros(&p, 8)      // reserved
	writeUint16(&p, 0)     // layer
	writeUint16(&p, 0)     // alternate_group
	// volume: 1.0 for audio, 0 for video
	if ts.hasVideo {
		writeUint16(&p, 0)
	} else {
		writeUint16(&p, 0x0100)
	}

	writeUint16(&p, 0) // reserved
	// identity matrix
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x40000000)
	// width and height (16.16 fixed point)
	if v, ok := ts.codec.(av.VideoCodecData); ok {
		writeUint32(&p, uint32(v.Width())<<16)
		writeUint32(&p, uint32(v.Height())<<16)
	} else {
		writeUint32(&p, 0)
		writeUint32(&p, 0)
	}

	return makeFullBox("tkhd", 0, 3, p.Bytes())
}

func buildMdia(ts *trackState) []byte {
	var p bytes.Buffer
	p.Write(buildMdhd(ts))
	p.Write(buildHdlr(ts))
	p.Write(buildMinf(ts))

	return makeBox("mdia", p.Bytes())
}

func buildMdhd(ts *trackState) []byte {
	var p bytes.Buffer
	writeUint32(&p, 0)            // creation_time
	writeUint32(&p, 0)            // modification_time
	writeUint32(&p, ts.timescale) // timescale
	writeUint32(&p, 0)            // duration = 0 (live)
	// language: 'und' = 0x55c4
	writeUint16(&p, 0x55c4)
	writeUint16(&p, 0) // pre_defined

	return makeFullBox("mdhd", 0, 0, p.Bytes())
}

func buildHdlr(ts *trackState) []byte {
	var p bytes.Buffer
	writeUint32(&p, 0) // pre_defined

	if ts.hasVideo {
		p.WriteString("vide")
		writeZeros(&p, 12)
		p.WriteString("VideoHandler")
	} else {
		p.WriteString("soun")
		writeZeros(&p, 12)
		p.WriteString("SoundHandler")
	}

	p.WriteByte(0) // null terminator

	return makeFullBox("hdlr", 0, 0, p.Bytes())
}

func buildMinf(ts *trackState) []byte {
	var p bytes.Buffer

	if ts.hasVideo {
		p.Write(buildVmhd())
	} else {
		p.Write(buildSmhd())
	}

	p.Write(buildDinf())
	p.Write(buildStbl(ts))

	return makeBox("minf", p.Bytes())
}

func buildVmhd() []byte {
	var p bytes.Buffer
	writeUint16(&p, 0) // graphicsMode
	writeZeros(&p, 6)  // opcolor

	return makeFullBox("vmhd", 0, 1, p.Bytes())
}

func buildSmhd() []byte {
	var p bytes.Buffer
	writeUint16(&p, 0) // balance
	writeUint16(&p, 0) // reserved

	return makeFullBox("smhd", 0, 0, p.Bytes())
}

func buildDinf() []byte {
	// dref with one self-contained 'url ' entry (flags=1)
	var urlp bytes.Buffer

	dref := makeFullBox("url ", 0, 1, urlp.Bytes())

	var drefp bytes.Buffer
	writeUint32(&drefp, 1) // entry_count
	drefp.Write(dref)

	return makeBox("dinf", makeFullBox("dref", 0, 0, drefp.Bytes()))
}

func buildStbl(ts *trackState) []byte {
	var p bytes.Buffer
	p.Write(buildStsd(ts))
	p.Write(buildStts())
	p.Write(buildStsc())
	p.Write(buildStsz())
	p.Write(buildStco())

	return makeBox("stbl", p.Bytes())
}

func buildStsd(ts *trackState) []byte {
	var entry []byte

	switch c := ts.codec.(type) {
	case h264parser.CodecData:
		entry = buildAvc1(c)
	case h265parser.CodecData:
		entry = buildHev1(c)
	case aacparser.CodecData:
		entry = buildMp4a(c)
	case pcm.FLACCodecData:
		entry = buildFLaC(c)
	default:
		entry = []byte{}
	}

	var p bytes.Buffer
	writeUint32(&p, 1) // entry_count
	p.Write(entry)

	return makeFullBox("stsd", 0, 0, p.Bytes())
}

// buildAvc1 writes the avc1 (H.264) sample entry with an embedded avcC box.
func buildAvc1(c h264parser.CodecData) []byte {
	var p bytes.Buffer
	writeZeros(&p, 6)  // reserved
	writeUint16(&p, 1) // data_reference_index
	writeUint16(&p, 0) // pre_defined
	writeUint16(&p, 0) // reserved
	writeZeros(&p, 12) // pre_defined[3]
	writeUint16(&p, uint16(c.Width()))
	writeUint16(&p, uint16(c.Height()))
	writeUint32(&p, 0x00480000) // horiz_resolution 72 dpi
	writeUint32(&p, 0x00480000) // vert_resolution 72 dpi
	writeUint32(&p, 0)          // reserved
	writeUint16(&p, 1)          // frame_count
	writeZeros(&p, 32)          // compressorname
	writeUint16(&p, 0x0018)     // depth
	writeInt16(&p, -1)          // pre_defined

	// avcC box
	avcc := makeBox("avcC", c.AVCDecoderConfRecordBytes())
	p.Write(avcc)

	return makeBox("avc1", p.Bytes())
}

// buildHev1 writes the hev1 (H.265) sample entry with an embedded hvcC box.
func buildHev1(c h265parser.CodecData) []byte {
	var p bytes.Buffer
	writeZeros(&p, 6)  // reserved
	writeUint16(&p, 1) // data_reference_index
	writeUint16(&p, 0) // pre_defined
	writeUint16(&p, 0) // reserved
	writeZeros(&p, 12) // pre_defined[3]
	writeUint16(&p, uint16(c.Width()))
	writeUint16(&p, uint16(c.Height()))
	writeUint32(&p, 0x00480000)
	writeUint32(&p, 0x00480000)
	writeUint32(&p, 0)
	writeUint16(&p, 1)
	writeZeros(&p, 32)
	writeUint16(&p, 0x0018)
	writeInt16(&p, -1)

	// hvcC box
	hvcc := makeBox("hvcC", c.HEVCDecoderConfigurationRecordBytes())
	p.Write(hvcc)

	return makeBox("hev1", p.Bytes())
}

// buildMp4a writes the mp4a (AAC) sample entry with an embedded esds box.
func buildMp4a(c aacparser.CodecData) []byte {
	var p bytes.Buffer
	writeZeros(&p, 6)  // reserved
	writeUint16(&p, 1) // data_reference_index
	// AudioSampleEntry fields
	writeUint32(&p, 0) // reserved
	writeUint32(&p, 0) // reserved
	writeUint16(&p, uint16(c.ChannelLayout().Count()))
	writeUint16(&p, 16) // samplesize
	writeUint16(&p, 0)  // pre_defined
	writeUint16(&p, 0)  // reserved
	writeUint32(&p, uint32(c.SampleRate())<<16)

	// esds box
	p.Write(buildEsds(c))

	return makeBox("mp4a", p.Bytes())
}

// buildFLaC writes the fLaC (FLAC) AudioSampleEntry + dfLaC child box.
// Reference: https://wiki.xiph.org/FLAC_in_ISOBMFF
func buildFLaC(c pcm.FLACCodecData) []byte {
	var p bytes.Buffer
	writeZeros(&p, 6)
	writeUint16(&p, 1) // data_reference_index
	writeUint32(&p, 0) // reserved
	writeUint32(&p, 0) // reserved
	writeUint16(&p, uint16(c.ChannelLayout().Count()))
	writeUint16(&p, 16) // samplesize
	writeUint16(&p, 0)  // pre_defined
	writeUint16(&p, 0)  // reserved
	writeUint32(&p, uint32(c.SampleRate())<<16)
	p.Write(makeFullBox("dfLa", 0, 0, c.STREAMINFOBlock())) // 38-byte STREAMINFO block

	return makeBox("fLaC", p.Bytes())
}

// buildEsds writes an esds (MPEG-4 audio Elementary Stream Descriptor) box.
func buildEsds(c aacparser.CodecData) []byte {
	asc := c.MPEG4AudioConfigBytes()

	// Build inner descriptors bottom-up.
	//
	// DecoderSpecificInfo (tag 0x05)
	dsi := buildDescriptor(0x05, asc)

	// DecoderConfigDescriptor (tag 0x04)
	var dcBody bytes.Buffer
	dcBody.WriteByte(0x40)  // objectTypeIndication = MPEG-4 Audio
	dcBody.WriteByte(0x15)  // streamType=0x05 (audio) <<2 | 0x01
	writeUint24(&dcBody, 0) // bufferSizeDB
	writeUint32(&dcBody, 0) // maxBitrate
	writeUint32(&dcBody, 0) // avgBitrate
	dcBody.Write(dsi)
	dc := buildDescriptor(0x04, dcBody.Bytes())

	// SLConfigDescriptor (tag 0x06)
	sl := buildDescriptor(0x06, []byte{0x02}) // predefined = 2

	// ES_Descriptor (tag 0x03)
	var esBody bytes.Buffer
	writeUint16(&esBody, 1) // ES_ID
	esBody.WriteByte(0x00)  // flags
	esBody.Write(dc)
	esBody.Write(sl)
	esDesc := buildDescriptor(0x03, esBody.Bytes())

	var p bytes.Buffer
	writeUint32(&p, 0) // version + flags (full box prefix, already in makeFullBox)
	p.Write(esDesc)

	return makeBox("esds", p.Bytes())
}

// buildDescriptor writes an MPEG-4 expandable class descriptor.
func buildDescriptor(tag byte, payload []byte) []byte {
	n := len(payload)

	var b bytes.Buffer
	b.WriteByte(tag)
	// Encode length as expandable class length (multibyte if needed).
	for n > 0x7f {
		b.WriteByte(byte(n>>21) | 0x80)
		n &= 0x1fffff
	}

	b.WriteByte(byte(n))
	b.Write(payload)

	return b.Bytes()
}

func buildMvex(tracks []*trackState) []byte {
	var p bytes.Buffer
	for _, ts := range tracks {
		p.Write(buildTrex(ts))
	}

	return makeBox("mvex", p.Bytes())
}

func buildTrex(ts *trackState) []byte {
	var p bytes.Buffer
	writeUint32(&p, ts.id) // track_ID
	writeUint32(&p, 1)     // default_sample_description_index
	writeUint32(&p, 0)     // default_sample_duration
	writeUint32(&p, 0)     // default_sample_size
	writeUint32(&p, 0)     // default_sample_flags

	return makeFullBox("trex", 0, 0, p.Bytes())
}

// Empty stts / stsc / stsz / stco (required even in fragmented files).

func buildStts() []byte {
	var p bytes.Buffer
	writeUint32(&p, 0) // entry_count

	return makeFullBox("stts", 0, 0, p.Bytes())
}

func buildStsc() []byte {
	var p bytes.Buffer
	writeUint32(&p, 0)

	return makeFullBox("stsc", 0, 0, p.Bytes())
}

func buildStsz() []byte {
	var p bytes.Buffer
	writeUint32(&p, 0) // sample_size
	writeUint32(&p, 0) // sample_count

	return makeFullBox("stsz", 0, 0, p.Bytes())
}

func buildStco() []byte {
	var p bytes.Buffer
	writeUint32(&p, 0)

	return makeFullBox("stco", 0, 0, p.Bytes())
}

// ── Fragment builders ────────────────────────────────────────────────────────

// estimateMoofSize calculates the exact byte length of the moof box.
func estimateMoofSize(active []*trackState) uint32 {
	size := uint32(8 + 16) // moof header + mfhd

	for _, ts := range active {
		n := len(ts.samples)
		trunPayload := 4 + 4 + n*perSampleSize(ts) // sample_count + data_offset + samples
		trunSize := 8 + 4 + trunPayload            // box header + version+flags
		// traf = box header(8) + tfhd(16) + tfdt(20) + trun
		size += uint32(8 + 16 + 20 + trunSize)
	}

	return size
}

// perSampleSize returns the number of bytes per sample entry in trun.
func perSampleSize(ts *trackState) int {
	if ts.hasVideo {
		return 16 // duration + size + flags + cts_offset
	}

	return 8 // duration + size
}

func buildMoof(active []*trackState, seqNum uint32, dataOffsets []uint32) []byte {
	var p bytes.Buffer
	p.Write(buildMfhd(seqNum))

	for i, ts := range active {
		p.Write(buildTraf(ts, dataOffsets[i]))
	}

	return makeBox("moof", p.Bytes())
}

func buildMfhd(seqNum uint32) []byte {
	var p bytes.Buffer
	writeUint32(&p, seqNum)

	return makeFullBox("mfhd", 0, 0, p.Bytes())
}

func buildTraf(ts *trackState, dataOffset uint32) []byte {
	var p bytes.Buffer
	p.Write(buildTfhd(ts.id))
	p.Write(buildTfdt(ts.baseTime))
	p.Write(buildTrun(ts, int32(dataOffset)))

	return makeBox("traf", p.Bytes())
}

// buildTfhd builds a Track Fragment Header with default-base-is-moof set.
// This means data_offset in trun is relative to the start of the moof box.
func buildTfhd(trackID uint32) []byte {
	var p bytes.Buffer
	writeUint32(&p, trackID)

	// flags = 0x020000 (default-base-is-moof)
	return makeFullBox("tfhd", 0, 0x020000, p.Bytes())
}

func buildTfdt(baseTime int64) []byte {
	var p bytes.Buffer
	writeUint64(&p, uint64(baseTime))

	// version=1 → 64-bit baseMediaDecodeTime
	return makeFullBox("tfdt", 1, 0, p.Bytes())
}

func buildTrun(ts *trackState, dataOffset int32) []byte {
	var flags uint32
	if ts.hasVideo {
		flags = videoTrunFlags
	} else {
		flags = audioTrunFlags
	}

	var p bytes.Buffer
	writeUint32(&p, uint32(len(ts.samples)))
	writeInt32(&p, dataOffset) // data_offset

	for _, s := range ts.samples {
		writeUint32(&p, s.duration)

		writeUint32(&p, s.size)

		if ts.hasVideo {
			writeUint32(&p, s.flags)
			writeInt32(&p, s.ptsOffset)
		}
	}

	// version=1 for signed composition-time-offset support
	return makeFullBox("trun", 1, flags, p.Bytes())
}

// buildMdat builds the mdat box containing all sample data.
func buildMdat(active []*trackState) []byte {
	var payload bytes.Buffer

	for _, ts := range active {
		for _, s := range ts.samples {
			payload.Write(s.data)
		}
	}

	return makeBox("mdat", payload.Bytes())
}

// buildEmsg builds an emsg (Event Message) box per ISO 14496-12 §12.5.3
// version 1. presentationTimeMS is the absolute presentation time in
// milliseconds; id is a monotonically increasing per-stream event counter;
// data is the raw event payload (e.g. bounding-box JSON).
//
// emsg boxes must appear before the moof box of the fragment they annotate so
// that MSE delivers the DataCue event synchronously with the video frame.
func buildEmsg(presentationTimeMS int64, id uint32, data []byte) []byte {
	const (
		schemeIDURI = "urn:vtpl:analytics:1"
		value       = ""
		timescale   = uint32(1000) // millisecond resolution
		eventDurInf = uint32(0xFFFFFFFF)
	)

	var p bytes.Buffer
	p.WriteString(schemeIDURI)
	p.WriteByte(0) // null-terminate scheme_id_uri
	p.WriteString(value)
	p.WriteByte(0) // null-terminate value
	writeUint32(&p, timescale)
	writeUint64(&p, uint64(presentationTimeMS))
	writeUint32(&p, eventDurInf)
	writeUint32(&p, id)
	p.Write(data)

	// version=1 → 64-bit presentation_time field
	return makeFullBox("emsg", 1, 0, p.Bytes())
}

// collectEmsg returns one emsg box for every sample that carries extra data,
// using seqBase as the starting event id. Returns the boxes and the next id.
func collectEmsg(active []*trackState, seqBase uint32) ([]byte, uint32) {
	var out bytes.Buffer

	id := seqBase

	for _, ts := range active {
		for _, s := range ts.samples {
			if len(s.extra) > 0 {
				out.Write(buildEmsg(s.presentationTimeMS, id, s.extra))
				id++
			}
		}
	}

	return out.Bytes(), id
}

// ── box writing primitives ───────────────────────────────────────────────────

// makeBox builds a regular (non-full) ISO BMFF box.
func makeBox(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:], size)
	copy(b[4:8], typ)
	copy(b[8:], payload)

	return b
}

// makeFullBox builds a full-box with version and flags prepended to payload.
func makeFullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	vf := make([]byte, 4+len(payload))
	vf[0] = version
	vf[1] = byte(flags >> 16)
	vf[2] = byte(flags >> 8)
	vf[3] = byte(flags)
	copy(vf[4:], payload)

	return makeBox(typ, vf)
}

func writeUint16(b *bytes.Buffer, v uint16) {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], v)
	b.Write(buf[:])
}

func writeInt16(b *bytes.Buffer, v int16) {
	writeUint16(b, uint16(v))
}

func writeUint32(b *bytes.Buffer, v uint32) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	b.Write(buf[:])
}

func writeInt32(b *bytes.Buffer, v int32) {
	writeUint32(b, uint32(v))
}

func writeUint24(b *bytes.Buffer, v uint32) {
	b.WriteByte(byte(v >> 16))
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v))
}

func writeUint64(b *bytes.Buffer, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	b.Write(buf[:])
}

func writeZeros(b *bytes.Buffer, n int) {
	b.Write(make([]byte, n))
}
