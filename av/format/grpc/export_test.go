package grpc

import (
	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

// MarshalStreamsForTest exposes marshalStreams for testing.
func MarshalStreamsForTest(streams []av.Stream) []*pb.StreamInfo {
	return marshalStreams(streams)
}

// UnmarshalStreamsForTest exposes unmarshalStreams for testing.
func UnmarshalStreamsForTest(infos []*pb.StreamInfo) ([]av.Stream, error) {
	return unmarshalStreams(infos)
}
