package parser_test

import (
	"encoding/hex"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
)

func TestSplitNALUs(t *testing.T) {
	annexbFrame, _ := hex.DecodeString("00000001223322330000000122332233223300000133000001000001")
	annexbFrame2, _ := hex.DecodeString(
		"000000016742c028d900780227e584000003000400000300503c60c920",
	)
	avccFrame, _ := hex.DecodeString("00000008aabbccaabbccaabb00000001aa")

	tests := []struct {
		name      string
		b         []byte
		wantNalus [][]byte
		wantTyp   parser.NALUAvccOrAnnexb
		wantLen   int
	}{
		{
			name:    "annexbFrame",
			b:       annexbFrame,
			wantTyp: parser.NALUAnnexb,
			wantLen: 3,
		},
		{
			name:    "annexbFrame2",
			b:       annexbFrame2,
			wantTyp: parser.NALUAnnexb,
			wantLen: 1,
		},
		{
			name:    "avccFrame",
			b:       avccFrame,
			wantTyp: parser.NALUAvcc,
			wantLen: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNalus, gotTyp := parser.SplitNALUs(tt.b)
			if gotTyp != tt.wantTyp {
				t.Errorf("SplitNALUs() gotTyp = %v, want %v", gotTyp, tt.wantTyp)
			}

			if len(gotNalus) != tt.wantLen {
				t.Errorf("SplitNALUs() len(gotNalus) = %v, want %v", len(gotNalus), tt.wantLen)
			}
		})
	}
}

func TestIsAnnexBOrAVCC(t *testing.T) {
	if typ := parser.IsAnnexBOrAVCC(
		[]byte{0x00, 0x00, 0x00, 0x01, 0x65},
	); typ != parser.NALUAnnexb {
		t.Errorf("Expected AnnexB, got %v", typ)
	}

	// 3-byte Annex-B start code with no valid AVCC interpretation:
	// 00 00 01 65 → AVCC length would be 0x00000165 = 357, but data is only 4 bytes.
	if typ := parser.IsAnnexBOrAVCC([]byte{0x00, 0x00, 0x01, 0x65}); typ != parser.NALUAnnexb {
		t.Errorf("Expected AnnexB, got %v", typ)
	}

	if typ := parser.IsAnnexBOrAVCC(
		[]byte{0x00, 0x00, 0x00, 0x05, 0x65, 0x88, 0x99, 0xaa, 0xbb},
	); typ != parser.NALUAvcc {
		t.Errorf("Expected AVCC, got %v", typ)
	}
}

// TestIsAnnexBOrAVCC_Size256to510 is a regression test for the AVCC/Annex-B
// misclassification bug.
//
// When an AVCC NALU has a size in the range 256–510, its 4-byte big-endian
// length prefix is 00 00 01 XX. The first three bytes match a 3-byte Annex-B
// start code (00 00 01). The old code checked for Annex-B start codes before
// checking AVCC lengths, so these NALUs were misclassified as Annex-B.
//
// The Annex-B splitter then consumed only 3 bytes of the 4-byte prefix,
// leaving the fourth byte (XX) as a spurious first byte in the NALU payload.
// Re-encoding to AVCC produced a corrupt sample with:
//   - A junk byte = (nalu_len & 0xFF) − 1 prepended to the real NAL header
//   - forbidden_zero_bit = 1 (invalid HEVC)
//   - The real TRAIL_R header (02 01) shifted by one byte
//
// This affected ~0.7% of samples in production fMP4 recordings (only P-frames
// small enough to fall in the 256–510 byte range).
func TestIsAnnexBOrAVCC_Size256to510(t *testing.T) {
	// Simulate an AVCC NALU of size 300 (0x012C).
	// Length prefix: 00 00 01 2C — first 3 bytes look like Annex-B start code.
	naluSize := 300
	data := make([]byte, 4+naluSize)
	data[0] = 0x00
	data[1] = 0x00
	data[2] = 0x01
	data[3] = 0x2C
	// HEVC TRAIL_R header (type=1, temporal_id=0)
	data[4] = 0x02
	data[5] = 0x01
	// Fill rest with dummy slice data
	for i := 6; i < len(data); i++ {
		data[i] = 0xAB
	}

	typ := parser.IsAnnexBOrAVCC(data)
	if typ != parser.NALUAvcc {
		t.Errorf("AVCC NALU with size 300 (prefix 00 00 01 2C) misclassified as %v, want AVCC", typ)
	}

	// Verify SplitNALUs also returns AVCC and a single NALU.
	nalus, splitTyp := parser.SplitNALUs(data)
	if splitTyp != parser.NALUAvcc {
		t.Errorf("SplitNALUs() type = %v, want AVCC", splitTyp)
	}

	if len(nalus) != 1 {
		t.Fatalf("SplitNALUs() returned %d NALUs, want 1", len(nalus))
	}

	if len(nalus[0]) != naluSize {
		t.Errorf("SplitNALUs() NALU size = %d, want %d", len(nalus[0]), naluSize)
	}

	// The first byte of the extracted NALU must be the real header (0x02),
	// not the junk byte (0x2B) that the old code would have produced.
	if nalus[0][0] != 0x02 {
		t.Errorf("SplitNALUs() NALU[0] starts with 0x%02X, want 0x02 (TRAIL_R header)", nalus[0][0])
	}

	// Also test edge cases: size 256 (min) and 510 (max of affected range).
	for _, sz := range []int{256, 510} {
		d := make([]byte, 4+sz)
		d[0] = 0x00
		d[1] = 0x00
		d[2] = 0x01
		d[3] = byte(sz & 0xFF)
		d[4] = 0x02
		d[5] = 0x01

		if got := parser.IsAnnexBOrAVCC(d); got != parser.NALUAvcc {
			t.Errorf("AVCC NALU size %d: classified as %v, want AVCC", sz, got)
		}
	}
}

