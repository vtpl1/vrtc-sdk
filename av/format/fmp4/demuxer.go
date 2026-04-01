package fmp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

var (
	// ErrNoMoovBox is returned by GetCodecs when the stream ends before a moov box is found.
	ErrNoMoovBox = errors.New("fmp4: no moov box found")
	// ErrMalformed is returned when a box is structurally invalid or truncated.
	ErrMalformed = errors.New("fmp4: malformed box")
)

// posReader wraps an io.Reader and tracks the cumulative byte offset consumed.
type posReader struct {
	r   io.Reader
	pos int64
}

func (p *posReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.pos += int64(n)

	return n, err
}

func (p *posReader) readFull(b []byte) error {
	_, err := io.ReadFull(p, b)

	return err
}

// trackDef holds per-track metadata extracted from the moov box.
type trackDef struct {
	id          uint32
	streamIdx   uint16
	timescale   uint32
	codec       av.CodecData
	hasVideo    bool
	defDuration uint32 // default_sample_duration from trex / tfhd
	defSize     uint32 // default_sample_size from trex / tfhd
	defFlags    uint32 // default_sample_flags from trex / tfhd
}

// rawSample carries the location and timing of one sample within a moof+mdat pair.
type rawSample struct {
	trackID  uint32
	dts      int64 // decode time in track timescale units (from tfdt + accumulated durations)
	cts      int32 // composition time offset in timescale units (PTS = DTS + CTS)
	duration uint32
	flags    uint32
	mdatOff  int // byte offset within the mdat payload
	size     int
}

// emsgEntry holds the parsed fields of one emsg (Event Message) box.
type emsgEntry struct {
	schemeIDURI   string
	presentTimeMS int64
	id            uint32
	payload       []byte
}

// Demuxer parses a fragmented MP4 byte stream and implements av.DemuxCloser.
// Create with NewDemuxer; call GetCodecs once, then loop on ReadPacket until io.EOF.
type Demuxer struct {
	pr                 posReader
	tracksByID         map[uint32]*trackDef
	streams            []av.Stream
	pending            []av.Packet
	pendingCodecChange []av.Stream // emitted on the first packet of the next fragment
	pendingEmsg        []emsgEntry // stashed emsg boxes awaiting the next moof+mdat pair
	moovRead           bool
}

// NewDemuxer returns a Demuxer that reads fMP4 data from r.
func NewDemuxer(r io.Reader) *Demuxer {
	return &Demuxer{
		pr:         posReader{r: r},
		tracksByID: make(map[uint32]*trackDef),
	}
}

// GetCodecs reads and parses the fMP4 init segment (ftyp + moov) and returns
// the initial stream list. Call exactly once before ReadPacket.
func (d *Demuxer) GetCodecs(_ context.Context) ([]av.Stream, error) {
	for {
		typ, payload, err := d.readBox()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrNoMoovBox
			}

			return nil, err
		}

		if typ == "moov" {
			d.parseMoov(payload)
			d.moovRead = true

			return d.streams, nil
		}
		// Skip ftyp and any other top-level box that precedes moov.
	}
}

