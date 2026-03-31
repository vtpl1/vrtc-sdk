package h265parser

import (
	"errors"
	"slices"
)

var (
	errInvalidPayload = errors.New("invalid payload")
	errInvalidFU      = errors.New("invalid FU")
)

// AccessUnit groups the NALUs of a single H.265 picture.
type AccessUnit struct {
	NALUs    [][]byte
	KeyFrame bool
}

// Parser reassembles H.265 access units from RFC 7798 RTP payloads.
type Parser struct {
	current  [][]byte
	fuBuffer []byte
	fuActive bool
}

// PushRTP processes one RTP payload and returns a complete AccessUnit when ready.
func (p *Parser) PushRTP(payload []byte) (*AccessUnit, bool, error) {
	if len(payload) < 2 {
		return nil, false, errInvalidPayload
	}

	naluType := (payload[0] >> 1) & 0x3F

	switch naluType {
	case 48: // Aggregation Packet (AP)
		return p.parseAP(payload)
	case 49: // Fragmentation Unit (FU)
		return p.parseFU(payload)
	default: // Single NALU
		return p.handleNALU(payload)
	}
}

func (p *Parser) parseAP(payload []byte) (*AccessUnit, bool, error) {
	offset := 2

	for offset+2 <= len(payload) {
		size := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2

		if offset+size > len(payload) {
			break
		}

		nalu := payload[offset : offset+size]
		offset += size

		if au, ready := p.pushNALU(nalu); ready {
			return au, true, nil
		}
	}

	return nil, false, nil
}

func (p *Parser) parseFU(payload []byte) (*AccessUnit, bool, error) {
	if len(payload) < 3 {
		return nil, false, errInvalidFU
	}

	start := payload[2]&0x80 != 0
	end := payload[2]&0x40 != 0
	typ := payload[2] & 0x3F

	if start {
		p.fuBuffer = p.fuBuffer[:0]
		naluHeader := (payload[0] & 0x81) | (typ << 1)
		p.fuBuffer = append(p.fuBuffer, naluHeader, payload[1])
		p.fuBuffer = append(p.fuBuffer, payload[3:]...)
		p.fuActive = true

		return nil, false, nil
	}

	if !p.fuActive {
		return nil, false, nil
	}

	p.fuBuffer = append(p.fuBuffer, payload[3:]...)

	if end {
		nalu := make([]byte, len(p.fuBuffer))
		copy(nalu, p.fuBuffer)
		p.fuActive = false

		return p.handleNALU(nalu)
	}

	return nil, false, nil
}

func (p *Parser) handleNALU(nalu []byte) (*AccessUnit, bool, error) {
	if au, ready := p.pushNALU(nalu); ready {
		return au, true, nil
	}

	return nil, false, nil
}

func (p *Parser) pushNALU(nalu []byte) (*AccessUnit, bool) {
	typ := NALUType(nalu)

	if IsFirstSlice(nalu) && len(p.current) > 0 {
		au := p.buildAU()
		p.current = p.current[:0]
		p.current = append(p.current, nalu)

		return au, true
	}

	if typ == 35 { // HEVC_NAL_AUD
		return nil, false
	}

	p.current = append(p.current, nalu)

	return nil, false
}

// Flush returns any buffered incomplete access unit.
func (p *Parser) Flush() *AccessUnit {
	if len(p.current) == 0 {
		return nil
	}

	au := p.buildAU()
	p.current = nil

	return au
}

func (p *Parser) buildAU() *AccessUnit {
	au := &AccessUnit{
		NALUs: make([][]byte, len(p.current)),
	}
	copy(au.NALUs, p.current)

	if slices.ContainsFunc(au.NALUs, IsIRAP) {
		au.KeyFrame = true
	}

	return au
}
