package pio

import (
	"testing"
)

func TestNumericReadWriteRoundTrips(t *testing.T) {
	t.Parallel()

	{
		b := make([]byte, 1)
		PutU8(b, 0xAB)
		if got := U8(b); got != 0xAB {
			t.Fatalf("U8 round-trip: got 0x%x", got)
		}
	}

	{
		b := make([]byte, 2)
		PutU16BE(b, 0xABCD)
		if got := U16BE(b); got != 0xABCD {
			t.Fatalf("U16BE round-trip: got 0x%x", got)
		}

		PutI16BE(b, -2)
		if got := I16BE(b); got != -2 {
			t.Fatalf("I16BE round-trip: got %d", got)
		}
	}

	{
		b := make([]byte, 3)
		PutU24BE(b, 0x00A1B2)
		if got := U24BE(b); got != 0x00A1B2 {
			t.Fatalf("U24BE round-trip: got 0x%x", got)
		}

		PutI24BE(b, -2)
		if got := I24BE(b); got != -2 {
			t.Fatalf("I24BE round-trip: got %d", got)
		}
	}

	{
		b := make([]byte, 4)
		PutU32BE(b, 0xA1B2C3D4)
		if got := U32BE(b); got != 0xA1B2C3D4 {
			t.Fatalf("U32BE round-trip: got 0x%x", got)
		}

		PutI32BE(b, -2)
		if got := I32BE(b); got != -2 {
			t.Fatalf("I32BE round-trip: got %d", got)
		}

		PutU32LE(b, 0x11223344)
		if got := U32LE(b); got != 0x11223344 {
			t.Fatalf("U32LE round-trip: got 0x%x", got)
		}
	}

	{
		b := make([]byte, 5)
		PutU40BE(b, 0x0102030405)
		if got := U40BE(b); got != 0x0102030405 {
			t.Fatalf("U40BE round-trip: got 0x%x", got)
		}
	}

	{
		b := make([]byte, 6)
		PutU48BE(b, 0x010203040506)
		want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
		for i := range want {
			if b[i] != want[i] {
				t.Fatalf("PutU48BE byte %d: got 0x%x want 0x%x", i, b[i], want[i])
			}
		}
	}

	{
		b := make([]byte, 8)
		PutU64BE(b, 0x0102030405060708)
		if got := U64BE(b); got != 0x0102030405060708 {
			t.Fatalf("U64BE round-trip: got 0x%x", got)
		}

		PutI64BE(b, -2)
		if got := I64BE(b); got != -2 {
			t.Fatalf("I64BE round-trip: got %d", got)
		}
	}
}

func TestVecHelpers(t *testing.T) {
	t.Parallel()

	in := [][]byte{{0, 1}, {2, 3, 4}, {5}}
	if got := VecLen(in); got != 6 {
		t.Fatalf("VecLen: got %d, want 6", got)
	}

	out := VecSlice(in, 1, 5)
	if len(out) != 2 {
		t.Fatalf("VecSlice len: got %d, want 2", len(out))
	}

	if string(out[0]) != string([]byte{1}) || string(out[1]) != string([]byte{2, 3, 4}) {
		t.Fatalf("VecSlice [1:5] unexpected: %#v", out)
	}

	out = VecSlice(in, -3, 3) // negative start clamps to zero.
	if len(out) != 2 || string(out[0]) != string([]byte{0, 1}) || string(out[1]) != string([]byte{2}) {
		t.Fatalf("VecSlice negative-start unexpected: %#v", out)
	}

	out = VecSlice(in, 2, -1) // e < 0 means until end.
	if len(out) != 2 || string(out[0]) != string([]byte{2, 3, 4}) || string(out[1]) != string([]byte{5}) {
		t.Fatalf("VecSlice to-end unexpected: %#v", out)
	}

	dst := make([][]byte, 4)
	n := VecSliceTo(in, dst, 1, 6)
	if n != 3 {
		t.Fatalf("VecSliceTo count: got %d, want 3", n)
	}
}

func TestVecSlicePanics(t *testing.T) {
	t.Parallel()

	in := [][]byte{{0, 1}, {2, 3, 4}, {5}}

	assertPanics(t, func() { _ = VecSlice(in, 4, 3) })  // start > end
	assertPanics(t, func() { _ = VecSlice(in, 99, -1) }) // start out of range
	assertPanics(t, func() { _ = VecSlice(in, 0, 99) })  // end out of range
}

func TestRecommendBufioSize(t *testing.T) {
	t.Parallel()

	if RecommendBufioSize != 64*1024 {
		t.Fatalf("RecommendBufioSize: got %d, want %d", RecommendBufioSize, 64*1024)
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

