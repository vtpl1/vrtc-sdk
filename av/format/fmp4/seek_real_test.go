package fmp4_test

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/h264parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
	"github.com/vtpl1/vrtc-sdk/av/codec/parser"
	"github.com/vtpl1/vrtc-sdk/av/format/fmp4"
)

const realRecordingsDir = "/home/m/edge-runtime/data/recordings"

// videoCodec identifies the video codec of a stream set.
type videoCodec int

const (
	codecUnknown videoCodec = iota
	codecH264
	codecH265
)

func (c videoCodec) String() string {
	switch c {
	case codecH264:
		return "H.264"
	case codecH265:
		return "H.265"
	default:
		return "unknown"
	}
}

// detectVideoCodec returns the video codec from the stream list.
func detectVideoCodec(streams []av.Stream) videoCodec {
	for _, s := range streams {
		switch s.Codec.(type) {
		case h264parser.CodecData:
			return codecH264
		case h265parser.CodecData:
			return codecH265
		}
	}

	return codecUnknown
}

// maxTestFiles caps the number of recording files used in a single test run.
// The recordings directory can contain hundreds of large files; testing all of
// them is prohibitively slow under -race. Files are shuffled before capping so
// that repeated runs still exercise different recordings.
const maxTestFiles = 10

// findFmp4Files returns up to maxTestFiles .fmp4 files under the recordings
// directory, skipping incomplete (no end-time) segments.
func findFmp4Files(t *testing.T) []string {
	t.Helper()

	var files []string

	err := filepath.Walk(realRecordingsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || filepath.Ext(path) != ".fmp4" {
			return nil
		}

		// Skip segments without end time (still recording).
		base := info.Name()
		base = base[:len(base)-len(".fmp4")]

		if len(base) == 6 {
			// "HHMMSS" only — no end time, still being written.
			return nil
		}

		// Only use files > 1MB (meaningful content).
		if info.Size() < 1<<20 {
			return nil
		}

		files = append(files, path)

		return nil
	})
	if err != nil {
		t.Fatalf("walking recordings dir: %v", err)
	}

	if len(files) == 0 {
		t.Skipf("no completed .fmp4 files found in %s", realRecordingsDir)
	}

	// Shuffle and cap to avoid spending minutes on hundreds of large files.
	rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })

	if len(files) > maxTestFiles {
		files = files[:maxTestFiles]
	}

	return files
}

// fileDuration opens an fMP4 and returns the total duration and video codec.
// When a sidx index is present the duration is computed from the index in O(1);
// otherwise it falls back to a full packet scan.
func fileDuration(t *testing.T, path string) (time.Duration, int, videoCodec) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	dmx := fmp4.NewDemuxer(f)
	ctx := context.Background()

	streams, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs %s: %v", path, err)
	}

	codec := detectVideoCodec(streams)

	// Fast path: derive duration from sidx without reading any packets.
	if sidx := dmx.Sidx(); len(sidx) > 0 {
		last := sidx[len(sidx)-1]
		dur := last.PTS + last.Duration

		return dur, len(sidx), codec // frame count approximated by fragment count
	}

	// Slow path: read every packet.
	var lastDTS time.Duration
	var lastDur time.Duration
	count := 0

	for {
		pkt, err := dmx.ReadPacket(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket %s: %v", path, err)
		}

		if pkt.Idx == 0 { // video track
			lastDTS = pkt.DTS
			lastDur = pkt.Duration
			count++
		}
	}

	return lastDTS + lastDur, count, codec
}

