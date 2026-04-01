package h265parser_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
)

// ---------------------------------------------------------------------------
// Known parameter-set bytes captured from a real 1920×1080 Main@L4.1 stream.
// Emulation-prevention bytes (0x000003) are present as in the bitstream.
// ---------------------------------------------------------------------------

// rawVPS is a VPS NAL (type 32, first byte = 0x40).
// Captured from a real 3840×2160 Main@L5.1 H.265 stream.
var rawVPS, _ = hex.DecodeString(
	"40010C01FFFF016000000300B00000030000030099958009",
)

// rawSPS is an SPS NAL (type 33, first byte = 0x42).
// Encodes 3840×2160 (UHD-1), Main profile, Level 5.1.
var rawSPS, _ = hex.DecodeString(
	"420101016000000300B00000030000030099A001E020021C4D8A1DCE4922B9AA4DE0C0",
)

// rawPPS is a PPS NAL (type 34, first byte = 0x44).
var rawPPS, _ = hex.DecodeString(
	"4401C0F3C1E00",
)

// ---------------------------------------------------------------------------
// NALU type predicates
// ---------------------------------------------------------------------------

func TestNALUType(t *testing.T) {
	tests := []struct {
		name    string
		nalu    []byte
		wantTyp uint8
	}{
		// first byte = (type << 1)
		{"VPS", []byte{0x40, 0x01}, 32},
		{"SPS", []byte{0x42, 0x01}, 33},
		{"PPS", []byte{0x44, 0x01}, 34},
		{"IDR_W_RADL", []byte{0x26, 0x01}, 19},
		{"IDR_N_LP", []byte{0x28, 0x01}, 20},
		{"TRAIL_R", []byte{0x02, 0x01}, 1},
		{"CRA_NUT", []byte{0x2A, 0x01}, 21},
		{"AUD", []byte{0x46, 0x01}, 35},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uint8(h265parser.NALUType(tt.nalu))
			if got != tt.wantTyp {
				t.Errorf("NALUType() = %d, want %d", got, tt.wantTyp)
			}
		})
	}
}

func TestIsPredicates(t *testing.T) {
	vps := []byte{0x40, 0x01, 0x0C}
	sps := []byte{0x42, 0x01, 0x01}
	pps := []byte{0x44, 0x01, 0xC0}
	idr := []byte{0x28, 0x01, 0x00} // IDR_N_LP
	cra := []byte{0x2A, 0x01, 0x00} // CRA_NUT
	trail := []byte{0x02, 0x01, 0x00}

	if !h265parser.IsVPSNALU(vps) {
		t.Error("expected VPS to be VPS NALU")
	}

	if !h265parser.IsSPSNALU(sps) {
		t.Error("expected SPS to be SPS NALU")
	}

	if !h265parser.IsPPSNALU(pps) {
		t.Error("expected PPS to be PPS NALU")
	}

	if !h265parser.IsParamSetNALU(vps) {
		t.Error("VPS should be param set")
	}

	if !h265parser.IsParamSetNALU(sps) {
		t.Error("SPS should be param set")
	}

	if !h265parser.IsParamSetNALU(pps) {
		t.Error("PPS should be param set")
	}

	if !h265parser.IsIRAP(idr) {
		t.Error("IDR_N_LP should be IRAP")
	}

	if !h265parser.IsIRAP(cra) {
		t.Error("CRA_NUT should be IRAP")
	}

	if !h265parser.IsKeyFrame(idr) {
		t.Error("IDR should be key frame")
	}

	if h265parser.IsIRAP(trail) {
		t.Error("TRAIL_R should not be IRAP")
	}

	if h265parser.IsDataNALU(sps) {
		t.Error("SPS should not be data NALU")
	}

	if !h265parser.IsDataNALU(idr) {
		t.Error("IDR_N_LP should be data NALU")
	}
}

// ---------------------------------------------------------------------------
// AnnexB / AVCC detection and conversion
// ---------------------------------------------------------------------------

func TestCheckNALUsTypeAnnexB(t *testing.T) {
	// 4-byte start code + VPS header
	annexb := append([]byte{0x00, 0x00, 0x00, 0x01}, rawVPS...)
	got := h265parser.CheckNALUsType(annexb)

	if got != parser.NALUAnnexb {
		t.Errorf("CheckNALUsType() = %v, want NALUAnnexb", got)
	}
}

