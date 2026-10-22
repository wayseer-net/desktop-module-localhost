package localhost

import (
	"cmp"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"wayseer.dev/sdk"
)

// connState is what connections carry from one sample to the next.
type connState struct {
	recent  map[flowKey]seenFlow
	listens map[int][]string // as last sent events for; nil before the first sample
	note    string           // why connections are not read, from config
	read    string           // what the last read could not see
}

type seenFlow struct {
	f  flow
	at time.Time
}

func connsNote(readable bool) string {
	if readable {
		return ""
	}
	return "connections are read on Linux only, and only on the machine itself"
}

func (c *connState) text() string {
	return strings.Join(slices.DeleteFunc([]string{c.note, c.read}, func(s string) bool { return s == "" }), "; ")
}

// withConnections is the catalogue with network rates for processes and remote addresses.
func withConnections(cat []sdk.Metric) []sdk.Metric {
	out := slices.Clone(cat)
	for i, m := range out {
		if m.Name == MetricNetReceive || m.Name == MetricNetSend {
			out[i].Kinds = append(slices.Clone(m.Kinds), sdk.KindProcess, KindRemote)
			out[i].Native += "; sock_diag tcp_info for processes and remotes"
		}
	}
	return out
}

// readSockets adds the sockets and their owners to s; failing to read them is a note, not an
// error, so the rest of the machine still shows.
func (r *reader) readSockets(s *sample) {
	socks, note, err := r.sockets()
	if err != nil {
		note = err.Error()
	}
	if note != "" {
		s.sockNote = "connections: " + note
	}
	s.socks = socks
	if s.socks == nil {
		s.socks = []socket{} // read, if empty, so the world shows connections
	}
	inodes := map[uint32]bool{}
	for _, k := range socks {
		if k.inode != 0 {
			inodes[k.inode] = true
		}
	}
	s.owners = r.socketOwners(inodes)
}

// viewSample compares s's sockets with prev's, if prev read them too.
func viewSample(s, prev *sample) connView {
	if s.socks == nil {
		return connView{}
	}
	var before []socket
	elapsed := 0.0
	if prev != nil && prev.socks != nil {
		before, elapsed = prev.socks, s.at.Sub(prev.at).Seconds()
	}
	return viewConns(s.socks, before, s.owners, elapsed, ownAddrs(s.nics))
}

func ownAddrs(nics []nic) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	for _, n := range nics {
		for _, a := range n.addrs {
			if ip, err := netip.ParseAddr(a); err == nil {
				out[ip.Unmap()] = true
			}
		}
	}
	return out
}

// shown merges this sample's flows with those closed within keep, which show at rest, and
// keeps only the limit remotes with the most traffic, then the most connections.
func (c *connState) shown(v connView, now time.Time, keep time.Duration, limit int) map[flowKey]flow {
	if c.recent == nil {
		c.recent = map[flowKey]seenFlow{}
	}
	for k, f := range v.flows {
		c.recent[k] = seenFlow{*f, now}
	}
	maps.DeleteFunc(c.recent, func(_ flowKey, s seenFlow) bool { return now.Sub(s.at) > keep })
	type score struct {
		bytes float64
		conns int
		at    time.Time
	}
	scores := map[netip.Addr]*score{}
	out := map[flowKey]flow{}
	for k, s := range c.recent {
		f := s.f
		if !s.at.Equal(now) {
			f.sent, f.received, f.conns = 0, 0, 0
		}
		out[k] = f
		if a := k.remote(); a.IsValid() {
			sc := scores[a]
			if sc == nil {
				sc = &score{}
				scores[a] = sc
			}
			sc.bytes += f.sent + f.received
			sc.conns += f.conns
			if s.at.After(sc.at) {
				sc.at = s.at
			}
		}
	}
	ranked := slices.SortedFunc(maps.Keys(scores), func(a, b netip.Addr) int {
		x, y := scores[a], scores[b]
		return cmp.Or(cmp.Compare(y.bytes, x.bytes), cmp.Compare(y.conns, x.conns), y.at.Compare(x.at), a.Compare(b))
	})
	for _, a := range ranked[min(limit, len(ranked)):] {
		drop := func(k flowKey, _ flow) bool { return k.remote() == a }
		maps.DeleteFunc(out, drop)
		maps.DeleteFunc(c.recent, func(k flowKey, _ seenFlow) bool { return k.remote() == a })
	}
	return out
}