// TestSeekRealFiles_RandomPositions opens real fMP4 recordings, seeks to random
// positions, and validates:
//  1. First packet after seek is a keyframe
//  2. First packet DTS <= target
//  3. Seek tolerance (how far before target the keyframe lands)
//  4. NALU data is valid AVCC and contains an IDR/IRAP slice for keyframes
func TestSeekRealFiles_RandomPositions(t *testing.T) {
	if _, err := os.Stat(realRecordingsDir); os.IsNotExist(err) {
		t.Skipf("recordings directory not found: %s", realRecordingsDir)
	}

	files := findFmp4Files(t)
	t.Logf("found %d completed .fmp4 files", len(files))

	const seeksPerFile = 20

	for _, path := range files {
		rel, _ := filepath.Rel(realRecordingsDir, path)
		t.Run(rel, func(t *testing.T) {
			// First pass: determine file duration and codec.
			dur, frameCount, codec := fileDuration(t, path)
			t.Logf("file: %s  codec: %s  duration: %v  video frames: %d", rel, codec, dur, frameCount)

			if dur < time.Second {
				t.Skipf("file too short: %v", dur)
			}

			// Open file for seeking.
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			dmx := fmp4.NewDemuxer(f)
			ctx := context.Background()

			if _, err := dmx.GetCodecs(ctx); err != nil {
				t.Fatalf("GetCodecs: %v", err)
			}

			// Log sidx info.
			sidx := dmx.Sidx()
			t.Logf("sidx entries: %d", len(sidx))

			for i, e := range sidx {
				if i < 5 || i == len(sidx)-1 {
					t.Logf("  sidx[%d]: PTS=%v  dur=%v  offset=%d  size=%d  SAP=%v",
						i, e.PTS, e.Duration, e.Offset, e.Size, e.StartsWithSAP)
				}
			}

			var maxTolerance time.Duration
			var totalTolerance time.Duration
			var tolerances []time.Duration

			for i := range seeksPerFile {
				// Generate random seek target within the file duration.
				target := time.Duration(rand.Int64N(int64(dur)))

				err := dmx.SeekToKeyframe(target)
				if err != nil {
					t.Errorf("seek #%d to %v: %v", i, target, err)
					continue
				}

				// Read the first video packet after seek.
				var pkt av.Packet
				found := false

				for range 200 { // read up to 200 packets to find a video one
					p, err := dmx.ReadPacket(ctx)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Errorf("seek #%d ReadPacket after seek to %v: %v", i, target, err)
						break
					}

					if p.Idx == 0 { // video track
						pkt = p
						found = true
						break
					}
				}

				if !found {
					t.Errorf("seek #%d to %v: no video packet found after seek", i, target)
					continue
				}

				// Validate: first packet must be a keyframe.
				if !pkt.KeyFrame {
					t.Errorf("seek #%d to %v: first video packet is NOT a keyframe (DTS=%v)",
						i, target, pkt.DTS)
				}

				// Validate: DTS must be <= target.
				if pkt.DTS > target {
					t.Errorf("seek #%d to %v: packet DTS %v is AFTER target",
						i, target, pkt.DTS)
				}

				// Track tolerance.
				tolerance := target - pkt.DTS
				tolerances = append(tolerances, tolerance)
				totalTolerance += tolerance

				if tolerance > maxTolerance {
					maxTolerance = tolerance
				}

				// Validate NALU structure.
				if len(pkt.Data) > 4 {
					validateNALUs(t, pkt.Data, codec, i, target, pkt.KeyFrame)
				}
			}

			// Report tolerance statistics.
			if len(tolerances) > 0 {
				avgTolerance := totalTolerance / time.Duration(len(tolerances))
				t.Logf("seek tolerance over %d seeks:", len(tolerances))
				t.Logf("  avg: %v", avgTolerance)
				t.Logf("  max: %v", maxTolerance)

				// Compute percentiles.
				sorted := make([]time.Duration, len(tolerances))
				copy(sorted, tolerances)
				sortDurations(sorted)
				t.Logf("  p50: %v", sorted[len(sorted)/2])
				t.Logf("  p90: %v", sorted[len(sorted)*90/100])
				t.Logf("  p99: %v", sorted[min(len(sorted)-1, len(sorted)*99/100)])
				t.Logf("  min: %v", sorted[0])
			}
		})
	}
}

