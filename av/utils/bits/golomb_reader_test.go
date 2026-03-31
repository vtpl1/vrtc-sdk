package bits

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func bitsToBytes(t *testing.T, bitString string) []byte {
	t.Helper()

	s := strings.ReplaceAll(bitString, " ", "")
	if len(s) == 0 {
		return nil
	}

	out := make([]byte, (len(s)+7)/8)
	for i := 0; i < len(s); i++ {
		if s[i] != '0' && s[i] != '1' {
			t.Fatalf("invalid bit char %q in %q", s[i], s)
		}

		if s[i] == '1' {
			out[i/8] |= 1 << uint(7-(i%8))
		}
	}

	return out
}

func TestGolombReadBitAndBits(t *testing.T) {
	t.Parallel()

	r := &GolombBitReader{R: bytes.NewReader([]byte{0b10110000})}

	b, err := r.ReadBit()
	if err != nil || b != 1 {
		t.Fatalf("ReadBit #1: got (%d,%v), want (1,nil)", b, err)
	}

	b, err = r.ReadBit()
	if err != nil || b != 0 {
		t.Fatalf("ReadBit #2: got (%d,%v), want (0,nil)", b, err)
	}

	v, err := r.ReadBits(3)
	if err != nil || v != 0b110 {
		t.Fatalf("ReadBits(3): got (%d,%v), want (6,nil)", v, err)
	}
}

func TestGolombReadBits32And64(t *testing.T) {
	t.Parallel()

	r := &GolombBitReader{R: bytes.NewReader([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01})}

	v32, err := r.ReadBits32(16)
	if err != nil || v32 != 0xDEAD {
		t.Fatalf("ReadBits32(16): got (0x%x,%v), want (0xDEAD,nil)", v32, err)
	}

	v64, err := r.ReadBits64(16)
	if err != nil || v64 != 0xBEEF {
		t.Fatalf("ReadBits64(16): got (0x%x,%v), want (0xBEEF,nil)", v64, err)
	}
}

func TestReadExponentialGolombCode(t *testing.T) {
	t.Parallel()

	// ue(0)=1, ue(1)=010, ue(2)=011, ue(3)=00100
	data := bitsToBytes(t, "1 010 011 00100")
	r := &GolombBitReader{R: bytes.NewReader(data)}

	for want := uint(0); want <= 3; want++ {
		got, err := r.ReadExponentialGolombCode()
		if err != nil || got != want {
			t.Fatalf("ReadExponentialGolombCode #%d: got (%d,%v), want (%d,nil)", want, got, err, want)
		}
	}
}

func TestReadSE(t *testing.T) {
	t.Parallel()

	// se values via ue mapping:
	// ue(0)->0  code=1
	// ue(1)->1  code=010
	// ue(2)->-1 code=011
	// ue(3)->2  code=00100
	data := bitsToBytes(t, "1 010 011 00100")
	r := &GolombBitReader{R: bytes.NewReader(data)}

	got, err := r.ReadSE()
	if err != nil || got != 0 {
		t.Fatalf("se #0: got (%d,%v), want (0,nil)", got, err)
	}

	got, err = r.ReadSE()
	if err != nil || got != 1 {
		t.Fatalf("se #1: got (%d,%v), want (1,nil)", got, err)
	}

	got, err = r.ReadSE()
	if err != nil || got != ^uint(0) {
		t.Fatalf("se #-1: got (%d,%v), want (%d,nil)", got, err, ^uint(0))
	}

	got, err = r.ReadSE()
	if err != nil || got != 2 {
		t.Fatalf("se #2: got (%d,%v), want (2,nil)", got, err)
	}
}

func TestGolombReadBitEOF(t *testing.T) {
	t.Parallel()

	r := &GolombBitReader{R: bytes.NewReader(nil)}
	if _, err := r.ReadBit(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadBit on empty stream: got %v, want EOF", err)
	}
}

