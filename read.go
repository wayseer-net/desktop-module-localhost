package localhost

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sample is one reading of the machine.
type sample struct {
	at    time.Time
	host  hostInfo
	stat  procStat
	mem   memInfo
	disks []disk
	fss   []filesystem
	nics  []nic
	procs []process
	units []unit // from systemd, not the file tree
}

type hostInfo struct{ name, kernel, os string }

type disk struct {
	name, model           string
	size                  uint64
	rotational, removable bool
	io                    ioCounters
}

type filesystem struct {
	id, name, fstype, disk string // disk is "" when the device is not a known disk
	mounts                 []string
	usage                  fsUsage
	usageErr               error
}

type fsUsage struct{ total, used, avail uint64 }

type nic struct {
	name, mac, state string
	mtu              int
	io               netCounters
	addrs            []string
}

type process struct {
	pidStat
	command, user, cgroup string
}

// procKey identifies a process across pid reuse.
type procKey struct {
	pid   int
	start uint64
}

type procDetail struct{ command, user, cgroup string }

// reader reads a machine through a file tree rooted like /.
type reader struct {
	fsys    fs.FS
	root    string
	statfs  func(path string) (fsUsage, error)
	addrs   func() map[string][]string // interface addresses; nil when unknown
	procs   bool
	cmds    bool                   // read command lines
	details map[procKey]procDetail // cached per process, since they rarely change
}

// read takes a sample; only /proc/stat and /proc/meminfo are required.
func (r *reader) read(now time.Time) (*sample, error) {
	s := &sample{at: now}
	var err1, err2 error
	s.stat, err1 = readParsed(r.fsys, "proc/stat", parseStat)
	s.mem, err2 = readParsed(r.fsys, "proc/meminfo", parseMeminfo)
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	s.host = r.hostInfo()
	s.disks, s.fss = r.storage()
	s.nics = r.nics()
	if r.procs {
		s.procs = r.processes()
	}
	return s, nil
}

func readParsed[T any](fsys fs.FS, name string, parse func([]byte) (T, error)) (T, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		var zero T
		return zero, unwrapPath(err)
	}
	v, err := parse(b)
	if err != nil {
		return v, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

// unwrapPath keeps an fs error's operation and path while dropping the fs.FS-relative form.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s /%s: %w", pe.Op, pe.Path, pe.Err)
	}
	return err
}

