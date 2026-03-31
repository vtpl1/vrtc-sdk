package h264parser

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/utils/bits/pio"
)

// CodecData holds the decoded H.264 codec parameters.
type CodecData struct {
	Record     []byte
	RecordInfo AVCDecoderConfRecord
	SPSInfo    SPSInfo
	ControlURL string
}

// NewCodecDataFromAVCDecoderConfRecord parses an avcC (AVC decoder configuration record).
func NewCodecDataFromAVCDecoderConfRecord(record []byte) (CodecData, error) {
	var s CodecData

	s.Record = record
	if _, err := (&s.RecordInfo).Unmarshal(record); err != nil {
		return s, err
	}

	if len(s.RecordInfo.SPS) == 0 {
		return s, ErrSPSNotFound
	}

	if len(s.RecordInfo.PPS) == 0 {
		return s, ErrPPSNotFound
	}

	var err error
	if s.SPSInfo, err = ParseSPS(s.RecordInfo.SPS[0]); err != nil {
		return s, errors.Join(ErrSPSNotFound, err)
	}

	return s, nil
}

// NewCodecDataFromSPSAndPPS constructs CodecData from raw SPS and PPS NALUs.
func NewCodecDataFromSPSAndPPS(sps, pps []byte) (CodecData, error) {
	var s CodecData

	recordinfo := AVCDecoderConfRecord{
		AVCProfileIndication: sps[1],
		ProfileCompatibility: sps[2],
		AVCLevelIndication:   sps[3],
		SPS:                  [][]byte{sps},
		PPS:                  [][]byte{pps},
		LengthSizeMinusOne:   3,
	}

	buf := make([]byte, recordinfo.Len())
	recordinfo.Marshal(buf)

	s.RecordInfo = recordinfo
	s.Record = buf

	var err error
	if s.SPSInfo, err = ParseSPS(sps); err != nil {
		return s, err
	}

	return s, nil
}

func (s CodecData) Type() av.CodecType {
	return av.H264
}

// AVCDecoderConfRecordBytes returns the raw avcC box bytes.
func (s CodecData) AVCDecoderConfRecordBytes() []byte {
	return s.Record
}

// SPS returns the first SPS NALU from the configuration record.
func (s CodecData) SPS() []byte {
	if len(s.RecordInfo.SPS) > 0 {
		return s.RecordInfo.SPS[0]
	}

	return []byte{0}
}

// PPS returns the first PPS NALU from the configuration record.
func (s CodecData) PPS() []byte {
	if len(s.RecordInfo.PPS) > 0 {
		return s.RecordInfo.PPS[0]
	}

	return []byte{0}
}

func (s CodecData) Width() int {
	return int(s.SPSInfo.Width)
}

func (s CodecData) Height() int {
	return int(s.SPSInfo.Height)
}

// TimeScale returns 90000 Hz per RFC 6184 / CMAF recommendation.
func (s CodecData) TimeScale() uint32 {
	return 90000
}

func (s CodecData) FPS() int {
	return int(s.SPSInfo.FPS)
}

func (s CodecData) TrackID() string {
	return s.ControlURL
}

func (s CodecData) Resolution() string {
	return fmt.Sprintf("%vx%v", s.Width(), s.Height())
}

// Tag returns an RFC 6381 codec string (e.g. "avc1.64001E").
func (s CodecData) Tag() string {
	return fmt.Sprintf(
		"avc1.%02X%02X%02X",
		s.RecordInfo.AVCProfileIndication,
		s.RecordInfo.ProfileCompatibility,
		s.RecordInfo.AVCLevelIndication,
	)
}

// Bandwidth returns an estimated bitrate string in bits/second.
func (s CodecData) Bandwidth() string {
	fps := s.FPS()
	if fps == 0 {
		fps = 25
	}

	return strconv.Itoa(
		(int(float64(s.Width()) * (float64(1.71) * (30 / float64(fps))))) * 1000,
	)
}

// PacketDuration returns the nominal frame duration.
func (s CodecData) PacketDuration(_ []byte) time.Duration {
	fps := s.FPS()
	if fps == 0 {
		fps = 25
	}

	return time.Duration(1000./float64(fps)) * time.Millisecond
}

// AVCDecoderConfRecord is the avcC box payload (ISO 14496-15).
type AVCDecoderConfRecord struct {
	AVCProfileIndication uint8
	ProfileCompatibility uint8
	AVCLevelIndication   uint8
	LengthSizeMinusOne   uint8
	SPS                  [][]byte
	PPS                  [][]byte
}

// Unmarshal parses the avcC record from b, returning the number of bytes consumed.
func (s *AVCDecoderConfRecord) Unmarshal(b []byte) (int, error) {
	if len(b) < 7 {
		return 0, ErrDecconfInvalid
	}

	s.AVCProfileIndication = b[1]
	s.ProfileCompatibility = b[2]
	s.AVCLevelIndication = b[3]
	s.LengthSizeMinusOne = b[4] & 0x03
	spscount := int(b[5] & 0x1f)
	n := 6

	for range spscount {
		if len(b) < n+2 {
			return n, ErrDecconfInvalid
		}

		spslen := int(pio.U16BE(b[n:]))
		n += 2

		if len(b) < n+spslen {
			return n, ErrDecconfInvalid
		}

		s.SPS = append(s.SPS, b[n:n+spslen])
		n += spslen
	}

	if len(b) < n+1 {
		return n, ErrDecconfInvalid
	}

	ppscount := int(b[n])
	n++

	for range ppscount {
		if len(b) < n+2 {
			return n, ErrDecconfInvalid
		}

		ppslen := int(pio.U16BE(b[n:]))
		n += 2

		if len(b) < n+ppslen {
			return n, ErrDecconfInvalid
		}

		s.PPS = append(s.PPS, b[n:n+ppslen])
		n += ppslen
	}

	return n, nil
}

// Len returns the marshalled size of the record in bytes.
func (s *AVCDecoderConfRecord) Len() int {
	n := 7
	for _, sps := range s.SPS {
		n += 2 + len(sps)
	}

	for _, pps := range s.PPS {
		n += 2 + len(pps)
	}

	return n
}

// Marshal serialises the record into b and returns the number of bytes written.
func (s *AVCDecoderConfRecord) Marshal(b []byte) int {
	n := 0
	b[0] = 1
	b[1] = s.AVCProfileIndication
	b[2] = s.ProfileCompatibility
	b[3] = s.AVCLevelIndication
	b[4] = s.LengthSizeMinusOne | 0xfc
	b[5] = uint8(len(s.SPS)) | 0xe0
	n += 6

	for _, sps := range s.SPS {
		pio.PutU16BE(b[n:], uint16(len(sps)))
		n += 2
		copy(b[n:], sps)
		n += len(sps)
	}

	b[n] = uint8(len(s.PPS))
	n++

	for _, pps := range s.PPS {
		pio.PutU16BE(b[n:], uint16(len(pps)))
		n += 2
		copy(b[n:], pps)
		n += len(pps)
	}

	return n
}