func TestCheckNALUsTypeAVCC(t *testing.T) {
	// AVCC-frame a small NALU
	nalu := []byte{0x42, 0x01, 0xAB, 0xCD}
	avcc := h265parser.AnnexBToAVCC([][]byte{nalu})
	got := h265parser.CheckNALUsType(avcc)

	if got != parser.NALUAvcc {
		t.Errorf("CheckNALUsType() = %v, want NALUAvcc", got)
	}
}

func TestAnnexBToAVCCRoundtrip(t *testing.T) {
	original := [][]byte{
		{0x40, 0x01, 0xAA, 0xBB}, // fake VPS
		{0x42, 0x01, 0xCC, 0xDD}, // fake SPS
		{0x44, 0x01, 0xEE},       // fake PPS
	}

	avcc := h265parser.AnnexBToAVCC(original)
	back := h265parser.AVCCToAnnexB(avcc)

	if len(back) != len(original) {
		t.Fatalf("roundtrip: got %d NALUs, want %d", len(back), len(original))
	}

	for i, n := range original {
		if !bytes.Equal(n, back[i]) {
			t.Errorf("NALU[%d] mismatch: got %x, want %x", i, back[i], n)
		}
	}
}

// ---------------------------------------------------------------------------
// SPS parsing
// ---------------------------------------------------------------------------

func TestParseSPS_Width(t *testing.T) {
	info, err := h265parser.ParseSPS(rawSPS)
	if err != nil {
		t.Fatalf("ParseSPS() error: %v", err)
	}

	if info.Width == 0 || info.Height == 0 {
		t.Errorf("ParseSPS() Width=%d Height=%d, both must be > 0", info.Width, info.Height)
	}

	// 1920×1080 encoded as 1920×1088 for CTU alignment; both are valid.
	if info.Width != 3840 {
		t.Errorf("ParseSPS() Width = %d, want 3840", info.Width)
	}

	if info.Height != 2160 {
		t.Errorf("ParseSPS() Height = %d, want 2160", info.Height)
	}
}

func TestParseSPS_TooShort(t *testing.T) {
	_, err := h265parser.ParseSPS([]byte{0x42})
	if err == nil {
		t.Error("expected error for too-short SPS")
	}
}

// ---------------------------------------------------------------------------
// CodecData / HEVCDecoderConfigurationRecord
// ---------------------------------------------------------------------------

func TestNewCodecDataFromVPSAndSPSAndPPS(t *testing.T) {
	cd, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(rawVPS, rawSPS, rawPPS)
	if err != nil {
		t.Fatalf("NewCodecDataFromVPSAndSPSAndPPS() error: %v", err)
	}

	if cd.Width() == 0 || cd.Height() == 0 {
		t.Errorf("Width=%d Height=%d, both must be > 0", cd.Width(), cd.Height())
	}

	if cd.TimeScale() != 90000 {
		t.Errorf("TimeScale() = %d, want 90000", cd.TimeScale())
	}

	res := cd.Resolution()
	if len(res) == 0 {
		t.Error("Resolution() must not be empty")
	}

	t.Logf("resolution=%s tag=%s fps=%d bw=%s", res, cd.Tag(), cd.FPS(), cd.Bandwidth())
}

