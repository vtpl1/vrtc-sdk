package h265parser

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/utils/bits/pio"
)

// CodecData holds the decoded H.265 codec parameters.
type CodecData struct {
	Record     []byte
	RecordInfo HEVCDecoderConfigurationRecord
	SPSInfo    SPSInfo
	ControlURL string
}

// NewCodecDataFromAVCDecoderConfRecord parses an hvcC (HEVC decoder configuration record).
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

	if len(s.RecordInfo.VPS) == 0 {
		return s, ErrVPSNotFound
	}

	var err error
	if s.SPSInfo, err = ParseSPS(s.RecordInfo.SPS[0]); err != nil {
		return s, errors.Join(ErrSPSParseFailed, err)
	}

	return s, nil
}

// NewCodecDataFromVPSAndSPSAndPPS constructs CodecData from raw VPS/SPS/PPS NALUs.
func NewCodecDataFromVPSAndSPSAndPPS(vps, sps, pps []byte) (CodecData, error) {
	var s CodecData

	if len(sps) < 6 {
		return s, ErrInvalidSPS
	}

	var err error
	if s.SPSInfo, err = ParseSPS(sps); err != nil {
		return s, err
	}

	recordinfo := HEVCDecoderConfigurationRecord{
		GeneralProfileIDC:                uint8(s.SPSInfo.generalProfileIDC),
		GeneralLevelIDC:                  uint8(s.SPSInfo.generalLevelIDC),
		GeneralTierFlag:                  uint8(s.SPSInfo.generalTierFlag),
		GeneralProfileCompatibilityFlags: s.SPSInfo.generalProfileCompatibilityFlags,
		GeneralConstraintIndicatorFlags:  s.SPSInfo.generalConstraintIndicatorFlags,
		SPS:                              [][]byte{sps},
		PPS:                              [][]byte{pps},
		VPS:                              [][]byte{vps},
		LengthSizeMinusOne:               3,
	}
	buf := make([]byte, recordinfo.Len())
	recordinfo.Marshal(buf, s.SPSInfo)
	s.RecordInfo = recordinfo
	s.Record = buf

	return s, nil
}

func (s CodecData) Type() av.CodecType {
	return av.H265
}

// HEVCDecoderConfigurationRecordBytes returns the raw hvcC box bytes.
func (s CodecData) HEVCDecoderConfigurationRecordBytes() []byte {
	return s.Record
}

// SPS returns the first SPS NALU from the configuration record.
func (s CodecData) SPS() []byte {
	if len(s.RecordInfo.SPS) > 0 {
		return s.RecordInfo.SPS[0]
	}

	return nil
}

// PPS returns the first PPS NALU from the configuration record.
func (s CodecData) PPS() []byte {
	if len(s.RecordInfo.PPS) > 0 {
		return s.RecordInfo.PPS[0]
	}

	return nil
}

// VPS returns the first VPS NALU from the configuration record.
func (s CodecData) VPS() []byte {
	if len(s.RecordInfo.VPS) > 0 {
		return s.RecordInfo.VPS[0]
	}

	return nil
}

func (s CodecData) Width() int {
	return int(s.SPSInfo.Width)
}

func (s CodecData) Height() int {
	return int(s.SPSInfo.Height)
}

// TimeScale returns 90000 Hz per RFC 7798 / CMAF recommendation.
func (s CodecData) TimeScale() uint32 {
	return 90000
}

func (s CodecData) FPS() int {
	return int(s.SPSInfo.fps)
}

func (s CodecData) TrackID() string {
	return s.ControlURL
}

func (s CodecData) Resolution() string {
	return fmt.Sprintf("%vx%v", s.Width(), s.Height())
}

// Tag returns an RFC 6381 codec string (e.g. "hvc1.1.60000000.L120.90").
func (s CodecData) Tag() string {
	tier := "L"
	if s.SPSInfo.generalTierFlag == 1 {
		tier = "H"
	}

	return fmt.Sprintf(
		"hvc1.%d.%X.%s%d",
		s.SPSInfo.generalProfileIDC,
		s.SPSInfo.generalProfileCompatibilityFlags,
		tier,
		s.SPSInfo.generalLevelIDC,
	)
}

// Bandwidth returns an estimated bitrate string in bits/second.
func (s CodecData) Bandwidth() string {
	fps := s.FPS()
	if fps == 0 {
		fps = 25
	}

	bw := int(float64(s.Width()*s.Height()*fps) * 0.07)

	return strconv.Itoa(bw)
}

// PacketDuration returns the nominal frame duration.
func (s CodecData) PacketDuration(_ []byte) time.Duration {
	fps := s.FPS()
	if fps == 0 {
		fps = 25
	}

	return time.Second / time.Duration(fps)
}

