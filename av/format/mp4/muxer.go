package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
)

// Sentinel errors returned by the muxer.
var (
	// ErrHeaderAlreadyWritten is returned when WriteHeader is called more than once.
	ErrHeaderAlreadyWritten = errors.New("mp4: WriteHeader already called")
	// ErrTrailerAlreadyWritten is returned when WriteTrailer is called more than once.
	ErrTrailerAlreadyWritten = errors.New("mp4: WriteTrailer already called")
	// ErrHeaderNotWritten is returned when WritePacket is called before WriteHeader.
	ErrHeaderNotWritten = errors.New("mp4: WriteHeader not called")
)

// ── bufferedSample / mp4Track ─────────────────────────────────────────────────

// bufferedSample holds one compressed sample pending serialisation.
type bufferedSample struct {
	isKey    bool
	duration uint32 // timescale units; 0 if not known
	ptsOff   int32  // composition time offset, timescale units
	data     []byte
}

// mp4Track holds per-track muxing state.
type mp4Track struct {
	streamIdx uint16
	id        uint32 // 1-based MP4 track ID
	codec     av.CodecData
	timescale uint32
	isVideo   bool
	samples   []bufferedSample
}

// ── Muxer ─────────────────────────────────────────────────────────────────────

// Muxer writes packets into a regular (non-fragmented) ISO MP4 container.
// All samples are buffered in memory; the complete file is written atomically
// by WriteTrailer or Close.
//
// Create with NewMuxer or Create; call WriteHeader once, then WritePacket for
// each packet, then WriteTrailer (or Close) when done.
type Muxer struct {
	w            io.Writer
	wc           io.Closer
	tracks       []*mp4Track
	trackMap     map[uint16]*mp4Track
	written      bool
	closed       bool
	writerClosed bool
}

// NewMuxer returns a Muxer that writes MP4 data to w.
// If w implements io.Closer, Close will delegate to it.
func NewMuxer(w io.Writer) *Muxer {
	m := &Muxer{w: w, trackMap: make(map[uint16]*mp4Track)}
	if wc, ok := w.(io.Closer); ok {
		m.wc = wc
	}

	return m
}

// Create opens (or truncates) the named file and returns a ready Muxer.
func Create(path string) (*Muxer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	return &Muxer{w: f, wc: f, trackMap: make(map[uint16]*mp4Track)}, nil
}

// WriteHeader declares the streams. No bytes are written to the underlying
// writer until WriteTrailer; all data is buffered in memory.
func (m *Muxer) WriteHeader(_ context.Context, streams []av.Stream) error {
	if m.written {
		return ErrHeaderAlreadyWritten
	}

	for _, s := range streams {
		ts, ok := timescaleForCodec(s.Codec)
		if !ok {
			continue // skip unsupported codecs silently
		}

		t := &mp4Track{
			streamIdx: s.Idx,
			id:        uint32(len(m.tracks) + 1),
			codec:     s.Codec,
			timescale: ts,
			isVideo:   s.Codec.Type().IsVideo(),
		}

		m.tracks = append(m.tracks, t)
		m.trackMap[s.Idx] = t
	}

	m.written = true

	return nil
}

// WritePacket buffers one compressed sample for later serialisation.
func (m *Muxer) WritePacket(_ context.Context, pkt av.Packet) error {
	if !m.written {
		return ErrHeaderNotWritten
	}

	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	t := m.trackMap[pkt.Idx]
	if t == nil {
		return nil // unknown stream — skip gracefully
	}

	dur := uint32(durationToTicks(pkt.Duration, t.timescale))
	cts := int32(durationToTicks(pkt.PTSOffset, t.timescale))

	data := make([]byte, len(pkt.Data))
	copy(data, pkt.Data)

	t.samples = append(t.samples, bufferedSample{
		isKey:    pkt.KeyFrame,
		duration: dur,
		ptsOff:   cts,
		data:     data,
	})

	return nil
}

// WriteTrailer serialises all buffered samples as a complete MP4 file and
// writes ftyp + moov + mdat to the underlying writer.
func (m *Muxer) WriteTrailer(_ context.Context, _ error) error {
	if m.closed {
		return ErrTrailerAlreadyWritten
	}

	m.closed = true

	return m.flush()
}