// remote is the flow's remote address, if it has one.
func (k flowKey) remote() netip.Addr {
	if k.from.remote.IsValid() {
		return k.from.remote
	}
	return k.to.remote
}

// owner is the end whose socket was read: the one that is not remote, or the dialling process.
func (k flowKey) owner() endpoint {
	if k.from.remote.IsValid() {
		return k.to
	}
	return k.from
}

// connections adds the remotes, an edge for each flow, and listening sockets as attributes.
func (b *builder) connections(flows map[flowKey]flow, listens map[int][]string) {
	b.w.flows = flows
	type edgeSum struct {
		out, in  float64 // bytes per second from the edge's From to its To, and back
		counted  bool
		conns    int
		ports    []uint16
		protocol []string
	}
	edges := map[sdk.EdgeKey]*edgeSum{}
	remotes := map[netip.Addr]*flow{}
	for _, k := range slices.SortedFunc(maps.Keys(flows), compareFlowKeys) {
		f := flows[k]
		from, to := b.endpointRef(k.from), b.endpointRef(k.to)
		if from == to {
			continue
		}
		out, in := f.sent, f.received
		if k.from.remote.IsValid() {
			out, in = in, out // the socket read is the accepting process's
		}
		ek := sdk.EdgeKey{From: from, To: to, Rel: sdk.RelTalksTo}
		e := edges[ek]
		if e == nil {
			e = &edgeSum{}
			edges[ek] = e
		}
		e.out, e.in, e.counted, e.conns = e.out+out, e.in+in, e.counted || f.counted, e.conns+f.conns
		e.ports, e.protocol = mergeSorted(e.ports, f.ports), mergeSorted(e.protocol, f.protos)
		if a := k.remote(); a.IsValid() {
			r := remotes[a]
			if r == nil {
				r = &flow{}
				remotes[a] = r
			}
			r.conns += f.conns
			r.ports, r.protos = mergeSorted(r.ports, f.ports), mergeSorted(r.protos, f.protos)
		}
	}
	for a, r := range remotes {
		b.remote(a, r)
	}
	for k, s := range edges {
		e := sdk.Edge{From: k.From, To: k.To, Rel: k.Rel, Weight: 1, Source: b.src, Attrs: map[string]sdk.Value{
			"connections": num(s.conns), "ports": portList(s.ports), "protocols": stringList(s.protocol),
		}}
		if s.counted {
			e.Traffic = sdk.Traffic{Rate: s.out + s.in, Unit: sdk.TrafficBytes}
			e.Attrs["bytes_out"] = sdk.Number(s.out).In(sdk.UnitBytesPS)
			e.Attrs["bytes_in"] = sdk.Number(s.in).In(sdk.UnitBytesPS)
		}
		b.w.edges[k] = e
	}
	for pid, ls := range listens {
		r := b.endpointRef(endpoint{pid: pid})
		e := b.w.ents[r]
		e.Attrs["listens"] = stringList(appendAllCapped(listsOf(e.Attrs["listens"]), ls))
		b.w.ents[r] = e
	}
}

// remote adds a remote address, placed and named by the IP databases if there are any.
func (b *builder) remote(a netip.Addr, f *flow) {
	attrs := map[string]sdk.Value{
		"address": sdk.String(a.String()), "scope": sdk.String(scope(a)), "ports": portList(f.ports),
		"protocols": stringList(f.protos), "connections": num(f.conns),
	}
	var l location
	if b.geo != nil {
		l = b.geo.locate(a)
		maps.Copy(attrs, l.attrs())
	}
	r := b.add(KindRemote, a.String(), a.String(), okStatus, attrs)
	e := b.w.ents[r]
	e.Place = l.place
	b.w.ents[r] = e
}

// endpointRef is a remote's entity, a listed process, or else the host.
func (b *builder) endpointRef(e endpoint) sdk.EntityRef {
	if e.remote.IsValid() {
		return b.ref(KindRemote, e.remote.String())
	}
	if r := b.ref(sdk.KindProcess, strconv.Itoa(e.pid)); e.pid > 0 && b.listed(r) {
		return r
	}
	return b.w.host
}

func compareFlowKeys(a, b flowKey) int {
	return cmp.Or(compareEndpoints(a.from, b.from), compareEndpoints(a.to, b.to))
}

