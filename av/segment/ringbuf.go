package segment

import (
	"context"
	"sync"
	"time"
)

// Fragment is one fMP4 fragment (moof + mdat) stored in the ring buffer.
type Fragment struct {
	DTS       time.Duration // decode timestamp of the first sample
	Duration  time.Duration // total duration of the fragment
	KeyFrame  bool          // true if this fragment starts with a keyframe
	Data      []byte        // raw fMP4 bytes (moof + mdat, possibly preceded by emsg)
	Timestamp time.Time     // wall-clock time when the fragment was captured
}

// RingBuffer holds the most recent fragments in memory for near-live playback.
// It is safe for concurrent use by one writer and multiple readers.
type RingBuffer struct {
	mu        sync.RWMutex
	fragments []Fragment
	maxAge    time.Duration
	notify    chan struct{} // closed and replaced on each Push to wake waiters
}

// NewRingBuffer creates a ring buffer that retains fragments for up to maxAge.
func NewRingBuffer(maxAge time.Duration) *RingBuffer {
	return &RingBuffer{
		maxAge: maxAge,
		notify: make(chan struct{}),
	}
}

// Push appends a fragment and evicts any fragments older than maxAge.
// It wakes all goroutines blocked in Wait.
func (rb *RingBuffer) Push(frag Fragment) {
	rb.mu.Lock()

	rb.fragments = append(rb.fragments, frag)

	// Evict expired fragments from the front.
	cutoff := time.Now().Add(-rb.maxAge)

	i := 0
	for i < len(rb.fragments) && rb.fragments[i].Timestamp.Before(cutoff) {
		i++
	}

	if i > 0 {
		// Slide remaining fragments to the front to allow GC of evicted data.
		n := copy(rb.fragments, rb.fragments[i:])
		// Clear trailing references so the GC can collect the old Data slices.
		for j := n; j < len(rb.fragments); j++ {
			rb.fragments[j] = Fragment{}
		}

		rb.fragments = rb.fragments[:n]
	}

	// Wake all waiters by closing the current channel and creating a new one.
	old := rb.notify
	rb.notify = make(chan struct{})
	rb.mu.Unlock()

	close(old)
}

// ReadFrom returns all fragments with DTS >= fromDTS, in chronological order.
// Returns nil if no matching fragments exist.
func (rb *RingBuffer) ReadFrom(fromDTS time.Duration) []Fragment {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	// Linear scan to find the first fragment with DTS >= fromDTS.
	idx := -1

	for i, f := range rb.fragments {
		if f.DTS >= fromDTS {
			idx = i

			break
		}
	}

	if idx < 0 {
		return nil
	}

	// Return a copy so callers don't hold a reference to the internal slice.
	out := make([]Fragment, len(rb.fragments)-idx)
	copy(out, rb.fragments[idx:])

	return out
}

// Len returns the number of fragments currently buffered.
func (rb *RingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	return len(rb.fragments)
}

// SizeBytes returns the total byte size of all buffered fragments.
func (rb *RingBuffer) SizeBytes() int64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	var total int64
	for i := range rb.fragments {
		total += int64(len(rb.fragments[i].Data))
	}

	return total
}

// Wait blocks until a new fragment is pushed or ctx is cancelled.
// Returns ctx.Err() on cancellation, nil on new fragment.
func (rb *RingBuffer) Wait(ctx context.Context) error {
	rb.mu.RLock()
	ch := rb.notify
	rb.mu.RUnlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
