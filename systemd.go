package localhost

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"wayseer.dev/sdk"

	"github.com/godbus/dbus/v5"
)

// KindUnit is a systemd unit other than a service: a timer, socket, target, path, mount...
const KindUnit sdk.Kind = "localhost/unit"

// unitRetry is how long to wait before reconnecting to systemd after a failure.
const unitRetry = 30 * time.Second

var errNoSystemd = errors.New("systemd not available")

// unitSystem is a service manager whose units are listed: systemd, or Windows' service control
// manager; tests replay recordings.
type unitSystem interface {
	connect(ctx context.Context) (unitSource, error)
	name() string // as notes name it
}

// system is the running machine's systemd and journal.
type system interface {
	unitSystem
	// journal runs journalctl with args; the stream ends with ctx, and Close reports why.
	journal(ctx context.Context, args []string) (io.ReadCloser, error)
}

// unitSource is a connection to systemd's manager.
type unitSource interface {
	units(ctx context.Context) ([]unitReply, error)
	details(ctx context.Context, u unitReply) (unitProps, error)
	close()
}

// unitReply is one entry of the manager's ListUnits reply, a(ssssssouso).
type unitReply struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	Path                                                           dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath

	// Windows only, so not in the D-Bus reply.
	pid   int  // the service's process
	ended bool // stopped cleanly, which Windows can't tell from a stop asked for
}

// unitDeps are the units a unit needs (hard) or merely wants (soft).
type unitDeps struct{ hard, soft []string }

// depsFrom reads the dependency properties of org.freedesktop.systemd1.Unit.
func depsFrom(prop func(name string) []string) unitDeps {
	empty := func(s string) bool { return s == "" }
	return unitDeps{
		hard: slices.DeleteFunc(slices.Concat(prop("Requires"), prop("Requisite"), prop("BindsTo")), empty),
		soft: slices.DeleteFunc(slices.Clone(prop("Wants")), empty),
	}
}

// unitProps are what localhost reads of a unit besides its listing.
type unitProps struct {
	deps        unitDeps
	enabled     bool      // started at boot
	fileState   string    // UnitFileState: enabled, static, disabled, transient...; a Windows start type
	svcType     string    // a service's Type: simple, oneshot...
	remain      bool      // a service's RemainAfterExit
	result      string    // a service's Result: success, signal, exit-code...
	triggeredBy []string  // the sockets, timers and paths that start it
	activeSince time.Time // ActiveEnterTimestamp; zero if it never became active
}

// propsFrom reads the properties of org.freedesktop.systemd1.Unit and .Service, as godbus
// decodes them; missing ones are left zero.
func propsFrom(get func(name string) any) unitProps {
	strs := func(name string) []string { ss, _ := get(name).([]string); return ss }
	str := func(name string) string { s, _ := get(name).(string); return s }
	p := unitProps{
		deps: depsFrom(strs), fileState: str("UnitFileState"), svcType: str("Type"), result: str("Result"),
		triggeredBy: strs("TriggeredBy"),
	}
	p.enabled = p.fileState == "enabled" || p.fileState == "enabled-runtime"
	p.remain, _ = get("RemainAfterExit").(bool)
	if us, _ := get("ActiveEnterTimestamp").(uint64); us > 0 {
		p.activeSince = time.UnixMicro(int64(us))
	}
	return p
}

type unit struct {
	unitReply
	props unitProps
	// Why a unit that is not running stopped, decided when it did: stopAsked when the stop was
	// meant, requested when it was asked for rather than the unit ending in order by itself.
	stopAsked, requested, wanted bool
}

// stopNote is when a unit's stop was seen, and whether it was asked for.
type stopNote struct {
	at        time.Time
	requested bool
}

// cachedProps are a unit's properties as read while it was in one active state.
type cachedProps struct {
	active string
	props  unitProps
}

