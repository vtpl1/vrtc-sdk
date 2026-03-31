package codec

import (
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// OpusCodecData holds the codec parameters for an Opus audio stream.
// It implements av.AudioCodecData.
type OpusCodecData struct {
	typ      av.CodecType
	SampleRt int
	ChLayout av.ChannelLayout
}

// NewOpusCodecData returns an av.AudioCodecData for an Opus stream with the
// given sample rate and channel layout.
func NewOpusCodecData(sr int, cc av.ChannelLayout) av.AudioCodecData {
	return OpusCodecData{
		typ:      av.OPUS,
		SampleRt: sr,
		ChLayout: cc,
	}
}

// ChannelLayout implements av.AudioCodecData.
func (s OpusCodecData) ChannelLayout() av.ChannelLayout {
	return s.ChLayout
}

// PacketDuration implements av.AudioCodecData. Returns a fixed 20 ms, which is
// the recommended default per RFC 6716 §2.1.4 and the frame size used by WebRTC
// and most RTSP implementations. Opus also supports 2.5, 5, 10, 40, and 60 ms
// frames; callers requiring exact per-packet durations must parse the Opus TOC byte.
func (s OpusCodecData) PacketDuration(_ []byte) (time.Duration, error) {
	return time.Duration(20) * time.Millisecond, nil
}

// SampleFormat implements av.AudioCodecData.
func (s OpusCodecData) SampleFormat() av.SampleFormat {
	return av.FLT
}

// SampleRate implements av.AudioCodecData.
func (s OpusCodecData) SampleRate() int {
	return s.SampleRt
}

// Type implements av.CodecData. Always returns av.OPUS.
func (s OpusCodecData) Type() av.CodecType {
	return av.OPUS
}
