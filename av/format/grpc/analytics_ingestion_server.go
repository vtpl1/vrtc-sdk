package grpc

import (
	"errors"
	"io"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	pb "github.com/vtpl1/vrtc-sdk/av/format/grpc/gen/avtransportv1"
)

// AnalyticsHandler is called for each analytics result received via the
// AnalyticsIngestionService gRPC stream.
//
//	sourceID    — camera / channel identifier (matches RelayHub sourceID)
//	frameID     — camera PTS ticks (av.Packet.FrameID) for correlation
//	wallClock   — avgrabber wall-clock of the frame (from wall_clock_ms in proto)
//	analytics   — the decoded FrameAnalytics result
type AnalyticsHandler func(sourceID string, frameID int64, wallClock time.Time, a *av.FrameAnalytics)

// AnalyticsIngestionServer implements the gRPC AnalyticsIngestionServiceServer.
// It receives streaming analytics results from an external inference tool and
// dispatches them to the registered AnalyticsHandler.
type AnalyticsIngestionServer struct {
	pb.UnimplementedAnalyticsIngestionServiceServer

	handler AnalyticsHandler
}

// NewAnalyticsIngestionServer creates a server that calls h for each ingested
// analytics result. h must be goroutine-safe; it is called from the gRPC
// receive goroutine.
func NewAnalyticsIngestionServer(h AnalyticsHandler) *AnalyticsIngestionServer {
	return &AnalyticsIngestionServer{handler: h}
}

// IngestAnalytics handles a client-streaming RPC. The analytics tool keeps the
// stream open and sends one IngestAnalyticsRequest per processed frame.
// The RPC returns when the client closes the stream or the context is cancelled.
func (s *AnalyticsIngestionServer) IngestAnalytics(
	stream pb.AnalyticsIngestionService_IngestAnalyticsServer,
) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return stream.SendAndClose(&pb.IngestAnalyticsResponse{})
			}

			return err
		}

		fa := req.GetAnalytics()
		if fa == nil {
			continue
		}

		wallClock := time.UnixMilli(req.GetWallClockMs())
		s.handler(req.GetSourceId(), req.GetFrameId(), wallClock, unmarshalAnalytics(fa))
	}
}
