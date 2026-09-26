package localhost

import (
	"context"
	"mindseye/pkg/sdk"
	"slices"
	"testing"
	"time"
)

var sshd = unitRef("sshd.service")

// pollAt polls at Unix second sec and returns the events sent with the changes.
func pollAt(m *Module, sec int64) []sdk.Event {
	return m.poll(context.Background(), time.Unix(sec, 0)).Events
}

func (f *fakeSystem) drop(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = slices.DeleteFunc(f.list, func(u unitReply) bool { return u.Name == name })
}

// askStop notes systemd's journal line for a stop of sshd, as `systemctl stop` writes it.
func askStop(m *Module, sec int64) {
	m.units.jobLogged(journalEntry{object: "sshd.service", jobType: "stop", at: time.Unix(sec, 0)})
}

func wantStateEvent(t *testing.T, evs []sdk.Event, msg, from, to string, sev sdk.Severity) {
	t.Helper()
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want one %q", evs, msg)
	}
	e := evs[0]
	if e.Entity != sshd || e.Kind != "state" || e.Message != msg || e.Severity != sev || e.Source != "local" ||
		e.Fields["from"].Str() != from || e.Fields["to"].Str() != to || e.ID == "" {
		t.Errorf("event = %+v; want %q from %s to %s", e, msg, from, to)
	}
}

func TestStoppedUnitStaysListedAsStopped(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	if evs := pollAt(m, 1000); len(evs) != 0 {
		t.Errorf("the first listing sent events %+v", evs)
	}
	askStop(m, 1001)
	f.setState("sshd.service", "inactive", "dead")
	wantStateEvent(t, pollAt(m, 1002), "stopped by request", "active", "inactive", sdk.SevInfo)
	e, ok := m.world.ents[sshd]
	if !ok {
		t.Fatal("a stopped unit left the world")
	}
	if e.Status != (sdk.Status{Level: sdk.StatusUnknown, Reason: "stopped"}) || e.Attrs["state"].Str() != "inactive" {
		t.Errorf("stopped sshd = %+v", e)
	}
	if _, ok := m.world.edges[sdk.EdgeKey{From: unitRef("multi-user.target"), To: sshd, Rel: sdk.RelDependsOn}]; !ok {
		t.Error("a stopped unit lost its dependencies")
	}
	if evs, _ := m.QueryEvents(context.Background(), sdk.EventQuery{Entities: []sdk.EntityRef{sshd}}); len(evs) != 1 {
		t.Errorf("queried events = %+v, want the stop", evs)
	}
}

func TestUnitGoneFromTheListingIsKeptAsStopped(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	pollAt(m, 1000)
	askStop(m, 1001)
	f.drop("sshd.service")
	wantStateEvent(t, pollAt(m, 1002), "stopped by request", "active", "inactive", sdk.SevInfo)
	if e := m.world.ents[sshd]; e.Status.Reason != "stopped" {
		t.Errorf("sshd = %+v, want kept as stopped", e)
	}
}

func TestStoppedUnitIsDroppedAfterKeepStopped(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "keep_stopped: 10s")
	pollAt(m, 1000)
	askStop(m, 1001)
	f.setState("sshd.service", "inactive", "dead")
	pollAt(m, 1002)
	pollAt(m, 1011)
	if _, ok := m.world.ents[sshd]; !ok {
		t.Error("dropped inside keep_stopped")
	}
	if evs := pollAt(m, 1012); len(evs) != 0 {
		t.Errorf("dropping sent events %+v", evs)
	}
	if _, ok := m.world.ents[sshd]; ok {
		t.Error("still listed after keep_stopped")
	}
}

func TestKeepStoppedZeroKeepsNone(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "keep_stopped: 0s")
	pollAt(m, 1000)
	askStop(m, 1001)
	f.setState("sshd.service", "inactive", "dead")
	wantStateEvent(t, pollAt(m, 1002), "stopped by request", "active", "inactive", sdk.SevInfo)
	if _, ok := m.world.ents[sshd]; ok {
		t.Error("a stopped unit listed with keep_stopped: 0s")
	}
}

func TestRestartedUnitIsOneEntityWithItsHistory(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	pollAt(m, 1000)
	askStop(m, 1001)
	f.setState("sshd.service", "inactive", "dead")
	pollAt(m, 1002)
	f.setState("sshd.service", "active", "running")
	wantStateEvent(t, pollAt(m, 1004), "started", "inactive", "active", sdk.SevInfo)
	if e := m.world.ents[sshd]; e.Status != okStatus {
		t.Errorf("restarted sshd = %+v", e.Status)
	}
	f.setState("sshd.service", "failed", "failed")
	wantStateEvent(t, pollAt(m, 1006), "failed", "active", "failed", sdk.SevError)
	evs, _ := m.QueryEvents(context.Background(), sdk.EventQuery{Entities: []sdk.EntityRef{sshd}})
	if len(evs) != 3 {
		t.Errorf("history = %+v, want stopped, started, failed", evs)
	}
	if n := pollAt(m, 1008); len(n) != 0 {
		t.Errorf("an unchanged state sent events %+v", n)
	}
}

func TestJournalEntriesAttachToAKeptUnit(t *testing.T) {
	f := newFakeSystem(t)
	m := systemModule(t, f, "")
	pollAt(m, 1000)
	askStop(m, 1001)
	f.setState("sshd.service", "inactive", "dead")
	pollAt(m, 1002)
	cs := m.logged([]journalEntry{{cursor: "c1", unit: "sshd.service", message: "Received signal 15; terminating."}})
	if got := cs.Events[0].Entity; got != sshd {
		t.Errorf("entry attached to %s, want the stopped unit", got)
	}
}
