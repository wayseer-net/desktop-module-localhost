package localhost

import (
	"context"
	"fmt"
	"strings"
)

// scmAPI is the service control manager's calls; sys_windows.go makes them, and tests answer
// from recordings on any OS.
type scmAPI interface {
	services() ([]winService, error)              // EnumServicesStatusExW, Win32 services only
	config(name string) (winServiceConfig, error) // QueryServiceConfigW and QueryServiceConfig2W
	close()
}

// winService is a service as listed, with its SERVICE_STATUS_PROCESS.
type winService struct {
	name, display              string
	state, exit, specific, pid uint32
}

// winServiceConfig is what a service's configuration says of when it starts and what it needs.
type winServiceConfig struct {
	start              uint32
	delayed, triggered bool
	deps               []string // services, and load-order groups after a +
	group              string   // its own load-order group
}

// Service states, start types and exit codes, from winsvc.h and winerror.h.
const (
	serviceStopped, serviceStartPending, serviceStopPending, serviceRunning = 1, 2, 3, 4
	serviceContinuePending, servicePausePending, servicePaused              = 5, 6, 7
	serviceAutoStart, serviceDemandStart, serviceDisabled                   = 2, 3, 4
	errServiceSpecific, errNeverStarted                                     = 1066, 1077
	groupPrefix                                                             = "+"
)

// serviceSuffix names a service as systemd does, so the unit watcher treats both alike.
const serviceSuffix = ".service"

// scmSystem is Windows' service control manager, listed as systemd's units are.
type scmSystem struct{ open func() (scmAPI, error) }

func (s scmSystem) connect(context.Context) (unitSource, error) {
	api, err := s.open()
	if err != nil {
		return nil, err
	}
	return &scmUnits{api: api}, nil
}

func (scmSystem) name() string { return "service control manager" }

// scmUnits lists services as units, with their needs as hard dependencies.
type scmUnits struct {
	api    scmAPI
	listed map[string]winService // by lower-case name, as Windows ignores case
	groups map[string][]string   // load-order group to its services, read once when first needed
}

func (s *scmUnits) units(context.Context) ([]unitReply, error) {
	list, err := s.api.services()
	if err != nil {
		return nil, err
	}
	s.listed = make(map[string]winService, len(list))
	out := make([]unitReply, 0, len(list))
	for _, svc := range list {
		s.listed[strings.ToLower(svc.name)] = svc
		out = append(out, serviceReply(svc))
	}
	return out, nil
}

// details reads a service's configuration; one that can't be read, such as a protected
// service's, leaves it without a start type or needs rather than failing the listing.
func (s *scmUnits) details(_ context.Context, u unitReply) (unitProps, error) {
	name := strings.TrimSuffix(u.Name, serviceSuffix)
	p := unitProps{result: serviceResult(s.listed[strings.ToLower(name)])}
	c, err := s.api.config(name)
	if err != nil {
		return p, nil //nolint:nilerr // a service's own error leaves only it unread
	}
	p.fileState, p.enabled = startType(c)
	p.deps.hard = s.needs(c.deps)
	return p, nil
}

// needs names each needed service as listed, and each member of a needed group.
func (s *scmUnits) needs(deps []string) []string {
	var out []string
	for _, d := range deps {
		if group, ok := strings.CutPrefix(d, groupPrefix); ok {
			out = append(out, s.members(group)...)
		} else if svc, ok := s.listed[strings.ToLower(d)]; ok {
			out = append(out, svc.name+serviceSuffix)
		}
	}
	return out
}

func (s *scmUnits) members(group string) []string {
	if s.groups == nil {
		s.groups = map[string][]string{}
		for _, svc := range s.listed {
			if c, err := s.api.config(svc.name); err == nil && c.group != "" {
				g := strings.ToLower(c.group)
				s.groups[g] = append(s.groups[g], svc.name+serviceSuffix)
			}
		}
	}
	return s.groups[strings.ToLower(group)]
}

func (s *scmUnits) close() { s.api.close() }

// serviceReply is a service as a listed unit. A clean stop, or never having started, is
// inactive; stopping with an error is failed.
func serviceReply(svc winService) unitReply {
	u := unitReply{Name: svc.name + serviceSuffix, Description: svc.display, LoadState: "loaded", pid: int(svc.pid)}
	switch svc.state {
	case serviceStopped:
		u.ActiveState, u.SubState = "inactive", "stopped"
		if serviceResult(svc) != "" {
			u.ActiveState = "failed"
		} else {
			u.ended = true
		}
	case serviceStartPending, serviceContinuePending:
		u.ActiveState, u.SubState = "activating", pendingName(svc.state)
	case serviceStopPending:
		u.ActiveState, u.SubState, u.JobType = "deactivating", "stop pending", "stop"
	case servicePausePending:
		u.ActiveState, u.SubState = "active", "pause pending"
	case servicePaused:
		u.ActiveState, u.SubState = "active", "paused"
	default:
		u.ActiveState, u.SubState = "active", "running"
	}
	return u
}

func pendingName(state uint32) string {
	if state == serviceContinuePending {
		return "continue pending"
	}
	return "start pending"
}

// serviceResult is a stopped service's error, or "" if it stopped cleanly or never started.
func serviceResult(svc winService) string {
	switch {
	case svc.state != serviceStopped, svc.exit == 0, svc.exit == errNeverStarted:
		return ""
	case svc.exit != errServiceSpecific:
		return fmt.Sprintf("error %d", svc.exit)
	case svc.specific != 0:
		return fmt.Sprintf("service error %d", svc.specific)
	}
	return ""
}

// startType names the start type as Services does, and says whether it should be running: an
// automatic service that no trigger starts.
func startType(c winServiceConfig) (string, bool) {
	var name string
	switch c.start {
	case serviceAutoStart:
		name = "Automatic"
	case serviceDemandStart:
		name = "Manual"
	case serviceDisabled:
		return "Disabled", false
	default:
		return "", false // boot and system starts are drivers'
	}
	var how []string
	if c.delayed && c.start == serviceAutoStart {
		how = append(how, "Delayed Start")
	}
	if c.triggered {
		how = append(how, "Trigger Start")
	}
	if len(how) > 0 {
		name += " (" + strings.Join(how, ", ") + ")"
	}
	return name, c.start == serviceAutoStart && !c.triggered
}