// TestSeekRealFiles_BoundaryPositions tests seeking to 0, near-end, and exact
// sidx PTS values.
func TestSeekRealFiles_BoundaryPositions(t *testing.T) {
	if _, err := os.Stat(realRecordingsDir); os.IsNotExist(err) {
		t.Skipf("recordings directory not found: %s", realRecordingsDir)
	}

	files := findFmp4Files(t)
	if len(files) == 0 {
		t.Skip("no files")
	}

	// Test both H.264 and H.265 files.
	tested := map[videoCodec]bool{}

	for _, path := range files {
		_, _, codec := fileDuration(t, path)
		if tested[codec] {
			continue
		}
		tested[codec] = true

		rel, _ := filepath.Rel(realRecordingsDir, path)
		t.Run(fmt.Sprintf("%s/%s", codec, rel), func(t *testing.T) {
			dur, _, _ := fileDuration(t, path)
			t.Logf("file: %s  codec: %s  duration: %v", rel, codec, dur)

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			dmx := fmp4.NewDemuxer(f)
			ctx := context.Background()

			if _, err := dmx.GetCodecs(ctx); err != nil {
				t.Fatalf("GetCodecs: %v", err)
			}

			targets := []struct {
				name   string
				target time.Duration
			}{
				{"start", 0},
				{"1ms", time.Millisecond},
				{"100ms", 100 * time.Millisecond},
				{"1s", time.Second},
				{"mid", dur / 2},
				{"near-end", dur - time.Second},
				{"last-ms", dur - time.Millisecond},
			}

			for _, tc := range targets {
				t.Run(tc.name, func(t *testing.T) {
					target := tc.target
					if target < 0 {
						target = 0
					}
					if target >= dur {
						target = dur - time.Millisecond
					}

					err := dmx.SeekToKeyframe(target)
					if err != nil {
						t.Fatalf("seek to %v: %v", target, err)
					}

					pkt, err := dmx.ReadPacket(ctx)
					if err != nil {
						t.Fatalf("ReadPacket after seek to %v: %v", target, err)
					}

					t.Logf("target=%v  got DTS=%v  keyframe=%v  tolerance=%v",
						target, pkt.DTS, pkt.KeyFrame, target-pkt.DTS)

					if pkt.DTS > target {
						t.Errorf("DTS %v > target %v", pkt.DTS, target)
					}
				})
			}
		})
	}
}

// TestSeekRealFiles_SeekThenDecodeSequence validates that after a seek, we can
// read a sequence of packets with monotonically non-decreasing DTS and that
// keyframes appear at expected intervals.
func TestSeekRealFiles_SeekThenDecodeSequence(t *testing.T) {
	if _, err := os.Stat(realRecordingsDir); os.IsNotExist(err) {
		t.Skipf("recordings directory not found: %s", realRecordingsDir)
	}

	files := findFmp4Files(t)
	if len(files) == 0 {
		t.Skip("no files")
	}

	// Test one file per codec type.
	tested := map[videoCodec]bool{}

	for _, path := range files {
		_, _, codec := fileDuration(t, path)
		if tested[codec] {
			continue
		}
		tested[codec] = true

		rel, _ := filepath.Rel(realRecordingsDir, path)
		t.Run(fmt.Sprintf("%s/%s", codec, rel), func(t *testing.T) {
			dur, _, _ := fileDuration(t, path)
			t.Logf("file: %s  codec: %s  duration: %v", rel, codec, dur)

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			dmx := fmp4.NewDemuxer(f)
			ctx := context.Background()

			if _, err := dmx.GetCodecs(ctx); err != nil {
				t.Fatalf("GetCodecs: %v", err)
			}

			// Seek to the middle.
			target := dur / 2
			if err := dmx.SeekToKeyframe(target); err != nil {
				t.Fatalf("seek: %v", err)
			}

			// Read 200 packets and check DTS monotonicity + NALU validity.
			var prevDTS time.Duration
			var keyframes, total int
			first := true

			for range 200 {
				pkt, err := dmx.ReadPacket(ctx)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("ReadPacket: %v", err)
				}

				if pkt.Idx != 0 {
					continue // skip audio
				}

				total++

				if first {
					if !pkt.KeyFrame {
						t.Error("first packet after seek must be a keyframe")
					}
					first = false
				}

				if pkt.DTS < prevDTS {
					t.Errorf("DTS went backwards: %v < %v (packet %d)", pkt.DTS, prevDTS, total)
				}
				prevDTS = pkt.DTS

				if pkt.KeyFrame {
					keyframes++
				}

				// Validate every packet's NALU structure.
				if len(pkt.Data) > 4 {
					validateNALUs(t, pkt.Data, codec, total, pkt.DTS, pkt.KeyFrame)
				}
			}

			t.Logf("read %d video packets after seek (keyframes: %d)", total, keyframes)

			if total == 0 {
				t.Error("no video packets read after seek")
			}
		})
	}
}

