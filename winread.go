package localhost

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"wayseer.dev/sdk"
)

// winAPI is the Windows calls the reader makes; sys_windows.go makes them, and tests answer
// from recordings on any OS.
type winAPI interface {
	hostname() (string, error)
	version() winVersion
	sinceBoot() time.Duration                            // GetTickCount64
	systemTimes() (idle, kernel, user uint64, err error) // GetSystemTimes, all CPUs
	processorTimes() ([]byte, error)                     // NtQuerySystemInformation, per CPU
	memoryStatus() ([]byte, error)                       // GlobalMemoryStatusEx
	driveStrings() ([]uint16, error)                     // GetLogicalDriveStringsW
	driveType(root string) uint32                        // GetDriveTypeW
	fileSystem(root string) (string, error)              // GetVolumeInformationW
	diskFree(root string) (fsUsage, error)               // GetDiskFreeSpaceExW
	interfaces() ([]net.Interface, error)                // GetAdaptersAddresses, through net
	ifTable() ([]byte, error)                            // GetIfTable2Ex
	processes() ([]winProcess, error)                    // CreateToolhelp32Snapshot
	processTimes(pid uint32) (winProcTimes, error)       // GetProcessTimes, GetProcessMemoryInfo
	processUser(pid uint32) (string, error)              // the process token's user
	processCommand(pid uint32) (string, error)           // NtQueryInformationProcess
}

// winProcess is a process as the snapshot lists it.
type winProcess struct {
	pid, ppid uint32
	exe       string
}

// winProcTimes is when a process started, its CPU time in 100 ns units, and its working set.
type winProcTimes struct {
	created         time.Time
	cpu, workingSet uint64
}

// winVersion is RtlGetVersion's numbers and the registry's names for the release.
type winVersion struct {
	major, minor, build uint32
	product, display    string
}

const driveFixed = 3 // DRIVE_FIXED

const idlePID = 0 // the System Idle Process, which is idle time rather than a process

// ticksPer100ns turns Windows' 100 ns units into clock ticks.
const ticksPer100ns = 1e7 / clockTick

// winReader reads Windows: the host, CPUs, memory, fixed volumes, network interfaces and
// processes.
type winReader struct {
	api         winAPI
	addrs       func() map[string][]string
	procs, cmds bool
	pageSize    uint64
	details     map[procKey]procDetail // cached per process, since they rarely change
	boot        time.Time              // from the first read, so it doesn't jitter by the tick count's rounding
}

func newWinReader(api winAPI, r *reader) *winReader {
	return &winReader{
		api: api, addrs: r.addrs, procs: r.procs, cmds: r.cmds,
		pageSize: uint64(os.Getpagesize()), details: map[procKey]procDetail{},
	}
}

// read takes a sample; only the CPUs and memory are required.
func (r *winReader) read(now time.Time) (*sample, error) {
	if r.boot.IsZero() {
		r.boot = now.Add(-r.api.sinceBoot()).Truncate(time.Second)
	}
	s := &sample{at: now, host: r.hostInfo()}
	s.stat.boot = r.boot
	var err1, err2 error
	s.stat.total, s.stat.cpus, err1 = r.cpus()
	s.mem, err2 = called(r.api.memoryStatus, parseMemoryStatus, "GlobalMemoryStatusEx")
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	s.fss = r.volumes()
	s.nics = r.nics()
	if r.procs {
		s.procs = r.processes()
	}
	return s, nil
}

