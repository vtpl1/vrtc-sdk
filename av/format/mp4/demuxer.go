// Package mp4 implements a regular (non-fragmented) ISO Base Media File Format
// (ISO 14496-12) DemuxCloser and MuxCloser.
// Supported tracks: H.264 (avc1/avc3), H.265 (hev1/hvc1), AAC (mp4a).
package mp4

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
)

// Sentinel errors returned by the demuxer.
var (
	// ErrNoMoovBox is returned when the file contains no moov box.
	ErrNoMoovBox = errors.New("mp4: no moov box found")
	// ErrMalformed is returned when a box is structurally invalid or truncated.
	ErrMalformed = errors.New("mp4: malformed box")
	// ErrUnsupportedCodec is returned for unrecognised sample entry types.
	ErrUnsupportedCodec = errors.New("mp4: unsupported codec")
)

// ── sampleEntry ───────────────────────────────────────────────────────────────

// sampleEntry describes one decodable media sample within the file.
type sampleEntry struct {
	streamIdx uint16
	codecType av.CodecType
	offset    int64
	size      uint32
	dts       time.Duration
	ptsOffset time.Duration
	duration  time.Duration
	isKey     bool
}

// ── Demuxer ───────────────────────────────────────────────────────────────────

// Demuxer reads a non-fragmented ISO MP4 file and implements av.DemuxCloser.
// Create with NewDemuxer or Open; call GetCodecs exactly once, then loop on
// ReadPacket until io.EOF.
type Demuxer struct {
	rs      io.ReadSeeker
	rc      io.Closer
	streams []av.Stream
	samples []sampleEntry // sorted by DTS across all tracks
	pos     int
}

// NewDemuxer creates a Demuxer that reads from r.
// r must implement io.ReadSeeker; if it also implements io.Closer, Close
// delegates to it.
func NewDemuxer(r io.ReadSeeker) *Demuxer {
	d := &Demuxer{rs: r}
	if rc, ok := r.(io.Closer); ok {
		d.rc = rc
	}

	return d
}

// Open opens the named MP4 file and returns a ready Demuxer.
func Open(path string) (*Demuxer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	return &Demuxer{rs: f, rc: f}, nil
}

// GetCodecs parses the moov box and returns the initial stream list.
// Call exactly once before ReadPacket.
func (d *Demuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	if _, err := d.rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	moovPayload, err := findTopLevelBox(d.rs, "moov")
	if err != nil {
		return nil, err
	}

	streams, samples, err := parseMoovPayload(moovPayload)
	if err != nil {
		return nil, err
	}

	d.streams = streams
	d.samples = samples

	return streams, nil
}

// ReadPacket returns the next av.Packet in DTS order. Returns io.EOF when all
// samples have been read.
func (d *Demuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	if ctx.Err() != nil {
		return av.Packet{}, ctx.Err()
	}

	if d.pos >= len(d.samples) {
		return av.Packet{}, io.EOF
	}

	s := d.samples[d.pos]
	d.pos++

	if _, err := d.rs.Seek(s.offset, io.SeekStart); err != nil {
		return av.Packet{}, err
	}

	data := make([]byte, s.size)
	if _, err := io.ReadFull(d.rs, data); err != nil {
		return av.Packet{}, err
	}

	return av.Packet{
		Idx:       s.streamIdx,
		KeyFrame:  s.isKey,
		DTS:       s.dts,
		PTSOffset: s.ptsOffset,
		Duration:  s.duration,
		CodecType: s.codecType,
		Data:      data,
	}, nil
}

// Close releases the underlying resource.
func (d *Demuxer) Close() error {
	if d.rc != nil {
		return d.rc.Close()
	}

	return nil
}

// ── File-level box scanning ───────────────────────────────────────────────────