// ReadPacket returns the next av.Packet from the stream. Returns io.EOF when
// the stream ends. Packet.NewCodecs is non-nil when a mid-stream codec change
// was signalled by a subsequent moov box in the stream; it carries the full
// replacement Stream list.
func (d *Demuxer) ReadPacket(ctx context.Context) (av.Packet, error) {
	for {
		if ctx.Err() != nil {
			return av.Packet{}, ctx.Err()
		}

		if len(d.pending) > 0 {
			pkt := d.pending[0]
			d.pending = d.pending[1:]

			return pkt, nil
		}

		typ, payload, err := d.readBox()
		if err != nil {
			return av.Packet{}, err
		}

		switch typ {
		case "moof":
			moofSize := int64(8 + len(payload))
			moofStart := d.pr.pos - moofSize

			pkts, err := d.parseMoofMdat(payload, moofStart, moofSize)
			if err != nil {
				return av.Packet{}, err
			}

			if len(d.pendingCodecChange) > 0 && len(pkts) > 0 {
				pkts[0].NewCodecs = d.pendingCodecChange
				d.pendingCodecChange = nil
			}

			// Attach analytics from any emsg boxes that preceded this fragment.
			if len(d.pendingEmsg) > 0 {
				for i := range pkts {
					ptMS := (pkts[i].DTS + pkts[i].PTSOffset).Milliseconds()
					for _, e := range d.pendingEmsg {
						if e.presentTimeMS == ptMS && e.schemeIDURI == emsgSchemeIDURI {
							var pvd av.FrameAnalytics
							if json.Unmarshal(e.payload, &pvd) == nil {
								pkts[i].Analytics = &pvd
							}

							break
						}
					}
				}

				d.pendingEmsg = d.pendingEmsg[:0]
			}

			d.pending = pkts

		case "moov":
			// Mid-stream codec change: re-parse moov, signal on next packet batch.
			d.parseMoov(payload)
			d.pendingCodecChange = d.streams

		case "emsg":
			// Stash the event message; it will be matched to a packet after the
			// following moof+mdat is parsed (emsg always precedes its moof box).
			if e, ok := parseEmsg(payload); ok {
				d.pendingEmsg = append(d.pendingEmsg, e)
			}

		default:
			// Skip ftyp, sidx, styp, and any other boxes between fragments.
		}
	}
}

// Close closes the underlying reader if it implements io.Closer.
func (d *Demuxer) Close() error {
	if c, ok := d.pr.r.(io.Closer); ok {
		return c.Close()
	}

	return nil
}

// ── emsg parsing ─────────────────────────────────────────────────────────────

// emsgSchemeIDURI is the scheme identifier written by buildEmsg in muxer.go.
const emsgSchemeIDURI = "urn:vtpl:analytics:1"

// parseEmsg parses an emsg (Event Message) version-1 full-box payload (the bytes
// after the 8-byte box header). Returns (entry, true) on success; (zero, false)
// if the box is malformed or not version 1.
func parseEmsg(payload []byte) (emsgEntry, bool) {
	// Full-box prefix: version(1) + flags(3) = 4 bytes. We only handle version 1
	// because version 0 uses a 32-bit presentation_time which is less precise.
	if len(payload) < 5 || payload[0] != 1 {
		return emsgEntry{}, false
	}

	pos := 4 // skip version + flags

	// null-terminated scheme_id_uri
	end := bytes.IndexByte(payload[pos:], 0)
	if end < 0 {
		return emsgEntry{}, false
	}

	scheme := string(payload[pos : pos+end])
	pos += end + 1

	// null-terminated value string (ignored)
	end = bytes.IndexByte(payload[pos:], 0)
	if end < 0 {
		return emsgEntry{}, false
	}

	pos += end + 1

	// timescale(4) + presentation_time(8) + event_duration(4) + id(4) = 20 bytes
	if len(payload) < pos+20 {
		return emsgEntry{}, false
	}

	timescale := binary.BigEndian.Uint32(payload[pos:])
	presentTime := binary.BigEndian.Uint64(payload[pos+4:])
	id := binary.BigEndian.Uint32(payload[pos+16:])
	pos += 20

	var ptMS int64
	if timescale > 0 {
		ptMS = int64(presentTime) * 1000 / int64(timescale)
	}

	return emsgEntry{
		schemeIDURI:   scheme,
		presentTimeMS: ptMS,
		id:            id,
		payload:       append([]byte(nil), payload[pos:]...),
	}, true
}

// ── Box reading ───────────────────────────────────────────────────────────────

// readBox reads the next ISO BMFF box from the stream. The returned payload
// contains everything after the 8-byte (size + type) box header.
func (d *Demuxer) readBox() (string, []byte, error) {
	var hdr [8]byte
	if err := d.pr.readFull(hdr[:]); err != nil {
		return "", nil, err
	}

	size := binary.BigEndian.Uint32(hdr[0:4])
	typ := string(hdr[4:8])

	if size < 8 {
		return "", nil, fmt.Errorf("%w: size %d < 8 for box %q", ErrMalformed, size, typ)
	}

	payload := make([]byte, int(size)-8)
	if err := d.pr.readFull(payload); err != nil {
		return "", nil, err
	}

	return typ, payload, nil
}

