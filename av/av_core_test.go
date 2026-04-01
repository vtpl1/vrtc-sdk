package av

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodecTypeConstructorsAndClassification(t *testing.T) {
	video := MakeVideoCodecType(42)
	audio := MakeAudioCodecType(42)

	if video.IsAudio() {
		t.Fatal("video codec reported as audio")
	}

	if !video.IsVideo() {
		t.Fatal("video codec not reported as video")
	}

	if !audio.IsAudio() {
		t.Fatal("audio codec not reported as audio")
	}

	if audio.IsVideo() {
		t.Fatal("audio codec reported as video")
	}
}

func TestCodecTypeStringMappings(t *testing.T) {
	t.Parallel()

	tests := map[CodecType]string{
		H264:       "H264",
		H265:       "H265",
		JPEG:       "JPEG",
		VP8:        "VP8",
		VP9:        "VP9",
		AV1:        "AV1",
		AAC:        "AAC",
		PCM_MULAW:  "PCM_MULAW",
		PCM_ALAW:   "PCM_ALAW",
		SPEEX:      "SPEEX",
		NELLYMOSER: "NELLYMOSER",
		PCM:        "PCM",
		OPUS:       "OPUS",
		MP3:        "MPA",
		PCML:       "PCML",
		ELD:        "AAC_ELD",
		FLAC:       "FLAC",
	}

	for codecType, want := range tests {
		if got := codecType.String(); got != want {
			t.Fatalf("codec %v: got %q, want %q", codecType, got, want)
		}
	}

	if got := CodecType(0).String(); got != "" {
		t.Fatalf("unknown codec string: got %q, want empty", got)
	}
}

func TestSampleFormatHelpers(t *testing.T) {
	t.Parallel()

	type sfCase struct {
		sf     SampleFormat
		bytes  int
		name   string
		planar bool
	}

	cases := []sfCase{
		{U8, 1, "U8", false},
		{S16, 2, "S16", false},
		{S32, 4, "S32", false},
		{FLT, 4, "FLT", false},
		{DBL, 8, "DBL", false},
		{U8P, 1, "U8P", false},
		{S16P, 2, "S16P", true},
		{S32P, 4, "S32P", true},
		{FLTP, 4, "FLTP", true},
		{DBLP, 8, "DBLP", true},
		{U32, 4, "U32", false},
		{SampleFormat(250), 0, "?", false},
	}

	for _, tc := range cases {
		if got := tc.sf.BytesPerSample(); got != tc.bytes {
			t.Fatalf("%s bytes/sample: got %d, want %d", tc.name, got, tc.bytes)
		}

		if got := tc.sf.String(); got != tc.name {
			t.Fatalf("%v String(): got %q, want %q", tc.sf, got, tc.name)
		}

		if got := tc.sf.IsPlanar(); got != tc.planar {
			t.Fatalf("%s IsPlanar(): got %v, want %v", tc.name, got, tc.planar)
		}
	}
}

func TestChannelLayoutHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		layout ChannelLayout
		count  int
		text   string
	}{
		{0, 0, "0ch"},
		{ChMono, 1, "1ch"},
		{ChStereo, 2, "2ch"},
		{Ch2_1, 3, "3ch"},
		{Ch2Point1, 3, "3ch"},
		{ChSurround, 3, "3ch"},
		{Ch3Point1, 4, "4ch"},
	}

	for _, tc := range tests {
		if got := tc.layout.Count(); got != tc.count {
			t.Fatalf("layout %v count: got %d, want %d", tc.layout, got, tc.count)
		}

		if got := tc.layout.String(); got != tc.text {
			t.Fatalf("layout %v string: got %q, want %q", tc.layout, got, tc.text)
		}
	}
}

