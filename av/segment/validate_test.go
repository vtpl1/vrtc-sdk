package segment_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/segment"
)

// writeBox writes an ISO BMFF box (size + type + payload) to buf.
func writeBox(buf []byte, offset int, typ string, payloadSize int) int {
	totalSize := 8 + payloadSize
	binary.BigEndian.PutUint32(buf[offset:], uint32(totalSize))
	copy(buf[offset+4:], typ)

	return offset + totalSize
}

func tmpFile(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.mp4")

	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	return path
}

func TestValidateSegment_Valid(t *testing.T) {
	t.Parallel()

	// ftyp (16 bytes) + moov (16 bytes) + moof (16 bytes) + mdat (16 bytes)
	buf := make([]byte, 64)
	off := writeBox(buf, 0, "ftyp", 8)
	off = writeBox(buf, off, "moov", 8)
	off = writeBox(buf, off, "moof", 8)
	writeBox(buf, off, "mdat", 8)

	path := tmpFile(t, buf)

	if err := segment.ValidateSegment(path); err != nil {
		t.Fatalf("expected valid segment, got: %v", err)
	}
}

func TestValidateSegment_Empty(t *testing.T) {
	t.Parallel()

	path := tmpFile(t, []byte{})

	err := segment.ValidateSegment(path)
	if !errors.Is(err, segment.ErrSegmentEmpty) {
		t.Fatalf("expected ErrSegmentEmpty, got: %v", err)
	}
}

func TestValidateSegment_NoFtyp(t *testing.T) {
	t.Parallel()

	buf := make([]byte, 32)
	writeBox(buf, 0, "moov", 8)
	writeBox(buf, 16, "moof", 8)

	path := tmpFile(t, buf)

	err := segment.ValidateSegment(path)
	if !errors.Is(err, segment.ErrSegmentNoFtyp) {
		t.Fatalf("expected ErrSegmentNoFtyp, got: %v", err)
	}
}

func TestValidateSegment_BadFtypSize(t *testing.T) {
	t.Parallel()

	// ftyp box with size=4 (invalid, minimum is 8).
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:], 4)
	copy(buf[4:], "ftyp")

	path := tmpFile(t, buf)

	err := segment.ValidateSegment(path)
	if !errors.Is(err, segment.ErrSegmentBadFtyp) {
		t.Fatalf("expected ErrSegmentBadFtyp, got: %v", err)
	}
}

func TestValidateSegment_NoMoof(t *testing.T) {
	t.Parallel()

	// ftyp (16 bytes) + moov (16 bytes), no moof.
	buf := make([]byte, 32)
	off := writeBox(buf, 0, "ftyp", 8)
	writeBox(buf, off, "moov", 8)

	path := tmpFile(t, buf)

	err := segment.ValidateSegment(path)
	if !errors.Is(err, segment.ErrSegmentNoMoof) {
		t.Fatalf("expected ErrSegmentNoMoof, got: %v", err)
	}
}

func TestValidateSegment_BadBoxSize(t *testing.T) {
	t.Parallel()

	// ftyp (16 bytes) + a box with size=2 (invalid).
	buf := make([]byte, 24)
	writeBox(buf, 0, "ftyp", 8)
	binary.BigEndian.PutUint32(buf[16:], 2)
	copy(buf[20:], "moov")

	path := tmpFile(t, buf)

	err := segment.ValidateSegment(path)
	if !errors.Is(err, segment.ErrSegmentBadBoxSize) {
		t.Fatalf("expected ErrSegmentBadBoxSize, got: %v", err)
	}
}

func TestValidateSegment_NonExistentFile(t *testing.T) {
	t.Parallel()

	err := segment.ValidateSegment(filepath.Join(t.TempDir(), "nonexistent.mp4"))
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
