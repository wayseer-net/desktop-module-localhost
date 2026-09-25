package localhost

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pure parsers for procfs files, kept apart from reading so they are tested on captured files.

const sectorSize = 512 // /proc/diskstats counts 512-byte sectors whatever the device

// pfKthread marks a kernel thread in /proc/<pid>/stat flags.
const pfKthread = 0x00200000

// cpuTimes are cumulative clock ticks: busy excludes idle and iowait.
type cpuTimes struct{ busy, total uint64 }

type namedCPU struct {
	name  string // cpu0, cpu1, ...
	times cpuTimes
}

type procStat struct {
	total cpuTimes
	cpus  []namedCPU
	boot  time.Time
}

// parseStat reads /proc/stat.
func parseStat(b []byte) (procStat, error) {
	var s procStat
	haveTotal := false
	for line := range lines(b) {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case f[0] == "cpu":
			t, err := parseCPUTimes(f[1:])
			if err != nil {
				return s, err
			}
			s.total, haveTotal = t, true
		case strings.HasPrefix(f[0], "cpu"):
			t, err := parseCPUTimes(f[1:])
			if err != nil {
				return s, fmt.Errorf("%s: %w", f[0], err)
			}
			s.cpus = append(s.cpus, namedCPU{f[0], t})
		case f[0] == "btime" && len(f) == 2:
			sec, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return s, fmt.Errorf("btime: %w", err)
			}
			s.boot = time.Unix(sec, 0)
		}
	}
	if !haveTotal {
		return s, errors.New("no cpu line")
	}
	return s, nil
}

// parseCPUTimes sums user nice system idle iowait irq softirq steal; guest time is already in user.
func parseCPUTimes(f []string) (cpuTimes, error) {
	if len(f) < 4 {
		return cpuTimes{}, fmt.Errorf("%d cpu fields, want at least 4", len(f))
	}
	var t cpuTimes
	for i, s := range f[:min(len(f), 8)] {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuTimes{}, err
		}
		t.total += v
		if i != 3 && i != 4 { // idle, iowait
			t.busy += v
		}
	}
	return t, nil
}

// memInfo is in bytes.
type memInfo struct{ total, available, swapTotal, swapFree uint64 }

// parseMeminfo reads /proc/meminfo.
func parseMeminfo(b []byte) (memInfo, error) {
	var m memInfo
	fields := map[string]*uint64{"MemTotal": &m.total, "MemAvailable": &m.available, "SwapTotal": &m.swapTotal, "SwapFree": &m.swapFree}
	for line := range lines(b) {
		key, rest, ok := strings.Cut(line, ":")
		dst := fields[key]
		if !ok || dst == nil {
			continue
		}
		num, unit, _ := strings.Cut(strings.TrimSpace(rest), " ")
		v, err := strconv.ParseUint(num, 10, 64)
		if err != nil {
			return m, fmt.Errorf("%s: %w", key, err)
		}
		if unit == "kB" {
			v <<= 10
		}
		*dst = v
	}
	if m.total == 0 {
		return m, errors.New("no MemTotal")
	}
	return m, nil
}

// ioCounters are cumulative: bytes read and written, and time the device was busy.
type ioCounters struct {
	read, written uint64
	busy          time.Duration
}

// parseDiskstats reads /proc/diskstats, keyed by device name.
func parseDiskstats(b []byte) (map[string]ioCounters, error) {
	out := map[string]ioCounters{}
	for line := range lines(b) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 13 {
			return nil, fmt.Errorf("diskstats line has %d fields, want at least 13", len(f))
		}
		n, err := uints(f[5], f[9], f[12])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f[2], err)
		}
		out[f[2]] = ioCounters{read: n[0] * sectorSize, written: n[1] * sectorSize, busy: time.Duration(n[2]) * time.Millisecond}
	}
	return out, nil
}

// netCounters are cumulative bytes received and sent.
type netCounters struct{ rx, tx uint64 }