func TestAudioFrameDurationAndFormatComparison(t *testing.T) {
	t.Parallel()

	a := AudioFrame{
		SampleFormat:  S16,
		ChannelLayout: ChStereo,
		SampleCount:   800,
		SampleRate:    8000,
	}

	if got := a.Duration(); got != 100*time.Millisecond {
		t.Fatalf("Duration: got %v, want 100ms", got)
	}

	if !a.HasSameFormat(a) {
		t.Fatal("HasSameFormat: expected true for same frame")
	}

	b := a
	b.SampleRate = 16000
	if a.HasSameFormat(b) {
		t.Fatal("HasSameFormat: expected false for different sample rate")
	}

	b = a
	b.ChannelLayout = ChMono
	if a.HasSameFormat(b) {
		t.Fatal("HasSameFormat: expected false for different channel layout")
	}

	b = a
	b.SampleFormat = FLTP
	if a.HasSameFormat(b) {
		t.Fatal("HasSameFormat: expected false for different sample format")
	}
}

func TestAudioFrameSliceAndConcat(t *testing.T) {
	t.Parallel()

	frame := AudioFrame{
		SampleFormat:  S16,
		ChannelLayout: ChMono,
		SampleCount:   4,
		SampleRate:    8000,
		Data:          [][]byte{{0, 1, 2, 3, 4, 5, 6, 7}},
	}

	sliced := frame.Slice(1, 3)
	if sliced.SampleCount != 2 {
		t.Fatalf("slice sample count: got %d, want 2", sliced.SampleCount)
	}

	wantSlice := []byte{2, 3, 4, 5}
	if string(sliced.Data[0]) != string(wantSlice) {
		t.Fatalf("slice data: got %v, want %v", sliced.Data[0], wantSlice)
	}

	next := AudioFrame{
		SampleFormat:  S16,
		ChannelLayout: ChMono,
		SampleCount:   2,
		SampleRate:    8000,
		Data:          [][]byte{{8, 9, 10, 11}},
	}

	concat := frame.Concat(next)
	if concat.SampleCount != 6 {
		t.Fatalf("concat sample count: got %d, want 6", concat.SampleCount)
	}

	wantConcat := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	if string(concat.Data[0]) != string(wantConcat) {
		t.Fatalf("concat data: got %v, want %v", concat.Data[0], wantConcat)
	}
}

func TestAudioFrameSlicePanicsOnInvalidRange(t *testing.T) {
	t.Parallel()

	frame := AudioFrame{
		SampleFormat:  S16,
		ChannelLayout: ChMono,
		SampleCount:   2,
		SampleRate:    8000,
		Data:          [][]byte{{0, 1, 2, 3}},
	}

	assertPanics(t, func() { _ = frame.Slice(-1, 1) })
	assertPanics(t, func() { _ = frame.Slice(0, 3) })
	assertPanics(t, func() { _ = frame.Slice(2, 1) })
}

func TestFrameAnalyticsString(t *testing.T) {
	t.Parallel()

	var nilAnalytics *FrameAnalytics
	if got := nilAnalytics.String(); got != "nil" {
		t.Fatalf("nil analytics string: got %q, want %q", got, "nil")
	}

	a := &FrameAnalytics{
		SiteID:       1,
		ChannelID:    2,
		FramePTS:     3,
		VehicleCount: 1,
		PeopleCount:  2,
		Objects: []*Detection{{
			X: 10, Y: 20, W: 30, H: 40,
			ClassID: 7, Confidence: 88, TrackID: 99, IsEvent: true,
		}},
	}

	got := a.String()
	if got == "" {
		t.Fatal("analytics string is empty")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("analytics string is not valid JSON: %v", err)
	}

	if _, ok := decoded["siteId"]; !ok {
		t.Fatal("analytics JSON missing siteId")
	}
}

