package localhost

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// recvBuf holds one netlink datagram; the kernel fills dumps a page or so at a time.
const recvBuf = 1 << 16

// liveSockets dumps this machine's TCP and UDP sockets over sock_diag. A protocol the kernel
// cannot dump, such as UDP without udp_diag, is skipped and named in the note.
func liveSockets() (out []socket, note string, err error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, "", fmt.Errorf("sock_diag: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, recvBuf)
	seq := uint32(0)
	for _, d := range []struct {
		proto  string
		number uint8
	}{{"tcp", protoTCP}, {"udp", protoUDP}} {
		for _, family := range []uint8{afInet, afInet6} {
			seq++
			got, err := dump(fd, buf, diagRequest(family, d.number, seq), d.proto)
			var de diagError
			if errors.As(err, &de) && de.errno == errnoNoEnt {
				note = "the kernel cannot list " + d.proto + " sockets"
				continue
			}
			if err != nil {
				return nil, "", err
			}
			out = append(out, got...)
		}
	}
	return out, note, nil
}

func dump(fd int, buf, req []byte, proto string) ([]socket, error) {
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sock_diag: %w", err)
	}
	var out []socket
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			return nil, fmt.Errorf("sock_diag: %w", err)
		}
		got, done, err := parseDiag(buf[:n], proto)
		out = append(out, got...)
		if err != nil || done {
			return out, err
		}
	}
}

var socketSource = liveSockets
