package segment_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vtpl1/vrtc-sdk/av/segment"
)

func TestRingBuffer_PushAndLen(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	if rb.Len() != 0 {
		t.Fatalf("expected Len 0, got %d", rb.Len())
	}

	rb.Push(segment.Fragment{Data: []byte("a"), Timestamp: time.Now()})
	rb.Push(segment.Fragment{Data: []byte("bb"), Timestamp: time.Now()})

	if rb.Len() != 2 {
		t.Fatalf("expected Len 2, got %d", rb.Len())
	}
}

func TestRingBuffer_SizeBytes(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	rb.Push(segment.Fragment{Data: make([]byte, 100), Timestamp: time.Now()})
	rb.Push(segment.Fragment{Data: make([]byte, 250), Timestamp: time.Now()})

	if got := rb.SizeBytes(); got != 350 {
		t.Fatalf("expected SizeBytes 350, got %d", got)
	}
}

func TestRingBuffer_ReadFrom(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	now := time.Now()

	rb.Push(segment.Fragment{DTS: 0, Data: []byte("f0"), Timestamp: now})
	rb.Push(segment.Fragment{DTS: 100 * time.Millisecond, Data: []byte("f1"), Timestamp: now})
	rb.Push(segment.Fragment{DTS: 200 * time.Millisecond, Data: []byte("f2"), Timestamp: now})
	rb.Push(segment.Fragment{DTS: 300 * time.Millisecond, Data: []byte("f3"), Timestamp: now})

	// Read from DTS >= 150ms should return f2, f3.
	frags := rb.ReadFrom(150 * time.Millisecond)
	if len(frags) != 2 {
		t.Fatalf("expected 2 fragments, got %d", len(frags))
	}

	if string(frags[0].Data) != "f2" {
		t.Errorf("expected first fragment data %q, got %q", "f2", frags[0].Data)
	}

	if string(frags[1].Data) != "f3" {
		t.Errorf("expected second fragment data %q, got %q", "f3", frags[1].Data)
	}
}

func TestRingBuffer_ReadFrom_NoMatch(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	now := time.Now()

	rb.Push(segment.Fragment{DTS: 0, Timestamp: now})
	rb.Push(segment.Fragment{DTS: 100 * time.Millisecond, Timestamp: now})

	frags := rb.ReadFrom(500 * time.Millisecond)
	if frags != nil {
		t.Fatalf("expected nil, got %d fragments", len(frags))
	}
}

func TestRingBuffer_ReadFrom_Empty(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	frags := rb.ReadFrom(0)
	if frags != nil {
		t.Fatal("expected nil from empty ring buffer")
	}
}

func TestRingBuffer_ReadFrom_ReturnsCopy(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)
	rb.Push(segment.Fragment{DTS: 0, Data: []byte("orig"), Timestamp: time.Now()})

	frags := rb.ReadFrom(0)
	frags[0].Data = []byte("modified")

	// Internal data should be unchanged.
	frags2 := rb.ReadFrom(0)
	if string(frags2[0].Data) != "orig" {
		t.Errorf("internal data modified: got %q, want %q", frags2[0].Data, "orig")
	}
}

func TestRingBuffer_Eviction(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(50 * time.Millisecond)

	old := time.Now().Add(-100 * time.Millisecond)
	rb.Push(segment.Fragment{DTS: 0, Data: []byte("old"), Timestamp: old})

	// Push a new fragment; the old one should be evicted.
	rb.Push(segment.Fragment{DTS: 100 * time.Millisecond, Data: []byte("new"), Timestamp: time.Now()})

	if rb.Len() != 1 {
		t.Fatalf("expected 1 fragment after eviction, got %d", rb.Len())
	}

	frags := rb.ReadFrom(0)
	if string(frags[0].Data) != "new" {
		t.Errorf("expected remaining fragment %q, got %q", "new", frags[0].Data)
	}
}

func TestRingBuffer_Wait_WakesOnPush(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	done := make(chan error, 1)

	go func() {
		done <- rb.Wait(context.Background())
	}()

	// Give goroutine time to block.
	time.Sleep(10 * time.Millisecond)

	rb.Push(segment.Fragment{Timestamp: time.Now()})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Push")
	}
}

func TestRingBuffer_Wait_CancelledContext(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(10 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- rb.Wait(ctx)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestRingBuffer_ConcurrentPushRead(t *testing.T) {
	t.Parallel()

	rb := segment.NewRingBuffer(5 * time.Second)

	const writers = 4
	const pushesPerWriter = 100

	var wg sync.WaitGroup

	wg.Add(writers)

	for range writers {
		go func() {
			defer wg.Done()

			for i := range pushesPerWriter {
				rb.Push(segment.Fragment{
					DTS:       time.Duration(i) * time.Millisecond,
					Data:      make([]byte, 64),
					Timestamp: time.Now(),
				})
			}
		}()
	}

	// Concurrent readers.
	wg.Add(writers)

	for range writers {
		go func() {
			defer wg.Done()

			for range pushesPerWriter {
				_ = rb.ReadFrom(0)
				_ = rb.Len()
				_ = rb.SizeBytes()
			}
		}()
	}

	wg.Wait()
}
