package localhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"wayseer.dev/sdk"
)

// fakeSystem replays recorded systemd replies and journal lines.
type fakeSystem struct {
	mu          sync.Mutex
	list        []unitReply
	props       map[string]map[string]any // D-Bus properties by unit, as godbus decodes them
	depCalls    map[string]int
	connectErr  error
	unitsErr    error
	journalText []byte
	journalRuns [][]string
}

func newFakeSystem(t *testing.T) *fakeSystem {
	t.Helper()
	f := &fakeSystem{props: map[string]map[string]any{}, depCalls: map[string]int{}}
	var reply struct{ Data [][][]any }
	readJSON(t, "testdata/systemd/list-units.json", &reply)
	for _, r := range reply.Data[0] {
		f.list = append(f.list, unitReply{
			Name: r[0].(string), Description: r[1].(string), LoadState: r[2].(string),
			ActiveState: r[3].(string), SubState: r[4].(string),
		})
	}
	var props map[string]map[string]busJSON
	readJSON(t, "testdata/systemd/dependencies.json", &props)
	for name, p := range props {
		f.setProps(t, name, p)
	}
	return f
}

// busJSON is a property as `busctl --json` prints it.
type busJSON struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// value decodes the property into the Go type godbus gives its signature.
func (b busJSON) value(t *testing.T) any {
	t.Helper()
	var v any
	switch b.Type {
	case "s":
		v = new(string)
	case "as":
		v = new([]string)
	case "b":
		v = new(bool)
	case "t":
		v = new(uint64)
	case "i":
		v = new(int32)
	default:
		t.Fatalf("no decoding for D-Bus type %q", b.Type)
	}
	if err := json.Unmarshal(b.Data, v); err != nil {
		t.Fatal(err)
	}
	return reflect.ValueOf(v).Elem().Interface()
}

// setProps sets or replaces recorded properties of a unit.
func (f *fakeSystem) setProps(t *testing.T, name string, props map[string]busJSON) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.props[name] == nil {
		f.props[name] = map[string]any{}
	}
	for k, v := range props {
		f.props[name][k] = v.value(t)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSystem) connect(context.Context) (unitSource, error) { return f, f.connectErr }

func (f *fakeSystem) units(context.Context) ([]unitReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list), f.unitsErr
}

func (f *fakeSystem) details(_ context.Context, u unitReply) (unitProps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depCalls[u.Name]++
	p := f.props[u.Name]
	return propsFrom(func(name string) any { return p[name] }), nil
}

func (f *fakeSystem) close() {}

// journal sends the recorded lines, then waits as --follow does until ctx ends.
func (f *fakeSystem) journal(ctx context.Context, args []string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.journalRuns = append(f.journalRuns, args)
	text := f.journalText
	f.mu.Unlock()
	r, w := io.Pipe()
	go func() {
		_, _ = w.Write(text)
		<-ctx.Done()
		_ = w.Close()
	}()
	return r, nil
}

func (f *fakeSystem) setState(name, active, sub string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.list, func(u unitReply) bool { return u.Name == name })
	f.list[i].ActiveState, f.list[i].SubState = active, sub
}

func systemModule(t *testing.T, f *fakeSystem, extra string) *Module {
	t.Helper()
	m := New()
	m.system = f
	m.statfs = func(string) (fsUsage, error) { return fsUsage{total: 100, used: 40, avail: 60}, nil }
	configure(t, m, "root: "+copyFixture(t)+"\n"+extra)
	return m
}

func unitRef(name string) sdk.EntityRef {
	kind, _ := unitKind(name)
	return ref(kind, name)
}