// TestSeekRealFiles_SidxAccuracy compares sidx PTS entries against actual
// fragment base times found by scanning, to validate the sidx index is accurate.
func TestSeekRealFiles_SidxAccuracy(t *testing.T) {
	if _, err := os.Stat(realRecordingsDir); os.IsNotExist(err) {
		t.Skipf("recordings directory not found: %s", realRecordingsDir)
	}

	files := findFmp4Files(t)

	for _, path := range files {
		rel, _ := filepath.Rel(realRecordingsDir, path)
		t.Run(rel, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			dmx := fmp4.NewDemuxer(f)
			ctx := context.Background()

			streams, err := dmx.GetCodecs(ctx)
			if err != nil {
				t.Fatalf("GetCodecs: %v", err)
			}

			codec := detectVideoCodec(streams)
			t.Logf("codec: %s", codec)

			sidx := dmx.Sidx()
			if len(sidx) == 0 {
				t.Skip("no sidx index in file")
			}

			t.Logf("sidx has %d entries", len(sidx))

			// For each sidx entry, seek to its PTS and verify the first
			// packet DTS matches.
			for i, entry := range sidx {
				if err := dmx.SeekToKeyframe(entry.PTS); err != nil {
					t.Errorf("seek to sidx[%d] PTS=%v: %v", i, entry.PTS, err)
					continue
				}

				pkt, err := dmx.ReadPacket(ctx)
				if err != nil {
					t.Errorf("ReadPacket after seek to sidx[%d]: %v", i, err)
					continue
				}

				drift := pkt.DTS - entry.PTS
				if drift < 0 {
					drift = -drift
				}

				// Allow up to 1 frame of drift (50ms at 20fps).
				if drift > 50*time.Millisecond {
					t.Errorf("sidx[%d]: PTS=%v but first packet DTS=%v (drift=%v)",
						i, entry.PTS, pkt.DTS, drift)
				}

				if i < 3 || i == len(sidx)-1 {
					t.Logf("sidx[%d]: PTS=%v  pkt.DTS=%v  drift=%v  keyframe=%v",
						i, entry.PTS, pkt.DTS, drift, pkt.KeyFrame)
				}
			}
		})
	}
}

// TestSeekRealFiles_RepeatedSeekConsistency seeks to the same position multiple
// times and verifies the same packet is returned each time.
func TestSeekRealFiles_RepeatedSeekConsistency(t *testing.T) {
	if _, err := os.Stat(realRecordingsDir); os.IsNotExist(err) {
		t.Skipf("recordings directory not found: %s", realRecordingsDir)
	}

	files := findFmp4Files(t)
	if len(files) == 0 {
		t.Skip("no files")
	}

	// Test one file per codec type.
	tested := map[videoCodec]bool{}

	for _, path := range files {
		_, _, codec := fileDuration(t, path)
		if tested[codec] {
			continue
		}
		tested[codec] = true

		rel, _ := filepath.Rel(realRecordingsDir, path)
		t.Run(fmt.Sprintf("%s/%s", codec, rel), func(t *testing.T) {
			dur, _, _ := fileDuration(t, path)

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			dmx := fmp4.NewDemuxer(f)
			ctx := context.Background()

			if _, err := dmx.GetCodecs(ctx); err != nil {
				t.Fatalf("GetCodecs: %v", err)
			}

			// Pick 5 random targets and seek to each 3 times.
			for i := range 5 {
				target := time.Duration(rand.Int64N(int64(dur)))

				var firstDTS time.Duration
				var firstLen int

				for attempt := range 3 {
					if err := dmx.SeekToKeyframe(target); err != nil {
						t.Fatalf("seek #%d attempt %d: %v", i, attempt, err)
					}

					pkt, err := dmx.ReadPacket(ctx)
					if err != nil {
						t.Fatalf("ReadPacket #%d attempt %d: %v", i, attempt, err)
					}

					if attempt == 0 {
						firstDTS = pkt.DTS
						firstLen = len(pkt.Data)
					} else {
						if pkt.DTS != firstDTS {
							t.Errorf("target=%v: attempt %d DTS=%v != first DTS=%v",
								target, attempt, pkt.DTS, firstDTS)
						}
						if len(pkt.Data) != firstLen {
							t.Errorf("target=%v: attempt %d data len=%d != first len=%d",
								target, attempt, len(pkt.Data), firstLen)
						}
					}
				}

				t.Logf("target=%v → consistent DTS=%v  dataLen=%d", target, firstDTS, firstLen)
			}
		})
	}
}

// ── NALU validation helpers ─────────────────────────────────────────────────