// unitWatcher lists units, keeping the last good listing across failures and units
// that stopped for keep after they did.
type unitWatcher struct {
	sys     unitSystem // nil when units are not read
	types   []string
	keep    time.Duration
	src     unitSource
	props   map[string]cachedProps
	seen    map[string]*seenUnit // units listed this run
	stops   map[string]stopNote  // when a unit's stop was seen, until it starts again
	changes []unitChange         // since last taken
	last    []unit
	listed  bool // a listing has been read, so units new to the next one have started
	note    string
	retry   time.Time
	absent  bool // no systemd on this machine; not retried
}

// seenUnit is a unit as last listed, and when it stopped if it has.
type seenUnit struct {
	unit
	stopped time.Time
}

// unitChange is a listed unit moving from one active state to another.
type unitChange struct {
	name, from, to string
	at             time.Time
	unit           unit // as it was after the change
}

func newUnitWatcher(sys unitSystem, types []string, keep time.Duration) unitWatcher {
	if len(types) == 0 {
		sys = nil
	}
	return unitWatcher{
		sys: sys, types: types, keep: keep,
		props: map[string]cachedProps{}, seen: map[string]*seenUnit{}, stops: map[string]stopNote{},
	}
}

// read lists the wanted units, connecting first if need be.
func (w *unitWatcher) read(ctx context.Context, now time.Time) []unit {
	if w.sys == nil || w.absent || !w.connected(ctx, now) {
		return w.last
	}
	listed, err := w.src.units(ctx)
	if err != nil {
		w.fail(err, now)
		return w.last
	}
	var out []unit
	for _, u := range listed {
		switch {
		case u.JobType == "stop":
			w.stopSeen(u.Name, now, true)
		case u.ended:
			w.stopSeen(u.Name, now, false)
		}
		if !w.wanted(u) {
			continue
		}
		p, err := w.details(ctx, u)
		if err != nil {
			w.fail(err, now)
			return w.last
		}
		out = append(out, unit{unitReply: u, props: p})
	}
	out = w.remember(judge(out, w.stops), now)
	maps.DeleteFunc(w.props, func(name string, _ cachedProps) bool {
		return !slices.ContainsFunc(out, func(u unit) bool { return u.Name == name })
	})
	maps.DeleteFunc(w.stops, func(name string, _ stopNote) bool { return w.seen[name] == nil })
	w.last, w.note, w.listed = out, "", true
	return out
}

func (w *unitWatcher) connected(ctx context.Context, now time.Time) bool {
	if w.src != nil {
		return true
	}
	if now.Before(w.retry) {
		return false
	}
	src, err := w.sys.connect(ctx)
	switch {
	case errors.Is(err, errNoSystemd):
		w.absent, w.note, w.last = true, errNoSystemd.Error(), nil
		return false
	case err != nil:
		w.fail(err, now)
		return false
	}
	w.src = src
	return true
}

// fail drops the connection so the next read after unitRetry reconnects.
func (w *unitWatcher) fail(err error, now time.Time) {
	w.note = w.sys.name() + ": " + err.Error()
	w.retry = now.Add(unitRetry)
	w.close()
}

func (w *unitWatcher) close() {
	if w.src != nil {
		w.src.close()
		w.src = nil
	}
}

// remember notes each listed unit's state changes, a unit new to a later listing having started
// from inactive, and adds the units that have stopped since they were listed, until keep has
// passed.
func (w *unitWatcher) remember(listed []unit, now time.Time) []unit {
	for _, u := range listed {
		if s, ok := w.seen[u.Name]; ok {
			w.changed(u, s.ActiveState, now)
		} else if w.listed {
			w.changed(u, "inactive", now)
		}
		w.seen[u.Name] = &seenUnit{unit: u}
	}
	names := slices.Sorted(maps.Keys(w.seen))
	for _, name := range names {
		s := w.seen[name]
		if slices.ContainsFunc(listed, func(u unit) bool { return u.Name == name }) {
			continue
		}
		if s.stopped.IsZero() {
			from := s.ActiveState
			s.stopped, s.ActiveState, s.SubState = now, "inactive", "dead"
			s.unit = judged(s.unit, listed, w.stops)
			w.changed(s.unit, from, now)
		}
		if now.Sub(s.stopped) >= w.keep {
			delete(w.seen, name)
			continue
		}
		listed = append(listed, s.unit)
	}
	return listed
}

