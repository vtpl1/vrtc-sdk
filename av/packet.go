package av

import (
	"fmt"
	"strings"
	"time"
)

// Packet stores one unit of compressed audio or video data flowing through
// the pipeline. See docs/av-packet-spec.md for the full field contract.
type Packet struct {
	// ── Flags ─────────────────────────────────────────────────────────────
	KeyFrame        bool // true iff this is an IDR/keyframe video packet; always false for audio
	IsDiscontinuity bool // DTS does not follow from the previous packet; receivers must reinitialise timing

	// ── Identity / routing ────────────────────────────────────────────────
	Idx       uint16    // stream index; matches Stream.Idx from GetCodecs
	CodecType CodecType // codec of this packet

	// FrameID is a stable identity assigned by the source device or stream.
	// Comparable across sessions for the same source. 0 means not assigned.
	FrameID int64

	// ── Timing ────────────────────────────────────────────────────────────
	DTS           time.Duration // decode timestamp; monotonically non-decreasing within a stream
	PTSOffset     time.Duration // PTS = DTS + PTSOffset; zero for codecs without B-frames
	Duration      time.Duration // nominal presentation duration; never 0 from a well-behaved demuxer
	WallClockTime time.Time     // wall-clock capture/arrival time; zero means not set

	// ── Payload ───────────────────────────────────────────────────────────
	// Data carries the compressed media payload:
	//   H.264/H.265 video — AVCC format: one or more NALUs, each prefixed with
	//                        a 4-byte big-endian length (ISO 14496-15). This is
	//                        the native format for MP4/fMP4 containers.
	//   Audio             — raw encoded samples, no container framing
	//                        (ADTS stripped for AAC).
	//   Empty (nil/len=0) — valid only for a pure codec-change notification
	//                        (KeyFrame==true, NewCodecs!=nil, no media data).
	Data []byte

	// Analytics carries per-frame analytics results (object detection, face
	// recognition, license plate recognition, etc.). nil when absent.
	Analytics *FrameAnalytics

	// ── Codec change ──────────────────────────────────────────────────────
	// NewCodecs is non-nil on the keyframe packet that immediately follows a
	// parameter-set change. Contains only the streams whose codec changed.
	// Receivers must update per-stream codec state when this is non-nil.
	NewCodecs []Stream
}

// PTS returns the presentation timestamp (DTS + PTSOffset).
// For streams without B-frames PTS == DTS and PTSOffset is zero.
func (m *Packet) PTS() time.Duration {
	return m.DTS + m.PTSOffset
}

// HasWallClockTime reports whether a wall-clock capture time has been set.
func (m *Packet) HasWallClockTime() bool {
	return !m.WallClockTime.IsZero()
}

// String returns a compact human-readable description of the packet for logging.
//
// Format: #<id> <codec>:<idx> dts=<ms>ms [pts=<ms>ms] dur=<ms>ms <size> [K] [DISC] [<nalu>]
//
// Examples:
//
//	#42 H264:0 dts=1234ms dur=33ms 12.3KB K IDR_SLICE
//	#42 H264:0 dts=1234ms pts=1267ms dur=33ms 12.3KB NON_IDR_SLICE
//	#43 AAC:1 dts=1234ms dur=21ms 1.2KB
//	#44 H264:0 dts=1267ms dur=33ms 8.1KB K DISC IDR_SLICE
func (m *Packet) String() string {
	var b strings.Builder

	fmt.Fprintf(&b, "#%d %s:%d dts=%dms", m.FrameID, m.CodecType, m.Idx, m.DTS.Milliseconds())

	if m.PTSOffset != 0 {
		fmt.Fprintf(&b, " pts=%dms", m.PTS().Milliseconds())
	}

	fmt.Fprintf(&b, " dur=%dms %s", m.Duration.Milliseconds(), packetSizeString(len(m.Data)))

	if m.KeyFrame {
		b.WriteString(" K")
	}

	if m.IsDiscontinuity {
		b.WriteString(" DISC")
	}

	if m.CodecType.IsVideo() && len(m.Data) > 0 {
		switch m.CodecType {
		case H264:
			fmt.Fprintf(&b, " %s", H264NaluType(m.Data[0])&H264NALTypeMask)
		case H265:
			fmt.Fprintf(&b, " %s", H265NaluType(m.Data[0]>>1)&H265NALTypeMask)
		}
	}

	return b.String()
}

// packetSizeString formats a byte count as a human-readable size string.
func packetSizeString(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// GoString returns a detailed developer-friendly representation of the packet.
// Used when printing with %#v.
func (m *Packet) GoString() string {
	wallStr := "not set"
	if m.HasWallClockTime() {
		wallStr = m.WallClockTime.String()
	}

	return fmt.Sprintf(
		"&av.Packet{\n"+
			"  FrameID:         %d,\n"+
			"  KeyFrame:        %t,\n"+
			"  IsDiscontinuity: %t,\n"+
			"  Idx:             %d,\n"+
			"  CodecType:       %s,\n"+
			"  DTS:             %s,\n"+
			"  PTS:             %s,\n"+
			"  PTSOffset:       %s,\n"+
			"  Duration:        %s,\n"+
			"  WallClockTime:   %s,\n"+
			"  DataLen:         %d,\n"+
			"  Analytics:       %s,\n"+
			"}",
		m.FrameID,
		m.KeyFrame,
		m.IsDiscontinuity,
		m.Idx,
		m.CodecType.String(),
		m.DTS,
		m.PTS(),
		m.PTSOffset,
		m.Duration,
		wallStr,
		len(m.Data),
		m.Analytics.String(),
	)
}

func (m *Packet) IsKeyFrame() bool {
	return m.KeyFrame
}

func (m *Packet) IsAudio() bool {
	return m.CodecType.IsAudio()
}

func (m *Packet) IsVideo() bool {
	return m.CodecType.IsVideo()
}
