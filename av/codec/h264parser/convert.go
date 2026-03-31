package h264parser

import (
	"bytes"

	"github.com/vtpl1/vrtc-sdk/av/utils/bits/pio"
)

// AnnexBToAVCC converts a slice of raw NALUs into a single AVCC-framed byte slice
// (4-byte big-endian length prefix per NALU).
func AnnexBToAVCC(nalus [][]byte) []byte {
	var buf bytes.Buffer

	for _, nalu := range nalus {
		var sz [4]byte
		pio.PutU32BE(sz[:], uint32(len(nalu)))
		buf.Write(sz[:])
		buf.Write(nalu)
	}

	return buf.Bytes()
}

// AVCCToAnnexB splits an AVCC-framed byte slice into individual raw NALUs.
func AVCCToAnnexB(avcc []byte) [][]byte {
	var nalus [][]byte

	for i := 0; i+4 <= len(avcc); {
		size := int(pio.U32BE(avcc[i:]))
		i += 4

		if i+size > len(avcc) {
			break
		}

		nalus = append(nalus, avcc[i:i+size])
		i += size
	}

	return nalus
}

// ParameterSetsAnnexB returns SPS+PPS concatenated with 4-byte start codes,
// suitable for prepending to an Annex-B elementary stream.
func ParameterSetsAnnexB(sps, pps []byte) []byte {
	var buf bytes.Buffer

	buf.Write([]byte{0, 0, 0, 1})
	buf.Write(sps)
	buf.Write([]byte{0, 0, 0, 1})
	buf.Write(pps)

	return buf.Bytes()
}
