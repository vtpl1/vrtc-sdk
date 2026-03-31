package h264parser

import (
	"bytes"
	"math"

	"github.com/vtpl1/vrtc-sdk/av/utils/bits"
)

// SPSInfo holds the parsed fields of an H.264 Sequence Parameter Set.
type SPSInfo struct {
	ID                uint
	ProfileIdc        uint
	LevelIdc          uint
	ConstraintSetFlag uint

	MbWidth  uint
	MbHeight uint

	CropLeft   uint
	CropRight  uint
	CropTop    uint
	CropBottom uint

	Width  uint
	Height uint
	FPS    uint
}

// RemoveH264orH265EmulationBytes strips emulation-prevention bytes (0x03 in
// 0x000003 sequences) from a raw byte slice, returning the RBSP.
func RemoveH264orH265EmulationBytes(b []byte) []byte {
	j := 0
	r := make([]byte, len(b))

	for i := 0; (i < len(b)) && (j < len(b)); {
		if i+2 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			r[j] = 0
			r[j+1] = 0
			j += 2
			i += 3
		} else {
			r[j] = b[i]
			j++
			i++
		}
	}

	return r[:j]
}

// ParseSPS parses the H.264 SPS NAL unit (including the 1-byte NAL header).
//
//nolint:cyclop,gocyclo
func ParseSPS(data []byte) (SPSInfo, error) {
	data = RemoveH264orH265EmulationBytes(data)
	r := &bits.GolombBitReader{R: bytes.NewReader(data)}

	var s SPSInfo

	var err error
	if _, err = r.ReadBits(bitsInByte); err != nil {
		return s, err
	}

	if s.ProfileIdc, err = r.ReadBits(bitsInByte); err != nil {
		return s, err
	}

	// constraint_set0_flag–constraint_set6_flag, reserved_zero_2bits
	if s.ConstraintSetFlag, err = r.ReadBits(bitsInByte); err != nil {
		return s, err
	}

	s.ConstraintSetFlag >>= 2

	if s.LevelIdc, err = r.ReadBits(bitsInByte); err != nil {
		return s, err
	}

	// seq_parameter_set_id
	if s.ID, err = r.ReadExponentialGolombCode(); err != nil {
		return s, err
	}

	if s.ProfileIdc == 100 || s.ProfileIdc == 110 ||
		s.ProfileIdc == 122 || s.ProfileIdc == 244 ||
		s.ProfileIdc == 44 || s.ProfileIdc == 83 ||
		s.ProfileIdc == 86 || s.ProfileIdc == 118 {
		if err = parseHighProfileFields(r, &s); err != nil {
			return s, err
		}
	}

	// log2_max_frame_num_minus4
	if _, err = r.ReadExponentialGolombCode(); err != nil {
		return s, err
	}

	var picOrderCntType uint

	if picOrderCntType, err = r.ReadExponentialGolombCode(); err != nil {
		return s, err
	}

	switch picOrderCntType {
	case 0:
		if _, err = r.ReadExponentialGolombCode(); err != nil { // log2_max_pic_order_cnt_lsb_minus4
			return s, err
		}
	case 1:
		if _, err = r.ReadBit(); err != nil { // delta_pic_order_always_zero_flag
			return s, err
		}

		if _, err = r.ReadSE(); err != nil { // offset_for_non_ref_pic
			return s, err
		}

		if _, err = r.ReadSE(); err != nil { // offset_for_top_to_bottom_field
			return s, err
		}

		var numRefFramesInPicOrderCntCycle uint

		if numRefFramesInPicOrderCntCycle, err = r.ReadExponentialGolombCode(); err != nil {
			return s, err
		}

		for range numRefFramesInPicOrderCntCycle {
			if _, err = r.ReadSE(); err != nil {
				return s, err
			}
		}
	}

	if _, err = r.ReadExponentialGolombCode(); err != nil { // max_num_ref_frames
		return s, err
	}

	if _, err = r.ReadBit(); err != nil { // gaps_in_frame_num_value_allowed_flag
		return s, err
	}

	if s.MbWidth, err = r.ReadExponentialGolombCode(); err != nil {
		return s, err
	}

	s.MbWidth++

	if s.MbHeight, err = r.ReadExponentialGolombCode(); err != nil {
		return s, err
	}

	s.MbHeight++

	var frameMbsOnlyFlag uint

	if frameMbsOnlyFlag, err = r.ReadBit(); err != nil {
		return s, err
	}

	if frameMbsOnlyFlag == 0 {
		if _, err = r.ReadBit(); err != nil { // mb_adaptive_frame_field_flag
			return s, err
		}
	}

	if _, err = r.ReadBit(); err != nil { // direct_8x8_inference_flag
		return s, err
	}

	var frameCroppingFlag uint

	if frameCroppingFlag, err = r.ReadBit(); err != nil {
		return s, err
	}

	if frameCroppingFlag != 0 {
		if s.CropLeft, err = r.ReadExponentialGolombCode(); err != nil {
			return s, err
		}

		if s.CropRight, err = r.ReadExponentialGolombCode(); err != nil {
			return s, err
		}

		if s.CropTop, err = r.ReadExponentialGolombCode(); err != nil {
			return s, err
		}

		if s.CropBottom, err = r.ReadExponentialGolombCode(); err != nil {
			return s, err
		}
	}

	s.Width = (s.MbWidth * 16) - s.CropLeft*2 - s.CropRight*2
	s.Height = ((2 - frameMbsOnlyFlag) * s.MbHeight * 16) - s.CropTop*2 - s.CropBottom*2

	var vuiParameterPresentFlag uint

	if vuiParameterPresentFlag, err = r.ReadBit(); err != nil {
		return s, err
	}

	if vuiParameterPresentFlag != 0 {
		if err = parseVUI(r, &s); err != nil {
			return s, err
		}
	}

	return s, nil
}

