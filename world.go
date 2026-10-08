package localhost

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"wayseer.dev/sdk"
)

// Entity kinds the module adds to the core vocabulary.
const (
	KindCPU        sdk.Kind = "localhost/cpu"
	KindMemory     sdk.Kind = "localhost/memory"
	KindFilesystem sdk.Kind = "localhost/filesystem"
)

// AttrLocalPID is a process's pid on the machine Wayseer runs on, which identity rules match
// against the pids of Wayseer's own module processes; another root's processes lack it.
const AttrLocalPID = "local.pid"

// clockTick is USER_HZ, which Linux fixes at 100 on every architecture it exports times for.
const clockTick = 100

// Thresholds for warning and critical status.
const (
	fsWarn, fsCrit   = 0.90, 0.95 // share of a filesystem used
	memWarn, memCrit = 0.10, 0.05 // share of memory still available
)

// world is the entities and edges one sample describes.
type world struct {
	ents  map[sdk.EntityRef]sdk.Entity
	edges map[sdk.EdgeKey]sdk.Edge
	host  sdk.EntityRef
}

// builder turns samples into entities owned by one module instance.
type builder struct {
	src      sdk.ModuleID
	pageSize uint64
	local    bool // the machine read is the one Wayseer runs on
	w        world
}

func (m *Module) buildWorld(s *sample) world {
	b := builder{src: m.name, pageSize: m.pageSize, local: m.opts.Root == "/", w: world{ents: map[sdk.EntityRef]sdk.Entity{}, edges: map[sdk.EdgeKey]sdk.Edge{}}}
	b.w.host = b.hostEntity(s)
	for i, c := range s.stat.cpus {
		r := b.add(KindCPU, c.name, c.name, okStatus, map[string]sdk.Value{"index": num(i)})
		b.link(r, b.w.host, sdk.RelMemberOf)
	}
	b.link(b.memory(s.mem), b.w.host, sdk.RelMemberOf)
	for _, d := range s.disks {
		r := b.add(sdk.KindDisk, d.name, d.name, okStatus, pruned(map[string]sdk.Value{
			"size": inBytes(d.size), "model": sdk.String(d.model),
			"rotational": sdk.Bool(d.rotational), "removable": sdk.Bool(d.removable),
		}))
		b.link(r, b.w.host, sdk.RelMemberOf)
	}
	for _, f := range s.fss {
		b.filesystem(f)
	}
	for _, n := range s.nics {
		r := b.add(sdk.KindInterface, n.name, n.name, nicStatus(n.state), pruned(map[string]sdk.Value{
			"mac": sdk.String(n.mac), "mtu": num(n.mtu), "address": stringList(n.addrs),
		}))
		b.link(r, b.w.host, sdk.RelMemberOf)
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
		r := b.add(kind, u.Name, name, u.status(), pruned(map[string]sdk.Value{
			"unit": sdk.String(u.Name), "type": sdk.String(typ), "description": sdk.String(u.Description),
			"state": sdk.String(u.ActiveState), "sub_state": sdk.String(u.SubState),
			"file_state": sdk.String(u.props.fileState), "result": sdk.String(u.props.result),
		}))
		b.link(r, b.w.host, sdk.RelRunsOn)
	}
	for _, u := range us {
		from := b.unitRef(u.Name)
		for _, d := range u.props.deps.hard {
			b.linkListed(from, b.unitRef(d), sdk.RelDependsOn, 1)
		}
		for _, d := range u.props.deps.soft {
			b.linkListed(from, b.unitRef(d), sdk.RelDependsOn, 0.5)
		}
	}
}

func (b *builder) unitRef(name string) sdk.EntityRef {
	kind, _ := unitKind(name)
	return b.ref(kind, name)
}

