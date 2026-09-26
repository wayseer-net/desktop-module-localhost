package localhost

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSystemd reads this machine's units, where it runs systemd.
func TestLiveSystemd(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil || !supported {
		t.Skip("not booted with systemd")
	}
	w := newUnitWatcher(liveSystem{}, defaults().Units, time.Hour)
	defer w.close()
	us := w.read(t.Context(), time.Now())
	if w.note != "" || len(us) == 0 {
		t.Fatalf("%d units, note %q", len(us), w.note)
	}
}

// TestLiveJournal reads the last journal entry, where journalctl exists.
func TestLiveJournal(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil || !supported {
		t.Skip("not booted with systemd")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	rc, err := liveSystem{}.journal(ctx, journalArgs(7, 1, ""))
	if err != nil {
		t.Fatal(err)
	}
	var got []journalEntry
	_ = eachLine(rc, func(b []byte) bool {
		e, err := parseJournalEntry(b)
		if err != nil {
			t.Errorf("%v: %.200s", err, b)
		}
		got = append(got, e)
		return false
	})
	cancel()
	_ = rc.Close()
	if len(got) != 1 {
		t.Errorf("%d entries from the backlog, want 1", len(got))
	}
}