// called makes a call and parses its answer, naming the call in an error.
func called[T any](call func() ([]byte, error), parse func([]byte) (T, error), name string) (T, error) {
	b, err := call()
	if err == nil {
		var v T
		if v, err = parse(b); err == nil {
			return v, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("%s: %w", name, err)
}

func (r *winReader) hostInfo() hostInfo {
	h := hostInfo{name: "localhost"}
	if name, err := r.api.hostname(); err == nil && name != "" {
		h.name = name
	}
	v := r.api.version()
	h.kernel = fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.build)
	h.os = windowsName(v.product, v.display, v.build)
	return h
}

func (r *winReader) cpus() (cpuTimes, []namedCPU, error) {
	idle, kernel, user, err := r.api.systemTimes()
	if err != nil {
		return cpuTimes{}, nil, fmt.Errorf("GetSystemTimes: %w", err)
	}
	cpus, err := called(r.api.processorTimes, parseProcessorTimes, "NtQuerySystemInformation")
	return systemTimes(idle, kernel, user), cpus, err
}

// volumes lists fixed drives by letter; one that can't be read, such as a locked one, is
// listed without its usage.
func (r *winReader) volumes() []filesystem {
	roots, err := r.api.driveStrings()
	if err != nil {
		return nil
	}
	var out []filesystem
	for _, root := range parseDriveStrings(roots) {
		if r.api.driveType(root) != driveFixed {
			continue
		}
		f := filesystem{id: strings.TrimSuffix(root, `\`), name: root, mounts: []string{root}}
		f.fstype, _ = r.api.fileSystem(root)
		f.usage, f.usageErr = r.api.diskFree(root)
		out = append(out, f)
	}
	return out
}

// nics lists network interfaces other than loopback, with their counters.
func (r *winReader) nics() []nic {
	ifs, err := r.api.interfaces()
	if err != nil {
		return nil
	}
	counters, _ := called(r.api.ifTable, parseIfTable, "GetIfTable2Ex")
	var addrs map[string][]string
	if r.addrs != nil {
		addrs = r.addrs()
	}
	var out []nic
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		state := "down"
		if i.Flags&net.FlagUp != 0 {
			state = "up"
		}
		out = append(out, nic{
			name: i.Name, mac: i.HardwareAddr.String(), state: state, mtu: i.MTU,
			io: counters[i.Index], addrs: addrs[i.Name],
		})
	}
	slices.SortFunc(out, func(a, b nic) int { return cmp.Compare(a.name, b.name) })
	return out
}

// processes lists processes in pid order; one that can't be opened, such as another user's or
// a protected one, is listed by name and pid only.
func (r *winReader) processes() []process {
	list, err := r.api.processes()
	if err != nil {
		return nil
	}
	seen := map[procKey]bool{}
	out := make([]process, 0, len(list))
	for _, wp := range list {
		if wp.pid == idlePID {
			continue
		}
		p := r.process(wp)
		key := procKey{p.pid, p.start}
		d, ok := r.details[key]
		if !ok {
			d = r.detail(wp.pid)
			r.details[key] = d
		}
		p.user, p.command = d.user, d.command
		seen[key] = true
		out = append(out, p)
	}
	maps.DeleteFunc(r.details, func(k procKey, _ procDetail) bool { return !seen[k] })
	slices.SortFunc(out, func(a, b process) int { return cmp.Compare(a.pid, b.pid) })
	return out
}

// process is a listed process with its times and memory, in clock ticks since boot and pages.
func (r *winReader) process(wp winProcess) process {
	p := process{pidStat: pidStat{pid: int(wp.pid), ppid: int(wp.ppid), comm: wp.exe}}
	t, err := r.api.processTimes(wp.pid)
	if err != nil {
		p.unmeasured, p.startUnknown = true, true
		return p
	}
	p.start = uint64(max(t.created.Sub(r.boot), 0) * clockTick / time.Second)
	p.cpu, p.rss = t.cpu/ticksPer100ns, t.workingSet/r.pageSize
	return p
}

func (r *winReader) detail(pid uint32) procDetail {
	var d procDetail
	d.user, _ = r.api.processUser(pid)
	if r.cmds {
		if c, err := r.api.processCommand(pid); err == nil {
			d.command = cut(c, commandCap)
		}
	}
	return d
}

// winNatives is where Windows' metrics come from; it has no disk or swap metrics yet.
var winNatives = map[string]string{
	MetricCPU:        "GetSystemTimes, NtQuerySystemInformation processor times, GetProcessTimes",
	MetricMemUtil:    "GlobalMemoryStatusEx available physical memory",
	MetricMemUsed:    "GlobalMemoryStatusEx total less available physical memory",
	MetricMemAvail:   "GlobalMemoryStatusEx available physical memory",
	MetricFSUsed:     "GetDiskFreeSpaceExW",
	MetricFSUtil:     "GetDiskFreeSpaceExW",
	MetricNetReceive: "GetIfTable2Ex InOctets",
	MetricNetSend:    "GetIfTable2Ex OutOctets",
	MetricRSS:        "GetProcessMemoryInfo WorkingSetSize",
}

// winCatalogue is the catalogue as Windows has it; a metric it lacks is left out, not zero.
func winCatalogue() []sdk.Metric { return catalogueFor(winNatives) }
