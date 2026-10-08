package localhost

import "wayseer.dev/sdk"

// AttrGroup names the group Topology draws an entity in.
const AttrGroup = "group"

// busyUnit is how many processes a unit needs to be drawn as a group of its own.
const busyUnit = 4

// hardware are the kinds of the machine's parts, drawn together around the host.
var hardware = map[sdk.Kind]bool{
	sdk.KindHost: true, KindCPU: true, KindMemory: true, sdk.KindDisk: true, KindFilesystem: true, sdk.KindInterface: true,
}

// group sets each entity's group: the machine's parts as hardware, a busy unit with its
// processes, other units with processes as services, the idle units as systemd, and processes
// in no unit as processes.
func (w *world) group() {
	unitOf := map[sdk.EntityRef]sdk.EntityRef{}
	procs := map[sdk.EntityRef]int{}
	for _, e := range w.edges {
		if e.Rel == sdk.RelMemberOf && w.ents[e.From].Kind == sdk.KindProcess {
			unitOf[e.From] = e.To
			procs[e.To]++
		}
	}
	unitGroup := func(u sdk.EntityRef) string {
		switch {
		case procs[u] >= busyUnit:
			return w.ents[u].Attrs["unit"].Str()
		case procs[u] > 0:
			return "services"
		}
		return "systemd"
	}
	for r, e := range w.ents {
		var g string
		switch {
		case hardware[e.Kind]:
			g = "hardware"
		case e.Kind == sdk.KindProcess && unitOf[r] == "":
			g = "processes"
		case e.Kind == sdk.KindProcess:
			g = unitGroup(unitOf[r])
		default:
			g = unitGroup(r)
		}
		e.Attrs[AttrGroup] = sdk.String(g)
		w.ents[r] = e
	}
}