// validateNALUs checks that packet data looks like valid AVCC-framed NALUs.
// For keyframe packets, it verifies the appropriate keyframe NALU is present
// (IDR for H.264, IRAP for H.265).
func validateNALUs(t *testing.T, data []byte, codec videoCodec, seekIdx int, target time.Duration, isKeyframe bool) {
	t.Helper()

	nalus, typ := parser.SplitNALUs(data)
	if typ != parser.NALUAvcc {
		t.Errorf("seek #%d (target=%v): expected AVCC format, got %v", seekIdx, target, typ)
		return
	}

	if len(nalus) == 0 {
		t.Errorf("seek #%d (target=%v): no NALUs found in %d bytes", seekIdx, target, len(data))
		return
	}

	switch codec {
	case codecH264:
		validateH264NALUs(t, nalus, seekIdx, target, isKeyframe)
	case codecH265:
		validateH265NALUs(t, nalus, seekIdx, target, isKeyframe)
	}
}

func validateH264NALUs(t *testing.T, nalus [][]byte, seekIdx int, target time.Duration, isKeyframe bool) {
	t.Helper()

	hasIDR := false
	hasSlice := false

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			t.Errorf("seek #%d (target=%v): empty NALU", seekIdx, target)
			continue
		}

		// Forbidden zero bit must be 0.
		if nalu[0]&0x80 != 0 {
			t.Errorf("seek #%d (target=%v): H.264 forbidden_zero_bit set in header 0x%02x",
				seekIdx, target, nalu[0])
		}

		if h264parser.IsKeyFrame(nalu) {
			hasIDR = true
		}

		if h264parser.IsDataNALU(nalu) {
			hasSlice = true
		}
	}

	if isKeyframe && !hasIDR {
		t.Errorf("seek #%d (target=%v): H.264 keyframe packet has no IDR NALU (%d NALUs, types: %s)",
			seekIdx, target, len(nalus), h264NaluTypeSummary(nalus))
	}

	if !hasSlice && !h264HasParamSets(nalus) {
		t.Errorf("seek #%d (target=%v): H.264 packet has no data NALUs and no param sets",
			seekIdx, target)
	}
}

func validateH265NALUs(t *testing.T, nalus [][]byte, seekIdx int, target time.Duration, isKeyframe bool) {
	t.Helper()

	hasIRAP := false
	hasSlice := false

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			t.Errorf("seek #%d (target=%v): empty NALU", seekIdx, target)
			continue
		}

		// H.265 NAL header is 2 bytes. Forbidden zero bit is bit 7 of byte 0.
		if nalu[0]&0x80 != 0 {
			t.Errorf("seek #%d (target=%v): H.265 forbidden_zero_bit set in header 0x%02x",
				seekIdx, target, nalu[0])
		}

		if len(nalu) < 2 {
			t.Errorf("seek #%d (target=%v): H.265 NALU too short (%d bytes)", seekIdx, target, len(nalu))
			continue
		}

		if h265parser.IsKeyFrame(nalu) {
			hasIRAP = true
		}

		if h265parser.IsDataNALU(nalu) {
			hasSlice = true
		}
	}

	if isKeyframe && !hasIRAP {
		t.Errorf("seek #%d (target=%v): H.265 keyframe packet has no IRAP NALU (%d NALUs, types: %s)",
			seekIdx, target, len(nalus), h265NaluTypeSummary(nalus))
	}

	if !hasSlice && !h265HasParamSets(nalus) {
		t.Errorf("seek #%d (target=%v): H.265 packet has no data NALUs and no param sets",
			seekIdx, target)
	}
}

func h264HasParamSets(nalus [][]byte) bool {
	for _, nalu := range nalus {
		if len(nalu) > 0 && h264parser.IsParamSetNALU(nalu) {
			return true
		}
	}

	return false
}

func h265HasParamSets(nalus [][]byte) bool {
	for _, nalu := range nalus {
		if len(nalu) > 0 && h265parser.IsParamSetNALU(nalu) {
			return true
		}
	}

	return false
}

func h264NaluTypeSummary(nalus [][]byte) string {
	var s string
	for i, nalu := range nalus {
		if i > 0 {
			s += ","
		}
		if len(nalu) == 0 {
			s += "empty"
			continue
		}
		s += fmt.Sprintf("%d", av.H264NaluType(nalu[0])&av.H264NALTypeMask)
	}

	return s
}

func h265NaluTypeSummary(nalus [][]byte) string {
	var s string
	for i, nalu := range nalus {
		if i > 0 {
			s += ","
		}
		if len(nalu) == 0 {
			s += "empty"
			continue
		}
		s += fmt.Sprintf("%s", h265parser.NALUType(nalu))
	}

	return s
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}
