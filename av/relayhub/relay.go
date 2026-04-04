package relayhub

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
	"github.com/vtpl1/vrtc-sdk/av/packetbuf"
)

// Relay manages one demuxer source and fans its decoded packets out to
// registered consumers. It is created on-demand by RelayHub and reclaimed
// automatically when its consumer count drops to zero.
type Relay struct {
	id             string
	demuxerFactory av.DemuxerFactory
	demuxerRemover av.DemuxerRemover

	cancel context.CancelFunc
	// producer's running context; set under mu in Start
	sctx           context.Context //nolint:containedctx
	wg             sync.WaitGroup
	mu             sync.RWMutex
	alreadyClosing atomic.Bool
	started        atomic.Bool
	consumers      map[string]*Consumer

	demuxer          av.DemuxCloser
	headers          []av.Stream
	headersErr       error
	headersAvailable chan struct{}

	// Packet buffer — stores recent packets for GOP replay on consumer attach
	// and seamless recorded-to-live playback transition.
	pktBuf *packetbuf.Buffer

	// maxConsumers limits the number of consumers on this relay. 0 means
	// unlimited. Used to enforce single-consumer isolation on recorded
	// playback relays so that leaky delivery mode is never triggered.
	maxConsumers int

	// Configurable constants (0 means use package default).
	maxFpsOverride    int
	packetBufWindow   time.Duration
	consumerQueueSize int

	// metrics — updated from readWriteLoop; read via Stats()
	packetsRead      atomic.Uint64
	videoPacketsRead atomic.Uint64
	bytesRead        atomic.Uint64
	keyFrames        atomic.Uint64
	droppedPackets   atomic.Uint64
	lastPacketAtNs   atomic.Int64 // unix nanoseconds; 0 = no packet yet
	startedAt        time.Time    // set once in Start; zero until Start is called

	// Rate-limited drop logging: only log once per dropLogInterval.
	lastDropLog atomic.Int64 // unix nanoseconds
}

func cloneStreamHeaders(streams []av.Stream) []av.Stream {
	if len(streams) == 0 {
		return nil
	}

	return append([]av.Stream(nil), streams...)
}

// NewRelay creates a Relay for the given sourceID. Start must be called to
// begin reading packets from the demuxer.
func NewRelay(
	sourceID string,
	demuxerFactory av.DemuxerFactory,
	demuxerRemover av.DemuxerRemover,
) *Relay {
	m := &Relay{
		id:               sourceID,
		demuxerFactory:   demuxerFactory,
		demuxerRemover:   demuxerRemover,
		headersAvailable: make(chan struct{}),
		consumers:        make(map[string]*Consumer),
	}

	return m
}

// Start opens the demuxer and begins the packet read/write loop. It must be
// called exactly once. A second call returns ErrRelayAlreadyStarted.
func (m *Relay) Start(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return ErrRelayAlreadyStarted
	}

	if m.alreadyClosing.Load() {
		return ErrRelayClosing
	}

	// Initialise packet buffer with configurable window (default 30s).
	// Done here so that RelayOptions applied after NewRelay take effect.
	bufWindow := 30 * time.Second
	if m.packetBufWindow > 0 {
		bufWindow = m.packetBufWindow
	}

	m.pktBuf = packetbuf.New(bufWindow)

	sctx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	m.cancel = cancel
	m.sctx = sctx
	m.mu.Unlock()
	m.mu.Lock()
	m.startedAt = time.Now()
	m.mu.Unlock()

	m.wg.Go(func() {
		defer cancel()

		demuxer, err := m.demuxerFactory(sctx, m.id)
		if err != nil || demuxer == nil {
			m.setLastCodecError(errors.Join(ErrRelayDemuxFactory, err))

			return
		}

		m.mu.Lock()
		m.demuxer = demuxer

		m.mu.Unlock()
		defer m.demuxer.Close()
		defer func(ctx context.Context) {
			if m.demuxerRemover != nil {
				ctxDetached := context.WithoutCancel(ctx)

				ctxTimeout, cancel := context.WithTimeout(ctxDetached, 5*time.Second)
				defer cancel()

				_ = m.demuxerRemover(ctxTimeout, m.id)
			}
		}(sctx)
		defer func() {
			m.mu.RLock()

			inactive := make(map[string]*Consumer, len(m.consumers))
			maps.Copy(inactive, m.consumers)

			m.mu.RUnlock()

			for _, c := range inactive {
				_ = c.Close()
			}

			m.mu.Lock()
			for consumerID := range m.consumers {
				delete(m.consumers, consumerID)
			}
			m.mu.Unlock()
		}()

		streams, err := m.demuxer.GetCodecs(sctx)
		if err != nil {
			m.setLastCodecError(err)

			return
		}

		m.mu.Lock()

		m.headers = cloneStreamHeaders(streams)
		select {
		case <-m.headersAvailable:
			// already closed
		default:
			close(m.headersAvailable)
		}
		m.mu.Unlock()

		m.pktBuf.WriteHeader(streams)

		m.wg.Go(func() {
			m.readWriteLoop(sctx)
		})

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.mu.RLock()

				inactive := make(map[string]*Consumer, len(m.consumers))
				for consumerID, c := range m.consumers {
					if !c.Inactive() {
						continue
					}

					inactive[consumerID] = c
				}

				m.mu.RUnlock()

				for _, c := range inactive {
					_ = c.Close()
				}

				m.mu.Lock()
				for consumerID := range inactive {
					delete(m.consumers, consumerID)
				}
				m.mu.Unlock()

			case <-sctx.Done():
				return
			}
		}
	})

	return nil
}

