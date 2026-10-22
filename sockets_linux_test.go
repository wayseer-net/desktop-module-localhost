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
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(c, 5000))
		}
		accepted <- c
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	if s := <-accepted; s != nil {
		defer s.Close()
	}
	socks, _, err := liveSockets()
	if err != nil {
		t.Fatal(err)
	}
	local := netip.MustParseAddrPort(c.LocalAddr().String())
	listen := netip.MustParseAddrPort(ln.Addr().String())
	var dialed, listening bool
	for _, s := range socks {
		switch {
		case s.local == local && s.remote == listen:
			dialed = true
			if s.state != tcpEstablished || s.inode == 0 || !s.counted || s.sent < 5000 {
				t.Errorf("dialed socket %+v, want established, an inode and 5000 bytes sent", s)
			}
		case s.local == listen && s.state == tcpListen:
			listening = true
		}
	}
	if !dialed || !listening {
		t.Errorf("found dialed %v, listening %v among %d sockets", dialed, listening, len(socks))
	}
}
