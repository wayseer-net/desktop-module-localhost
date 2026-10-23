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
//	      connections: false    # network connections and listening sockets, on Linux
//	      connection_limit: 500 # remote addresses listed, busiest first
//	      connection_keep: 1m   # how long a remote stays listed after its connections close
//	      geoip:                # IP databases in MMDB format, such as DB-IP Lite or GeoLite2
//	        city: ~/.local/share/geoip/city.mmdb  # places remotes on Geo, with city and country
//	        asn: ~/.local/share/geoip/asn.mmdb    # names each remote's network, grouping them by it
//	      place: {lat: 52.37, lon: 4.89}  # where this machine is, for the host and its processes on Geo
//
// Units never seen running are left out; one that stops stays for keep_stopped, and failed ones
// stay. A unit that should be running (enabled, or wanted by an active target; not a oneshot, nor
// started by a timer or path when due) and stops without a stop being asked for is
// down, "should be running"; a stop asked for (a stop job listed, or systemd's journal line for
// one) leaves it unknown, "stopped", and a oneshot that ends is "finished". Other failed units
// are critical. Each change of a unit's state is an event on it. A machine without
// systemd, or a journal that cannot be read, shows as a note in Health rather than an error.
//
// With connections on, it reads TCP and UDP sockets over sock_diag, as ss does, and the process
// holding each from /proc/<pid>/fd. Each remote address is an entity, joined to the process
// talking to it by a talks_to edge that carries TCP's bytes per second; connections between
// local processes join them directly. Without privileges, other users' sockets show on the
// host. Listening sockets are an attribute, and a TCP listener opening or closing is an event.
// Public remotes are looked up in the geoip databases, which the user provides; each remote
// placed or named credits the database in location_source, as its license asks. With a place,
// the host and its processes are there, so their connections to placed remotes draw on Geo.
//
// Parsing is kept apart from reading so the parsers are tested on captured files.
package localhost
