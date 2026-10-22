package localhost

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"
)

// portCap bounds the ports and listening sockets kept per flow or process.
const portCap = 16

// endpoint is one end of a flow: a remote address, or a process on this machine (pid 0 for
// a socket whose owner cannot be read, shown on the host).
type endpoint struct {
	pid    int
	remote netip.Addr // valid for a remote
}

// flowKey runs from whoever opened the connections to whoever accepted them.
type flowKey struct{ from, to endpoint }

// flow sums the connections between two endpoints; rates are bytes per second from this
// machine's side.
type flow struct {
	sent, received float64
	counted        bool // some connection had byte counts to compare
	conns          int
	ports          []uint16 // the service ports connected to
	protos         []string
}

// connView is what one sample's sockets show.
type connView struct {
	flows   map[flowKey]*flow
	listens map[int][]string // owner to its listening sockets, like "tcp 0.0.0.0:22"
}

// viewConns sums sockets into flows, with rates from prev when elapsed is known. self holds
// this machine's own addresses besides loopback, so connections to them join processes.
func viewConns(cur, prev []socket, owners map[uint32]int, elapsed float64, self map[netip.Addr]bool) connView {
	v := connView{flows: map[flowKey]*flow{}, listens: map[int][]string{}}
	before := map[uint64]socket{}
	for _, s := range prev {
		before[s.cookie] = s
	}
	listeners := map[netip.AddrPort]int{}
	for _, s := range cur {
		if listening(s) {
			if s.proto == "tcp" {
				listeners[s.local] = owners[s.inode]
			}
			v.listens[owners[s.inode]] = appendCapped(v.listens[owners[s.inode]], s.proto+" "+s.local.String())
		}
	}
	local := func(a netip.Addr) bool { return a.IsLoopback() || self[a] }
	for _, s := range cur {
		if listening(s) || !s.remote.Addr().IsValid() || s.remote.Port() == 0 {
			continue
		}
		owner := endpoint{pid: owners[s.inode]}
		var k flowKey
		var port uint16
		_, accepted := listenerOf(listeners, s.local)
		switch {
		case accepted && local(s.remote.Addr()):
			continue // the accepted end of a local pair; the dialling end counts it
		case accepted:
			k, port = flowKey{endpoint{remote: s.remote.Addr()}, owner}, s.local.Port()
		case local(s.remote.Addr()):
			pid, _ := listenerOf(listeners, s.remote)
			k, port = flowKey{owner, endpoint{pid: pid}}, s.remote.Port()
		default:
			k, port = flowKey{owner, endpoint{remote: s.remote.Addr()}}, s.remote.Port()
		}
		if k.from == k.to {
			continue
		}
		f := v.flows[k]
		if f == nil {
			f = &flow{}
			v.flows[k] = f
		}
		f.add(s, before, prev != nil && elapsed > 0, elapsed, port)
	}
	return v
}

func (f *flow) add(s socket, before map[uint64]socket, rated bool, elapsed float64, port uint16) {
	f.conns++
	if !slices.Contains(f.ports, port) && len(f.ports) < portCap {
		f.ports = append(f.ports, port)
		slices.Sort(f.ports)
	}
	if !slices.Contains(f.protos, s.proto) {
		f.protos = append(f.protos, s.proto)
		slices.Sort(f.protos)
	}
	if !rated || !s.counted {
		return
	}
	b := before[s.cookie] // a socket opened since prev starts from zero
	if s.sent >= b.sent && s.received >= b.received {
		f.sent += float64(s.sent-b.sent) / elapsed
		f.received += float64(s.received-b.received) / elapsed
		f.counted = true
	}
}

// listening is a TCP listener, or a UDP socket bound but not connected.
func listening(s socket) bool {
	if s.proto == "tcp" {
		return s.state == tcpListen
	}
	return s.remote.Port() == 0 && s.local.Port() != 0
}

// listenerOf finds which process listens over TCP where a connection's end is: on that address, or on all.
func listenerOf(listeners map[netip.AddrPort]int, at netip.AddrPort) (int, bool) {
	if pid, ok := listeners[at]; ok {
		return pid, true
	}
	for _, wild := range []netip.Addr{netip.IPv4Unspecified(), netip.IPv6Unspecified()} {
		if pid, ok := listeners[netip.AddrPortFrom(wild, at.Port())]; ok {
			return pid, true
		}
	}
	return 0, false
}

func appendCapped(ss []string, s string) []string {
	if len(ss) >= portCap || slices.Contains(ss, s) {
		return ss
	}
	ss = append(ss, s)
	slices.SortFunc(ss, func(a, b string) int { return cmp.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return ss
}
