package localhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"wayseer/pkg/sdk"
)

// messageCap bounds a journal message kept in an event.
const messageCap = 2048

// journalEntry is the part of a journal record an event needs.
type journalEntry struct {
	cursor   string
	at       time.Time
	priority int    // syslog: 0 emerg to 7 debug
	unit     string // the unit that logged it
	object   string // the unit the service manager logged it about
	jobType  string // the manager's job the line is about: start, stop...
	ident    string
	pid      int
	message  string
}

// journalJSON is `journalctl -o json`: every field a string, except that bytes which are not
// UTF-8 come as a number array, a repeated field as an array, and an oversized one as null.
type journalJSON struct {
	Cursor   string          `json:"__CURSOR"`
	Realtime string          `json:"__REALTIME_TIMESTAMP"` // microseconds since the epoch
	Priority string          `json:"PRIORITY"`
	Unit     json.RawMessage `json:"_SYSTEMD_UNIT"`
	Ident    json.RawMessage `json:"SYSLOG_IDENTIFIER"`
	PID      json.RawMessage `json:"_PID"`
	Message  json.RawMessage `json:"MESSAGE"`
	Object   json.RawMessage `json:"UNIT"`     // set by the service manager, and by anyone else
	JobType  json.RawMessage `json:"JOB_TYPE"` // likewise
}

// managerPID is the system's service manager; only its lines name the unit they are about.
const managerPID = 1

// parseJournalEntry reads one line of `journalctl -o json`.
func parseJournalEntry(b []byte) (journalEntry, error) {
	var j journalJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return journalEntry{}, err
	}
	us, err := strconv.ParseInt(j.Realtime, 10, 64)
	if err != nil || j.Cursor == "" {
		return journalEntry{}, errors.New("journal entry has no cursor or time")
	}
	e := journalEntry{
		cursor: j.Cursor, at: time.UnixMicro(us), priority: 6,
		unit: journalField(j.Unit), ident: journalField(j.Ident),
		message: cut(strings.TrimRight(journalField(j.Message), " \t\r\n"), messageCap),
	}
	e.pid, _ = strconv.Atoi(journalField(j.PID))
	if e.pid == managerPID {
		e.object, e.jobType = journalField(j.Object), journalField(j.JobType)
	}
	if p, err := strconv.Atoi(j.Priority); err == nil && p >= 0 && p <= 7 {
		e.priority = p
	}
	return e, nil
}

// journalField decodes a field's string, byte-array, repeated (first value) or null form.
func journalField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []byte
	var nums []uint8
	if json.Unmarshal(raw, &nums) == nil {
		bs = nums
		return strings.ToValidUTF8(string(bs), "�")
	}
	var many []json.RawMessage
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		return journalField(many[0])
	}
	return ""
}

// cut shortens s to at most n bytes without splitting a character, marking the cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// severityOf maps a syslog priority to an event severity.
func severityOf(priority int) sdk.Severity {
	switch {
	case priority <= 2:
		return sdk.SevCritical
	case priority == 3:
		return sdk.SevError
	case priority == 4:
		return sdk.SevWarn
	case priority <= 6:
		return sdk.SevInfo
	}
	return sdk.SevDebug
}

var priorityNames = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// parsePriority accepts a syslog priority by name or number, as journalctl does.
func parsePriority(s string) (int, error) {
	for i, n := range priorityNames {
		if s == n {
			return i, nil
		}
	}
	if p, err := strconv.Atoi(s); err == nil && p >= 0 && p <= 7 {
		return p, nil
	}
	return 0, fmt.Errorf("journal priority %q is not one of %s", s, strings.Join(priorityNames, ", "))
}

// journalArgs follows the journal from the backlog, or after the cursor when resuming. Below
// info, the manager's job lines (info) are matched as well as entries up to the priority.
func journalArgs(priority, backlog int, cursor string) []string {
	args := []string{"--output=json", "--follow", "--all", "--no-pager", "--quiet"}
	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "--lines="+strconv.Itoa(backlog))
	}
	if priority >= 6 {
		return append(args, "--priority="+strconv.Itoa(priority))
	}
	for p := range priority + 1 {
		args = append(args, "PRIORITY="+strconv.Itoa(p))
	}
	return append(args, "+", "_PID="+strconv.Itoa(managerPID), "JOB_TYPE=start", "JOB_TYPE=stop")
}
