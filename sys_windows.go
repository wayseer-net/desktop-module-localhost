package localhost

import (
	"errors"
	"net"
	"os"
	"time"
	"unsafe"

	"wayseer.dev/sdk"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const supported = true

var errUnsupported error

// The parsers' offsets, checked against x/sys's structures: a mismatch does not compile.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(windows.MibIfRow2{})-ifRow2Size]
	_ = [1]struct{}{}[unsafe.Offsetof(windows.MibIfRow2{}.InterfaceIndex)-ifRow2Index]
	_ = [1]struct{}{}[unsafe.Offsetof(windows.MibIfRow2{}.InOctets)-ifRow2InOctets]
	_ = [1]struct{}{}[unsafe.Offsetof(windows.MibIfRow2{}.OutOctets)-ifRow2OutOctets]
	_ = [1]struct{}{}[unsafe.Offsetof(windows.MibIfTable2{}.Table)-ifTable2Rows]
)

// statfs reports a volume's size, for a copied Linux tree read on Windows.
func statfs(path string) (fsUsage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fsUsage{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return fsUsage{}, err
	}
	return fsUsage{total: total, used: total - min(free, total), avail: avail}, nil
}

// nativeSource reads Windows through Win32 calls.
func nativeSource(r *reader) (source, []sdk.Metric) {
	return newWinReader(liveWindows{}, r), winCatalogue()
}

// Calls x/sys/windows lacks, from kernel32.
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// liveWindows makes the calls on this machine.
type liveWindows struct{}

func (liveWindows) hostname() (string, error) { return os.Hostname() }

func (liveWindows) version() winVersion {
	v := windows.RtlGetVersion()
	out := winVersion{major: v.MajorVersion, minor: v.MinorVersion, build: v.BuildNumber}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return out
	}
	defer k.Close() //nolint:errcheck // a read-only key
	out.product, _, _ = k.GetStringValue("ProductName")
	out.display, _, _ = k.GetStringValue("DisplayVersion")
	return out
}

func (liveWindows) sinceBoot() time.Duration { return windows.DurationSinceBoot() }

func (liveWindows) systemTimes() (idle, kernel, user uint64, err error) {
	var i, k, u windows.Filetime
	r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, 0, e
	}
	return filetime(i), filetime(k), filetime(u), nil
}

// filetime is a FILETIME's count of 100 ns units.
func filetime(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// processorTimes covers the CPUs of this process's processor group, up to 64.
func (liveWindows) processorTimes() ([]byte, error) {
	b := make([]byte, 64*procPerfSize)
	var n uint32
	err := windows.NtQuerySystemInformation(windows.SystemProcessorPerformanceInformation, unsafe.Pointer(&b[0]), uint32(len(b)), &n)
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}

func (liveWindows) memoryStatus() ([]byte, error) {
	b := make([]byte, memStatusSize)
	le.PutUint32(b, memStatusSize) // dwLength
	if r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&b[0]))); r == 0 {
		return nil, e
	}
	return b, nil
}

func (liveWindows) driveStrings() ([]uint16, error) {
	b := make([]uint16, 512) // 26 letters of 4 characters each, with room to spare
	n, err := windows.GetLogicalDriveStrings(uint32(len(b)), &b[0])
	if err != nil {
		return nil, err
	}
	if int(n) > len(b) {
		return nil, errors.New("GetLogicalDriveStringsW: more drives than fit")
	}
	return b[:n], nil
}

func (liveWindows) driveType(root string) uint32 {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	return windows.GetDriveType(p)
}

func (liveWindows) fileSystem(root string) (string, error) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", err
	}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(p, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(name), nil
}

func (liveWindows) diskFree(root string) (fsUsage, error) { return statfs(root) }

func (liveWindows) interfaces() ([]net.Interface, error) { return net.Interfaces() }

func (liveWindows) ifTable() ([]byte, error) {
	var t *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &t); err != nil {
		return nil, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	size := ifTable2Rows + int(t.NumEntries)*ifRow2Size
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(t)), size)...), nil
}
