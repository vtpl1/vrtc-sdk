package pcm

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

func TestFLACHeaderLayout(t *testing.T) {
	t.Parallel()

	const sampleRate = uint32(48000)
	h := FLACHeader(true, sampleRate)

	if len(h) != 42 {
		t.Fatalf("header len: got %d, want 42", len(h))
	}

	if string(h[:4]) != "fLaC" {
		t.Fatalf("magic: got %q, want fLaC", string(h[:4]))
	}

	if h[4] != 0x80 {
		t.Fatalf("metadata header byte: got 0x%02x, want 0x80", h[4])
	}

	if h[7] != 0x22 {
		t.Fatalf("metadata block len byte: got 0x%02x, want 0x22", h[7])
	}

	if got := binary.BigEndian.Uint16(h[8:10]); got != 32768 {
		t.Fatalf("block size min: got %d, want 32768", got)
	}

	if got := binary.BigEndian.Uint16(h[10:12]); got != 32768 {
		t.Fatalf("block size max: got %d, want 32768", got)
	}

	gotSR := uint32(h[18])<<12 | uint32(h[19])<<4 | uint32(h[20])>>4
	if gotSR != sampleRate {
		t.Fatalf("sample rate encoded: got %d, want %d", gotSR, sampleRate)
	}
}

func TestSTREAMINFOBlock(t *testing.T) {
	t.Parallel()

	cd := NewFLACCodecData(av.PCM_MULAW, 8000, av.ChMono)
	block := cd.STREAMINFOBlock()
	full := FLACHeader(false, 8000)

	if len(block) != 38 {
		t.Fatalf("streaminfo len: got %d, want 38", len(block))
	}

	if string(block) != string(full[4:]) {
		t.Fatal("streaminfo block mismatch against FLACHeader payload")
	}
}

func TestFLACCodecDataPacketDuration(t *testing.T) {
	t.Parallel()

	cd := NewFLACCodecData(av.PCM_MULAW, 8000, av.ChMono)

	valid := []byte{0, 0, 0, 0, 'A', 0x1f, 0x3f, 0x00} // blockSize=7999+1=8000 => 1s at 8kHz
	dur, err := cd.PacketDuration(valid)
	if err != nil {
		t.Fatalf("PacketDuration(valid): %v", err)
	}

	if dur != time.Second {
		t.Fatalf("PacketDuration(valid): got %v, want 1s", dur)
	}

	short := []byte{0, 1, 2, 3, 4, 5, 6}
	if _, err := cd.PacketDuration(short); err == nil {
		t.Fatal("PacketDuration(short): expected error")
	}

	// 4-byte UTF-8 rune starts at index 4, so offset+2 exceeds packet len and
	// should trigger the truncation error path.
	truncated := []byte{0, 0, 0, 0, 0xF0, 0x90, 0x80, 0x80}
	if _, err := cd.PacketDuration(truncated); err == nil {
		t.Fatal("PacketDuration(truncated): expected error")
	}
}

func TestFLACEncoderUnsupportedSampleRate(t *testing.T) {
	t.Parallel()

	if enc := FLACEncoder(av.PCM, 12345); enc != nil {
		t.Fatal("expected nil encoder for unsupported sample rate")
	}
}

func TestFLACEncoderDurationRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		codec    av.CodecType
		inputLen int
		samples  int
	}{
		{name: "mulaw", codec: av.PCM_MULAW, inputLen: 80, samples: 80},
		{name: "alaw", codec: av.PCM_ALAW, inputLen: 80, samples: 80},
		{name: "pcm-be", codec: av.PCM, inputLen: 80, samples: 40},
		{name: "pcm-le", codec: av.PCML, inputLen: 80, samples: 80},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := FLACEncoder(tc.codec, 8000)
			if enc == nil {
				t.Fatal("encoder is nil")
			}

			in := make([]byte, tc.inputLen)
			for i := range in {
				in[i] = byte(i)
			}

			out := enc(in)
			if len(out) == 0 {
				t.Fatal("encoded frame is empty")
			}

			cd := NewFLACCodecData(tc.codec, 8000, av.ChMono)
			dur, err := cd.PacketDuration(out)
			if err != nil {
				t.Fatalf("PacketDuration(encoded): %v", err)
			}

			want := time.Duration(tc.samples) * time.Second / 8000
			if dur != want {
				t.Fatalf("duration mismatch: got %v, want %v", dur, want)
			}
		})
	}
}
