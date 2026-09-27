package localhost

import (
	"context"
	"mindseye/pkg/sdk"
	"testing"
	"time"
)

func TestFakeReadsOnlyItsFixture(t *testing.T) {
	m := Fake()
	configure(t, m, "root: "+copyFixture(t)+"\n")
	cs := m.poll(context.Background(), time.Unix(1790000000, 0))
	filesystems := 0
	for _, e := range cs.Upserts {
		switch e.Kind {
		case KindFilesystem:
			filesystems++
			if e.Attrs["size"] != sdk.Number(100<<30) || e.Status.Level != sdk.StatusOK {
				t.Errorf("%s: size %v, status %v; want the fake's 100 GiB, 40%% used", e.Name, e.Attrs["size"], e.Status)
			}
		case sdk.KindService:
			t.Errorf("service %s; the fake has no services", e.Name)
		}
	}
	if filesystems == 0 {
		t.Error("no filesystems read from the fixture")
	}
}
