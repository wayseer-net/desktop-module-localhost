package localhost

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"wayseer/pkg/sdk"
)

const journalFixture = "testdata/journal/entries.jsonl"

func journalLines(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile(journalFixture)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func TestParseJournalEntry(t *testing.T) {
	lines := journalLines(t)
	got, err := parseJournalEntry(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	want := journalEntry{
		cursor: "s=fixture;i=1", at: time.UnixMicro(1790000001000000), priority: 6,
		unit: "sshd.service", ident: "sshd", pid: 412, message: "Server listening on 0.0.0.0 port 22.",
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if _, err := parseJournalEntry(lines[5]); err == nil {
		t.Error("a line that is not JSON parsed")
	}
}

func TestJournalMessageShapes(t *testing.T) {
	lines := journalLines(t)
	cases := []struct {
		line     int
		message  string
		priority int
	}{
		{3, "bad � byte", 2}, // bytes that are not UTF-8 come as a number array
		{4, "", 5},           // null: too large, or unset
		{6, "Unit gone.service entered failed state.", 4}, // trailing space trimmed
		{7, "first", 6}, // a repeated field; no PRIORITY means info
	}
	for _, c := range cases {
		e, err := parseJournalEntry(lines[c.line])
		if err != nil {
			t.Fatalf("line %d: %v", c.line, err)
		}
		if e.message != c.message || e.priority != c.priority {
			t.Errorf("line %d: message %q priority %d, want %q %d", c.line, e.message, e.priority, c.message, c.priority)
		}
	}
}

func TestLongJournalMessagesAreCut(t *testing.T) {
	long := `{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":"` + string(bytes.Repeat([]byte("é"), messageCap)) + `"}`
	e, err := parseJournalEntry([]byte(long))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.message) > messageCap+len("…") || !slices.Contains([]rune(e.message), '…') {
		t.Errorf("message of %d bytes not cut to %d", len(e.message), messageCap)
	}
}

func TestSeverityFromPriority(t *testing.T) {
	want := []sdk.Severity{
		sdk.SevCritical, sdk.SevCritical, sdk.SevCritical, sdk.SevError,
		sdk.SevWarn, sdk.SevInfo, sdk.SevInfo, sdk.SevDebug,
	}
	for p, w := range want {
		if got := severityOf(p); got != w {
			t.Errorf("priority %d: %v, want %v", p, got, w)
		}
	}
}

func TestJournalArgs(t *testing.T) {
	first := journalArgs(4, 100, "")
	if !slices.Contains(first, "--lines=100") || !slices.Contains(first, "--follow") {
		t.Errorf("first start: %v", first)
	}
	// Below info, the manager's job lines are asked for besides entries up to the priority.
	want := []string{"PRIORITY=0", "PRIORITY=1", "PRIORITY=2", "PRIORITY=3", "PRIORITY=4", "+", "_PID=1", "JOB_TYPE=start", "JOB_TYPE=stop"}
	if got := first[len(first)-len(want):]; !slices.Equal(got, want) || slices.Contains(first, "--priority=4") {
		t.Errorf("matches %v, want %v", first, want)
	}
	if info := journalArgs(6, 100, ""); !slices.Contains(info, "--priority=6") || slices.Contains(info, "+") {
		t.Errorf("at info: %v; job lines are already sent", info)
	}
	again := journalArgs(4, 100, "s=abc")
	if !slices.Contains(again, "--after-cursor=s=abc") || slices.Contains(again, "--lines=100") {
		t.Errorf("restart: %v; want to resume after the cursor, without the backlog", again)
	}
}

func TestParsePriorityName(t *testing.T) {
	for name, want := range map[string]int{"warning": 4, "err": 3, "info": 6, "debug": 7, "emerg": 0, "3": 3} {
		if got, err := parsePriority(name); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := parsePriority("loud"); err == nil {
		t.Error("an unknown priority parsed")
	}
}

func TestOverlongJournalLinesAreSkipped(t *testing.T) {
	in := "first\n" + strings.Repeat("x", lineCap+10) + "\nlast"
	var got []string
	if err := eachLine(strings.NewReader(in), func(b []byte) bool { got = append(got, strings.TrimSpace(string(b))); return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"first", "last"}) {
		t.Errorf("lines %q, want [first last]", got)
	}
}
