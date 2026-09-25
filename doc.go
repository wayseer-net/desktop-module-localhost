// Package localhost is the `localhost` module: the machine Mind's Eye runs on, read from /proc
// and /sys. It lists the host, its CPUs, memory, disks, filesystems, network interfaces and
// processes, with series for CPU, memory, disk and network use. Linux only for now; other
// platforms build but refuse to configure unless root points at a copied Linux tree.
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
//
// Parsing is kept apart from reading so the parsers are tested on captured files.
package localhost
