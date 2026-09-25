package localhost

import (
	"maps"
	"mindseye/internal/model"
	"strconv"
	"strings"
	"time"
)

// Entity kinds the module adds to the core vocabulary.
const (
	KindCPU        model.Kind = "localhost/cpu"
	KindMemory     model.Kind = "localhost/memory"
	KindFilesystem model.Kind = "localhost/filesystem"
)

// clockTick is USER_HZ, which Linux fixes at 100 on every architecture it exports times for.
const clockTick = 100

// Thresholds for warning and critical status.
const (
	fsWarn, fsCrit   = 0.90, 0.95 // share of a filesystem used
	memWarn, memCrit = 0.10, 0.05 // share of memory still available
)

// world is the entities and edges one sample describes.
type world struct {
	ents  map[model.EntityRef]model.Entity
	edges map[model.EdgeKey]model.Edge
	host  model.EntityRef
}

// builder turns samples into entities owned by one module instance.
type builder struct {
	src      model.ModuleID
	pageSize uint64
	w        world
}

func (m *Module) buildWorld(s *sample) world {
	b := builder{src: m.name, pageSize: m.pageSize, w: world{ents: map[model.EntityRef]model.Entity{}, edges: map[model.EdgeKey]model.Edge{}}}
	b.w.host = b.hostEntity(s)
	for i, c := range s.stat.cpus {
		r := b.add(KindCPU, c.name, c.name, okStatus, map[string]model.Value{"index": num(i)})
		b.link(r, b.w.host, model.RelMemberOf)
	}
	b.link(b.memory(s.mem), b.w.host, model.RelMemberOf)
	for _, d := range s.disks {
		r := b.add(model.KindDisk, d.name, d.name, okStatus, pruned(map[string]model.Value{
			"size": num(d.size), "model": model.String(d.model),
			"rotational": model.Bool(d.rotational), "removable": model.Bool(d.removable),
		}))
		b.link(r, b.w.host, model.RelMemberOf)
	}
	for _, f := range s.fss {
		b.filesystem(f)
	}
	for _, n := range s.nics {
		r := b.add(model.KindInterface, n.name, n.name, nicStatus(n.state), pruned(map[string]model.Value{
			"mac": model.String(n.mac), "mtu": num(n.mtu), "address": stringList(n.addrs),
		}))
		b.link(r, b.w.host, model.RelMemberOf)
	}
	b.units(s.units)
	b.processes(s)
	return b.w
}

// units adds each unit running on the host, and dependencies between listed units: a soft
// dependency (Wants) weighs half.
func (b *builder) units(us []unit) {
	for _, u := range us {
		kind, typ := unitKind(u.Name)
		name := strings.TrimSuffix(u.Name, ".service")
		r := b.add(kind, u.Name, name, unitStatus(u.ActiveState), pruned(map[string]model.Value{
			"unit": model.String(u.Name), "type": model.String(typ), "description": model.String(u.Description),
			"state": model.String(u.ActiveState), "sub_state": model.String(u.SubState),
		}))
		b.link(r, b.w.host, model.RelRunsOn)
	}
	for _, u := range us {
		from := b.unitRef(u.Name)
		for _, d := range u.deps.hard {
			b.linkListed(from, b.unitRef(d), model.RelDependsOn, 1)
		}
		for _, d := range u.deps.soft {
			b.linkListed(from, b.unitRef(d), model.RelDependsOn, 0.5)
		}
	}
}

func (b *builder) unitRef(name string) model.EntityRef {
	kind, _ := unitKind(name)
	return b.ref(kind, name)
}

// unitOfCgroup is the innermost listed unit in a cgroup path, so a user's session processes
// belong to user@UID.service when their own user units are not listed.
func (b *builder) unitOfCgroup(path string) (model.EntityRef, bool) {
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if !strings.Contains(parts[i], ".") {
			continue
		}
		if r := b.unitRef(parts[i]); b.listed(r) {
			return r, true
		}
	}
	return "", false
}

func (b *builder) listed(r model.EntityRef) bool {
	_, ok := b.w.ents[r]
	return ok
}

var okStatus = model.Status{Level: model.StatusOK}

func (b *builder) hostEntity(s *sample) model.EntityRef {
	var ips []string
	for _, n := range s.nics {
		ips = append(ips, n.addrs...)
	}
	return b.add(model.KindHost, s.host.name, s.host.name, okStatus, pruned(map[string]model.Value{
		"hostname": model.String(s.host.name), "kernel": model.String(s.host.kernel), "os": model.String(s.host.os),
		"cores": num(len(s.stat.cpus)), "memory": num(s.mem.total), "booted": model.Time(s.stat.boot),
		"ip": stringList(ips),
	}))
}

