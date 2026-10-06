package localhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wayseer.dev/sdk"
)

// Kind is the module kind in config.
const Kind = "localhost"

const version = "1"

func init() { sdk.Register(Kind, func() sdk.Module { return New() }) }

// Module reports the machine it runs on: hardware, storage, network and processes.
type Module struct {
	name     sdk.ModuleID
	opts     options
	pageSize uint64
	health   atomic.Pointer[sdk.Health]

	// Replaceable for tests; default to the running system.
	statfs       func(path string) (fsUsage, error)
	addrs        func() map[string][]string
	native       func(*reader) (source, []sdk.Metric)
	system       system
	journalRetry time.Duration // wait before restarting journalctl

	mu          sync.Mutex // guards what follows, shared by Run and queries
	reader      source
	metrics     []sdk.Metric // what reader can measure
	units       unitWatcher
	journal     system // nil when the journal is not read
	world       world
	last        *sample
	tracker     sdk.Tracker
	series      map[sdk.SeriesRef]*sdk.Ring
	events      *sdk.EventLog
	journalNote string
	running     bool
}

// New makes an unconfigured module.
func New() *Module {
	return &Module{statfs: statfs, addrs: interfaceAddrs, native: nativeSource, system: liveSystem{}, journalRetry: 30 * time.Second, metrics: catalogue}
}

// Info describes the module.
func (m *Module) Info() sdk.Info {
	return sdk.Info{Kind: Kind, Version: version, Description: "This machine: CPUs, memory, disks, filesystems, network interfaces and processes"}
}

// Configure decodes options; nothing is read until Run or Discover.
func (m *Module) Configure(_ context.Context, cfg sdk.Config) error {
	o := defaults()
	if err := cfg.Decode(&o); err != nil {
		return err
	}
	if err := o.validate(); err != nil {
		return fmt.Errorf("line %d: %w", cfg.Line, err)
	}
	if !supported && o.Root == "/" {
		return fmt.Errorf("line %d: %w", cfg.Line, errUnsupported)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.name, m.opts, m.pageSize = cfg.Name, o, uint64(os.Getpagesize())
	addrs, sys := m.addrs, m.system
	if _, live := sys.(liveSystem); live && o.Root != "/" {
		addrs, sys = nil, nil // another machine's tree: this one's addresses and services would be wrong
	}
	r := &reader{
		fsys: os.DirFS(o.Root), root: o.Root, statfs: m.statfs, addrs: addrs,
		procs: o.Processes, cmds: o.Commands, details: map[procKey]procDetail{},
	}
	m.reader, m.metrics = r, catalogue
	if o.Root == "/" {
		if n, metrics := m.native(r); n != nil {
			m.reader, m.metrics, sys = n, metrics, nil // not Linux: no procfs, systemd or journal
		}
	}
	m.units.close()
	m.units, m.journal = newUnitWatcher(sys, o.Units, o.KeepStopped), sys
	if o.priority < 0 {
		m.journal = nil
	}
	m.world, m.last = world{}, nil
	m.tracker.Reset()
	m.series = map[sdk.SeriesRef]*sdk.Ring{}
	m.events, m.journalNote = sdk.NewEventLog(eventCap), ""
	m.health.Store(&sdk.Health{})
	return nil
}

// Run sends a snapshot, then a delta of what changed at every interval and of journal
// entries as they come, until ctx ends.
func (m *Module) Run(ctx context.Context, sink sdk.Sink) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	m.tracker.Reset() // a snapshot resends everything
	m.running = true
	journal := m.journal
	m.mu.Unlock()
	defer m.stop()
	entries := make(chan journalEntry, entryBatch)
	if journal != nil {
		wg.Go(func() { m.followJournal(ctx, journal, entries) })
	}
	if err := sink.Snapshot(ctx, m.poll(ctx, time.Now())); err != nil {
		return err
	}
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		var cs *sdk.ChangeSet
		read := false // a good read is sent even when nothing changed, so the data stays fresh
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			cs = m.poll(ctx, now)
			read = m.Health().Err == nil
		case e := <-entries:
			cs = m.logged(drain(e, entries))
		}
		if read || !cs.Empty() {
			if err := sink.Delta(ctx, cs); err != nil {
				return err
			}
		}
	}
}

