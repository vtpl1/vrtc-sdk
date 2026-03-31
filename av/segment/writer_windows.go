package segment

import "os"

// fallocate is a no-op on Windows; the OS handles file allocation.
func fallocate(_ *os.File, _ int64) {}

// DetectProfile returns ProfileSSD on Windows as the default.
// Windows does not expose rotational status without WMI queries,
// so we default to SSD (the safest and most common modern choice).
func DetectProfile(_ string) StorageProfile {
	return ProfileSSD
}