// findBox searches data for the first box with the given 4-character type and
// returns its payload (everything after the 8-byte header). Returns false if
// not found or data is truncated.
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

// findBoxes returns the payloads of all boxes with the given type in data.
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

// ── moov parsing ──────────────────────────────────────────────────────────────

func (d *Demuxer) parseMoov(payload []byte) {
	trakBoxes := findBoxes(payload, "trak")

	newTracks := make([]*trackDef, 0, len(trakBoxes))

	for _, trakPayload := range trakBoxes {
		track, err := parseTrak(trakPayload)
		if err != nil {
			continue // skip unsupported-codec or malformed tracks
		}

		newTracks = append(newTracks, track)
	}

	d.tracksByID = make(map[uint32]*trackDef, len(newTracks))
	d.streams = make([]av.Stream, 0, len(newTracks))

	for i, track := range newTracks {
		track.streamIdx = uint16(i)
		d.tracksByID[track.id] = track
		d.streams = append(d.streams, av.Stream{
			Idx:   uint16(i),
			Codec: track.codec,
		})
	}
}

func parseTrak(payload []byte) (*trackDef, error) {
	tkhdPayload, ok := findBox(payload, "tkhd")
	if !ok {
		return nil, ErrMalformed
	}

	trackID, err := parseTkhdTrackID(tkhdPayload)
	if err != nil {
		return nil, err
	}

	mdiaPayload, ok := findBox(payload, "mdia")
	if !ok {
		return nil, ErrMalformed
	}

	mdhdPayload, ok := findBox(mdiaPayload, "mdhd")
	if !ok {
		return nil, ErrMalformed
	}

	timescale, err := parseMdhdTimescale(mdhdPayload)
	if err != nil {
		return nil, err
	}

	hdlrPayload, ok := findBox(mdiaPayload, "hdlr")
	if !ok {
		return nil, ErrMalformed
	}

	handlerType, err := parseHdlrType(hdlrPayload)
	if err != nil {
		return nil, err
	}

	minfPayload, ok := findBox(mdiaPayload, "minf")
	if !ok {
		return nil, ErrMalformed
	}

	stblPayload, ok := findBox(minfPayload, "stbl")
	if !ok {
		return nil, ErrMalformed
	}

	stsdPayload, ok := findBox(stblPayload, "stsd")
	if !ok {
		return nil, ErrMalformed
	}

	// stsd is a full box: 4 bytes (version+flags) + 4 bytes (entry_count) + entries.
	if len(stsdPayload) < 8 {
		return nil, ErrMalformed
	}

	codec, hasVideo, err := parseStsdEntry(stsdPayload[8:], handlerType)
	if err != nil {
		return nil, err
	}

	return &trackDef{
		id:        trackID,
		timescale: timescale,
		codec:     codec,
		hasVideo:  hasVideo,
	}, nil
}

// parseTkhdTrackID extracts track_ID from a tkhd full-box payload.
// Layout after the 8-byte box header (payload starts here):
//
//	version(1) + flags(3) + creation_time + modification_time + track_ID(4)
//
// version=0: times are uint32 (4 bytes each); version=1: times are uint64 (8 bytes each).
func parseTkhdTrackID(payload []byte) (uint32, error) {
	if len(payload) < 4 {
		return 0, ErrMalformed
	}

	if payload[0] == 0 { // version 0
		if len(payload) < 16 {
			return 0, ErrMalformed
		}

		return binary.BigEndian.Uint32(payload[12:16]), nil
	}

	// version 1
	if len(payload) < 24 {
		return 0, ErrMalformed
	}

	return binary.BigEndian.Uint32(payload[20:24]), nil
}

// parseMdhdTimescale extracts the timescale from an mdhd full-box payload.
func parseMdhdTimescale(payload []byte) (uint32, error) {
	if len(payload) < 4 {
		return 0, ErrMalformed
	}

	if payload[0] == 0 { // version 0
		if len(payload) < 16 {
			return 0, ErrMalformed
		}

		return binary.BigEndian.Uint32(payload[12:16]), nil
	}

	// version 1
	if len(payload) < 24 {
		return 0, ErrMalformed
	}

	return binary.BigEndian.Uint32(payload[20:24]), nil
}