// Close calls WriteTrailer (best-effort) and then closes the underlying writer.
// Safe to call multiple times.
func (m *Muxer) Close() error {
	if !m.closed {
		_ = m.WriteTrailer(context.Background(), nil)
	}

	if m.writerClosed {
		return nil
	}

	m.writerClosed = true

	if m.wc != nil {
		return m.wc.Close()
	}

	return nil
}

// ── flush ─────────────────────────────────────────────────────────────────────

// flush builds and writes the complete MP4 file: ftyp + moov + mdat.
//
// Layout strategy (moov-first / "fast-start"):
//  1. Build ftyp (known, fixed size).
//  2. Build moov with placeholder stco values (all 0) to measure moovSize.
//  3. Compute mdatBase = ftypSize + moovSize + 8 (mdat box header).
//  4. Compute per-track chunk offsets using mdatBase.
//  5. Rebuild moov with correct stco values (same size as pass 1).
//  6. Write ftyp + moov_final + mdat.
func (m *Muxer) flush() error {
	ftyp := buildFtyp(m.tracks)
	ftypSize := int64(len(ftyp))

	// Pass 1: measure moov size.
	moovPass1 := m.buildMoov(nil)
	moovSize := int64(len(moovPass1))

	// Compute per-track mdat starting offsets.
	mdatBase := ftypSize + moovSize + 8 // 8 = mdat box header

	trackBases := make([]int64, len(m.tracks))
	acc := int64(0)

	for i, t := range m.tracks {
		trackBases[i] = mdatBase + acc

		for _, s := range t.samples {
			acc += int64(len(s.data))
		}
	}

	mdatPayloadSize := acc

	// Pass 2: build moov with correct stco values.
	moovFinal := m.buildMoov(trackBases)

	// Write ftyp.
	if _, err := m.w.Write(ftyp); err != nil {
		return err
	}

	// Write moov.
	if _, err := m.w.Write(moovFinal); err != nil {
		return err
	}

	// Write mdat header.
	var mdatHdr [8]byte
	binary.BigEndian.PutUint32(mdatHdr[0:4], uint32(8+mdatPayloadSize))
	copy(mdatHdr[4:8], "mdat")

	if _, err := m.w.Write(mdatHdr[:]); err != nil {
		return err
	}

	// Write mdat payload track by track.
	for _, t := range m.tracks {
		for _, s := range t.samples {
			if _, err := m.w.Write(s.data); err != nil {
				return err
			}
		}
	}

	return nil
}

// ── moov building ─────────────────────────────────────────────────────────────

// buildMoov builds the complete moov box. trackBases[i] is the absolute file
// offset of the first sample for track i in the mdat payload. If trackBases is
// nil, all stco values are written as 0 (used for size measurement).
func (m *Muxer) buildMoov(trackBases []int64) []byte {
	var p bytes.Buffer

	p.Write(buildMvhd(m.movieDuration()))

	for i, t := range m.tracks {
		base := int64(0)
		if trackBases != nil {
			base = trackBases[i]
		}

		p.Write(m.buildTrak(t, base))
	}

	// Non-fragmented MP4 has no mvex box.

	return makeBox("moov", p.Bytes())
}

// movieDuration returns the movie duration in movie timescale units (ms).
func (m *Muxer) movieDuration() uint32 {
	maxMs := uint32(0)

	for _, t := range m.tracks {
		ticks := uint64(0)

		for _, s := range t.samples {
			ticks += uint64(s.duration)
		}

		ms := uint32(ticks * 1000 / uint64(t.timescale))

		if ms > maxMs {
			maxMs = ms
		}
	}

	return maxMs
}

// buildTrak builds one trak box for the given track.
// base is the absolute file offset of the track's first sample in the mdat
// payload (0 during the size-measurement pass).
func (m *Muxer) buildTrak(t *mp4Track, base int64) []byte {
	var p bytes.Buffer
	p.Write(buildTkhd(t))
	p.Write(buildMdia(t, base))

	return makeBox("trak", p.Bytes())
}

