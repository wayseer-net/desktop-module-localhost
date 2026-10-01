package localhost

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pure parsers for what macOS's sysctls and Mach calls return, kept apart from reading so they
// are tested on every OS. Offsets are for 64-bit darwin; sys_darwin.go checks them against x/sys.

var le = binary.LittleEndian

// processor_cpu_load_info: user, system, idle and nice ticks per CPU.
const cpuLoadSize = 16

// parseCPULoad reads host_processor_info's PROCESSOR_CPU_LOAD_INFO array.
func parseCPULoad(b []byte) (total cpuTimes, cpus []namedCPU, err error) {
	if len(b) == 0 || len(b)%cpuLoadSize != 0 {
		return total, nil, fmt.Errorf("cpu load info is %d bytes, not a multiple of %d", len(b), cpuLoadSize)
	}
	for i := 0; i < len(b); i += cpuLoadSize {
		user, sys, idle, nice := le.Uint32(b[i:]), le.Uint32(b[i+4:]), le.Uint32(b[i+8:]), le.Uint32(b[i+12:])
		t := cpuTimes{busy: uint64(user) + uint64(sys) + uint64(nice)}
		t.total = t.busy + uint64(idle)
		cpus = append(cpus, namedCPU{"cpu" + strconv.Itoa(i/cpuLoadSize), t})
		total.busy += t.busy
		total.total += t.total
	}
	return total, cpus, nil
}

// vm_statistics64 page counts.
const (
	vmFree, vmActive, vmInactive = 0, 4, 8
	vmStatsSize                  = 152
)

// parseVMStats reads host_statistics64's HOST_VM_INFO64: memory available to new work is the
// free and inactive pages, as Activity Monitor counts it.
func parseVMStats(b []byte, pageSize uint64) (uint64, error) {
	if len(b) < vmStatsSize {
		return 0, fmt.Errorf("vm statistics are %d bytes, want %d", len(b), vmStatsSize)
	}
	return (uint64(le.Uint32(b[vmFree:])) + uint64(le.Uint32(b[vmInactive:]))) * pageSize, nil
}

// parseSwapUsage reads vm.swapusage, an xsw_usage: total, available and used bytes.
func parseSwapUsage(b []byte) (total, free uint64, err error) {
	if len(b) < 24 {
		return 0, 0, fmt.Errorf("swap usage is %d bytes, want at least 24", len(b))
	}
	return le.Uint64(b), le.Uint64(b[8:]), nil
}

// struct statfs as getfsstat fills it.
const (
	fsBsize, fsBlocks, fsBfree, fsBavail = 0, 8, 16, 24
	fsFlags                              = 64
	fsTypename, fsOnName, fsFromName     = 72, 88, 1112
	statfsSize                           = 2168
	mntDontBrowse                        = 0x00100000 // hidden from Finder: the system's own volumes
	dataVolume                           = "/System/Volumes/Data"
)

// parseFsstat reads getfsstat's records: filesystems on devices, less the system's hidden
// volumes, though the Data volume that holds the owner's files is kept.
func parseFsstat(b []byte) ([]filesystem, error) {
	if len(b)%statfsSize != 0 {
		return nil, fmt.Errorf("fsstat is %d bytes, not a multiple of %d", len(b), statfsSize)
	}
	var out []filesystem
	for i := 0; i < len(b); i += statfsSize {
		r := b[i : i+statfsSize]
		dev, point := cstring(r[fsFromName:fsFromName+1024]), cstring(r[fsOnName:fsOnName+1024])
		hidden := le.Uint32(r[fsFlags:])&mntDontBrowse != 0 && point != dataVolume
		if !strings.HasPrefix(dev, "/dev/") || hidden {
			continue
		}
		bs := uint64(le.Uint32(r[fsBsize:]))
		blocks, bfree := le.Uint64(r[fsBlocks:]), le.Uint64(r[fsBfree:])
		out = append(out, filesystem{
			id: strings.TrimPrefix(dev, "/dev/"), name: point, fstype: cstring(r[fsTypename : fsTypename+16]),
			mounts: []string{point},
			usage:  fsUsage{total: blocks * bs, used: (blocks - min(bfree, blocks)) * bs, avail: le.Uint64(r[fsBavail:]) * bs},
		})
	}
	return out, nil
}