// findTopLevelBox scans rs (from its current position) for the first top-level
// box with the given 4-character type, skipping all others. Returns the payload
// (bytes after the 8- or 16-byte header).
func findTopLevelBox(rs io.ReadSeeker, typ string) ([]byte, error) {
	for {
		startPos, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}

		var hdr [8]byte
		if _, err := io.ReadFull(rs, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, ErrNoMoovBox
			}

			return nil, err
		}

		boxSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
		boxTyp := string(hdr[4:8])
		headerSize := int64(8)

		switch boxSize {
		case 1:
			// Extended 64-bit size — next 8 bytes hold the full box size.
			var ext [8]byte
			if _, err := io.ReadFull(rs, ext[:]); err != nil {
				return nil, err
			}

			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			headerSize = 16

		case 0:
			// Box extends to EOF.
			end, err := rs.Seek(0, io.SeekEnd)
			if err != nil {
				return nil, err
			}

			boxSize = end - startPos

			if _, err := rs.Seek(startPos+headerSize, io.SeekStart); err != nil {
				return nil, err
			}
		}

		payloadSize := boxSize - headerSize
		if payloadSize < 0 {
			return nil, fmt.Errorf("%w: negative payload for box %q", ErrMalformed, boxTyp)
		}

		if boxTyp == typ {
			payload := make([]byte, payloadSize)
			if _, err := io.ReadFull(rs, payload); err != nil {
				return nil, err
			}

			return payload, nil
		}

		// Skip this box.
		if _, err := rs.Seek(startPos+boxSize, io.SeekStart); err != nil {
			return nil, ErrNoMoovBox
		}
	}
}

// ── In-memory box helpers ─────────────────────────────────────────────────────