func buildTkhd(t *mp4Track) []byte {
	// Track duration in movie timescale (ms).
	ticks := uint64(0)
	for _, s := range t.samples {
		ticks += uint64(s.duration)
	}

	durMs := uint32(ticks * 1000 / uint64(t.timescale))

	var p bytes.Buffer
	writeUint32(&p, 0)     // creation_time
	writeUint32(&p, 0)     // modification_time
	writeUint32(&p, t.id)  // track_ID
	writeUint32(&p, 0)     // reserved
	writeUint32(&p, durMs) // duration (movie timescale = 1000)
	writeZeros(&p, 8)      // reserved
	writeUint16(&p, 0)     // layer
	writeUint16(&p, 0)     // alternate_group

	if t.isVideo {
		writeUint16(&p, 0)
	} else {
		writeUint16(&p, 0x0100) // volume = 1.0 for audio
	}

	writeUint16(&p, 0) // reserved
	// Unity matrix
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x40000000)
	// Width and height (16.16 fixed point)
	if v, ok := t.codec.(av.VideoCodecData); ok {
		writeUint32(&p, uint32(v.Width())<<16)
		writeUint32(&p, uint32(v.Height())<<16)
	} else {
		writeUint32(&p, 0)
		writeUint32(&p, 0)
	}

	// flags: 0x3 = track_enabled | track_in_movie
	return makeFullBox("tkhd", 0, 3, p.Bytes())
}

func buildMdia(t *mp4Track, base int64) []byte {
	var p bytes.Buffer
	p.Write(buildMdhd(t))
	p.Write(buildHdlr(t))
	p.Write(buildMinf(t, base))

	return makeBox("mdia", p.Bytes())
}

func buildMdhd(t *mp4Track) []byte {
	ticks := uint64(0)
	for _, s := range t.samples {
		ticks += uint64(s.duration)
	}

	var p bytes.Buffer
	writeUint32(&p, 0)           // creation_time
	writeUint32(&p, 0)           // modification_time
	writeUint32(&p, t.timescale) // timescale
	writeUint32(&p, uint32(ticks))
	writeUint16(&p, 0x55c4) // language: 'und'
	writeUint16(&p, 0)      // pre_defined

	return makeFullBox("mdhd", 0, 0, p.Bytes())
}