func TestHEVCDecoderConfigurationRecord_Roundtrip(t *testing.T) {
	cd, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(rawVPS, rawSPS, rawPPS)
	if err != nil {
		t.Fatalf("NewCodecDataFromVPSAndSPSAndPPS() error: %v", err)
	}

	record := cd.HEVCDecoderConfigurationRecordBytes()
	if len(record) < 23 {
		t.Fatalf("record too short: %d bytes", len(record))
	}

	if record[0] != 1 {
		t.Fatalf("configurationVersion = %d, want 1", record[0])
	}

	if record[13]&0xF0 != 0xF0 {
		t.Fatalf("reserved min_spatial_segmentation_idc bits not set: 0x%02x", record[13])
	}

	if record[15]&0xFC != 0xFC {
		t.Fatalf("reserved parallelismType bits not set: 0x%02x", record[15])
	}

	if record[16]&0xFC != 0xFC {
		t.Fatalf("reserved chromaFormat bits not set: 0x%02x", record[16])
	}

	if record[17]&0xF8 != 0xF8 {
		t.Fatalf("reserved bitDepthLumaMinus8 bits not set: 0x%02x", record[17])
	}

	if record[18]&0xF8 != 0xF8 {
		t.Fatalf("reserved bitDepthChromaMinus8 bits not set: 0x%02x", record[18])
	}

	if got := record[21] & 0x03; got != 3 {
		t.Fatalf("lengthSizeMinusOne = %d, want 3", got)
	}

	if record[22] != 3 {
		t.Fatalf("numOfArrays = %d, want 3", record[22])
	}

	offset := 23
	for _, wantType := range []byte{32, 33, 34} {
		if len(record) < offset+5 {
			t.Fatalf("record truncated before array type %d", wantType)
		}

		if got := record[offset] & 0x3f; got != wantType {
			t.Fatalf("array type = %d at offset %d, want %d", got, offset, wantType)
		}

		if got := binary.BigEndian.Uint16(record[offset+1 : offset+3]); got != 1 {
			t.Fatalf("array type %d numNalus = %d, want 1", wantType, got)
		}

		naluLen := int(binary.BigEndian.Uint16(record[offset+3 : offset+5]))
		offset += 5 + naluLen
	}

	cd2, err := h265parser.NewCodecDataFromAVCDecoderConfRecord(record)
	if err != nil {
		t.Fatalf("NewCodecDataFromAVCDecoderConfRecord() error: %v", err)
	}

	if cd.Width() != cd2.Width() || cd.Height() != cd2.Height() {
		t.Errorf("roundtrip mismatch: %dx%d vs %dx%d",
			cd.Width(), cd.Height(), cd2.Width(), cd2.Height())
	}

	if !bytes.Equal(cd.VPS(), cd2.VPS()) {
		t.Errorf("VPS mismatch after round-trip")
	}

	if !bytes.Equal(cd.SPS(), cd2.SPS()) {
		t.Errorf("SPS mismatch after round-trip")
	}

	if !bytes.Equal(cd.PPS(), cd2.PPS()) {
		t.Errorf("PPS mismatch after round-trip")
	}

	if cd.RecordInfo.GeneralProfileCompatibilityFlags != cd2.RecordInfo.GeneralProfileCompatibilityFlags {
		t.Errorf("compatibility flags mismatch: %#x vs %#x",
			cd.RecordInfo.GeneralProfileCompatibilityFlags,
			cd2.RecordInfo.GeneralProfileCompatibilityFlags)
	}

	if cd.RecordInfo.GeneralConstraintIndicatorFlags != cd2.RecordInfo.GeneralConstraintIndicatorFlags {
		t.Errorf("constraint flags mismatch: %#x vs %#x",
			cd.RecordInfo.GeneralConstraintIndicatorFlags,
			cd2.RecordInfo.GeneralConstraintIndicatorFlags)
	}
}

func TestParameterSetsAnnexB(t *testing.T) {
	cd, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(rawVPS, rawSPS, rawPPS)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	annexb := cd.ParameterSetsAnnexB()
	// Must start with 0x00000001 start code
	if !bytes.HasPrefix(annexb, []byte{0, 0, 0, 1}) {
		t.Error("ParameterSetsAnnexB() must start with 4-byte start code")
	}
}

// ---------------------------------------------------------------------------
// Synthetic AnnexB stream — saved to testdata/annexb.bin for future use
// ---------------------------------------------------------------------------

