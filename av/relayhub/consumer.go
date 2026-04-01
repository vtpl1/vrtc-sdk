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
		m.run(sctx, cancel)
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

	if len(changed) == 0 {
		m.headers = nil

		return nil
	}

	m.headers = append([]av.Stream(nil), changed...)

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

func (m *Consumer) run(sctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	defer m.cleanupRemover(sctx)
	defer m.inactive.Store(true)

	if !m.waitForHeaders(sctx) {
		return
	}

	muxer, err := m.openAndInitMuxer(sctx)
	if err != nil {
		m.setLastError(err)

		return
	}

	defer func() {
		m.closeMuxer(sctx, muxer)
	}()

	if err := m.packetLoop(sctx, &muxer); err != nil {
		m.setLastError(err)
	}
}

func (m *Consumer) cleanupRemover(sctx context.Context) {
	if m.muxerRemover == nil {
		return
	}

	ctxDetached := context.WithoutCancel(sctx)

	ctxTimeout, cancel := context.WithTimeout(ctxDetached, 5*time.Second)
	defer cancel()

	_ = m.muxerRemover(ctxTimeout, m.id)
}

func (m *Consumer) waitForHeaders(sctx context.Context) bool {
	select {
	case <-sctx.Done():
		return false
	case _, ok := <-m.headersAvailable:
		return ok
	}
}

func (m *Consumer) openAndInitMuxer(sctx context.Context) (av.MuxCloser, error) {
	muxer, err := m.muxerFactory(sctx, m.id)
	if err != nil || muxer == nil {
		return nil, errors.Join(ErrConsumerMuxFactory, err)
	}

	if err := m.writeMuxerHeader(sctx, muxer); err != nil {
		m.closeMuxer(sctx, muxer)

		return nil, err
	}

	return muxer, nil
}

func (m *Consumer) writeMuxerHeader(ctx context.Context, muxer av.MuxCloser) error {
	if err := muxer.WriteHeader(ctx, m.currentStreams()); err != nil {
		return errors.Join(ErrMuxerWriteHeader, err)
	}

	return nil
}

func (m *Consumer) currentStreams() []av.Stream {
	m.dataMu.RLock()
	defer m.dataMu.RUnlock()

	return m.headers
}

func (m *Consumer) packetLoop(sctx context.Context, muxer *av.MuxCloser) error {
	for {
		select {
		case <-sctx.Done():
			return nil
		case pkt, ok := <-m.packets:
			if !ok {
				return nil
			}

			if err := m.handlePacket(sctx, muxer, pkt); err != nil {
				return err
			}
		}
	}
}

func (m *Consumer) handlePacket(sctx context.Context, muxer *av.MuxCloser, pkt av.Packet) error {
	if pkt.NewCodecs != nil {
		if cc, ok := (*muxer).(av.CodecChanger); ok {
			if err := cc.WriteCodecChange(sctx, pkt.NewCodecs); err != nil {
				return errors.Join(ErrMuxerWriteCodecChange, err)
			}
		}
	}

	if err := (*muxer).WritePacket(sctx, pkt); err != nil {
		if !errors.Is(err, ErrMuxerRotate) {
			return errors.Join(ErrMuxerWritePacket, err)
		}

		return m.rotateMuxer(sctx, muxer, pkt)
	}

	return nil
}

func (m *Consumer) rotateMuxer(sctx context.Context, muxer *av.MuxCloser, pkt av.Packet) error {
	m.closeMuxer(sctx, *muxer)

	newMuxer, err := m.openAndInitMuxer(sctx)
	if err != nil {
		return err
	}

	if err := newMuxer.WritePacket(sctx, pkt); err != nil {
		m.closeMuxer(sctx, newMuxer)

		return errors.Join(ErrMuxerWritePacket, err)
	}

	*muxer = newMuxer

	return nil
}

func (m *Consumer) closeMuxer(sctx context.Context, muxer av.MuxCloser) {
	ctxDetached := context.WithoutCancel(sctx)

	ctxTimeout, cancel := context.WithTimeout(ctxDetached, 5*time.Second)
	defer cancel()

	_ = muxer.WriteTrailer(ctxTimeout, nil)
	_ = muxer.Close()
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
