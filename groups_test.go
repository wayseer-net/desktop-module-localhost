package localhost

import (
	"fmt"
	"testing"

	"wayseer.dev/sdk"
)

// TestEntitiesAreGroupedForTopology groups the machine's parts as hardware, a unit with several
// processes with them, units with few together with theirs as services, the idle units as
// systemd, and processes in no unit as processes.
func TestEntitiesAreGroupedForTopology(t *testing.T) {
	m := New()
	m.name = "local"
	s := &sample{
		host:  hostInfo{name: "desk"},
		disks: []disk{{name: "nvme0n1"}},
		nics:  []nic{{name: "eth0", state: "up"}},
		units: []unit{
			{unitReply: unitReply{Name: "user@1000.service", ActiveState: "active"}},
			{unitReply: unitReply{Name: "sshd.service", ActiveState: "active"}},
			{unitReply: unitReply{Name: "timers.target", ActiveState: "active"}},
		},
	}
	s.stat.cpus = []namedCPU{{name: "cpu0"}}
	s.procs = append(s.procs, process{pidStat: pidStat{pid: 1, comm: "init"}})
	s.procs = append(s.procs, process{pidStat: pidStat{pid: 2, ppid: 1, comm: "sshd"}, cgroup: "/system.slice/sshd.service"})
	for pid := 10; pid < 10+busyUnit; pid++ {
		s.procs = append(s.procs, process{pidStat: pidStat{pid: pid, ppid: 1, comm: fmt.Sprint("p", pid)}, cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice"})
	}
	w := m.buildWorld(s)
	for r, want := range map[sdk.EntityRef]string{
		w.host:                                    "hardware",
		ref(KindCPU, "cpu0"):                      "hardware",
		ref(KindMemory, "memory"):                 "hardware",
		ref(sdk.KindDisk, "nvme0n1"):              "hardware",
		ref(sdk.KindInterface, "eth0"):            "hardware",
		ref(sdk.KindService, "user@1000.service"): "user@1000.service",
		ref(sdk.KindProcess, "10"):                "user@1000.service",
		ref(sdk.KindService, "sshd.service"):      "services",
		ref(sdk.KindProcess, "2"):                 "services",
		ref(KindUnit, "timers.target"):            "systemd",
		ref(sdk.KindProcess, "1"):                 "processes",
	} {
		if got := w.ents[r].Attrs[AttrGroup].Str(); got != want {
			t.Errorf("%s is in group %q; want %q", r, got, want)
		}
	}
}