// struct kinfo_proc as kern.proc.all returns it.
const (
	kpStartSec = 0   // p_starttime.tv_sec
	kpPid      = 40  // p_pid
	kpComm     = 243 // p_comm, MAXCOMLEN+1
	kpRuid     = 392 // e_pcred.p_ruid
	kpPpid     = 560 // e_ppid
	kinfoSize  = 648
	maxComLen  = 16
)

// macProc is what kern.proc.all says about a process; start is clock ticks since boot.
type macProc struct {
	pid, ppid int
	uid       uint32
	comm      string
	start     uint64
}

// parseKinfoProcs reads kern.proc.all, leaving out the kernel (pid 0).
func parseKinfoProcs(b []byte, boot time.Time) ([]macProc, error) {
	if len(b)%kinfoSize != 0 {
		return nil, fmt.Errorf("kinfo_proc list is %d bytes, not a multiple of %d", len(b), kinfoSize)
	}
	var out []macProc
	for i := 0; i < len(b); i += kinfoSize {
		r := b[i : i+kinfoSize]
		pid := int(int32(le.Uint32(r[kpPid:]))) //nolint:gosec // a pid_t
		if pid == 0 {
			continue
		}
		since := time.Unix(int64(le.Uint64(r[kpStartSec:])), 0).Sub(boot) //nolint:gosec // a time_t
		out = append(out, macProc{
			pid: pid, ppid: int(int32(le.Uint32(r[kpPpid:]))), uid: le.Uint32(r[kpRuid:]), //nolint:gosec // a pid_t
			comm: cstring(r[kpComm : kpComm+maxComLen]), start: uint64(max(since, 0) * clockTick / time.Second),
		})
	}
	return out, nil
}

// struct proc_taskinfo, from proc_pidinfo's PROC_PIDTASKINFO.
const taskInfoSize = 96

// parseTaskInfo returns resident bytes and user plus system time in clock ticks; times count
// Mach units, which the timebase numer/denom turns into nanoseconds.
func parseTaskInfo(b []byte, numer, denom uint32) (rss, cpu uint64, err error) {
	if len(b) < taskInfoSize {
		return 0, 0, fmt.Errorf("task info is %d bytes, want %d", len(b), taskInfoSize)
	}
	ns := (le.Uint64(b[16:]) + le.Uint64(b[24:])) * uint64(numer) / uint64(max(denom, 1))
	return le.Uint64(b[8:]), ns * clockTick / uint64(time.Second), nil
}

// parseProcArgs reads kern.procargs2: argc, the executable path, NUL padding, then the
// arguments, then the environment, which is left out.
func parseProcArgs(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	argc := int(le.Uint32(b))
	_, rest, _ := bytes.Cut(b[4:], []byte{0})
	args := strings.Split(string(bytes.TrimLeft(rest, "\x00")), "\x00")
	return strings.Join(args[:min(argc, len(args))], " ")
}

// struct if_msghdr2 with its if_data64, from the NET_RT_IFLIST2 route sysctl.
const (
	rtmIfinfo2         = 0x12
	ifIndex            = 12
	ifIbytes, ifObytes = 96, 104
	ifMsgSize          = 160
)

// parseIfList2 reads the route sysctl's interface messages into counters by interface index.
func parseIfList2(b []byte) (map[int]netCounters, error) {
	out := map[int]netCounters{}
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("%d bytes left, too few for a message header", len(b))
		}
		n := int(le.Uint16(b))
		if n < 4 || n > len(b) {
			return nil, fmt.Errorf("message of %d bytes with %d left", n, len(b))
		}
		if b[3] == rtmIfinfo2 {
			if n < ifMsgSize {
				return nil, fmt.Errorf("interface message of %d bytes, want %d", n, ifMsgSize)
			}
			out[int(le.Uint16(b[ifIndex:]))] = netCounters{rx: le.Uint64(b[ifIbytes:]), tx: le.Uint64(b[ifObytes:])}
		}
		b = b[n:]
	}
	return out, nil
}

// cstring is b up to its first NUL.
func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
