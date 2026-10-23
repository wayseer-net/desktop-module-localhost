package localhost

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"wayseer.dev/sdk"
)

type options struct {
	Interval       time.Duration `yaml:"interval"`        // how often the machine is read; default 2s
	History        time.Duration `yaml:"history"`         // how far back machine series are kept, 10 intervals to 24h; default 1h
	Processes      bool          `yaml:"processes"`       // list processes as entities; default true
	ProcessHistory time.Duration `yaml:"process_history"` // how far back per-process series are kept, up to history; default 5m
	Commands       bool          `yaml:"commands"`        // show command lines, which can hold secrets; default true
	Root           string        `yaml:"root"`            // where proc, sys and etc are found, such as a container's mount of the host; default /
	Units          []string      `yaml:"units"`           // systemd unit types listed, [] for none; default service, socket, timer, target, path
	Journal        string        `yaml:"journal"`         // least severe journal priority sent, emerg to debug, or off; default warning
	JournalBacklog int           `yaml:"journal_backlog"` // journal entries sent from before the start, up to 1000; default 100
	KeepStopped    time.Duration `yaml:"keep_stopped"`    // how long a unit that stopped stays listed, up to 24h; default 1h

	Connections     bool          `yaml:"connections"`      // list network connections and listening sockets, on Linux; default false
	ConnectionLimit int           `yaml:"connection_limit"` // remote addresses listed, busiest first, 1 to 5000; default 500
	ConnectionKeep  time.Duration `yaml:"connection_keep"`  // how long a remote stays listed after its last connection closes, up to 1h; default 1m
	GeoIP           geoipOptions  `yaml:"geoip"`            // IP databases that place and name public remote addresses; default none

	Place placeOptions `yaml:"place"` // where this machine is, which the host and its processes take on the map; default none

	priority int // Journal parsed; -1 when off
}

// maxConnections is the most remotes connection_limit allows.
const maxConnections = 5000

// geoipOptions are paths to databases in MMDB format, such as DB-IP Lite or MaxMind GeoLite2.
type geoipOptions struct {
	City string `yaml:"city"` // places an address and names its city and country
	ASN  string `yaml:"asn"`  // names the network an address is in
}

// placeOptions are a latitude and longitude in degrees; both or neither are given.
type placeOptions struct {
	Lat *float32 `yaml:"lat"` // degrees north, -90 to 90
	Lon *float32 `yaml:"lon"` // degrees east, -180 to 180
}

// place is where the options put the machine, or no place.
func (p placeOptions) place() sdk.Place {
	if p.Lat == nil || p.Lon == nil {
		return sdk.Place{}
	}
	return sdk.At(*p.Lat, *p.Lon)
}

func (p placeOptions) validate() error {
	switch {
	case p.Lat == nil && p.Lon == nil:
		return nil
	case p.Lat == nil || p.Lon == nil:
		return errors.New("place needs both lat and lon")
	case *p.Lat < -90 || *p.Lat > 90:
		return fmt.Errorf("place lat %v must be between -90 and 90", *p.Lat)
	case *p.Lon < -180 || *p.Lon > 180:
		return fmt.Errorf("place lon %v must be between -180 and 180", *p.Lon)
	}
	return nil
}

var unitTypes = []string{"service", "socket", "timer", "target", "path", "mount", "automount", "swap", "device", "slice", "scope"}

func defaults() options {
	return options{
		Interval: 2 * time.Second, History: time.Hour, Processes: true, ProcessHistory: 5 * time.Minute, Commands: true, Root: "/",
		Units: []string{"service", "socket", "timer", "target", "path"}, Journal: "warning", JournalBacklog: 100,
		KeepStopped: time.Hour, ConnectionLimit: 500, ConnectionKeep: time.Minute,
	}
}

func (o *options) validate() error {
	switch {
	case o.Interval < 100*time.Millisecond:
		return fmt.Errorf("interval %v is below 100ms", o.Interval)
	case o.History < 10*o.Interval || o.History > 24*time.Hour:
		return fmt.Errorf("history %v must be at least 10 intervals and at most 24h", o.History)
	case o.ProcessHistory < o.Interval || o.ProcessHistory > o.History:
		return fmt.Errorf("process_history %v must be between the interval and history", o.ProcessHistory)
	case o.Root != "/" && !filepath.IsAbs(o.Root): // "/" is this machine's root on every platform
		return fmt.Errorf("root %q must be an absolute path", o.Root)
	case o.JournalBacklog < 0 || o.JournalBacklog > 1000:
		return fmt.Errorf("journal_backlog %d must be between 0 and 1000", o.JournalBacklog)
	case o.KeepStopped < 0 || o.KeepStopped > 24*time.Hour:
		return fmt.Errorf("keep_stopped %v must be between 0 and 24h", o.KeepStopped)
	}
	for _, t := range o.Units {
		if !slices.Contains(unitTypes, t) {
			return fmt.Errorf("unit type %q is not one of %s", t, strings.Join(unitTypes, ", "))
		}
	}
	if o.Root != "/" {
		o.Root = filepath.Clean(o.Root)
	}
	return cmp.Or(o.validateConnections(), o.Place.validate(), o.parseJournal())
}

func (o *options) validateConnections() error {
	switch {
	case o.ConnectionLimit < 1 || o.ConnectionLimit > maxConnections:
		return fmt.Errorf("connection_limit %d must be between 1 and %d", o.ConnectionLimit, maxConnections)
	case o.ConnectionKeep < 0 || o.ConnectionKeep > time.Hour:
		return fmt.Errorf("connection_keep %v must be between 0 and 1h", o.ConnectionKeep)
	case !o.Connections && o.GeoIP != geoipOptions{}:
		return errGeoNeedsConnections
	}
	for _, p := range []*string{&o.GeoIP.City, &o.GeoIP.ASN} {
		if *p = expandHome(*p); *p != "" && !filepath.IsAbs(*p) {
			return fmt.Errorf("geoip path %q must be absolute or start with ~/", *p)
		}
	}
	return nil
}

func (o *options) parseJournal() error {
	if o.Journal == "off" {
		o.priority = -1
		return nil
	}
	p, err := parsePriority(o.Journal)
	o.priority = p
	return err
}

// expandHome replaces a leading ~/ with the home directory.
func expandHome(p string) string {
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, rest)
	}
	return p
}

// points is how many samples a history of d holds.
func (o *options) points(d time.Duration) int { return int(d / o.Interval) }
