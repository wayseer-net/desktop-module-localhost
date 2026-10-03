package localhost

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"os/user"
	"slices"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"wayseer.dev/sdk"

	"github.com/ebitengine/purego"
	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

const supported = true

var errUnsupported error

// The parsers' offsets, checked against x/sys's structures: a mismatch does not compile.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(unix.KinfoProc{})-kinfoSize]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.ExternProc{}.P_pid)-kpPid]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.ExternProc{}.P_comm)-kpComm]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.KinfoProc{}.Eproc)+unsafe.Offsetof(unix.Eproc{}.Ppid)-kpPpid]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.KinfoProc{}.Eproc)+unsafe.Offsetof(unix.Eproc{}.Pcred)+
		unsafe.Offsetof(unix.Pcred{}.P_ruid)-kpRuid]
	_ = [1]struct{}{}[unsafe.Sizeof(unix.Statfs_t{})-statfsSize]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.Statfs_t{}.Blocks)-fsBlocks]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.Statfs_t{}.Bavail)-fsBavail]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.Statfs_t{}.Flags)-fsFlags]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.Statfs_t{}.Fstypename)-fsTypename]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.Statfs_t{}.Mntfromname)-fsFromName]
	_ = [1]struct{}{}[unsafe.Sizeof(unix.IfMsghdr2{})-ifMsgSize]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.IfMsghdr2{}.Data)+unsafe.Offsetof(unix.IfData64{}.Ibytes)-ifIbytes]
	_ = [1]struct{}{}[unsafe.Offsetof(unix.IfMsghdr2{}.Data)+unsafe.Offsetof(unix.IfData64{}.Obytes)-ifObytes]
)

// statfs reports a filesystem's size; used excludes the blocks reserved for root.
func statfs(path string) (fsUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fsUsage{}, err
	}
	bs := uint64(st.Bsize)
	return fsUsage{total: st.Blocks * bs, used: (st.Blocks - st.Bfree) * bs, avail: st.Bavail * bs}, nil
}

// nativeSource reads macOS through sysctls and Mach calls.
func nativeSource(r *reader) (source, []sdk.Metric) {
	return &macReader{
		procs: r.procs, cmds: r.cmds, addrs: r.addrs, pageSize: uint64(os.Getpagesize()),
		users: map[uint32]string{}, commands: map[procKey]string{},
	}, macCatalogue()
}

// Mach and libproc calls, from libSystem.
const (
	processorCPULoadInfo = 2 // PROCESSOR_CPU_LOAD_INFO
	hostVMInfo64         = 4 // HOST_VM_INFO64
	procPIDTaskInfo      = 4 // PROC_PIDTASKINFO
	kernSuccess          = 0 // KERN_SUCCESS
	netRTIfList2         = 6 // NET_RT_IFLIST2
	libSystem            = "/usr/lib/libSystem.B.dylib"
)

var mach struct {
	host, task    uint32
	numer, denom  uint32
	processorInfo func(host uint32, flavor int32, count *uint32, info *uintptr, infoCount *uint32) int32
	statistics64  func(host uint32, flavor int32, info *byte, count *uint32) int32
	deallocate    func(task uint32, addr, size uintptr) int32
	pidInfo       func(pid, flavor int32, arg uint64, buf *byte, size int32) int32
	memcpy        func(dst *byte, src, n uintptr) uintptr
}

var loadMach = sync.OnceValue(func() error {
	lib, err := purego.Dlopen(libSystem, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	var hostSelf func() uint32
	var timebase func(info *[2]uint32) int32
	purego.RegisterLibFunc(&hostSelf, lib, "mach_host_self")
	purego.RegisterLibFunc(&timebase, lib, "mach_timebase_info")
	purego.RegisterLibFunc(&mach.processorInfo, lib, "host_processor_info")
	purego.RegisterLibFunc(&mach.statistics64, lib, "host_statistics64")
	purego.RegisterLibFunc(&mach.deallocate, lib, "vm_deallocate")
	purego.RegisterLibFunc(&mach.pidInfo, lib, "proc_pidinfo")
	purego.RegisterLibFunc(&mach.memcpy, lib, "memcpy")
	task, err := purego.Dlsym(lib, "mach_task_self_")
	if err != nil {
		return err
	}
	var port [4]byte
	mach.memcpy(&port[0], task, 4)
	mach.task, mach.host = le.Uint32(port[:]), hostSelf()
	var tb [2]uint32
	if timebase(&tb) != kernSuccess {
		return errors.New("mach_timebase_info failed")
	}
	mach.numer, mach.denom = tb[0], tb[1]
	return nil
})

// macReader reads this Mac; it has no disks, as those need IOKit.
type macReader struct {
	procs, cmds bool
	addrs       func() map[string][]string
	pageSize    uint64
	users       map[uint32]string  // uid to name, cached
	commands    map[procKey]string // cached per process, since they rarely change
}

// read takes a sample; only the CPUs and memory are required.
func (r *macReader) read(now time.Time) (*sample, error) {
	if err := loadMach(); err != nil {
		return nil, fmt.Errorf("libSystem: %w", err)
	}
	s := &sample{at: now, host: r.hostInfo()}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return nil, fmt.Errorf("kern.boottime: %w", err)
	}
	s.stat.boot = time.Unix(boot.Unix())
	var err1, err2 error
	s.stat.total, s.stat.cpus, err1 = cpuLoad()
	s.mem, err2 = r.memory()
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	s.fss = filesystems()
	s.nics = r.nics()
	if r.procs {
		s.procs = r.processes(s.stat.boot)
	}
	return s, nil
}

