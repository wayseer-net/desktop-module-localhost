package localhost

import (
	"strings"
	"testing"
	"time"

	"wayseer.dev/sdk"
)

// TestWindowsLive reads this Windows machine: the host, its CPUs, memory, a volume and an
// interface, with the host's CPU use on the second read.
func TestWindowsLive(t *testing.T) {
	m := New()
	configure(t, m, "interval: 1s")
	m.poll(t.Context(), time.Now())
	time.Sleep(200 * time.Millisecond)
	m.poll(t.Context(), time.Now())
	if err := m.Health().Err; err != nil {
		t.Fatal(err)
	}
	count := map[sdk.Kind]int{}
	var host sdk.Entity
	for _, e := range m.world.ents {
		count[e.Kind]++
		if e.Kind == sdk.KindHost {
			host = e
		}
	}
	t.Logf("%v", count)
	if count[sdk.KindHost] != 1 || count[KindCPU] == 0 || count[KindMemory] != 1 || count[KindFilesystem] == 0 || count[sdk.KindInterface] == 0 {
		t.Fatalf("read %v; want a host, CPUs, memory, a volume and an interface", count)
	}
	if name := host.Attrs["os"].Str(); !strings.HasPrefix(name, "Windows") {
		t.Errorf("os is %q", name)
	}
	if cpu := latest(t, m, host.Ref, MetricCPU); cpu < 0 || cpu > 100 {
		t.Errorf("the host is %v%% busy", cpu)
	}
	if mem := latest(t, m, host.Ref, MetricMemUtil); mem <= 0 || mem > 100 {
		t.Errorf("memory is %v%% used", mem)
	}
}
