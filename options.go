package localhost

import (
	"fmt"
	"path/filepath"
	"time"
)

type options struct {
	Interval       time.Duration `yaml:"interval"`        // polling period
	History        time.Duration `yaml:"history"`         // how far back machine series are kept
	Processes      bool          `yaml:"processes"`       // list processes as entities
	ProcessHistory time.Duration `yaml:"process_history"` // how far back per-process series are kept
	Commands       bool          `yaml:"commands"`        // show command lines, which can hold secrets
	Root           string        `yaml:"root"`            // where proc, sys and etc are found
}

func defaults() options {
	return options{Interval: 2 * time.Second, History: time.Hour, Processes: true, ProcessHistory: 5 * time.Minute, Commands: true, Root: "/"}
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
	}
	o.Root = filepath.Clean(o.Root)
	return nil
}

// points is how many samples a history of d holds.
func (o *options) points(d time.Duration) int { return int(d / o.Interval) }
