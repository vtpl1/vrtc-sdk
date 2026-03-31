package mse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

type captureWriteCloser struct {
	writes   [][]byte
	writeErr error
	closeErr error
	closed   int
}

func (w *captureWriteCloser) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}

	cp := make([]byte, len(p))
	copy(cp, p)
	w.writes = append(w.writes, cp)

	return len(p), nil
}

func (w *captureWriteCloser) Close() error {
	w.closed++

	return w.closeErr
}

func mustAACCodecData(t *testing.T) aacparser.CodecData {
	t.Helper()

	cfg := aacparser.MPEG4AudioConfig{
		ObjectType:    aacparser.AOT_AAC_LC,
		SampleRate:    48000,
		ChannelLayout: av.ChMono,
	}

	cd, err := aacparser.NewCodecDataFromMPEG4AudioConfig(cfg)
	if err != nil {
		t.Fatalf("NewCodecDataFromMPEG4AudioConfig: %v", err)
	}

	return cd
}

type taggedCodec struct {
	codecType av.CodecType
	tag       string
}

func (c taggedCodec) Type() av.CodecType { return c.codecType }
func (c taggedCodec) Tag() string        { return c.tag }

func TestTranscodePCM(t *testing.T) {
	t.Parallel()

	streams := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMMulawCodecData{
				Typ: av.PCM_MULAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
		{
			Idx: 1,
			Codec: pcm.PCMAlawCodecData{
				Typ: av.PCM_ALAW, SmplFormat: av.S16, SmplRate: 12345, ChLayout: av.ChMono,
			},
		},
		{Idx: 2, Codec: pcm.PCMCodecData{Typ: av.PCM, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono}},
	}

	out, encoders := transcodePCM(streams)

	if out[0].Codec.Type() != av.FLAC {
		t.Fatalf("mulaw stream type after transcode: got %v, want FLAC", out[0].Codec.Type())
	}

	if _, ok := encoders[0]; !ok {
		t.Fatal("missing mulaw encoder")
	}

	if out[1].Codec.Type() != av.PCM_ALAW {
		t.Fatalf("unsupported sample-rate stream must stay PCM_ALAW, got %v", out[1].Codec.Type())
	}

	if _, ok := encoders[1]; ok {
		t.Fatal("unexpected encoder for unsupported sample-rate stream")
	}

	if out[2].Codec.Type() != av.PCM {
		t.Fatalf("non-g711 stream changed unexpectedly: got %v", out[2].Codec.Type())
	}
}

func TestBuildCodecString(t *testing.T) {
	t.Parallel()

	if got := buildCodecString(nil); got != `video/mp4; codecs=""` {
		t.Fatalf("empty codec string: got %q", got)
	}

	streams := []av.Stream{
		{Idx: 0, Codec: taggedCodec{codecType: av.H264, tag: "avc1.test"}},
		{Idx: 1, Codec: pcm.NewFLACCodecData(av.PCM_MULAW, 8000, av.ChMono)},
	}
	if got := buildCodecString(streams); got != `video/mp4; codecs="avc1.test,flac"` {
		t.Fatalf("codec string: got %q", got)
	}
}

