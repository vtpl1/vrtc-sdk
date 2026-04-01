package relayhub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vtpl1/vrtc-sdk/av"
)

// Consumer buffers packets received from a Relay and drives an av.Muxer in a
// dedicated goroutine. It is created by Relay.AddConsumer and is not intended
// for direct use by callers outside the relayhub package.
type Consumer struct {
	id           string
	muxerFactory av.MuxerFactory
	muxerRemover av.MuxerRemover
	errCh        chan<- error

	// lifecycle: serialises Start vs Close (protects cancel and wg.Go/wg.Wait ordering)
	mu             sync.Mutex
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	alreadyClosing atomic.Bool
	started        atomic.Bool
	inactive       atomic.Bool
	writeOnce      sync.Once

	// data: protects headers and headersErr
	dataMu           sync.RWMutex
	headers          []av.Stream
	headersErr       error
	headersAvailable chan []av.Stream
	packets          chan av.Packet
}

// NewConsumer creates a Consumer for the given consumerID. Start must be called
// before any packets can be delivered.
func NewConsumer(
	consumerID string,
	muxerFactory av.MuxerFactory,
	muxerRemover av.MuxerRemover,
	errCh chan<- error,
) *Consumer {
	m := &Consumer{
		id:               consumerID,
		muxerFactory:     muxerFactory,
		muxerRemover:     muxerRemover,
		errCh:            errCh,
		headersAvailable: make(chan []av.Stream, 1),
		packets:          make(chan av.Packet, 50),
	}

	return m
}

// Start launches the goroutine that opens the muxer and delivers packets.
// It must be called exactly once; a second call returns ErrConsumerAlreadyStarted.
func (m *Consumer) Start(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return ErrConsumerAlreadyStarted
	}

	if m.alreadyClosing.Load() {
		return ErrConsumerClosing
	}

	sctx, cancel := context.WithCancel(ctx)

	m.mu.Lock()
	// Definitive check under lock: Close may have run between the early check
	// above and here. Checking inside the lock ensures wg.Go() never races
	// with Close's wg.Wait() (which also acquires m.mu before waiting).
	if m.alreadyClosing.Load() {
		m.mu.Unlock()
		cancel()

		return ErrConsumerClosing
	}

	m.cancel = cancel
	m.wg.Go(func() {
		defer cancel()
		defer func() {
			if m.muxerRemover != nil {
				ctxDetached := context.WithoutCancel(sctx)

				ctxTimeout, cancel := context.WithTimeout(ctxDetached, 5*time.Second)
				defer cancel()

				_ = m.muxerRemover(ctxTimeout, m.id)
			}
		}()
		defer m.inactive.Store(true)

		select {
		case <-sctx.Done():
			return
		case _, ok := <-m.headersAvailable:
			if !ok {
				return
			}

			muxer, err := m.muxerFactory(sctx, m.id)
			if err != nil || muxer == nil {
				m.setLastError(errors.Join(ErrConsumerMuxFactory, err))

				return
			}

			defer func() {
				ctxDetached := context.WithoutCancel(sctx)

				ctxTimeout, cancel := context.WithTimeout(ctxDetached, 5*time.Second)
				defer cancel()

				_ = muxer.WriteTrailer(ctxTimeout, nil)
				_ = muxer.Close()
			}()

			m.dataMu.RLock()
			streams := m.headers
			m.dataMu.RUnlock()

			if err := muxer.WriteHeader(sctx, streams); err != nil {
				m.setLastError(errors.Join(ErrMuxerWriteHeader, err))

				return
			}

			for {
				select {
				case <-sctx.Done():
					return
				case pkt, ok := <-m.packets:
					if !ok {
						return
					}

					if pkt.NewCodecs != nil {
						if cc, ok := muxer.(av.CodecChanger); ok {
							if err := cc.WriteCodecChange(sctx, pkt.NewCodecs); err != nil {
								m.setLastError(errors.Join(ErrMuxerWriteCodecChange, err))

								return
							}
						}
					}

					if err := muxer.WritePacket(sctx, pkt); err != nil {
						if !errors.Is(err, ErrMuxerRotate) {
							m.setLastError(errors.Join(ErrMuxerWritePacket, err))

							return
						}

						// Rotation: finalize old muxer, open a new one.
						_ = muxer.WriteTrailer(sctx, nil)
						_ = muxer.Close()

						newMuxer, fErr := m.muxerFactory(sctx, m.id)
						if fErr != nil || newMuxer == nil {
							m.setLastError(errors.Join(ErrConsumerMuxFactory, fErr))

							return
						}

						muxer = newMuxer

						m.dataMu.RLock()
						streams = m.headers
						m.dataMu.RUnlock()

						if err := muxer.WriteHeader(sctx, streams); err != nil {
							m.setLastError(errors.Join(ErrMuxerWriteHeader, err))

							return
						}

						// Re-deliver the keyframe packet that triggered rotation.
						if err := muxer.WritePacket(sctx, pkt); err != nil {
							m.setLastError(errors.Join(ErrMuxerWritePacket, err))

							return
						}
					}
				}
			}
		}
	})
	m.mu.Unlock()

	return nil
}

// Close marks the consumer inactive, cancels its context, and waits for the
// mux goroutine to exit. Calling Close multiple times is safe.
func (m *Consumer) Close() error {
	if !m.alreadyClosing.CompareAndSwap(false, true) {
		return nil
	}

	m.inactive.Store(true)
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	m.wg.Wait()

	return nil
}

func (m *Consumer) WriteHeader(ctx context.Context, streams []av.Stream) error {
	m.writeOnce.Do(func() {
		defer close(m.headersAvailable)

		if len(streams) == 0 {
			m.setLastError(ErrCodecsNotAvailable)

			return
		}

		_ = m.WriteCodecChange(ctx, streams)
		select {
		case <-ctx.Done():
		case m.headersAvailable <- streams:
		}
	})

	return m.LastError()
}

func (m *Consumer) WriteCodecChange(_ context.Context, changed []av.Stream) error {
	m.dataMu.Lock()
	defer m.dataMu.Unlock()

	m.headers = changed

	return nil
}

func (m *Consumer) WritePacket(ctx context.Context, pkt av.Packet) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.packets <- pkt:
	}

	return nil
}

func (m *Consumer) WritePacketLeaky(ctx context.Context, pkt av.Packet) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case m.packets <- pkt:
	default:
		return ErrDroppingPacket
	}

	return nil
}

func (m *Consumer) WriteTrailer(_ context.Context, _ error) error {
	return nil
}

func (m *Consumer) LastError() error {
	m.dataMu.RLock()
	defer m.dataMu.RUnlock()

	return m.headersErr
}

// Inactive reports whether the consumer has stopped processing packets, either
// because it was closed or because its muxer reported an error.
func (m *Consumer) Inactive() bool {
	return m.inactive.Load()
}

func (m *Consumer) setLastError(err error) {
	if err == nil {
		return
	}

	m.dataMu.Lock()
	defer m.dataMu.Unlock()

	m.headersErr = err
	if m.errCh == nil {
		return
	}

	select {
	case m.errCh <- err:
	default:
	}

	m.inactive.Store(true)
}