// parseHdlrType extracts the handler_type from an hdlr full-box payload.
// Layout: version+flags(4) + pre_defined(4) + handler_type(4) + ...
func parseHdlrType(payload []byte) (string, error) {
	if len(payload) < 12 {
		return "", ErrMalformed
	}

	return string(payload[8:12]), nil
}

// visualSampleEntryHeaderSize is the fixed-size header for a visual sample entry
// (ISO 14496-12 §12.1.3): reserved(6)+data_reference_index(2)+pre_defined(2)+
// reserved(2)+pre_defined[3](12)+width(2)+height(2)+horiz_resolution(4)+
// vert_resolution(4)+reserved(4)+frame_count(2)+compressorname(32)+depth(2)+pre_defined(2).
const visualSampleEntryHeaderSize = 78

// audioSampleEntryHeaderSize is the fixed-size header for an audio sample entry
// (ISO 14496-12 §12.2.3): reserved(6)+data_reference_index(2)+reserved(8)+
// channelcount(2)+samplesize(2)+pre_defined(2)+reserved(2)+samplerate(4).
const audioSampleEntryHeaderSize = 28

// parseStsdEntry parses the first sample entry from the stsd payload (after
// version+flags and entry_count have been skipped). Returns the codec, whether
// it is a video track, and any error.
func parseStsdEntry(data []byte, _ string) (av.CodecData, bool, error) {
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
		if len(entryPayload) < visualSampleEntryHeaderSize {
			return nil, false, ErrMalformed
		}

		avccPayload, ok := findBox(entryPayload[visualSampleEntryHeaderSize:], "avcC")
		if !ok {
			return nil, false, ErrMalformed
		}

		codec, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(avccPayload)

		return codec, true, err

	case "hev1", "hvc1":
		if len(entryPayload) < visualSampleEntryHeaderSize {
			return nil, false, ErrMalformed
		}

		hvccPayload, ok := findBox(entryPayload[visualSampleEntryHeaderSize:], "hvcC")
		if !ok {
			return nil, false, ErrMalformed
		}

		codec, err := h265parser.NewCodecDataFromAVCDecoderConfRecord(hvccPayload)

		return codec, true, err

	case "mp4a":
		if len(entryPayload) < audioSampleEntryHeaderSize {
			return nil, false, ErrMalformed
		}

		esdsPayload, ok := findBox(entryPayload[audioSampleEntryHeaderSize:], "esds")
		if !ok {
			return nil, false, ErrMalformed
		}

		asc, err := parseEsds(esdsPayload)
		if err != nil {
			return nil, false, err
		}

		codec, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(asc)

		return codec, false, err

	case "fLaC":
		if len(entryPayload) < audioSampleEntryHeaderSize {
			return nil, false, ErrMalformed
		}

		chCount := binary.BigEndian.Uint16(entryPayload[16:18])
		if chCount == 0 {
			return nil, false, ErrMalformed
		}

		sampleRateFP := binary.BigEndian.Uint32(entryPayload[24:28])

		sampleRate := sampleRateFP >> 16
		if sampleRate == 0 {
			return nil, false, ErrMalformed
		}

		_, ok := findBox(entryPayload[audioSampleEntryHeaderSize:], "dfLa")
		if !ok {
			return nil, false, ErrMalformed
		}

		return pcm.NewFLACCodecData(
			av.FLAC,
			sampleRate,
			channelLayoutFromCount(chCount),
		), false, nil

	default:
		return nil, false, fmt.Errorf(
			"%w: unsupported sample entry type %q",
			ErrUnsupportedCodec,
			typ,
		)
	}
}

func channelLayoutFromCount(chCount uint16) av.ChannelLayout {
	switch chCount {
	case 1:
		return av.ChMono
	case 2:
		return av.ChStereo
	}

	// Fall back to a low-bit mask with the requested channel count.
	if chCount >= 16 {
		return av.ChannelLayout(0xFFFF)
	}

	return av.ChannelLayout((1 << chCount) - 1)
}

