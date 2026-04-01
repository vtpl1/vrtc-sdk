// Package codec provides codec-specific data types and utilities for audio and
// video streams. It includes SDP parsing for RTSP media tracks.
package codec

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

// SdpToCodecs parses an SDP session description and returns CodecData for each
// supported RTSP media track. Unsupported tracks are silently skipped.
//
//nolint:funlen
func SdpToCodecs(s string) ([]av.CodecData, error) {
	sd := sdp.SessionDescription{}
	err := sd.UnmarshalString(s)

	var ret []av.CodecData

	if err != nil {
		return ret, err
	}

	for _, media := range sd.MediaDescriptions {
		mediaStr := media.MediaName.Media

		field, ok := media.Attribute("rtpmap")
		if !ok {
			continue
		}

		payloadType, mediaTypeStr, clockRate, channels := parseRTPMap(field)
		if mediaTypeStr == "" {
			continue
		}

		controlURL := ""

		field, ok = media.Attribute("control")
		if ok {
			controlURL = field
		}

		field, _ = media.Attribute("fmtp")
		fmtp := parseFMTP(field)

		switch mediaStr {
		case "video":
			codecData, ok := parseVideoCodec(mediaTypeStr, controlURL, fmtp)
			if ok {
				ret = append(ret, codecData)
			}
		case "audio":
			codecData, ok := parseAudioCodec(
				mediaTypeStr,
				controlURL,
				payloadType,
				clockRate,
				channels,
				fmtp,
			)
			if ok {
				ret = append(ret, codecData)
			}
		default:
			continue
		}
	}

	return ret, nil
}

func parseRTPMap(field string) (uint8, string, int, int) {
	keyval := strings.Split(field, " ")
	if len(keyval) < 2 {
		return 0, "", 0, 0
	}

	pt64, err := strconv.ParseUint(strings.TrimSpace(keyval[0]), 10, 8)
	if err != nil {
		return 0, "", 0, 0
	}

	parts := strings.Split(keyval[1], "/")
	if len(parts) < 2 {
		return 0, "", 0, 0
	}

	clockRate, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", 0, 0
	}

	channels := 0
	if len(parts) >= 3 {
		channels, _ = strconv.Atoi(parts[2])
	}

	return uint8(pt64), parts[0], clockRate, channels
}

func parseFMTP(field string) map[string]string {
	out := make(map[string]string)
	if field == "" {
		return out
	}

	keyval := strings.FieldsFunc(field, func(r rune) bool {
		return r == ' ' || r == ';'
	})

	for _, field := range keyval {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])

		val := strings.TrimSpace(parts[1])
		if key == "" || val == "" {
			continue
		}

		out[strings.ToLower(key)] = val
	}

	return out
}

func parseVideoCodec(
	mediaTypeStr string,
	controlURL string,
	fmtp map[string]string,
) (av.CodecData, bool) {
	var spropVPS, spropSPS, spropPPS []byte

	for key, val := range fmtp {
		switch key {
		case "sprop-vps":
			valb, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				continue
			}

			spropVPS = valb
		case "sprop-sps":
			valb, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				continue
			}

			spropSPS = valb
		case "sprop-pps":
			valb, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				continue
			}

			spropPPS = valb
		case "sprop-parameter-sets":
			fields := strings.Split(val, ",")
			for idx, field := range fields {
				valb, err := base64.StdEncoding.DecodeString(field)
				if err != nil {
					continue
				}

				switch strings.ToUpper(mediaTypeStr) {
				case av.H264.String():
					switch idx {
					case 0:
						spropSPS = valb
					case 1:
						spropPPS = valb
					}
				case av.H265.String():
					switch idx {
					case 0:
						spropVPS = valb
					case 1:
						spropSPS = valb
					case 2:
						spropPPS = valb
					}
				}
			}
		}
	}

	switch strings.ToUpper(mediaTypeStr) {
	case av.H264.String():
		codecData, err := h264parser.NewCodecDataFromSPSAndPPS(spropSPS, spropPPS)
		if err != nil {
			return nil, false
		}

		codecData.ControlURL = controlURL

		return codecData, true
	case av.H265.String():
		codecData, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(spropVPS, spropSPS, spropPPS)
		if err != nil {
			return nil, false
		}

		codecData.ControlURL = controlURL

		return codecData, true
	default:
		return nil, false
	}
}

func parseAudioCodec(
	mediaTypeStr string,
	controlURL string,
	payloadType uint8,
	clockRate int,
	channels int,
	fmtp map[string]string,
) (av.CodecData, bool) {
	layout := channelLayoutFromCount(channels)

	switch strings.ToUpper(mediaTypeStr) {
	case "MPEG4-GENERIC":
		configHex := fmtp["config"]
		if configHex == "" {
			return nil, false
		}

		config, err := hex.DecodeString(configHex)
		if err != nil {
			return nil, false
		}

		base, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(config)
		if err != nil {
			return nil, false
		}

		return RTSPAudioCodecData{
			AudioCodec:  base,
			ControlURL:  controlURL,
			ClockRate:   clockRate,
			Channels:    channels,
			PayloadType: payloadType,
			Fmtp:        fmtp,
		}, true
	case "PCMU":
		return RTSPAudioCodecData{
			AudioCodec: pcm.PCMMulawCodecData{
				Typ:        av.PCM_MULAW,
				SmplFormat: av.S16,
				SmplRate:   defaultIfZero(clockRate, 8000),
				ChLayout:   layout,
			},
			ControlURL:  controlURL,
			ClockRate:   defaultIfZero(clockRate, 8000),
			Channels:    defaultIfZero(channels, 1),
			PayloadType: payloadType,
			Fmtp:        fmtp,
		}, true
	case "PCMA":
		return RTSPAudioCodecData{
			AudioCodec: pcm.PCMAlawCodecData{
				Typ:        av.PCM_ALAW,
				SmplFormat: av.S16,
				SmplRate:   defaultIfZero(clockRate, 8000),
				ChLayout:   layout,
			},
			ControlURL:  controlURL,
			ClockRate:   defaultIfZero(clockRate, 8000),
			Channels:    defaultIfZero(channels, 1),
			PayloadType: payloadType,
			Fmtp:        fmtp,
		}, true
	case "OPUS":
		return RTSPAudioCodecData{
			AudioCodec:  NewOpusCodecData(defaultIfZero(clockRate, 48000), layout),
			ControlURL:  controlURL,
			ClockRate:   defaultIfZero(clockRate, 48000),
			Channels:    defaultIfZero(channels, 2),
			PayloadType: payloadType,
			Fmtp:        fmtp,
		}, true
	default:
		return nil, false
	}
}

func channelLayoutFromCount(channels int) av.ChannelLayout {
	switch channels {
	case 2:
		return av.ChStereo
	default:
		return av.ChMono
	}
}

func defaultIfZero(v int, fallback int) int {
	if v > 0 {
		return v
	}

	return fallback
}
