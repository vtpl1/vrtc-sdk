package rtsp

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/codec"
	"github.com/vtpl1/vrtc-sdk/av/codec/pcm"
)

func TestParseSenderReports(t *testing.T) {
	packet := makeSenderReportPacket(0x11223344, 2208988810, 0, 90000)

	reports, err := parseSenderReports(packet)
	if err != nil {
		t.Fatalf("parseSenderReports() error = %v", err)
	}

	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}

	if reports[0].ssrc != 0x11223344 {
		t.Fatalf("reports[0].ssrc = %#x, want %#x", reports[0].ssrc, uint32(0x11223344))
	}

	want := time.Unix(10, 0).UTC()
	if !reports[0].ntpTime.Equal(want) {
		t.Fatalf("reports[0].ntpTime = %v, want %v", reports[0].ntpTime, want)
	}

	if reports[0].rtpTimestamp != 90000 {
		t.Fatalf("reports[0].rtpTimestamp = %d, want 90000", reports[0].rtpTimestamp)
	}
}

func TestTrackHandleRTCPAndWallClock(t *testing.T) {
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

	tr.ssrc = 0x01020304

	if err := tr.handleRTCP(makeSenderReportPacket(tr.ssrc, 2208988805, 0, 16000)); err != nil {
		t.Fatalf("handleRTCP() error = %v", err)
	}

	pkts, err := tr.decodeRTP(&rtp.Packet{
		Header:  rtp.Header{SSRC: tr.ssrc, Timestamp: 24000},
		Payload: bytes.Repeat([]byte{0xff}, 160),
	})
	if err != nil {
		t.Fatalf("decodeRTP() error = %v", err)
	}

	if len(pkts) != 1 {
		t.Fatalf("len(pkts) = %d, want 1", len(pkts))
	}

	want := time.Unix(6, 0).UTC()
	if !pkts[0].WallClockTime.Equal(want) {
		t.Fatalf("pkts[0].WallClockTime = %v, want %v", pkts[0].WallClockTime, want)
	}
}

func makeSenderReportPacket(ssrc, ntpSeconds, ntpFraction, rtpTimestamp uint32) []byte {
	packet := make([]byte, 28)
	packet[0] = 0x80
	packet[1] = rtcpTypeSenderReport
	binary.BigEndian.PutUint16(packet[2:4], 6)
	binary.BigEndian.PutUint32(packet[4:8], ssrc)
	binary.BigEndian.PutUint32(packet[8:12], ntpSeconds)
	binary.BigEndian.PutUint32(packet[12:16], ntpFraction)
	binary.BigEndian.PutUint32(packet[16:20], rtpTimestamp)

	return packet
}
