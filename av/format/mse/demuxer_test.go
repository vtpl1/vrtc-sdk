package mse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

// compile-time check: *Demuxer satisfies av.DemuxCloser.
var _ av.DemuxCloser = (*Demuxer)(nil)

func joinWrites(writes [][]byte) []byte {
	var out bytes.Buffer

	for _, b := range writes {
		out.Write(b)
	}

	return out.Bytes()
}

func TestDemuxerRoundTrip(t *testing.T) {
	t.Parallel()

	binaryWriter := &captureWriteCloser{}
	jsonWriter := &captureWriteCloser{}

	mux, err := NewFromWriters(binaryWriter, jsonWriter)
	if err != nil {
		t.Fatalf("NewFromWriters: %v", err)
	}

	streams := []av.Stream{{Idx: 0, Codec: mustAACCodecData(t)}}
	ctx := context.Background()

	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	pkt1 := av.Packet{
		Idx:       0,
		CodecType: av.AAC,
		DTS:       0,
		Duration:  20 * time.Millisecond,
		Data:      []byte{0x11, 0x22, 0x33},
	}
	pkt2 := av.Packet{
		Idx:       0,
		CodecType: av.AAC,
		DTS:       20 * time.Millisecond,
		Duration:  20 * time.Millisecond,
		Data:      []byte{0x44, 0x55},
		Analytics: &av.FrameAnalytics{SiteID: 7, ChannelID: 3},
	}

	if err := mux.WritePacket(ctx, pkt1); err != nil {
		t.Fatalf("WritePacket(pkt1): %v", err)
	}

	if err := mux.WritePacket(ctx, pkt2); err != nil {
		t.Fatalf("WritePacket(pkt2): %v", err)
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	dmx := NewDemuxer(bytes.NewReader(joinWrites(binaryWriter.writes)))

	gotStreams, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(gotStreams) != 1 || gotStreams[0].Codec.Type() != av.AAC {
		t.Fatalf("unexpected streams: %#v", gotStreams)
	}

	got1, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket(1): %v", err)
	}

	got2, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket(2): %v", err)
	}

	if _, err := dmx.ReadPacket(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadPacket(3): got %v, want io.EOF", err)
	}

	if got1.DTS != pkt1.DTS || got1.Duration != pkt1.Duration || !bytes.Equal(got1.Data, pkt1.Data) {
		t.Fatalf("packet 1 mismatch: got=%+v want=%+v", got1, pkt1)
	}

	if got2.DTS != pkt2.DTS || got2.Duration != pkt2.Duration || !bytes.Equal(got2.Data, pkt2.Data) {
		t.Fatalf("packet 2 mismatch: got=%+v want=%+v", got2, pkt2)
	}

	if got2.Analytics == nil || got2.Analytics.SiteID != 7 || got2.Analytics.ChannelID != 3 {
		t.Fatalf("packet 2 analytics mismatch: %#v", got2.Analytics)
	}
}

func TestDemuxerRoundTripWithPCMTranscode(t *testing.T) {
	t.Parallel()

	binaryWriter := &captureWriteCloser{}
	jsonWriter := &captureWriteCloser{}

	mux, err := NewFromWriters(binaryWriter, jsonWriter)
	if err != nil {
		t.Fatalf("NewFromWriters: %v", err)
	}

	streams := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMMulawCodecData{
				Typ: av.PCM_MULAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
	}
	ctx := context.Background()

	if err := mux.WriteHeader(ctx, streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if err := mux.WritePacket(ctx, av.Packet{
		Idx:       0,
		CodecType: av.PCM_MULAW,
		DTS:       0,
		Duration:  20 * time.Millisecond,
		Data:      []byte{0x00, 0x7f, 0xff, 0x80},
	}); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if err := mux.WriteTrailer(ctx, nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	dmx := NewDemuxer(bytes.NewReader(joinWrites(binaryWriter.writes)))

	gotStreams, err := dmx.GetCodecs(ctx)
	if err != nil {
		t.Fatalf("GetCodecs: %v", err)
	}

	if len(gotStreams) != 1 || gotStreams[0].Codec.Type() != av.FLAC {
		t.Fatalf("unexpected streams: %#v", gotStreams)
	}

	got, err := dmx.ReadPacket(ctx)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}

	if got.CodecType != av.FLAC {
		t.Fatalf("codec type: got %v, want FLAC", got.CodecType)
	}

	if got.Duration != 20*time.Millisecond {
		t.Fatalf("duration: got %v, want %v", got.Duration, 20*time.Millisecond)
	}

	if len(got.Data) == 0 {
		t.Fatal("empty FLAC payload")
	}
}

type closeReader struct {
	io.Reader
	err    error
	closed int
}

func (r *closeReader) Close() error {
	r.closed++

	return r.err
}

func TestDemuxerCloseDelegates(t *testing.T) {
	t.Parallel()

	expected := errors.New("close failed")
	r := &closeReader{
		Reader: bytes.NewReader(nil),
		err:    expected,
	}

	dmx := NewDemuxer(r)

	if err := dmx.Close(); !errors.Is(err, expected) {
		t.Fatalf("Close: got %v, want %v", err, expected)
	}

	if r.closed != 1 {
		t.Fatalf("close call count: got %d, want 1", r.closed)
	}
}
