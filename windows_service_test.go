package localhost

import (
	"cmp"
	"context"
	"slices"
	"testing"
	"time"

	"wayseer.dev/sdk"
)

func serviceRef(name string) sdk.EntityRef { return unitRef(name + ".service") }

func winProcessRef(pid string) sdk.EntityRef { return ref(sdk.KindProcess, pid) }

// pollWindows polls the recorded machine once per read, two seconds apart, and returns each
// poll's events.
func pollWindows(t *testing.T, w *recordedWindows, m *Module, reads int) [][]sdk.Event {
	t.Helper()
	var out [][]sdk.Event
	for i := range reads {
		if i > 0 {
			w.next()
		}
		out = append(out, m.poll(context.Background(), time.Unix(100000+2*int64(i), 0)).Events)
		if err := m.Health(); err.Err != nil || err.Note != "" {
			t.Fatalf("read %d: health %+v", i, err)
		}
	}
	return out
}

func TestWindowsServiceReply(t *testing.T) {
	for _, c := range []struct {
		name                     string
		s                        winService
		active, sub, job, result string
		ended                    bool
	}{
		{"running", winService{state: 4, pid: 9}, "active", "running", "", "", false},
		{"starting", winService{state: 2}, "activating", "start pending", "", "", false},
		{"asked to stop", winService{state: 3}, "deactivating", "stop pending", "stop", "", false},
		{"paused", winService{state: 7}, "active", "paused", "", "", false},
		{"stopped cleanly", winService{state: 1}, "inactive", "stopped", "", "", true},
		{"never started", winService{state: 1, exit: 1077}, "inactive", "stopped", "", "", true},
		{"crashed", winService{state: 1, exit: 1067}, "failed", "stopped", "", "error 1067", false},
		{"its own error", winService{state: 1, exit: 1066, specific: 3}, "failed", "stopped", "", "service error 3", false},
	} {
		u := serviceReply(c.s)
		if u.ActiveState != c.active || u.SubState != c.sub || u.JobType != c.job || u.ended != c.ended || serviceResult(c.s) != c.result {
			t.Errorf("%s: %+v, result %q", c.name, u, serviceResult(c.s))
		}
	}
	u := serviceReply(winService{name: "Spooler", display: "Print Spooler", state: 4, pid: 2900})
	if u.Name != "Spooler.service" || u.Description != "Print Spooler" || u.LoadState != "loaded" || u.pid != 2900 {
		t.Errorf("Spooler = %+v", u)
	}
}

func TestWindowsServiceStartType(t *testing.T) {
	for _, c := range []struct {
		c       winServiceConfig
		want    string
		enabled bool
	}{
		{winServiceConfig{start: 2}, "Automatic", true},
		{winServiceConfig{start: 2, delayed: true}, "Automatic (Delayed Start)", true},
		{winServiceConfig{start: 2, triggered: true}, "Automatic (Trigger Start)", false},
		{winServiceConfig{start: 2, delayed: true, triggered: true}, "Automatic (Delayed Start, Trigger Start)", false},
		{winServiceConfig{start: 3}, "Manual", false},
		{winServiceConfig{start: 3, triggered: true}, "Manual (Trigger Start)", false},
		{winServiceConfig{start: 4}, "Disabled", false},
	} {
		if got, enabled := startType(c.c); got != c.want || enabled != c.enabled {
			t.Errorf("%+v: %q, %v; want %q, %v", c.c, got, enabled, c.want, c.enabled)
		}
	}
}

func TestWindowsServicesListed(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	pollWindows(t, w, m, 1)
	var names []string
	for _, e := range m.world.ents {
		if e.Kind == sdk.KindService {
			names = append(names, e.Name)
		}
	}
	want := []string{"BITS", "ContosoSync", "DcomLaunch", "LanmanWorkstation", "RpcEptMapper", "RpcSs", "Spooler", "WinBackup"}
	if slices.Sort(names); !slices.Equal(names, want) {
		t.Errorf("services %v; want the running ones, %v", names, want)
	}
	spooler := m.world.ents[serviceRef("Spooler")]
	if spooler.Attrs["description"].Str() != "Print Spooler" || spooler.Attrs["file_state"].Str() != "Automatic" ||
		spooler.Attrs["state"].Str() != "active" || spooler.Status != okStatus {
		t.Errorf("Spooler = %+v", spooler)
	}
	if bits := m.world.ents[serviceRef("BITS")]; bits.Attrs["file_state"].Str() != "Manual (Trigger Start)" {
		t.Errorf("BITS = %+v", bits)
	}
}

func TestWindowsServiceDependencies(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	pollWindows(t, w, m, 1)
	for _, c := range []struct{ from, to string }{
		{"RpcSs", "RpcEptMapper"},
		{"RpcSs", "DcomLaunch"},
		{"Spooler", "RpcSs"},               // named RPCSS
		{"WinBackup", "LanmanWorkstation"}, // in the NetworkProvider group
		{"BITS", "RpcSs"},
	} {
		e, ok := m.world.edges[sdk.EdgeKey{From: serviceRef(c.from), To: serviceRef(c.to), Rel: sdk.RelDependsOn}]
		if !ok || e.Weight != 1 {
			t.Errorf("%s needs %s: %+v, %v", c.from, c.to, e, ok)
		}
	}
	for k := range m.world.edges {
		if k.Rel == sdk.RelDependsOn && k.From == serviceRef("Spooler") && k.To != serviceRef("RpcSs") {
			t.Errorf("Spooler depends on %s; its other need is a driver", k.To)
		}
	}
}

