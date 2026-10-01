package localhost

import (
	"mindseye/pkg/sdk"
	"syscall"
)

const supported = true

var errUnsupported error

// statfs reports a filesystem's size; used excludes the blocks reserved for root.
func statfs(path string) (fsUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsUsage{}, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // block sizes are small and positive
	return fsUsage{total: st.Blocks * bs, used: (st.Blocks - st.Bfree) * bs, avail: st.Bavail * bs}, nil
}

// nativeSource is nil: Linux is read through /proc and /sys.
func nativeSource(*reader) (source, []sdk.Metric) { return nil, nil }
