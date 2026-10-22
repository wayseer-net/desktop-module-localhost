package localhost

import (
	"io"
	"net"
	"net/netip"
	"testing"
)

// TestLiveSockets finds a loopback connection this test makes, with the bytes it sent.
func TestLiveSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		if c, err := ln.Accept(); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(c, 5000))
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Write(make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	<-accepted
	socks, _, err := liveSockets()
	if err != nil {
		t.Fatal(err)
	}
	dialed, listening := findSockets(socks, netip.MustParseAddrPort(c.LocalAddr().String()), netip.MustParseAddrPort(ln.Addr().String()))
	if dialed == nil || !listening {
		t.Fatalf("found dialed %v, listening %v among %d sockets", dialed, listening, len(socks))
	}
	if dialed.state != tcpEstablished || dialed.inode == 0 || !dialed.counted || dialed.sent < 5000 {
		t.Errorf("dialed socket %+v, want established, an inode and 5000 bytes sent", *dialed)
	}
}

func findSockets(socks []socket, local, listen netip.AddrPort) (dialed *socket, listening bool) {
	for i, s := range socks {
		switch {
		case s.local == local && s.remote == listen:
			dialed = &socks[i]
		case s.local == listen && s.state == tcpListen:
			listening = true
		}
	}
	return dialed, listening
}
