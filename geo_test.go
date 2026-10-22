package localhost

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"wayseer.dev/sdk"
)

// writeMMDB writes a database of the given type holding a record for each network.
func writeMMDB(t *testing.T, dbType string, records map[string]mmdbtype.Map) string {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: dbType, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, rec := range records {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Insert(n, rec); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), dbType+".mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := tree.WriteTo(f); err != nil {
		t.Fatal(err)
	}
	return path
}

func names(en string) mmdbtype.Map {
	return mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(en)}}
}

// geoFixture is a city and an ASN database that place 93.184.216.0/24 in Amsterdam, run by
// Example Networks, and 192.168.0.0/16 too, which must never be looked up.
func geoFixture(t *testing.T) (city, asn string) {
	t.Helper()
	amsterdam := mmdbtype.Map{
		"city":     names("Amsterdam"),
		"country":  mmdbtype.Map{"iso_code": mmdbtype.String("NL"), "names": mmdbtype.Map{"en": mmdbtype.String("Netherlands")}},
		"location": mmdbtype.Map{"latitude": mmdbtype.Float64(52.37), "longitude": mmdbtype.Float64(4.89)},
	}
	city = writeMMDB(t, "DBIP-City-Lite", map[string]mmdbtype.Map{"93.184.216.0/24": amsterdam, "192.168.0.0/16": amsterdam})
	network := mmdbtype.Map{
		"autonomous_system_number": mmdbtype.Uint32(64500), "autonomous_system_organization": mmdbtype.String("Example Networks"),
	}
	asn = writeMMDB(t, "DBIP-ASN-Lite", map[string]mmdbtype.Map{"93.184.216.0/24": network, "192.168.0.0/16": network})
	return city, asn
}

func TestGeoIPPlacesAndNamesRemotes(t *testing.T) {
	city, asn := geoFixture(t)
	socks := []socket{
		tcp(1, 4711, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 0, 0),
		tcp(2, 4711, tcpEstablished, "192.168.1.10:50413", "192.168.1.20:22", 0, 0),
	}
	m := connModule(t, &socks, "geoip: {city: "+city+", asn: "+asn+"}\n")
	pollConns(m, t0)

	e := m.world.ents[ref(KindRemote, "93.184.216.34")]
	if e.Place != sdk.At(52.37, 4.89) {
		t.Errorf("place %+v", e.Place)
	}
	for k, want := range map[string]string{
		"country": "NL", "city": "Amsterdam", "organization": "Example Networks", AttrGroup: "Example Networks",
		"location_source": "IP geolocation by DB-IP (https://db-ip.com), CC BY 4.0",
	} {
		if got := e.Attrs[k].Str(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := e.Attrs["asn"].Num(); got != 64500 {
		t.Errorf("asn %v", got)
	}

	private := m.world.ents[ref(KindRemote, "192.168.1.20")]
	if private.Place.Known || private.Attrs["country"].Str() != "" || private.Attrs[AttrGroup].Str() != "network" {
		t.Errorf("private remote looked up: %+v", private)
	}
}

func TestGeoIPEitherDatabaseAlone(t *testing.T) {
	_, asn := geoFixture(t)
	socks := []socket{tcp(1, 4711, tcpEstablished, "192.168.1.10:50412", "93.184.216.34:443", 0, 0)}
	m := connModule(t, &socks, "geoip: {asn: "+asn+"}\n")
	pollConns(m, t0)
	e := m.world.ents[ref(KindRemote, "93.184.216.34")]
	if e.Place.Known || e.Attrs["organization"].Str() != "Example Networks" {
		t.Errorf("entity %+v", e)
	}
}

func TestGeoIPOptionErrors(t *testing.T) {
	city, asn := geoFixture(t)
	for name, tc := range map[string]struct{ cfg, want string }{
		"missing":         {"connections: true\ngeoip: {city: /nonexistent/city.mmdb}", "no such file"},
		"relative":        {"connections: true\ngeoip: {city: city.mmdb}", "absolute"},
		"asn as city":     {"connections: true\ngeoip: {city: " + asn + "}", "ASN database"},
		"city as asn":     {"connections: true\ngeoip: {asn: " + city + "}", "not an ASN database"},
		"no connections":  {"geoip: {city: " + city + "}", "connections: true"},
		"not a database":  {"connections: true\ngeoip: {city: " + writeFile(t, "junk") + "}", "geoip city"},
		"unknown key":     {"connections: true\ngeoip: {country: " + city + "}", "country"},
		"empty geoip map": {"connections: true\ngeoip: {}", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := configureErr(t, New(), tc.cfg)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("error %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGeoIPCredit(t *testing.T) {
	for dbType, want := range map[string]string{
		"DBIP-City-Lite": "IP geolocation by DB-IP (https://db-ip.com), CC BY 4.0",
		"DBIP-ASN-Lite":  "IP geolocation by DB-IP (https://db-ip.com), CC BY 4.0",
		"GeoLite2-City":  "GeoLite2 data created by MaxMind (https://www.maxmind.com)",
		"Acme-City":      "Acme-City",
	} {
		if got := creditFor(dbType); got != want {
			t.Errorf("%s: %q", dbType, got)
		}
	}
}

func TestFakePlacesItsRemote(t *testing.T) {
	m := Fake()
	configure(t, m, "root: "+copyFixture(t)+"\nconnections: true\n")
	pollConns(m, t0)
	if e := m.world.ents[ref(KindRemote, "198.51.100.7")]; !e.Place.Known || e.Attrs["organization"].Str() == "" {
		t.Errorf("fake remote %+v", e)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
