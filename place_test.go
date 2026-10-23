package localhost

import (
	"strings"
	"testing"

	"wayseer.dev/sdk"
)

// With a place, the host and its processes are there; remotes keep the IP database's place.
func TestPlacePutsTheHostAndItsProcessesThere(t *testing.T) {
	city, _ := geoFixture(t)
	socks := []socket{tcp(1, 4711, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 1000, 2000)}
	m := connModule(t, &socks, "geoip: {city: "+city+"}\nplace: {lat: 40.71, lon: -74.0}\n")
	pollConns(m, t0)

	here := sdk.At(40.71, -74.0)
	for _, r := range []sdk.EntityRef{ref(sdk.KindHost, "testbox"), ref(sdk.KindProcess, "1200")} {
		if p := m.world.ents[r].Place; p != here {
			t.Errorf("%s at %+v", r, p)
		}
	}
	if p := m.world.ents[ref(KindCPU, "cpu0")].Place; p.Known {
		t.Errorf("cpu placed at %+v", p)
	}
	if p := m.world.ents[ref(KindRemote, "93.184.216.34")].Place; p != sdk.At(52.37, 4.89) {
		t.Errorf("remote at %+v", p)
	}
}

func TestNoPlaceLeavesTheHostUnplaced(t *testing.T) {
	m := testModule(t, copyFixture(t), "")
	pollConns(m, t0)
	if p := m.world.ents[ref(sdk.KindHost, "testbox")].Place; p.Known {
		t.Errorf("host at %+v", p)
	}
}

func TestPlaceOptionErrors(t *testing.T) {
	for name, tc := range map[string]struct{ cfg, want string }{
		"no lon":      {"place: {lat: 40}", "lat and lon"},
		"no lat":      {"place: {lon: 40}", "lat and lon"},
		"lat range":   {"place: {lat: 91, lon: 0}", "lat 91"},
		"lon range":   {"place: {lat: 0, lon: -181}", "lon -181"},
		"unknown key": {"place: {lat: 0, lon: 0, alt: 3}", "alt"},
		"equator":     {"place: {lat: 0, lon: 0}", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := configureErr(t, New(), "root: "+copyFixture(t)+"\n"+tc.cfg)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("error %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
}
