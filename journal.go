package localhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"mindseye/internal/model"
	"strconv"
	"strings"
	"time"
)

// messageCap bounds a journal message kept in an event.
const messageCap = 2048

// journalEntry is the part of a journal record an event needs.
type journalEntry struct {
	cursor   string
	at       time.Time
	priority int // syslog: 0 emerg to 7 debug
	unit     string
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
}

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
func severityOf(priority int) model.Severity {
	switch {
	case priority <= 2:
		return model.SevCritical
	case priority == 3:
		return model.SevError
	case priority == 4:
		return model.SevWarn
	case priority <= 6:
		return model.SevInfo
	}
	return model.SevDebug
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

// journalArgs follows the journal from the backlog, or after the cursor when resuming.
func journalArgs(priority, backlog int, cursor string) []string {
	args := []string{"--output=json", "--follow", "--all", "--no-pager", "--quiet", "--priority=" + strconv.Itoa(priority)}
	if cursor != "" {
		return append(args, "--after-cursor="+cursor)
	}
	return append(args, "--lines="+strconv.Itoa(backlog))
}