func (r *macReader) hostInfo() hostInfo {
	h := hostInfo{name: "localhost"}
	if name, err := os.Hostname(); err == nil && name != "" {
		h.name = name
	}
	h.kernel, _ = unix.Sysctl("kern.osrelease")
	if v, err := unix.Sysctl("kern.osproductversion"); err == nil {
		h.os = "macOS " + v
	}
	return h
}

func cpuLoad() (cpuTimes, []namedCPU, error) {
	var count, n uint32
	var info uintptr
	if kr := mach.processorInfo(mach.host, processorCPULoadInfo, &count, &info, &n); kr != kernSuccess {
		return cpuTimes{}, nil, fmt.Errorf("host_processor_info: kern_return %d", kr)
	}
	b := make([]byte, uintptr(n)*4)
	mach.memcpy(&b[0], info, uintptr(len(b)))
	mach.deallocate(mach.task, info, uintptr(len(b)))
	return parseCPULoad(b)
}

func (r *macReader) memory() (memInfo, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return memInfo{}, fmt.Errorf("hw.memsize: %w", err)
	}
	b := make([]byte, vmStatsSize)
	n := uint32(vmStatsSize / 4)
	if kr := mach.statistics64(mach.host, hostVMInfo64, &b[0], &n); kr != kernSuccess {
		return memInfo{}, fmt.Errorf("host_statistics64: kern_return %d", kr)
	}
	m := memInfo{total: total}
	if m.available, err = parseVMStats(b, r.pageSize); err != nil {
		return memInfo{}, err
	}
	if sw, err := unix.SysctlRaw("vm.swapusage"); err == nil {
		m.swapTotal, m.swapFree, _ = parseSwapUsage(sw)
	}
	return m, nil
}

// filesystems lists mounted filesystems; usage comes with the listing.
func filesystems() []filesystem {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return nil
	}
	buf := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), n*statfsSize)
	fss, _ := parseFsstat(b)
	return fss
}

// nics lists network interfaces other than loopback, with their counters.
func (r *macReader) nics() []nic {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var counters map[int]netCounters
	if rib, err := route.FetchRIB(unix.AF_UNSPEC, netRTIfList2, 0); err == nil {
		counters, _ = parseIfList2(rib)
	}
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
		if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagRunning != 0 {
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

// processes lists processes in pid order; another user's has no CPU or memory, which macOS
// does not tell an unprivileged reader.
func (r *macReader) processes(boot time.Time) []process {
	raw, err := unix.SysctlRaw("kern.proc.all")
	if err != nil {
		return nil
	}
	procs, err := parseKinfoProcs(raw, boot)
	if err != nil {
		return nil
	}
	seen := map[procKey]bool{}
	task := make([]byte, taskInfoSize)
	out := make([]process, 0, len(procs))
	for _, mp := range procs {
		p := process{pidStat: pidStat{pid: mp.pid, ppid: mp.ppid, comm: mp.comm, start: mp.start}, user: r.user(mp.uid)}
		got := mach.pidInfo(int32(mp.pid), procPIDTaskInfo, 0, &task[0], taskInfoSize) //nolint:gosec // a pid_t
		if rss, cpu, err := parseTaskInfo(task[:max(got, 0)], mach.numer, mach.denom); err == nil {
			p.rss, p.cpu = rss/r.pageSize, cpu
		} else {
			p.unmeasured = true
		}
		key := procKey{mp.pid, mp.start}
		if r.cmds {
			if _, ok := r.commands[key]; !ok {
				args, _ := unix.SysctlRaw("kern.procargs2", mp.pid)
				r.commands[key] = cut(parseProcArgs(args), commandCap)
			}
			p.command = r.commands[key]
		}
		seen[key] = true
		out = append(out, p)
	}
	maps.DeleteFunc(r.commands, func(k procKey, _ string) bool { return !seen[k] })
	slices.SortFunc(out, func(a, b process) int { return cmp.Compare(a.pid, b.pid) })
	return out
}

func (r *macReader) user(uid uint32) string {
	if name, ok := r.users[uid]; ok {
		return name
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	r.users[uid] = name
	return name
}
