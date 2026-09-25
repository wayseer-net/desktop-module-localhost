// Package localhost is the `localhost` module: the machine Mind's Eye runs on, read from /proc
// and /sys. It lists the host, its CPUs, memory, disks, filesystems, network interfaces and
// processes, with series for CPU, memory, disk and network use. Where the machine runs
// systemd it adds the running system units (over D-Bus) with their dependencies, and journal
// entries as events (from journalctl). Linux only for now; other platforms build but refuse to
// configure unless root points at a copied Linux tree, where systemd and the journal are off.
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
//
// Units that are simply stopped are left out; failed ones stay, as critical. A machine without
// systemd, or a journal that cannot be read, shows as a note in Health rather than an error.
// Parsing is kept apart from reading so the parsers are tested on captured files.
package localhost
