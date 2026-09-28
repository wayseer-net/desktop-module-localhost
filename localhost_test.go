package localhost

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"mindseye/pkg/sdk"
	"mindseye/pkg/sdk/sdktest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// copyFixture copies the captured tree so a test can change it between polls.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	return dst
}

// fakeUsage is statfs for the fixture: every filesystem is 100 GiB, 40 used, 60 free.
var fakeUsage = map[string]fsUsage{}

func testModule(t *testing.T, root, extra string) *Module {
	t.Helper()
	m := New()
	m.statfs = func(p string) (fsUsage, error) {
		rel, _ := filepath.Rel(root, p)
		if u, ok := fakeUsage[rel]; ok {
			return u, nil
		}
		if rel == "media/usb stick" {
			return fsUsage{}, errors.New("unplugged")
		}
		return fsUsage{total: 100 << 30, used: 40 << 30, avail: 60 << 30}, nil
	}
	configure(t, m, "root: "+root+"\n"+extra)
	return m
}

func configure(t *testing.T, m *Module, opts string) {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(opts), &n); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(context.Background(), sdk.Config{Name: "local", Options: *n.Content[0]}); err != nil {
		t.Fatal(err)
	}
}

func ref(kind sdk.Kind, native string) sdk.EntityRef {
	r, _ := sdk.NewEntityRef("local", kind, native)
	return r
}

func TestConformance(t *testing.T) {
	root := copyFixture(t)
	sdktest.Conform(t, sdktest.Case{
		New:     func() sdk.Module { return New() },
		Name:    "local",
		Options: "interval: 100ms\nroot: " + root,
		Failing: "root: " + filepath.Join(root, "missing"),
	})
}

func TestWorldFromFixture(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	byKind := map[sdk.Kind][]string{}
	for _, e := range m.world.ents {
		byKind[e.Kind] = append(byKind[e.Kind], e.Ref.Native())
	}
	for k, want := range map[sdk.Kind][]string{
		sdk.KindHost:      {"testbox"},
		KindCPU:           {"cpu0", "cpu1", "cpu2", "cpu3"},
		KindMemory:        {"memory"},
		sdk.KindDisk:      {"nvme0n1"},
		KindFilesystem:    {"dm-0", "nvme0n1p1", "sdz1"},
		sdk.KindInterface: {"eth0", "wlan0"},
		sdk.KindProcess:   {"1", "1200", "1201", "412"},
	} {
		if got := slices.Sorted(slices.Values(byKind[k])); !slices.Equal(got, want) {
			t.Errorf("%s = %v; want %v", k, got, want)
		}
	}
}

func TestAttributesAndStatus(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	host := m.world.ents[ref(sdk.KindHost, "testbox")]
	if host.Attrs["os"].Str() != "Testix Linux 1.0" || host.Attrs["cores"].Num() != 4 || host.Attrs["kernel"].Str() != "6.9.1-test" {
		t.Errorf("host attrs = %v", host.Attrs)
	}
	root := m.world.ents[ref(KindFilesystem, "dm-0")]
	if root.Name != "/" || root.Attrs["mounts"].String() != sdk.List(sdk.String("/"), sdk.String("/home"), sdk.String("/var/log")).String() {
		t.Errorf("root filesystem = %+v", root)
	}
	if st := m.world.ents[ref(KindFilesystem, "sdz1")].Status.Level; st != sdk.StatusUnknown {
		t.Errorf("unreadable filesystem status = %v", st)
	}
	if st := m.world.ents[ref(sdk.KindInterface, "eth0")].Status; st.Level != sdk.StatusDown || st.Reason != "link down" {
		t.Errorf("eth0 status = %+v", st)
	}
	bash := m.world.ents[ref(sdk.KindProcess, "1201")]
	if bash.Name != "bash" || bash.Attrs["user"].Str() != "alice" || bash.Attrs["command"].Str() != "-bash" ||
		!bash.Attrs["started"].Time().Equal(time.Unix(1790302804+50, 100_000_000)) {
		t.Errorf("bash = %+v", bash)
	}
}