// parseEsds extracts the AudioSpecificConfig bytes from an esds box payload.
// The esds payload written by our muxer is: version+flags(4) + ES_Descriptor.
func parseEsds(payload []byte) ([]byte, error) {
	if len(payload) < 4 {
		return nil, ErrMalformed
	}
	// Skip version + flags.
	data := payload[4:]

	// ES_Descriptor (tag 0x03).
	esBody, err := readDescriptorBody(data, 0x03)
	if err != nil {
		return nil, fmt.Errorf("fmp4: esds ES_Descriptor: %w", err)
	}

	// ES_Descriptor body: ES_ID(2) + flags(1) + optional extensions.
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

	// DecoderConfigDescriptor (tag 0x04).
	dcBody, err := readDescriptorBody(esBody[off:], 0x04)
	if err != nil {
		return nil, fmt.Errorf("fmp4: esds DecoderConfigDescriptor: %w", err)
	}

	// DecoderConfigDescriptor body: objectTypeIndication(1)+streamType(1)+
	// bufferSizeDB(3)+maxBitrate(4)+avgBitrate(4) = 13 bytes before DecoderSpecificInfo.
	if len(dcBody) < 13 {
		return nil, ErrMalformed
	}

	// DecoderSpecificInfo (tag 0x05) = AudioSpecificConfig.
	asc, err := readDescriptorBody(dcBody[13:], 0x05)
	if err != nil {
		return nil, fmt.Errorf("fmp4: esds DecoderSpecificInfo: %w", err)
	}

	return asc, nil
}

