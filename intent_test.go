package localhost

import (
	"context"
	"encoding/json"
	"fmt"
	"mindseye/pkg/sdk"
	"slices"
	"testing"
	"time"
)

// stopRecording is one way a unit ended, from testdata/systemd/stops.json.
type stopRecording struct {
	Before      map[string]busJSON
	During      []any // the listing while it ended, or null
	After       map[string]busJSON
	ListedAfter []any `json:"listed_after"` // null once systemd unloaded it
	Journal     []json.RawMessage
}

func recordings(t *testing.T) map[string]stopRecording {
	t.Helper()
	var recs map[string]stopRecording
	readJSON(t, "../../testdata/systemd/stops.json", &recs)
	return recs
}

// asSystem reads a user manager's journal line as the system manager writes it: UNIT for
// USER_UNIT, from pid 1.
func asSystem(t *testing.T, line json.RawMessage) journalEntry {
	t.Helper()
	var j map[string]any
	if err := json.Unmarshal(line, &j); err != nil {
		t.Fatal(err)
	}
	if u, ok := j["USER_UNIT"]; ok {
		j["UNIT"], j["_PID"] = u, "1"
		delete(j, "USER_UNIT")
	}
	if u, ok := j["_SYSTEMD_USER_UNIT"]; ok {
		j["_SYSTEMD_UNIT"] = u
		delete(j, "_SYSTEMD_USER_UNIT")
	}
	b, _ := json.Marshal(j)
	e, err := parseJournalEntry(b)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// endCase replays a recording with the unit's file state and surroundings chosen.
type endCase struct {
	rec       string
	fileState string // replaces the recorded "transient"; "" keeps it
	svcType   string // replaces the recorded Type; "" keeps it
	pulled    bool   // an active target wants the unit
	trigger   string // an active unit that starts it
	journal   bool   // the manager's journal lines are read
	during    bool   // a listing is taken while it ends
	status    sdk.Status
	message   string
}

func TestRecordedEndingsClassify(t *testing.T) {
	down := sdk.Status{Level: sdk.StatusDown, Reason: "should be running"}
	stopped := sdk.Status{Level: sdk.StatusUnknown, Reason: "stopped"}
	finished := sdk.Status{Level: sdk.StatusUnknown, Reason: "finished"}
	for name, c := range map[string]endCase{
		"systemctl stop, seen in the journal":          {rec: "stop", fileState: "enabled", journal: true, status: stopped, message: "stopped by request"},
		"systemctl stop, seen as a stop job":           {rec: "stop", fileState: "enabled", during: true, status: stopped, message: "stopped by request"},
		"killed while enabled":                         {rec: "kill", fileState: "enabled", journal: true, during: true, status: down, message: "exited unexpectedly"},
		"killed while static and wanted by a target":   {rec: "kill", fileState: "static", pulled: true, journal: true, status: down, message: "exited unexpectedly"},
		"killed while static and wanted by nothing":    {rec: "kill", fileState: "static", journal: true, status: stopped, message: "stopped"},
		"killed while disabled and wanted by a target": {rec: "kill", fileState: "disabled", pulled: true, journal: true, status: down, message: "exited unexpectedly"},
		"killed while enabled and started by a socket": {rec: "kill", fileState: "enabled", trigger: "mindseye-rec.socket", journal: true, status: down, message: "exited unexpectedly"},
		"killed while transient":                       {rec: "kill", journal: true, status: stopped, message: "stopped"},
		"crashed while enabled":                        {rec: "crash", fileState: "enabled", journal: true, status: down, message: "failed"},
		"crashed while transient":                      {rec: "crash", journal: true, status: sdk.Status{Level: sdk.StatusCrit, Reason: "failed"}, message: "failed"},
		"an enabled oneshot finished":                  {rec: "oneshot", fileState: "enabled", pulled: true, journal: true, status: finished, message: "finished"},
		"a timer's oneshot finished":                   {rec: "timer", fileState: "static", trigger: "mindseye-rec-timer.timer", journal: true, status: finished, message: "finished"},
		"a timer's long-running service ended":         {rec: "timer", fileState: "enabled", svcType: "simple", trigger: "mindseye-rec-timer.timer", journal: true, status: stopped, message: "stopped"},
	} {
		t.Run(name, func(t *testing.T) {
			m, unit, evs := replay(t, recordings(t)[c.rec], c)
			if got := m.world.ents[unitRef(unit)].Status; got != c.status {
				t.Errorf("status %+v, want %+v", got, c.status)
			}
			i := slices.IndexFunc(evs, func(e sdk.Event) bool { return e.Kind == "state" && e.Entity == unitRef(unit) })
			if i < 0 || evs[i].Message != c.message {
				t.Errorf("state events %+v, want %q", evs, c.message)
			}
		})
	}
}

// replay lists the recorded unit running, then as it ended, with the extra journal entries read
// before it did; it returns the events of the end.
func replay(t *testing.T, rec stopRecording, c endCase, extra ...journalEntry) (*Module, string, []sdk.Event) {
	t.Helper()
	unit := "mindseye-rec-" + c.rec + ".service"
	f := newFakeSystem(t)
	running := "active"
	if rec.Before == nil || rec.Before["ActiveState"].value(t) != "active" {
		running = "activating" // a oneshot never becomes active
	}
	f.list = append(f.list, unitReply{Name: unit, LoadState: "loaded", ActiveState: running, SubState: "start"})
	f.setProps(t, unit, chosen(t, cmpMap(rec.Before, rec.After), c))
	if c.trigger != "" {
		f.list = append(f.list, unitReply{Name: c.trigger, LoadState: "loaded", ActiveState: "active", SubState: "waiting"})
	}
	if c.pulled {
		f.props["multi-user.target"]["Wants"] = append(f.props["multi-user.target"]["Wants"].([]string), unit)
	}
	m := systemModule(t, f, "")
	t0 := asSystem(t, rec.Journal[0]).at
	m.poll(context.Background(), t0)
	if c.during {
		setListing(f, unit, rec.During)
		m.poll(context.Background(), t0.Add(time.Second))
	}
	if c.journal {
		for _, line := range rec.Journal {
			m.units.jobLogged(asSystem(t, line))
		}
	}
	for _, e := range extra {
		m.units.jobLogged(e)
	}
	setListing(f, unit, rec.ListedAfter)
	f.setProps(t, unit, chosen(t, rec.After, c))
	return m, unit, m.poll(context.Background(), t0.Add(3*time.Second)).Events
}

func cmpMap(a, b map[string]busJSON) map[string]busJSON {
	if a != nil {
		return a
	}
	return b
}

// chosen is the recorded properties with the case's file state and type.
func chosen(t *testing.T, props map[string]busJSON, c endCase) map[string]busJSON {
	t.Helper()
	out := map[string]busJSON{}
	for k, v := range props {
		out[k] = v
	}
	str := func(s string) busJSON { b, _ := json.Marshal(s); return busJSON{Type: "s", Data: b} }
	if c.fileState != "" {
		out["UnitFileState"] = str(c.fileState)
	}
	if c.svcType != "" {
		out["Type"] = str(c.svcType)
	}
	if c.trigger != "" {
		b, _ := json.Marshal([]string{c.trigger})
		out["TriggeredBy"] = busJSON{Type: "as", Data: b}
	}
	return out
}

// setListing sets the unit's ListUnits entry to a recorded one, or drops it when null.
func setListing(f *fakeSystem, unit string, entry []any) {
	if entry == nil {
		f.drop(unit)
		return
	}
	f.setState(unit, entry[3].(string), entry[4].(string))
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.list, func(u unitReply) bool { return u.Name == unit })
	f.list[i].JobType = entry[8].(string)
}