func TestBroadcastFactoryErrorsAndCloseErrors(t *testing.T) {
	t.Parallel()

	fErr := errors.New("factory")
	m, err := NewFromFactories(
		func() (io.WriteCloser, error) { return nil, fErr },
		func() (io.WriteCloser, error) { return nil, fErr },
	)
	if err != nil {
		t.Fatalf("NewFromFactories: %v", err)
	}

	err = m.broadcast(outFrame{kind: messageBinary, data: []byte{1}})
	if !errors.Is(err, ErrFailedToCreateBinaryWriter) {
		t.Fatalf("binary factory error not wrapped as ErrFailedToCreateBinaryWriter: %v", err)
	}

	err = m.broadcast(outFrame{kind: messageText, data: []byte("x")})
	if !errors.Is(err, ErrFailedToCreateJSONWriter) {
		t.Fatalf("json factory error not wrapped as ErrFailedToCreateJSONWriter: %v", err)
	}

	m2, _ := NewFromFactories(
		func() (io.WriteCloser, error) { return nil, nil },
		func() (io.WriteCloser, error) { return nil, nil },
	)
	if err := m2.broadcast(outFrame{kind: messageBinary, data: []byte{1}}); !errors.Is(err, ErrFailedToCreateBinaryWriter) {
		t.Fatalf("nil binary writer: %v", err)
	}
	if err := m2.broadcast(outFrame{kind: messageText, data: []byte("x")}); !errors.Is(err, ErrFailedToCreateJSONWriter) {
		t.Fatalf("nil json writer: %v", err)
	}

	closeErr := errors.New("close")
	m3, _ := NewFromFactories(
		func() (io.WriteCloser, error) { return &captureWriteCloser{closeErr: closeErr}, nil },
		func() (io.WriteCloser, error) { return &captureWriteCloser{closeErr: closeErr}, nil },
	)
	if err := m3.broadcast(outFrame{kind: messageBinary, data: []byte{1}}); !errors.Is(err, closeErr) {
		t.Fatalf("binary close err: %v", err)
	}
	if err := m3.broadcast(outFrame{kind: messageText, data: []byte("x")}); !errors.Is(err, closeErr) {
		t.Fatalf("text close err: %v", err)
	}

	writeErr := errors.New("write")
	m4, _ := NewFromFactories(
		func() (io.WriteCloser, error) { return &captureWriteCloser{writeErr: writeErr}, nil },
		func() (io.WriteCloser, error) { return &captureWriteCloser{writeErr: writeErr}, nil },
	)
	if err := m4.broadcast(outFrame{kind: messageBinary, data: []byte{1}}); !errors.Is(err, writeErr) {
		t.Fatalf("binary write err: %v", err)
	}
	if err := m4.broadcast(outFrame{kind: messageText, data: []byte("x")}); !errors.Is(err, writeErr) {
		t.Fatalf("text write err: %v", err)
	}
}

