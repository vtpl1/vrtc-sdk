package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec"
	"github.com/vtpl1/vrtc-sdk/av/codec/aacparser"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

func TestResolveControlURL(t *testing.T) {
	base, err := url.Parse("rtsp://user:pass@10.0.0.10:554/stream")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}

	tests := []struct {
		name    string
		control string
		want    string
	}{
		{
			name:    "relative track id",
			control: "trackID=1",
			want:    "rtsp://user:pass@10.0.0.10:554/stream/trackID=1",
		},
		{
			name:    "absolute path",
			control: "/live/trackID=0",
			want:    "rtsp://user:pass@10.0.0.10:554/live/trackID=0",
		},
		{
			name:    "empty uses base",
			control: "",
			want:    "rtsp://user:pass@10.0.0.10:554/stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveControlURL(base, tt.control)
			if got != tt.want {
				t.Fatalf("resolveControlURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestH264RTPDecoderSingleNALU(t *testing.T) {
	d := &h264RTPDecoder{}

	pkt := &rtp.Packet{
		Header:  rtp.Header{Timestamp: 1000, Marker: true},
		Payload: []byte{0x65, 0x88, 0x84}, // IDR
	}

	nalus, err := d.Decode(pkt)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if len(nalus) != 1 {
		t.Fatalf("len(nalus) = %d, want 1", len(nalus))
	}

	if len(nalus[0]) != len(pkt.Payload) {
		t.Fatalf("payload size = %d, want %d", len(nalus[0]), len(pkt.Payload))
	}
}

func TestH264RTPDecoderFUA(t *testing.T) {
	d := &h264RTPDecoder{}

	// Reassemble IDR (type=5) across FU-A fragments.
	p1 := &rtp.Packet{
		Header: rtp.Header{SequenceNumber: 10, Timestamp: 9000, Marker: false},
		Payload: []byte{
			0x7c,       // FU indicator (type 28)
			0x85,       // start=1, end=0, type=5
			0xaa, 0xbb, // fragment bytes
		},
	}

	p2 := &rtp.Packet{
		Header: rtp.Header{SequenceNumber: 11, Timestamp: 9000, Marker: true},
		Payload: []byte{
			0x7c,       // FU indicator
			0x45,       // start=0, end=1, type=5
			0xcc, 0xdd, // fragment bytes
		},
	}

	if _, err := d.Decode(p1); err != errNeedMorePackets {
		t.Fatalf("first fragment error = %v, want errNeedMorePackets", err)
	}

	nalus, err := d.Decode(p2)
	if err != nil {
		t.Fatalf("Decode() second fragment error = %v", err)
	}

	if len(nalus) != 1 {
		t.Fatalf("len(nalus) = %d, want 1", len(nalus))
	}

	want := []byte{0x65, 0xaa, 0xbb, 0xcc, 0xdd}
	if string(nalus[0]) != string(want) {
		t.Fatalf("decoded nalu = %x, want %x", nalus[0], want)
	}
}

func TestAACRTPDecoderSingleAU(t *testing.T) {
	d, err := newAACRTPDecoder(map[string]string{
		"sizelength":       "13",
		"indexlength":      "3",
		"indexdeltalength": "3",
	})
	if err != nil {
		t.Fatalf("newAACRTPDecoder() error = %v", err)
	}

	pkt := &rtp.Packet{
		Header: rtp.Header{Timestamp: 1600},
		Payload: []byte{
			0x00, 0x10, // AU-headers-length = 16 bits
			0x00, 0x20, // size = 4, index = 0
			0x11, 0x22, 0x33, 0x44,
		},
	}

	aus, err := d.Decode(pkt)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if len(aus) != 1 {
		t.Fatalf("len(aus) = %d, want 1", len(aus))
	}

	want := []byte{0x11, 0x22, 0x33, 0x44}
	if !bytes.Equal(aus[0], want) {
		t.Fatalf("au[0] = %x, want %x", aus[0], want)
	}
}

func TestNewTrackPCMUDecode(t *testing.T) {
	tr, err := newTrack(1, codec.RTSPAudioCodecData{
		AudioCodec: pcm.PCMMulawCodecData{
			Typ:        av.PCM_MULAW,
			SmplFormat: av.S16,
			SmplRate:   8000,
			ChLayout:   av.ChMono,
		},
		ControlURL: "trackID=1",
		ClockRate:  8000,
	})
	if err != nil {
		t.Fatalf("newTrack() error = %v", err)
	}

	pkts, err := tr.decodeRTP(&rtp.Packet{
		Header:  rtp.Header{Timestamp: 0},
		Payload: bytes.Repeat([]byte{0xff}, 160),
	})
	if err != nil {
		t.Fatalf("decodeRTP() error = %v", err)
	}

	if len(pkts) != 1 {
		t.Fatalf("len(pkts) = %d, want 1", len(pkts))
	}

	if pkts[0].CodecType != av.PCM_MULAW {
		t.Fatalf("pkts[0].CodecType = %v, want %v", pkts[0].CodecType, av.PCM_MULAW)
	}

	if pkts[0].Duration != 20*time.Millisecond {
		t.Fatalf("pkts[0].Duration = %v, want 20ms", pkts[0].Duration)
	}
}

func TestNewTrackAACDecode(t *testing.T) {
	base, err := aacparser.NewCodecDataFromMPEG4AudioConfigBytes([]byte{0x14, 0x08})
	if err != nil {
		t.Fatalf("NewCodecDataFromMPEG4AudioConfigBytes() error = %v", err)
	}

	tr, err := newTrack(2, codec.RTSPAudioCodecData{
		AudioCodec:  base,
		ControlURL:  "trackID=2",
		ClockRate:   16000,
		PayloadType: 96,
		Fmtp: map[string]string{
			"sizelength":       "13",
			"indexlength":      "3",
			"indexdeltalength": "3",
		},
	})
	if err != nil {
		t.Fatalf("newTrack() error = %v", err)
	}

	pkts, err := tr.decodeRTP(&rtp.Packet{
		Header: rtp.Header{Timestamp: 0},
		Payload: []byte{
			0x00, 0x10,
			0x00, 0x20,
			0xaa, 0xbb, 0xcc, 0xdd,
		},
	})
	if err != nil {
		t.Fatalf("decodeRTP() error = %v", err)
	}

	if len(pkts) != 1 {
		t.Fatalf("len(pkts) = %d, want 1", len(pkts))
	}

	if pkts[0].CodecType != av.AAC {
		t.Fatalf("pkts[0].CodecType = %v, want %v", pkts[0].CodecType, av.AAC)
	}

	if !bytes.Equal(pkts[0].Data, []byte{0xaa, 0xbb, 0xcc, 0xdd}) {
		t.Fatalf("pkts[0].Data = %x", pkts[0].Data)
	}
}

func TestAppendPendingLockedMarksDiscontinuity(t *testing.T) {
	d := &Demuxer{pendingDiscontinuity: true}
	d.appendPendingLocked([]av.Packet{
		{CodecType: av.H264},
		{CodecType: av.H264},
	})

	if len(d.pending) != 2 {
		t.Fatalf("len(d.pending) = %d, want 2", len(d.pending))
	}

	if !d.pending[0].IsDiscontinuity {
		t.Fatal("first pending packet missing discontinuity flag")
	}

	if d.pending[1].IsDiscontinuity {
		t.Fatal("second pending packet unexpectedly marked discontinuity")
	}
}

func TestCarryTrackState(t *testing.T) {
	prev, err := newTrack(0, codec.RTSPAudioCodecData{
		AudioCodec: pcm.PCMMulawCodecData{
			Typ:        av.PCM_MULAW,
			SmplFormat: av.S16,
			SmplRate:   8000,
			ChLayout:   av.ChMono,
		},
		ControlURL: "trackID=0",
		ClockRate:  8000,
	})
	if err != nil {
		t.Fatalf("newTrack(prev) error = %v", err)
	}

	prev.haveLastDTS = true
	prev.lastDTS = 2 * time.Second
	prev.lastDur = 20 * time.Millisecond

	next, err := newTrack(0, codec.RTSPAudioCodecData{
		AudioCodec: pcm.PCMMulawCodecData{
			Typ:        av.PCM_MULAW,
			SmplFormat: av.S16,
			SmplRate:   8000,
			ChLayout:   av.ChMono,
		},
		ControlURL: "trackID=0",
		ClockRate:  8000,
	})
	if err != nil {
		t.Fatalf("newTrack(next) error = %v", err)
	}

	carryTrackState([]*rtspTrack{prev}, []*rtspTrack{next})

	if got := next.decodeDTS(1234); got != 2020*time.Millisecond {
		t.Fatalf("next.decodeDTS() = %v, want 2020ms", got)
	}
}

func TestDemuxerPauseResume(t *testing.T) {
	conn := &scriptedConn{
		readBuf: bytes.NewBufferString(
			"RTSP/1.0 200 OK\r\nCSeq: 1\r\nSession: sess\r\n\r\n" +
				"RTSP/1.0 200 OK\r\nCSeq: 2\r\nSession: sess\r\n\r\n",
		),
	}

	base, err := url.Parse("rtsp://example.com/stream")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}

	d := &Demuxer{
		timeNow:      time.Now,
		started:      true,
		conn:         conn,
		reader:       bufio.NewReader(conn),
		baseURL:      base,
		sessionID:    "sess",
		requestID:    1,
		keepAliveFor: defaultKeepAlivePeriod,
	}

	if err := d.Pause(context.Background()); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}

	if !d.IsPaused() {
		t.Fatal("demuxer should be paused after Pause()")
	}

	if err := d.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	if d.IsPaused() {
		t.Fatal("demuxer should not be paused after Resume()")
	}

	if !d.pendingDiscontinuity {
		t.Fatal("resume should mark pending discontinuity")
	}

	writes := conn.writeBuf.String()
	if !strings.Contains(writes, "PAUSE rtsp://example.com/stream RTSP/1.0") {
		t.Fatalf("missing PAUSE request in writes: %q", writes)
	}

	if !strings.Contains(writes, "PLAY rtsp://example.com/stream RTSP/1.0") {
		t.Fatalf("missing PLAY request in writes: %q", writes)
	}
}

func TestWaitIfPausedContextCancel(t *testing.T) {
	d := &Demuxer{}
	d.paused.Store(true)
	d.pauseCh = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := d.waitIfPaused(ctx)
	if err == nil {
		t.Fatal("waitIfPaused() returned nil, want context error")
	}
}

type scriptedConn struct {
	readBuf  *bytes.Buffer
	writeBuf bytes.Buffer
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if c.readBuf == nil {
		return 0, io.EOF
	}

	return c.readBuf.Read(p)
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	return c.writeBuf.Write(p)
}

func (c *scriptedConn) Close() error { return nil }

func (c *scriptedConn) LocalAddr() net.Addr { return dummyAddr("local") }

func (c *scriptedConn) RemoteAddr() net.Addr { return dummyAddr("remote") }

func (c *scriptedConn) SetDeadline(time.Time) error { return nil }

func (c *scriptedConn) SetReadDeadline(time.Time) error { return nil }

func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (d dummyAddr) Network() string { return string(d) }

func (d dummyAddr) String() string { return string(d) }
