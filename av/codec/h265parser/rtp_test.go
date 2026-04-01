package h265parser_test

import (
	"bytes"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/codec/h265parser"
)

func TestRTPParser_DoesNotEmitPrefixOnlyAccessUnit(t *testing.T) {
	var p h265parser.Parser

	prefixSEI := []byte{0x4E, 0x01, 0x80}
	firstSlice := []byte{0x02, 0x01, 0x80, 0xAA}
	secondSlice := []byte{0x02, 0x01, 0x80, 0xBB}

	if au, ready, err := p.PushRTP(prefixSEI); err != nil || ready || au != nil {
		t.Fatalf("PushRTP(prefixSEI) = (%v, %v, %v), want (nil, false, nil)", au, ready, err)
	}

	if au, ready, err := p.PushRTP(firstSlice); err != nil || ready || au != nil {
		t.Fatalf("PushRTP(firstSlice) = (%v, %v, %v), want (nil, false, nil)", au, ready, err)
	}

	au, ready, err := p.PushRTP(secondSlice)
	if err != nil {
		t.Fatalf("PushRTP(secondSlice) error: %v", err)
	}

	if !ready || au == nil {
		t.Fatal("expected completed access unit when second first-slice arrives")
	}

	if len(au.NALUs) != 2 {
		t.Fatalf("len(au.NALUs) = %d, want 2", len(au.NALUs))
	}

	if !bytes.Equal(au.NALUs[0], prefixSEI) {
		t.Fatalf("au.NALUs[0] mismatch")
	}

	if !bytes.Equal(au.NALUs[1], firstSlice) {
		t.Fatalf("au.NALUs[1] mismatch")
	}
}

func TestRTPParser_FlushDropsNonVCLTail(t *testing.T) {
	var p h265parser.Parser

	suffixSEI := []byte{0x50, 0x01, 0x80}

	if au, ready, err := p.PushRTP(suffixSEI); err != nil || ready || au != nil {
		t.Fatalf("PushRTP(suffixSEI) = (%v, %v, %v), want (nil, false, nil)", au, ready, err)
	}

	if au := p.Flush(); au != nil {
		t.Fatalf("Flush() = %v, want nil", au)
	}
}

func TestRTPParser_FlushKeepsVCLWithSuffixSEI(t *testing.T) {
	var p h265parser.Parser

	firstSlice := []byte{0x02, 0x01, 0x80, 0xAA}
	suffixSEI := []byte{0x50, 0x01, 0x80}

	if au, ready, err := p.PushRTP(firstSlice); err != nil || ready || au != nil {
		t.Fatalf("PushRTP(firstSlice) = (%v, %v, %v), want (nil, false, nil)", au, ready, err)
	}

	if au, ready, err := p.PushRTP(suffixSEI); err != nil || ready || au != nil {
		t.Fatalf("PushRTP(suffixSEI) = (%v, %v, %v), want (nil, false, nil)", au, ready, err)
	}

	au := p.Flush()
	if au == nil {
		t.Fatal("Flush() returned nil, want completed access unit")
	}

	if len(au.NALUs) != 2 {
		t.Fatalf("len(au.NALUs) = %d, want 2", len(au.NALUs))
	}

	if !bytes.Equal(au.NALUs[0], firstSlice) {
		t.Fatalf("au.NALUs[0] mismatch")
	}

	if !bytes.Equal(au.NALUs[1], suffixSEI) {
		t.Fatalf("au.NALUs[1] mismatch")
	}
}