// parseHighProfileFields reads the extra SPS fields present for High-profile
// and related profiles (profile_idc 100, 110, 122, 244, 44, 83, 86, 118).
func parseHighProfileFields(r *bits.GolombBitReader, _ *SPSInfo) error {
	var err error

	var chromaFormatIdc uint

	if chromaFormatIdc, err = r.ReadExponentialGolombCode(); err != nil {
		return err
	}

	if chromaFormatIdc == 3 {
		if _, err = r.ReadBit(); err != nil { // residual_colour_transform_flag
			return err
		}
	}

	if _, err = r.ReadExponentialGolombCode(); err != nil { // bit_depth_luma_minus8
		return err
	}

	if _, err = r.ReadExponentialGolombCode(); err != nil { // bit_depth_chroma_minus8
		return err
	}

	if _, err = r.ReadBit(); err != nil { // qpprime_y_zero_transform_bypass_flag
		return err
	}

	var seqScalingMatrixPresentFlag uint

	if seqScalingMatrixPresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if seqScalingMatrixPresentFlag != 0 {
		for i := range 8 {
			var seqScalingListPresentFlag uint

			if seqScalingListPresentFlag, err = r.ReadBit(); err != nil {
				return err
			}

			if seqScalingListPresentFlag != 0 {
				sizeOfScalingList := uint(16)
				if i >= 6 {
					sizeOfScalingList = 64
				}

				lastScale := uint(8)
				nextScale := uint(8)

				for range sizeOfScalingList {
					if nextScale != 0 {
						var deltaScale uint

						if deltaScale, err = r.ReadSE(); err != nil {
							return err
						}

						nextScale = (lastScale + deltaScale + 256) % 256
					}

					if nextScale != 0 {
						lastScale = nextScale
					}
				}
			}
		}
	}

	return nil
}

// parseVUI reads the VUI parameters from the SPS, extracting FPS from
// timing_info_present_flag when available.
func parseVUI(r *bits.GolombBitReader, s *SPSInfo) error {
	var err error

	var aspectRatioInfoPresentFlag uint

	if aspectRatioInfoPresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if aspectRatioInfoPresentFlag != 0 {
		var aspectRatioIdc uint

		if aspectRatioIdc, err = r.ReadBits(8); err != nil {
			return err
		}

		if aspectRatioIdc == 255 { // EXTENDED_SAR
			if _, err = r.ReadBits(16); err != nil { // sar_width
				return err
			}

			if _, err = r.ReadBits(16); err != nil { // sar_height
				return err
			}
		}
	}

	var overscanInfoPresentFlag uint

	if overscanInfoPresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if overscanInfoPresentFlag != 0 {
		if _, err = r.ReadBit(); err != nil { // overscan_appropriate_flag
			return err
		}
	}

	var videoSignalTypePresentFlag uint

	if videoSignalTypePresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if videoSignalTypePresentFlag != 0 {
		if _, err = r.ReadBits(3); err != nil { // video_format
			return err
		}

		if _, err = r.ReadBit(); err != nil { // video_full_range_flag
			return err
		}

		var colourDescriptionPresentFlag uint

		if colourDescriptionPresentFlag, err = r.ReadBit(); err != nil {
			return err
		}

		if colourDescriptionPresentFlag != 0 {
			if _, err = r.ReadBits(8); err != nil { // colour_primaries
				return err
			}

			if _, err = r.ReadBits(8); err != nil { // transfer_characteristics
				return err
			}

			if _, err = r.ReadBits(8); err != nil { // matrix_coefficients
				return err
			}
		}
	}

	var chromaLocInfoPresentFlag uint

	if chromaLocInfoPresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if chromaLocInfoPresentFlag != 0 {
		if _, err = r.ReadSE(); err != nil { // chroma_sample_loc_type_top_field
			return err
		}

		if _, err = r.ReadSE(); err != nil { // chroma_sample_loc_type_bottom_field
			return err
		}
	}

	var timingInfoPresentFlag uint

	if timingInfoPresentFlag, err = r.ReadBit(); err != nil {
		return err
	}

	if timingInfoPresentFlag != 0 {
		var numUnitsInTick uint

		if numUnitsInTick, err = r.ReadBits(32); err != nil {
			return err
		}

		var timeScale uint

		if timeScale, err = r.ReadBits(32); err != nil {
			return err
		}

		s.FPS = uint(math.Floor(float64(timeScale) / float64(numUnitsInTick) / 2.0))
	}

	return nil
}
