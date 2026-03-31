package relayhub

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// consumerSeq is a process-wide monotonic counter used to generate unique
// consumer IDs when the caller does not supply one.
var consumerSeq atomic.Uint64 //nolint:gochecknoglobals

type RelayHub struct {
	demuxerFactory av.DemuxerFactory
	demuxerRemover av.DemuxerRemover

	cancel         context.CancelFunc
	wg             sync.WaitGroup
	mu             sync.RWMutex
	alreadyClosing atomic.Bool
	started        atomic.Bool
	relays         map[string]*Relay

	relaysToStart chan *Relay
}

type consumerHandle struct {
	hub        *RelayHub
	sourceID   string
	consumerID string
	closed     atomic.Bool
}

func New(
	demuxerFactory av.DemuxerFactory,
	demuxerRemover av.DemuxerRemover,
) *RelayHub {
	m := &RelayHub{
		demuxerFactory: demuxerFactory,
		demuxerRemover: demuxerRemover,
		relays:         make(map[string]*Relay),
		relaysToStart:  make(chan *Relay, 10),
	}

	return m
}

func (h *consumerHandle) ID() string {
	return h.consumerID
}

func (h *consumerHandle) Close(ctx context.Context) error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}

	err := h.hub.removeConsumer(ctx, h.sourceID, h.consumerID)
	if errors.Is(err, ErrRelayNotFound) {
		return nil
	}

	return err
}

func (m *RelayHub) Consume(
	ctx context.Context,
	sourceID string,
	opts av.ConsumeOptions,
) (av.ConsumerHandle, error) {
	if opts.ConsumerID == "" {
		opts.ConsumerID = fmt.Sprintf("consumer-%d", consumerSeq.Add(1))
	}

	if m.alreadyClosing.Load() {
		return nil, ErrRelayHubClosing
	}

	if !m.started.Load() {
		return nil, ErrRelayHubNotStartedYet
	}

	for {
		m.mu.Lock()

		p, existed := m.relays[sourceID]
		if !existed {
			p = NewRelay(sourceID, m.demuxerFactory, m.demuxerRemover)
			m.relays[sourceID] = p
		}
		m.mu.Unlock()

		if !existed {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case m.relaysToStart <- p:
			}
		}

		if err := p.LastError(); err != nil {
			return nil, fmt.Errorf(
				"sourceID: %s:\n%w",
				sourceID,
				errors.Join(ErrRelayLastError, err),
			)
		}

		if err := p.AddConsumer(
			ctx,
			opts.ConsumerID,
			opts.MuxerFactory,
			opts.MuxerRemover,
			opts.ErrChan,
		); err != nil {
			if errors.Is(err, ErrRelayClosing) || errors.Is(err, ErrRelayNotStartedYet) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}

				continue
			}

			return nil, fmt.Errorf("%s: %w", sourceID, err)
		}

		return &consumerHandle{
			hub:        m,
			sourceID:   sourceID,
			consumerID: opts.ConsumerID,
		}, nil
	}
}

func (m *RelayHub) removeConsumer(
	ctx context.Context,
	sourceID string,
	consumerID string,
) error {
	m.mu.RLock()
	p, ok := m.relays[sourceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s: %w", sourceID, ErrRelayNotFound)
	}

	return p.RemoveConsumer(ctx, consumerID)
}

func (m *RelayHub) GetActiveRelayCount(_ context.Context) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.relays)
}

func (m *RelayHub) GetRelayStats(_ context.Context) []av.RelayStats {
	m.mu.RLock()
	stats := make([]av.RelayStats, 0, len(m.relays))

	for _, p := range m.relays {
		stats = append(stats, p.Stats())
	}

	m.mu.RUnlock()

	return stats
}

func (m *RelayHub) PauseRelay(ctx context.Context, sourceID string) error {
	m.mu.RLock()
	p, ok := m.relays[sourceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s: %w", sourceID, ErrRelayNotFound)
	}

	return p.Pause(ctx)
}

func (m *RelayHub) ResumeRelay(ctx context.Context, sourceID string) error {
	m.mu.RLock()
	p, ok := m.relays[sourceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%s: %w", sourceID, ErrRelayNotFound)
	}

	return p.Resume(ctx)
}

func (m *RelayHub) Start(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return ErrRelayHubAlreadyStarted
	}

	sctx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	m.wg.Go(func() {
		defer cancel()
		defer func() {
			m.mu.RLock()

			inactive := make(map[string]*Relay, len(m.relays))
			maps.Copy(inactive, m.relays)

			m.mu.RUnlock()

			for _, p := range inactive {
				_ = p.Close()
			}

			m.mu.Lock()
			for sourceID := range m.relays {
				delete(m.relays, sourceID)
			}
			m.mu.Unlock()
		}()

		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				m.mu.RLock()

				inactive := make(map[string]*Relay, len(m.relays))
				for sourceID, p := range m.relays {
					if p.ConsumerCount() == 0 {
						inactive[sourceID] = p
					}
				}

				m.mu.RUnlock()

				for _, p := range inactive {
					_ = p.Close()
				}

				m.mu.Lock()
				for sourceID := range inactive {
					delete(m.relays, sourceID)
				}
				m.mu.Unlock()
			case <-sctx.Done():
				return
			case p, ok := <-m.relaysToStart:
				if ok {
					err := p.Start(sctx)
					if err != nil {
						m.mu.Lock()
						delete(m.relays, p.id)
						m.mu.Unlock()
					}
				}
			}
		}
	})

	return nil
}

func (m *RelayHub) SignalStop() bool {
	if !m.alreadyClosing.CompareAndSwap(false, true) {
		return false
	}

	m.mu.RLock()
	cancel := m.cancel
	m.mu.RUnlock()

	if cancel != nil {
		cancel()
	}

	return true
}

func (m *RelayHub) WaitStop() error {
	m.wg.Wait()

	return nil
}

func (m *RelayHub) Stop() error {
	if !m.SignalStop() {
		return nil
	}

	return m.WaitStop()
}