func TestWindowsServiceProcesses(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	pollWindows(t, w, m, 1)
	for _, c := range []struct{ pid, service string }{
		{"2900", "Spooler"}, {"1040", "RpcSs"}, {"1040", "RpcEptMapper"}, {"880", "DcomLaunch"},
	} {
		if _, ok := m.world.edges[sdk.EdgeKey{From: winProcessRef(c.pid), To: serviceRef(c.service), Rel: sdk.RelMemberOf}]; !ok {
			t.Errorf("process %s is not in %s", c.pid, c.service)
		}
	}
}

func TestWindowsServiceStopsAndStarts(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	polls := pollWindows(t, w, m, 3)
	if len(polls[0]) != 0 {
		t.Errorf("the first listing sent %+v", polls[0])
	}
	type said struct {
		service, message string
		sev              sdk.Severity
	}
	var got [][]said
	for _, evs := range polls[1:] {
		var s []said
		for _, e := range evs {
			s = append(s, said{e.Fields["unit"].Str(), e.Message, e.Severity})
		}
		slices.SortFunc(s, func(a, b said) int { return cmp.Compare(a.service, b.service) })
		got = append(got, s)
	}
	want := [][]said{
		{
			{"BITS.service", "stopped", sdk.SevInfo},
			{"ContosoSync.service", "failed", sdk.SevError},
			{"Spooler.service", "stopping", sdk.SevInfo},
			{"WSearch.service", "started", sdk.SevInfo},
		},
		{{"Spooler.service", "stopped by request", sdk.SevInfo}},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("events %+v;\nwant %+v", got, want)
	}
	for name, st := range map[string]sdk.Status{
		"Spooler":     {Level: sdk.StatusUnknown, Reason: "stopped"},
		"BITS":        {Level: sdk.StatusUnknown, Reason: "stopped"},
		"ContosoSync": shouldRun,
		"WSearch":     okStatus,
	} {
		if e := m.world.ents[serviceRef(name)]; e.Status != st {
			t.Errorf("%s status %+v; want %+v", name, e.Status, st)
		}
	}
	if r := m.world.ents[serviceRef("ContosoSync")].Attrs["result"].Str(); r != "error 1067" {
		t.Errorf("ContosoSync result %q", r)
	}
	if _, ok := m.world.edges[sdk.EdgeKey{From: serviceRef("Spooler"), To: serviceRef("RpcSs"), Rel: sdk.RelDependsOn}]; !ok {
		t.Error("the stopped spooler lost its dependency")
	}
	if _, ok := m.world.edges[sdk.EdgeKey{From: winProcessRef("3700"), To: serviceRef("WSearch"), Rel: sdk.RelMemberOf}]; !ok {
		t.Error("the search indexer is not in WSearch")
	}
}

func TestWindowsProcesses(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s")
	pollWindows(t, w, m, 2)
	if _, ok := m.world.ents[winProcessRef("0")]; ok {
		t.Error("the idle process is listed")
	}
	explorer := m.world.ents[winProcessRef("5200")]
	if explorer.Name != "explorer.exe" || explorer.Attrs["user"].Str() != "alex" ||
		explorer.Attrs["command"].Str() != `C:\Windows\Explorer.EXE` || !explorer.Attrs["started"].Time().Equal(time.Unix(88000, 0)) {
		t.Errorf("explorer = %+v", explorer)
	}
	system := m.world.ents[winProcessRef("4")]
	if system.Name != "System" || system.Attrs["user"].Str() != "" {
		t.Errorf("System = %+v", system)
	}
	if _, ok := system.Attrs["started"]; ok {
		t.Errorf("System, which can't be opened, has a start: %+v", system.Attrs)
	}
	if _, ok := m.world.edges[sdk.EdgeKey{From: winProcessRef("700"), To: winProcessRef("2900"), Rel: sdk.RelParentOf}]; !ok {
		t.Error("services.exe is not spoolsv.exe's parent")
	}
	// 0.1 s of CPU in 2 s on 4 CPUs
	if cpu := latest(t, m, winProcessRef("5200"), MetricCPU); cpu != 1.25 {
		t.Errorf("explorer CPU %v%%", cpu)
	}
	if rss := latest(t, m, winProcessRef("5200"), MetricRSS); rss != 200<<20 {
		t.Errorf("explorer memory %v", rss)
	}
	ss, _ := m.QuerySeries(context.Background(), sdk.SeriesQuery{
		Entities: []sdk.EntityRef{winProcessRef("4")}, Metrics: []string{MetricRSS},
		Window: sdk.TimeWindow{From: time.Unix(0, 0), To: time.Unix(200000, 0)},
	})
	if len(ss) == 1 && len(ss[0].Points) > 0 {
		t.Errorf("System has memory points %+v", ss[0].Points)
	}
}

func TestWindowsProcessesWithoutCommands(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s\ncommands: false")
	pollWindows(t, w, m, 1)
	if c, ok := m.world.ents[winProcessRef("5200")].Attrs["command"]; ok {
		t.Errorf("explorer's command %v, with commands off", c)
	}
}

func TestWindowsServicesOff(t *testing.T) {
	w := loadRecordedWindows(t)
	m := windowsModule(t, w, "interval: 2s\nunits: []\nprocesses: false")
	pollWindows(t, w, m, 1)
	for _, e := range m.world.ents {
		if e.Kind == sdk.KindService || e.Kind == sdk.KindProcess {
			t.Errorf("%s listed with units and processes off", e.Ref)
		}
	}
}
