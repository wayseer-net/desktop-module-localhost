package localhost

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
	"wayseer/pkg/sdk"
)

// macBytes builds a darwin structure: put writes v little-endian at off.
type macBytes []byte

func (b macBytes) put(off int, v any) macBytes {
	switch v := v.(type) {
	case uint16:
		binary.LittleEndian.PutUint16(b[off:], v)
	case uint32:
		binary.LittleEndian.PutUint32(b[off:], v)
	case int32:
		binary.LittleEndian.PutUint32(b[off:], uint32(v)) //nolint:gosec // test values are small
	case uint64:
		binary.LittleEndian.PutUint64(b[off:], v)
	case string:
		copy(b[off:], v)
	}
	return b
}

func TestDarwinParseCPULoad(t *testing.T) {
	// two CPUs: user, system, idle, nice
	b := make(macBytes, 32).put(0, uint32(10)).put(4, uint32(5)).put(8, uint32(80)).put(12, uint32(5)).
		put(16, uint32(1)).put(20, uint32(1)).put(24, uint32(98)).put(28, uint32(0))
	total, cpus, err := parseCPULoad(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []namedCPU{{"cpu0", cpuTimes{busy: 20, total: 100}}, {"cpu1", cpuTimes{busy: 2, total: 100}}}
	if !slices.Equal(cpus, want) || total != (cpuTimes{busy: 22, total: 200}) {
		t.Errorf("cpus %v total %v; want %v and 22/200", cpus, total, want)
	}
	if _, _, err := parseCPULoad(b[:20]); err == nil {
		t.Error("a partial CPU parsed")
	}
}

func TestDarwinParseMemory(t *testing.T) {
	vm := make(macBytes, vmStatsSize).put(vmFree, uint32(100)).put(vmInactive, uint32(50)).put(vmActive, uint32(999))
	avail, err := parseVMStats(vm, 16384)
	if err != nil || avail != 150*16384 {
		t.Errorf("available %d, %v; want %d", avail, err, 150*16384)
	}
	if _, err := parseVMStats(vm[:40], 16384); err == nil {
		t.Error("short vm statistics parsed")
	}
	swap := make(macBytes, 32).put(0, uint64(2<<30)).put(8, uint64(1<<30)).put(16, uint64(1<<30))
	total, free, err := parseSwapUsage(swap)
	if err != nil || total != 2<<30 || free != 1<<30 {
		t.Errorf("swap %d free %d, %v", total, free, err)
	}
}

func statfsRecord(device, point, fstype string, flags uint32, bsize uint32, blocks, bfree, bavail uint64) macBytes {
	return make(macBytes, statfsSize).put(fsBsize, bsize).put(fsBlocks, blocks).put(fsBfree, bfree).put(fsBavail, bavail).
		put(fsFlags, flags).put(fsTypename, fstype).put(fsOnName, point).put(fsFromName, device)
}

func TestDarwinParseFilesystems(t *testing.T) {
	var b []byte
	b = append(b, statfsRecord("/dev/disk3s1s1", "/", "apfs", 0x4001, 4096, 1000, 400, 300)...)
	b = append(b, statfsRecord("devfs", "/dev", "devfs", 0, 512, 10, 0, 0)...)
	b = append(b, statfsRecord("/dev/disk3s6", "/System/Volumes/VM", "apfs", mntDontBrowse, 4096, 1000, 400, 300)...)
	b = append(b, statfsRecord("/dev/disk3s5", "/System/Volumes/Data", "apfs", mntDontBrowse, 4096, 1000, 500, 500)...)
	fss, err := parseFsstat(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(fss) != 2 {
		t.Fatalf("%d filesystems %+v; want / and the hidden Data volume, not devfs or the hidden VM volume", len(fss), fss)
	}
	root := fss[0]
	if root.id != "disk3s1s1" || root.name != "/" || root.fstype != "apfs" || !slices.Equal(root.mounts, []string{"/"}) {
		t.Errorf("root is %+v", root)
	}
	if root.usage != (fsUsage{total: 1000 * 4096, used: 600 * 4096, avail: 300 * 4096}) {
		t.Errorf("root usage %+v", root.usage)
	}
	if _, err := parseFsstat(b[:100]); err == nil {
		t.Error("a partial record parsed")
	}
}

func kinfoRecord(pid, ppid int32, uid uint32, comm string, start int64) macBytes {
	return make(macBytes, kinfoSize).put(kpStartSec, uint64(start)).put(kpPid, pid).put(kpPpid, ppid).
		put(kpRuid, uid).put(kpComm, comm)
}

func TestDarwinParseProcesses(t *testing.T) {
	boot := time.Unix(1_700_000_000, 0)
	var b []byte
	b = append(b, kinfoRecord(0, 0, 0, "kernel_task", boot.Unix())...)
	b = append(b, kinfoRecord(1, 0, 0, "launchd", boot.Unix()+2)...)
	b = append(b, kinfoRecord(501, 1, 501, "a sixteen-char-name", boot.Unix()+60)...)
	ps, err := parseKinfoProcs(b, boot)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("%d processes; want launchd and one more, not the kernel", len(ps))
	}
	if p := ps[1]; p.pid != 501 || p.ppid != 1 || p.uid != 501 || p.comm != "a sixteen-char-n" || p.start != 60*clockTick {
		t.Errorf("process is %+v", p)
	}
	if _, err := parseKinfoProcs(b[:kinfoSize+1], boot); err == nil {
		t.Error("a partial record parsed")
	}
}

func TestDarwinParseTaskInfo(t *testing.T) {
	b := make(macBytes, taskInfoSize).put(8, uint64(64<<20)).put(16, uint64(3e9)).put(24, uint64(1e9))
	rss, cpu, err := parseTaskInfo(b, 1, 1)
	if err != nil || rss != 64<<20 || cpu != 4*clockTick {
		t.Errorf("rss %d cpu %d, %v; want 64 MiB and 4s in ticks", rss, cpu, err)
	}
	// Apple silicon counts in 41.67ns units
	if _, cpu, _ := parseTaskInfo(b, 125, 3); cpu != 4*125*clockTick/3 {
		t.Errorf("cpu %d with the arm64 timebase", cpu)
	}
}

func TestDarwinParseProcArgs(t *testing.T) {
	b := make(macBytes, 4).put(0, int32(2))
	b = append(b, "/usr/bin/ssh\x00\x00\x00\x00ssh\x00host\x00TERM=xterm\x00"...)
	if got := parseProcArgs(b); got != "ssh host" {
		t.Errorf("command %q; want the two arguments without the environment", got)
	}
	if got := parseProcArgs(b[:2]); got != "" {
		t.Errorf("short args gave %q", got)
	}
}

func ifRecord(typ uint8, index uint16, rx, tx uint64) macBytes {
	b := make(macBytes, ifMsgSize).put(0, uint16(ifMsgSize)).put(ifIndex, index).put(ifIbytes, rx).put(ifObytes, tx)
	b[3] = typ
	return b
}

func TestDarwinParseInterfaces(t *testing.T) {
	var b []byte
	b = append(b, ifRecord(rtmIfinfo2, 4, 1000, 2000)...)
	b = append(b, ifRecord(0x13, 4, 9, 9)...) // a multicast address message
	b = append(b, ifRecord(rtmIfinfo2, 7, 5, 6)...)
	got, err := parseIfList2(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[4] != (netCounters{rx: 1000, tx: 2000}) || got[7] != (netCounters{rx: 5, tx: 6}) {
		t.Errorf("counters %v", got)
	}
	if _, err := parseIfList2(b[:ifMsgSize-1]); err == nil {
		t.Error("a partial message parsed")
	}
}

func TestDarwinParseCatalogueLeavesOutDisks(t *testing.T) {
	cat := macCatalogue()
	for _, m := range cat {
		if slices.Contains(m.Kinds, sdk.KindDisk) {
			t.Errorf("%s is in the macOS catalogue; macOS disks are not read", m.Name)
		}
		if strings.Contains(m.Native, "/proc") {
			t.Errorf("%s comes from %q on macOS", m.Name, m.Native)
		}
	}
	if len(cat) != len(catalogue)-3 {
		t.Errorf("%d metrics; want all but the three disk ones", len(cat))
	}
}

// macSource replays samples as the macOS reader takes them.
type macSource struct{ samples []*sample }

func (s *macSource) read(now time.Time) (*sample, error) {
	out := s.samples[0]
	s.samples = s.samples[1:]
	out.at = now
	return out, nil
}

func macSample(ownCPU uint64) *sample {
	return &sample{
		host: hostInfo{name: "mac", kernel: "24.5.0", os: "macOS 15.5"},
		stat: procStat{total: cpuTimes{busy: ownCPU, total: 1000 + ownCPU}, cpus: []namedCPU{{"cpu0", cpuTimes{busy: ownCPU, total: 1000 + ownCPU}}}},
		mem:  memInfo{total: 16 << 30, available: 8 << 30},
		procs: []process{
			{pidStat: pidStat{pid: 1, comm: "launchd"}, user: "root", unmeasured: true},
			{pidStat: pidStat{pid: 501, ppid: 1, comm: "zsh", cpu: ownCPU, rss: 10}, user: "owner"},
		},
	}
}

func TestDarwinParseAnotherUsersProcessHasNoPoints(t *testing.T) {
	m := New()
	configure(t, m, "root: "+t.TempDir())
	m.reader, m.metrics = &macSource{[]*sample{macSample(0), macSample(100)}}, macCatalogue()
	m.poll(t.Context(), time.Unix(1000, 0))
	m.poll(t.Context(), time.Unix(1001, 0))
	if got := latest(t, m, ref(sdk.KindProcess, "501"), MetricCPU); got != 100 {
		t.Errorf("zsh is %v%% busy; want 100", got)
	}
	ss, err := m.QuerySeries(t.Context(), sdk.SeriesQuery{
		Entities: []sdk.EntityRef{ref(sdk.KindProcess, "1")}, Metrics: []string{MetricCPU, MetricRSS},
		Window: sdk.TimeWindow{From: time.Unix(0, 0), To: time.Unix(1e6, 0)},
	})
	if err != nil || len(ss) != 2 || len(ss[0].Points)+len(ss[1].Points) != 0 {
		t.Errorf("launchd's series %+v, %v; want both empty, not zero", ss, err)
	}
	if slices.ContainsFunc(m.Metrics(), func(x sdk.Metric) bool { return x.Name == MetricDiskRead }) {
		t.Error("the module offers disk.read on macOS")
	}
}
