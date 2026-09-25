package localhost

import (
	"mindseye/internal/data"
	"mindseye/internal/model"
	"mindseye/internal/module"
	"strconv"
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

func kinds(ks ...model.Kind) []model.Kind { return ks }

var catalogue = []module.Metric{
	{Name: MetricCPU, Unit: model.UnitPercent, Kinds: kinds(model.KindHost, KindCPU, model.KindProcess), Description: "share of CPU time spent busy; for a process, its share of the whole host", Native: "/proc/stat, /proc/<pid>/stat"},
	{Name: MetricMemUtil, Unit: model.UnitPercent, Kinds: kinds(model.KindHost, KindMemory), Description: "share of memory not available to new work", Native: "/proc/meminfo MemAvailable"},
	{Name: MetricMemUsed, Unit: model.UnitBytes, Kinds: kinds(KindMemory), Description: "memory not available to new work", Native: "MemTotal - MemAvailable"},
	{Name: MetricMemAvail, Unit: model.UnitBytes, Kinds: kinds(KindMemory), Description: "memory available to new work without swapping", Native: "MemAvailable"},
	{Name: MetricSwapUsed, Unit: model.UnitBytes, Kinds: kinds(KindMemory), Description: "swap in use", Native: "SwapTotal - SwapFree"},
	{Name: MetricDiskRead, Unit: model.UnitBytesPS, Kinds: kinds(model.KindDisk), Description: "bytes read", Native: "/proc/diskstats sectors read"},
	{Name: MetricDiskWrite, Unit: model.UnitBytesPS, Kinds: kinds(model.KindDisk), Description: "bytes written", Native: "/proc/diskstats sectors written"},
	{Name: MetricDiskUtil, Unit: model.UnitPercent, Kinds: kinds(model.KindDisk), Description: "share of time with I/O in flight", Native: "/proc/diskstats io ticks"},
	{Name: MetricFSUsed, Unit: model.UnitBytes, Kinds: kinds(KindFilesystem), Description: "space used", Native: "statfs"},
	{Name: MetricFSUtil, Unit: model.UnitPercent, Kinds: kinds(KindFilesystem), Description: "share of space used, as df reports it", Native: "statfs"},
	{Name: MetricNetReceive, Unit: model.UnitBytesPS, Kinds: kinds(model.KindInterface), Description: "bytes received", Native: "/proc/net/dev"},
	{Name: MetricNetSend, Unit: model.UnitBytesPS, Kinds: kinds(model.KindInterface), Description: "bytes sent", Native: "/proc/net/dev"},
	{Name: MetricRSS, Unit: model.UnitBytes, Kinds: kinds(model.KindProcess), Description: "resident memory", Native: "/proc/<pid>/stat rss"},
}

func unitOf(metric string) model.Unit {
	for _, m := range catalogue {
		if m.Name == metric {
			return m.Unit
		}
	}
	return model.UnitNone
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
			ref := r.ref(model.KindDisk, d.name)
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
			ref := r.ref(model.KindInterface, n.name)
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
		ref := r.ref(model.KindProcess, strconv.Itoa(p.pid))
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
func (r *recorder) rate(ref model.EntityRef, metric string, before, after uint64) {
	if after >= before {
		r.put(ref, metric, float64(after-before)/r.elapsed)
	}
}

func (r *recorder) ref(kind model.Kind, native string) model.EntityRef {
	b := builder{src: r.m.name}
	return b.ref(kind, native)
}

func (r *recorder) put(ref model.EntityRef, metric string, v float64) {
	key := data.SeriesRef{Entity: ref, Metric: metric}
	h := r.m.series[key]
	if h == nil {
		keep := r.m.opts.History
		if ref.Kind() == model.KindProcess {
			keep = r.m.opts.ProcessHistory
		}
		h = data.NewRing(r.m.opts.points(keep))
		r.m.series[key] = h
	}
	h.Add(data.Point{T: r.cur.at.UnixNano(), V: v})
}
