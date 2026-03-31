// Package parser provides utilities for detecting and converting between NALU
// bitstream formats: raw, Annex B (start-code-delimited, ISO 14496-10 §B.1),
// and AVCC / ISO BMFF (4-byte big-endian length-prefixed, ISO 14496-15 §5.3).
//
// The primary entry points are SplitNALUs, AnnexBToAVCC, and AVCCToAnnexB.
package parser

import (
	"encoding/binary"

	"github.com/vtpl1/vrtc-sdk/av/utils/bits/pio"
)

// Annex B start codes used to delimit NAL units in a byte stream.
var (
	StartCode3 = []byte{0x00, 0x00, 0x01} //nolint:gochecknoglobals // 3-byte start code
	StartCode4 = []byte{
		0x00,
		0x00,
		0x00,
		0x01,
	} //nolint:gochecknoglobals // 4-byte start code (preferred)
	StartCodes = [][]byte{StartCode3, StartCode4} //nolint:gochecknoglobals // all known start codes
)

// NALUAvccOrAnnexb identifies the framing format of a NALU bitstream.
type NALUAvccOrAnnexb int

const (
	NALURaw    NALUAvccOrAnnexb = iota // unrecognised or plain raw NALU bytes
	NALUAvcc                           // AVCC / ISO BMFF length-prefixed format
	NALUAnnexb                         // Annex B start-code-delimited format
)

// -----------------------------
// NALU Format Detection
// -----------------------------

// Optimized lenStartCode to use direct byte checks, avoiding bytes.HasPrefix and loop overhead.
func lenStartCode(data []byte) int {
	if len(data) >= 4 && data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x00 && data[3] == 0x01 {
		return 4
	}

	if len(data) >= 3 && data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x01 {
		return 3
	}

	return 0
}

func hasAnnexBStartCode(data []byte) bool {
	return lenStartCode(data) > 0
}

// IsAnnexBOrAVCC heuristically detects whether data is Annex B, AVCC, or raw.
// A minimum of 4 bytes is required; shorter slices return NALURaw.
func IsAnnexBOrAVCC(data []byte) NALUAvccOrAnnexb {
	if len(data) < 4 {
		return NALURaw
	}

	if hasAnnexBStartCode(data) {
		return NALUAnnexb
	}
	// Check if the first 4 bytes represent a valid NALU length for AVCC.
	// The length should be greater than 0 and not exceed the remaining data length.
	naluLen := readNALULength(data[:4])
	if naluLen > 0 && naluLen <= len(data)-4 {
		return NALUAvcc
	}

	return NALURaw
}

func readNALULength(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	// Using binary.BigEndian.Uint32 is generally idiomatic and might be micro-optimized by the Go runtime.
	return int(binary.BigEndian.Uint32(b[:4]))
}

