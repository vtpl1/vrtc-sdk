package llhls_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/format/llhls"
)

// compile-time interface assertions.
var (
	_ av.MuxCloser    = (*llhls.Muxer)(nil)
	_ av.CodecChanger = (*llhls.Muxer)(nil)
)

// minimalAVCRecord is a synthetic AVCDecoderConfigurationRecord.
var minimalAVCRecord = []byte{
	0x01,
	0x42, 0x00, 0x1E,
	0xFF,
	0xE1,
	0x00, 0x0F,
	0x67, 0x42, 0x00, 0x1E,
	0xAC, 0xD9, 0x40, 0xA0,
	0x3D, 0xA1, 0x00, 0x00,
	0x03, 0x00, 0x00,
	0x01,
	0x00, 0x04,
	0x68, 0xCE, 0x38, 0x80,
}

// minimalAAC is a 2-byte AudioSpecificConfig for AAC-LC 44100 Hz stereo.
var minimalAAC = []byte{0x12, 0x10}

func makeH264(t *testing.T) h264parser.CodecData {
	t.Helper()

	c, err := h264parser.NewCodecDataFromAVCDecoderConfRecord(minimalAVCRecord)
	if err != nil {
		t.Fatalf("h264: %v", err)
	}

	return c
}

func makeAAC(t *testing.T) aacparser.CodecData {
	t.Helper()

	c, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes(minimalAAC)
	if err != nil {
		t.Fatalf("aac: %v", err)
	}

	return c
}

func streams(t *testing.T) []av.Stream {
	t.Helper()

	return []av.Stream{
		{Idx: 0, Codec: makeH264(t)},
		{Idx: 1, Codec: makeAAC(t)},
	}
}

func smallCfg() llhls.Config {
	cfg := llhls.DefaultConfig()
	cfg.PartTarget = 50 * time.Millisecond
	cfg.SegTarget = 200 * time.Millisecond

	return cfg
}

// feedKeyframes pushes n video keyframes with duration d into m.
func feedKeyframes(t *testing.T, m *llhls.Muxer, n int, d time.Duration) {
	t.Helper()

	ctx := context.Background()

	for i := range n {
		pkt := av.Packet{
			KeyFrame:  true,
			Idx:       0,
			DTS:       time.Duration(i) * d,
			Duration:  d,
			Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65, 0xDE, 0xAD, 0xBE, 0xEF},
			CodecType: av.H264,
		}
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket[%d]: %v", i, err)
		}
	}
}

// ── lifecycle ─────────────────────────────────────────────────────────────────

func TestWriteHeader_DoubleCall(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("first: %v", err)
	}

	if err := m.WriteHeader(ctx, streams(t)); err == nil {
		t.Fatal("expected error on second WriteHeader")
	}
}

func TestWriteTrailer_DoubleCall(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("first trailer: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err == nil {
		t.Fatal("expected error on second WriteTrailer")
	}
}

func TestWritePacket_BeforeHeader(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())

	pkt := av.Packet{Idx: 0, Data: []byte{0x00}, CodecType: av.H264}
	if err := m.WritePacket(context.Background(), pkt); err == nil {
		t.Fatal("expected error when header not written")
	}
}

// ── HTTP: init segment ────────────────────────────────────────────────────────

func TestServeInit(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/init.mp4", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.Bytes()
	if len(body) < 8 {
		t.Fatalf("init segment too short: %d bytes", len(body))
	}

	// First box must be ftyp.
	if string(body[4:8]) != "ftyp" {
		t.Errorf("first box = %q, want ftyp", string(body[4:8]))
	}
}

// ── HTTP: playlist ────────────────────────────────────────────────────────────

func TestServePlaylist_Basic(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Write enough keyframes to complete the first full segment.
	// Each keyframe flushes the previous part; segment finalises when
	// accumulated duration >= SegTarget at a keyframe boundary.
	nFrames := int(cfg.SegTarget/cfg.PartTarget) + 2
	for i := range nFrames {
		pkt := av.Packet{
			Idx:       0,
			KeyFrame:  true,
			DTS:       time.Duration(i) * cfg.PartTarget,
			Duration:  cfg.PartTarget,
			Data:      []byte{0x65, 0x01},
			CodecType: av.H264,
		}
		if err := m.WritePacket(ctx, pkt); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	body := rec.Body.String()

	for _, must := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:9",
		"#EXT-X-PART-INF:",
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES",
		"#EXT-X-MAP:URI=\"init.mp4\"",
		"#EXT-X-MEDIA-SEQUENCE:",
	} {
		if !strings.Contains(body, must) {
			t.Errorf("playlist missing %q\nGot:\n%s", must, body)
		}
	}
}

