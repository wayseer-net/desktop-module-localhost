package localhost

import (
	"context"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
	"unicode/utf16"

	"wayseer.dev/sdk"
	"wayseer.dev/sdk/sdktest"

	"go.yaml.in/yaml/v3"
)

type recordedTimes struct{ Idle, Kernel, User uint64 }

type recordedRead struct {
	SystemTimes recordedTimes   `yaml:"system_times"`
	Processors  []recordedTimes `yaml:"processors"`
	Memory      struct{ Total, Avail uint64 }
	Counters    map[int]struct{ In, Out uint64 }
	Running     map[uint32]uint64 // CPU time by pid
	Services    map[string]recordedStatus
}

type recordedProcess struct {
	PPID               uint32
	Exe, User, Command string
	Created            int64
	WS                 uint64 `yaml:"ws"`
	Denied             bool
}

type recordedService struct {
	Display            string
	Start              uint32
	Delayed, Triggered bool
	Deps               []string
	Group              string
}

type recordedStatus struct{ State, PID, Exit, Specific uint32 }

type recordedDrive struct {
	Root, FSType       string
	Type               uint32
	Total, Free, Avail uint64
	Locked             bool
}

type recordedInterface struct {
	Index, MTU   int
	Name, MAC    string
	Up, Loopback bool
	Addrs        []string
}

// recordedWindows answers the reader's calls from testdata/windows; next moves to the next read.
type recordedWindows struct {
	Hostname string
	Version  struct {
		Major, Minor, Build uint32
		Product, Display    string
	}
	SinceBoot  time.Duration `yaml:"since_boot"`
	Drives     []recordedDrive
	Interfaces []recordedInterface
	Processes  map[uint32]recordedProcess
	Services   map[string]recordedService
	Reads      []recordedRead
	read       int
}

func loadRecordedWindows(t *testing.T) *recordedWindows {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "windows", "machine.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var w recordedWindows
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	return &w
}

func (w *recordedWindows) next() { w.read = min(w.read+1, len(w.Reads)-1) }

func (w *recordedWindows) now() recordedRead { return w.Reads[w.read] }

func (w *recordedWindows) hostname() (string, error) { return w.Hostname, nil }

func (w *recordedWindows) version() winVersion {
	v := w.Version
	return winVersion{major: v.Major, minor: v.Minor, build: v.Build, product: v.Product, display: v.Display}
}

func (w *recordedWindows) sinceBoot() time.Duration { return w.SinceBoot }

func (w *recordedWindows) systemTimes() (idle, kernel, user uint64, err error) {
	st := w.now().SystemTimes
	return st.Idle, st.Kernel, st.User, nil
}

func (w *recordedWindows) processorTimes() ([]byte, error) {
	var b []byte
	for _, p := range w.now().Processors {
		b = append(b, processorRecord(p.Idle, p.Kernel, p.User)...)
	}
	return b, nil
}

func (w *recordedWindows) memoryStatus() ([]byte, error) {
	m := w.now().Memory
	return make(macBytes, memStatusSize).put(memTotalPhys, m.Total).put(memAvailPhys, m.Avail), nil
}

func (w *recordedWindows) driveStrings() ([]uint16, error) {
	var b []uint16
	for _, d := range w.Drives {
		b = append(b, utf16.Encode([]rune(d.Root+"\x00"))...)
	}
	return append(b, 0), nil
}

func (w *recordedWindows) drive(root string) recordedDrive {
	i := slices.IndexFunc(w.Drives, func(d recordedDrive) bool { return d.Root == root })
	return w.Drives[i]
}

func (w *recordedWindows) driveType(root string) uint32 { return w.drive(root).Type }

var errLocked = errors.New("this drive is locked by BitLocker Drive Encryption")

func (w *recordedWindows) fileSystem(root string) (string, error) {
	if d := w.drive(root); !d.Locked {
		return d.FSType, nil
	}
	return "", errLocked
}

func (w *recordedWindows) diskFree(root string) (fsUsage, error) {
	d := w.drive(root)
	if d.Locked {
		return fsUsage{}, errLocked
	}
	return fsUsage{total: d.Total, used: d.Total - d.Free, avail: d.Avail}, nil
}

func (w *recordedWindows) interfaces() ([]net.Interface, error) {
	var out []net.Interface
	for _, i := range w.Interfaces {
		mac, _ := net.ParseMAC(i.MAC)
		n := net.Interface{Index: i.Index, MTU: i.MTU, Name: i.Name, HardwareAddr: mac}
		if i.Up {
			n.Flags |= net.FlagUp | net.FlagRunning
		}
		if i.Loopback {
			n.Flags |= net.FlagLoopback
		}
		out = append(out, n)
	}
	return out, nil
}

