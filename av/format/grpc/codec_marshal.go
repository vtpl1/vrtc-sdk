// Package grpc implements gRPC-based Muxer and Demuxer for streaming AV packets
// between vrtc nodes (edge → cloud push, cloud → consumer pull).
package grpc

import (
	"fmt"
	"math"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/mjpeg"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

// marshalStream converts an av.Stream to a proto StreamInfo.
func marshalStream(s av.Stream) *pb.StreamInfo {
	si := &pb.StreamInfo{
		Idx:       uint32(s.Idx),
		CodecType: uint32(s.Codec.Type()),
	}

	switch cd := s.Codec.(type) {
	case h264parser.CodecData:
		si.CodecConfig = &pb.StreamInfo_Video{Video: &pb.VideoCodecConfig{
			Width:        int32(cd.Width()),
			Height:       int32(cd.Height()),
			TimeScale:    cd.TimeScale(),
			ConfigRecord: cd.AVCDecoderConfRecordBytes(),
		}}
	case h265parser.CodecData:
		si.CodecConfig = &pb.StreamInfo_Video{Video: &pb.VideoCodecConfig{
			Width:        int32(cd.Width()),
			Height:       int32(cd.Height()),
			TimeScale:    cd.TimeScale(),
			ConfigRecord: cd.HEVCDecoderConfigurationRecordBytes(),
		}}
	case av.AudioCodecData:
		acfg := &pb.AudioCodecConfig{
			SampleFormat:  uint32(cd.SampleFormat()),
			SampleRate:    int32(cd.SampleRate()),
			ChannelLayout: uint32(cd.ChannelLayout()),
		}
		if aac, ok := cd.(aacparser.CodecData); ok {
			acfg.ConfigBytes = aac.MPEG4AudioConfigBytes()
		}

		si.CodecConfig = &pb.StreamInfo_Audio{Audio: acfg}
	case av.VideoCodecData:
		// Generic video codec (e.g., VP8, VP9, AV1) without specific config records.
		si.CodecConfig = &pb.StreamInfo_Video{Video: &pb.VideoCodecConfig{
			Width:     int32(cd.Width()),
			Height:    int32(cd.Height()),
			TimeScale: cd.TimeScale(),
		}}
	default:
		// Minimal codec (e.g., MJPEG) — only type is transmitted.
	}

	return si
}

// unmarshalStream converts a proto StreamInfo back to an av.Stream.
func unmarshalStream(si *pb.StreamInfo) (av.Stream, error) {
	if si.GetIdx() > math.MaxUint16 {
		return av.Stream{}, fmt.Errorf("%w: stream idx %d", errIdxOverflow, si.GetIdx())
	}

	ct := av.CodecType(si.GetCodecType())
	s := av.Stream{Idx: uint16(si.GetIdx())}

	switch cfg := si.GetCodecConfig().(type) {
	case *pb.StreamInfo_Video:
		var err error

		s.Codec, err = unmarshalVideoCodec(ct, cfg.Video)
		if err != nil {
			return s, err
		}
	case *pb.StreamInfo_Audio:
		var err error

		s.Codec, err = unmarshalAudioCodec(ct, cfg.Audio)
		if err != nil {
			return s, err
		}
	default:
		// Minimal codec — only type known.
		s.Codec = genericCodecData{codecType: ct}
	}

	return s, nil
}

// unmarshalVideoCodec reconstructs a video CodecData from proto.
func unmarshalVideoCodec(ct av.CodecType, vc *pb.VideoCodecConfig) (av.CodecData, error) {
	rec := vc.GetConfigRecord()

	switch ct {
	case av.H264:
		if len(rec) == 0 {
			return nil, errH264MissingConfig
		}

		return h264parser.NewCodecDataFromAVCDecoderConfRecord(rec)
	case av.H265:
		if len(rec) == 0 {
			return nil, errH265MissingConfig
		}

		return h265parser.NewCodecDataFromAVCDecoderConfRecord(rec)
	case av.MJPEG:
		return mjpeg.CodecData{}, nil
	default:
		// Generic video codec — no specific reconstruction available.
		return genericVideoCodecData{
			codecType: ct,
			width:     int(vc.GetWidth()),
			height:    int(vc.GetHeight()),
			timeScale: vc.GetTimeScale(),
		}, nil
	}
}

// unmarshalAudioCodec reconstructs an audio CodecData from proto.
func unmarshalAudioCodec(ct av.CodecType, ac *pb.AudioCodecConfig) (av.CodecData, error) {
	sr := int(ac.GetSampleRate())
	cl := av.ChannelLayout(ac.GetChannelLayout())

	switch ct {
	case av.AAC:
		if len(ac.GetConfigBytes()) == 0 {
			return nil, errAACMissingConfig
		}

		return aacparser.NewCodecDataFromMPEG4AudioConfigBytes(ac.GetConfigBytes())
	case av.OPUS:
		return codec.NewOpusCodecData(sr, cl), nil
	case av.PCM_MULAW:
		return pcm.PCMMulawCodecData{
			Typ:        av.PCM_MULAW,
			SmplFormat: av.SampleFormat(ac.GetSampleFormat()),
			SmplRate:   sr,
			ChLayout:   cl,
		}, nil
	case av.PCM_ALAW:
		return pcm.PCMAlawCodecData{
			Typ:        av.PCM_ALAW,
			SmplFormat: av.SampleFormat(ac.GetSampleFormat()),
			SmplRate:   sr,
			ChLayout:   cl,
		}, nil
	case av.SPEEX:
		return pcm.NewSpeexCodecData(sr, cl), nil
	case av.PCM:
		return genericAudioCodecData{
			codecType:     ct,
			sampleFormat:  av.SampleFormat(ac.GetSampleFormat()),
			sampleRate:    sr,
			channelLayout: cl,
		}, nil
	case av.PCML:
		return pcm.PCMLCodecData{
			Typ:        av.PCML,
			SmplFormat: av.SampleFormat(ac.GetSampleFormat()),
			SmplRate:   sr,
			ChLayout:   cl,
		}, nil
	case av.FLAC:
		return pcm.NewFLACCodecData(av.FLAC, uint32(sr), cl), nil
	default:
		return genericAudioCodecData{
			codecType:     ct,
			sampleFormat:  av.SampleFormat(ac.GetSampleFormat()),
			sampleRate:    sr,
			channelLayout: cl,
		}, nil
	}
}

// marshalStreams converts a slice of av.Stream to proto StreamInfo messages.
func marshalStreams(streams []av.Stream) []*pb.StreamInfo {
	out := make([]*pb.StreamInfo, len(streams))
	for i, s := range streams {
		out[i] = marshalStream(s)
	}

	return out
}

// unmarshalStreams converts proto StreamInfo messages to av.Stream slice.
func unmarshalStreams(infos []*pb.StreamInfo) ([]av.Stream, error) {
	out := make([]av.Stream, len(infos))
	for i, si := range infos {
		s, err := unmarshalStream(si)
		if err != nil {
			return nil, err
		}

		out[i] = s
	}

	return out, nil
}

// ── Generic fallback codec data types ────────────────────────

// genericCodecData implements av.CodecData for codecs with no additional metadata.
type genericCodecData struct {
	codecType av.CodecType
}

func (g genericCodecData) Type() av.CodecType { return g.codecType }

// genericVideoCodecData implements av.VideoCodecData for unrecognised video codecs.
type genericVideoCodecData struct {
	codecType av.CodecType
	width     int
	height    int
	timeScale uint32
}

func (g genericVideoCodecData) Type() av.CodecType { return g.codecType }
func (g genericVideoCodecData) Width() int         { return g.width }
func (g genericVideoCodecData) Height() int        { return g.height }
func (g genericVideoCodecData) TimeScale() uint32  { return g.timeScale }

// genericAudioCodecData implements av.AudioCodecData for unrecognised audio codecs.
type genericAudioCodecData struct {
	codecType     av.CodecType
	sampleFormat  av.SampleFormat
	sampleRate    int
	channelLayout av.ChannelLayout
}

func (g genericAudioCodecData) Type() av.CodecType              { return g.codecType }
func (g genericAudioCodecData) SampleFormat() av.SampleFormat   { return g.sampleFormat }
func (g genericAudioCodecData) SampleRate() int                 { return g.sampleRate }
func (g genericAudioCodecData) ChannelLayout() av.ChannelLayout { return g.channelLayout }
func (g genericAudioCodecData) PacketDuration(_ []byte) (time.Duration, error) {
	return 0, errPacketDurationUnsupported
}