func TestUnitsFromRecordedReply(t *testing.T) {
	m := systemModule(t, newFakeSystem(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	for _, name := range []string{"basic.target", "multi-user.target", "sshd.service", "cups.service", "cups.socket", "backup.timer", "nginx.service", "user@1000.service", "systemd-journald.service"} {
		if _, ok := m.world.ents[unitRef(name)]; !ok {
			t.Errorf("%s not listed", name)
		}
	}
	for _, name := range []string{"sshdgenkeys.service", "backup.service", "dev-nvme0n1.device", "boot.mount", "init.scope", "tmp.mount"} {
		if _, ok := m.world.ents[unitRef(name)]; ok {
			t.Errorf("%s listed; inactive units and unwanted types are left out", name)
		}
	}
	sshd := m.world.ents[unitRef("sshd.service")]
	if sshd.Kind != sdk.KindService || sshd.Name != "sshd" || sshd.Attrs["description"].Str() != "OpenSSH Daemon" || sshd.Attrs["sub_state"].Str() != "running" {
		t.Errorf("sshd = %+v", sshd)
	}
	if timer := m.world.ents[unitRef("backup.timer")]; timer.Kind != KindUnit || timer.Name != "backup.timer" {
		t.Errorf("timer = %+v", timer)
	}
}

func TestUnitStatus(t *testing.T) {
	m := systemModule(t, newFakeSystem(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	for name, want := range map[string]sdk.Status{
		"sshd.service":  {Level: sdk.StatusOK},
		"cups.service":  {Level: sdk.StatusDown, Reason: "should be running"}, // multi-user.target wants it
		"nginx.service": {Level: sdk.StatusWarn, Reason: "starting"},
		"backup.timer":  {Level: sdk.StatusOK},
	} {
		if got := m.world.ents[unitRef(name)].Status; got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
}

func TestUnitEdges(t *testing.T) {
	m := systemModule(t, newFakeSystem(t), "")
	m.poll(context.Background(), time.Unix(1000, 0))
	host := ref(sdk.KindHost, "testbox")
	want := map[sdk.EdgeKey]float64{
		{From: unitRef("multi-user.target"), To: unitRef("basic.target"), Rel: sdk.RelDependsOn}:         1,
		{From: unitRef("multi-user.target"), To: unitRef("sshd.service"), Rel: sdk.RelDependsOn}:         0.5,
		{From: unitRef("cups.service"), To: unitRef("cups.socket"), Rel: sdk.RelDependsOn}:               1,
		{From: unitRef("nginx.service"), To: unitRef("systemd-journald.service"), Rel: sdk.RelDependsOn}: 1,
		{From: unitRef("nginx.service"), To: unitRef("basic.target"), Rel: sdk.RelDependsOn}:             1,
		{From: unitRef("sshd.service"), To: host, Rel: sdk.RelRunsOn}:                                    1,
		{From: ref(sdk.KindProcess, "412"), To: unitRef("sshd.service"), Rel: sdk.RelMemberOf}:           1,
		{From: ref(sdk.KindProcess, "1201"), To: unitRef("user@1000.service"), Rel: sdk.RelMemberOf}:     1,
	}
	for k, w := range want {
		if e, ok := m.world.edges[k]; !ok || e.Weight != w {
			t.Errorf("edge %v: %+v, %v; want weight %v", k, e, ok, w)
		}
	}
	for k := range m.world.edges {
		if k.To == unitRef("sshdgenkeys.service") || k.From == ref(sdk.KindProcess, "1") && k.Rel == sdk.RelMemberOf {
			t.Errorf("unexpected edge %v", k)
		}
	}
}

func TestDependenciesAreCachedUntilStateChanges(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	m.poll(context.Background(), time.Unix(1000, 0))
	m.poll(context.Background(), time.Unix(1002, 0))
	if n := f.depCalls["sshd.service"]; n != 1 {
		t.Errorf("sshd dependencies read %d times over two polls, want 1", n)
	}
	f.setState("sshd.service", "activating", "start")
	m.poll(context.Background(), time.Unix(1004, 0))
	if n := f.depCalls["sshd.service"]; n != 2 {
		t.Errorf("sshd dependencies read %d times after a restart, want 2", n)
	}
}

func TestUnitTypesOption(t *testing.T) {
	m := systemModule(t, newFakeSystem(t), "units: [mount]")
	m.poll(context.Background(), time.Unix(1000, 0))
	if _, ok := m.world.ents[unitRef("boot.mount")]; !ok {
		t.Error("boot.mount not listed with units: [mount]")
	}
	if _, ok := m.world.ents[unitRef("sshd.service")]; ok {
		t.Error("sshd listed with units: [mount]")
	}
	off := systemModule(t, newFakeSystem(t), "units: []")
	off.poll(context.Background(), time.Unix(1000, 0))
	if _, ok := off.world.ents[unitRef("sshd.service")]; ok {
		t.Error("units listed with units: []")
	}
}

func TestNoSystemdIsANoteNotAnError(t *testing.T) {
	f := newFakeSystem(t)
	f.connectErr = errNoSystemd
	m := systemModule(t, f, "")
	m.poll(context.Background(), time.Unix(1000, 0))
	if h := m.Health(); h.Err != nil || h.Note != "systemd not available" {
		t.Errorf("health = %+v", h)
	}
	if _, ok := m.world.ents[ref(sdk.KindHost, "testbox")]; !ok {
		t.Error("the host is missing without systemd")
	}
}

func TestUnitListFailureKeepsLastUnits(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	m.poll(context.Background(), time.Unix(1000, 0))
	f.unitsErr = errors.New("connection reset")
	m.poll(context.Background(), time.Unix(1002, 0))
	if _, ok := m.world.ents[unitRef("sshd.service")]; !ok {
		t.Error("units dropped after one failed listing")
	}
	if h := m.Health(); h.Err != nil || h.Note != "systemd: connection reset" {
		t.Errorf("health = %+v", h)
	}
}

func TestParseCgroup(t *testing.T) {
	for in, want := range map[string]string{
		"0::/system.slice/sshd.service\n":                                        "/system.slice/sshd.service",
		"12:pids:/user.slice\n1:name=systemd:/system.slice/cron.service\n0::/\n": "/system.slice/cron.service",
		"": "",
	} {
		if got := parseCgroup([]byte(in)); got != want {
			t.Errorf("parseCgroup(%q) = %q, want %q", in, got, want)
		}
	}
}