func (b *builder) memory(m memInfo) model.EntityRef {
	st := okStatus
	switch free := float64(m.available) / float64(m.total); {
	case free < memCrit:
		st = model.Status{Level: model.StatusCrit, Reason: "almost out of memory"}
	case free < memWarn:
		st = model.Status{Level: model.StatusWarn, Reason: "low on memory"}
	}
	return b.add(KindMemory, "memory", "memory", st, map[string]model.Value{"total": num(m.total), "swap_total": num(m.swapTotal)})
}

func (b *builder) filesystem(f filesystem) {
	st := okStatus
	switch u := f.usage.share(); {
	case f.usageErr != nil:
		st = model.Status{Level: model.StatusUnknown, Reason: "cannot read usage"}
	case u >= fsCrit:
		st = model.Status{Level: model.StatusCrit, Reason: "almost full"}
	case u >= fsWarn:
		st = model.Status{Level: model.StatusWarn, Reason: "nearly full"}
	}
	r := b.add(KindFilesystem, f.id, f.name, st, map[string]model.Value{
		"type": model.String(f.fstype), "mounts": stringList(f.mounts), "size": num(f.usage.total),
	})
	if f.disk != "" {
		b.link(r, b.ref(model.KindDisk, f.disk), model.RelRunsOn)
	} else {
		b.link(r, b.w.host, model.RelMemberOf)
	}
}

// share is the used fraction as df reports it: space reserved for root counts as neither.
func (u fsUsage) share() float64 {
	if u.used+u.avail == 0 {
		return 0
	}
	return float64(u.used) / float64(u.used+u.avail)
}

func nicStatus(state string) model.Status {
	switch state {
	case "up":
		return okStatus
	case "down", "lowerlayerdown", "notpresent":
		return model.Status{Level: model.StatusDown, Reason: "link " + state}
	}
	return model.Status{Level: model.StatusUnknown}
}

// processes adds each process on the host, and parent edges between listed processes.
func (b *builder) processes(s *sample) {
	listed := map[int]bool{}
	for _, p := range s.procs {
		listed[p.pid] = true
	}
	for _, p := range s.procs {
		started := s.stat.boot.Add(time.Duration(p.start) * time.Second / clockTick)
		a := pruned(map[string]model.Value{
			"pid": num(p.pid), "ppid": num(p.ppid), "user": model.String(p.user), "started": model.Time(started),
			"command": model.String(p.command),
		})
		r := b.add(model.KindProcess, strconv.Itoa(p.pid), p.comm, okStatus, a)
		b.link(r, b.w.host, model.RelRunsOn)
		if u, ok := b.unitOfCgroup(p.cgroup); ok {
			b.link(r, u, model.RelMemberOf)
		}
		if listed[p.ppid] {
			b.link(b.ref(model.KindProcess, strconv.Itoa(p.ppid)), r, model.RelParentOf)
		}
	}
}

func (b *builder) ref(kind model.Kind, native string) model.EntityRef {
	r, err := model.NewEntityRef(string(b.src), kind, native)
	if err != nil {
		panic(err) // the instance name is checked by config and native ids are never empty
	}
	return r
}

func (b *builder) add(kind model.Kind, native, name string, st model.Status, a map[string]model.Value) model.EntityRef {
	r := b.ref(kind, native)
	b.w.ents[r] = model.Entity{Ref: r, Kind: kind, Name: name, Status: st, Attrs: a, Source: b.src}
	return r
}

func (b *builder) link(from, to model.EntityRef, rel model.Relation) {
	b.linkWeighted(from, to, rel, 1)
}

func (b *builder) linkWeighted(from, to model.EntityRef, rel model.Relation, weight float64) {
	e := model.Edge{From: from, To: to, Rel: rel, Weight: weight, Source: b.src}
	b.w.edges[e.Key()] = e
}

// linkListed links to an entity only if it is listed; a hard dependency wins over a soft one.
func (b *builder) linkListed(from, to model.EntityRef, rel model.Relation, weight float64) {
	k := model.EdgeKey{From: from, To: to, Rel: rel}
	if old, ok := b.w.edges[k]; b.listed(to) && (!ok || old.Weight < weight) {
		b.linkWeighted(from, to, rel, weight)
	}
}

// pruned drops empty strings and lists, which say nothing.
func pruned(a map[string]model.Value) map[string]model.Value {
	maps.DeleteFunc(a, func(_ string, v model.Value) bool {
		return v.Type() == model.TypeString && v.Str() == "" || v.Type() == model.TypeList && len(v.List()) == 0
	})
	return a
}

func num[T int | uint64](v T) model.Value { return model.Number(float64(v)) }

func stringList(ss []string) model.Value {
	vs := make([]model.Value, len(ss))
	for i, s := range ss {
		vs[i] = model.String(s)
	}
	return model.List(vs...)
}
