package localhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wayseer.dev/sdk/sdktest"
)

func TestAQuietMachineIsStillSentEachPoll(t *testing.T) {
	root := copyFixture(t)
	m := testModule(t, root, "interval: 100ms")
	s := sdktest.Run(t, func(ctx context.Context, s *sdktest.Sink) error { return m.Run(ctx, s) })
	s.WaitFor(t, 3)
	for i, cs := range s.Sets()[1:] {
		if !cs.Empty() {
			t.Errorf("delta %d of the unchanged fixture = %+v, want empty", i, cs)
		}
	}
	if err := os.Remove(filepath.Join(root, "proc/stat")); err != nil {
		t.Fatal(err)
	}
	sdktest.Eventually(t, func() bool { return m.Health().Err != nil })
	n := len(s.Sets())
	time.Sleep(500 * time.Millisecond)
	if got := len(s.Sets()); got > n+1 {
		t.Errorf("%d deltas sent while the machine could not be read", got-n)
	}
}