// readDescriptorBody reads an MPEG-4 expandable-class descriptor with the
// given tag byte and returns the descriptor body. The length field uses an
// expandable encoding where each byte contributes 7 bits; the MSB is 1 if
// more bytes follow.
func readDescriptorBody(data []byte, expectedTag byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, ErrMalformed
	}

	if data[0] != expectedTag {
		return nil, fmt.Errorf(
			"%w: expected descriptor tag 0x%02X, got 0x%02X",
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

// ── Fragment parsing ──────────────────────────────────────────────────────────

// parseMoofMdat parses a moof box and the immediately following mdat box,
// returning av.Packets sorted by DTS.
// moofStart is the absolute byte position of the moof box start in the stream.
// moofSize is the total moof box size including the 8-byte box header.
func (d *Demuxer) parseMoofMdat(
	moofPayload []byte,
	moofStart, moofSize int64,
) ([]av.Packet, error) {
	samples, err := d.parseMoof(moofPayload, moofStart, moofSize)
	if err != nil {
		return nil, err
	}

	// The mdat box must follow immediately after the moof box.
	mdatTyp, mdatPayload, err := d.readBox()
	if err != nil {
		return nil, err
	}

	if mdatTyp != "mdat" {
		return nil, fmt.Errorf("%w: expected mdat after moof, got %q", ErrMalformed, mdatTyp)
	}

	pkts := make([]av.Packet, 0, len(samples))

	for _, s := range samples {
		track := d.tracksByID[s.trackID]
		if track == nil {
			continue
		}

		if s.mdatOff < 0 || s.mdatOff+s.size > len(mdatPayload) {
			continue // out-of-bounds sample (malformed fragment)
		}

		raw := mdatPayload[s.mdatOff : s.mdatOff+s.size]

		data := make([]byte, len(raw))
		copy(data, raw)

		dts := ticksToDuration(s.dts, track.timescale)
		dur := ticksToDuration(int64(s.duration), track.timescale)
		pts := ticksToDuration(int64(s.cts), track.timescale)

		// sample_is_non_sync_sample is bit 16 of the sample flags word.
		// 0 means the sample IS a sync point (keyframe); 1 means it is not.
		isKey := (s.flags>>16)&0x1 == 0

		pkts = append(pkts, av.Packet{
			KeyFrame:  isKey,
			Idx:       track.streamIdx,
			DTS:       dts,
			PTSOffset: pts,
			Duration:  dur,
			CodecType: track.codec.Type(),
			Data:      data,
		})
	}

	// Sort interleaved audio and video packets into DTS order.
	sort.Slice(pkts, func(i, j int) bool {
		return pkts[i].DTS < pkts[j].DTS
	})

	return pkts, nil
}

// parseMoof parses the moof box payload and returns raw sample descriptors.
// moofStart is the absolute file position of the first byte of the moof box.
// moofSize is the total size of the moof box (payload + 8-byte header).
func (d *Demuxer) parseMoof(payload []byte, moofStart, moofSize int64) ([]rawSample, error) {
	var all []rawSample

	// The mdat payload starts immediately after the moof box plus the mdat header (8 bytes).
	mdatPayloadBase := moofStart + moofSize + 8

	for _, trafPayload := range findBoxes(payload, "traf") {
		tfhdPayload, ok := findBox(trafPayload, "tfhd")
		if !ok {
			continue
		}

		tfhdRes, err := parseTfhd(tfhdPayload, moofStart)
		if err != nil {
			continue
		}

		track := d.tracksByID[tfhdRes.trackID]
		if track == nil {
			continue // track not declared in moov
		}

		// Override per-track defaults from this tfhd when present.
		if tfhdRes.flags&0x000008 != 0 {
			track.defDuration = tfhdRes.defDuration
		}

		if tfhdRes.flags&0x000010 != 0 {
			track.defSize = tfhdRes.defSize
		}

		if tfhdRes.flags&0x000020 != 0 {
			track.defFlags = tfhdRes.defFlags
		}

		baseDTS := int64(0)

		if tfdtPayload, ok := findBox(trafPayload, "tfdt"); ok {
			baseDTS, err = parseTfdt(tfdtPayload)
			if err != nil {
				return nil, err
			}
		}

		for _, trunPayload := range findBoxes(trafPayload, "trun") {
			samps, err := parseTrun(
				trunPayload, track, baseDTS, tfhdRes.baseDataOffset, mdatPayloadBase,
			)
			if err != nil {
				return nil, err
			}

			all = append(all, samps...)

			// Advance baseDTS for subsequent trun boxes within the same traf.
			for _, s := range samps {
				baseDTS += int64(s.duration)
			}
		}
	}

	return all, nil
}

// tfhdResult holds the parsed fields of a tfhd full-box.
type tfhdResult struct {
	trackID        uint32
	flags          uint32
	baseDataOffset int64
	defDuration    uint32
	defSize        uint32
	defFlags       uint32
}

// parseTfhd parses a tfhd full-box payload. moofStart is used as the default
// base data offset when the default-base-is-moof flag (0x020000) is set.
func parseTfhd(payload []byte, moofStart int64) (tfhdResult, error) {
	var res tfhdResult

	if len(payload) < 8 {
		return res, ErrMalformed
	}

	res.flags = uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	res.trackID = binary.BigEndian.Uint32(payload[4:8])

	// Default base is the moof box start (ISO 14496-12 §8.8.7, default-base-is-moof).
	res.baseDataOffset = moofStart

	off := 8

	if res.flags&0x000001 != 0 { // base-data-offset-present: absolute file offset
		if len(payload) < off+8 {
			return res, ErrMalformed
		}

		res.baseDataOffset = int64(binary.BigEndian.Uint64(payload[off : off+8]))
		off += 8
	}

	if res.flags&0x000002 != 0 { // sample-description-index-present
		off += 4
	}

	if res.flags&0x000008 != 0 { // default-sample-duration-present
		if len(payload) < off+4 {
			return res, ErrMalformed
		}

		res.defDuration = binary.BigEndian.Uint32(payload[off : off+4])
		off += 4
	}

	if res.flags&0x000010 != 0 { // default-sample-size-present
		if len(payload) < off+4 {
			return res, ErrMalformed
		}

		res.defSize = binary.BigEndian.Uint32(payload[off : off+4])
		off += 4
	}

	if res.flags&0x000020 != 0 { // default-sample-flags-present
		if len(payload) < off+4 {
			return res, ErrMalformed
		}

		res.defFlags = binary.BigEndian.Uint32(payload[off : off+4])
	}

	return res, nil
}

// parseTfdt extracts the baseMediaDecodeTime from a tfdt full-box payload.
// version=0 encodes a 32-bit time; version=1 encodes a 64-bit time.
func parseTfdt(payload []byte) (int64, error) {
	if len(payload) < 4 {
		return 0, ErrMalformed
	}

	if payload[0] == 0 { // version 0: 32-bit
		if len(payload) < 8 {
			return 0, ErrMalformed
		}

		return int64(binary.BigEndian.Uint32(payload[4:8])), nil
	}

	// version 1: 64-bit
	if len(payload) < 12 {
		return 0, ErrMalformed
	}

	return int64(binary.BigEndian.Uint64(payload[4:12])), nil
}

// parseTrun parses one trun full-box payload and returns sample descriptors.
// baseDTS is the running decode time in timescale units (from tfdt).
// baseDataOffset is the absolute file position that trun.data_offset is relative to.
// mdatPayloadBase is the absolute file position of the mdat payload start.
func parseTrun(
	payload []byte,
	track *trackDef,
	baseDTS, baseDataOffset, mdatPayloadBase int64,
) ([]rawSample, error) {
	if len(payload) < 8 {
		return nil, ErrMalformed
	}

	_ = payload[0] // version: parsed implicitly; CTS is always treated as signed int32
	trunFlags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	sampleCount := binary.BigEndian.Uint32(payload[4:8])

	off := 8

	// data_offset is signed and relative to baseDataOffset (usually the moof start).
	dataOffset := int64(0)

	if trunFlags&0x000001 != 0 { // data-offset-present
		if len(payload) < off+4 {
			return nil, ErrMalformed
		}

		dataOffset = int64(int32(binary.BigEndian.Uint32(payload[off : off+4])))
		off += 4
	}

	// first-sample-flags overrides defFlags for the first sample only.
	firstSampleFlags := track.defFlags

	if trunFlags&0x000004 != 0 { // first-sample-flags-present
		if len(payload) < off+4 {
			return nil, ErrMalformed
		}

		firstSampleFlags = binary.BigEndian.Uint32(payload[off : off+4])
		off += 4
	}

	// Byte offset of this trun's first sample within the mdat payload.
	mdatOff := int(baseDataOffset + dataOffset - mdatPayloadBase)

	samples := make([]rawSample, 0, sampleCount)
	curDTS := baseDTS

	for i := range int(sampleCount) {
		dur := track.defDuration
		sampleSize := track.defSize
		sflags := track.defFlags

		if i == 0 {
			sflags = firstSampleFlags
		}

		cts := int32(0)

		if trunFlags&0x000100 != 0 { // sample-duration-present
			if len(payload) < off+4 {
				return nil, ErrMalformed
			}

			dur = binary.BigEndian.Uint32(payload[off : off+4])
			off += 4
		}

		if trunFlags&0x000200 != 0 { // sample-size-present
			if len(payload) < off+4 {
				return nil, ErrMalformed
			}

			sampleSize = binary.BigEndian.Uint32(payload[off : off+4])
			off += 4
		}

		if trunFlags&0x000400 != 0 { // sample-flags-present (per-sample)
			if len(payload) < off+4 {
				return nil, ErrMalformed
			}

			sflags = binary.BigEndian.Uint32(payload[off : off+4])
			off += 4
		}

		if trunFlags&0x000800 != 0 { // sample-composition-time-offset-present
			if len(payload) < off+4 {
				return nil, ErrMalformed
			}

			// version=1: signed CTS (B-frame support); version=0: treat as signed too,
			// since ISO 14496-12:2012 §8.8.8.2 effectively uses the same 32-bit field.
			cts = int32(binary.BigEndian.Uint32(payload[off : off+4]))
			off += 4
		}

		samples = append(samples, rawSample{
			trackID:  track.id,
			dts:      curDTS,
			cts:      cts,
			duration: dur,
			flags:    sflags,
			mdatOff:  mdatOff,
			size:     int(sampleSize),
		})

		curDTS += int64(dur)
		mdatOff += int(sampleSize)
	}

	return samples, nil
}

// ticksToDuration converts timescale ticks to a time.Duration.
func ticksToDuration(ticks int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}

	return time.Duration(ticks) * time.Second / time.Duration(timescale)
}
