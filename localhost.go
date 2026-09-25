package localhost

import (
	"context"
	"errors"
	"fmt"
	"mindseye/internal/data"
	"mindseye/internal/model"
	"mindseye/internal/module"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind is the module kind in config.
const Kind = "localhost"

const version = "1"

func init() { module.Register(Kind, func() module.Module { return New() }) }

// Module reports the machine it runs on: hardware, storage, network and processes.
type Module struct {
	name     model.ModuleID
	opts     options
	pageSize uint64
	health   atomic.Pointer[data.Health]

	// Replaceable for tests; default to the running system.
	statfs func(path string) (fsUsage, error)
	addrs  func() map[string][]string

	mu      sync.Mutex // guards what follows, shared by Run and queries
	reader  reader
	world   world
	last    *sample
	tracker module.Tracker
	series  map[data.SeriesRef]*data.Ring
}

// New makes an unconfigured module.
func New() *Module { return &Module{statfs: statfs, addrs: interfaceAddrs} }

// Info describes the module.
func (m *Module) Info() module.Info {
	return module.Info{Kind: Kind, Version: version, Description: "This machine: CPUs, memory, disks, filesystems, network interfaces and processes"}
}

// Configure decodes options; nothing is read until Run or Discover.
func (m *Module) Configure(_ context.Context, cfg module.Config) error {
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
	addrs := m.addrs
	if o.Root != "/" {
		addrs = nil // another machine's tree: this one's addresses would be wrong
	}
	m.reader = reader{
		fsys: os.DirFS(o.Root), root: o.Root, statfs: m.statfs, addrs: addrs,
		procs: o.Processes, cmds: o.Commands, details: map[procKey]procDetail{},
	}
	m.world, m.last = world{}, nil
	m.tracker.Reset()
	m.series = map[data.SeriesRef]*data.Ring{}
	m.health.Store(&data.Health{})
	return nil
}

// Run sends a snapshot, then a delta of what changed at every interval, until ctx ends.
func (m *Module) Run(ctx context.Context, sink module.Sink) error {
	m.mu.Lock()
	m.tracker.Reset() // a snapshot resends everything
	m.mu.Unlock()
	if err := sink.Snapshot(ctx, m.poll(time.Now())); err != nil {
		return err
	}
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			if cs := m.poll(now); !cs.Empty() {
				if err := sink.Delta(ctx, cs); err != nil {
					return err
				}
			}
		}
	}
}

// poll samples the machine, records series, and returns what changed since the last send. A
// failed read keeps the last world and shows in Health.
func (m *Module) poll(now time.Time) *model.ChangeSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.reader.read(now)
	m.health.Store(&data.Health{Err: err})
	if err == nil {
		m.world = m.buildWorld(s)
		m.record(&m.world, s, m.last)
		m.last = s
	}
	return m.tracker.Changes(m.world.ents, m.world.edges, now)
}

// Health reports whether the machine could be read.
func (m *Module) Health() data.Health {
	if h := m.health.Load(); h != nil {
		return *h
	}
	return data.Health{}
}

// Discover returns the machine as last read, reading it first if Run has not.
func (m *Module) Discover(ctx context.Context) (*model.ChangeSet, error) {
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
		m.world = m.buildWorld(s)
	}
	var probe module.Tracker
	return probe.Changes(m.world.ents, m.world.edges, time.Now()), nil
}

// Metrics lists the series the module records.
func (m *Module) Metrics() []module.Metric { return slices.Clone(catalogue) }

// QuerySeries answers from the recorded history, thinned to about one point per step.
func (m *Module) QuerySeries(ctx context.Context, q data.SeriesQuery) ([]data.Series, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []data.Series
	for _, e := range m.matching(q) {
		for _, name := range q.Metrics {
			ref := data.SeriesRef{Entity: e.Ref, Metric: name}
			if h := m.series[ref]; h != nil {
				out = append(out, data.Series{Ref: ref, Unit: unitOf(name), Points: thin(h.In(q.Window), q)})
			}
		}
	}
	return out, nil
}

func (m *Module) matching(q data.SeriesQuery) []model.Entity {
	var out []model.Entity
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

func (m *Module) sortedRefs() []model.EntityRef {
	refs := make([]model.EntityRef, 0, len(m.world.ents))
	for r := range m.world.ents {
		refs = append(refs, r)
	}
	slices.Sort(refs)
	return refs
}

func thin(ps []data.Point, q data.SeriesQuery) []data.Point {
	if q.Step <= 0 {
		return ps
	}
	return data.Downsample(ps, q.Window, int(q.Window.Span()/q.Step))
}

// Search finds entities whose name or id contains text, ignoring case: a process by name or
// pid, a filesystem by mount point.
func (m *Module) Search(ctx context.Context, text string, limit int) ([]model.EntityRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("search limit must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	text = strings.ToLower(text)
	var out []model.EntityRef
	for _, r := range m.sortedRefs() {
		e := m.world.ents[r]
		if len(out) < limit && (strings.Contains(strings.ToLower(e.Name), text) || strings.Contains(strings.ToLower(r.Native()), text)) {
			out = append(out, r)
		}
	}
	return out, nil
}
