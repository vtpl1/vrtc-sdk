package rtsp

import (
	"bytes"
	"fmt"

	"github.com/pion/rtp"
)

const (
	h264TypeFUA   = 28
	h264TypeSTAPA = 24

	maxH264AccessUnitSize = 4 * 1024 * 1024
	maxH264NALUsPerAU     = 512
)

// h264RTPDecoder reassembles H264 access units from RTP payloads.
type h264RTPDecoder struct {
	firstPacketReceived bool
	fragments           [][]byte
	fragmentsSize       int
	fragmentNextSeqNum  uint16

	// Some servers don't set Marker properly; split on timestamp changes too.
	frameBuffer          [][]byte
	frameBufferLen       int
	frameBufferSize      int
	frameBufferTimestamp uint32
}

// Decode returns a complete access unit (NALU list) when available.
func (d *h264RTPDecoder) Decode(pkt *rtp.Packet) ([][]byte, error) {
	nalus, err := d.decodeNALUs(pkt)
	if err != nil {
		return nil, err
	}

	l := len(nalus)

	if d.frameBuffer != nil && pkt.Timestamp != d.frameBufferTimestamp {
		ret := d.frameBuffer
		d.resetFrameBuffer()

		if err := d.addToFrameBuffer(nalus, l, pkt.Timestamp); err != nil {
			return nil, err
		}

		return ret, nil
	}

	if err := d.addToFrameBuffer(nalus, l, pkt.Timestamp); err != nil {
		return nil, err
	}

	if !pkt.Marker {
		return nil, errNeedMorePackets
	}

	ret := d.frameBuffer
	d.resetFrameBuffer()

	return ret, nil
}

func (d *h264RTPDecoder) resetFragments() {
	d.fragments = d.fragments[:0]
	d.fragmentsSize = 0
}

func (d *h264RTPDecoder) decodeNALUs(pkt *rtp.Packet) ([][]byte, error) {
	if len(pkt.Payload) < 1 {
		d.resetFragments()

		return nil, errH264PayloadTooShort
	}

	typ := pkt.Payload[0] & 0x1f

	var nalus [][]byte

	switch typ {
	case h264TypeFUA:
		if len(pkt.Payload) < 2 {
			d.resetFragments()

			return nil, errH264InvalidFUAPacket
		}

		start := pkt.Payload[1] >> 7
		end := (pkt.Payload[1] >> 6) & 0x01

		if start == 1 {
			d.resetFragments()

			nri := (pkt.Payload[0] >> 5) & 0x03
			naluType := pkt.Payload[1] & 0x1f
			d.fragmentsSize = len(pkt.Payload[1:])
			d.fragments = append(d.fragments, []byte{(nri << 5) | naluType}, pkt.Payload[2:])
			d.fragmentNextSeqNum = pkt.SequenceNumber + 1
			d.firstPacketReceived = true

			if end == 1 {
				nalus = splitNALUsAnnexB(joinFragments(d.fragments, d.fragmentsSize))
				d.resetFragments()

				break
			}

			return nil, errNeedMorePackets
		}

		if d.fragmentsSize == 0 {
			if !d.firstPacketReceived {
				return nil, errNeedMorePackets
			}

			return nil, errH264NonStartingFUAPacket
		}

		if pkt.SequenceNumber != d.fragmentNextSeqNum {
			d.resetFragments()

			return nil, errH264PacketLossWhileDecodingFUA
		}

		d.fragmentsSize += len(pkt.Payload[2:])
		if d.fragmentsSize > maxH264AccessUnitSize {
			errSize := d.fragmentsSize
			d.resetFragments()

			return nil, fmt.Errorf("%w: %d", errH264AccessUnitTooBig, errSize)
		}

		d.fragments = append(d.fragments, pkt.Payload[2:])
		d.fragmentNextSeqNum++

		if end != 1 {
			return nil, errNeedMorePackets
		}

		nalus = splitNALUsAnnexB(joinFragments(d.fragments, d.fragmentsSize))
		d.resetFragments()

	case h264TypeSTAPA:
		d.resetFragments()

		payload := pkt.Payload[1:]

		for {
			if len(payload) < 2 {
				return nil, errH264InvalidSTAPAPacket
			}

			size := int(uint16(payload[0])<<8 | uint16(payload[1]))
			payload = payload[2:]

			if size == 0 || size > len(payload) {
				return nil, errH264InvalidSTAPAPayloadSize
			}

			nalus = append(nalus, payload[:size])
			payload = payload[size:]

			if len(payload) == 0 {
				break
			}
		}

		d.firstPacketReceived = true

	default:
		d.resetFragments()
		d.firstPacketReceived = true
		nalus = [][]byte{pkt.Payload}
	}

	return nalus, nil
}

func (d *h264RTPDecoder) addToFrameBuffer(nalus [][]byte, l int, ts uint32) error {
	if (d.frameBufferLen + l) > maxH264NALUsPerAU {
		d.resetFrameBuffer()

		return errH264TooManyNALUsInAccessUnit
	}

	addSize := accessUnitSize(nalus)
	if (d.frameBufferSize + addSize) > maxH264AccessUnitSize {
		d.resetFrameBuffer()

		return errH264AccessUnitTooBig
	}

	d.frameBuffer = append(d.frameBuffer, nalus...)
	d.frameBufferLen += l
	d.frameBufferSize += addSize
	d.frameBufferTimestamp = ts

	return nil
}

func (d *h264RTPDecoder) resetFrameBuffer() {
	d.frameBuffer = nil
	d.frameBufferLen = 0
	d.frameBufferSize = 0
}

func splitNALUsAnnexB(b []byte) [][]byte {
	startCode := []byte{0x00, 0x00, 0x01}
	nalus := make([][]byte, 0, 1)

	for len(b) > 0 {
		idx := bytes.Index(b, startCode)
		if idx == -1 {
			nalus = append(nalus, b)

			break
		}

		sz := 3

		if idx > 0 && b[idx-1] == 0x00 {
			idx--
			sz++
		}

		if idx == 0 {
			b = b[sz:]

			continue
		}

		nalus = append(nalus, b[:idx])
		b = b[idx+sz:]
	}

	return nalus
}

func accessUnitSize(nalus [][]byte) int {
	total := 0
	for _, nalu := range nalus {
		total += len(nalu)
	}

	return total
}

func joinFragments(fragments [][]byte, size int) []byte {
	ret := make([]byte, size)
	n := 0

	for _, part := range fragments {
		n += copy(ret[n:], part)
	}

	return ret
}
