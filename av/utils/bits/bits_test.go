package bits

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type shortReadReader struct {
	data []byte
	pos  int
	max  int
}

func (r *shortReadReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	n := len(p)
	if n > r.max {
		n = r.max
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}

	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n

	return n, nil
}

type errReader struct {
	err error
}

func (r errReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

func TestWriterAndReaderRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := &Writer{W: &buf}

	if err := w.WriteBits64(0b101, 3); err != nil {
		t.Fatalf("WriteBits64(3): %v", err)
	}

	if err := w.WriteBits64(0b11111, 5); err != nil {
		t.Fatalf("WriteBits64(5): %v", err)
	}

	if err := w.WriteBits64(0xAB, 8); err != nil {
		t.Fatalf("WriteBits64(8): %v", err)
	}

	if err := w.FlushBits(); err != nil {
		t.Fatalf("FlushBits: %v", err)
	}

	want := []byte{0xBF, 0xAB}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("encoded bytes: got %x, want %x", buf.Bytes(), want)
	}

	r := &Reader{R: bytes.NewReader(buf.Bytes())}
	got3, err := r.ReadBits64(3)
	if err != nil || got3 != 0b101 {
		t.Fatalf("ReadBits64(3): got (%d,%v), want (5,nil)", got3, err)
	}

	got5, err := r.ReadBits64(5)
	if err != nil || got5 != 0b11111 {
		t.Fatalf("ReadBits64(5): got (%d,%v), want (31,nil)", got5, err)
	}

	got8, err := r.ReadBits64(8)
	if err != nil || got8 != 0xAB {
		t.Fatalf("ReadBits64(8): got (%d,%v), want (171,nil)", got8, err)
	}
}

func TestWriterFlushPadsPartialByte(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := &Writer{W: &buf}

	if err := w.WriteBits64(0b101, 3); err != nil {
		t.Fatalf("WriteBits64: %v", err)
	}

	if err := w.FlushBits(); err != nil {
		t.Fatalf("FlushBits: %v", err)
	}

	if got := buf.Bytes(); !bytes.Equal(got, []byte{0xA0}) {
		t.Fatalf("partial-byte flush: got %x, want a0", got)
	}
}

func TestWriterCrosses64BitBoundary(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := &Writer{W: &buf}

	if err := w.WriteBits64(^uint64(0), 64); err != nil {
		t.Fatalf("WriteBits64(64): %v", err)
	}

	if err := w.WriteBits64(0xAB, 8); err != nil {
		t.Fatalf("WriteBits64(overflow): %v", err)
	}

	if err := w.FlushBits(); err != nil {
		t.Fatalf("FlushBits: %v", err)
	}

	got := buf.Bytes()
	if len(got) != 9 {
		t.Fatalf("cross-boundary len: got %d, want 9", len(got))
	}

	if !bytes.Equal(got[:8], bytes.Repeat([]byte{0xFF}, 8)) || got[8] != 0xAB {
		t.Fatalf("cross-boundary payload: got %x", got)
	}
}

func TestReaderReadUnalignedBytes(t *testing.T) {
	t.Parallel()

	r := &Reader{R: bytes.NewReader([]byte{0xAB, 0xCD, 0xEF})}

	prefix, err := r.ReadBits(4)
	if err != nil || prefix != 0xA {
		t.Fatalf("ReadBits(4): got (%d,%v), want (10,nil)", prefix, err)
	}

	out := make([]byte, 2)
	n, err := r.Read(out)
	if err != nil {
		t.Fatalf("Read(2): %v", err)
	}

	if n != 2 || !bytes.Equal(out, []byte{0xBC, 0xDE}) {
		t.Fatalf("unaligned Read(2): n=%d out=%x", n, out)
	}

	n, err = r.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("final Read expected EOF, got n=%d err=%v", n, err)
	}
}

func TestReadBits64ShortReadAndUnderlyingError(t *testing.T) {
	t.Parallel()

	r := &Reader{R: &shortReadReader{data: []byte{0x12, 0x34}, max: 1}}
	if _, err := r.ReadBits64(16); !errors.Is(err, io.EOF) {
		t.Fatalf("short read expected EOF, got %v", err)
	}

	boom := errors.New("boom")
	r = &Reader{R: errReader{err: boom}}
	if _, err := r.ReadBits64(8); !errors.Is(err, boom) {
		t.Fatalf("expected underlying error, got %v", err)
	}
}