func (m *Module) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	m.units.close()
}

// poll samples the machine and its units, records series, and returns what changed since the
// last send. A failed read keeps the last world and shows in Health.
func (m *Module) poll(ctx context.Context, now time.Time) *sdk.ChangeSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.reader.read(now)
	if err == nil {
		s.units = m.units.read(ctx, now)
		m.world = m.buildWorld(s)
		m.record(&m.world, s, m.last)
		m.last = s
	}
	m.health.Store(&sdk.Health{Err: err, Note: m.note()})
	cs := m.tracker.Changes(m.world.ents, m.world.edges, now)
	cs.Events = m.stateEvents(m.units.takeChanges())
	m.events.Add(cs.Events...)
	return cs
}

// note joins what limits the view without being an error: systemd or the journal missing.
func (m *Module) note() string {
	var notes []string
	for _, n := range []string{m.units.note, m.journalNote} {
		if n != "" {
			notes = append(notes, n)
		}
	}
	return strings.Join(notes, "; ")
}

// Health reports whether the machine could be read.
func (m *Module) Health() sdk.Health {
	if h := m.health.Load(); h != nil {
		return *h
	}
	return sdk.Health{}
}

// Discover returns the machine as last read, reading it first if Run has not.
func (m *Module) Discover(ctx context.Context) (*sdk.ChangeSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		s, err := m.reader.read(time.Now())
		if err != nil {
			return nil, err
		}
		s.units = m.units.read(ctx, time.Now())
		if !m.running {
			m.units.close()
		}
		m.world = m.buildWorld(s)
	}
	var probe sdk.Tracker
	return probe.Changes(m.world.ents, m.world.edges, time.Now()), nil
}

// Metrics lists the series the module records.
func (m *Module) Metrics() []sdk.Metric { return slices.Clone(m.metrics) }

// QuerySeries answers from the recorded history, thinned to about one point per step; a metric
// of the entity's kind with no samples yet is an empty series, as the entity has it.
func (m *Module) QuerySeries(ctx context.Context, q sdk.SeriesQuery) ([]sdk.Series, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sdk.Series
	for _, e := range m.matching(q) {
		for _, name := range q.Metrics {
			ref := sdk.SeriesRef{Entity: e.Ref, Metric: name}
			switch h := m.series[ref]; {
			case h != nil:
				out = append(out, sdk.Series{Ref: ref, Unit: unitOf(m.metrics, name), Points: thin(h.In(q.Window), q)})
			case applies(m.metrics, name, e.Kind):
				out = append(out, sdk.Series{Ref: ref, Unit: unitOf(m.metrics, name)})
			}
		}
	}
	return out, nil
}

func (m *Module) matching(q sdk.SeriesQuery) []sdk.Entity {
	var out []sdk.Entity
	if len(q.Entities) > 0 {
		for _, r := range q.Entities {
			if e, ok := m.world.ents[r]; ok {
				out = append(out, e)
			}
		}
		return out
	}
	for _, r := range m.sortedRefs() {
		if e := m.world.ents[r]; q.Filter.Match(&e) {
			out = append(out, e)
		}
	}
	return out
}

func (m *Module) sortedRefs() []sdk.EntityRef {
	refs := make([]sdk.EntityRef, 0, len(m.world.ents))
	for r := range m.world.ents {
		refs = append(refs, r)
	}
	slices.Sort(refs)
	return refs
}

func thin(ps []sdk.Point, q sdk.SeriesQuery) []sdk.Point {
	if q.Step <= 0 {
		return ps
	}
	return sdk.Downsample(ps, q.Window, int(q.Window.Span()/q.Step))
}

// Search finds entities whose name or id contains text, ignoring case: a process by name or
// pid, a filesystem by mount point.
func (m *Module) Search(ctx context.Context, text string, limit int) ([]sdk.EntityRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("search limit must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	text = strings.ToLower(text)
	var out []sdk.EntityRef
	for _, r := range m.sortedRefs() {
		e := m.world.ents[r]
		if len(out) < limit && (strings.Contains(strings.ToLower(e.Name), text) || strings.Contains(strings.ToLower(r.Native()), text)) {
			out = append(out, r)
		}
	}
	return out, nil
}
