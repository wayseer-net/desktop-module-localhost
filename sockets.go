package localhost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"strconv"
	"strings"
)

// socket is one TCP or UDP socket as sock_diag reports it.
type socket struct {
	proto          string // tcp or udp
	state          uint8  // TCP state numbering, which UDP shares
	local, remote  netip.AddrPort
	uid, inode     uint32
	cookie         uint64 // the kernel's unique id for the socket
	sent, received uint64 // bytes acknowledged and received, when counted
	counted        bool   // the kernel sent byte counts (TCP, Linux 4.2 and later)
}

// Netlink and sock_diag constants, from linux/netlink.h, sock_diag.h and inet_diag.h.
const (
	nlmsgHdrLen      = 16
	nlmsgError       = 2
	nlmsgDone        = 3
	nlmRequest       = 0x1
	nlmDump          = 0x300
	sockDiagByFamily = 20
	diagReqLen       = 56
	diagMsgLen       = 72
	inetDiagInfo     = 2

	afInet   = 2
	afInet6  = 10
	protoTCP = 6
	protoUDP = 17

	errnoNoEnt = 2
)

// TCP states, from linux/tcp_states.h.
const (
	tcpEstablished = 1
	tcpSynSent     = 2
	tcpSynRecv     = 3
	tcpTimeWait    = 6
	tcpClose       = 7
	tcpListen      = 10
)

// wantedStates is every state but TIME_WAIT, whose sockets no longer belong to a process.
const wantedStates = ^uint32(1 << tcpTimeWait)

// Offsets in struct tcp_info of the byte counters, which arrived in Linux 4.2.
const (
	tcpInfoAcked    = 120
	tcpInfoReceived = 128
	tcpInfoBytesLen = 136
)

// diagError is the kernel refusing a dump, such as ENOENT when udp_diag is not loaded.
type diagError struct{ errno int32 }

func (e diagError) Error() string { return fmt.Sprintf("sock_diag: errno %d", e.errno) }

var errTruncated = errors.New("sock_diag: truncated message")

// diagRequest asks for a dump of one family and protocol's sockets, with TCP's byte counts.
func diagRequest(family, proto uint8, seq uint32) []byte {
	b := make([]byte, nlmsgHdrLen+diagReqLen)
	binary.NativeEndian.PutUint32(b[0:], uint32(len(b)))
	binary.NativeEndian.PutUint16(b[4:], sockDiagByFamily)
	binary.NativeEndian.PutUint16(b[6:], nlmRequest|nlmDump)
	binary.NativeEndian.PutUint32(b[8:], seq)
	p := b[nlmsgHdrLen:]
	p[0], p[1], p[2] = family, proto, 1<<(inetDiagInfo-1)
	binary.NativeEndian.PutUint32(p[4:], wantedStates)
	return b
}

// parseDiag reads one datagram of a dump; done is set by the message that ends it.
func parseDiag(b []byte, proto string) (out []socket, done bool, err error) {
	for len(b) > 0 {
		if len(b) < nlmsgHdrLen {
			return out, false, errTruncated
		}
		n := int(binary.NativeEndian.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return out, false, errTruncated
		}
		payload := b[nlmsgHdrLen:n]
		switch binary.NativeEndian.Uint16(b[4:]) {
		case nlmsgDone:
			return out, true, nil
		case nlmsgError:
			if len(payload) < 4 {
				return out, false, errTruncated
			}
			if errno := -int32(binary.NativeEndian.Uint32(payload)); errno != 0 { //nolint:gosec // errno is negative in the message
				return out, false, diagError{errno}
			}
		case sockDiagByFamily:
			s, err := parseDiagMsg(payload, proto)
			if err != nil {
				return out, false, err
			}
			out = append(out, s)
		}
		b = b[min(align4(n), len(b)):]
	}
	return out, false, nil
}

func parseDiagMsg(p []byte, proto string) (socket, error) {
	if len(p) < diagMsgLen {
		return socket{}, errTruncated
	}
	s := socket{
		proto: proto, state: p[1],
		local:  netip.AddrPortFrom(diagAddr(p[0], p[8:24]), binary.BigEndian.Uint16(p[4:])),
		remote: netip.AddrPortFrom(diagAddr(p[0], p[24:40]), binary.BigEndian.Uint16(p[6:])),
		cookie: binary.NativeEndian.Uint64(p[44:]),
		uid:    binary.NativeEndian.Uint32(p[64:]), inode: binary.NativeEndian.Uint32(p[68:]),
	}
	for a := p[diagMsgLen:]; len(a) >= 4; {
		n := int(binary.NativeEndian.Uint16(a[0:]))
		if n < 4 || n > len(a) {
			return socket{}, errTruncated
		}
		if binary.NativeEndian.Uint16(a[2:]) == inetDiagInfo && n-4 >= tcpInfoBytesLen {
			info := a[4:n]
			s.sent, s.received = binary.NativeEndian.Uint64(info[tcpInfoAcked:]), binary.NativeEndian.Uint64(info[tcpInfoReceived:])
			s.counted = true
		}
		a = a[min(align4(n), len(a)):]
	}
	return s, nil
}

// diagAddr reads an address field: IPv4 uses its first four bytes.
func diagAddr(family uint8, b []byte) netip.Addr {
	if family == afInet {
		return netip.AddrFrom4([4]byte(b[:4]))
	}
	return netip.AddrFrom16([16]byte(b)).Unmap()
}

func align4(n int) int { return (n + 3) &^ 3 }

// socketOwners maps the wanted socket inodes to the pids holding them. It rescans /proc/*/fd
// only when an inode is new, since sockets rarely change hands; another user's processes
// cannot be read without privileges, so their sockets stay unowned.
func (r *reader) socketOwners(want map[uint32]bool) map[uint32]int {
	fresh := false
	for ino := range want {
		if _, ok := r.owners[ino]; !ok {
			fresh = true
			break
		}
	}
	if fresh {
		r.owners = r.scanOwners()
		r.ownerScans++
	}
	out := map[uint32]int{}
	for ino := range want {
		if pid, ok := r.owners[ino]; ok && pid > 0 {
			out[ino] = pid
		}
	}
	for ino := range want { // remember unowned inodes too, so they do not cause rescans
		if _, ok := r.owners[ino]; !ok {
			r.owners[ino] = 0
		}
	}
	maps.DeleteFunc(r.owners, func(ino uint32, _ int) bool { return !want[ino] })
	return out
}

func (r *reader) scanOwners() map[uint32]int {
	out := map[uint32]int{}
	procs, _ := fs.ReadDir(r.fsys, "proc")
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		dir := "proc/" + p.Name() + "/fd"
		fds, _ := fs.ReadDir(r.fsys, dir)
		for _, fd := range fds {
			target, err := fs.ReadLink(r.fsys, dir+"/"+fd.Name())
			if ino, ok := socketInode(target); err == nil && ok {
				out[ino] = pid
			}
		}
	}
	return out
}

// socketInode reads the inode from a socket fd's link, "socket:[12345]".
func socketInode(link string) (uint32, bool) {
	s, ok := strings.CutPrefix(link, "socket:[")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(s, "]"), 10, 32)
	return uint32(n), err == nil
}