func TestAnnexBToAVCC(t *testing.T) {
	annexb := append([]byte{0x00, 0x00, 0x00, 0x01}, []byte{0x65, 0x88, 0x99}...)

	avcc, err := parser.AnnexBToAVCC(annexb)
	if err != nil {
		t.Fatalf("AnnexBToAVCC() returned error: %v", err)
	}

	expected := []byte{0x00, 0x00, 0x00, 0x03, 0x65, 0x88, 0x99}
	if !equalSlices(avcc, expected) {
		t.Errorf("AnnexBToAVCC() = %v, want %v", avcc, expected)
	}
}

func TestAVCCToAnnexB(t *testing.T) {
	avcc := []byte{0x00, 0x00, 0x00, 0x03, 0x65, 0x88, 0x99}

	annexb, err := parser.AVCCToAnnexB(avcc)
	if err != nil {
		t.Fatalf("AVCCToAnnexB() returned error: %v", err)
	}

	expected := append([]byte{0x00, 0x00, 0x00, 0x01}, avcc[4:]...)
	if !equalSlices(annexb, expected) {
		t.Errorf("AVCCToAnnexB() = %v, want %v", annexb, expected)
	}
}

func TestSplitNALUs_H264(t *testing.T) {
	raw := append([]byte{0x00, 0x00, 0x01, 0x65, 0x44}, []byte{0x00, 0x00, 0x01, 0x61, 0x33}...)
	nalus, format := parser.SplitNALUs(raw)

	if len(nalus) != 2 {
		t.Errorf("Expected 2 NALUs, got %d", len(nalus))
	}

	if format != parser.NALUAnnexb {
		t.Errorf("Expected format Annexb, got %v", format)
	}
}

func TestSplitNALUs_H265(t *testing.T) {
	raw := append([]byte{0x00, 0x00, 0x01, 0x40, 0x01}, []byte{0x00, 0x00, 0x01, 0x26, 0x99}...)
	nalus, format := parser.SplitNALUs(raw)

	if len(nalus) != 2 {
		t.Errorf("Expected 2 NALUs, got %d", len(nalus))
	}

	if format != parser.NALUAnnexb {
		t.Errorf("Expected format Annexb, got %v", format)
	}
}

func TestSplitNALUs_1(t *testing.T) {
	data := []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0x00, 0x1f, // SPS
		0x00, 0x00, 0x01, 0x68, 0xee, 0x3c, 0x80, // PPS
		0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00, // IDR frame
	}

	nalus, format := parser.SplitNALUs(data)

	if len(nalus) != 3 {
		t.Errorf("Expected 3 NALUs, got %d", len(nalus))
	}

	if format != parser.NALUAnnexb {
		t.Errorf("Expected format Annexb, got %v", format)
	}

	if len(nalus) >= 3 {
		if nalus[0][0] != 0x67 {
			t.Errorf("Expected first NALU to start with 0x67, got 0x%x", nalus[0][0])
		}

		if nalus[1][0] != 0x68 {
			t.Errorf("Expected second NALU to start with 0x68, got 0x%x", nalus[1][0])
		}

		if nalus[2][0] != 0x65 {
			t.Errorf("Expected third NALU to start with 0x65, got 0x%x", nalus[2][0])
		}
	}
}

func equalSlices(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