func (w *recordedWindows) ifTable() ([]byte, error) {
	var rows []macBytes
	for index, c := range w.now().Counters {
		rows = append(rows, ifRow(uint32(index), c.In, c.Out)) //nolint:gosec // small test indexes
	}
	return ifTable(rows...), nil
}

func (w *recordedWindows) addrs() map[string][]string {
	out := map[string][]string{}
	for _, i := range w.Interfaces {
		if len(i.Addrs) > 0 {
			out[i.Name] = i.Addrs
		}
	}
	return out
}

var errDenied = errors.New("access is denied")

func (w *recordedWindows) processes() ([]winProcess, error) {
	var out []winProcess
	for _, pid := range slices.Sorted(maps.Keys(w.now().Running)) {
		out = append(out, winProcess{pid: pid, ppid: w.Processes[pid].PPID, exe: w.Processes[pid].Exe})
	}
	return out, nil
}

func (w *recordedWindows) process(pid uint32) (recordedProcess, error) {
	if p := w.Processes[pid]; !p.Denied {
		return p, nil
	}
	return recordedProcess{}, errDenied
}

func (w *recordedWindows) processTimes(pid uint32) (winProcTimes, error) {
	p, err := w.process(pid)
	return winProcTimes{created: time.Unix(p.Created, 0), cpu: w.now().Running[pid], workingSet: p.WS}, err
}

func (w *recordedWindows) processUser(pid uint32) (string, error) {
	p, err := w.process(pid)
	return p.User, err
}

func (w *recordedWindows) processCommand(pid uint32) (string, error) {
	p, err := w.process(pid)
	return p.Command, err
}

// recordedSCM answers the service control manager's calls from the recording.
type recordedSCM struct{ w *recordedWindows }

func (s recordedSCM) services() ([]winService, error) {
	var out []winService
	for _, name := range slices.Sorted(maps.Keys(s.w.Services)) {
		st := s.w.now().Services[name]
		out = append(out, winService{
			name: name, display: s.w.Services[name].Display,
			state: st.State, pid: st.PID, exit: st.Exit, specific: st.Specific,
		})
	}
	return out, nil
}

func (s recordedSCM) config(name string) (winServiceConfig, error) {
	c, ok := s.w.Services[name]
	if !ok {
		return winServiceConfig{}, errors.New("the specified service does not exist as an installed service")
	}
	return winServiceConfig{start: c.Start, delayed: c.Delayed, triggered: c.Triggered, deps: c.Deps, group: c.Group}, nil
}

func (recordedSCM) close() {}

// windowsPlatform reads the recorded Windows machine and its services.
func windowsPlatform(w *recordedWindows) func(*reader) platform {
	return func(r *reader) platform {
		open := func() (scmAPI, error) { return recordedSCM{w}, nil }
		return platform{src: newWinReader(w, r), metrics: winCatalogue(), units: scmSystem{open: open}}
	}
}

// windowsModule is a module reading the recorded Windows machine.
func windowsModule(t *testing.T, w *recordedWindows, opts string) *Module {
	t.Helper()
	m := New()
	m.addrs, m.system, m.native = w.addrs, nil, windowsPlatform(w)
	configure(t, m, opts)
	return m
}

func TestWindowsRecordedMachine(t *testing.T) {
	m := windowsModule(t, loadRecordedWindows(t), "interval: 2s")
	m.poll(context.Background(), time.Unix(100000, 0))
	if err := m.Health().Err; err != nil {
		t.Fatal(err)
	}
	byKind := map[sdk.Kind][]string{}
	for _, e := range m.world.ents {
		byKind[e.Kind] = append(byKind[e.Kind], e.Ref.Native())
	}
	for k, want := range map[sdk.Kind][]string{
		sdk.KindHost:      {"WINBOX"},
		KindCPU:           {"cpu0", "cpu1", "cpu2", "cpu3"},
		KindMemory:        {"memory"},
		sdk.KindDisk:      nil,
		KindFilesystem:    {"C:", "F:", "G:"},
		sdk.KindInterface: {"Ethernet", "Wi-Fi", "vEthernet (WSL)"},
	} {
		if got := slices.Sorted(slices.Values(byKind[k])); !slices.Equal(got, want) {
			t.Errorf("%s = %v; want %v", k, got, want)
		}
	}
}

func TestWindowsRecordedHost(t *testing.T) {
	m := windowsModule(t, loadRecordedWindows(t), "interval: 2s")
	m.poll(context.Background(), time.Unix(100000, 0))
	host := m.world.ents[ref(sdk.KindHost, "WINBOX")]
	if host.Attrs["os"].Str() != "Windows 11 Pro 24H2" || host.Attrs["kernel"].Str() != "10.0.26100" ||
		host.Attrs["cores"].Num() != 4 || host.Attrs["memory"].Num() != 16<<30 {
		t.Errorf("host attrs = %v", host.Attrs)
	}
	if boot := host.Attrs["booted"].Time(); !boot.Equal(time.Unix(100000, 0).Add(-(3*time.Hour + 25*time.Minute))) {
		t.Errorf("booted %v", boot)
	}
	if ips := host.Attrs["ip"].String(); ips != sdk.List(sdk.String("192.0.2.10"), sdk.String("2001:db8::10"), sdk.String("172.20.0.1")).String() {
		t.Errorf("host ips %s", ips)
	}
}

