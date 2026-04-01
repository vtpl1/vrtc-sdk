package rtsp

import (
	"net/url"
	"testing"

	"github.com/pion/rtp"
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