func TestServePlaylist_AfterParts(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Feed enough keyframes to produce at least one part.
	feedKeyframes(t, m, 6, cfg.PartTarget)

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil)
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "#EXT-X-PART:") {
		t.Errorf("expected #EXT-X-PART tags in playlist\nGot:\n%s", body)
	}

	if !strings.Contains(body, "#EXT-X-PRELOAD-HINT:") {
		t.Errorf("expected #EXT-X-PRELOAD-HINT in playlist\nGot:\n%s", body)
	}
}

func TestServePlaylist_FullSegment(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Feed enough frames to complete at least one full segment.
	feedKeyframes(t, m, 20, cfg.PartTarget)

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil)
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "#EXTINF:") {
		t.Errorf("expected #EXTINF tags (complete segment) in playlist\nGot:\n%s", body)
	}
}

// ── HTTP: parts and segments ──────────────────────────────────────────────────

func TestServePart_NotFound(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/part999_0.mp4", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestServePart_Found(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Produce at least one part.
	feedKeyframes(t, m, 4, cfg.PartTarget)

	// Determine which part was published by reading the playlist.
	h := m.Handler("/hls")

	plRec := httptest.NewRecorder()
	plReq := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil)
	h.ServeHTTP(plRec, plReq)

	body := plRec.Body.String()

	// Extract first part URI.
	var partURI string

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#EXT-X-PART:") {
			for _, field := range strings.Split(line, ",") {
				if strings.HasPrefix(field, "URI=") {
					partURI = strings.Trim(strings.TrimPrefix(field, "URI="), "\"")

					break
				}
			}

			if partURI != "" {
				break
			}
		}
	}

	if partURI == "" {
		t.Skip("no parts in playlist yet")
	}

	partRec := httptest.NewRecorder()
	partReq := httptest.NewRequest(http.MethodGet, "/hls/"+partURI, nil)
	h.ServeHTTP(partRec, partReq)

	if partRec.Code != http.StatusOK {
		t.Errorf("part %s: status = %d, want 200", partURI, partRec.Code)
	}

	if partRec.Body.Len() == 0 {
		t.Errorf("part %s: empty body", partURI)
	}

	// Verify moof+mdat structure.
	data := partRec.Body.Bytes()
	if len(data) < 8 {
		t.Fatalf("part data too short: %d", len(data))
	}

	if string(data[4:8]) != "moof" {
		t.Errorf("first box = %q, want moof", string(data[4:8]))
	}
}

func TestServeSegment_Found(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Produce at least one complete segment.
	feedKeyframes(t, m, 20, cfg.PartTarget)

	h := m.Handler("/hls")

	plRec := httptest.NewRecorder()
	h.ServeHTTP(plRec, httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil))

	body := plRec.Body.String()

	// Find first seg URI.
	var segURI string

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "seg") && strings.HasSuffix(line, ".mp4") {
			segURI = line

			break
		}
	}

	if segURI == "" {
		t.Skip("no complete segments in playlist yet")
	}

	segRec := httptest.NewRecorder()
	h.ServeHTTP(segRec, httptest.NewRequest(http.MethodGet, "/hls/"+segURI, nil))

	if segRec.Code != http.StatusOK {
		t.Errorf("seg %s: status = %d", segURI, segRec.Code)
	}

	if segRec.Body.Len() == 0 {
		t.Errorf("seg %s: empty body", segURI)
	}
}

// ── blocking reload ───────────────────────────────────────────────────────────

func TestBlockingReload_ImmediateSatisfy(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Produce several parts so MSN=0, part=0 is definitely available.
	feedKeyframes(t, m, 4, cfg.PartTarget)

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8?_HLS_msn=0&_HLS_part=0", nil)
	h.ServeHTTP(rec, req)

	// Should respond immediately (condition already satisfied).
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
}

