package localhost

import (
	"context"
	"errors"
	"io"
	"maps"
	"mindseye/internal/model"
	"slices"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// KindUnit is a systemd unit other than a service: a timer, socket, target, path, mount...
const KindUnit model.Kind = "localhost/unit"

// unitRetry is how long to wait before reconnecting to systemd after a failure.
const unitRetry = 30 * time.Second

var errNoSystemd = errors.New("systemd not available")

// system is the running machine's service manager and journal; tests replay recordings.
type system interface {
	connect(ctx context.Context) (unitSource, error)
	// journal runs journalctl with args; the stream ends with ctx, and Close reports why.
	journal(ctx context.Context, args []string) (io.ReadCloser, error)
}

// unitSource is a connection to systemd's manager.
type unitSource interface {
	units(ctx context.Context) ([]unitReply, error)
	dependencies(ctx context.Context, u unitReply) (unitDeps, error)
	close()
}

// unitReply is one entry of the manager's ListUnits reply, a(ssssssouso).
type unitReply struct {
	Name, Description, LoadState, ActiveState, SubState, Following string
	Path                                                           dbus.ObjectPath
	JobID                                                          uint32
	JobType                                                        string
	JobPath                                                        dbus.ObjectPath
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

type unit struct {
	unitReply
	deps unitDeps
}

// cachedDeps are a unit's dependencies as read while it was in one active state.
type cachedDeps struct {
	active string
	deps   unitDeps
}

// unitWatcher lists systemd units, keeping the last good listing across failures.
type unitWatcher struct {
	sys    system // nil when units are not read
	types  []string
	src    unitSource
	deps   map[string]cachedDeps
	last   []unit
	note   string
	retry  time.Time
	absent bool // no systemd on this machine; not retried
}

func newUnitWatcher(sys system, types []string) unitWatcher {
	if len(types) == 0 {
		sys = nil
	}
	return unitWatcher{sys: sys, types: types, deps: map[string]cachedDeps{}}
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
		if !w.wanted(u) {
			continue
		}
		d, err := w.dependencies(ctx, u)
		if err != nil {
			w.fail(err, now)
			return w.last
		}
		out = append(out, unit{u, d})
	}
	maps.DeleteFunc(w.deps, func(name string, _ cachedDeps) bool {
		return !slices.ContainsFunc(out, func(u unit) bool { return u.Name == name })
	})
	w.last, w.note = out, ""
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
	w.note = "systemd: " + err.Error()
	w.retry = now.Add(unitRetry)
	w.close()
}

func (w *unitWatcher) close() {
	if w.src != nil {
		w.src.close()
		w.src = nil
	}
}

// wanted keeps loaded units of the configured types that are not simply stopped.
func (w *unitWatcher) wanted(u unitReply) bool {
	_, typ := unitKind(u.Name)
	return u.LoadState == "loaded" && u.ActiveState != "inactive" && slices.Contains(w.types, typ)
}

// dependencies are reread when a unit changes state, as a restart may follow a reload.
func (w *unitWatcher) dependencies(ctx context.Context, u unitReply) (unitDeps, error) {
	if c, ok := w.deps[u.Name]; ok && c.active == u.ActiveState {
		return c.deps, nil
	}
	d, err := w.src.dependencies(ctx, u)
	if err != nil {
		return unitDeps{}, err
	}
	w.deps[u.Name] = cachedDeps{u.ActiveState, d}
	return d, nil
}

// unitKind is a service for .service units and KindUnit for the rest, with the unit type.
func unitKind(name string) (model.Kind, string) {
	typ := name[strings.LastIndexByte(name, '.')+1:]
	if typ == "service" {
		return model.KindService, typ
	}
	return KindUnit, typ
}

// unitStatus maps systemd's active state to a status.
func unitStatus(active string) model.Status {
	switch active {
	case "active":
		return okStatus
	case "failed":
		return model.Status{Level: model.StatusCrit, Reason: "failed"}
	case "activating":
		return model.Status{Level: model.StatusWarn, Reason: "starting"}
	case "deactivating":
		return model.Status{Level: model.StatusWarn, Reason: "stopping"}
	case "reloading", "refreshing":
		return model.Status{Level: model.StatusWarn, Reason: active}
	case "maintenance":
		return model.Status{Level: model.StatusWarn, Reason: "in maintenance"}
	}
	return model.Status{Level: model.StatusUnknown}
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
