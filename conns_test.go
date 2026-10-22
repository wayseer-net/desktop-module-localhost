package localhost

import (
	"net/netip"
	"slices"
	"testing"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

var anyPort = netip.AddrPortFrom(netip.IPv4Unspecified(), 0)

func tcp(cookie uint64, inode uint32, state uint8, local, remote string, sent, received uint64) socket {
	r := anyPort
	if remote != "" {
		r = ap(remote)
	}
	return socket{proto: "tcp", state: state, local: ap(local), remote: r, inode: inode, cookie: cookie, sent: sent, received: received, counted: true}
}

func proc(pid int) endpoint { return endpoint{pid: pid} }

func remoteAt(a string) endpoint { return endpoint{remote: netip.MustParseAddr(a)} }

func TestViewConnsOutbound(t *testing.T) {
	prev := []socket{tcp(1, 10, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 1000, 2000)}
	cur := []socket{tcp(1, 10, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 3000, 10000)}
	v := viewConns(cur, prev, map[uint32]int{10: 1200}, 2, nil)
	f := v.flows[flowKey{proc(1200), remoteAt("93.184.216.34")}]
	if f == nil {
		t.Fatalf("no flow in %v", v.flows)
	}
	if !f.counted || f.sent != 1000 || f.received != 4000 || f.conns != 1 || !slices.Equal(f.ports, []uint16{443}) {
		t.Errorf("flow %+v", *f)
	}
}

func TestViewConnsInbound(t *testing.T) {
	cur := []socket{
		tcp(1, 20, tcpListen, "0.0.0.0:22", "", 0, 0),
		tcp(2, 21, tcpEstablished, "192.168.1.10:22", "203.0.113.5:51000", 10, 20),
	}
	v := viewConns(cur, nil, map[uint32]int{20: 1201, 21: 1201}, 2, nil)
	f := v.flows[flowKey{remoteAt("203.0.113.5"), proc(1201)}]
	if f == nil || !slices.Equal(f.ports, []uint16{22}) {
		t.Fatalf("flows %v", v.flows)
	}
	if !slices.Equal(v.listens[1201], []string{"tcp 0.0.0.0:22"}) {
		t.Errorf("listens %v", v.listens)
	}
}

// A first sample has nothing to compare with, so it shows connections without rates.
func TestViewConnsFirstSampleHasNoRates(t *testing.T) {
	cur := []socket{tcp(1, 10, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 3000, 10000)}
	v := viewConns(cur, nil, nil, 0, nil)
	if f := v.flows[flowKey{proc(0), remoteAt("93.184.216.34")}]; f == nil || f.counted {
		t.Fatalf("flows %v", v.flows)
	}
}

// A socket opened since the last sample counts all its bytes.
func TestViewConnsNewSocketCountsAll(t *testing.T) {
	cur := []socket{tcp(5, 10, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 3000, 1000)}
	v := viewConns(cur, []socket{}, nil, 2, nil)
	if f := v.flows[flowKey{proc(0), remoteAt("93.184.216.34")}]; f == nil || f.sent != 1500 || f.received != 500 {
		t.Fatalf("flows %v", v.flows)
	}
}

func TestViewConnsLoopbackJoinsProcesses(t *testing.T) {
	cur := []socket{
		tcp(1, 30, tcpListen, "0.0.0.0:8080", "", 0, 0),
		tcp(2, 31, tcpEstablished, "127.0.0.1:40000", "127.0.0.1:8080", 100, 900),
		tcp(3, 32, tcpEstablished, "127.0.0.1:8080", "127.0.0.1:40000", 900, 100),
		tcp(4, 33, tcpEstablished, "192.168.1.10:41000", "192.168.1.10:8080", 0, 0), // to its own address
	}
	v := viewConns(cur, nil, map[uint32]int{30: 1201, 31: 1200, 32: 1201, 33: 1200}, 0, map[netip.Addr]bool{netip.MustParseAddr("192.168.1.10"): true})
	if len(v.flows) != 1 {
		t.Fatalf("flows %v, want one", v.flows)
	}
	f := v.flows[flowKey{proc(1200), proc(1201)}]
	if f == nil || f.conns != 2 || !slices.Equal(f.ports, []uint16{8080}) {
		t.Fatalf("flows %v", v.flows)
	}
}

func TestViewConnsSkipsSelfAndUnconnected(t *testing.T) {
	cur := []socket{
		tcp(1, 30, tcpListen, "127.0.0.1:8080", "", 0, 0),
		tcp(2, 31, tcpEstablished, "127.0.0.1:40000", "127.0.0.1:8080", 1, 1), // a process talking to itself
		tcp(3, 32, tcpClose, "0.0.0.0:0", "", 0, 0),
		{proto: "udp", state: tcpClose, local: ap("0.0.0.0:5353"), remote: anyPort, inode: 34},
	}
	v := viewConns(cur, nil, map[uint32]int{30: 1200, 31: 1200, 34: 900}, 2, nil)
	if len(v.flows) != 0 {
		t.Errorf("flows %v, want none", v.flows)
	}
	if !slices.Equal(v.listens[900], []string{"udp 0.0.0.0:5353"}) || !slices.Equal(v.listens[1200], []string{"tcp 127.0.0.1:8080"}) {
		t.Errorf("listens %v", v.listens)
	}
}

func TestViewConnsUDPHasNoRates(t *testing.T) {
	cur := []socket{{proto: "udp", state: tcpEstablished, local: ap("192.168.1.10:40000"), remote: ap("9.9.9.9:53"), inode: 40}}
	v := viewConns(cur, []socket{}, map[uint32]int{40: 1200}, 2, nil)
	f := v.flows[flowKey{proc(1200), remoteAt("9.9.9.9")}]
	if f == nil || f.counted || !slices.Equal(f.protos, []string{"udp"}) {
		t.Fatalf("flows %v", v.flows)
	}
}