// Close cancels the relay's context and waits for all goroutines to exit.
// Calling Close multiple times is safe; subsequent calls are no-ops.
func (m *Relay) Close() error {
	if !m.alreadyClosing.CompareAndSwap(false, true) {
		return nil
	}

	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()

	if cancel != nil {
		cancel()
	}

	m.wg.Wait()
	if m.pktBuf != nil {
		m.pktBuf.Close()
	}

	return nil
}

// GetCodecs blocks until the demuxer has produced codec headers or the context
// is cancelled. Implements av.Demuxer.
func (m *Relay) GetCodecs(ctx context.Context) ([]av.Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.headersAvailable:
		m.mu.RLock()
		headers, headersErr := m.headers, m.headersErr
		m.mu.RUnlock()

		return headers, headersErr
	}
}

// ReadPacket returns the next packet from the underlying demuxer.
func (m *Relay) ReadPacket(ctx context.Context) (av.Packet, error) {
	return m.demuxer.ReadPacket(ctx)
}

// PacketBuffer returns the relay's packet buffer for recorded-to-live playback.
func (m *Relay) PacketBuffer() *packetbuf.Buffer {
	return m.pktBuf
}

// ConsumerCount returns the number of consumers currently registered on this relay.
func (m *Relay) ConsumerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.consumers)
}

// Stats returns a point-in-time snapshot of the producer's metrics.
func (m *Relay) Stats() av.RelayStats {
	m.mu.RLock()
	consumerCount := len(m.consumers)
	lastErr := m.headersErr
	startedAt := m.startedAt
	headers := m.headers

	var totalRotations, totalSkips uint64
	for _, c := range m.consumers {
		totalRotations += c.Rotations()
		totalSkips += c.Skips()
	}

	m.mu.RUnlock()

	errStr := ""
	if lastErr != nil {
		errStr = lastErr.Error()
	}

	var lastPacketAt time.Time
	if ns := m.lastPacketAtNs.Load(); ns > 0 {
		lastPacketAt = time.Unix(0, ns)
	}

	// Build per-stream info from codec headers.
	streams := make([]av.StreamInfo, 0, len(headers))

	for _, s := range headers {
		si := av.StreamInfo{
			Idx:       s.Idx,
			CodecType: s.Codec.Type(),
		}
		if vcd, ok := s.Codec.(av.VideoCodecData); ok {
			si.Width = vcd.Width()
			si.Height = vcd.Height()
		}

		if acd, ok := s.Codec.(av.AudioCodecData); ok {
			si.SampleRate = acd.SampleRate()
		}

		streams = append(streams, si)
	}

	// Compute average FPS (video only) and bitrate from counters.
	packetsRead := m.packetsRead.Load()
	videoPacketsRead := m.videoPacketsRead.Load()
	bytesRead := m.bytesRead.Load()

	var actualFPS, bitrateBps float64

	if !startedAt.IsZero() {
		elapsed := time.Since(startedAt).Seconds()
		if elapsed > 0 {
			actualFPS = float64(videoPacketsRead) / elapsed
			bitrateBps = float64(bytesRead) * 8 / elapsed
		}
	}

	return av.RelayStats{
		ID:             m.id,
		ConsumerCount:  consumerCount,
		PacketsRead:    packetsRead,
		BytesRead:      bytesRead,
		KeyFrames:      m.keyFrames.Load(),
		DroppedPackets: m.droppedPackets.Load(),
		StartedAt:      startedAt,
		LastPacketAt:   lastPacketAt,
		LastError:      errStr,
		Streams:        streams,
		ActualFPS:      actualFPS,
		BitrateBps:     bitrateBps,
		Rotations:      totalRotations,
		Skips:          totalSkips,
	}
}