func TestPacketHelpersAndFormatting(t *testing.T) {
	t.Parallel()

	wall := time.Unix(1700000000, 0).UTC()
	pkt := &Packet{
		FrameID:         42,
		KeyFrame:        true,
		IsDiscontinuity: true,
		Idx:             2,
		CodecType:       H264,
		DTS:             1234 * time.Millisecond,
		PTSOffset:       33 * time.Millisecond,
		Duration:        20 * time.Millisecond,
		WallClockTime:   wall,
		Data:            []byte{0x00, 0x00, 0x00, 0x01, 0x65}, // AVCC H264 IDR
	}

	if got := pkt.PTS(); got != 1267*time.Millisecond {
		t.Fatalf("PTS: got %v, want %v", got, 1267*time.Millisecond)
	}

	if !pkt.HasWallClockTime() {
		t.Fatal("HasWallClockTime: expected true")
	}

	if !pkt.IsKeyFrame() {
		t.Fatal("IsKeyFrame: expected true")
	}

	if !pkt.IsVideo() || pkt.IsAudio() {
		t.Fatal("video packet classification mismatch")
	}

	s := pkt.String()
	mustContain(t, s, "#42")
	mustContain(t, s, "H264:2")
	mustContain(t, s, "dts=1234ms")
	mustContain(t, s, "pts=1267ms")
	mustContain(t, s, "dur=20ms")
	mustContain(t, s, "K")
	mustContain(t, s, "DISC")
	mustContain(t, s, "IDR_SLICE")

	goString := pkt.GoString()
	mustContain(t, goString, "FrameID:         42")
	mustContain(t, goString, "WallClockTime:   "+wall.String())
	mustContain(t, goString, "Analytics:       nil")
}

func TestPacketStringForAudioAndH265AndSizes(t *testing.T) {
	t.Parallel()

	audio := &Packet{
		FrameID:   1,
		Idx:       1,
		CodecType: AAC,
		DTS:       1 * time.Second,
		Duration:  10 * time.Millisecond,
		Data:      []byte{1, 2, 3},
	}
	audioS := audio.String()
	mustContain(t, audioS, "AAC:1")
	if strings.Contains(audioS, "pts=") {
		t.Fatalf("unexpected pts in audio string: %s", audioS)
	}
	if strings.Contains(audioS, "IDR") || strings.Contains(audioS, "TRAIL") {
		t.Fatalf("unexpected video nalu type in audio string: %s", audioS)
	}

	h265 := &Packet{
		FrameID:   2,
		Idx:       0,
		CodecType: H265,
		DTS:       2 * time.Second,
		Duration:  40 * time.Millisecond,
		Data:      []byte{0x00, 0x00, 0x00, 0x01, 0x40}, // AVCC H265 VPS
	}
	mustContain(t, h265.String(), "VPS")

	kbPkt := &Packet{
		FrameID:   3,
		Idx:       0,
		CodecType: AAC,
		DTS:       0,
		Duration:  1 * time.Millisecond,
		Data:      make([]byte, 1536),
	}
	mustContain(t, kbPkt.String(), "1.5KB")

	mbPkt := &Packet{
		FrameID:   4,
		Idx:       0,
		CodecType: AAC,
		DTS:       0,
		Duration:  1 * time.Millisecond,
		Data:      make([]byte, 2*1024*1024),
	}
	mustContain(t, mbPkt.String(), "2.0MB")
}

func TestNALTypeStringers(t *testing.T) {
	t.Parallel()

	if got := H264_NAL_IDR_SLICE.String(); got != "IDR_SLICE" {
		t.Fatalf("H264 IDR name: got %q", got)
	}

	if got := H264NaluType(31).String(); got != "UNSPECIFIED(31)" {
		t.Fatalf("H264 unknown name: got %q", got)
	}

	if got := HEVC_NAL_VPS.String(); got != "VPS" {
		t.Fatalf("H265 VPS name: got %q", got)
	}

	if got := H265NaluType(63).String(); got != "UNSPECIFIED(63)" {
		t.Fatalf("H265 unknown name: got %q", got)
	}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic, got nil")
		}
	}()
	fn()
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("expected %q to contain %q", s, sub)
	}
}
