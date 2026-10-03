package localhost

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"wayseer.dev/sdk"
	"wayseer.dev/sdk/sdktest"
)

func journalModule(t *testing.T, f *fakeSystem, extra string) (*Module, *sdktest.Sink) {
	t.Helper()
	text, err := os.ReadFile(journalFixture)
	if err != nil {
		t.Fatal(err)
	}
	f.journalText = text
	m := systemModule(t, f, "interval: 100ms\n"+extra)
	m.journalRetry = 20 * time.Millisecond
	return m, sdktest.Run(t, func(ctx context.Context, s *sdktest.Sink) error { return m.Run(ctx, s) })
}

func waitForEvents(t *testing.T, s *sdktest.Sink, n int) []sdk.Event {
	t.Helper()
	sdktest.Eventually(t, func() bool { return len(s.Events()) >= n })
	return s.Events()
}

func TestJournalEntriesBecomeEvents(t *testing.T) {
	_, sink := journalModule(t, newFakeSystem(t), "journal: debug")
	evs := waitForEvents(t, sink, 7)
	want := []struct {
		entity sdk.EntityRef
		sev    sdk.Severity
	}{
		{unitRef("sshd.service"), sdk.SevInfo},
		{unitRef("cups.service"), sdk.SevError},
		{ref(sdk.KindHost, "testbox"), sdk.SevWarn}, // the kernel
		{unitRef("nginx.service"), sdk.SevCritical},
		{ref(sdk.KindProcess, "1201"), sdk.SevInfo}, // an unlisted unit, a listed pid
		{ref(sdk.KindHost, "testbox"), sdk.SevWarn}, // neither listed
		{unitRef("backup.timer"), sdk.SevInfo},
	}
	for i, w := range want {
		if e := evs[i]; e.Entity != w.entity || e.Severity != w.sev || e.Kind != "log" || e.Source != "local" {
			t.Errorf("event %d = %+v; want entity %s, severity %v", i, e, w.entity, w.sev)
		}
	}
	first := evs[0]
	if first.ID != "s=fixture;i=1" || !first.At.Equal(time.UnixMicro(1790000001000000)) || first.Message != "Server listening on 0.0.0.0 port 22." {
		t.Errorf("first = %+v", first)
	}
	if f := first.Fields; f["identifier"].Str() != "sshd" || f["unit"].Str() != "sshd.service" || f["pid"].Num() != 412 || f["priority"].Str() != "info" {
		t.Errorf("fields = %v", f)
	}
}

func TestJournalEventsAreQueryable(t *testing.T) {
	m, sink := journalModule(t, newFakeSystem(t), "journal: debug")
	waitForEvents(t, sink, 7)
	got, err := m.QueryEvents(context.Background(), sdk.EventQuery{Entities: []sdk.EntityRef{unitRef("cups.service")}})
	if err != nil || len(got) != 1 || got[0].Message != "Failed to start CUPS Scheduler." {
		t.Errorf("cups events = %+v, %v", got, err)
	}
}

func TestJournalStartsFromTheBacklog(t *testing.T) {
	f := newFakeSystem(t)
	_, sink := journalModule(t, f, "journal: err\njournal_backlog: 20")
	waitForEvents(t, sink, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	args := f.journalRuns[0]
	if !slices.Contains(args, "PRIORITY=3") || slices.Contains(args, "PRIORITY=4") || !slices.Contains(args, "--lines=20") {
		t.Errorf("journalctl %v", args)
	}
}

// endingSystem's journal ends after its lines, as journalctl does when it fails.
type endingSystem struct{ *fakeSystem }

func (e endingSystem) journal(_ context.Context, args []string) (io.ReadCloser, error) {
	e.mu.Lock()
	e.journalRuns = append(e.journalRuns, args)
	text := e.journalText
	e.mu.Unlock()
	return io.NopCloser(strings.NewReader(string(text))), nil
}

func TestJournalResumesAfterTheLastEntry(t *testing.T) {
	f := newFakeSystem(t)
	text, _ := os.ReadFile(journalFixture)
	f.journalText = text
	m := New()
	m.system, m.journalRetry = endingSystem{f}, 10*time.Millisecond
	m.statfs = func(string) (fsUsage, error) { return fsUsage{}, nil }
	configure(t, m, "root: "+copyFixture(t))
	sdktest.Run(t, func(ctx context.Context, s *sdktest.Sink) error { return m.Run(ctx, s) })
	sdktest.Eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.journalRuns) >= 2 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.journalRuns[1], "--after-cursor=s=fixture;i=7") {
		t.Errorf("restarted with %v", f.journalRuns[1])
	}
}

// failingSystem cannot start journalctl.
type failingSystem struct{ *fakeSystem }

func (failingSystem) journal(context.Context, []string) (io.ReadCloser, error) {
	return nil, errors.New(`exec: "journalctl": executable file not found in $PATH`)
}

func TestMissingJournalIsANote(t *testing.T) {
	m := New()
	m.system, m.journalRetry = failingSystem{newFakeSystem(t)}, time.Hour
	m.statfs = func(string) (fsUsage, error) { return fsUsage{}, nil }
	configure(t, m, "interval: 100ms\nroot: "+copyFixture(t))
	sdktest.Run(t, func(ctx context.Context, s *sdktest.Sink) error { return m.Run(ctx, s) })
	sdktest.Eventually(t, func() bool { return strings.HasPrefix(m.Health().Note, "journal: ") })
	if h := m.Health(); h.Err != nil {
		t.Errorf("health = %+v", h)
	}
}

func TestJournalOff(t *testing.T) {
	m := systemModule(t, newFakeSystem(t), "journal: off")
	if m.journal != nil {
		t.Error("the journal is read with journal: off")
	}
}
