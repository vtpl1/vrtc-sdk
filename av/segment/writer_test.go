package segment_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vtpl1/vrtc-sdk/av/segment"
)

func TestAdaptiveWriter_WriteAndBytesWritten(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "test.mp4")

	w, err := segment.NewAdaptiveWriter(path, segment.ProfileSSD, 0, false)
	if err != nil {
		t.Fatalf("NewAdaptiveWriter: %v", err)
	}

	data := []byte("hello world, this is segment data")

	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}

	if got := w.BytesWritten(); got != int64(len(data)) {
		t.Errorf("BytesWritten = %d, want %d", got, len(data))
	}

	// Write more data and verify cumulative count.
	more := []byte("more data")

	n2, err := w.Write(more)
	if err != nil {
		t.Fatalf("Write(more): %v", err)
	}

	expected := int64(n + n2)
	if got := w.BytesWritten(); got != expected {
		t.Errorf("BytesWritten = %d, want %d", got, expected)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify file on disk matches.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	want := string(data) + string(more)
	if string(contents) != want {
		t.Errorf("file contents = %q, want %q", contents, want)
	}
}

func TestAdaptiveWriter_CloseFlushesToDisk(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "flush.mp4")

	w, err := segment.NewAdaptiveWriter(path, segment.ProfileSSD, 0, false)
	if err != nil {
		t.Fatalf("NewAdaptiveWriter: %v", err)
	}

	data := []byte("buffered content")

	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if info.Size() != int64(len(data)) {
		t.Errorf("file size = %d, want %d", info.Size(), len(data))
	}
}

func TestAdaptiveWriter_Profiles(t *testing.T) {
	t.Parallel()

	profiles := []segment.StorageProfile{
		segment.ProfileSSD,
		segment.ProfileHDD,
		segment.ProfileNAS,
		segment.ProfileSAN,
		segment.ProfileAuto,
	}

	for _, p := range profiles {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "test.mp4")

			w, err := segment.NewAdaptiveWriter(path, p, 0, false)
			if err != nil {
				t.Fatalf("NewAdaptiveWriter(%s): %v", p, err)
			}

			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write: %v", err)
			}

			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestAdaptiveWriter_InvalidPath(t *testing.T) {
	t.Parallel()

	_, err := segment.NewAdaptiveWriter(
		filepath.Join(t.TempDir(), "nonexistent", "dir", "test.mp4"),
		segment.ProfileSSD, 0, false,
	)
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}