// findBox searches data for the first child box with the given type and returns
// its payload (bytes after the 8-byte header). Returns false if not found.
func findBox(data []byte, typ string) ([]byte, bool) {
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

// findBoxes returns the payloads of all child boxes with the given type.
func findBoxes(data []byte, typ string) [][]byte {
	var result [][]byte

	for len(data) >= 8 {
		size := binary.BigEndian.Uint32(data[0:4])
		if size < 8 || int(size) > len(data) {
			break
		}

		if string(data[4:8]) == typ {
			result = append(result, data[8:size])
		}

		data = data[size:]
	}

	return result
}

// ── Moov parsing ──────────────────────────────────────────────────────────────

// parseMoovPayload parses the moov payload and builds the stream list and
// merged sample table sorted by DTS.
func parseMoovPayload(moovPayload []byte) ([]av.Stream, []sampleEntry, error) {
	trakBoxes := findBoxes(moovPayload, "trak")

	var streams []av.Stream

	var allSamples []sampleEntry

	for _, trakPayload := range trakBoxes {
		streamIdx := uint16(len(streams))

		stream, samples, err := parseTrak(trakPayload, streamIdx)
		if err != nil {
			continue // skip unsupported or malformed tracks
		}

		streams = append(streams, stream)
		allSamples = append(allSamples, samples...)
	}

	if len(streams) == 0 {
		return nil, nil, ErrNoMoovBox
	}

	sort.SliceStable(allSamples, func(i, j int) bool {
		return allSamples[i].dts < allSamples[j].dts
	})

	return streams, allSamples, nil
}

func parseTrak(trakPayload []byte, streamIdx uint16) (av.Stream, []sampleEntry, error) {
	mdiaPayload, ok := findBox(trakPayload, "mdia")
	if !ok {
		return av.Stream{}, nil, ErrMalformed
	}

	mdhdPayload, ok := findBox(mdiaPayload, "mdhd")
	if !ok {
		return av.Stream{}, nil, ErrMalformed
	}

	timescale, err := parseMdhdTimescale(mdhdPayload)
	if err != nil {
		return av.Stream{}, nil, err
	}

	minfPayload, ok := findBox(mdiaPayload, "minf")
	if !ok {
		return av.Stream{}, nil, ErrMalformed
	}

	stblPayload, ok := findBox(minfPayload, "stbl")
	if !ok {
		return av.Stream{}, nil, ErrMalformed
	}

	stsdPayload, ok := findBox(stblPayload, "stsd")
	if !ok {
		return av.Stream{}, nil, ErrMalformed
	}

	// stsd full-box: version+flags(4) + entry_count(4) + entries.
	if len(stsdPayload) < 8 {
		return av.Stream{}, nil, ErrMalformed
	}

	codec, isVideo, err := parseStsdEntry(stsdPayload[8:])
	if err != nil {
		return av.Stream{}, nil, err
	}

	samples, err := parseSampleTable(stblPayload, timescale, streamIdx, codec.Type(), isVideo)
	if err != nil {
		return av.Stream{}, nil, err
	}

	return av.Stream{Idx: streamIdx, Codec: codec}, samples, nil
}

// parseMdhdTimescale extracts the timescale from an mdhd full-box payload.
func parseMdhdTimescale(payload []byte) (uint32, error) {
	if len(payload) < 4 {
		return 0, ErrMalformed
	}

	if payload[0] == 0 { // version 0: times are uint32
		if len(payload) < 16 {
			return 0, ErrMalformed
		}

		return binary.BigEndian.Uint32(payload[12:16]), nil
	}

	// version 1: times are uint64
	if len(payload) < 24 {
		return 0, ErrMalformed
	}

	return binary.BigEndian.Uint32(payload[20:24]), nil
}

// ── Sample table parsing ──────────────────────────────────────────────────────

// parseSampleTable builds the per-sample entry list from the stbl box payload.
func parseSampleTable(
	stblPayload []byte,
	timescale uint32,
	streamIdx uint16,
	ct av.CodecType,
	isVideo bool,
) ([]sampleEntry, error) {
	// ── stsz: sample sizes ──────────────────────────────────────────────────
	sizes, err := parseStsz(stblPayload)
	if err != nil {
		return nil, err
	}

	n := len(sizes)
	if n == 0 {
		return nil, nil
	}

	// ── stco / co64: chunk file offsets ─────────────────────────────────────
	chunkOffsets, err := parseChunkOffsets(stblPayload)
	if err != nil {
		return nil, err
	}

	// ── stsc: sample-to-chunk mapping ────────────────────────────────────────
	chunkSamples, err := buildChunkSamplesMap(stblPayload, len(chunkOffsets))
	if err != nil {
		return nil, err
	}

	// ── stts: per-sample DTS and duration ────────────────────────────────────
	dtss, durations, err := parseStts(stblPayload, n, timescale)
	if err != nil {
		return nil, err
	}

	// ── ctts: per-sample PTS offset (optional) ───────────────────────────────
	ptsOffsets, _ := parseCtts(stblPayload, n, timescale)

	// ── stss: keyframe flags (optional, video only) ──────────────────────────
	keyframes := parseStss(stblPayload, n, isVideo)

	// ── Compute per-sample absolute file offsets ─────────────────────────────
	sampleOffsets := make([]int64, n)
	si := 0

	for ci, spc := range chunkSamples {
		chunkBase := chunkOffsets[ci]
		acc := int64(0)

		for k := 0; k < spc && si < n; k++ {
			sampleOffsets[si] = chunkBase + acc
			acc += int64(sizes[si])
			si++
		}
	}

	// ── Assemble sampleEntry slice ───────────────────────────────────────────
	entries := make([]sampleEntry, n)

	for i := range entries {
		entries[i] = sampleEntry{
			streamIdx: streamIdx,
			codecType: ct,
			offset:    sampleOffsets[i],
			size:      sizes[i],
			dts:       dtss[i],
			duration:  durations[i],
			isKey:     keyframes[i],
		}

		if ptsOffsets != nil {
			entries[i].ptsOffset = ptsOffsets[i]
		}
	}

	return entries, nil
}

// parseStsz returns the per-sample size array from the stsz box.
// The stsz full-box layout: version+flags(4) + constant_size(4) + sample_count(4) + [entry_size(4)...].
func parseStsz(stblPayload []byte) ([]uint32, error) {
	stszPayload, ok := findBox(stblPayload, "stsz")
	if !ok {
		return nil, fmt.Errorf("%w: missing stsz box", ErrMalformed)
	}

	if len(stszPayload) < 12 {
		return nil, ErrMalformed
	}

	constantSize := binary.BigEndian.Uint32(stszPayload[4:8])
	count := int(binary.BigEndian.Uint32(stszPayload[8:12]))

	if count == 0 {
		return nil, nil
	}

	sizes := make([]uint32, count)

	if constantSize != 0 {
		for i := range sizes {
			sizes[i] = constantSize
		}
	} else {
		if len(stszPayload) < 12+count*4 {
			return nil, ErrMalformed
		}

		for i := range sizes {
			sizes[i] = binary.BigEndian.Uint32(stszPayload[12+i*4:])
		}
	}

	return sizes, nil
}

// parseChunkOffsets returns the absolute file offset for each chunk.
// Supports both stco (32-bit) and co64 (64-bit).
func parseChunkOffsets(stblPayload []byte) ([]int64, error) {
	if payload, ok := findBox(stblPayload, "co64"); ok {
		if len(payload) < 8 {
			return nil, ErrMalformed
		}

		count := int(binary.BigEndian.Uint32(payload[4:8]))

		if len(payload) < 8+count*8 {
			return nil, ErrMalformed
		}

		offsets := make([]int64, count)

		for i := range offsets {
			offsets[i] = int64(binary.BigEndian.Uint64(payload[8+i*8:]))
		}

		return offsets, nil
	}

	if payload, ok := findBox(stblPayload, "stco"); ok {
		if len(payload) < 8 {
			return nil, ErrMalformed
		}

		count := int(binary.BigEndian.Uint32(payload[4:8]))

		if len(payload) < 8+count*4 {
			return nil, ErrMalformed
		}

		offsets := make([]int64, count)

		for i := range offsets {
			offsets[i] = int64(binary.BigEndian.Uint32(payload[8+i*4:]))
		}

		return offsets, nil
	}

	return nil, fmt.Errorf("%w: missing stco/co64 box", ErrMalformed)
}

// buildChunkSamplesMap returns a slice where [i] is the number of samples in
// chunk i (0-based) according to the stsc (sample-to-chunk) box.
func buildChunkSamplesMap(stblPayload []byte, numChunks int) ([]int, error) {
	stscPayload, ok := findBox(stblPayload, "stsc")
	if !ok {
		return nil, fmt.Errorf("%w: missing stsc box", ErrMalformed)
	}

	// Full-box: version+flags(4) + entry_count(4) + entries(12*n)
	if len(stscPayload) < 8 {
		return nil, ErrMalformed
	}

	entryCount := int(binary.BigEndian.Uint32(stscPayload[4:8]))

	if len(stscPayload) < 8+entryCount*12 {
		return nil, ErrMalformed
	}

	type stscEntry struct {
		firstChunk, samplesPerChunk int
	}

	entries := make([]stscEntry, entryCount)

	for i := range entries {
		off := 8 + i*12
		entries[i].firstChunk = int(binary.BigEndian.Uint32(stscPayload[off:]))
		entries[i].samplesPerChunk = int(binary.BigEndian.Uint32(stscPayload[off+4:]))
	}

	result := make([]int, numChunks)

	for ei, e := range entries {
		lastChunk := numChunks
		if ei+1 < len(entries) {
			lastChunk = entries[ei+1].firstChunk - 1
		}

		for c := e.firstChunk; c <= lastChunk && c <= numChunks; c++ {
			result[c-1] = e.samplesPerChunk
		}
	}

	return result, nil
}

// parseStts returns per-sample DTS and duration (as time.Duration) from stts.
// stts full-box: version+flags(4) + entry_count(4) + entries{count(4)+delta(4)}...
func parseStts(
	stblPayload []byte,
	n int,
	timescale uint32,
) ([]time.Duration, []time.Duration, error) {
	sttsPayload, ok := findBox(stblPayload, "stts")
	if !ok {
		return nil, nil, fmt.Errorf("%w: missing stts box", ErrMalformed)
	}

	if len(sttsPayload) < 8 {
		return nil, nil, ErrMalformed
	}

	entryCount := int(binary.BigEndian.Uint32(sttsPayload[4:8]))

	if len(sttsPayload) < 8+entryCount*8 {
		return nil, nil, ErrMalformed
	}

	dtss := make([]time.Duration, n)
	durs := make([]time.Duration, n)

	si := 0
	dts := int64(0)

	for i := 0; i < entryCount && si < n; i++ {
		off := 8 + i*8
		count := int(binary.BigEndian.Uint32(sttsPayload[off:]))
		delta := int64(binary.BigEndian.Uint32(sttsPayload[off+4:]))

		for j := 0; j < count && si < n; j++ {
			dtss[si] = ticksToDuration(dts, timescale)
			durs[si] = ticksToDuration(delta, timescale)
			dts += delta
			si++
		}
	}

	return dtss, durs, nil
}

// parseCtts returns per-sample PTS offsets from the ctts box, or nil if absent.
// ctts full-box: version+flags(4) + entry_count(4) + entries{count(4)+offset(4)}...
func parseCtts(stblPayload []byte, n int, timescale uint32) ([]time.Duration, error) {
	cttsPayload, ok := findBox(stblPayload, "ctts")
	if !ok {
		return nil, nil
	}

	if len(cttsPayload) < 8 {
		return nil, ErrMalformed
	}

	entryCount := int(binary.BigEndian.Uint32(cttsPayload[4:8]))

	if len(cttsPayload) < 8+entryCount*8 {
		return nil, ErrMalformed
	}

	ctss := make([]time.Duration, n)
	si := 0

	for i := 0; i < entryCount && si < n; i++ {
		off := 8 + i*8
		count := int(binary.BigEndian.Uint32(cttsPayload[off:]))
		rawOffset := binary.BigEndian.Uint32(cttsPayload[off+4:])

		// version=1: signed; version=0: treat as signed per ISO 14496-12:2012.
		offset := int32(rawOffset)

		d := ticksToDuration(int64(offset), timescale)

		for j := 0; j < count && si < n; j++ {
			ctss[si] = d
			si++
		}
	}

	return ctss, nil
}

// parseStss returns per-sample keyframe flags from the stss box.
// For audio tracks (isVideo=false) all entries are false; audio packets are
// never "key frames" in the av.Packet sense.
// For video tracks with no stss box, all samples are key frames per ISO spec.
func parseStss(stblPayload []byte, n int, isVideo bool) []bool {
	keyframes := make([]bool, n) // default: false

	if !isVideo {
		return keyframes // audio: never set KeyFrame
	}

	stssPayload, ok := findBox(stblPayload, "stss")
	if !ok {
		// No stss: all video samples are sync samples (keyframes) per ISO spec.
		for i := range keyframes {
			keyframes[i] = true
		}

		return keyframes
	}

	if len(stssPayload) < 8 {
		for i := range keyframes {
			keyframes[i] = true
		}

		return keyframes
	}

	entryCount := int(binary.BigEndian.Uint32(stssPayload[4:8]))

	if len(stssPayload) < 8+entryCount*4 {
		for i := range keyframes {
			keyframes[i] = true
		}

		return keyframes
	}

	for i := range entryCount {
		idx := int(binary.BigEndian.Uint32(stssPayload[8+i*4:])) - 1 // convert to 0-based

		if idx >= 0 && idx < n {
			keyframes[idx] = true
		}
	}

	return keyframes
}

// ── Codec parsing (stsd sample entries) ──────────────────────────────────────

// Visual sample entry header size (ISO 14496-12 §12.1.3):
// reserved(6)+data_ref_idx(2)+pre_defined(2)+reserved(2)+pre_defined[3](12)+
// width(2)+height(2)+horiz_res(4)+vert_res(4)+reserved(4)+frame_count(2)+
// compressorname(32)+depth(2)+pre_defined(2) = 78 bytes.
const visualSampleEntryHdrSize = 78

// Audio sample entry header size (ISO 14496-12 §12.2.3):
// reserved(6)+data_ref_idx(2)+reserved(8)+channelcount(2)+samplesize(2)+
// pre_defined(2)+reserved(2)+samplerate(4) = 28 bytes.
const audioSampleEntryHdrSize = 28

// parseStsdEntry parses the first sample description entry from the payload
// immediately following the stsd full-box header (version+flags + entry_count).
func parseStsdEntry(data []byte) (av.CodecData, bool, error) {
	if len(data) < 8 {
		return nil, false, ErrMalformed
	}

	size := binary.BigEndian.Uint32(data[0:4])
	if size < 8 || int(size) > len(data) {
		return nil, false, ErrMalformed
	}

	typ := string(data[4:8])
	entryPayload := data[8:size]

	switch typ {
	case "avc1", "avc3":
		if len(entryPayload) < visualSampleEntryHdrSize {
			return nil, false, ErrMalformed
		}

		avccPayload, ok := findBox(entryPayload[visualSampleEntryHdrSize:], "avcC")
		if !ok {
			return nil, false, ErrMalformed
		}

		codec, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(avccPayload)

		return codec, true, err

	case "hev1", "hvc1":
		if len(entryPayload) < visualSampleEntryHdrSize {
			return nil, false, ErrMalformed
		}

		hvccPayload, ok := findBox(entryPayload[visualSampleEntryHdrSize:], "hvcC")
		if !ok {
			return nil, false, ErrMalformed
		}

		codec, err := h265parser.NewCodecDataFromAVCDecoderConfRecord(hvccPayload)

		return codec, true, err

	case "mp4a":
		if len(entryPayload) < audioSampleEntryHdrSize {
			return nil, false, ErrMalformed
		}

		esdsPayload, ok := findBox(entryPayload[audioSampleEntryHdrSize:], "esds")
		if !ok {
			return nil, false, ErrMalformed
		}

		asc, err := parseEsds(esdsPayload)
		if err != nil {
			return nil, false, err
		}

		codec, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(asc)

		return codec, false, err

	default:
		return nil, false, fmt.Errorf("%w: sample entry type %q", ErrUnsupportedCodec, typ)
	}
}

// parseEsds extracts the AudioSpecificConfig bytes from an esds full-box payload.
func parseEsds(payload []byte) ([]byte, error) {
	if len(payload) < 4 {
		return nil, ErrMalformed
	}

	data := payload[4:] // skip version + flags

	esBody, err := readDescriptorBody(data, 0x03)
	if err != nil {
		return nil, fmt.Errorf("mp4: esds ES_Descriptor: %w", err)
	}

	if len(esBody) < 3 {
		return nil, ErrMalformed
	}

	flags := esBody[2]
	off := 3

	if flags&0x80 != 0 { // streamDependenceFlag
		off += 2
	}

	if flags&0x40 != 0 { // URL_flag
		if off >= len(esBody) {
			return nil, ErrMalformed
		}

		off += 1 + int(esBody[off])
	}

	if flags&0x20 != 0 { // OCRstreamFlag
		off += 2
	}

	if off >= len(esBody) {
		return nil, ErrMalformed
	}

	dcBody, err := readDescriptorBody(esBody[off:], 0x04)
	if err != nil {
		return nil, fmt.Errorf("mp4: esds DecoderConfigDescriptor: %w", err)
	}

	if len(dcBody) < 13 {
		return nil, ErrMalformed
	}

	asc, err := readDescriptorBody(dcBody[13:], 0x05)
	if err != nil {
		return nil, fmt.Errorf("mp4: esds DecoderSpecificInfo: %w", err)
	}

	return asc, nil
}

// readDescriptorBody reads an MPEG-4 expandable-class descriptor with the given
// tag and returns its body. The length uses a variable-length encoding (7 bits
// per byte, MSB=1 if more bytes follow).
func readDescriptorBody(data []byte, expectedTag byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, ErrMalformed
	}

	if data[0] != expectedTag {
		return nil, fmt.Errorf(
			"%w: expected tag 0x%02X, got 0x%02X",
			ErrMalformed,
			expectedTag,
			data[0],
		)
	}

	length := 0
	i := 1

	for ; i <= 4 && i < len(data); i++ {
		b := data[i]
		length = (length << 7) | int(b&0x7f)

		if b&0x80 == 0 {
			i++

			break
		}
	}

	if i+length > len(data) {
		return nil, ErrMalformed
	}

	return data[i : i+length], nil
}

// ticksToDuration converts timescale ticks to a time.Duration.
func ticksToDuration(ticks int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}

	return time.Duration(ticks) * time.Second / time.Duration(timescale)
}
