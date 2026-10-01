package localhost

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"wayseer/pkg/sdk"
)

// TestDarwinLive reads this Mac: the host, its CPUs, and at least one filesystem and process,
// with this test's own process measured on the second read.
func TestDarwinLive(t *testing.T) {
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
	if count[sdk.KindHost] != 1 || count[KindCPU] == 0 || count[KindFilesystem] == 0 || count[sdk.KindProcess] == 0 {
		t.Fatalf("read %v; want a host, CPUs, a filesystem and processes", count)
	}
	if name := host.Attrs["os"].Str(); !strings.HasPrefix(name, "macOS ") {
		t.Errorf("os is %q", name)
	}
	if c := host.Attrs["cores"].Num(); int(c) != count[KindCPU] {
		t.Errorf("%v cores and %d CPUs", c, count[KindCPU])
	}
	if count[sdk.KindDisk] != 0 {
		t.Errorf("%d disks; macOS disks are not read", count[sdk.KindDisk])
	}
	self := ref(sdk.KindProcess, strconv.Itoa(os.Getpid()))
	if rss := latest(t, m, self, MetricRSS); rss < 1<<20 {
		t.Errorf("this test's resident memory is %v bytes", rss)
	}
	if cpu := latest(t, m, host.Ref, MetricCPU); cpu < 0 || cpu > 100 {
		t.Errorf("the host is %v%% busy", cpu)
	}
	if m.Health().Note != "" {
		t.Errorf("note %q; a Mac has no systemd to miss", m.Health().Note)
	}
}
