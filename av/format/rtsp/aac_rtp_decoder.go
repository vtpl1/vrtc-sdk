package rtsp

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/pion/rtp"
)

type aacRTPDecoder struct {
	sizeLength       int
	indexLength      int
	indexDeltaLength int
}

func newAACRTPDecoder(fmtp map[string]string) (*aacRTPDecoder, error) {
	sizeLength, err := strconv.Atoi(fmtp["sizelength"])
	if err != nil || sizeLength <= 0 {
		return nil, fmt.Errorf("rtsp: invalid AAC sizelength")
	}

	indexLength, err := strconv.Atoi(fmtp["indexlength"])
	if err != nil || indexLength < 0 {
		return nil, fmt.Errorf("rtsp: invalid AAC indexlength")
	}

	indexDeltaLength, err := strconv.Atoi(fmtp["indexdeltalength"])
	if err != nil || indexDeltaLength < 0 {
		return nil, fmt.Errorf("rtsp: invalid AAC indexdeltalength")
	}

	return &aacRTPDecoder{
		sizeLength:       sizeLength,
		indexLength:      indexLength,
		indexDeltaLength: indexDeltaLength,
	}, nil
}

func (d *aacRTPDecoder) Decode(pkt *rtp.Packet) ([][]byte, error) {
	if len(pkt.Payload) < 2 {
		return nil, fmt.Errorf("rtsp: AAC payload too short")
	}

	auHeadersBits := int(binary.BigEndian.Uint16(pkt.Payload[:2]))
	if auHeadersBits == 0 {
		return nil, nil
	}

	auHeaderBytes := (auHeadersBits + 7) / 8
	if len(pkt.Payload) < 2+auHeaderBytes {
		return nil, fmt.Errorf("rtsp: AAC AU headers truncated")
	}

	headerData := pkt.Payload[2 : 2+auHeaderBytes]
	payloadData := pkt.Payload[2+auHeaderBytes:]

	reader := bitReader{data: headerData}
	remainingBits := auHeadersBits
	first := true
	sizes := make([]int, 0, 4)

	for remainingBits > 0 {
		size, err := reader.readBits(d.sizeLength)
		if err != nil {
			return nil, err
		}

		remainingBits -= d.sizeLength

		indexBits := d.indexLength
		if !first {
			indexBits = d.indexDeltaLength
		}

		if indexBits > remainingBits {
			return nil, fmt.Errorf("rtsp: AAC AU index bits exceed header length")
		}

		if _, err := reader.readBits(indexBits); err != nil {
			return nil, err
		}

		remainingBits -= indexBits
		sizes = append(sizes, size)
		first = false
	}

	aus := make([][]byte, 0, len(sizes))
	offset := 0
	for _, size := range sizes {
		if size < 0 || offset+size > len(payloadData) {
			return nil, fmt.Errorf("rtsp: AAC AU payload truncated")
		}

		aus = append(aus, append([]byte(nil), payloadData[offset:offset+size]...))
		offset += size
	}

	return aus, nil
}

type bitReader struct {
	data []byte
	pos  int
}

func (r *bitReader) readBits(n int) (int, error) {
	if n == 0 {
		return 0, nil
	}

	if n < 0 || r.pos+n > len(r.data)*8 {
		return 0, fmt.Errorf("rtsp: bit reader overflow")
	}

	v := 0
	for i := 0; i < n; i++ {
		bytePos := (r.pos + i) / 8
		bitPos := 7 - ((r.pos + i) % 8)
		v = (v << 1) | int((r.data[bytePos]>>bitPos)&0x01)
	}

	r.pos += n

	return v, nil
}
