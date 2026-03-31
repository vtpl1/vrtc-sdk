package h264parser

import (
	"bytes"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
	"github.com/vtpl1/vrtc-sdk/av/utils/bits"
)

const bitsInByte = 8

var (
	StartCodeBytes = []byte{0, 0, 1}                           //nolint:gochecknoglobals
	AUDBytes       = []byte{0, 0, 0, 1, 0x9, 0xf0, 0, 0, 0, 1} //nolint:gochecknoglobals // AUD
)

// IsKeyFrame reports whether the NAL unit is an IDR slice (keyframe).
func IsKeyFrame(nalHeader []byte) bool {
	return av.H264NaluType(nalHeader[0])&av.H264NALTypeMask == av.H264_NAL_IDR_SLICE
}

// IsDataNALU reports whether the NAL unit carries coded video data.
func IsDataNALU(nalHeader []byte) bool {
	typ := av.H264NaluType(nalHeader[0]) & av.H264NALTypeMask

	return typ >= av.H264_NAL_SLICE && typ <= av.H264_NAL_IDR_SLICE
}

// IsSPSNALU reports whether the NAL unit is a Sequence Parameter Set.
func IsSPSNALU(nalHeader []byte) bool {
	return av.H264NaluType(nalHeader[0])&av.H264NALTypeMask == av.H264_NAL_SPS
}

// IsPPSNALU reports whether the NAL unit is a Picture Parameter Set.
func IsPPSNALU(nalHeader []byte) bool {
	return av.H264NaluType(nalHeader[0])&av.H264NALTypeMask == av.H264_NAL_PPS
}

// IsParamSetNALU reports whether the NAL unit is SPS or PPS.
func IsParamSetNALU(nalHeader []byte) bool {
	return IsSPSNALU(nalHeader) || IsPPSNALU(nalHeader)
}

// CheckNALUsType detects whether b contains AnnexB or AVCC formatted NALUs.
func CheckNALUsType(b []byte) parser.NALUAvccOrAnnexb {
	_, typ := parser.SplitNALUs(b)

	return typ
}

// SliceType represents the H.264 slice type.
type SliceType uint

// String returns a human-readable slice type name.
func (s SliceType) String() string {
	switch s {
	case SliceP:
		return "P"
	case SliceB:
		return "B"
	case SliceI:
		return "I"
	}

	return ""
}

const (
	SliceP SliceType = iota + 1
	SliceB
	SliceI
)

// ParseSliceHeaderFromNALU extracts the slice type from a VCL NALU.
func ParseSliceHeaderFromNALU(pkt []byte) (SliceType, error) {
	if len(pkt) <= 1 {
		return 0, ErrPacketTooShort
	}

	nalUnitType := pkt[0] & 0x1f
	switch nalUnitType {
	case 1, 2, 5, 19:
		// slice_layer_without_partitioning_rbsp / slice_data_partition_a_layer_rbsp
	default:
		return 0, ErrNalHasNoSliceHeader
	}

	r := &bits.GolombBitReader{R: bytes.NewReader(pkt[1:])}

	if _, err := r.ReadExponentialGolombCode(); err != nil { // first_mb_in_slice
		return 0, err
	}

	u, err := r.ReadExponentialGolombCode() // slice_type
	if err != nil {
		return 0, err
	}

	switch u {
	case 0, 3, 5, 8:
		return SliceP, nil
	case 1, 6:
		return SliceB, nil
	case 2, 4, 7, 9:
		return SliceI, nil
	default:
		return 0, ErrInvalidSliceType
	}
}