// SplitNALUs optimizes AnnexB parsing by performing direct byte checks for start codes
// within the main loop to avoid repeated slicing and function call overhead.
//
//nolint:nestif
func SplitNALUs(b []byte) ([][]byte, NALUAvccOrAnnexb) {
	annexBOrAvccOrRaw := IsAnnexBOrAVCC(b)
	if annexBOrAvccOrRaw == NALUAnnexb {
		var nalus [][]byte

		// Optimized loop to find all start code positions by direct byte checking
		naluIndices := []int{}

		i := 0
		for i < len(b) {
			scLen := 0
			// Directly check for 4-byte start code first (most common and longest)
			if i+4 <= len(b) && b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x00 && b[i+3] == 0x01 {
				scLen = 4
			} else if i+3 <= len(b) && b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x01 {
				// Directly check for 3-byte start code
				scLen = 3
			}

			if scLen > 0 {
				naluIndices = append(naluIndices, i)
				i += scLen
			} else {
				i++
			}
		}

		// If no start codes found, fall back to single raw NALU
		if len(naluIndices) == 0 {
			return [][]byte{b}, NALURaw
		}

		// Extract NALUs
		for i := range naluIndices {
			start := naluIndices[i]

			end := len(b)
			if next := i + 1; next < len(naluIndices) {
				end = naluIndices[next]
			}

			nalu := b[start:end]

			// Determine offset using the now-optimized lenStartCode
			offset := lenStartCode(nalu)

			if offset >= len(nalu) {
				continue // corrupted NALU or just a start code (e.g., 00 00 01 at end of stream)
			}

			naluNoPrefix := nalu[offset:]
			if len(naluNoPrefix) > 0 {
				nalus = append(nalus, naluNoPrefix)
			}
		}

		return nalus, NALUAnnexb
	} else if annexBOrAvccOrRaw == NALUAvcc {
		_val4 := pio.U32BE(b)
		_b := b[4:]
		nalus := [][]byte{}

		// The AVCC parsing loop is already quite efficient with direct slicing and integer operations.
		for _val4 <= uint32(len(_b)) {
			nalus = append(nalus, _b[:_val4])

			_b = _b[_val4:]
			if len(_b) < 4 {
				break
			}

			_val4 = pio.U32BE(_b)
			_b = _b[4:]

			if _val4 > uint32(len(_b)) {
				break
			}
		}

		if len(_b) == 0 { // Check if all data was consumed
			return nalus, NALUAvcc
		}
	}

	return [][]byte{b}, NALURaw
}

// FindNextAnnexBNALUnit locates the next NAL unit in an Annex B byte stream
// beginning at byte offset start. It returns the byte range [nalStart, nalEnd)
// of the NALU payload (after the start code). Both values are -1 if no start
// code is found.
//
//nolint:nonamedreturns
func FindNextAnnexBNALUnit(data []byte, start int) (nalStart, nalEnd int) {
	nalStart = -1

	// Find start code
	for i := start; i+3 < len(data); i++ {
		// Benefits from the optimized hasAnnexBStartCode (which uses optimized lenStartCode)
		if hasAnnexBStartCode(data[i:]) {
			nalStart = i + lenStartCode(data[i:])

			break
		}
	}

	if nalStart == -1 {
		return -1, -1
	}

	// Find next start code
	for i := nalStart; i+3 < len(data); i++ {
		// Benefits from the optimized hasAnnexBStartCode (which uses optimized lenStartCode)
		if hasAnnexBStartCode(data[i:]) {
			nalEnd = i

			return nalStart, nalEnd
		}
	}

	nalEnd = len(data)

	return nalStart, nalEnd
}

// AnnexBToAVCC converts an Annex B byte stream to AVCC (4-byte length-prefixed)
// format. Each start-code-delimited NALU is replaced by its big-endian uint32
// length followed by the NALU payload.
func AnnexBToAVCC(data []byte) ([]byte, error) {
	var output []byte

	offset := 0

	for offset < len(data) {
		start, end := FindNextAnnexBNALUnit(data, offset)
		if start < 0 || end < 0 {
			break
		}

		nalu := data[start:end]
		naluLen := uint32(len(nalu))

		var lengthBuf [4]byte
		binary.BigEndian.PutUint32(lengthBuf[:], naluLen)
		output = append(output, lengthBuf[:]...)
		output = append(output, nalu...)

		offset = end
	}

	return output, nil
}

// AVCCToAnnexB converts AVCC (4-byte length-prefixed) data to Annex B format,
// prepending each NALU with a 4-byte start code (0x00 0x00 0x00 0x01).
func AVCCToAnnexB(data []byte) ([]byte, error) {
	var output []byte

	offset := 0

	for offset+4 <= len(data) {
		naluLen := readNALULength(data[offset : offset+4])
		offset += 4

		if offset+naluLen > len(data) {
			return nil, errInvalidNALULength
		}

		output = append(output, StartCode4...) // 4-byte start code
		output = append(output, data[offset:offset+naluLen]...)
		offset += naluLen
	}

	return output, nil
}