func TestSyntheticAnnexBStream(t *testing.T) {
	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	var buf bytes.Buffer
	buf.Write(startCode)
	buf.Write(rawVPS)
	buf.Write(startCode)
	buf.Write(rawSPS)
	buf.Write(startCode)
	buf.Write(rawPPS)

	stream := buf.Bytes()

	// Verify format detection
	if h265parser.CheckNALUsType(stream) != parser.NALUAnnexb {
		t.Error("synthetic stream should be detected as AnnexB")
	}

	// Save to testdata for reuse
	saveTestdata(t, "annexb.bin", stream)

	// Also save the AVCC version
	nalus := [][]byte{rawVPS, rawSPS, rawPPS}
	avccStream := h265parser.AnnexBToAVCC(nalus)
	saveTestdata(t, "avcc.bin", avccStream)

	// Verify AVCC version round-trips
	back := h265parser.AVCCToAnnexB(avccStream)
	if len(back) != 3 {
		t.Errorf("AVCC round-trip: got %d NALUs, want 3", len(back))
	}

	for i, n := range nalus {
		if !bytes.Equal(n, back[i]) {
			t.Errorf("NALU[%d] roundtrip mismatch", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Download real H.265 Annex-B stream from the internet and cache to testdata/
// ---------------------------------------------------------------------------

// candidateURLs are tried in order until one succeeds.
// All are publicly accessible raw H.265 Annex-B elementary streams.
var candidateURLs = []string{
	// ~49 KB raw Annex-B stream from the libde265 test suite (strukturag/libde265 on GitHub).
	"https://raw.githubusercontent.com/strukturag/libde265/master/testdata/girlshy.h265",
}

func TestDownloadRealHEVCStream(t *testing.T) {
	const cacheFile = "real_hevc.265"
	cached := filepath.Join("testdata", cacheFile)

	data, err := os.ReadFile(cached)
	if err != nil {
		// Not cached — try each candidate URL
		for _, u := range candidateURLs {
			data = downloadFile(t, u)
			if data != nil {
				break
			}
		}

		if data == nil {
			t.Skip("skipping: could not download real HEVC stream and no cached copy found")
		}

		saveTestdata(t, cacheFile, data)
	}

	if len(data) < 4 {
		t.Fatal("downloaded data too short")
	}

	// The first few bytes tell us whether it is raw Annex-B or container-wrapped.
	typ := h265parser.CheckNALUsType(data[:min(len(data), 512)])
	t.Logf("real HEVC stream: %d bytes, format=%v", len(data), typ)
}

// TestParseRealHEVCStream parses NALUs from the cached real stream and
// verifies that at least VPS, SPS, and PPS are present and that
// CodecData can be built from them.
func TestParseRealHEVCStream(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "real_hevc.265"))
	if err != nil {
		t.Skip("skipping: testdata/real_hevc.265 not available (run TestDownloadRealHEVCStream first)")
	}

	nalus, typ := parser.SplitNALUs(data)
	if typ != parser.NALUAnnexb {
		t.Fatalf("expected Annex-B stream, got %v", typ)
	}

	t.Logf("total NALUs in stream: %d", len(nalus))

	var vps, sps, pps []byte

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		switch {
		case h265parser.IsVPSNALU(nalu) && vps == nil:
			vps = nalu
			t.Logf("VPS found: %d bytes, first 8: %x", len(vps), vps[:min(8, len(vps))])
		case h265parser.IsSPSNALU(nalu) && sps == nil:
			sps = nalu
			t.Logf("SPS found: %d bytes, first 8: %x", len(sps), sps[:min(8, len(sps))])
		case h265parser.IsPPSNALU(nalu) && pps == nil:
			pps = nalu
			t.Logf("PPS found: %d bytes, first 8: %x", len(pps), pps[:min(8, len(pps))])
		}
	}

	if vps == nil {
		t.Error("no VPS found in stream")
	}

	if sps == nil {
		t.Error("no SPS found in stream")
	}

	if pps == nil {
		t.Error("no PPS found in stream")
	}

	if vps == nil || sps == nil || pps == nil {
		return
	}

	// Parse SPS directly
	info, err := h265parser.ParseSPS(sps)
	if err != nil {
		t.Fatalf("ParseSPS on real stream: %v", err)
	}

	t.Logf("parsed SPS: Width=%d Height=%d profile=%d level=%d",
		info.Width, info.Height, info.ProfileIdc, info.LevelIdc)

	if info.Width == 0 || info.Height == 0 {
		t.Error("ParseSPS returned zero width or height")
	}

	// Build CodecData
	cd, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(vps, sps, pps)
	if err != nil {
		t.Fatalf("NewCodecDataFromVPSAndSPSAndPPS on real stream: %v", err)
	}

	t.Logf("codec: %s fps=%d bw=%s tag=%s",
		cd.Resolution(), cd.FPS(), cd.Bandwidth(), cd.Tag())

	// Verify hvcC record round-trip
	record := cd.HEVCDecoderConfigurationRecordBytes()

	cd2, err := h265parser.NewCodecDataFromAVCDecoderConfRecord(record)
	if err != nil {
		t.Fatalf("hvcC round-trip: %v", err)
	}

	if cd.Width() != cd2.Width() || cd.Height() != cd2.Height() {
		t.Errorf("hvcC round-trip dimension mismatch: %dx%d vs %dx%d",
			cd.Width(), cd.Height(), cd2.Width(), cd2.Height())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func saveTestdata(t *testing.T, name string, data []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Errorf("failed to save testdata/%s: %v", name, err)
	} else {
		t.Logf("saved testdata/%s (%d bytes)", name, len(data))
	}
}

func downloadFile(t *testing.T, url string) []byte {
	t.Helper()

	//nolint:gosec // test helper, URL is a constant
	resp, err := http.Get(url)
	if err != nil {
		t.Logf("download failed: %v", err)

		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Logf("download HTTP %d", resp.StatusCode)

		return nil
	}

	const maxBytes = 10 * 1024 * 1024 // 10 MB cap
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		t.Logf("read failed: %v", err)

		return nil
	}

	return data
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}
