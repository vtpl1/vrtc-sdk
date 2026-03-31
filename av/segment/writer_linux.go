package segment

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// fallocate preallocates disk space for f on Linux, avoiding fragmentation.
func fallocate(f *os.File, size int64) {
	_ = syscall.Fallocate(int(f.Fd()), 0, 0, size)
}

// DetectProfile inspects the filesystem type and device characteristics at path
// to choose an appropriate StorageProfile.
func DetectProfile(path string) StorageProfile {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return ProfileSSD // safe default
	}

	// NFS magic number: 0x6969
	if stat.Type == 0x6969 {
		return ProfileNAS
	}

	// SMB/CIFS magic number: 0xFF534D42
	if stat.Type == 0xFF534D42 {
		return ProfileNAS
	}

	// Local filesystem — check if the underlying device is rotational.
	if isRotational(path) {
		return ProfileHDD
	}

	return ProfileSSD
}

// isRotational checks /sys/block/<dev>/queue/rotational to determine if the
// device backing path is a spinning disk.
func isRotational(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	var stat syscall.Stat_t
	if err := syscall.Stat(abs, &stat); err != nil {
		return false
	}

	// Extract major device number.
	major := (stat.Dev >> 8) & 0xFF

	// Scan /sys/block/ for a device with matching major number.
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return false
	}

	for _, e := range entries {
		devPath := filepath.Join("/sys/block", e.Name(), "dev")

		data, err := os.ReadFile(devPath)
		if err != nil {
			continue
		}

		devStr := strings.TrimSpace(string(data))
		// dev file format: "major:minor"
		parts := strings.SplitN(devStr, ":", 2)
		if len(parts) != 2 {
			continue
		}

		if parts[0] != strconv.FormatUint(major, 10) {
			continue
		}

		rotPath := filepath.Join("/sys/block", e.Name(), "queue", "rotational")

		rotData, err := os.ReadFile(rotPath)
		if err != nil {
			continue
		}

		return strings.TrimSpace(string(rotData)) == "1"
	}

	return false
}
