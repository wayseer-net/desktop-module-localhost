package localhost

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mindseye/pkg/sdk"
	"strconv"
	"time"
)

const (
	eventCap   = 1000    // journal events kept for queries
	entryBatch = 256     // journal entries sent in one delta at most
	lineCap    = 1 << 20 // longer journal lines are skipped
)

// followJournal streams journal entries to out until ctx ends, restarting journalctl after
// the last entry seen whenever it stops.
func (m *Module) followJournal(ctx context.Context, sys system, out chan<- journalEntry) {
	cursor := ""
	for {
		cursor = m.readJournal(ctx, sys, cursor, out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.journalRetry):
		}
	}
}

// readJournal runs journalctl once and returns the cursor of the last entry it sent.
func (m *Module) readJournal(ctx context.Context, sys system, cursor string, out chan<- journalEntry) string {
	rc, err := sys.journal(ctx, journalArgs(m.opts.priority, m.opts.JournalBacklog, cursor))
	if err != nil {
		m.setJournalNote("journal: " + err.Error())
		return cursor
	}
	m.setJournalNote("")
	err = eachLine(rc, func(line []byte) bool {
		e, err := parseJournalEntry(line)
		if err != nil {
			return true // not an entry: skip it
		}
		select {
		case out <- e:
			cursor = e.cursor
			return true
		case <-ctx.Done():
			return false
		}
	})
	if err = errors.Join(err, rc.Close()); ctx.Err() == nil {
		m.setJournalNote("journal stopped: " + cmpErr(err, "it exited"))
	}
	return cursor
}

func cmpErr(err error, otherwise string) string {
	if err == nil {
		return otherwise
	}
	return err.Error()
}

func (m *Module) setJournalNote(note string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journalNote = note
}

// eachLine calls fn with each line of r, skipping lines over lineCap, until fn returns false.
func eachLine(r io.Reader, fn func([]byte) bool) error {
	br := bufio.NewReaderSize(r, lineCap)
	skipping := false
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			skipping = true
			continue
		case skipping:
			skipping = false
		case len(line) > 0 && !fn(line):
			return nil
		}
		if err != nil {
			return ignoreEOF(err)
		}
	}
}

func ignoreEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// drain collects first and whatever else is waiting, up to entryBatch.
func drain(first journalEntry, in <-chan journalEntry) []journalEntry {
	out := []journalEntry{first}
	for len(out) < entryBatch {
		select {
		case e := <-in:
			out = append(out, e)
		default:
			return out
		}
	}
	return out
}

// logged turns journal entries into events, kept for queries and returned to send. The service
// manager's job lines are read for stops asked for; below the chosen priority they are not sent.
func (m *Module) logged(entries []journalEntry) *sdk.ChangeSet {
	m.mu.Lock()
	defer m.mu.Unlock()
	var evs []sdk.Event
	for _, e := range entries {
		m.units.jobLogged(e)
		if e.priority > m.opts.priority {
			continue
		}
		ev := sdk.Event{
			ID: e.cursor, Entity: m.eventEntity(e), At: e.at, Severity: severityOf(e.priority),
			Kind: "log", Message: e.message, Source: m.name,
			Fields: pruned(map[string]sdk.Value{
				"identifier": sdk.String(e.ident), "unit": sdk.String(e.unit),
				"priority": sdk.String(priorityNames[e.priority]),
			}),
		}
		if e.pid > 0 {
			ev.Fields["pid"] = num(e.pid)
		}
		evs = append(evs, ev)
	}
	m.events.Add(evs...)
	return &sdk.ChangeSet{Events: evs}
}

// stateEvents are events on units for their changes of active state.
func (m *Module) stateEvents(changes []unitChange) []sdk.Event {
	var evs []sdk.Event
	for _, c := range changes {
		sev, msg := sdk.SevInfo, stateMessage(c.from, c.to)
		if c.to == "inactive" {
			msg = c.unit.endMessage()
		}
		if c.to == "failed" || c.unit.wanted && c.to == "inactive" {
			sev = sdk.SevError
		}
		b := builder{src: m.name}
		evs = append(evs, sdk.Event{
			ID: "state;" + c.name + ";" + strconv.FormatInt(c.at.UnixNano(), 10), Entity: b.unitRef(c.name),
			At: c.at, Severity: sev, Kind: "state", Message: msg, Source: m.name,
			Fields: map[string]sdk.Value{"unit": sdk.String(c.name), "from": sdk.String(c.from), "to": sdk.String(c.to)},
		})
	}
	return evs
}

// stateMessage names a change of active state the way people say it.
func stateMessage(from, to string) string {
	switch {
	case to == "active" && (from == "reloading" || from == "refreshing"):
		return "reloaded"
	case to == "active":
		return "started"
	case to == "inactive":
		return "stopped"
	case to == "activating":
		return "starting"
	case to == "deactivating":
		return "stopping"
	}
	return to
}

// eventEntity is the unit the entry is about or from if listed, else its process if listed,
// else the host.
func (m *Module) eventEntity(e journalEntry) sdk.EntityRef {
	b := builder{src: m.name, w: m.world}
	for _, u := range []string{e.object, e.unit} {
		if u == "" {
			continue
		}
		if r := b.unitRef(u); b.listed(r) {
			return r
		}
	}
	if e.pid > 0 {
		if r := b.ref(sdk.KindProcess, strconv.Itoa(e.pid)); b.listed(r) {
			return r
		}
	}
	return m.world.host
}

// QueryEvents answers from the journal entries received so far.
func (m *Module) QueryEvents(ctx context.Context, q sdk.EventQuery) ([]sdk.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events.Query(q), nil
}