func (w *unitWatcher) changed(u unit, from string, now time.Time) {
	if from == u.ActiveState {
		return
	}
	w.changes = append(w.changes, unitChange{u.Name, from, u.ActiveState, now, u})
	if u.ActiveState == "active" {
		delete(w.stops, u.Name)
	}
}

// takeChanges returns the state changes seen since it was last called.
func (w *unitWatcher) takeChanges() []unitChange {
	c := w.changes
	w.changes = nil
	return c
}

// wanted keeps loaded units of the configured types that are not simply stopped.
func (w *unitWatcher) wanted(u unitReply) bool {
	_, typ := unitKind(u.Name)
	return u.LoadState == "loaded" && u.ActiveState != "inactive" && slices.Contains(w.types, typ)
}

// details are reread when a unit changes state, as a restart may follow a reload.
func (w *unitWatcher) details(ctx context.Context, u unitReply) (unitProps, error) {
	if c, ok := w.props[u.Name]; ok && c.active == u.ActiveState {
		return c.props, nil
	}
	p, err := w.src.details(ctx, u)
	if err != nil {
		return unitProps{}, err
	}
	w.props[u.Name] = cachedProps{u.ActiveState, p}
	return p, nil
}

// jobLogged notes the service manager's journal line about a job: a stop asked for, or a start
// that ends one.
func (w *unitWatcher) jobLogged(e journalEntry) {
	switch {
	case e.object == "":
	case e.jobType == "stop":
		w.stopSeen(e.object, e.at, true)
	case e.jobType == "start":
		delete(w.stops, e.object)
	}
}

// stopSeen keeps when a stop was first seen, and whether any sighting was a request.
func (w *unitWatcher) stopSeen(name string, at time.Time, requested bool) {
	n, ok := w.stops[name]
	if !ok {
		n.at = at
	}
	n.requested = n.requested || requested
	w.stops[name] = n
}

// unitKind is a service for .service units and KindUnit for the rest, with the unit type.
func unitKind(name string) (sdk.Kind, string) {
	typ := name[strings.LastIndexByte(name, '.')+1:]
	if typ == "service" {
		return sdk.KindService, typ
	}
	return KindUnit, typ
}

// unitStatus maps systemd's active state to a status.
func unitStatus(active string) sdk.Status {
	switch active {
	case "active":
		return okStatus
	case "failed":
		return sdk.Status{Level: sdk.StatusCrit, Reason: "failed"}
	case "activating":
		return sdk.Status{Level: sdk.StatusWarn, Reason: "starting"}
	case "deactivating":
		return sdk.Status{Level: sdk.StatusWarn, Reason: "stopping"}
	case "reloading", "refreshing":
		return sdk.Status{Level: sdk.StatusWarn, Reason: active}
	case "maintenance":
		return sdk.Status{Level: sdk.StatusWarn, Reason: "in maintenance"}
	case "inactive":
		return sdk.Status{Level: sdk.StatusUnknown, Reason: "stopped"}
	}
	return sdk.Status{Level: sdk.StatusUnknown}
}

// parseCgroup returns a process's systemd cgroup path from /proc/<pid>/cgroup: the
// name=systemd hierarchy under cgroup v1, else the unified one.
func parseCgroup(b []byte) string {
	unified := ""
	for line := range lines(b) {
		_, rest, _ := strings.Cut(line, ":")
		ctrl, path, ok := strings.Cut(rest, ":")
		switch {
		case !ok:
		case ctrl == "name=systemd":
			return path
		case ctrl == "":
			unified = path
		}
	}
	return unified
}
