package localhost

import (
	"errors"
	"net"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
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

// nativeSource reads Windows through Win32 calls, and its services from the service control
// manager.
func nativeSource(r *reader) platform {
	live := &liveWindows{accounts: map[string]string{}}
	return platform{src: newWinReader(live, r), metrics: winCatalogue(), units: scmSystem{open: openSCM}}
}

// Calls x/sys/windows lacks, from kernel32.
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

// Caps on what a call may need, beyond which its answer is refused.
const (
	servicesCap    = 16 << 20
	commandLineCap = 1<<16 + 16 // the longest command line Windows allows, with its header
)

// liveWindows makes the calls on this machine.
type liveWindows struct {
	accounts map[string]string // by SID, as a lookup can wait on a domain controller
}

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

func (liveWindows) processes() ([]winProcess, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // a snapshot handle
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var out []winProcess
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out = append(out, winProcess{pid: e.ProcessID, ppid: e.ParentProcessID, exe: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

// queried runs fn on a process opened to query, which another user's or a protected one refuses.
func queried[T any](pid uint32, fn func(windows.Handle) (T, error)) (T, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		var zero T
		return zero, err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // a query handle
	return fn(h)
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb, pageFaultCount                                 uint32
	peakWorkingSetSize, workingSetSize                 uintptr
	quotaPeakPagedPoolUsage, quotaPagedPoolUsage       uintptr
	quotaPeakNonPagedPoolUsage, quotaNonPagedPoolUsage uintptr
	pagefileUsage, peakPagefileUsage                   uintptr
}

func (liveWindows) processTimes(pid uint32) (winProcTimes, error) {
	return queried(pid, func(h windows.Handle) (winProcTimes, error) {
		var created, exited, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
			return winProcTimes{}, err
		}
		mc := processMemoryCounters{cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
		if r, _, e := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.cb)); r == 0 {
			return winProcTimes{}, e
		}
		return winProcTimes{
			created: time.Unix(0, created.Nanoseconds()), cpu: filetime(kernel) + filetime(user),
			workingSet: uint64(mc.workingSetSize),
		}, nil
	})
}

// processUser is the account the process runs as, without its domain, as Task Manager shows it.
func (w *liveWindows) processUser(pid uint32) (string, error) {
	return queried(pid, func(h windows.Handle) (string, error) {
		var tok windows.Token
		if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
			return "", err
		}
		defer tok.Close() //nolint:errcheck // a query handle
		u, err := tok.GetTokenUser()
		if err != nil {
			return "", err
		}
		sid := u.User.Sid.String()
		if name, ok := w.accounts[sid]; ok {
			return name, nil
		}
		name, _, _, err := u.User.Sid.LookupAccount("")
		if err != nil {
			name = sid
		}
		w.accounts[sid] = name
		return name, nil
	})
}

func (liveWindows) processCommand(pid uint32) (string, error) {
	return queried(pid, func(h windows.Handle) (string, error) {
		var n uint32
		b := make([]uint64, 64) // 8-byte aligned for the UNICODE_STRING
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&b[0]), uint32(len(b)*8), &n)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) && n <= commandLineCap {
			b = make([]uint64, (n+7)/8)
			err = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&b[0]), uint32(len(b)*8), &n)
		}
		if err != nil {
			return "", err
		}
		return (*windows.NTUnicodeString)(unsafe.Pointer(&b[0])).String(), nil
	})
}

// openSCM connects to the service control manager to list and query, which needs no elevation.
func openSCM() (scmAPI, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	return liveSCM{h}, nil
}

type liveSCM struct{ h windows.Handle }

func (s liveSCM) services() ([]winService, error) {
	var needed, count uint32
	b := make([]uint64, 8<<10)
	for {
		err := windows.EnumServicesStatusEx(s.h, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL,
			(*byte)(unsafe.Pointer(&b[0])), uint32(len(b)*8), &needed, &count, nil, nil)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_MORE_DATA) || needed <= uint32(len(b)*8) || needed > servicesCap {
			return nil, err
		}
		b = make([]uint64, (needed+7)/8)
	}
	out := make([]winService, 0, count)
	for _, e := range unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&b[0])), count) {
		st := e.ServiceStatusProcess
		out = append(out, winService{
			name: windows.UTF16PtrToString(e.ServiceName), display: windows.UTF16PtrToString(e.DisplayName),
			state: st.CurrentState, exit: st.Win32ExitCode, specific: st.ServiceSpecificExitCode, pid: st.ProcessId,
		})
	}
	return out, nil
}

func (s liveSCM) config(name string) (winServiceConfig, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return winServiceConfig{}, err
	}
	h, err := windows.OpenService(s.h, p, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return winServiceConfig{}, err
	}
	svc := &mgr.Service{Name: name, Handle: h}
	defer svc.Close() //nolint:errcheck // a query handle
	c, err := svc.Config()
	if err != nil {
		return winServiceConfig{}, err
	}
	return winServiceConfig{
		start: c.StartType, delayed: c.DelayedAutoStart, triggered: triggered(h),
		deps: c.Dependencies, group: c.LoadOrderGroup,
	}, nil
}

// triggered is a service that a trigger starts; SERVICE_TRIGGER_INFO starts with the count.
func triggered(h windows.Handle) bool {
	var needed uint32
	b := make([]uint64, 512)
	err := windows.QueryServiceConfig2(h, windows.SERVICE_CONFIG_TRIGGER_INFO, (*byte)(unsafe.Pointer(&b[0])), uint32(len(b)*8), &needed)
	if errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return true // too many triggers to fit is still triggers
	}
	return err == nil && uint32(b[0]) > 0
}

func (s liveSCM) close() { _ = windows.CloseServiceHandle(s.h) }
