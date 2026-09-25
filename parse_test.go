package localhost

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const fixture = "../../testdata/procfs"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixture, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseStat(t *testing.T) {
	s, err := parseStat(readFixture(t, "proc/stat"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (cpuTimes{busy: 5400, total: 95900}); s.total != want {
		t.Errorf("total = %+v; want %+v (idle and iowait are not busy)", s.total, want)
	}
	if len(s.cpus) != 4 || s.cpus[3].name != "cpu3" || s.cpus[3].times.total != 95900/4 {
		t.Errorf("cpus = %+v", s.cpus)
	}
	if !s.boot.Equal(time.Unix(1790302804, 0)) {
		t.Errorf("boot = %v", s.boot)
	}
}

func TestParseStatRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "cpu  1 2 x\n", "btime 1\n"} {
		if _, err := parseStat([]byte(in)); err == nil {
			t.Errorf("parseStat(%q) succeeded", in)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo(readFixture(t, "proc/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	want := memInfo{total: 16384000 << 10, available: 8192000 << 10, swapTotal: 4194304 << 10, swapFree: 3145728 << 10}
	if m != want {
		t.Errorf("meminfo = %+v; want %+v", m, want)
	}
	if _, err := parseMeminfo([]byte("MemFree: 1 kB\n")); err == nil {
		t.Error("meminfo without MemTotal accepted")
	}
}

func TestParseDiskstats(t *testing.T) {
	d, err := parseDiskstats(readFixture(t, "proc/diskstats"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d["nvme0n1"], (ioCounters{read: 21638309 * 512, written: 61380677 * 512, busy: 224120 * time.Millisecond}); got != want {
		t.Errorf("nvme0n1 = %+v; want %+v", got, want)
	}
	if len(d) != 6 {
		t.Errorf("devices = %v", slices.Sorted(maps.Keys(d)))
	}
}

func TestParseNetDev(t *testing.T) {
	n, err := parseNetDev(readFixture(t, "proc/net/dev"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n["wlan0"], (netCounters{rx: 4926113947, tx: 245141713}); got != want {
		t.Errorf("wlan0 = %+v; want %+v", got, want)
	}
	if len(n) != 3 {
		t.Errorf("interfaces = %v", slices.Sorted(maps.Keys(n)))
	}
}

func TestParseMountinfo(t *testing.T) {
	ms, err := parseMountinfo(readFixture(t, "proc/self/mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 8 {
		t.Fatalf("got %d mounts", len(ms))
	}
	if want := (mount{point: "/home", fstype: "btrfs", device: "/dev/mapper/luks-0000"}); ms[3] != want {
		t.Errorf("mount 3 = %+v; want %+v", ms[3], want)
	}
	if ms[7].point != "/media/usb stick" {
		t.Errorf("escaped mount point = %q", ms[7].point)
	}
}

func TestParsePidStat(t *testing.T) {
	p, err := parsePidStat(readFixture(t, "proc/1200/stat"))
	if err != nil {
		t.Fatal(err)
	}
	want := pidStat{pid: 1200, ppid: 1, comm: "tmux: server", cpu: 30, start: 5000, rss: 2000}
	if p != want {
		t.Errorf("stat = %+v; want %+v", p, want)
	}
	k, _ := parsePidStat(readFixture(t, "proc/3/stat"))
	if !k.kernel {
		t.Error("rcu_gp not seen as a kernel thread")
	}
	if _, err := parsePidStat([]byte("12 (x) S 1")); err == nil {
		t.Error("short stat accepted")
	}
}

func TestParseProcessDetails(t *testing.T) {
	if got := parseCmdline(readFixture(t, "proc/1200/cmdline")); got != "tmux new-session -d" {
		t.Errorf("cmdline = %q", got)
	}
	if uid, ok := parseUID(readFixture(t, "proc/1201/status")); !ok || uid != 1000 {
		t.Errorf("uid = %d, %v", uid, ok)
	}
	users := parsePasswd(readFixture(t, "etc/passwd"))
	if users[0] != "root" || users[1000] != "alice" {
		t.Errorf("users = %v", users)
	}
	if got := parseOSRelease(readFixture(t, "etc/os-release")); got != "Testix Linux 1.0" {
		t.Errorf("os = %q", got)
	}
}