func (m *Relay) LastError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.headersErr
}

// AddConsumer registers a new consumer on this relay, waits for codec headers,
// and sends WriteHeader to the consumer's muxer.
func (m *Relay) AddConsumer(
	ctx context.Context,
	consumerID string,
	muxerFactory av.MuxerFactory,
	muxerRemover av.MuxerRemover,
	errChan chan<- error,
) error {
	if m.alreadyClosing.Load() {
		return ErrRelayClosing
	}

	if !m.started.Load() {
		return ErrRelayNotStartedYet
	}

	if err := m.LastError(); err != nil {
		return err
	}

	m.mu.RLock()

	if m.maxConsumers > 0 && len(m.consumers) >= m.maxConsumers {
		m.mu.RUnlock()

		return ErrMaxConsumersReached
	}

	_, existed := m.consumers[consumerID]
	if existed {
		m.mu.RUnlock()

		return ErrConsumerAlreadyExists
	}

	sctx := m.sctx
	m.mu.RUnlock()

	if sctx == nil {
		return ErrRelayNotStartedYet
	}

	streams, err := m.GetCodecs(ctx)
	if err != nil {
		return errors.Join(ErrCodecsNotAvailable, err)
	}

	queueSize := 50
	if m.consumerQueueSize > 0 {
		queueSize = m.consumerQueueSize
	}

	c := NewConsumer(consumerID, muxerFactory, muxerRemover, errChan, queueSize)

	if err := c.Start(sctx); err != nil { //nolint:contextcheck
		c.inactive.Store(true)

		return ErrRelayClosing
	}

	if err := c.WriteHeader(ctx, streams); err != nil {
		return err
	}

	// Inject cached GOP so the consumer starts with a keyframe immediately
	// instead of waiting for the next one on the live stream.
	// This happens BEFORE the consumer is visible to readWriteLoop, ensuring
	// GOP packets are ordered before any live packets.
	gop := m.pktBuf.LastGOP()
	for _, pkt := range gop {
		_ = c.WritePacket(ctx, pkt)
	}

	// Mark the last injected DTS so the consumer skips any overlapping live
	// packets that readWriteLoop delivers after the consumer becomes visible.
	if len(gop) > 0 {
		c.SetSkipBefore(gop[len(gop)-1].DTS)
	}

	// Now make the consumer visible to readWriteLoop for live packet delivery.
	// Re-check closing state and duplicates under write lock.
	m.mu.Lock()

	if m.alreadyClosing.Load() {
		m.mu.Unlock()

		_ = c.Close()

		return ErrRelayClosing
	}

	if _, dup := m.consumers[consumerID]; dup {
		m.mu.Unlock()

		_ = c.Close()

		return ErrConsumerAlreadyExists
	}

	m.consumers[consumerID] = c
	m.mu.Unlock()

	return nil
}

// RemoveConsumer deregisters the consumer with the given ID and closes it.
func (m *Relay) RemoveConsumer(_ context.Context, consumerID string) error {
	m.mu.Lock()

	consumer, exists := m.consumers[consumerID]
	if exists {
		delete(m.consumers, consumerID)
	}
	m.mu.Unlock()

	if exists {
		_ = consumer.Close()
	}

	return nil
}

