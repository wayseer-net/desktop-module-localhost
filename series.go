package localhost

import (
	"slices"
	"strconv"
	"wayseer/pkg/sdk"
)

// Metric names.
const (
	MetricCPU        = "cpu.utilisation"
	MetricMemUtil    = "memory.utilisation"
	MetricMemUsed    = "memory.used"
	MetricMemAvail   = "memory.available"
	MetricSwapUsed   = "swap.used"
	MetricDiskRead   = "disk.read"
	MetricDiskWrite  = "disk.write"
	MetricDiskUtil   = "disk.utilisation"
	MetricFSUsed     = "fs.used"
	MetricFSUtil     = "fs.utilisation"
	MetricNetReceive = "net.receive"
	MetricNetSend    = "net.transmit"
	MetricRSS        = "memory.rss"
)

func kinds(ks ...sdk.Kind) []sdk.Kind { return ks }

var catalogue = []sdk.Metric{
	{Name: MetricCPU, Unit: sdk.UnitPercent, Kinds: kinds(sdk.KindHost, KindCPU, sdk.KindProcess), Description: "share of CPU time spent busy; for a process, its share of the whole host", Native: "/proc/stat, /proc/<pid>/stat"},
	{Name: MetricMemUtil, Unit: sdk.UnitPercent, Kinds: kinds(sdk.KindHost, KindMemory), Description: "share of memory not available to new work", Native: "/proc/meminfo MemAvailable"},
	{Name: MetricMemUsed, Unit: sdk.UnitBytes, Kinds: kinds(KindMemory), Description: "memory not available to new work", Native: "MemTotal - MemAvailable"},
	{Name: MetricMemAvail, Unit: sdk.UnitBytes, Kinds: kinds(KindMemory), Description: "memory available to new work without swapping", Native: "MemAvailable"},
	{Name: MetricSwapUsed, Unit: sdk.UnitBytes, Kinds: kinds(KindMemory), Description: "swap in use", Native: "SwapTotal - SwapFree"},
	{Name: MetricDiskRead, Unit: sdk.UnitBytesPS, Kinds: kinds(sdk.KindDisk), Description: "bytes read", Native: "/proc/diskstats sectors read"},
	{Name: MetricDiskWrite, Unit: sdk.UnitBytesPS, Kinds: kinds(sdk.KindDisk), Description: "bytes written", Native: "/proc/diskstats sectors written"},
	{Name: MetricDiskUtil, Unit: sdk.UnitPercent, Kinds: kinds(sdk.KindDisk), Description: "share of time with I/O in flight", Native: "/proc/diskstats io ticks"},
	{Name: MetricFSUsed, Unit: sdk.UnitBytes, Kinds: kinds(KindFilesystem), Description: "space used", Native: "statfs"},
	{Name: MetricFSUtil, Unit: sdk.UnitPercent, Kinds: kinds(KindFilesystem), Description: "share of space used, as df reports it", Native: "statfs"},
	{Name: MetricNetReceive, Unit: sdk.UnitBytesPS, Kinds: kinds(sdk.KindInterface), Description: "bytes received", Native: "/proc/net/dev"},
	{Name: MetricNetSend, Unit: sdk.UnitBytesPS, Kinds: kinds(sdk.KindInterface), Description: "bytes sent", Native: "/proc/net/dev"},
	{Name: MetricRSS, Unit: sdk.UnitBytes, Kinds: kinds(sdk.KindProcess), Description: "resident memory", Native: "/proc/<pid>/stat rss"},
}

// macNatives is where macOS's metrics come from; it has no disk metrics, as disks are not read.
var macNatives = map[string]string{
	MetricCPU:        "host_processor_info, proc_pidinfo",
	MetricMemUtil:    "host_statistics64 free and inactive pages",
	MetricMemUsed:    "hw.memsize less free and inactive pages",
	MetricMemAvail:   "free and inactive pages",
	MetricSwapUsed:   "vm.swapusage",
	MetricFSUsed:     "getfsstat",
	MetricFSUtil:     "getfsstat",
	MetricNetReceive: "NET_RT_IFLIST2 ibytes",
	MetricNetSend:    "NET_RT_IFLIST2 obytes",
	MetricRSS:        "proc_pidinfo resident size",
}

// macCatalogue is the catalogue as macOS has it; a metric it lacks is left out, not zero.
func macCatalogue() []sdk.Metric {
	var out []sdk.Metric
	for _, m := range catalogue {
		if native, ok := macNatives[m.Name]; ok {
			m.Native = native
			out = append(out, m)
		}
	}
	return out
}

// applies reports whether metric is one entities of kind have.
func applies(cat []sdk.Metric, metric string, kind sdk.Kind) bool {
	i := slices.IndexFunc(cat, func(m sdk.Metric) bool { return m.Name == metric })
	return i >= 0 && slices.Contains(cat[i].Kinds, kind)
}

func unitOf(cat []sdk.Metric, metric string) sdk.Unit {
	for _, m := range cat {
		if m.Name == metric {
			return m.Unit
		}
	}
	return sdk.UnitNone
}

// recorder writes one sample's points; rates compare it with the previous sample.
type recorder struct {
	m       *Module
	w       *world
	cur     *sample
	prev    *sample // nil on the first sample
	elapsed float64 // seconds since prev
}