func buildHdlr(t *mp4Track) []byte {
	var p bytes.Buffer
	writeUint32(&p, 0) // pre_defined

	if t.isVideo {
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

func buildMinf(t *mp4Track, base int64) []byte {
	var p bytes.Buffer

	if t.isVideo {
		p.Write(buildVmhd())
	} else {
		p.Write(buildSmhd())
	}

	p.Write(buildDinf())
	p.Write(buildStbl(t, base))

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
	var urlp bytes.Buffer

	dref := makeFullBox("url ", 0, 1, urlp.Bytes()) // flags=1: self-contained

	var drefp bytes.Buffer
	writeUint32(&drefp, 1) // entry_count
	drefp.Write(dref)

	return makeBox("dinf", makeFullBox("dref", 0, 0, drefp.Bytes()))
}

// ── stbl building ─────────────────────────────────────────────────────────────

func buildStbl(t *mp4Track, base int64) []byte {
	var p bytes.Buffer
	p.Write(buildStsd(t))
	p.Write(buildStts(t))

	if ctts := buildCtts(t); ctts != nil {
		p.Write(ctts)
	}

	if t.isVideo {
		p.Write(buildStss(t))
	}

	p.Write(buildStsc(t))
	p.Write(buildStsz(t))
	p.Write(buildStco(t, base))

	return makeBox("stbl", p.Bytes())
}

// buildStsd builds the sample description box.
func buildStsd(t *mp4Track) []byte {
	var entry []byte

	switch c := t.codec.(type) {
	case h264parser.CodecData:
		entry = buildAvc1(c)
	case h265parser.CodecData:
		entry = buildHev1(c)
	case aacparser.CodecData:
		entry = buildMp4a(c)
	}

	var p bytes.Buffer
	writeUint32(&p, 1) // entry_count
	p.Write(entry)

	return makeFullBox("stsd", 0, 0, p.Bytes())
}

// buildStts run-length encodes the per-sample durations.
// stts full-box: version+flags(4) + entry_count(4) + {count(4)+delta(4)}...
func buildStts(t *mp4Track) []byte {
	type entry struct{ count, delta uint32 }

	var entries []entry

	for _, s := range t.samples {
		if len(entries) > 0 && entries[len(entries)-1].delta == s.duration {
			entries[len(entries)-1].count++
		} else {
			entries = append(entries, entry{count: 1, delta: s.duration})
		}
	}

	var p bytes.Buffer
	writeUint32(&p, uint32(len(entries)))

	for _, e := range entries {
		writeUint32(&p, e.count)
		writeUint32(&p, e.delta)
	}

	return makeFullBox("stts", 0, 0, p.Bytes())
}

// buildCtts builds the composition time offset box. Returns nil if all CTS
// offsets are zero (no B-frames) — the box is optional in that case.
func buildCtts(t *mp4Track) []byte {
	anyNonZero := false

	for _, s := range t.samples {
		if s.ptsOff != 0 {
			anyNonZero = true

			break
		}
	}

	if !anyNonZero {
		return nil
	}

	type entry struct {
		count  uint32
		offset int32
	}

	var entries []entry

	for _, s := range t.samples {
		if len(entries) > 0 && entries[len(entries)-1].offset == s.ptsOff {
			entries[len(entries)-1].count++
		} else {
			entries = append(entries, entry{count: 1, offset: s.ptsOff})
		}
	}

	var p bytes.Buffer
	writeUint32(&p, uint32(len(entries)))

	for _, e := range entries {
		writeUint32(&p, e.count)
		writeInt32(&p, e.offset)
	}

	// version=1 for signed composition-time-offset support.
	return makeFullBox("ctts", 1, 0, p.Bytes())
}

// buildStss builds the sync-sample (keyframe) table for video tracks.
func buildStss(t *mp4Track) []byte {
	var indices []uint32

	for i, s := range t.samples {
		if s.isKey {
			indices = append(indices, uint32(i+1))
		}
	}

	var p bytes.Buffer
	writeUint32(&p, uint32(len(indices)))

	for _, idx := range indices {
		writeUint32(&p, idx)
	}

	return makeFullBox("stss", 0, 0, p.Bytes())
}

// buildStsc builds the sample-to-chunk box using one sample per chunk (the
// simplest possible layout): a single entry (first_chunk=1, spc=1, desc=1).
func buildStsc(t *mp4Track) []byte {
	var p bytes.Buffer

	if len(t.samples) > 0 {
		writeUint32(&p, 1) // entry_count
		writeUint32(&p, 1) // first_chunk
		writeUint32(&p, 1) // samples_per_chunk
		writeUint32(&p, 1) // sample_description_index
	} else {
		writeUint32(&p, 0) // entry_count
	}

	return makeFullBox("stsc", 0, 0, p.Bytes())
}

// buildStsz builds the sample size table.
func buildStsz(t *mp4Track) []byte {
	var p bytes.Buffer
	writeUint32(&p, 0) // constant_size = 0 (variable)
	writeUint32(&p, uint32(len(t.samples)))

	for _, s := range t.samples {
		writeUint32(&p, uint32(len(s.data)))
	}

	return makeFullBox("stsz", 0, 0, p.Bytes())
}

// buildStco builds the chunk-offset table. Since one sample per chunk, there
// is one stco entry per sample. base is the absolute file offset of the first
// sample in the mdat payload; subsequent entries are base + cumulative sizes.
func buildStco(t *mp4Track, base int64) []byte {
	var p bytes.Buffer
	writeUint32(&p, uint32(len(t.samples)))

	offset := base

	for _, s := range t.samples {
		writeUint32(&p, uint32(offset))
		offset += int64(len(s.data))
	}

	return makeFullBox("stco", 0, 0, p.Bytes())
}

// ── Sample entry builders (stsd entries) ──────────────────────────────────────

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

	p.Write(makeBox("avcC", c.AVCDecoderConfRecordBytes()))

	return makeBox("avc1", p.Bytes())
}

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

	p.Write(makeBox("hvcC", c.HEVCDecoderConfigurationRecordBytes()))

	return makeBox("hev1", p.Bytes())
}

func buildMp4a(c aacparser.CodecData) []byte {
	var p bytes.Buffer
	writeZeros(&p, 6)  // reserved
	writeUint16(&p, 1) // data_reference_index
	// AudioSampleEntry
	writeUint32(&p, 0) // reserved
	writeUint32(&p, 0) // reserved
	writeUint16(&p, uint16(c.ChannelLayout().Count()))
	writeUint16(&p, 16) // samplesize
	writeUint16(&p, 0)  // pre_defined
	writeUint16(&p, 0)  // reserved
	writeUint32(&p, uint32(c.SampleRate())<<16)

	p.Write(buildEsds(c))

	return makeBox("mp4a", p.Bytes())
}

