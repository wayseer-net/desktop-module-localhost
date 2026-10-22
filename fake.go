package localhost

import "net/netip"

// Fake is a module for tests elsewhere: it reads the machine under its root option, as a
// fixture, with every filesystem 100 GiB and 40 used, no services or journal, and made-up
// sockets that the fixture's tmux and bash hold.
func Fake() *Module {
	m := New()
	m.statfs = func(string) (fsUsage, error) { return fsUsage{total: 100 << 30, used: 40 << 30, avail: 60 << 30}, nil }
	m.addrs, m.system = nil, nil
	m.sockets = fakeSockets()
	return m
}

// fakeSockets is tmux fetching from a documentation address and bash serving on loopback,
// with byte counts that grow on every read.
func fakeSockets() func() ([]socket, string, error) {
	var reads uint64
	return func() ([]socket, string, error) {
		reads++
		return []socket{
			{
				proto: "tcp", state: tcpEstablished, local: netip.MustParseAddrPort("192.0.2.10:50412"), remote: netip.MustParseAddrPort("198.51.100.7:443"),
				inode: 4711, cookie: 1, sent: 2000 * reads, received: 50000 * reads, counted: true,
			},
			{
				proto: "tcp", state: tcpListen, local: netip.MustParseAddrPort("127.0.0.1:8080"), remote: netip.AddrPortFrom(netip.IPv4Unspecified(), 0),
				inode: 4712, cookie: 2,
			},
		}, "", nil
	}
}
