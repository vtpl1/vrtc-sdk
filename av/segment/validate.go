package segment

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	ErrSegmentEmpty      = errors.New("segment is empty")
	ErrSegmentNoFtyp     = errors.New("expected ftyp box")
	ErrSegmentBadFtyp    = errors.New("invalid ftyp box size")
	ErrSegmentNoMoof     = errors.New("no moof box found")
	ErrSegmentBadBoxSize = errors.New("invalid box size")
)

// ValidateSegment performs quick structural checks on an fMP4 segment file.
// Returns nil if the file appears valid, or an error describing the problem.
func ValidateSegment(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat segment: %w", err)
	}

	if info.Size() == 0 {
		return ErrSegmentEmpty
	}

	var buf [8]byte

	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return fmt.Errorf("read first box header: %w", err)
	}

	boxType := string(buf[4:8])
	if boxType != "ftyp" {
		return fmt.Errorf("%w: got %q", ErrSegmentNoFtyp, boxType)
	}

	ftypSize := int64(binary.BigEndian.Uint32(buf[0:4]))
	if ftypSize < 8 {
		return fmt.Errorf("%w: %d", ErrSegmentBadFtyp, ftypSize)
	}

	return findMoofBox(f, ftypSize, info.Size())
}

// findMoofBox scans MP4 boxes starting at ftypSize until it finds a "moof"
// box or reaches the end of the file.
func findMoofBox(f *os.File, ftypSize, fileSize int64) error {
	var buf [8]byte

	for pos := ftypSize; pos < fileSize; {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return fmt.Errorf("seek to offset %d: %w", pos, err)
		}

		if _, err := io.ReadFull(f, buf[:]); err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}

			return fmt.Errorf("read box header at offset %d: %w", pos, err)
		}

		size := int64(binary.BigEndian.Uint32(buf[0:4]))
		typ := string(buf[4:8])

		if typ == "moof" {
			return nil
		}

		if size < 8 {
			return fmt.Errorf("%w: %d at offset %d", ErrSegmentBadBoxSize, size, pos)
		}

		pos += size
	}

	return ErrSegmentNoMoof
}