// buildEsds writes an MPEG-4 Elementary Stream Descriptor box.
func buildEsds(c aacparser.CodecData) []byte {
	asc := c.MPEG4AudioConfigBytes()

	dsi := buildDescriptor(0x05, asc)

	var dcBody bytes.Buffer
	dcBody.WriteByte(0x40)  // objectTypeIndication = MPEG-4 Audio
	dcBody.WriteByte(0x15)  // streamType=audio (0x05<<2 | 0x01)
	writeUint24(&dcBody, 0) // bufferSizeDB
	writeUint32(&dcBody, 0) // maxBitrate
	writeUint32(&dcBody, 0) // avgBitrate
	dcBody.Write(dsi)
	dc := buildDescriptor(0x04, dcBody.Bytes())

	sl := buildDescriptor(0x06, []byte{0x02}) // SLConfigDescriptor predefined=2

	var esBody bytes.Buffer
	writeUint16(&esBody, 1) // ES_ID
	esBody.WriteByte(0x00)  // flags
	esBody.Write(dc)
	esBody.Write(sl)
	esDesc := buildDescriptor(0x03, esBody.Bytes())

	var p bytes.Buffer
	writeUint32(&p, 0) // version + flags
	p.Write(esDesc)

	return makeBox("esds", p.Bytes())
}

// buildDescriptor encodes an MPEG-4 expandable-class descriptor.
func buildDescriptor(tag byte, payload []byte) []byte {
	n := len(payload)

	var b bytes.Buffer

	b.WriteByte(tag)

	for n > 0x7f {
		b.WriteByte(byte(n>>21) | 0x80)
		n &= 0x1fffff
	}

	b.WriteByte(byte(n))
	b.Write(payload)

	return b.Bytes()
}

// ── mvhd ──────────────────────────────────────────────────────────────────────

func buildMvhd(durationMs uint32) []byte {
	var p bytes.Buffer
	writeUint32(&p, 0)          // creation_time
	writeUint32(&p, 0)          // modification_time
	writeUint32(&p, 1000)       // timescale (ms)
	writeUint32(&p, durationMs) // duration
	writeUint32(&p, 0x00010000) // rate = 1.0
	writeUint16(&p, 0x0100)     // volume = 1.0
	writeZeros(&p, 10)          // reserved
	// Unity matrix
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x00010000)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0)
	writeUint32(&p, 0x40000000)
	writeZeros(&p, 24)          // pre_defined
	writeUint32(&p, 0xFFFFFFFF) // next_track_ID = 0xFFFFFFFF (unassigned)

	return makeFullBox("mvhd", 0, 0, p.Bytes())
}

// ── ftyp ──────────────────────────────────────────────────────────────────────

func buildFtyp(tracks []*mp4Track) []byte {
	codecBrand := "avc1" // default for H.264
	for _, t := range tracks {
		if _, ok := t.codec.(h265parser.CodecData); ok {
			codecBrand = "hev1"
			break
		}
	}

	var p bytes.Buffer
	p.WriteString("isom")       // major_brand
	writeUint32(&p, 0x00000200) // minor_version
	p.WriteString("isom")       // compatible_brands
	p.WriteString("iso2")
	p.WriteString(codecBrand)
	p.WriteString("mp41")

	return makeBox("ftyp", p.Bytes())
}

// ── Box building primitives ───────────────────────────────────────────────────

func makeBox(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b[0:], size)
	copy(b[4:8], typ)
	copy(b[8:], payload)

	return b
}

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

func writeZeros(b *bytes.Buffer, n int) {
	b.Write(make([]byte, n))
}

// ── Codec helpers ─────────────────────────────────────────────────────────────

// timescaleForCodec returns the track timescale for the given codec.
func timescaleForCodec(codec av.CodecData) (uint32, bool) {
	switch c := codec.(type) {
	case h264parser.CodecData:
		return c.TimeScale(), true
	case h265parser.CodecData:
		return c.TimeScale(), true
	case aacparser.CodecData:
		return uint32(c.SampleRate()), true
	}

	return 0, false
}

// durationToTicks converts a time.Duration to timescale ticks.
func durationToTicks(d time.Duration, timescale uint32) int64 {
	if timescale == 0 || d == 0 {
		return 0
	}

	return int64(d) * int64(timescale) / int64(time.Second)
}