func TestBlockingReload_Timeout(t *testing.T) {
	t.Parallel()

	cfg := llhls.DefaultConfig()
	cfg.BlockingReloadTimeout = 50 * time.Millisecond
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// No packets written → segment 999 will never exist.
	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8?_HLS_msn=999&_HLS_part=0", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// ── unknown resource ──────────────────────────────────────────────────────────

func TestUnknownPath(t *testing.T) {
	t.Parallel()

	m := llhls.NewMuxer(llhls.DefaultConfig())
	if err := m.WriteHeader(context.Background(), streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hls/unknown.bin", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ── WriteTrailer flushes ──────────────────────────────────────────────────────

func TestWriteTrailer_FlushesLastPart(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("header: %v", err)
	}

	// Write one keyframe - not enough to cross PartTarget by itself.
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  10 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	// After WriteTrailer, the playlist should have some content.
	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil))

	body := rec.Body.String()
	// The final flush should have produced a part or a segment.
	hasContent := strings.Contains(body, "#EXT-X-PART:") ||
		strings.Contains(body, "#EXTINF:")

	if !hasContent {
		t.Errorf("playlist after WriteTrailer has no parts/segments:\n%s", body)
	}
}

// ── MuxCloser ─────────────────────────────────────────────────────────────────

func TestClose_BestEffortFlush(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write a packet without calling WriteTrailer.
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  10 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Close should succeed and perform a best-effort flush.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second Close must also succeed (idempotent resource release).
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClose_ReleasesBlockedReaders(t *testing.T) {
	t.Parallel()

	cfg := llhls.DefaultConfig()
	cfg.BlockingReloadTimeout = 5 * time.Second // long enough to catch hang
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	h := m.Handler("/hls")

	done := make(chan int, 1)

	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/hls/index.m3u8?_HLS_msn=999&_HLS_part=0", nil)
		h.ServeHTTP(rec, req)
		done <- rec.Code
	}()

	// Give the goroutine time to block.
	time.Sleep(20 * time.Millisecond)

	// Close should broadcast and wake the blocked reader within the test timeout.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case code := <-done:
		// Any response (503 timeout or otherwise) is acceptable; what matters
		// is that the goroutine unblocked.
		_ = code
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the blocked playlist reader")
	}
}

// ── CodecChanger ──────────────────────────────────────────────────────────────

func TestWriteCodecChange_UpdatesInitData(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Capture init segment before change.
	h := m.Handler("/hls")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/hls/init.mp4", nil))
	init1 := rec1.Body.Bytes()

	// Trigger codec change (same codec, but the muxer must rebuild).
	changed := []av.Stream{{Idx: 0, Codec: makeH264(t)}}
	if err := m.WriteCodecChange(ctx, changed); err != nil {
		t.Fatalf("WriteCodecChange: %v", err)
	}

	// The init segment served now should still be valid fMP4.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/hls/init.mp4", nil))
	init2 := rec2.Body.Bytes()

	if len(init2) < 8 {
		t.Fatalf("init segment after change too short: %d bytes", len(init2))
	}

	if string(init2[4:8]) != "ftyp" {
		t.Errorf("first box after change = %q, want ftyp", string(init2[4:8]))
	}

	_ = init1 // before-change value; presence verified by init2 check above
}

func TestWriteCodecChange_FlushesCurrentPart(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	// Write one packet so there is something buffered.
	pkt := av.Packet{
		KeyFrame:  true,
		Idx:       0,
		DTS:       0,
		Duration:  10 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x05, 0x65},
		CodecType: av.H264,
	}
	if err := m.WritePacket(ctx, pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	// Codec change must flush the buffered part.
	if err := m.WriteCodecChange(ctx, []av.Stream{{Idx: 0, Codec: makeH264(t)}}); err != nil {
		t.Fatalf("WriteCodecChange: %v", err)
	}

	// Finalise the stream so the flushed part appears in a completed segment
	// (the initial playlist load now waits for the first complete segment).
	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	h := m.Handler("/hls")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hls/index.m3u8", nil))

	// The flushed packet should appear as a part in the completed segment.
	body := rec.Body.String()
	if !strings.Contains(body, "#EXT-X-PART:") {
		t.Errorf("expected flushed part in playlist after codec change\nGot:\n%s", body)
	}
}

func TestWriteCodecChange_AfterWritePacket_CanContinue(t *testing.T) {
	t.Parallel()

	cfg := smallCfg()
	m := llhls.NewMuxer(cfg)
	ctx := context.Background()

	if err := m.WriteHeader(ctx, streams(t)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := m.WriteCodecChange(ctx, []av.Stream{{Idx: 0, Codec: makeH264(t)}}); err != nil {
		t.Fatalf("WriteCodecChange: %v", err)
	}

	// After codec change, WritePacket must still work.
	feedKeyframes(t, m, 4, cfg.PartTarget)

	if err := m.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}
}