// unitOfCgroup is the innermost listed unit in a cgroup path, so a user's session processes
// belong to user@UID.service when their own user units are not listed.
func (b *builder) unitOfCgroup(path string) (sdk.EntityRef, bool) {
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

func (b *builder) listed(r sdk.EntityRef) bool {
	_, ok := b.w.ents[r]
	return ok
}

var okStatus = sdk.Status{Level: sdk.StatusOK}

func (b *builder) hostEntity(s *sample) sdk.EntityRef {
	var ips []string
	for _, n := range s.nics {
		ips = append(ips, n.addrs...)
	}
	return b.add(sdk.KindHost, s.host.name, s.host.name, okStatus, pruned(map[string]sdk.Value{
		"hostname": sdk.String(s.host.name), "kernel": sdk.String(s.host.kernel), "os": sdk.String(s.host.os),
		"cores": num(len(s.stat.cpus)), "memory": inBytes(s.mem.total), "booted": sdk.Time(s.stat.boot),
		"ip": stringList(ips),
	}))
}

func (b *builder) memory(m memInfo) sdk.EntityRef {
	st := okStatus
	switch free := float64(m.available) / float64(m.total); {
	case free < memCrit:
		st = sdk.Status{Level: sdk.StatusCrit, Reason: "almost out of memory"}
	case free < memWarn:
		st = sdk.Status{Level: sdk.StatusWarn, Reason: "low on memory"}
	}
	return b.add(KindMemory, "memory", "memory", st, map[string]sdk.Value{"total": inBytes(m.total), "swap_total": inBytes(m.swapTotal)})
}

func (b *builder) filesystem(f filesystem) {
	st := okStatus
	switch u := f.usage.share(); {
	case f.usageErr != nil:
		st = sdk.Status{Level: sdk.StatusUnknown, Reason: "cannot read usage"}
	case u >= fsCrit:
		st = sdk.Status{Level: sdk.StatusCrit, Reason: "almost full"}
	case u >= fsWarn:
		st = sdk.Status{Level: sdk.StatusWarn, Reason: "nearly full"}
	}
	r := b.add(KindFilesystem, f.id, f.name, st, map[string]sdk.Value{
		"type": sdk.String(f.fstype), "mounts": stringList(f.mounts), "size": inBytes(f.usage.total),
	})
	if f.disk != "" {
		b.link(r, b.ref(sdk.KindDisk, f.disk), sdk.RelRunsOn)
	} else {
		b.link(r, b.w.host, sdk.RelMemberOf)
	}
}

// share is the used fraction as df reports it: space reserved for root counts as neither.
func (u fsUsage) share() float64 {
	if u.used+u.avail == 0 {
		return 0
	}
	return float64(u.used) / float64(u.used+u.avail)
}

func nicStatus(state string) sdk.Status {
	switch state {
	case "up":
		return okStatus
	case "down", "lowerlayerdown", "notpresent":
		return sdk.Status{Level: sdk.StatusDown, Reason: "link " + state}
	}
	return sdk.Status{Level: sdk.StatusUnknown}
}

// processes adds each process on the host, and parent edges between listed processes.
func (b *builder) processes(s *sample) {
	listed := map[int]bool{}
	for _, p := range s.procs {
		listed[p.pid] = true
	}
	services := servicesByPID(s.units)
	for _, p := range s.procs {
		a := pruned(map[string]sdk.Value{
			"pid": num(p.pid), "ppid": num(p.ppid), "user": sdk.String(p.user), "command": sdk.String(p.command),
		})
		if b.local {
			a[AttrLocalPID] = num(p.pid)
		}
		if !p.startUnknown {
			a["started"] = sdk.Time(s.stat.boot.Add(time.Duration(p.start) * time.Second / clockTick))
		}
		r := b.add(sdk.KindProcess, strconv.Itoa(p.pid), p.comm, okStatus, a)
		b.link(r, b.w.host, sdk.RelRunsOn)
		if u, ok := b.unitOfCgroup(p.cgroup); ok {
			b.link(r, u, sdk.RelMemberOf)
		}
		for _, name := range services[p.pid] {
			b.link(r, b.unitRef(name), sdk.RelMemberOf)
		}
		if listed[p.ppid] {
			b.link(b.ref(sdk.KindProcess, strconv.Itoa(p.ppid)), r, sdk.RelParentOf)
		}
	}
}

// servicesByPID are the running units by their process, which only Windows reports.
func servicesByPID(us []unit) map[int][]string {
	out := map[int][]string{}
	for _, u := range us {
		if u.pid > 0 && u.ActiveState != "inactive" && u.ActiveState != "failed" {
			out[u.pid] = append(out[u.pid], u.Name)
		}
	}
	return out
}

func (b *builder) ref(kind sdk.Kind, native string) sdk.EntityRef {
	r, err := sdk.NewEntityRef(string(b.src), kind, native)
	if err != nil {
		panic(err) // the instance name is checked by config and native ids are never empty
	}
	return r
}

func (b *builder) add(kind sdk.Kind, native, name string, st sdk.Status, a map[string]sdk.Value) sdk.EntityRef {
	r := b.ref(kind, native)
	b.w.ents[r] = sdk.Entity{Ref: r, Kind: kind, Name: name, Status: st, Attrs: a, Source: b.src}
	return r
}

func (b *builder) link(from, to sdk.EntityRef, rel sdk.Relation) {
	b.linkWeighted(from, to, rel, 1)
}

func (b *builder) linkWeighted(from, to sdk.EntityRef, rel sdk.Relation, weight float64) {
	e := sdk.Edge{From: from, To: to, Rel: rel, Weight: weight, Source: b.src}
	b.w.edges[e.Key()] = e
}

// linkListed links to an entity only if it is listed; a hard dependency wins over a soft one.
func (b *builder) linkListed(from, to sdk.EntityRef, rel sdk.Relation, weight float64) {
	k := sdk.EdgeKey{From: from, To: to, Rel: rel}
	if old, ok := b.w.edges[k]; b.listed(to) && (!ok || old.Weight < weight) {
		b.linkWeighted(from, to, rel, weight)
	}
}

// pruned drops empty strings and lists, which say nothing.
func pruned(a map[string]sdk.Value) map[string]sdk.Value {
	maps.DeleteFunc(a, func(_ string, v sdk.Value) bool {
		return v.Type() == sdk.TypeString && v.Str() == "" || v.Type() == sdk.TypeList && len(v.List()) == 0
	})
	return a
}

func num[T int | uint64](v T) sdk.Value { return sdk.Number(float64(v)) }

// inBytes is a size in bytes.
func inBytes(n uint64) sdk.Value { return num(n).In(sdk.UnitBytes) }

func stringList(ss []string) sdk.Value {
	vs := make([]sdk.Value, len(ss))
	for i, s := range ss {
		vs[i] = sdk.String(s)
	}
	return sdk.List(vs...)
}
