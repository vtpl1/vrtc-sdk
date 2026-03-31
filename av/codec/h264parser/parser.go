// Package h264parser provides H.264/AVC NAL-unit parsing utilities:
// SPS decoding, parameter-set handling (SPS/PPS), avcC record
// marshalling/unmarshalling, AnnexB/AVCC conversion, and slice-header parsing.
//
// # AnnexB vs AVCC
//
// AnnexB (used in MPEG-TS, live streams, DVDs) prefixes each NALU with a
// 3- or 4-byte start code (0x000001 or 0x00000001).
//
// AVCC (used in MP4/MKV containers) prefixes each NALU with a 4-byte
// big-endian length field. The out-of-band codec configuration is stored in
// an AVCDecoderConfigurationRecord (avcC box).
//
// Emulation-prevention bytes (0x03 inserted to avoid false start codes within
// RBSP payloads) are transparently stripped by RemoveH264orH265EmulationBytes.
package h264parser
