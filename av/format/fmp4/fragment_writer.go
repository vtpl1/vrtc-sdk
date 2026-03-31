package fmp4

import (
	"bytes"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
)

// FragmentWriter builds fMP4 media fragments on demand.
// Unlike Muxer, the caller decides when to flush each fragment — suitable for
// LL-HLS parts, DASH segments, or any use case requiring explicit boundaries.
//
// Usage:
//
//	fw, initSeg, err := fmp4.NewFragmentWriter(streams)
//	// write initSeg to the client once (ftyp+moov)
//	for each group of packets:
//	    fw.WritePacket(pkt)
//	    fragment := fw.Flush()   // moof+mdat
//	    // write fragment to the client
type FragmentWriter struct {
	tracks   []*trackState
	trackMap map[uint16]*trackState
	seqNum   uint32
	emsgID   uint32 // monotonically increasing emsg event id
	hasVideo bool
}

// NewFragmentWriter creates a FragmentWriter for the given streams and returns
// the fMP4 initialisation segment (ftyp+moov). The init segment must be sent
// to the client before any fragment produced by Flush.
func NewFragmentWriter(streams []av.Stream) (*FragmentWriter, []byte, error) {
	tracks := make([]*trackState, 0, len(streams))
	trackMap := make(map[uint16]*trackState, len(streams))
	hasVideo := false

	for i, s := range streams {
		ts, err := newTrackState(s, uint32(i+1))
		if err != nil {
			return nil, nil, err
		}

		tracks = append(tracks, ts)
		trackMap[s.Idx] = ts

		if ts.hasVideo {
			hasVideo = true
		}
	}

	init := buildInitSegment(tracks)

	return &FragmentWriter{
		tracks:   tracks,
		trackMap: trackMap,
		seqNum:   0,
		hasVideo: hasVideo,
	}, init, nil
}

// WritePacket adds pkt to the current fragment buffer.
// Unknown stream indices are silently dropped.
func (fw *FragmentWriter) WritePacket(pkt av.Packet) {
	ts := fw.trackMap[pkt.Idx]
	if ts == nil {
		return
	}

	newDTS := dtsToTimescale(pkt.DTS, ts.timescale)

	if len(ts.samples) == 0 {
		ts.baseTime = newDTS
	} else {
		prev := &ts.samples[len(ts.samples)-1]
		if prev.duration == 0 && newDTS > prev.dts {
			prev.duration = uint32(newDTS - prev.dts)
		}
	}

	ts.samples = append(ts.samples, makeSample(pkt, ts))
}

// HasSamples reports whether any track has buffered samples.
func (fw *FragmentWriter) HasSamples() bool {
	for _, ts := range fw.tracks {
		if len(ts.samples) > 0 {
			return true
		}
	}

	return false
}

// FirstIsKeyframe reports whether the first video sample in the buffer is a
// keyframe (IDR). Returns false for audio-only streams.
func (fw *FragmentWriter) FirstIsKeyframe() bool {
	for _, ts := range fw.tracks {
		if ts.hasVideo && len(ts.samples) > 0 {
			return ts.samples[0].flags == sampleFlagsKeyframe
		}
	}

	return false
}

// HasVideo reports whether any stream is a video track.
func (fw *FragmentWriter) HasVideo() bool {
	return fw.hasVideo
}

// Flush emits a moof+mdat fragment for all currently buffered samples and
// clears the internal buffers. Returns nil if no samples are buffered.
func (fw *FragmentWriter) Flush() []byte {
	active := make([]*trackState, 0, len(fw.tracks))

	for _, ts := range fw.tracks {
		if len(ts.samples) > 0 {
			active = append(active, ts)
		}
	}

	if len(active) == 0 {
		return nil
	}

	// Patch the last sample duration for each track (carry forward preceding).
	for _, ts := range active {
		last := &ts.samples[len(ts.samples)-1]
		if last.duration == 0 && len(ts.samples) >= 2 {
			last.duration = ts.samples[len(ts.samples)-2].duration
		}
	}

	fw.seqNum++

	dataOffsets := make([]uint32, len(active))
	moofSize := estimateMoofSize(active)
	offset := moofSize + 8 // +8 for mdat box header

	for i, ts := range active {
		dataOffsets[i] = offset
		for _, s := range ts.samples {
			offset += s.size
		}
	}

	emsgs, nextID := collectEmsg(active, fw.emsgID)
	fw.emsgID = nextID

	moof := buildMoof(active, fw.seqNum, dataOffsets)
	mdat := buildMdat(active)

	for _, ts := range active {
		for _, s := range ts.samples {
			ts.nextTime += int64(s.duration)
		}

		ts.samples = ts.samples[:0]
	}

	var out bytes.Buffer
	out.Write(emsgs)
	out.Write(moof)
	out.Write(mdat)

	return out.Bytes()
}

// CodecDataForStreams returns the codec data for each stream, keyed by stream
// index. Used by callers that need per-track metadata (e.g. codec tags).
func (fw *FragmentWriter) CodecDataForStreams() map[uint16]av.CodecData {
	m := make(map[uint16]av.CodecData, len(fw.tracks))
	for _, ts := range fw.tracks {
		m[ts.streamIdx] = ts.codec
	}

	return m
}

// BuildInitSegment builds a standalone fMP4 initialisation segment
// (ftyp+moov) for the given streams without creating a FragmentWriter.
// Useful when the init segment must be stored separately.
func BuildInitSegment(streams []av.Stream) ([]byte, error) {
	tracks := make([]*trackState, 0, len(streams))

	for i, s := range streams {
		ts, err := newTrackState(s, uint32(i+1))
		if err != nil {
			return nil, err
		}

		tracks = append(tracks, ts)
	}

	return buildInitSegment(tracks), nil
}

// CodecTag returns the HLS/DASH codec string for the given CodecData, e.g.
// "avc1.640028" for H.264, "hvc1.1.6.L120.90" for H.265, "mp4a.40.2" for AAC.
// Returns an empty string for unsupported codecs.
func CodecTag(c av.CodecData) string {
	switch v := c.(type) {
	case h264parser.CodecData:
		return v.Tag()
	case h265parser.CodecData:
		return v.Tag()
	case aacparser.CodecData:
		return v.Tag()
	default:
		return ""
	}
}
