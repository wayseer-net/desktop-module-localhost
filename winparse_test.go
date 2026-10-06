package localhost

import (
	"maps"
	"slices"
	"testing"
	"unicode/utf16"
)

func processorRecord(idle, kernel, user uint64) macBytes {
	return make(macBytes, procPerfSize).put(procIdle, idle).put(procKernel, kernel).put(procUser, user)
}

func TestWindowsParseProcessorTimes(t *testing.T) {
	b := append(processorRecord(900, 1000, 100), processorRecord(500, 600, 400)...)
	cpus, err := parseProcessorTimes(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []namedCPU{{"cpu0", cpuTimes{busy: 200, total: 1100}}, {"cpu1", cpuTimes{busy: 500, total: 1000}}}
	if !slices.Equal(cpus, want) {
		t.Errorf("cpus %v; want %v", cpus, want)
	}
	for _, bad := range [][]byte{nil, b[:50]} {
		if _, err := parseProcessorTimes(bad); err == nil {
			t.Errorf("%d bytes of processor times parsed", len(bad))
		}
	}
}

func TestWindowsSystemTimes(t *testing.T) {
	// kernel time includes idle time
	if got := systemTimes(3600, 4000, 400); got != (cpuTimes{busy: 800, total: 4400}) {
		t.Errorf("systemTimes = %+v", got)
	}
	if got := systemTimes(5000, 4000, 400); got != (cpuTimes{busy: 400, total: 4400}) {
		t.Errorf("idle beyond kernel: systemTimes = %+v", got)
	}
}

func TestWindowsParseMemoryStatus(t *testing.T) {
	b := make(macBytes, memStatusSize).put(0, uint32(memStatusSize)).put(memTotalPhys, uint64(16<<30)).put(memAvailPhys, uint64(6<<30))
	m, err := parseMemoryStatus(b)
	if err != nil || m != (memInfo{total: 16 << 30, available: 6 << 30}) {
		t.Errorf("memory %+v, %v", m, err)
	}
	if _, err := parseMemoryStatus(b[:32]); err == nil {
		t.Error("a short MEMORYSTATUSEX parsed")
	}
}

func TestWindowsParseDriveStrings(t *testing.T) {
	b := utf16.Encode([]rune("C:\\\x00D:\\\x00\x00"))
	if got := parseDriveStrings(b); !slices.Equal(got, []string{`C:\`, `D:\`}) {
		t.Errorf("drives %q", got)
	}
	if got := parseDriveStrings(nil); got != nil {
		t.Errorf("no drives: %q", got)
	}
}

func ifRow(index uint32, in, out uint64) macBytes {
	return make(macBytes, ifRow2Size).put(ifRow2Index, index).put(ifRow2InOctets, in).put(ifRow2OutOctets, out)
}

func ifTable(rows ...macBytes) []byte {
	b := make(macBytes, ifTable2Rows).put(0, uint32(len(rows)))
	for _, r := range rows {
		b = append(b, r...)
	}
	return b
}

func TestWindowsParseIfTable(t *testing.T) {
	b := ifTable(ifRow(7, 1000, 500), ifRow(12, 0, 0))
	got, err := parseIfTable(b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]netCounters{7: {rx: 1000, tx: 500}, 12: {}}
	if !maps.Equal(got, want) {
		t.Errorf("counters %v; want %v", got, want)
	}
	if _, err := parseIfTable(b[:len(b)-1]); err == nil {
		t.Error("a table shorter than its count parsed")
	}
	if _, err := parseIfTable(b[:3]); err == nil {
		t.Error("a table without its count parsed")
	}
}

func TestWindowsName(t *testing.T) {
	for _, c := range []struct {
		product, display string
		build            uint32
		want             string
	}{
		// Windows 11 still names itself Windows 10 in the registry.
		{"Windows 10 Pro", "24H2", 26100, "Windows 11 Pro 24H2"},
		{"Windows 10 Pro", "22H2", 19045, "Windows 10 Pro 22H2"},
		{"Windows Server 2022 Datacenter", "", 20348, "Windows Server 2022 Datacenter"},
		{"", "", 26100, "Windows"},
	} {
		if got := windowsName(c.product, c.display, c.build); got != c.want {
			t.Errorf("windowsName(%q, %q, %d) = %q; want %q", c.product, c.display, c.build, got, c.want)
		}
	}
}