// text reads a small file and trims it, or returns "".
func (r *reader) text(name string) string {
	b, err := fs.ReadFile(r.fsys, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (r *reader) number(name string) uint64 {
	v, _ := strconv.ParseUint(r.text(name), 10, 64)
	return v
}

func (r *reader) hostInfo() hostInfo {
	h := hostInfo{name: r.text("proc/sys/kernel/hostname"), kernel: r.text("proc/sys/kernel/osrelease")}
	if b, err := fs.ReadFile(r.fsys, "etc/os-release"); err == nil {
		h.os = parseOSRelease(b)
	}
	if h.name == "" {
		h.name = "localhost"
	}
	return h
}

// skippedDisk names block devices that are not physical disks; device-mapper ones are resolved
// to the disks under them.
func skippedDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "dm-", "sr", "fd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// storage lists disks, and the filesystems on block devices with the disk each lives on.
func (r *reader) storage() ([]disk, []filesystem) {
	blocks, _ := fs.ReadDir(r.fsys, "sys/block")
	stats, _ := readParsed(r.fsys, "proc/diskstats", parseDiskstats)
	owner := map[string]string{}  // kernel device name -> disk
	mapper := map[string]string{} // device-mapper name -> dm-N
	slave := map[string]string{}  // dm-N -> a device under it
	var disks []disk
	for _, b := range blocks {
		name := b.Name()
		dir := path.Join("sys/block", name)
		if strings.HasPrefix(name, "dm-") {
			mapper[r.text(dir+"/dm/name")] = name
			if under, _ := fs.ReadDir(r.fsys, dir+"/slaves"); len(under) > 0 {
				slave[name] = under[0].Name()
			}
			continue
		}
		size := r.number(dir+"/size") * sectorSize
		if skippedDisk(name) || size == 0 {
			continue
		}
		owner[name] = name
		for _, p := range r.partitions(dir) {
			owner[p] = name
		}
		disks = append(disks, disk{
			name: name, model: r.text(dir + "/device/model"), size: size,
			rotational: r.text(dir+"/queue/rotational") == "1", removable: r.text(dir+"/removable") == "1",
			io: stats[name],
		})
	}
	resolve := func(dev string) (kernel, disk string) {
		kernel = strings.TrimPrefix(dev, "/dev/")
		if name, ok := strings.CutPrefix(kernel, "mapper/"); ok && mapper[name] != "" {
			kernel = mapper[name]
		}
		under := kernel
		for range 8 { // stacked device-mapper targets, bounded against loops
			if slave[under] == "" {
				break
			}
			under = slave[under]
		}
		return kernel, owner[under]
	}
	return disks, r.filesystems(resolve)
}

func (r *reader) partitions(dir string) []string {
	var out []string
	entries, _ := fs.ReadDir(r.fsys, dir)
	for _, e := range entries {
		if _, err := fs.Stat(r.fsys, path.Join(dir, e.Name(), "partition")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// filesystems groups mounts of block devices by device, named by the shortest mount point.
func (r *reader) filesystems(resolve func(string) (string, string)) []filesystem {
	mounts, _ := readParsed(r.fsys, "proc/self/mountinfo", parseMountinfo)
	byDevice := map[string]*filesystem{}
	var order []string
	for _, m := range mounts {
		if !strings.HasPrefix(m.device, "/dev/") {
			continue
		}
		f := byDevice[m.device]
		if f == nil {
			id, disk := resolve(m.device)
			f = &filesystem{id: path.Base(id), fstype: m.fstype, disk: disk}
			byDevice[m.device] = f
			order = append(order, m.device)
		}
		f.mounts = append(f.mounts, m.point)
	}
	out := make([]filesystem, 0, len(order))
	for _, dev := range order {
		f := byDevice[dev]
		slices.SortFunc(f.mounts, func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b)) })
		f.name = f.mounts[0]
		f.usage, f.usageErr = r.statfs(path.Join(r.root, f.name))
		out = append(out, *f)
	}
	return out
}

// nics lists network interfaces other than loopback.
func (r *reader) nics() []nic {
	stats, _ := readParsed(r.fsys, "proc/net/dev", parseNetDev)
	var addrs map[string][]string
	if r.addrs != nil {
		addrs = r.addrs()
	}
	var out []nic
	for name, io := range stats {
		if name == "lo" {
			continue
		}
		dir := "sys/class/net/" + name
		mtu, _ := strconv.Atoi(r.text(dir + "/mtu"))
		out = append(out, nic{
			name: name, mac: r.text(dir + "/address"), state: r.text(dir + "/operstate"),
			mtu: mtu, io: io, addrs: addrs[name],
		})
	}
	slices.SortFunc(out, func(a, b nic) int { return cmp.Compare(a.name, b.name) })
	return out
}

// processes lists user-space processes in pid order; ones that exit mid-read are left out.
func (r *reader) processes() []process {
	entries, _ := fs.ReadDir(r.fsys, "proc")
	var users map[int]string
	seen := map[procKey]bool{}
	var out []process
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		dir := "proc/" + e.Name()
		st, err := readParsed(r.fsys, dir+"/stat", parsePidStat)
		if err != nil || st.kernel {
			continue
		}
		key := procKey{st.pid, st.start}
		d, ok := r.details[key]
		if !ok {
			if users == nil {
				users = r.users()
			}
			d = r.detail(dir, users)
			r.details[key] = d
		}
		seen[key] = true
		out = append(out, process{pidStat: st, command: d.command, user: d.user, cgroup: d.cgroup})
	}
	maps.DeleteFunc(r.details, func(k procKey, _ procDetail) bool { return !seen[k] })
	slices.SortFunc(out, func(a, b process) int { return cmp.Compare(a.pid, b.pid) })
	return out
}

func (r *reader) users() map[int]string {
	b, _ := fs.ReadFile(r.fsys, "etc/passwd")
	return parsePasswd(b)
}

// commandCap bounds a command line kept as an attribute.
const commandCap = 512

func (r *reader) detail(dir string, users map[int]string) procDetail {
	var d procDetail
	if b, err := fs.ReadFile(r.fsys, dir+"/cmdline"); err == nil && r.cmds {
		d.command = cut(parseCmdline(b), commandCap)
	}
	if b, err := fs.ReadFile(r.fsys, dir+"/cgroup"); err == nil {
		d.cgroup = parseCgroup(b)
	}
	if b, err := fs.ReadFile(r.fsys, dir+"/status"); err == nil {
		if uid, ok := parseUID(b); ok {
			d.user = cmp.Or(users[uid], strconv.Itoa(uid))
		}
	}
	return d
}
