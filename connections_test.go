package localhost

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"wayseer.dev/sdk"
)

// connModule reads the fixture tree with sockets from *socks, where the fixture's fd links
// give inode 4711 to pid 1200 (tmux) and 4712 to pid 1201 (bash).
func connModule(t *testing.T, socks *[]socket, extra string) *Module {
	t.Helper()
	m := New()
	m.statfs = func(string) (fsUsage, error) { return fsUsage{total: 100 << 30, used: 40 << 30, avail: 60 << 30}, nil }
	m.addrs, m.system = nil, nil
	m.sockets = func() ([]socket, string, error) { return slices.Clone(*socks), "", nil }
	configure(t, m, "root: "+copyFixture(t)+"\nconnections: true\n"+extra)
	return m
}

func pollConns(m *Module, at time.Time) *sdk.ChangeSet { return m.poll(context.Background(), at) }

var errNoSockets = errors.New("netlink refused")

var (
	t0 = time.Unix(1_800_000_000, 0)
	t1 = t0.Add(2 * time.Second)
)

func TestConnectionsShowRemotesWithTraffic(t *testing.T) {
	socks := []socket{
		tcp(1, 4711, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 1000, 2000),
		tcp(2, 4712, tcpListen, "127.0.0.1:8080", "", 0, 0),
	}
	m := connModule(t, &socks, "")
	pollConns(m, t0)
	socks[0].sent, socks[0].received = 3000, 10000
	pollConns(m, t1)

	r := ref(KindRemote, "93.184.216.34")
	e, ok := m.world.ents[r]
	if !ok {
		t.Fatal("remote not listed")
	}
	if e.Attrs[AttrGroup].Str() != "network" || e.Attrs["scope"].Str() != "public" {
		t.Errorf("remote attrs %v", e.Attrs)
	}
	edge, ok := m.world.edges[sdk.EdgeKey{From: ref(sdk.KindProcess, "1200"), To: r, Rel: sdk.RelTalksTo}]
	if !ok {
		t.Fatalf("no edge from tmux; edges %v", m.world.edges)
	}
	if edge.Traffic != (sdk.Traffic{Rate: 5000, Unit: sdk.TrafficBytes}) {
		t.Errorf("traffic %+v", edge.Traffic)
	}
	if got := m.world.ents[ref(sdk.KindProcess, "1201")].Attrs["listens"]; got.Type() != sdk.TypeList || got.List()[0].Str() != "tcp 127.0.0.1:8080" {
		t.Errorf("bash listens %v", got)
	}
	if h := m.series[sdk.SeriesRef{Entity: r, Metric: MetricNetReceive}]; h == nil || h.Len() != 1 {
		t.Errorf("remote receive series %v", h)
	}
}

func TestConnectionsOffByDefault(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	called := false
	m.sockets = func() ([]socket, string, error) { called = true; return nil, "", nil }
	pollConns(m, t0)
	for r := range m.world.ents {
		if r.Kind() == KindRemote {
			t.Fatal("remote listed with connections off")
		}
	}
	if called {
		t.Error("sockets read with connections off")
	}
}

func TestConnectionsKeepClosedRemotes(t *testing.T) {
	socks := []socket{tcp(1, 4711, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 0, 0)}
	m := connModule(t, &socks, "connection_keep: 10s\n")
	pollConns(m, t0)
	socks = nil
	pollConns(m, t1)
	r := ref(KindRemote, "93.184.216.34")
	if _, ok := m.world.ents[r]; !ok {
		t.Fatal("closed remote dropped before connection_keep")
	}
	pollConns(m, t0.Add(11*time.Second))
	if _, ok := m.world.ents[r]; ok {
		t.Error("closed remote kept after connection_keep")
	}
}

func TestConnectionsLimitKeepsBusiest(t *testing.T) {
	socks := []socket{
		tcp(1, 4711, tcpEstablished, "192.168.1.10:1", "198.51.100.1:443", 0, 0),
		tcp(2, 4711, tcpEstablished, "192.168.1.10:2", "198.51.100.2:443", 0, 0),
		tcp(3, 4711, tcpEstablished, "192.168.1.10:3", "198.51.100.3:443", 0, 0),
	}
	m := connModule(t, &socks, "connection_limit: 2\n")
	pollConns(m, t0)
	socks[1].received, socks[2].received = 100, 900
	pollConns(m, t1)
	var got []string
	for r := range m.world.ents {
		if r.Kind() == KindRemote {
			got = append(got, r.Native())
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"198.51.100.2", "198.51.100.3"}) {
		t.Errorf("remotes %v", got)
	}
}

func TestConnectionsListenEvents(t *testing.T) {
	socks := []socket{tcp(1, 4712, tcpListen, "127.0.0.1:8080", "", 0, 0)}
	m := connModule(t, &socks, "")
	if cs := pollConns(m, t0); len(cs.Events) != 0 {
		t.Fatalf("first poll raised %v", cs.Events)
	}
	socks = append(socks, tcp(2, 4711, tcpListen, "0.0.0.0:8000", "", 0, 0))
	cs := pollConns(m, t1)
	if len(cs.Events) != 1 || cs.Events[0].Severity != sdk.SevWarn || cs.Events[0].Entity != ref(sdk.KindProcess, "1200") ||
		!strings.Contains(cs.Events[0].Message, "all interfaces") {
		t.Fatalf("events %+v", cs.Events)
	}
	socks = socks[1:]
	cs = pollConns(m, t1.Add(2*time.Second))
	if len(cs.Events) != 1 || cs.Events[0].Severity != sdk.SevInfo || !strings.Contains(cs.Events[0].Message, "stopped listening on tcp 127.0.0.1:8080") {
		t.Fatalf("events %+v", cs.Events)
	}
}

func TestConnectionsNoteWhenUnreadable(t *testing.T) {
	var socks []socket
	m := connModule(t, &socks, "")
	m.reader.(*reader).sockets = func() ([]socket, string, error) { return nil, "", errNoSockets }
	pollConns(m, t0)
	if h := m.Health(); h.Err != nil || !strings.Contains(h.Note, "connections") {
		t.Errorf("health %+v", h)
	}
}

func TestFakeShowsMadeUpConnections(t *testing.T) {
	m := Fake()
	configure(t, m, "root: "+copyFixture(t)+"\nconnections: true\n")
	pollConns(m, t0)
	pollConns(m, t1)
	e, ok := m.world.edges[sdk.EdgeKey{From: ref(sdk.KindProcess, "1200"), To: ref(KindRemote, "198.51.100.7"), Rel: sdk.RelTalksTo}]
	if !ok || e.Traffic.Rate != 26000 {
		t.Fatalf("edge %+v, found %v", e, ok)
	}
}