// record adds the sample's points to the histories and drops series whose entity is gone.
func (m *Module) record(w *world, cur, prev *sample) {
	r := recorder{m: m, w: w, cur: cur, prev: prev}
	if prev != nil {
		r.elapsed = cur.at.Sub(prev.at).Seconds()
	}
	r.machine()
	r.processes()
	for ref := range m.series {
		if _, ok := w.ents[ref.Entity]; !ok {
			delete(m.series, ref)
		}
	}
}

func (r *recorder) machine() {
	s, p := r.cur, r.prev
	if p != nil {
		r.put(r.w.host, MetricCPU, busyShare(p.stat.total, s.stat.total))
		prevCPUs := map[string]cpuTimes{}
		for _, c := range p.stat.cpus {
			prevCPUs[c.name] = c.times
		}
		for _, c := range s.stat.cpus {
			if before, ok := prevCPUs[c.name]; ok {
				r.put(r.ref(KindCPU, c.name), MetricCPU, busyShare(before, c.times))
			}
		}
	}
	used := s.mem.total - min(s.mem.available, s.mem.total)
	mem := r.ref(KindMemory, "memory")
	r.put(r.w.host, MetricMemUtil, 100*float64(used)/float64(s.mem.total))
	r.put(mem, MetricMemUtil, 100*float64(used)/float64(s.mem.total))
	r.put(mem, MetricMemUsed, float64(used))
	r.put(mem, MetricMemAvail, float64(s.mem.available))
	r.put(mem, MetricSwapUsed, float64(s.mem.swapTotal-min(s.mem.swapFree, s.mem.swapTotal)))
	for _, f := range s.fss {
		if f.usageErr == nil {
			ref := r.ref(KindFilesystem, f.id)
			r.put(ref, MetricFSUsed, float64(f.usage.used))
			r.put(ref, MetricFSUtil, 100*f.usage.share())
		}
	}
	r.devices()
}

// devices records disk and interface throughput.
func (r *recorder) devices() {
	if r.prev == nil || r.elapsed <= 0 {
		return
	}
	before := map[string]ioCounters{}
	for _, d := range r.prev.disks {
		before[d.name] = d.io
	}
	for _, d := range r.cur.disks {
		if b, ok := before[d.name]; ok {
			ref := r.ref(sdk.KindDisk, d.name)
			r.rate(ref, MetricDiskRead, b.read, d.io.read)
			r.rate(ref, MetricDiskWrite, b.written, d.io.written)
			if d.io.busy >= b.busy {
				r.put(ref, MetricDiskUtil, min(100, 100*(d.io.busy-b.busy).Seconds()/r.elapsed))
			}
		}
	}
	nets := map[string]netCounters{}
	for _, n := range r.prev.nics {
		nets[n.name] = n.io
	}
	for _, n := range r.cur.nics {
		if b, ok := nets[n.name]; ok {
			ref := r.ref(sdk.KindInterface, n.name)
			r.rate(ref, MetricNetReceive, b.rx, n.io.rx)
			r.rate(ref, MetricNetSend, b.tx, n.io.tx)
		}
	}
}

// processes records each process's memory, and its CPU share once it has been seen twice.
func (r *recorder) processes() {
	before := map[procKey]uint64{}
	if r.prev != nil {
		for _, p := range r.prev.procs {
			before[procKey{p.pid, p.start}] = p.cpu
		}
	}
	capacity := r.elapsed * clockTick * float64(max(len(r.cur.stat.cpus), 1))
	for _, p := range r.cur.procs {
		if p.unmeasured {
			continue
		}
		ref := r.ref(sdk.KindProcess, strconv.Itoa(p.pid))
		r.put(ref, MetricRSS, float64(p.rss*r.m.pageSize))
		if cpu, ok := before[procKey{p.pid, p.start}]; ok && capacity > 0 && p.cpu >= cpu {
			r.put(ref, MetricCPU, min(100, 100*float64(p.cpu-cpu)/capacity))
		}
	}
}

// busyShare is the busy percentage between two cumulative readings.
func busyShare(a, b cpuTimes) float64 {
	if b.total <= a.total || b.busy < a.busy {
		return 0
	}
	return min(100, 100*float64(b.busy-a.busy)/float64(b.total-a.total))
}

// rate records growth per second; a counter that went backwards (a reset) records nothing.
func (r *recorder) rate(ref sdk.EntityRef, metric string, before, after uint64) {
	if after >= before {
		r.put(ref, metric, float64(after-before)/r.elapsed)
	}
}

func (r *recorder) ref(kind sdk.Kind, native string) sdk.EntityRef {
	b := builder{src: r.m.name}
	return b.ref(kind, native)
}

func (r *recorder) put(ref sdk.EntityRef, metric string, v float64) {
	key := sdk.SeriesRef{Entity: ref, Metric: metric}
	h := r.m.series[key]
	if h == nil {
		keep := r.m.opts.History
		if ref.Kind() == sdk.KindProcess {
			keep = r.m.opts.ProcessHistory
		}
		h = sdk.NewRing(r.m.opts.points(keep))
		r.m.series[key] = h
	}
	h.Add(sdk.Point{T: r.cur.at.UnixNano(), V: v})
}
