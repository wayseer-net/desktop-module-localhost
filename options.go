package localhost

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type options struct {
	Interval       time.Duration `yaml:"interval"`        // polling period
	History        time.Duration `yaml:"history"`         // how far back machine series are kept
	Processes      bool          `yaml:"processes"`       // list processes as entities
	ProcessHistory time.Duration `yaml:"process_history"` // how far back per-process series are kept
	Commands       bool          `yaml:"commands"`        // show command lines, which can hold secrets
	Root           string        `yaml:"root"`            // where proc, sys and etc are found
	Units          []string      `yaml:"units"`           // systemd unit types listed; [] for none
	Journal        string        `yaml:"journal"`         // least severe journal priority sent, or off
	JournalBacklog int           `yaml:"journal_backlog"` // journal entries sent from before the start

	priority int // Journal parsed; -1 when off
}

var unitTypes = []string{"service", "socket", "timer", "target", "path", "mount", "automount", "swap", "device", "slice", "scope"}

func defaults() options {
	return options{
		Interval: 2 * time.Second, History: time.Hour, Processes: true, ProcessHistory: 5 * time.Minute, Commands: true, Root: "/",
		Units: []string{"service", "socket", "timer", "target", "path"}, Journal: "warning", JournalBacklog: 100,
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
	case !filepath.IsAbs(o.Root):
		return fmt.Errorf("root %q must be an absolute path", o.Root)
	case o.JournalBacklog < 0 || o.JournalBacklog > 1000:
		return fmt.Errorf("journal_backlog %d must be between 0 and 1000", o.JournalBacklog)
	}
	for _, t := range o.Units {
		if !slices.Contains(unitTypes, t) {
			return fmt.Errorf("unit type %q is not one of %s", t, strings.Join(unitTypes, ", "))
		}
	}
	o.Root = filepath.Clean(o.Root)
	return o.parseJournal()
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

// points is how many samples a history of d holds.
func (o *options) points(d time.Duration) int { return int(d / o.Interval) }
