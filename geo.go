package localhost

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
	"wayseer.dev/sdk"
)

// location is what IP databases say about an address; the zero value is nothing known.
type location struct {
	place   sdk.Place
	city    string
	country string // ISO 3166 code
	asn     uint32
	org     string // the autonomous system's organization
	credit  string // whose data this is, as its license requires
}

// locator looks up remote addresses; a nil locator knows nothing.
type locator interface {
	locate(netip.Addr) location
	close()
}

// geoIP reads city and ASN databases in MMDB format; either may be nil. A lookup takes about
// 10µs, so remotes seen before are remembered, up to twice the most listed.
type geoIP struct {
	city, asn *maxminddb.Reader
	credit    string
	seen      map[netip.Addr]location
}

const geoCache = 2 * maxConnections

func openGeoIP(o geoipOptions) (locator, error) {
	if o.City == "" && o.ASN == "" {
		return nil, nil
	}
	g := &geoIP{seen: map[netip.Addr]location{}}
	var err error
	if g.city, err = openMMDB("city", o.City, false); err == nil {
		g.asn, err = openMMDB("asn", o.ASN, true)
	}
	if err != nil {
		g.close()
		return nil, err
	}
	var credits []string
	for _, r := range []*maxminddb.Reader{g.city, g.asn} {
		if c := creditOf(r); c != "" && !slices.Contains(credits, c) {
			credits = append(credits, c)
		}
	}
	g.credit = strings.Join(credits, "; ")
	return g, nil
}

// openMMDB opens one database, checking it is an ASN database or is not, as wanted.
func openMMDB(what, path string, wantASN bool) (*maxminddb.Reader, error) {
	if path == "" {
		return nil, nil
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geoip %s: %w", what, err)
	}
	dbType := r.Metadata.DatabaseType
	switch isASN := strings.Contains(strings.ToLower(dbType), "asn"); {
	case isASN && !wantASN:
		err = fmt.Errorf("geoip %s: %s is an ASN database (%s)", what, path, dbType)
	case !isASN && wantASN:
		err = fmt.Errorf("geoip %s: %s is not an ASN database (%s)", what, path, dbType)
	}
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func creditOf(r *maxminddb.Reader) string {
	if r == nil {
		return ""
	}
	return creditFor(r.Metadata.DatabaseType)
}

// creditFor is the attribution a database's license asks for, known by its type.
func creditFor(dbType string) string {
	switch {
	case strings.HasPrefix(dbType, "DBIP"):
		return "IP geolocation by DB-IP (https://db-ip.com), CC BY 4.0"
	case strings.HasPrefix(dbType, "GeoLite2"), strings.HasPrefix(dbType, "GeoIP2"):
		return strings.SplitN(dbType, "-", 2)[0] + " data created by MaxMind (https://www.maxmind.com)"
	}
	return dbType
}

type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

type asnRecord struct {
	Number uint32 `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

// locate looks up a public address; a record that cannot be read is nothing known.
func (g *geoIP) locate(a netip.Addr) location {
	if scope(a) != "public" {
		return location{}
	}
	if l, ok := g.seen[a]; ok {
		return l
	}
	if len(g.seen) >= geoCache {
		clear(g.seen)
	}
	l := g.lookup(a)
	g.seen[a] = l
	return l
}

func (g *geoIP) lookup(a netip.Addr) location {
	var l location
	var c cityRecord
	if g.city != nil && g.city.Lookup(a).Decode(&c) == nil {
		l.city, l.country = c.City.Names["en"], c.Country.ISOCode
		if lat, lon := c.Location.Latitude, c.Location.Longitude; lat != nil && lon != nil {
			l.place = sdk.At(float32(*lat), float32(*lon))
		}
	}
	var n asnRecord
	if g.asn != nil && g.asn.Lookup(a).Decode(&n) == nil {
		l.asn, l.org = n.Number, n.Org
	}
	if l != (location{}) {
		l.credit = g.credit
	}
	return l
}

func (g *geoIP) close() {
	for _, r := range []*maxminddb.Reader{g.city, g.asn} {
		if r != nil {
			_ = r.Close()
		}
	}
}

// attrs are a location's attributes, without empty ones.
func (l location) attrs() map[string]sdk.Value {
	a := pruned(map[string]sdk.Value{
		"city": sdk.String(l.city), "country": sdk.String(l.country), "organization": sdk.String(l.org),
		"location_source": sdk.String(l.credit),
	})
	if l.asn != 0 {
		a["asn"] = num(int(l.asn))
	}
	return a
}

var errGeoNeedsConnections = errors.New("geoip places remote addresses, so it needs connections: true")
