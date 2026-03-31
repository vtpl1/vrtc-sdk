package h265parser

import (
	"bytes"

	"github.com/vtpl1/vrtc-sdk/av/utils/bits"
)

// SPSInfo holds the parsed fields of an H.265 Sequence Parameter Set.
type SPSInfo struct {
	ProfileIdc                       uint
	LevelIdc                         uint
	MbWidth                          uint
	MbHeight                         uint
	CropLeft                         uint
	CropRight                        uint
	CropTop                          uint
	CropBottom                       uint
	Width                            uint
	Height                           uint
	numTemporalLayers                uint
	temporalIDNested                 uint
	chromaFormat                     uint
	PicWidthInLumaSamples            uint
	PicHeightInLumaSamples           uint
	bitDepthLumaMinus8               uint
	bitDepthChromaMinus8             uint
	generalProfileSpace              uint
	generalTierFlag                  uint
	generalProfileIDC                uint
	generalProfileCompatibilityFlags uint32
	generalConstraintIndicatorFlags  uint64
	generalLevelIDC                  uint
	fps                              uint
}

const (
	MaxVpsCount  = 16
	MaxSubLayers = 7
	MaxSpsCount  = 32
)

// ParseSPS parses the H.265 SPS NAL unit (including the 2-byte NAL header).
//

func ParseSPS(sps []byte) (SPSInfo, error) {
	var spsInfo SPSInfo

	var err error

	if len(sps) < 2 {
		return spsInfo, ErrH265IncorrectUnitSize
	}

	rbsp := nal2rbsp(sps[2:])
	br := &bits.GolombBitReader{R: bytes.NewReader(rbsp)}

	if _, err = br.ReadBits(4); err != nil {
		return spsInfo, err
	}

	spsMaxSubLayersMinus1, err := br.ReadBits(3)
	if err != nil {
		return spsInfo, err
	}

	if spsMaxSubLayersMinus1+1 > spsInfo.numTemporalLayers {
		spsInfo.numTemporalLayers = spsMaxSubLayersMinus1 + 1
	}

	if spsInfo.temporalIDNested, err = br.ReadBit(); err != nil {
		return spsInfo, err
	}

	if err = parsePTL(br, &spsInfo, spsMaxSubLayersMinus1); err != nil {
		return spsInfo, err
	}

	if _, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	cf, err := br.ReadExponentialGolombCode()
	if err != nil {
		return spsInfo, err
	}

	spsInfo.chromaFormat = cf
	if spsInfo.chromaFormat == 3 {
		if _, err = br.ReadBit(); err != nil {
			return spsInfo, err
		}
	}

	if spsInfo.PicWidthInLumaSamples, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	spsInfo.Width = spsInfo.PicWidthInLumaSamples

	if spsInfo.PicHeightInLumaSamples, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	spsInfo.Height = spsInfo.PicHeightInLumaSamples

	conformanceWindowFlag, err := br.ReadBit()
	if err != nil {
		return spsInfo, err
	}

	if conformanceWindowFlag != 0 {
		for range 4 {
			if _, err = br.ReadExponentialGolombCode(); err != nil {
				return spsInfo, err
			}
		}
	}

	if spsInfo.bitDepthLumaMinus8, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	if spsInfo.bitDepthChromaMinus8, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	if _, err = br.ReadExponentialGolombCode(); err != nil {
		return spsInfo, err
	}

	spsSubLayerOrderingInfoPresentFlag, err := br.ReadBit()
	if err != nil {
		return spsInfo, err
	}

	var i uint
	if spsSubLayerOrderingInfoPresentFlag != 0 {
		i = 0
	} else {
		i = spsMaxSubLayersMinus1
	}

	for ; i <= spsMaxSubLayersMinus1; i++ {
		for range 3 {
			if _, err = br.ReadExponentialGolombCode(); err != nil {
				return spsInfo, err
			}
		}
	}

	for range 6 {
		if _, err = br.ReadExponentialGolombCode(); err != nil {
			return spsInfo, err
		}
	}

	vuiParametersPresentFlag, err := br.ReadBit()
	if err != nil {
		return spsInfo, err
	}

	if vuiParametersPresentFlag != 0 {
		if err = parseVUI(br, &spsInfo); err != nil {
			return spsInfo, err
		}
	}

	return spsInfo, nil
}

