// Package geoip implements design.md §14 country judgement for node IPs:
// a local GeoLite2-Country MMDB first, with an optional online API as a
// fallback for misses. The hub writes the result to nodes.country_code.
package geoip

// Resolver maps an IP address to its ISO 3166-1 alpha-2 country code
// (uppercase). ok reports a hit; false means unknown or not resolvable.
type Resolver interface {
	Country(ip string) (string, bool)
}

// DefaultMMDBPath is where the deployment keeps the GeoLite2-Country
// database; cmd/server overrides it via FOBE_GEOIP_MMDB.
const DefaultMMDBPath = "/data/geoip/GeoLite2-Country.mmdb"

// New assembles the §14 resolver: a local MMDB at mmdbPath first, plus the
// ip-api.com online fallback when online is set. It never fails: a missing
// or unreadable database just resolves to misses until the file shows up.
func New(mmdbPath string, online bool) Resolver {
	resolvers := []Resolver{NewMMDB(mmdbPath)}
	if online {
		resolvers = append(resolvers, NewOnline(DefaultOnlineEndpoint, onlineTimeout, onlineCacheSize))
	}
	return Chain(resolvers...)
}

// Chain combines resolvers: the first non-nil one to answer a hit wins.
func Chain(resolvers ...Resolver) Resolver {
	return &chain{resolvers: resolvers}
}

type chain struct {
	resolvers []Resolver
}

func (c *chain) Country(ip string) (string, bool) {
	for _, r := range c.resolvers {
		if r == nil {
			continue
		}
		if code, ok := r.Country(ip); ok {
			return code, true
		}
	}
	return "", false
}
