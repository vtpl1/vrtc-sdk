package rtsp

import "errors"

var (
	ErrNotStarted            = errors.New("rtsp: demuxer is not started")
	ErrNoSupportedTrack      = errors.New("rtsp: no supported audio/video track in SDP")
	ErrNoSupportedVideoTrack = ErrNoSupportedTrack
	ErrUnexpectedStatusLine  = errors.New("rtsp: malformed status line")
	ErrUnexpectedInterleaved = errors.New("rtsp: unexpected interleaved frame")
)

var (
	errNeedMorePackets = errors.New("rtsp: need more packets")
)