func TestNewFromFactoriesWriteHeaderWritesAndCloses(t *testing.T) {
	t.Parallel()

	var binaryWriters []*captureWriteCloser
	var jsonWriters []*captureWriteCloser

	m, err := NewFromFactories(
		func() (io.WriteCloser, error) {
			w := &captureWriteCloser{}
			binaryWriters = append(binaryWriters, w)

			return w, nil
		},
		func() (io.WriteCloser, error) {
			w := &captureWriteCloser{}
			jsonWriters = append(jsonWriters, w)

			return w, nil
		},
	)
	if err != nil {
		t.Fatalf("NewFromFactories: %v", err)
	}

	streams := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMMulawCodecData{
				Typ: av.PCM_MULAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
	}
	if err := m.WriteHeader(context.Background(), streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	if len(jsonWriters) == 0 || len(binaryWriters) == 0 {
		t.Fatalf("expected both json and binary writer allocations, got json=%d binary=%d", len(jsonWriters), len(binaryWriters))
	}

	if got := string(jsonWriters[0].writes[0]); !strings.Contains(got, `"type":"mse"`) || !strings.Contains(got, "flac") {
		t.Fatalf("unexpected mse metadata frame: %s", got)
	}

	if len(binaryWriters[0].writes[0]) == 0 {
		t.Fatal("empty init segment write")
	}

	if jsonWriters[0].closed != 1 || binaryWriters[0].closed != 1 {
		t.Fatalf("factory writers must be closed once, got json=%d binary=%d", jsonWriters[0].closed, binaryWriters[0].closed)
	}
}

func TestWriteHeaderWritePacketWriteTrailerAndClose(t *testing.T) {
	t.Parallel()

	binaryWriter := &captureWriteCloser{}
	jsonWriter := &captureWriteCloser{}

	m, err := NewFromWriters(binaryWriter, jsonWriter)
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
	if err := m.WriteHeader(context.Background(), streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	select {
	case <-m.codecsReady:
	default:
		t.Fatal("codecsReady was not closed by WriteHeader")
	}

	if len(m.initSeg) == 0 {
		t.Fatal("init segment not cached")
	}

	if m.codecStr != `video/mp4; codecs="flac"` {
		t.Fatalf("codecStr: got %q", m.codecStr)
	}

	if len(m.streams) != 1 || m.streams[0].Codec.Type() != av.FLAC {
		t.Fatalf("transcoded stream state unexpected: %#v", m.streams)
	}

	initialJSONWrites := len(jsonWriter.writes)
	initialBinaryWrites := len(binaryWriter.writes)

	pkt := av.Packet{
		Idx:       0,
		CodecType: av.PCM_MULAW,
		Duration:  20 * time.Millisecond,
		Data:      []byte{0x00, 0x7f, 0xff, 0x80},
		Analytics: &av.FrameAnalytics{SiteID: 7, ChannelID: 3},
	}
	if err := m.WritePacket(context.Background(), pkt); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	if len(binaryWriter.writes) <= initialBinaryWrites {
		t.Fatal("WritePacket did not broadcast binary fragment")
	}

	if len(jsonWriter.writes) <= initialJSONWrites {
		t.Fatal("WritePacket did not broadcast analytics metadata")
	}

	foundAnalytics := false
	for _, msg := range jsonWriter.writes {
		if strings.Contains(string(msg), `"siteId":7`) {
			foundAnalytics = true
			break
		}
	}
	if !foundAnalytics {
		t.Fatal("analytics JSON frame was not broadcast")
	}

	if err := m.WriteTrailer(context.Background(), nil); err != nil {
		t.Fatalf("WriteTrailer: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close first call: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close second call: %v", err)
	}
}

func TestWriteCodecChangeNoOpWhenCodecStringUnchanged(t *testing.T) {
	t.Parallel()

	binaryWriter := &captureWriteCloser{}
	jsonWriter := &captureWriteCloser{}
	m, _ := NewFromWriters(binaryWriter, jsonWriter)

	streams := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMMulawCodecData{
				Typ: av.PCM_MULAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
	}
	if err := m.WriteHeader(context.Background(), streams); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	jsonCount := len(jsonWriter.writes)
	binCount := len(binaryWriter.writes)

	changed := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMAlawCodecData{
				Typ: av.PCM_ALAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
	}
	if err := m.WriteCodecChange(context.Background(), changed); err != nil {
		t.Fatalf("WriteCodecChange(no-op): %v", err)
	}

	if got := len(jsonWriter.writes); got != jsonCount {
		t.Fatalf("unexpected json writes on no-op codec change: got %d, want %d", got, jsonCount)
	}

	if got := len(binaryWriter.writes); got != binCount {
		t.Fatalf("unexpected binary writes on no-op codec change: got %d, want %d", got, binCount)
	}
}

func TestWriteCodecChangeBroadcastsOnActualCodecChange(t *testing.T) {
	t.Parallel()

	binaryWriter := &captureWriteCloser{}
	jsonWriter := &captureWriteCloser{}
	m, _ := NewFromWriters(binaryWriter, jsonWriter)

	initial := []av.Stream{{Idx: 0, Codec: mustAACCodecData(t)}}
	if err := m.WriteHeader(context.Background(), initial); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}

	jsonCount := len(jsonWriter.writes)
	binCount := len(binaryWriter.writes)

	changed := []av.Stream{
		{
			Idx: 0,
			Codec: pcm.PCMAlawCodecData{
				Typ: av.PCM_ALAW, SmplFormat: av.S16, SmplRate: 8000, ChLayout: av.ChMono,
			},
		},
	}

	if err := m.WriteCodecChange(context.Background(), changed); err != nil {
		t.Fatalf("WriteCodecChange(change): %v", err)
	}

	if len(jsonWriter.writes) <= jsonCount {
		t.Fatal("codec-change metadata frame not broadcast")
	}

	if len(binaryWriter.writes) <= binCount {
		t.Fatal("codec-change binary frame not broadcast")
	}

	if !strings.Contains(m.codecStr, "flac") {
		t.Fatalf("codecStr not updated to flac: %q", m.codecStr)
	}

	lastJSON := jsonWriter.writes[len(jsonWriter.writes)-1]
	var msg wsMessage
	if err := json.Unmarshal(lastJSON, &msg); err != nil {
		t.Fatalf("codec-change json frame invalid: %v", err)
	}

	if msg.Type != "mse" {
		t.Fatalf("unexpected message type: %q", msg.Type)
	}

	if !strings.Contains(msg.Value.(string), "flac") {
		t.Fatalf("expected flac codec string in codec-change message, got %v", msg.Value)
	}
}