// ParameterSetsAnnexB returns VPS+SPS+PPS concatenated with 4-byte start codes.
func (s CodecData) ParameterSetsAnnexB() []byte {
	var buf bytes.Buffer

	buf.Write([]byte{0, 0, 0, 1})
	buf.Write(s.VPS())
	buf.Write([]byte{0, 0, 0, 1})
	buf.Write(s.SPS())
	buf.Write([]byte{0, 0, 0, 1})
	buf.Write(s.PPS())

	return buf.Bytes()
}

// HEVCDecoderConfigurationRecord is the hvcC box payload (ISO 14496-15).
type HEVCDecoderConfigurationRecord struct {
	ConfigurationVersion uint8

	GeneralProfileSpace              uint8
	GeneralTierFlag                  uint8
	GeneralProfileIDC                uint8
	GeneralProfileCompatibilityFlags uint32
	GeneralConstraintIndicatorFlags  uint64
	GeneralLevelIDC                  uint8

	LengthSizeMinusOne uint8

	VPS [][]byte
	SPS [][]byte
	PPS [][]byte
}

// Unmarshal parses the hvcC record from b, returning the number of bytes consumed.
func (s *HEVCDecoderConfigurationRecord) Unmarshal(b []byte) (int, error) {
	if len(b) < 30 {
		return 0, ErrDecconfInvalid
	}

	s.GeneralProfileIDC = b[1]
	s.GeneralProfileCompatibilityFlags = uint32(b[2])
	s.GeneralLevelIDC = b[3]
	s.LengthSizeMinusOne = b[4] & 0x03

	n := 26
	vpscount := int(b[25] & 0x1f)

	for range vpscount {
		if len(b) < n+2 {
			return n, ErrDecconfInvalid
		}

		vpslen := int(pio.U16BE(b[n:]))
		n += 2

		if len(b) < n+vpslen {
			return n, ErrDecconfInvalid
		}

		s.VPS = append(s.VPS, b[n:n+vpslen])
		n += vpslen
	}

	// Each array section: array_completeness|reserved|nal_unit_type (1 byte)
	// + numNalus (2 bytes BE). We skip the type byte and high byte of count.
	if len(b) < n+3 {
		return n, ErrDecconfInvalid
	}

	n++ // skip nal_unit_type for SPS section
	n++ // skip high byte of numNalus
	spscount := int(b[n])
	n++

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

	if len(b) < n+3 {
		return n, ErrDecconfInvalid
	}

	n++ // skip nal_unit_type for PPS section
	n++ // skip high byte of numNalus
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
func (s *HEVCDecoderConfigurationRecord) Len() int {
	n := 23
	for _, sps := range s.SPS {
		n += 5 + len(sps)
	}

	for _, pps := range s.PPS {
		n += 5 + len(pps)
	}

	for _, vps := range s.VPS {
		n += 5 + len(vps)
	}

	return n
}

// Marshal serialises the record into b and returns the number of bytes written.
func (s *HEVCDecoderConfigurationRecord) Marshal(b []byte, _ SPSInfo) int {
	n := 0
	b[0] = 1
	b[1] = s.GeneralProfileIDC
	b[2] = byte(s.GeneralProfileCompatibilityFlags)
	b[3] = s.GeneralLevelIDC
	b[21] = 3
	b[22] = 3
	n += 23

	b[n] = (s.VPS[0][0] >> 1) & 0x3f
	n++
	b[n] = byte(len(s.VPS) >> 8)
	n++
	b[n] = byte(len(s.VPS))
	n++

	for _, vps := range s.VPS {
		pio.PutU16BE(b[n:], uint16(len(vps)))
		n += 2
		copy(b[n:], vps)
		n += len(vps)
	}

	b[n] = (s.SPS[0][0] >> 1) & 0x3f
	n++
	b[n] = byte(len(s.SPS) >> 8)
	n++
	b[n] = byte(len(s.SPS))
	n++

	for _, sps := range s.SPS {
		pio.PutU16BE(b[n:], uint16(len(sps)))
		n += 2
		copy(b[n:], sps)
		n += len(sps)
	}

	b[n] = (s.PPS[0][0] >> 1) & 0x3f
	n++
	b[n] = byte(len(s.PPS) >> 8)
	n++
	b[n] = byte(len(s.PPS))
	n++

	for _, pps := range s.PPS {
		pio.PutU16BE(b[n:], uint16(len(pps)))
		n += 2
		copy(b[n:], pps)
		n += len(pps)
	}

	return n
}