// parseNetDev reads /proc/net/dev, keyed by interface name.
func parseNetDev(b []byte) (map[string]netCounters, error) {
	out := map[string]netCounters{}
	for line := range lines(b) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") {
			continue // the two header lines
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return nil, fmt.Errorf("net/dev line has %d fields, want at least 9", len(f))
		}
		n, err := uints(f[0], f[8])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(name), err)
		}
		out[strings.TrimSpace(name)] = netCounters{rx: n[0], tx: n[1]}
	}
	return out, nil
}

type mount struct{ point, fstype, device string }

// parseMountinfo reads /proc/<pid>/mountinfo in order.
func parseMountinfo(b []byte) ([]mount, error) {
	var out []mount
	for line := range lines(b) {
		pre, post, ok := strings.Cut(line, " - ")
		f, g := strings.Fields(pre), strings.Fields(post)
		if !ok || len(f) < 5 || len(g) < 2 {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}
		out = append(out, mount{point: unescapeOctal(f[4]), fstype: g[0], device: unescapeOctal(g[1])})
	}
	return out, nil
}

// unescapeOctal undoes the kernel's \ooo escapes of space, tab, newline and backslash.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// pidStat is what /proc/<pid>/stat says about a process; times are clock ticks.
type pidStat struct {
	pid, ppid int
	comm      string
	kernel    bool
	cpu       uint64 // user plus system time
	start     uint64 // since boot
	rss       uint64 // pages
}

// parsePidStat reads /proc/<pid>/stat; comm can hold spaces and parentheses, so it ends at the last ')'.
func parsePidStat(b []byte) (pidStat, error) {
	s := string(bytes.TrimSpace(b))
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return pidStat{}, errors.New("stat has no (comm)")
	}
	f := strings.Fields(s[end+1:])
	if len(f) < 22 {
		return pidStat{}, fmt.Errorf("stat has %d fields after comm, want at least 22", len(f))
	}
	pid, err1 := strconv.Atoi(strings.TrimSpace(s[:open]))
	ppid, err2 := strconv.Atoi(f[1])
	n, err3 := uints(f[6], f[11], f[12], f[19], f[21])
	if err := errors.Join(err1, err2, err3); err != nil {
		return pidStat{}, err
	}
	return pidStat{
		pid: pid, ppid: ppid, comm: s[open+1 : end], kernel: n[0]&pfKthread != 0,
		cpu: n[1] + n[2], start: n[3], rss: n[4],
	}, nil
}

// parseCmdline joins the NUL-separated arguments with spaces.
func parseCmdline(b []byte) string {
	return strings.Join(strings.FieldsFunc(string(b), func(r rune) bool { return r == 0 }), " ")
}

// parseUID finds the real uid in /proc/<pid>/status.
func parseUID(b []byte) (int, bool) {
	for line := range lines(b) {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) == 0 {
				return 0, false
			}
			uid, err := strconv.Atoi(f[0])
			return uid, err == nil
		}
	}
	return 0, false
}

// parsePasswd maps uids to user names.
func parsePasswd(b []byte) map[int]string {
	out := map[int]string{}
	for line := range lines(b) {
		f := strings.Split(line, ":")
		if len(f) < 3 {
			continue
		}
		if uid, err := strconv.Atoi(f[2]); err == nil {
			out[uid] = f[0]
		}
	}
	return out
}

// parseOSRelease returns PRETTY_NAME, else NAME.
func parseOSRelease(b []byte) string {
	vals := map[string]string{}
	for line := range lines(b) {
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	if v := vals["PRETTY_NAME"]; v != "" {
		return v
	}
	return vals["NAME"]
}

func lines(b []byte) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if !yield(sc.Text()) {
				return
			}
		}
	}
}

func uints(ss ...string) ([]uint64, error) {
	out := make([]uint64, len(ss))
	for i, s := range ss {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
