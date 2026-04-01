package rtsp

import "errors"

var (
	ErrNotStarted            = errors.New("rtsp: demuxer is not started")
	ErrNoSupportedVideoTrack = errors.New("rtsp: no supported video track in SDP")
	ErrUnexpectedStatusLine  = errors.New("rtsp: malformed status line")
	ErrUnexpectedInterleaved = errors.New("rtsp: unexpected interleaved frame")
)

var (
	errNeedMorePackets = errors.New("rtsp: need more packets")
)