func TestWindowsRecordedVolumesAndInterfaces(t *testing.T) {
	m := windowsModule(t, loadRecordedWindows(t), "interval: 2s")
	m.poll(context.Background(), time.Unix(100000, 0))
	c := m.world.ents[ref(KindFilesystem, "C:")]
	if c.Name != `C:\` || c.Attrs["type"].Str() != "NTFS" || c.Attrs["size"].Num() != 512<<30 || c.Status.Level != sdk.StatusOK {
		t.Errorf("C: = %+v", c)
	}
	if f := m.world.ents[ref(KindFilesystem, "F:")]; f.Status.Level != sdk.StatusWarn {
		t.Errorf("F:, 90%% full, status %+v", f.Status)
	}
	if g := m.world.ents[ref(KindFilesystem, "G:")]; g.Status.Level != sdk.StatusUnknown {
		t.Errorf("G:, locked, status %+v", g.Status)
	}
	eth := m.world.ents[ref(sdk.KindInterface, "Ethernet")]
	if eth.Status.Level != sdk.StatusOK || eth.Attrs["mac"].Str() != "00:15:5d:0a:0b:0c" || eth.Attrs["mtu"].Num() != 1500 {
		t.Errorf("Ethernet = %+v", eth)
	}
	if wifi := m.world.ents[ref(sdk.KindInterface, "Wi-Fi")]; wifi.Status.Level != sdk.StatusDown {
		t.Errorf("Wi-Fi, disconnected, status %+v", wifi.Status)
	}
}

func TestWindowsRecordedSeries(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	m.poll(context.Background(), time.Unix(100000, 0))
	w.next()
	m.poll(context.Background(), time.Unix(100002, 0))
	for _, c := range []struct {
		ref    sdk.EntityRef
		metric string
		want   float64
	}{
		{ref(sdk.KindHost, "WINBOX"), MetricCPU, 18.75},
		{ref(KindCPU, "cpu0"), MetricCPU, 25},
		{ref(KindCPU, "cpu1"), MetricCPU, 50},
		{ref(KindCPU, "cpu2"), MetricCPU, 0},
		{ref(sdk.KindHost, "WINBOX"), MetricMemUtil, 62.5},
		{ref(KindMemory, "memory"), MetricMemAvail, 6 << 30},
		{ref(KindFilesystem, "C:"), MetricFSUsed, 312 << 30},
		{ref(sdk.KindInterface, "Ethernet"), MetricNetReceive, 100000},
		{ref(sdk.KindInterface, "Ethernet"), MetricNetSend, 20000},
	} {
		if got := latest(t, m, c.ref, c.metric); got != c.want {
			t.Errorf("%s %s = %v; want %v", c.ref, c.metric, got, c.want)
		}
	}
	ss, _ := m.QuerySeries(context.Background(), sdk.SeriesQuery{
		Entities: []sdk.EntityRef{ref(KindMemory, "memory")}, Metrics: []string{MetricSwapUsed},
		Window: sdk.TimeWindow{From: time.Unix(0, 0), To: time.Unix(200000, 0)},
	})
	if len(ss) != 0 {
		t.Errorf("swap series %+v; Windows reports no swap", ss)
	}
}

func TestWindowsCatalogue(t *testing.T) {
	m := windowsModule(t, loadRecordedWindows(t), "interval: 2s")
	var names []string
	for _, metric := range m.Metrics() {
		names = append(names, metric.Name)
		linux := catalogue[slices.IndexFunc(catalogue, func(l sdk.Metric) bool { return l.Name == metric.Name })]
		if metric.Native == "" || metric.Native == linux.Native {
			t.Errorf("%s keeps Linux's native %q", metric.Name, metric.Native)
		}
	}
	for _, missing := range []string{MetricSwapUsed, MetricDiskRead, MetricDiskWrite, MetricDiskUtil} {
		if slices.Contains(names, missing) {
			t.Errorf("Windows lists %s, which it doesn't read", missing)
		}
	}
}

func TestWindowsConformance(t *testing.T) {
	sdktest.Conform(t, sdktest.Case{
		New: func() sdk.Module {
			w := loadRecordedWindows(t)
			m := New()
			m.addrs, m.system, m.native = w.addrs, nil, windowsPlatform(w)
			return m
		},
		Name:    "local",
		Options: "interval: 100ms",
	})
}
