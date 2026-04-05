package grpc

import (
	"fmt"
	"math"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

// marshalPacket converts an av.Packet to a proto AVPacket.
func marshalPacket(pkt av.Packet) *pb.AVPacket {
	p := &pb.AVPacket{
		KeyFrame:        pkt.KeyFrame,
		IsDiscontinuity: pkt.IsDiscontinuity,
		Idx:             uint32(pkt.Idx),
		CodecType:       uint32(pkt.CodecType),
		FrameId:         pkt.FrameID,
		DtsNs:           int64(pkt.DTS),
		PtsOffsetNs:     int64(pkt.PTSOffset),
		DurationNs:      int64(pkt.Duration),
		Data:            pkt.Data,
	}

	if !pkt.WallClockTime.IsZero() {
		p.WallClockMs = pkt.WallClockTime.UnixMilli()
	}

	if pkt.Analytics != nil {
		p.Analytics = marshalAnalytics(pkt.Analytics)
	}

	if len(pkt.NewCodecs) > 0 {
		p.NewCodecs = marshalStreams(pkt.NewCodecs)
	}

	return p
}

// unmarshalPacket converts a proto AVPacket back to an av.Packet.
func unmarshalPacket(p *pb.AVPacket) (av.Packet, error) {
	if p.GetIdx() > math.MaxUint16 {
		return av.Packet{}, fmt.Errorf("%w: packet idx %d", errIdxOverflow, p.GetIdx())
	}

	pkt := av.Packet{
		KeyFrame:        p.GetKeyFrame(),
		IsDiscontinuity: p.GetIsDiscontinuity(),
		Idx:             uint16(p.GetIdx()),
		CodecType:       av.CodecType(p.GetCodecType()),
		FrameID:         p.GetFrameId(),
		DTS:             time.Duration(p.GetDtsNs()),
		PTSOffset:       time.Duration(p.GetPtsOffsetNs()),
		Duration:        time.Duration(p.GetDurationNs()),
		Data:            p.GetData(),
	}

	if p.GetWallClockMs() != 0 {
		pkt.WallClockTime = time.UnixMilli(p.GetWallClockMs())
	}

	if p.GetAnalytics() != nil {
		pkt.Analytics = unmarshalAnalytics(p.GetAnalytics())
	}

	if len(p.GetNewCodecs()) > 0 {
		nc, err := unmarshalStreams(p.GetNewCodecs())
		if err != nil {
			return pkt, err
		}

		pkt.NewCodecs = nc
	}

	return pkt, nil
}

func marshalAnalytics(a *av.FrameAnalytics) *pb.FrameAnalytics {
	fa := &pb.FrameAnalytics{
		SiteId:       a.SiteID,
		ChannelId:    a.ChannelID,
		FramePts:     a.FramePTS,
		CaptureMs:    a.CaptureMS,
		CaptureEndMs: a.CaptureEndMS,
		InferenceMs:  a.InferenceMS,
		RefWidth:     a.RefWidth,
		RefHeight:    a.RefHeight,
		VehicleCount: a.VehicleCount,
		PeopleCount:  a.PeopleCount,
	}

	for _, d := range a.Objects {
		if d != nil {
			fa.Objects = append(fa.Objects, &pb.Detection{
				X: d.X, Y: d.Y, W: d.W, H: d.H,
				ClassId: d.ClassID, Confidence: d.Confidence,
				TrackId: d.TrackID, IsEvent: d.IsEvent,
			})
		}
	}

	return fa
}

func unmarshalAnalytics(fa *pb.FrameAnalytics) *av.FrameAnalytics {
	a := &av.FrameAnalytics{
		SiteID:       fa.GetSiteId(),
		ChannelID:    fa.GetChannelId(),
		FramePTS:     fa.GetFramePts(),
		CaptureMS:    fa.GetCaptureMs(),
		CaptureEndMS: fa.GetCaptureEndMs(),
		InferenceMS:  fa.GetInferenceMs(),
		RefWidth:     fa.GetRefWidth(),
		RefHeight:    fa.GetRefHeight(),
		VehicleCount: fa.GetVehicleCount(),
		PeopleCount:  fa.GetPeopleCount(),
	}

	for _, d := range fa.GetObjects() {
		a.Objects = append(a.Objects, &av.Detection{
			X: d.GetX(), Y: d.GetY(), W: d.GetW(), H: d.GetH(),
			ClassID: d.GetClassId(), Confidence: d.GetConfidence(),
			TrackID: d.GetTrackId(), IsEvent: d.GetIsEvent(),
		})
	}

	return a
}
