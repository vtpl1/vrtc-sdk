package h264parser_test

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
)

// ---------------------------------------------------------------------------
// SPS NAL units captured from real H.264 streams.
// ---------------------------------------------------------------------------

var (
	sps1nalu, _ = hex.DecodeString(
		"67640020accac05005bb0169e0000003002000000c9c4c000432380008647c12401cb1c31380",
	)
	sps2nalu, _ = hex.DecodeString("6764000dacd941419f9e10000003001000000303c0f1429960")
	sps3nalu, _ = hex.DecodeString(
		"27640020ac2ec05005bb011000000300100000078e840016e300005b8d8bdef83b438627",
	)
	sps4nalu, _ = hex.DecodeString(
		"674d00329a64015005fff8037010101400000fa000013883a1800fee0003fb52ef2e343001fdc0007f6a5de5c280",
	)

	// ppsNALU is a minimal PPS NAL paired with sps1nalu.
	ppsNALU, _ = hex.DecodeString("68ce3880")
)

// ---------------------------------------------------------------------------
// ParseSPS — existing table-driven tests (kept verbatim)
// ---------------------------------------------------------------------------

func TestParseSPS(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		want    h264parser.SPSInfo
		wantErr bool
	}{
		{
			name: "sps1nalu 1280x720@50fps",
			data: sps1nalu,
			want: h264parser.SPSInfo{
				ID:                0,
				ProfileIdc:        100,
				LevelIdc:          32,
				ConstraintSetFlag: 0,
				MbWidth:           80,
				MbHeight:          45,
				CropLeft:          0,
				CropRight:         0,
				CropTop:           0,
				CropBottom:        0,
				Width:             1280,
				Height:            720,
				FPS:               50,
			},
		},
		{
			name: "sps2nalu 320x180@30fps",
			data: sps2nalu,
			want: h264parser.SPSInfo{
				ID:                0,
				ProfileIdc:        100,
				LevelIdc:          13,
				ConstraintSetFlag: 0,
				MbWidth:           20,
				MbHeight:          12,
				CropLeft:          0,
				CropRight:         0,
				CropTop:           0,
				CropBottom:        6,
				Width:             320,
				Height:            180,
				FPS:               30,
			},
		},
		{
			name: "sps3nalu 1280x720@60fps",
			data: sps3nalu,
			want: h264parser.SPSInfo{
				ID:                0,
				ProfileIdc:        100,
				LevelIdc:          32,
				ConstraintSetFlag: 0,
				MbWidth:           80,
				MbHeight:          45,
				CropLeft:          0,
				CropRight:         0,
				CropTop:           0,
				CropBottom:        0,
				Width:             1280,
				Height:            720,
				FPS:               60,
			},
		},
		{
			name: "sps4nalu 2688x1520@10fps getsaridc",
			data: sps4nalu,
			want: h264parser.SPSInfo{
				ID:                0,
				ProfileIdc:        77,
				LevelIdc:          50,
				ConstraintSetFlag: 0,
				MbWidth:           168,
				MbHeight:          95,
				CropLeft:          0,
				CropRight:         0,
				CropTop:           0,
				CropBottom:        0,
				Width:             2688,
				Height:            1520,
				FPS:               10,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := h264parser.ParseSPS(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseSPS() error = %v, wantErr %v", err, tt.wantErr)

				return
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseSPS() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NALU type predicates
// ---------------------------------------------------------------------------

func TestIsPredicates(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1E} // SPS (type 7)
	pps := []byte{0x68, 0xCE, 0x38, 0x80} // PPS (type 8)
	idr := []byte{0x65, 0x88, 0x84}       // IDR slice (type 5)
	p := []byte{0x61, 0x00}               // non-IDR slice (type 1)

	if !h264parser.IsSPSNALU(sps) {
		t.Error("expected SPS to be SPS NALU")
	}

	if !h264parser.IsPPSNALU(pps) {
		t.Error("expected PPS to be PPS NALU")
	}

	if !h264parser.IsParamSetNALU(sps) {
		t.Error("SPS should be param set")
	}

	if !h264parser.IsParamSetNALU(pps) {
		t.Error("PPS should be param set")
	}

	if !h264parser.IsKeyFrame(idr) {
		t.Error("IDR should be keyframe")
	}

	if h264parser.IsKeyFrame(p) {
		t.Error("non-IDR should not be keyframe")
	}

	if !h264parser.IsDataNALU(idr) {
		t.Error("IDR should be data NALU")
	}

	if !h264parser.IsDataNALU(p) {
		t.Error("non-IDR slice should be data NALU")
	}

	if h264parser.IsDataNALU(sps) {
		t.Error("SPS should not be data NALU")
	}
}

// ---------------------------------------------------------------------------
// AnnexB / AVCC detection and conversion
// ---------------------------------------------------------------------------

func TestCheckNALUsTypeAnnexB(t *testing.T) {
	annexb := append([]byte{0x00, 0x00, 0x00, 0x01}, sps1nalu...)
	got := h264parser.CheckNALUsType(annexb)

	if got != parser.NALUAnnexb {
		t.Errorf("CheckNALUsType() = %v, want NALUAnnexb", got)
	}
}

func TestCheckNALUsTypeAVCC(t *testing.T) {
	avcc := h264parser.AnnexBToAVCC([][]byte{sps1nalu})
	got := h264parser.CheckNALUsType(avcc)

	if got != parser.NALUAvcc {
		t.Errorf("CheckNALUsType() = %v, want NALUAvcc", got)
	}
}

func TestAnnexBToAVCCRoundtrip(t *testing.T) {
	original := [][]byte{sps1nalu, ppsNALU}
	avcc := h264parser.AnnexBToAVCC(original)
	back := h264parser.AVCCToAnnexB(avcc)

	if len(back) != len(original) {
		t.Fatalf("roundtrip: got %d NALUs, want %d", len(back), len(original))
	}

	for i, n := range original {
		if !bytes.Equal(n, back[i]) {
			t.Errorf("NALU[%d] mismatch: got %x, want %x", i, back[i], n)
		}
	}
}

func TestAVCCToAnnexBEmpty(t *testing.T) {
	if got := h264parser.AVCCToAnnexB(nil); got != nil {
		t.Errorf("AVCCToAnnexB(nil) = %v, want nil", got)
	}
}

func TestParameterSetsAnnexB(t *testing.T) {
	out := h264parser.ParameterSetsAnnexB(sps1nalu, ppsNALU)

	if !bytes.HasPrefix(out, []byte{0, 0, 0, 1}) {
		t.Error("ParameterSetsAnnexB() must start with 4-byte start code")
	}

	// Must contain two start codes: one for SPS, one for PPS
	count := bytes.Count(out, []byte{0, 0, 0, 1})
	if count != 2 {
		t.Errorf("ParameterSetsAnnexB() start code count = %d, want 2", count)
	}
}

// ---------------------------------------------------------------------------
// CodecData / AVCDecoderConfRecord
// ---------------------------------------------------------------------------

func TestNewCodecDataFromSPSAndPPS(t *testing.T) {
	cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps1nalu, ppsNALU)
	if err != nil {
		t.Fatalf("NewCodecDataFromSPSAndPPS() error: %v", err)
	}

	if cd.Width() != 1280 || cd.Height() != 720 {
		t.Errorf("dimensions = %dx%d, want 1280x720", cd.Width(), cd.Height())
	}

	if cd.FPS() != 50 {
		t.Errorf("FPS() = %d, want 50", cd.FPS())
	}

	if cd.TimeScale() != 90000 {
		t.Errorf("TimeScale() = %d, want 90000", cd.TimeScale())
	}

	t.Logf("resolution=%s tag=%s fps=%d bw=%s", cd.Resolution(), cd.Tag(), cd.FPS(), cd.Bandwidth())
}

func TestAVCDecoderConfRecord_Roundtrip(t *testing.T) {
	cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps1nalu, ppsNALU)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	record := cd.AVCDecoderConfRecordBytes()

	cd2, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(record)
	if err != nil {
		t.Fatalf("NewCodecDataFromAVCDecoderConfRecord() error: %v", err)
	}

	if cd.Width() != cd2.Width() || cd.Height() != cd2.Height() {
		t.Errorf("roundtrip mismatch: %dx%d vs %dx%d",
			cd.Width(), cd.Height(), cd2.Width(), cd2.Height())
	}

	if cd.FPS() != cd2.FPS() {
		t.Errorf("FPS roundtrip: %d vs %d", cd.FPS(), cd2.FPS())
	}
}

// ---------------------------------------------------------------------------
// Synthetic AnnexB/AVCC streams — saved to testdata/ for future use
// ---------------------------------------------------------------------------

func TestSyntheticAnnexBStream(t *testing.T) {
	startCode := []byte{0x00, 0x00, 0x00, 0x01}

	var buf bytes.Buffer
	buf.Write(startCode)
	buf.Write(sps1nalu)
	buf.Write(startCode)
	buf.Write(ppsNALU)

	stream := buf.Bytes()

	if h264parser.CheckNALUsType(stream) != parser.NALUAnnexb {
		t.Error("synthetic stream should be detected as AnnexB")
	}

	saveTestdata(t, "annexb.bin", stream)

	avccStream := h264parser.AnnexBToAVCC([][]byte{sps1nalu, ppsNALU})
	saveTestdata(t, "avcc.bin", avccStream)

	back := h264parser.AVCCToAnnexB(avccStream)
	if len(back) != 2 {
		t.Errorf("AVCC roundtrip: got %d NALUs, want 2", len(back))
	}

	if !bytes.Equal(back[0], sps1nalu) {
		t.Error("AVCC roundtrip: SPS mismatch")
	}

	if !bytes.Equal(back[1], ppsNALU) {
		t.Error("AVCC roundtrip: PPS mismatch")
	}
}

// ---------------------------------------------------------------------------
// Download real H.264 Annex-B stream from the internet and cache to testdata/
// ---------------------------------------------------------------------------

// candidateURLs are tried in order until one succeeds.
// Source: cisco/openh264 test vectors on GitHub (~56 KB raw Annex-B).
var candidateURLs = []string{
	"https://raw.githubusercontent.com/cisco/openh264/master/res/test_vd_1d.264",
}

func TestDownloadRealH264Stream(t *testing.T) {
	const cacheFile = "real_h264.264"
	cached := filepath.Join("testdata", cacheFile)

	data, err := os.ReadFile(cached)
	if err != nil {
		for _, u := range candidateURLs {
			data = downloadFile(t, u)
			if data != nil {
				break
			}
		}

		if data == nil {
			t.Skip("skipping: could not download real H.264 stream and no cached copy found")
		}

		saveTestdata(t, cacheFile, data)
	}

	if len(data) < 4 {
		t.Fatal("downloaded data too short")
	}

	typ := h264parser.CheckNALUsType(data[:min(len(data), 512)])
	t.Logf("real H.264 stream: %d bytes, format=%v", len(data), typ)
}

// ---------------------------------------------------------------------------
// Parse real H.264 stream — requires TestDownloadRealH264Stream to run first
// ---------------------------------------------------------------------------

func TestParseRealH264Stream(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "real_h264.264"))
	if err != nil {
		t.Skip("skipping: testdata/real_h264.264 not available (run TestDownloadRealH264Stream first)")
	}

	nalus, typ := parser.SplitNALUs(data)
	if typ != parser.NALUAnnexb {
		t.Fatalf("expected Annex-B stream, got %v", typ)
	}

	t.Logf("total NALUs in stream: %d", len(nalus))

	var sps, pps []byte

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		switch {
		case h264parser.IsSPSNALU(nalu) && sps == nil:
			sps = nalu
			t.Logf("SPS found: %d bytes, first 8: %x", len(sps), sps[:min(8, len(sps))])
		case h264parser.IsPPSNALU(nalu) && pps == nil:
			pps = nalu
			t.Logf("PPS found: %d bytes, first 8: %x", len(pps), pps[:min(8, len(pps))])
		}
	}

	if sps == nil {
		t.Error("no SPS found in stream")
	}

	if pps == nil {
		t.Error("no PPS found in stream")
	}

	if sps == nil || pps == nil {
		return
	}

	info, err := h264parser.ParseSPS(sps)
	if err != nil {
		t.Fatalf("ParseSPS on real stream: %v", err)
	}

	t.Logf("parsed SPS: Width=%d Height=%d Profile=%d Level=%d FPS=%d",
		info.Width, info.Height, info.ProfileIdc, info.LevelIdc, info.FPS)

	if info.Width == 0 || info.Height == 0 {
		t.Error("ParseSPS returned zero width or height")
	}

	cd, err := h264parser.NewCodecDataFromSPSAndPPS(sps, pps)
	if err != nil {
		t.Fatalf("NewCodecDataFromSPSAndPPS on real stream: %v", err)
	}

	t.Logf("codec: %s fps=%d bw=%s tag=%s", cd.Resolution(), cd.FPS(), cd.Bandwidth(), cd.Tag())

	// hvcC round-trip
	record := cd.AVCDecoderConfRecordBytes()

	cd2, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(record)
	if err != nil {
		t.Fatalf("avcC round-trip: %v", err)
	}

	if cd.Width() != cd2.Width() || cd.Height() != cd2.Height() {
		t.Errorf("avcC round-trip dimension mismatch: %dx%d vs %dx%d",
			cd.Width(), cd.Height(), cd2.Width(), cd2.Height())
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func saveTestdata(t *testing.T, name string, data []byte) {
	t.Helper()

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Errorf("mkdir testdata: %v", err)

		return
	}

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
		t.Logf("download HTTP %d for %s", resp.StatusCode, url)

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
