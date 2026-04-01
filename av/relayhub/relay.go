package relayhub

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
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

	// metrics — updated from readWriteLoop; read via Stats()
	packetsRead    atomic.Uint64
	bytesRead      atomic.Uint64
	keyFrames      atomic.Uint64
	droppedPackets atomic.Uint64
	lastPacketAtNs atomic.Int64 // unix nanoseconds; 0 = no packet yet
	startedAt      time.Time    // set once in Start; zero until Start is called
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

		m.headers = streams
		select {
		case <-m.headersAvailable:
			// already closed
		default:
			close(m.headersAvailable)
		}
		m.mu.Unlock()

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
	var streams []av.StreamInfo
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

	// Compute average FPS and bitrate from counters.
	packetsRead := m.packetsRead.Load()
	bytesRead := m.bytesRead.Load()
	var actualFPS, bitrateBps float64
	if !startedAt.IsZero() {
		elapsed := time.Since(startedAt).Seconds()
		if elapsed > 0 {
			actualFPS = float64(packetsRead) / elapsed
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

	m.mu.Lock()

	_, existed := m.consumers[consumerID]
	if existed {
		m.mu.Unlock()

		return ErrConsumerAlreadyExists
	}

	c := NewConsumer(consumerID, muxerFactory, muxerRemover, errChan)
	m.consumers[consumerID] = c
	m.mu.Unlock()

	streams, err := m.GetCodecs(ctx)
	if err != nil {
		m.mu.Lock()
		delete(m.consumers, consumerID)
		m.mu.Unlock()
		c.setLastError(errors.Join(ErrCodecsNotAvailable, err))

		return err
	}

	// Start the consumer directly using the producer's stored context.
	// If the producer is closing (sctx already cancelled or alreadyClosing set),
	// Consumer.Start detects this and returns an error without spawning a goroutine.
	m.mu.RLock()
	sctx := m.sctx
	m.mu.RUnlock()

	if err := c.Start(sctx); err != nil { //nolint:contextcheck
		c.inactive.Store(true)
		m.mu.Lock()
		delete(m.consumers, consumerID)
		m.mu.Unlock()

		return ErrRelayClosing
	}

	return c.WriteHeader(ctx, streams)
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
	fpsLimitTicker := time.NewTicker(time.Second / time.Duration(maxFps))
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

			if pkt.KeyFrame {
				m.keyFrames.Add(1)
			}

			m.lastPacketAtNs.Store(time.Now().UnixNano())

			if pkt.NewCodecs != nil {
				m.mu.Lock()
				m.headers = pkt.NewCodecs
				m.mu.Unlock()
			}

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
					_ = c.WritePacket(ctx, pkt)
				}

				continue
			}

			for _, c := range active {
				if err := c.WritePacketLeaky(ctx, pkt); errors.Is(err, ErrDroppingPacket) {
					m.droppedPackets.Add(1)
				}
			}
		}
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
