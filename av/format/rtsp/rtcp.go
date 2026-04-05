package rtsp

import (
	"encoding/binary"
	"fmt"
	"time"
)

const (
	rtcpVersion            = 2
	rtcpTypeSenderReport   = 200
	rtcpHeaderLength       = 4
	rtcpSenderReportLength = 24
	ntpUnixOffsetSeconds   = 2208988800
)

type senderReport struct {
	ssrc         uint32
	ntpTime      time.Time
	rtpTimestamp uint32
}

func parseSenderReports(payload []byte) ([]senderReport, error) {
	var reports []senderReport

	for len(payload) > 0 {
		if len(payload) < rtcpHeaderLength {
			return nil, errRTCPPacketTooShort
		}

		version := payload[0] >> 6
		if version != rtcpVersion {
			return nil, fmt.Errorf("%w: %d", errUnsupportedRTCPVersion, version)
		}

		packetType := payload[1]
		packetLengthWords := binary.BigEndian.Uint16(payload[2:4])

		packetLength := int(packetLengthWords+1) * 4
		if packetLength < rtcpHeaderLength || packetLength > len(payload) {
			return nil, errInvalidRTCPPacketLength
		}

		packet := payload[:packetLength]
		if packetType == rtcpTypeSenderReport {
			report, err := parseSenderReport(packet)
			if err != nil {
				return nil, err
			}

			reports = append(reports, report)
		}

		payload = payload[packetLength:]
	}

	return reports, nil
}

func parseSenderReport(packet []byte) (senderReport, error) {
	if len(packet) < rtcpHeaderLength+rtcpSenderReportLength {
		return senderReport{}, errRTCPSenderReportTooShort
	}

	body := packet[rtcpHeaderLength:]
	ntpSeconds := binary.BigEndian.Uint32(body[4:8])
	ntpFraction := binary.BigEndian.Uint32(body[8:12])

	return senderReport{
		ssrc:         binary.BigEndian.Uint32(body[0:4]),
		ntpTime:      ntpToTime(ntpSeconds, ntpFraction),
		rtpTimestamp: binary.BigEndian.Uint32(body[12:16]),
	}, nil
}

func ntpToTime(seconds, fraction uint32) time.Time {
	secs := int64(seconds) - ntpUnixOffsetSeconds
	nsecs := (int64(fraction) * int64(time.Second)) >> 32

	return time.Unix(secs, nsecs).UTC()
}
