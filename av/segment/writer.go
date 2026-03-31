package segment

import (
	"bufio"
	"fmt"
	"os"
	"sync/atomic"
)

// StorageProfile selects I/O tuning parameters for the target storage device.
type StorageProfile string

const (
	// ProfileAuto detects the storage type from the filesystem and device.
	ProfileAuto StorageProfile = "auto"
	// ProfileSSD uses small buffers and skips preallocation (no write amplification penalty).
	ProfileSSD StorageProfile = "ssd"
	// ProfileHDD uses large buffers and preallocates files to avoid fragmentation.
	ProfileHDD StorageProfile = "hdd"
	// ProfileNAS uses large buffers and retries on transient network errors.
	ProfileNAS StorageProfile = "nas"
	// ProfileSAN uses moderate buffers; treats the block device like local storage.
	ProfileSAN StorageProfile = "san"
)

// Buffer sizes per storage profile.
const (
	bufSizeSSD = 256 * 1024      // 256 KB
	bufSizeHDD = 1 * 1024 * 1024 // 1 MB
	bufSizeNAS = 4 * 1024 * 1024 // 4 MB
	bufSizeSAN = 512 * 1024      // 512 KB

	// nasMaxRetries is the number of retries for transient NAS write errors.
	nasMaxRetries = 3
)

// AdaptiveWriter wraps an os.File with a storage-profile-aware buffered writer.
// It tracks bytes written for size-based segment rotation, and optionally
// preallocates disk space (HDD) to avoid fragmentation.
type AdaptiveWriter struct {
	f            *os.File
	bw           *bufio.Writer
	profile      StorageProfile
	written      atomic.Int64 // bytes flushed to the underlying file
	preallocated int64        // preallocated size in bytes (0 = none)
	closed       bool
}

// NewAdaptiveWriter creates a segment file at path with storage-optimised buffering.
// If profile is ProfileAuto, it is resolved via DetectProfile.
// preallocBytes requests disk preallocation (effective only on HDD/SAN + Linux).
func NewAdaptiveWriter(
	path string,
	profile StorageProfile,
	preallocBytes int64,
) (*AdaptiveWriter, error) {
	if profile == ProfileAuto {
		profile = DetectProfile(path)
	}

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("segment: create segment file %q: %w", path, err)
	}

	// Preallocate on HDD/SAN to avoid fragmentation (no-op on unsupported platforms).
	if preallocBytes > 0 && (profile == ProfileHDD || profile == ProfileSAN) {
		fallocate(f, preallocBytes) // best-effort; errors are ignored
	}

	bufSize := bufSizeForProfile(profile)

	return &AdaptiveWriter{
		f:            f,
		bw:           bufio.NewWriterSize(f, bufSize),
		profile:      profile,
		preallocated: preallocBytes,
	}, nil
}

// Write writes p through the buffered writer. For NAS profile, transient write
// errors are retried up to nasMaxRetries times.
func (w *AdaptiveWriter) Write(p []byte) (int, error) {
	if w.profile == ProfileNAS {
		return w.writeWithRetry(p)
	}

	n, err := w.bw.Write(p)
	w.written.Add(int64(n))

	return n, err
}

// BytesWritten returns the total bytes written so far (thread-safe).
func (w *AdaptiveWriter) BytesWritten() int64 {
	return w.written.Load()
}

// Close flushes the buffer, syncs to stable storage, truncates any preallocated
// unused space, and closes the file. Safe to call multiple times.
func (w *AdaptiveWriter) Close() error {
	if w.closed {
		return nil
	}

	w.closed = true

	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()

		return fmt.Errorf("segment: flush: %w", err)
	}

	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()

		return fmt.Errorf("segment: fsync: %w", err)
	}

	// Truncate preallocated file to actual written size.
	if w.preallocated > 0 {
		actual := w.written.Load()
		if actual < w.preallocated {
			_ = w.f.Truncate(actual) // best-effort
		}
	}

	return w.f.Close()
}

// writeWithRetry retries on transient I/O errors (NAS profile).
func (w *AdaptiveWriter) writeWithRetry(p []byte) (int, error) {
	var lastErr error

	for range nasMaxRetries {
		n, err := w.bw.Write(p)
		w.written.Add(int64(n))

		if err == nil {
			return n, nil
		}

		lastErr = err
	}

	return 0, fmt.Errorf("segment: NAS write failed after %d retries: %w", nasMaxRetries, lastErr)
}

func bufSizeForProfile(p StorageProfile) int {
	switch p {
	case ProfileSSD:
		return bufSizeSSD
	case ProfileHDD:
		return bufSizeHDD
	case ProfileNAS:
		return bufSizeNAS
	case ProfileSAN:
		return bufSizeSAN
	case ProfileAuto:
		return bufSizeSSD
	}

	return bufSizeSSD
}
