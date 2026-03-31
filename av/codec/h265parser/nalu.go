package h265parser

import (
	"bytes"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
	"github.com/vtpl1/vrtc-sdk/av/utils/bits"
)

// AUDBytes is a pre-built H.265 Access Unit Delimiter NAL in AnnexB format.
var AUDBytes = []byte{0, 0, 0, 1, 0x46, 0x01} //nolint:gochecknoglobals

// NALUType returns the 6-bit H.265 NAL unit type from the first byte of a NALU.
func NALUType(nalu []byte) av.H265NaluType {
	return av.H265NaluType(nalu[0]>>1) & av.H265NALTypeMask
}

// IsKeyFrame reports whether the NALU is an IRAP (keyframe) picture.
func IsKeyFrame(nalu []byte) bool {
	return IsIRAP(nalu)
}

// IsIRAP reports whether the NALU is an Intra Random Access Point picture.
func IsIRAP(nalu []byte) bool {
	switch NALUType(nalu) { //nolint:exhaustive
	case
		av.HEVC_NAL_BLA_W_LP,
		av.HEVC_NAL_BLA_W_RADL,
		av.HEVC_NAL_BLA_N_LP,
		av.HEVC_NAL_IDR_W_RADL,
		av.HEVC_NAL_IDR_N_LP,
		av.HEVC_NAL_CRA_NUT:
		return true
	}

	return false
}

// IsFirstSlice reports whether the NALU is the first slice of a picture.
func IsFirstSlice(nalu []byte) bool {
	if len(nalu) < 3 {
		return false
	}

	if NALUType(nalu) > 31 {
		return false
	}

	br := &bits.GolombBitReader{R: bytes.NewReader(nalu[2:])}

	firstSliceFlag, err := br.ReadBit()
	if err != nil {
		return false
	}

	return firstSliceFlag == 1
}

// IsDataNALU reports whether the NALU carries coded video data.
func IsDataNALU(nalu []byte) bool {
	typ := NALUType(nalu)

	return typ >= av.HEVC_NAL_TRAIL_R && typ <= av.HEVC_NAL_IDR_N_LP
}

// IsSPSNALU reports whether the NALU is a Sequence Parameter Set.
func IsSPSNALU(nalu []byte) bool {
	return NALUType(nalu) == av.HEVC_NAL_SPS
}

// IsPPSNALU reports whether the NALU is a Picture Parameter Set.
func IsPPSNALU(nalu []byte) bool {
	return NALUType(nalu) == av.HEVC_NAL_PPS
}

// IsVPSNALU reports whether the NALU is a Video Parameter Set.
func IsVPSNALU(nalu []byte) bool {
	return NALUType(nalu) == av.HEVC_NAL_VPS
}

// IsParamSetNALU reports whether the NALU is VPS, SPS, or PPS.
func IsParamSetNALU(nalu []byte) bool {
	return IsVPSNALU(nalu) || IsSPSNALU(nalu) || IsPPSNALU(nalu)
}

// CheckNALUsType detects whether b contains AnnexB or AVCC formatted NALUs.
func CheckNALUsType(b []byte) parser.NALUAvccOrAnnexb {
	_, typ := parser.SplitNALUs(b)

	return typ
}

// SliceType represents the H.265 slice type.
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

	nalUnitType := (pkt[0] >> 1) & 0x3F
	if nalUnitType > 31 {
		return 0, ErrNalHasNoSliceHeader
	}

	r := &bits.GolombBitReader{R: bytes.NewReader(pkt[1:])}
	if _, err := r.ReadExponentialGolombCode(); err != nil {
		return 0, err
	}

	u, err := r.ReadExponentialGolombCode()
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
