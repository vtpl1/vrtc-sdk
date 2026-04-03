// Package av defines the core audio/video interfaces and types used throughout
// vrtc-sdk. It provides codec type constants, stream metadata structures, and
// the foundational interfaces (Demuxer, Muxer, CodecData) that all format and
// transport packages build on.
//
// # Packet format
//
// H.264 and H.265 video data inside a Packet is always in AVCC format (ISO
// 14496-15): each NALU is preceded by a big-endian length field. This library
// always uses 4-byte length fields (lengthSizeMinusOne=3 in the decoder config
// record); the spec allows 1- or 2-byte lengths but they are not supported here.
// Use av/codec/parser to convert between AVCC and Annex B.
//
// # Codec types
//
// Codec types are opaque uint32 values created by MakeVideoCodecType or
// MakeAudioCodecType. The package-level variables (H264, AAC, …) are the
// canonical instances; compare with == rather than type-asserting.
package av

import "time"

// CodecType represents Video/Audio codec type. can be H264/AAC/SPEEX/...
type CodecType uint32

const (
	codecTypeAudioBit  = 0x1
	codecTypeOtherBits = 1
	avCodecTypeMagic   = 233333
)

// MakeVideoCodecType makes a new video codec type.
func MakeVideoCodecType(base uint32) CodecType {
	c := CodecType(base) << codecTypeOtherBits

	return c
}

// MakeAudioCodecType makes a new audio codec type.
func MakeAudioCodecType(base uint32) CodecType {
	c := CodecType(base)<<codecTypeOtherBits | CodecType(codecTypeAudioBit)

	return c
}

// Well-known codec type singletons. Video types are created with
// MakeVideoCodecType; audio types with MakeAudioCodecType (low bit set).
//
// RTP payload type notes:
//   - Static PTs (assigned by RFC 3551): PCMU=0, PCMA=8, JPEG=26, MPA=14.
//   - Dynamic PTs (96–127, negotiated via SDP per RFC 3551 §3): H.264 and Opus
//     have no static assignment; values 96 and 111 are conventional defaults
//     widely used in WebRTC and RTSP but are not mandated by any RFC.
var (
	UNKNOWN    = MakeVideoCodecType(avCodecTypeMagic + 0)
	H264       = MakeVideoCodecType(avCodecTypeMagic + 1) // dynamic RTP PT, conventionally 96
	H265       = MakeVideoCodecType(avCodecTypeMagic + 2)
	JPEG       = MakeVideoCodecType(avCodecTypeMagic + 3) // static RTP PT: 26 (RFC 3551)
	VP8        = MakeVideoCodecType(avCodecTypeMagic + 4)
	VP9        = MakeVideoCodecType(avCodecTypeMagic + 5)
	AV1        = MakeVideoCodecType(avCodecTypeMagic + 6)
	MJPEG      = MakeVideoCodecType(avCodecTypeMagic + 7)
	AAC        = MakeAudioCodecType(avCodecTypeMagic + 1) // MPEG4-GENERIC
	PCM_MULAW  = MakeAudioCodecType(avCodecTypeMagic + 2) // static RTP PT: 0  (RFC 3551, PCMU)
	PCM_ALAW   = MakeAudioCodecType(avCodecTypeMagic + 3) // static RTP PT: 8  (RFC 3551, PCMA)
	SPEEX      = MakeAudioCodecType(avCodecTypeMagic + 4) // Speex
	NELLYMOSER = MakeAudioCodecType(avCodecTypeMagic + 5)
	PCM        = MakeAudioCodecType(avCodecTypeMagic + 6)
	OPUS       = MakeAudioCodecType(avCodecTypeMagic + 7) // dynamic RTP PT, conventionally 111
	MP3        = MakeAudioCodecType(
		avCodecTypeMagic + 8,
	) // static RTP PT: 14 (RFC 3551, MPA — MPEG-1 Layer III)
	PCML = MakeAudioCodecType(avCodecTypeMagic + 9)  // Linear PCM (little endian)
	ELD  = MakeAudioCodecType(avCodecTypeMagic + 10) // AAC-ELD
	FLAC = MakeAudioCodecType(avCodecTypeMagic + 11)
)

// String returns the human-readable name of the codec type (e.g. "H264", "AAC").
// Returns an empty string for unrecognised types.
func (s CodecType) String() string {
	switch s {
	case H264:
		return "H264"
	case H265:
		return "H265"
	case JPEG:
		return "JPEG"
	case VP8:
		return "VP8"
	case VP9:
		return "VP9"
	case AV1:
		return "AV1"
	case AAC:
		return "AAC"
	case PCM_MULAW:
		return "PCM_MULAW"
	case PCM_ALAW:
		return "PCM_ALAW"
	case SPEEX:
		return "SPEEX"
	case NELLYMOSER:
		return "NELLYMOSER"
	case PCM:
		return "PCM"
	case OPUS:
		return "OPUS"
	case MP3:
		return "MPA"
	case PCML:
		return "PCML"
	case ELD:
		return "AAC_ELD"
	case FLAC:
		return "FLAC"
	}

	return ""
}

// MarshalJSON encodes CodecType as its human-readable name.
func (s CodecType) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// IsAudio reports whether the codec type represents an audio codec.
func (s CodecType) IsAudio() bool {
	return s&codecTypeAudioBit != 0
}

// IsVideo reports whether the codec type represents a video codec.
func (s CodecType) IsVideo() bool {
	return s&codecTypeAudioBit == 0
}

// Stream pairs a stream index with its codec configuration.
// Idx matches Packet.Idx; it is the authoritative identifier and must not be inferred
// from the slice position, as stream indices may be non-contiguous (e.g. MPEG-TS PIDs).
type Stream struct {
	Idx   uint16 // stream index; matches Packet.Idx
	Codec CodecData
}

// CodecData is some important bytes for initialising audio/video decoder,
// can be converted to VideoCodecData or AudioCodecData using:
//
//	codecdata.(AudioCodecData) or codecdata.(VideoCodecData)
//
// for H264, CodecData is AVCDecoderConfigurationRecord bytes, includes SPS/PPS.
// for H265, CodecData is HEVCDecoderConfigurationRecord bytes, includes VPS/SPS/PPS.
type CodecData interface {
	Type() CodecType // Video/Audio codec type
}

// VideoCodecData extends CodecData with video-specific properties.
type VideoCodecData interface {
	CodecData
	Width() int        // Video width
	Height() int       // Video height
	TimeScale() uint32 // clock frequency for timestamp conversion (e.g. 90000 for RTP and fMP4/CMAF)
}

// AudioCodecData extends CodecData with audio-specific properties.
type AudioCodecData interface {
	CodecData
	SampleFormat() SampleFormat                       // audio sample format
	SampleRate() int                                  // audio sample rate
	ChannelLayout() ChannelLayout                     // audio channel layout
	PacketDuration(pkt []byte) (time.Duration, error) // get audio compressed packet duration
}