// Pause forwards a pause request to the underlying demuxer if it implements
// av.Pauser. Otherwise it is a no-op.
func (m *Relay) Pause(ctx context.Context) error {
	m.mu.RLock()
	dmx := m.demuxer
	m.mu.RUnlock()

	if pauser, ok := dmx.(av.Pauser); ok {
		return pauser.Pause(ctx)
	}

	return nil
}

// Resume forwards a resume request to the underlying demuxer if it implements
// av.Pauser. Otherwise it is a no-op.
func (m *Relay) Resume(ctx context.Context) error {
	m.mu.RLock()
	dmx := m.demuxer
	m.mu.RUnlock()

	if pauser, ok := dmx.(av.Pauser); ok {
		return pauser.Resume(ctx)
	}

	return nil
}

func (m *Relay) readWriteLoop(ctx context.Context) {
	fps := maxFps
	if m.maxFpsOverride > 0 {
		fps = m.maxFpsOverride
	}

	fpsLimitTicker := time.NewTicker(time.Second / time.Duration(fps))
	defer fpsLimitTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fpsLimitTicker.C:
			pkt, err := m.ReadPacket(ctx)
			if err != nil {
				m.mu.RLock()
				cancel := m.cancel
				m.mu.RUnlock()

				if cancel != nil {
					cancel()
				}

				return
			}

			m.packetsRead.Add(1)
			m.bytesRead.Add(uint64(len(pkt.Data)))

			if pkt.CodecType.IsVideo() {
				m.videoPacketsRead.Add(1)
			}

			if pkt.KeyFrame {
				m.keyFrames.Add(1)
			}

			m.lastPacketAtNs.Store(time.Now().UnixNano())

			if pkt.NewCodecs != nil {
				m.mu.Lock()
				m.headers = cloneStreamHeaders(pkt.NewCodecs)
				m.mu.Unlock()

				m.pktBuf.WriteHeader(pkt.NewCodecs)
			}

			m.pktBuf.WritePacket(pkt)

			m.mu.RLock()

			active := make([]*Consumer, 0, len(m.consumers))
			for _, c := range m.consumers {
				if c.LastError() != nil {
					continue
				}

				if c.Inactive() {
					continue
				}

				active = append(active, c)
			}

			m.mu.RUnlock()
			// Delivery policy:
			//   1 consumer  → blocking write (WritePacket).
			//     Back-pressure propagates up to ReadPacket, so no packets are
			//     dropped as long as the single consumer can keep up.
			//   2+ consumers → leaky write (WritePacketLeaky).
			//     A slow or stalled consumer does not block the others; it simply
			//     misses frames that do not fit in its queue.
			if len(active) == 1 {
				for _, c := range active {
					if c.ShouldSkip(pkt) {
						continue
					}

					_ = c.WritePacket(ctx, pkt)
				}

				continue
			}

			for _, c := range active {
				if c.ShouldSkip(pkt) || c.NeedsKeyframeRecovery(pkt) {
					continue
				}

				if err := c.WritePacketLeaky(ctx, pkt); errors.Is(err, ErrDroppingPacket) {
					m.droppedPackets.Add(1)
					c.needsKeyframe.Store(true)
					m.logDropRateLimited()
				}
			}
		}
	}
}

const dropLogInterval = 10 * time.Second

// logDropRateLimited emits a warning log when packets are being dropped, at
// most once per dropLogInterval to avoid log spam.
func (m *Relay) logDropRateLimited() {
	now := time.Now().UnixNano()
	last := m.lastDropLog.Load()

	if now-last < int64(dropLogInterval) {
		return
	}

	if m.lastDropLog.CompareAndSwap(last, now) {
		slog.Warn("relay: dropping packets for slow consumer(s)",
			"relay", m.id,
			"total_dropped", m.droppedPackets.Load(),
			"consumers", m.ConsumerCount(),
		)
	}
}

func (m *Relay) setLastCodecError(err error) {
	if err == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.headersErr = err
	select {
	case <-m.headersAvailable:
		// already closed
	default:
		close(m.headersAvailable)
	}
}