func TestEdges(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	host := ref(sdk.KindHost, "testbox")
	for _, e := range []sdk.Edge{
		{From: ref(KindFilesystem, "dm-0"), To: ref(sdk.KindDisk, "nvme0n1"), Rel: sdk.RelRunsOn},
		{From: ref(KindFilesystem, "sdz1"), To: host, Rel: sdk.RelMemberOf},
		{From: ref(sdk.KindProcess, "1200"), To: ref(sdk.KindProcess, "1201"), Rel: sdk.RelParentOf},
		{From: ref(sdk.KindProcess, "1201"), To: host, Rel: sdk.RelRunsOn},
		{From: ref(KindCPU, "cpu2"), To: host, Rel: sdk.RelMemberOf},
	} {
		if _, ok := m.world.edges[e.Key()]; !ok {
			t.Errorf("missing edge %s -%s-> %s", e.From, e.Rel, e.To)
		}
	}
}

func TestCommandsCanBeHidden(t *testing.T) {
	m := testModule(t, copyFixture(t), "commands: false")
	m.poll(context.Background(), time.Unix(1000, 0))
	if _, ok := m.world.ents[ref(sdk.KindProcess, "1201")].Attrs["command"]; ok {
		t.Error("command shown with commands: false")
	}
}

// rewrite replaces old with new in a fixture file.
func rewrite(t *testing.T, root, name, old, replacement string) {
	t.Helper()
	p := filepath.Join(root, name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("%s has no %q", name, old)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(s, old, replacement, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func latest(t *testing.T, m *Module, r sdk.EntityRef, metric string) float64 {
	t.Helper()
	ss, err := m.QuerySeries(context.Background(), sdk.SeriesQuery{
		Entities: []sdk.EntityRef{r}, Metrics: []string{metric},
		Window: sdk.TimeWindow{From: time.Unix(0, 0), To: time.Unix(1e6, 0)},
	})
	if err != nil || len(ss) != 1 || len(ss[0].Points) == 0 {
		t.Fatalf("%s %s: %+v, %v", r, metric, ss, err)
	}
	return ss[0].Points[len(ss[0].Points)-1].V
}

func TestRatesBetweenPolls(t *testing.T) {
	root := copyFixture(t)
	m := testModule(t, root, "")
	m.poll(context.Background(), time.Unix(1000, 0))
	// Over 2 s: 400 ticks pass on 4 CPUs, 100 of them busy on cpu0; bash uses 40.
	rewrite(t, root, "proc/stat", "cpu  4000 100 1000 90000", "cpu  4100 100 1000 90300")
	rewrite(t, root, "proc/stat", "cpu0 1000 25 250 22500", "cpu0 1100 25 250 22500")
	for _, c := range []string{"cpu1", "cpu2", "cpu3"} {
		rewrite(t, root, "proc/stat", c+" 1000 25 250 22500", c+" 1000 25 250 22600")
	}
	rewrite(t, root, "proc/1201/stat", "300 100", "330 110")
	rewrite(t, root, "proc/diskstats", "nvme0n1 346373 16893 21638309", "nvme0n1 346373 16893 21642405")
	rewrite(t, root, "proc/net/dev", "wlan0: 4926113947", "wlan0: 4926115947")
	m.poll(context.Background(), time.Unix(1002, 0))
	for _, c := range []struct {
		r      sdk.EntityRef
		metric string
		want   float64
	}{
		{ref(sdk.KindHost, "testbox"), MetricCPU, 25},
		{ref(KindCPU, "cpu0"), MetricCPU, 100},
		{ref(KindCPU, "cpu1"), MetricCPU, 0},
		{ref(sdk.KindProcess, "1201"), MetricCPU, 5}, // 40 ticks of 800
		{ref(sdk.KindDisk, "nvme0n1"), MetricDiskRead, 4096 * 512 / 2},
		{ref(sdk.KindInterface, "wlan0"), MetricNetReceive, 1000},
		{ref(KindMemory, "memory"), MetricMemUtil, 50},
		{ref(KindFilesystem, "dm-0"), MetricFSUtil, 40},
		{ref(sdk.KindProcess, "1201"), MetricRSS, float64(1000 * os.Getpagesize())},
	} {
		if got := latest(t, m, c.r, c.metric); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s %s = %v; want %v", c.r.Native(), c.metric, got, c.want)
		}
	}
}

func TestDeltasSendOnlyChanges(t *testing.T) {
	root := copyFixture(t)
	m := testModule(t, root, "")
	if cs := m.poll(context.Background(), time.Unix(1000, 0)); len(cs.Upserts) != len(m.world.ents) {
		t.Fatalf("first poll sent %d of %d entities", len(cs.Upserts), len(m.world.ents))
	}
	if cs := m.poll(context.Background(), time.Unix(1002, 0)); !cs.Empty() {
		t.Errorf("unchanged machine sent %+v", cs)
	}
	if err := os.RemoveAll(filepath.Join(root, "proc/1201")); err != nil {
		t.Fatal(err)
	}
	fakeUsage["."] = fsUsage{total: 100, used: 92, avail: 8}
	t.Cleanup(func() { delete(fakeUsage, ".") })
	cs := m.poll(context.Background(), time.Unix(1004, 0))
	if !slices.Equal(cs.Removes, []sdk.EntityRef{ref(sdk.KindProcess, "1201")}) {
		t.Errorf("removes = %v; want bash", cs.Removes)
	}
	if len(cs.Upserts) != 1 || cs.Upserts[0].Status.Level != sdk.StatusWarn {
		t.Errorf("upserts = %+v; want the root filesystem nearly full", cs.Upserts)
	}
	if _, ok := m.series[sdk.SeriesRef{Entity: ref(sdk.KindProcess, "1201"), Metric: MetricRSS}]; ok {
		t.Error("an exited process kept its series")
	}
}

func TestUnreadableRootShowsInHealth(t *testing.T) {
	root := copyFixture(t)
	m := testModule(t, root, "")
	m.poll(context.Background(), time.Unix(1000, 0))
	if err := os.Remove(filepath.Join(root, "proc/stat")); err != nil {
		t.Fatal(err)
	}
	cs := m.poll(context.Background(), time.Unix(1002, 0))
	if err := m.Health().Err; !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "/proc/stat") {
		t.Errorf("health = %v", err)
	}
	if !cs.Empty() {
		t.Errorf("a failed read changed the world: %+v", cs)
	}
}

func TestProcessesCanBeLeftOut(t *testing.T) {
	m := testModule(t, copyFixture(t), "processes: false")
	m.poll(context.Background(), time.Unix(1000, 0))
	for _, e := range m.world.ents {
		if e.Kind == sdk.KindProcess {
			t.Fatalf("process %s listed with processes: false", e.Name)
		}
	}
}

func TestBadOptions(t *testing.T) {
	for _, opts := range []string{
		"interval: 10ms", "history: 5s", "process_history: 2h", "root: relative/path", "interval: soon",
		"units: [widget]", "journal: loud", "journal_backlog: 5000", "keep_stopped: -1s", "keep_stopped: 25h",
	} {
		var n yaml.Node
		_ = yaml.Unmarshal([]byte(opts), &n)
		if err := New().Configure(context.Background(), sdk.Config{Name: "local", Options: *n.Content[0]}); err == nil {
			t.Errorf("Configure(%q) succeeded", opts)
		}
	}
}

func TestUnitsOnSizes(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	sized := map[sdk.EntityRef][]string{
		ref(sdk.KindHost, "testbox"): {"memory"},
		ref(KindMemory, "memory"):    {"total", "swap_total"},
		ref(KindFilesystem, "dm-0"):  {"size"},
		ref(sdk.KindDisk, "nvme0n1"): {"size"},
	}
	for r, keys := range sized {
		for _, k := range keys {
			if u := m.world.ents[r].Attrs[k].Unit(); u != sdk.UnitBytes {
				t.Errorf("%s %s: unit %q, want bytes", r, k, u)
			}
		}
	}
	if u := m.world.ents[ref(sdk.KindInterface, "eth0")].Attrs["mtu"].Unit(); u != sdk.UnitNone {
		t.Errorf("mtu has unit %q", u)
	}
}