func TestAStopFromBeforeTheLastStartIsForgotten(t *testing.T) {
	rec := recordings(t)["stop"]
	stop := asSystem(t, rec.Journal[1])
	stop.at = stop.at.Add(-time.Hour)
	c := endCase{rec: "kill", fileState: "enabled"}
	m, unit := replayWith(t, recordings(t)["kill"], c, stop)
	if got := m.world.ents[unitRef(unit)].Status.Level; got != sdk.StatusDown {
		t.Errorf("an old stop excused a kill: %v", got)
	}
}

func TestAStartClearsAStopRequest(t *testing.T) {
	kill := recordings(t)["kill"]
	stop := asSystem(t, recordings(t)["stop"].Journal[1])
	start := asSystem(t, kill.Journal[0])
	stop.at = start.at.Add(100 * time.Millisecond) // after it started, so only the start can clear it
	start.at = stop.at.Add(100 * time.Millisecond)
	m, unit := replayWith(t, kill, endCase{rec: "kill", fileState: "enabled"}, stop, start)
	if got := m.world.ents[unitRef(unit)].Status.Level; got != sdk.StatusDown {
		t.Errorf("a stop then a start excused a kill: %v", got)
	}
}

// replayWith replays a recording with the given journal entries, about its unit, read before it ends.
func replayWith(t *testing.T, rec stopRecording, c endCase, entries ...journalEntry) (*Module, string) {
	t.Helper()
	for i := range entries {
		entries[i].object = "mindseye-rec-" + c.rec + ".service"
	}
	m, unit, _ := replay(t, rec, c, entries...)
	return m, unit
}

func TestJobLinesBelowThePriorityAreOnlySignals(t *testing.T) {
	rec := recordings(t)["stop"]
	var entries []journalEntry
	for _, line := range rec.Journal {
		entries = append(entries, asSystem(t, line))
	}
	quiet := systemModule(t, newFakeSystem(t), "")
	if cs := quiet.logged(entries); len(cs.Events) != 0 {
		t.Errorf("journal: warning sent info lines %+v", cs.Events)
	}
	if _, ok := quiet.units.stops["mindseye-rec-stop.service"]; !ok {
		t.Error("the stop was not noted")
	}
	f := newFakeSystem(t)
	f.list = append(f.list, unitReply{Name: "mindseye-rec-stop.service", LoadState: "loaded", ActiveState: "active"})
	loud := systemModule(t, f, "journal: info")
	loud.poll(context.Background(), entries[0].at)
	cs := loud.logged(entries)
	if len(cs.Events) != len(entries) || cs.Events[1].Entity != unitRef("mindseye-rec-stop.service") {
		t.Errorf("journal: info sent %+v, want the lines on the unit", cs.Events)
	}
}

func TestOnlyTheManagerNamesAUnit(t *testing.T) {
	line := `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","_PID":"%s","UNIT":"sshd.service","JOB_TYPE":"stop","MESSAGE":"Stopping"}`
	for pid, want := range map[string]string{"1": "sshd.service", "4242": ""} {
		e, err := parseJournalEntry([]byte(fmt.Sprintf(line, pid)))
		if err != nil {
			t.Fatal(err)
		}
		if e.object != want {
			t.Errorf("pid %s: unit %q, want %q", pid, e.object, want)
		}
	}
}
