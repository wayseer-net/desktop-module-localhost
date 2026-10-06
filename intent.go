package localhost

import (
	"slices"
	"strings"

	"wayseer.dev/sdk"
)

var (
	shouldRun = sdk.Status{Level: sdk.StatusDown, Reason: "should be running"}
	finished  = sdk.Status{Level: sdk.StatusUnknown, Reason: "finished"}
)

// judge decides, for each listed unit, whether it should be running and whether a stop was asked.
func judge(listed []unit, stops map[string]stopNote) []unit {
	for i := range listed {
		listed[i] = judged(listed[i], listed, stops)
	}
	return listed
}

// judged is u with a stop seen since it last started, and whether it should be running: it is
// enabled or an active target wants it, and it is neither a oneshot nor started when due.
func judged(u unit, listed []unit, stops map[string]stopNote) unit {
	n, ok := stops[u.Name]
	u.stopAsked = ok && !n.at.Before(u.props.activeSince)
	u.requested = u.stopAsked && n.requested
	u.wanted = (u.props.enabled || pulled(u.Name, listed)) && !u.stopAsked && !u.oneshot() && !whenDue(u, listed)
	return u
}

// oneshot is a service that exits when done and is then inactive by design.
func (u unit) oneshot() bool { return u.props.svcType == "oneshot" && !u.props.remain }

// whenDue is a unit that an active timer or path starts when due. A socket's service is not:
// it may be wanted running as well.
func whenDue(u unit, listed []unit) bool {
	return slices.ContainsFunc(listed, func(t unit) bool {
		_, typ := unitKind(t.Name)
		return (typ == "timer" || typ == "path") && t.ActiveState == "active" && slices.Contains(u.props.triggeredBy, t.Name)
	})
}

// pulled is a unit that an active target needs or wants.
func pulled(name string, listed []unit) bool {
	return slices.ContainsFunc(listed, func(t unit) bool {
		return t.ActiveState == "active" && strings.HasSuffix(t.Name, ".target") &&
			(slices.Contains(t.props.deps.hard, name) || slices.Contains(t.props.deps.soft, name))
	})
}

// status is the unit's active state as a status, a problem when it should be running and is not.
func (u unit) status() sdk.Status {
	switch {
	case u.wanted && (u.ActiveState == "inactive" || u.ActiveState == "failed"):
		return shouldRun
	case u.ActiveState == "inactive" && u.oneshot() && !u.stopAsked:
		return finished
	}
	return unitStatus(u.ActiveState)
}

// endMessage says how a unit became inactive.
func (u unit) endMessage() string {
	switch {
	case u.requested:
		return "stopped by request"
	case u.stopAsked:
		return "stopped"
	case u.wanted:
		return "exited unexpectedly"
	case u.oneshot():
		return "finished"
	}
	return "stopped"
}
