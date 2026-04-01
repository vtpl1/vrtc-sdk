package h265parser

import (
	"bytes"
	"encoding/binary"
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
		ConfigurationVersion:             1,
		GeneralProfileSpace:              uint8(s.SPSInfo.generalProfileSpace),
		GeneralProfileIDC:                uint8(s.SPSInfo.generalProfileIDC),
		GeneralLevelIDC:                  uint8(s.SPSInfo.generalLevelIDC),
		GeneralTierFlag:                  uint8(s.SPSInfo.generalTierFlag),
		GeneralProfileCompatibilityFlags: s.SPSInfo.generalProfileCompatibilityFlags,
		GeneralConstraintIndicatorFlags:  s.SPSInfo.generalConstraintIndicatorFlags,
		ChromaFormat:                     uint8(s.SPSInfo.chromaFormat),
		BitDepthLumaMinus8:               uint8(s.SPSInfo.bitDepthLumaMinus8),
		BitDepthChromaMinus8:             uint8(s.SPSInfo.bitDepthChromaMinus8),
		NumTemporalLayers:                uint8(s.SPSInfo.numTemporalLayers),
		TemporalIDNested:                 uint8(s.SPSInfo.temporalIDNested),
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

	MinSpatialSegmentationIDC uint16
	ParallelismType           uint8
	ChromaFormat              uint8
	BitDepthLumaMinus8        uint8
	BitDepthChromaMinus8      uint8
	AvgFrameRate              uint16
	ConstantFrameRate         uint8
	NumTemporalLayers         uint8
	TemporalIDNested          uint8
	LengthSizeMinusOne        uint8

	VPS [][]byte
	SPS [][]byte
	PPS [][]byte
}

// Unmarshal parses the hvcC record from b, returning the number of bytes consumed.
func (s *HEVCDecoderConfigurationRecord) Unmarshal(b []byte) (int, error) {
	if len(b) < 23 {
		return 0, ErrDecconfInvalid
	}

	*s = HEVCDecoderConfigurationRecord{}
	s.ConfigurationVersion = b[0]
	s.GeneralProfileSpace = b[1] >> 6
	s.GeneralTierFlag = (b[1] >> 5) & 0x01
	s.GeneralProfileIDC = b[1] & 0x1f
	s.GeneralProfileCompatibilityFlags = binary.BigEndian.Uint32(b[2:6])
	s.GeneralConstraintIndicatorFlags = uint64(b[6])<<40 |
		uint64(b[7])<<32 |
		uint64(b[8])<<24 |
		uint64(b[9])<<16 |
		uint64(b[10])<<8 |
		uint64(b[11])
	s.GeneralLevelIDC = b[12]
	s.MinSpatialSegmentationIDC = uint16(b[13]&0x0f)<<8 | uint16(b[14])
	s.ParallelismType = b[15] & 0x03
	s.ChromaFormat = b[16] & 0x03
	s.BitDepthLumaMinus8 = b[17] & 0x07
	s.BitDepthChromaMinus8 = b[18] & 0x07
	s.AvgFrameRate = pio.U16BE(b[19:])
	s.ConstantFrameRate = b[21] >> 6
	s.NumTemporalLayers = (b[21] >> 3) & 0x07
	s.TemporalIDNested = (b[21] >> 2) & 0x01
	s.LengthSizeMinusOne = b[21] & 0x03

	n := 23
	numArrays := int(b[22])

	for range numArrays {
		if len(b) < n+3 {
			return n, ErrDecconfInvalid
		}

		nalType := b[n] & 0x3f
		n++

		numNALUs := int(pio.U16BE(b[n:]))
		n += 2

		for range numNALUs {
			if len(b) < n+2 {
				return n, ErrDecconfInvalid
			}

			naluLen := int(pio.U16BE(b[n:]))
			n += 2

			if len(b) < n+naluLen {
				return n, ErrDecconfInvalid
			}

			nalu := b[n : n+naluLen]
			n += naluLen

			switch nalType {
			case 32:
				s.VPS = append(s.VPS, nalu)
			case 33:
				s.SPS = append(s.SPS, nalu)
			case 34:
				s.PPS = append(s.PPS, nalu)
			}
		}
	}

	return n, nil
}

// Len returns the marshalled size of the record in bytes.
func (s *HEVCDecoderConfigurationRecord) Len() int {
	n := 23

	n += hevcArrayLen(s.VPS)
	n += hevcArrayLen(s.SPS)
	n += hevcArrayLen(s.PPS)

	return n
}

