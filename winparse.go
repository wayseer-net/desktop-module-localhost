package localhost

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Pure parsers for what Windows' calls return, kept apart from the calls so they are tested
// on every OS. Offsets are for 64-bit Windows; sys_windows.go checks them against x/sys.

// SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION, from NtQuerySystemInformation: idle, kernel (which
// includes idle) and user time per CPU.
const (
	procIdle, procKernel, procUser = 0, 8, 16
	procPerfSize                   = 48
)

// parseProcessorTimes reads one SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION per CPU.
func parseProcessorTimes(b []byte) ([]namedCPU, error) {
	if len(b) == 0 || len(b)%procPerfSize != 0 {
		return nil, fmt.Errorf("processor times are %d bytes, not a multiple of %d", len(b), procPerfSize)
	}
	var cpus []namedCPU
	for i := 0; i < len(b); i += procPerfSize {
		t := systemTimes(le.Uint64(b[i+procIdle:]), le.Uint64(b[i+procKernel:]), le.Uint64(b[i+procUser:]))
		cpus = append(cpus, namedCPU{"cpu" + strconv.Itoa(i/procPerfSize), t})
	}
	return cpus, nil
}

// systemTimes turns idle, kernel and user times, kernel including idle, into busy and total.
func systemTimes(idle, kernel, user uint64) cpuTimes {
	return cpuTimes{busy: kernel - min(idle, kernel) + user, total: kernel + user}
}

// MEMORYSTATUSEX, from GlobalMemoryStatusEx.
const (
	memTotalPhys, memAvailPhys = 8, 16
	memStatusSize              = 64
)

// parseMemoryStatus reads MEMORYSTATUSEX; available is what Task Manager calls Available.
func parseMemoryStatus(b []byte) (memInfo, error) {
	if len(b) < memStatusSize {
		return memInfo{}, fmt.Errorf("memory status is %d bytes, want %d", len(b), memStatusSize)
	}
	return memInfo{total: le.Uint64(b[memTotalPhys:]), available: le.Uint64(b[memAvailPhys:])}, nil
}

// parseDriveStrings reads GetLogicalDriveStringsW's list of roots, such as C:\.
func parseDriveStrings(b []uint16) []string {
	var out []string
	for _, root := range strings.Split(string(utf16.Decode(b)), "\x00") {
		if root != "" {
			out = append(out, root)
		}
	}
	return out
}

// MIB_IF_TABLE2 of MIB_IF_ROW2, from GetIfTable2Ex.
const (
	ifTable2Rows                    = 8 // the rows follow the count, 8-byte aligned
	ifRow2Index                     = 8
	ifRow2InOctets, ifRow2OutOctets = 1208, 1280
	ifRow2Size                      = 1352
)

// parseIfTable reads GetIfTable2Ex's table into byte counters by interface index.
func parseIfTable(b []byte) (map[int]netCounters, error) {
	if len(b) < ifTable2Rows {
		return nil, fmt.Errorf("interface table is %d bytes, too few for its count", len(b))
	}
	n := int(le.Uint32(b))
	if rows := (len(b) - ifTable2Rows) / ifRow2Size; n > rows {
		return nil, fmt.Errorf("interface table counts %d rows and holds %d", n, rows)
	}
	out := make(map[int]netCounters, n)
	for i := range n {
		r := b[ifTable2Rows+i*ifRow2Size:]
		out[int(le.Uint32(r[ifRow2Index:]))] = netCounters{rx: le.Uint64(r[ifRow2InOctets:]), tx: le.Uint64(r[ifRow2OutOctets:])}
	}
	return out, nil
}

// firstWindows11 is the first build of Windows 11, which the registry still calls Windows 10.
const firstWindows11 = 22000

// windowsName is the edition and release, as Settings shows them, from the registry's
// ProductName and DisplayVersion.
func windowsName(product, display string, build uint32) string {
	if product == "" {
		return "Windows"
	}
	if rest, ok := strings.CutPrefix(product, "Windows 10 "); ok && build >= firstWindows11 {
		product = "Windows 11 " + rest
	}
	return strings.TrimSpace(product + " " + display)
}
