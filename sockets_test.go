package localhost

import (
	"encoding/binary"
	"errors"
	"maps"
	"net/netip"
	"os"
	"testing"
)

// diagSocket encodes one inet_diag_msg as the kernel sends it, with tcp_info cut to infoLen
// bytes (0 for none).
func diagSocket(family, state uint8, local, remote netip.AddrPort, uid, inode uint32, cookie uint64, acked, received uint64, infoLen int) []byte {
	msg := make([]byte, diagMsgLen)
	msg[0], msg[1] = family, state
	binary.BigEndian.PutUint16(msg[4:], local.Port())
	binary.BigEndian.PutUint16(msg[6:], remote.Port())
	putAddr(msg[8:24], local.Addr())
	putAddr(msg[24:40], remote.Addr())
	binary.NativeEndian.PutUint64(msg[44:], cookie)
	binary.NativeEndian.PutUint32(msg[64:], uid)
	binary.NativeEndian.PutUint32(msg[68:], inode)
	if infoLen > 0 {
		info := make([]byte, infoLen)
		if infoLen >= tcpInfoBytesLen {
			binary.NativeEndian.PutUint64(info[tcpInfoAcked:], acked)
			binary.NativeEndian.PutUint64(info[tcpInfoReceived:], received)
		}
		msg = append(msg, rtattr(inetDiagInfo, info)...)
	}
	return nlmsg(sockDiagByFamily, msg)
}

func putAddr(dst []byte, a netip.Addr) {
	if a.Is4() {
		b := a.As4()
		copy(dst, b[:])
		return
	}
	b := a.As16()
	copy(dst, b[:])
}

func rtattr(typ uint16, data []byte) []byte {
	b := make([]byte, align4(4+len(data)))
	binary.NativeEndian.PutUint16(b[0:], uint16(4+len(data)))
	binary.NativeEndian.PutUint16(b[2:], typ)
	copy(b[4:], data)
	return b
}

func nlmsg(typ uint16, payload []byte) []byte {
	b := make([]byte, align4(nlmsgHdrLen+len(payload)))
	binary.NativeEndian.PutUint32(b[0:], uint32(nlmsgHdrLen+len(payload)))
	binary.NativeEndian.PutUint16(b[4:], typ)
	copy(b[nlmsgHdrLen:], payload)
	return b
}

func nlerror(errno int32) []byte {
	p := make([]byte, 4+nlmsgHdrLen)
	binary.NativeEndian.PutUint32(p, uint32(-errno))
	return nlmsg(nlmsgError, p)
}

var (
	v4Local  = netip.MustParseAddrPort("192.168.1.10:50412")
	v4Remote = netip.MustParseAddrPort("93.184.216.34:443")
	v6Local  = netip.MustParseAddrPort("[2001:db8::10]:22")
	v6Remote = netip.MustParseAddrPort("[2001:db8::99]:61000")
)

func TestParseDiagReadsSockets(t *testing.T) {
	var b []byte
	b = append(b, diagSocket(afInet, tcpEstablished, v4Local, v4Remote, 1000, 4711, 9, 1200, 3400, 232)...)
	b = append(b, diagSocket(afInet6, tcpEstablished, v6Local, v6Remote, 0, 4712, 10, 5, 6, 232)...)
	got, done, err := parseDiag(b, "tcp")
	if err != nil || done {
		t.Fatalf("parseDiag: done %v, err %v", done, err)
	}
	want := []socket{
		{proto: "tcp", state: tcpEstablished, local: v4Local, remote: v4Remote, uid: 1000, inode: 4711, cookie: 9, sent: 1200, received: 3400, counted: true},
		{proto: "tcp", state: tcpEstablished, local: v6Local, remote: v6Remote, uid: 0, inode: 4712, cookie: 10, sent: 5, received: 6, counted: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sockets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("socket %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDiagWithoutByteCounts(t *testing.T) {
	for _, n := range []int{0, 104} { // UDP has no tcp_info; kernels before 4.2 send a shorter one
		b := diagSocket(afInet, tcpEstablished, v4Local, v4Remote, 1, 2, 3, 77, 88, n)
		got, _, err := parseDiag(b, "udp")
		if err != nil || len(got) != 1 {
			t.Fatalf("info %d: got %v, %v", n, got, err)
		}
		if got[0].counted || got[0].sent != 0 || got[0].received != 0 {
			t.Errorf("info %d: counters %+v, want none", n, got[0])
		}
	}
}

func TestParseDiagStopsAtDone(t *testing.T) {
	b := append(diagSocket(afInet, tcpListen, v4Local, netip.AddrPortFrom(netip.IPv4Unspecified(), 0), 0, 1, 1, 0, 0, 0), nlmsg(nlmsgDone, make([]byte, 4))...)
	got, done, err := parseDiag(b, "tcp")
	if err != nil || !done || len(got) != 1 {
		t.Fatalf("got %d sockets, done %v, err %v", len(got), done, err)
	}
}

func TestParseDiagReportsKernelError(t *testing.T) {
	_, _, err := parseDiag(nlerror(int32(errnoNoEnt)), "udp")
	var de diagError
	if !errors.As(err, &de) || de.errno != errnoNoEnt {
		t.Fatalf("err = %v, want errno %d", err, errnoNoEnt)
	}
}

func TestParseDiagRejectsTruncated(t *testing.T) {
	b := diagSocket(afInet, tcpEstablished, v4Local, v4Remote, 1, 2, 3, 4, 5, 232)
	for _, n := range []int{3, nlmsgHdrLen + 10, len(b) - 8} {
		if _, _, err := parseDiag(b[:n], "tcp"); err == nil {
			t.Errorf("%d bytes: no error", n)
		}
	}
}

func TestDiagRequest(t *testing.T) {
	b := diagRequest(afInet6, protoUDP, 7)
	if len(b) != nlmsgHdrLen+diagReqLen {
		t.Fatalf("len %d", len(b))
	}
	if got := binary.NativeEndian.Uint16(b[4:]); got != sockDiagByFamily {
		t.Errorf("type %d", got)
	}
	if got := binary.NativeEndian.Uint16(b[6:]); got != nlmRequest|nlmDump {
		t.Errorf("flags %#x", got)
	}
	if got := binary.NativeEndian.Uint32(b[8:]); got != 7 {
		t.Errorf("seq %d", got)
	}
	p := b[nlmsgHdrLen:]
	if p[0] != afInet6 || p[1] != protoUDP || p[2] != 1<<(inetDiagInfo-1) {
		t.Errorf("family %d proto %d ext %#x", p[0], p[1], p[2])
	}
	if got := binary.NativeEndian.Uint32(p[4:]); got != wantedStates {
		t.Errorf("states %#x", got)
	}
}

func TestSocketOwners(t *testing.T) {
	r := &reader{fsys: os.DirFS(fixture)}
	got := r.socketOwners(map[uint32]bool{4711: true, 4712: true, 9999: true})
	want := map[uint32]int{4711: 1200, 4712: 1201}
	if !maps.Equal(got, want) {
		t.Fatalf("owners %v, want %v", got, want)
	}
	if r.ownerScans != 1 {
		t.Fatalf("%d scans", r.ownerScans)
	}
	r.socketOwners(map[uint32]bool{4711: true})
	if r.ownerScans != 1 {
		t.Errorf("known inodes rescanned: %d scans", r.ownerScans)
	}
}
