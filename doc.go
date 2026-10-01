// Package localhost is the `localhost` module: the machine Wayseer runs on, read from /proc
// and /sys, or on macOS from sysctls and Mach calls (no disks, units or journal). It lists the host, its CPUs, memory, disks, filesystems, network interfaces and
// processes, with series for CPU, memory, disk and network use. Where the machine runs
// systemd it adds the running system units (over D-Bus) with their dependencies, and journal
// entries as events (from journalctl). Other platforms build but refuse to configure unless
// root points at a copied Linux tree, where systemd and the journal are off.
//
//	modules:
//	  - kind: localhost
//	    name: local
//	    options:
//	      interval: 2s          # polling period
//	      history: 1h           # how far back machine series are kept
//	      processes: true       # list processes as entities
//	      process_history: 5m   # how far back per-process series are kept
//	      commands: true        # show command lines, which can hold secrets passed as arguments
//	      root: /               # where proc, sys and etc are found, e.g. /host in a container
//	      units: [service, socket, timer, target, path]  # unit types listed; [] for none
//	      journal: warning      # least severe priority sent as events (emerg ... debug), or off
//	      journal_backlog: 100  # entries from before the start sent first
//	      keep_stopped: 1h      # how long a unit that stopped stays listed; 0 for not at all
//
// Units never seen running are left out; one that stops stays for keep_stopped, and failed ones
// stay. A unit that should be running (enabled, or wanted by an active target; not a oneshot, nor
// started by a timer or path when due) and stops without a stop being asked for is
// down, "should be running"; a stop asked for (a stop job listed, or systemd's journal line for
// one) leaves it unknown, "stopped", and a oneshot that ends is "finished". Other failed units
// are critical. Each change of a unit's state is an event on it. A machine without
// systemd, or a journal that cannot be read, shows as a note in Health rather than an error.
// Parsing is kept apart from reading so the parsers are tested on captured files.
package localhost
