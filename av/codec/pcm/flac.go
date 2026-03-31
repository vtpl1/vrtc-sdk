package pcm

import (
	"encoding/binary"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sigurn/crc16"
	"github.com/sigurn/crc8"
	"github.com/vtpl1/vrtc-sdk/av"
)

var (
	errFlacPacketTooShort  = errors.New("flac: packet too short")
	errFlacInvalidUTF8     = errors.New("flac: invalid UTF-8 in frame header")
	errFlacPacketTruncated = errors.New("flac: packet truncated before block size")
)

// FLACCodecData implements av.AudioCodecData for FLAC-wrapped PCM audio.
type FLACCodecData struct {
	sourceType av.CodecType
	sampleRate uint32
	chLayout   av.ChannelLayout
}

func NewFLACCodecData(
	sourceType av.CodecType,
	sampleRate uint32,
	ch av.ChannelLayout,
) FLACCodecData {
	return FLACCodecData{sourceType: sourceType, sampleRate: sampleRate, chLayout: ch}
}

func (f FLACCodecData) Type() av.CodecType              { return av.FLAC }
func (f FLACCodecData) SampleFormat() av.SampleFormat   { return av.S16 }
func (f FLACCodecData) SampleRate() int                 { return int(f.sampleRate) }
func (f FLACCodecData) ChannelLayout() av.ChannelLayout { return f.chLayout }
func (f FLACCodecData) SourceType() av.CodecType        { return f.sourceType }

// STREAMINFOBlock returns the 38-byte STREAMINFO metadata block for dfLaC box.
// FLACHeader(false, sr) returns 42 bytes: 4-byte magic placeholder + 38 bytes.
func (f FLACCodecData) STREAMINFOBlock() []byte {
	return FLACHeader(false, f.sampleRate)[4:]
}

// PacketDuration parses the FLAC frame header. FLACEncoder always uses
// blockSizeType=7 (16-bit block size - 1 follows the UTF-8 sample number).
func (f FLACCodecData) PacketDuration(pkt []byte) (time.Duration, error) {
	if len(pkt) < 8 {
		return 0, errFlacPacketTooShort
	}

	_, runeLen := utf8.DecodeRune(pkt[4:])
	if runeLen == 0 {
		return 0, errFlacInvalidUTF8
	}

	offset := 4 + runeLen
	if offset+2 > len(pkt) {
		return 0, errFlacPacketTruncated
	}

	blockSize := uint32(binary.BigEndian.Uint16(pkt[offset:])) + 1

	return time.Duration(blockSize) * time.Second / time.Duration(f.sampleRate), nil
}

func FLACHeader(magic bool, sampleRate uint32) []byte {
	b := make([]byte, 42)

	if magic {
		copy(b, "fLaC") // [0..3]
	}

	// https://xiph.org/flac/format.html#metadata_block_header
	b[4] = 0x80 // [4] lastMetadata=1 (1 bit), blockType=0 - STREAMINFO (7 bit)
	b[7] = 0x22 // [5..7] blockLength=34 (24 bit)

	// Important for Apple QuickTime player:
	// 1. Both values should be same
	// 2. Maximum value = 32768
	binary.BigEndian.PutUint16(b[8:], 32768)  // [8..9] info.BlockSizeMin=32768 (16 bit)
	binary.BigEndian.PutUint16(b[10:], 32768) // [10..11] info.BlockSizeMax=32768 (16 bit)

	// [12..14] info.FrameSizeMin=0 (24 bit)
	// [15..17] info.FrameSizeMax=0 (24 bit)

	b[18] = byte(sampleRate >> 12)
	b[19] = byte(sampleRate >> 4)
	b[20] = byte(
		sampleRate << 4,
	) // [18..20] info.SampleRate=8000 (20 bit), info.NChannels=1-1 (3 bit)

	b[21] = 0xF0 // [21..25] info.BitsPerSample=16-1 (5 bit), info.NSamples (36 bit)

	// [26..41] MD5sum (16 bytes)

	return b
}

var (
	flacCRCOnce sync.Once    //nolint:gochecknoglobals
	table8      *crc8.Table  //nolint:gochecknoglobals
	table16     *crc16.Table //nolint:gochecknoglobals
)

func initCRCTables() {
	flacCRCOnce.Do(func() {
		table8 = crc8.MakeTable(crc8.CRC8)
		table16 = crc16.MakeTable(crc16.CRC16_BUYPASS)
	})
}

func FLACEncoder(codecName av.CodecType, clockRate uint32) func([]byte) []byte {
	var sr byte

	switch clockRate {
	case 8000:
		sr = 0b0100
	case 16000:
		sr = 0b0101
	case 22050:
		sr = 0b0110
	case 24000:
		sr = 0b0111
	case 32000:
		sr = 0b1000
	case 44100:
		sr = 0b1001
	case 48000:
		sr = 0b1010
	case 96000:
		sr = 0b1011
	default:
		return nil
	}

	initCRCTables()

	var sampleNumber int32

	return func(src []byte) []byte {
		samples := uint16(len(src))

		if codecName == av.PCM /*||codecName == core.CodecPCML*/ {
			samples /= 2
		}

		// https://xiph.org/flac/format.html#frame_header
		buf := make([]byte, samples*2+30)

		// 1. Frame header
		buf[0] = 0xFF
		buf[1] = 0xF9      // [0..1] syncCode=0xFFF8 - reserved (15 bit), blockStrategy=1 - variable-blocksize (1 bit)
		buf[2] = 0x70 | sr // blockSizeType=7 (4 bit), sampleRate=4 - 8000 (4 bit)
		buf[3] = 0x08      // channels=1-1 (4 bit), sampleSize=4 - 16 (3 bit), reserved=0 (1 bit)

		n := 4 + utf8.EncodeRune(buf[4:], sampleNumber) // 4 bytes max
		sampleNumber += int32(samples)

		// this is wrong but very simple frame block size value
		binary.BigEndian.PutUint16(buf[n:], samples-1)
		n += 2

		buf[n] = crc8.Checksum(buf[:n], table8)
		n++

		// 2. Subframe header
		buf[n] = 0x02 // padding=0 (1 bit), subframeType=1 - verbatim (6 bit), wastedFlag=0 (1 bit)
		n++

		// 3. Subframe
		switch codecName {
		case av.PCM_ALAW:
			for _, b := range src {
				s16 := PCMAtoPCM(b)
				buf[n] = byte(s16 >> 8)
				buf[n+1] = byte(s16)
				n += 2
			}
		case av.PCM_MULAW:
			for _, b := range src {
				s16 := PCMUtoPCM(b)
				buf[n] = byte(s16 >> 8)
				buf[n+1] = byte(s16)
				n += 2
			}
		case av.PCM:
			n += copy(buf[n:], src)
		case av.PCML:
			// reverse endian from little to big
			size := len(src)
			for i := 0; i < size; i += 2 {
				buf[n] = src[i+1]
				buf[n+1] = src[i]
				n += 2
			}
		}

		// 4. Frame footer
		crc := crc16.Checksum(buf[:n], table16)
		binary.BigEndian.PutUint16(buf[n:], crc)
		n += 2

		dst := buf[:n]

		return dst
	}
}
