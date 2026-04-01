package codec

import (
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// RTSPAudioCodecData carries RTSP-specific track metadata for audio codecs while
// still implementing av.AudioCodecData.
type RTSPAudioCodecData struct {
	AudioCodec  av.AudioCodecData
	ControlURL  string
	ClockRate   int
	Channels    int
	PayloadType uint8
	Fmtp        map[string]string
}

func (d RTSPAudioCodecData) Type() av.CodecType {
	return d.AudioCodec.Type()
}

func (d RTSPAudioCodecData) SampleFormat() av.SampleFormat {
	return d.AudioCodec.SampleFormat()
}

func (d RTSPAudioCodecData) SampleRate() int {
	return d.AudioCodec.SampleRate()
}

func (d RTSPAudioCodecData) ChannelLayout() av.ChannelLayout {
	return d.AudioCodec.ChannelLayout()
}

func (d RTSPAudioCodecData) PacketDuration(pkt []byte) (time.Duration, error) {
	return d.AudioCodec.PacketDuration(pkt)
}

func (d RTSPAudioCodecData) RTPClockRate() int {
	if d.ClockRate > 0 {
		return d.ClockRate
	}

	return d.AudioCodec.SampleRate()
}

func (d RTSPAudioCodecData) FMTPValue(key string) string {
	if d.Fmtp == nil {
		return ""
	}

	return d.Fmtp[key]
}
