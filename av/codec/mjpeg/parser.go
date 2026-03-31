// Package mjpeg holds implementations for mjpeg
package mjpeg

import "github.com/vtpl1/vrtc-sdk/av"

type CodecData struct{}

func (d CodecData) Type() av.CodecType {
	return av.MJPEG
}