func parseVUI(br *bits.GolombBitReader, spsInfo *SPSInfo) error { //nolint:unparam
	aspectRatioInfoPresentFlag, _ := br.ReadBit()
	if aspectRatioInfoPresentFlag != 0 {
		aspectRatioIdc, _ := br.ReadBits(8)
		if aspectRatioIdc == 255 {
			br.ReadBits(16) //nolint:errcheck
			br.ReadBits(16) //nolint:errcheck
		}
	}

	overscanInfoPresentFlag, _ := br.ReadBit()
	if overscanInfoPresentFlag != 0 {
		br.ReadBit() //nolint:errcheck
	}

	videoSignalTypePresentFlag, _ := br.ReadBit()
	if videoSignalTypePresentFlag != 0 {
		br.ReadBits(3) //nolint:errcheck
		br.ReadBit()   //nolint:errcheck

		colourDescriptionPresentFlag, _ := br.ReadBit()
		if colourDescriptionPresentFlag != 0 {
			br.ReadBits(8) //nolint:errcheck
			br.ReadBits(8) //nolint:errcheck
			br.ReadBits(8) //nolint:errcheck
		}
	}

	chromaLocInfoPresentFlag, _ := br.ReadBit()
	if chromaLocInfoPresentFlag != 0 {
		br.ReadExponentialGolombCode() //nolint:errcheck
		br.ReadExponentialGolombCode() //nolint:errcheck
	}

	br.ReadBit() //nolint:errcheck
	br.ReadBit() //nolint:errcheck
	br.ReadBit() //nolint:errcheck

	defaultDisplayWindowFlag, _ := br.ReadBit()
	if defaultDisplayWindowFlag != 0 {
		for range 4 {
			br.ReadExponentialGolombCode() //nolint:errcheck
		}
	}

	vuiTimingInfoPresentFlag, _ := br.ReadBit()
	if vuiTimingInfoPresentFlag != 0 {
		numUnitsInTick, _ := br.ReadBits32(32)
		timeScale, _ := br.ReadBits32(32)

		if numUnitsInTick > 0 {
			spsInfo.fps = uint(timeScale / (2 * numUnitsInTick))
		}
	}

	return nil
}

func parsePTL(br *bits.GolombBitReader, ctx *SPSInfo, maxSubLayersMinus1 uint) error {
	var (
		err error
		ptl SPSInfo
	)

	if ptl.generalProfileSpace, err = br.ReadBits(2); err != nil {
		return err
	}

	if ptl.generalTierFlag, err = br.ReadBit(); err != nil {
		return err
	}

	if ptl.generalProfileIDC, err = br.ReadBits(5); err != nil {
		return err
	}

	if ptl.generalProfileCompatibilityFlags, err = br.ReadBits32(32); err != nil {
		return err
	}

	if ptl.generalConstraintIndicatorFlags, err = br.ReadBits64(48); err != nil {
		return err
	}

	if ptl.generalLevelIDC, err = br.ReadBits(8); err != nil {
		return err
	}

	updatePTL(ctx, &ptl)

	if maxSubLayersMinus1 == 0 {
		return nil
	}

	subLayerProfilePresentFlag := make([]uint, maxSubLayersMinus1)
	subLayerLevelPresentFlag := make([]uint, maxSubLayersMinus1)

	for i := range maxSubLayersMinus1 {
		if subLayerProfilePresentFlag[i], err = br.ReadBit(); err != nil {
			return err
		}

		if subLayerLevelPresentFlag[i], err = br.ReadBit(); err != nil {
			return err
		}
	}

	for i := maxSubLayersMinus1; i < 8; i++ {
		if _, err = br.ReadBits(2); err != nil {
			return err
		}
	}

	for i := range maxSubLayersMinus1 {
		if subLayerProfilePresentFlag[i] != 0 {
			if _, err = br.ReadBits32(32); err != nil {
				return err
			}

			if _, err = br.ReadBits32(32); err != nil {
				return err
			}

			if _, err = br.ReadBits32(24); err != nil {
				return err
			}
		}

		if subLayerLevelPresentFlag[i] != 0 {
			if _, err = br.ReadBits(8); err != nil {
				return err
			}
		}
	}

	return nil
}

func updatePTL(ctx, ptl *SPSInfo) {
	ctx.generalProfileSpace = ptl.generalProfileSpace

	if ptl.generalTierFlag > ctx.generalTierFlag {
		ctx.generalLevelIDC = ptl.generalLevelIDC
		ctx.generalTierFlag = ptl.generalTierFlag
	} else if ptl.generalLevelIDC > ctx.generalLevelIDC {
		ctx.generalLevelIDC = ptl.generalLevelIDC
	}

	if ptl.generalProfileIDC > ctx.generalProfileIDC {
		ctx.generalProfileIDC = ptl.generalProfileIDC
	}

	ctx.generalProfileCompatibilityFlags |= ptl.generalProfileCompatibilityFlags
	ctx.generalConstraintIndicatorFlags |= ptl.generalConstraintIndicatorFlags
}

// nal2rbsp strips H.265 emulation prevention bytes (0x000003 → 0x0000).
func nal2rbsp(nal []byte) []byte {
	out := make([]byte, 0, len(nal))

	for i := 0; i < len(nal); i++ {
		if i+2 < len(nal) && nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2

			continue
		}

		out = append(out, nal[i])
	}

	return out
}