func compareEndpoints(a, b endpoint) int {
	return cmp.Or(cmp.Compare(a.pid, b.pid), a.remote.Compare(b.remote))
}

// scope says what kind of address a remote is.
func scope(a netip.Addr) string {
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsLinkLocalUnicast():
		return "link-local"
	case a.IsPrivate():
		return "private"
	case a.IsMulticast():
		return "multicast"
	}
	return "public"
}

func portList(ps []uint16) sdk.Value {
	vs := make([]sdk.Value, len(ps))
	for i, p := range ps {
		vs[i] = num(int(p))
	}
	return sdk.List(vs...)
}

func listsOf(v sdk.Value) []string {
	var out []string
	if v.Type() == sdk.TypeList {
		for _, s := range v.List() {
			out = append(out, s.Str())
		}
	}
	return out
}

func appendAllCapped(ss, more []string) []string {
	for _, s := range more {
		ss = appendCapped(ss, s)
	}
	return ss
}

func mergeSorted[T cmp.Ordered](a, b []T) []T {
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) && len(out) < portCap {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// connections records each remote's and process's network rates from the flows shown.
func (r *recorder) connections() {
	if r.w.flows == nil || r.elapsed <= 0 {
		return
	}
	type rates struct{ rx, tx float64 }
	sum := map[sdk.EntityRef]*rates{}
	add := func(ref sdk.EntityRef, rx, tx float64) {
		if ref == r.w.host {
			return // the host's rates are its interfaces'
		}
		s := sum[ref]
		if s == nil {
			s = &rates{}
			sum[ref] = s
		}
		s.rx, s.tx = s.rx+rx, s.tx+tx
	}
	b := builder{src: r.m.name, w: *r.w}
	for k, f := range r.w.flows {
		if !f.counted {
			continue
		}
		owner, other := k.owner(), k.from
		if owner == k.from {
			other = k.to
		}
		add(b.endpointRef(owner), f.received, f.sent)
		if other.remote.IsValid() {
			add(b.endpointRef(other), f.received, f.sent) // as this machine sees the remote
		} else {
			add(b.endpointRef(other), f.sent, f.received) // a local peer receives what the owner sent
		}
	}
	for ref, s := range sum {
		r.put(ref, MetricNetReceive, s.rx)
		r.put(ref, MetricNetSend, s.tx)
	}
}

// listenEvents reports listening sockets opened or closed since the last sample, by TCP only:
// UDP's bound sockets come and go with every lookup. A listener on every interface warns.
func (m *Module) listenEvents(s *sample) []sdk.Event {
	if s.socks == nil {
		return nil
	}
	cur := s.conns.listens
	prev := m.conns.listens
	m.conns.listens = cur
	if prev == nil {
		return nil
	}
	b := builder{src: m.name, w: m.world}
	var evs []sdk.Event
	diff := func(a, z map[int][]string, opened bool) {
		for _, pid := range slices.Sorted(maps.Keys(a)) {
			for _, l := range a[pid] {
				if !strings.HasPrefix(l, "tcp ") || slices.Contains(z[pid], l) {
					continue
				}
				evs = append(evs, m.listenEvent(b.endpointRef(endpoint{pid: pid}), pid, l, opened, s.at))
			}
		}
	}
	diff(cur, prev, true)
	diff(prev, cur, false)
	return evs
}

func (m *Module) listenEvent(on sdk.EntityRef, pid int, sock string, opened bool, at time.Time) sdk.Event {
	sev, msg := sdk.SevInfo, "stopped listening on "+sock
	if opened {
		sev, msg = sdk.SevInfo, "listening on "+sock
		if wildcard(sock) {
			sev, msg = sdk.SevWarn, "listening on all interfaces: "+sock
		}
	}
	return sdk.Event{
		ID: "listen;" + strconv.Itoa(pid) + ";" + sock + ";" + strconv.FormatInt(at.UnixNano(), 10), Entity: on,
		At: at, Severity: sev, Kind: "listen", Message: msg, Source: m.name,
		Fields: map[string]sdk.Value{"socket": sdk.String(sock), "open": sdk.Bool(opened)},
	}
}

func wildcard(sock string) bool {
	return strings.Contains(sock, " 0.0.0.0:") || strings.Contains(sock, " [::]:")
}