// Marshal serialises the record into b and returns the number of bytes written.
func (s *HEVCDecoderConfigurationRecord) Marshal(b []byte, info SPSInfo) int {
	cfgVersion := s.ConfigurationVersion
	if cfgVersion == 0 {
		cfgVersion = 1
	}

	profileSpace := s.GeneralProfileSpace & 0x03
	if profileSpace == 0 && info.generalProfileSpace != 0 {
		profileSpace = uint8(info.generalProfileSpace)
	}

	tierFlag := s.GeneralTierFlag & 0x01
	if tierFlag == 0 && info.generalTierFlag != 0 {
		tierFlag = uint8(info.generalTierFlag)
	}

	profileIDC := s.GeneralProfileIDC & 0x1f
	if profileIDC == 0 && info.generalProfileIDC != 0 {
		profileIDC = uint8(info.generalProfileIDC)
	}

	compatFlags := s.GeneralProfileCompatibilityFlags
	if compatFlags == 0 && info.generalProfileCompatibilityFlags != 0 {
		compatFlags = info.generalProfileCompatibilityFlags
	}

	constraintFlags := s.GeneralConstraintIndicatorFlags & 0x0000FFFFFFFFFFFF
	if constraintFlags == 0 && info.generalConstraintIndicatorFlags != 0 {
		constraintFlags = info.generalConstraintIndicatorFlags & 0x0000FFFFFFFFFFFF
	}

	levelIDC := s.GeneralLevelIDC
	if levelIDC == 0 && info.generalLevelIDC != 0 {
		levelIDC = uint8(info.generalLevelIDC)
	}

	chromaFormat := s.ChromaFormat & 0x03
	if chromaFormat == 0 && info.chromaFormat != 0 {
		chromaFormat = uint8(info.chromaFormat)
	}

	bitDepthLumaMinus8 := s.BitDepthLumaMinus8 & 0x07
	if bitDepthLumaMinus8 == 0 && info.bitDepthLumaMinus8 != 0 {
		bitDepthLumaMinus8 = uint8(info.bitDepthLumaMinus8)
	}

	bitDepthChromaMinus8 := s.BitDepthChromaMinus8 & 0x07
	if bitDepthChromaMinus8 == 0 && info.bitDepthChromaMinus8 != 0 {
		bitDepthChromaMinus8 = uint8(info.bitDepthChromaMinus8)
	}

	numTemporalLayers := s.NumTemporalLayers & 0x07
	if numTemporalLayers == 0 && info.numTemporalLayers != 0 {
		numTemporalLayers = uint8(info.numTemporalLayers)
	}

	temporalIDNested := s.TemporalIDNested & 0x01
	if temporalIDNested == 0 && info.temporalIDNested != 0 {
		temporalIDNested = uint8(info.temporalIDNested)
	}

	lengthSizeMinusOne := s.LengthSizeMinusOne & 0x03
	if lengthSizeMinusOne == 0 {
		lengthSizeMinusOne = 3
	}

	b[0] = cfgVersion
	b[1] = (profileSpace << 6) | (tierFlag << 5) | profileIDC
	binary.BigEndian.PutUint32(b[2:6], compatFlags)
	b[6] = byte(constraintFlags >> 40)
	b[7] = byte(constraintFlags >> 32)
	b[8] = byte(constraintFlags >> 24)
	b[9] = byte(constraintFlags >> 16)
	b[10] = byte(constraintFlags >> 8)
	b[11] = byte(constraintFlags)
	b[12] = levelIDC
	b[13] = 0xF0 | byte((s.MinSpatialSegmentationIDC>>8)&0x0f)
	b[14] = byte(s.MinSpatialSegmentationIDC)
	b[15] = 0xFC | (s.ParallelismType & 0x03)
	b[16] = 0xFC | chromaFormat
	b[17] = 0xF8 | bitDepthLumaMinus8
	b[18] = 0xF8 | bitDepthChromaMinus8
	pio.PutU16BE(b[19:], s.AvgFrameRate)
	b[21] = (s.ConstantFrameRate&0x03)<<6 |
		(numTemporalLayers&0x07)<<3 |
		(temporalIDNested&0x01)<<2 |
		lengthSizeMinusOne

	numArrays := 0
	if len(s.VPS) > 0 {
		numArrays++
	}
	if len(s.SPS) > 0 {
		numArrays++
	}
	if len(s.PPS) > 0 {
		numArrays++
	}

	b[22] = byte(numArrays)
	n := 23

	n = marshalHEVCArray(b, n, 32, s.VPS)
	n = marshalHEVCArray(b, n, 33, s.SPS)
	n = marshalHEVCArray(b, n, 34, s.PPS)

	return n
}

func hevcArrayLen(nalus [][]byte) int {
	if len(nalus) == 0 {
		return 0
	}

	n := 3
	for _, nalu := range nalus {
		n += 2 + len(nalu)
	}

	return n
}

func marshalHEVCArray(b []byte, n int, nalType uint8, nalus [][]byte) int {
	if len(nalus) == 0 {
		return n
	}

	b[n] = 0x80 | (nalType & 0x3f)
	n++
	pio.PutU16BE(b[n:], uint16(len(nalus)))
	n += 2

	for _, nalu := range nalus {
		pio.PutU16BE(b[n:], uint16(len(nalu)))
		n += 2
		copy(b[n:], nalu)
		n += len(nalu)
	}

	return n
}
