package geoip

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultOnlineEndpoint asks ip-api.com for just the country code; {ip} is
// replaced per lookup (§14: online API as fallback for local MMDB misses).
const DefaultOnlineEndpoint = "http://ip-api.com/json/{ip}?fields=countryCode"

const (
	onlineTimeout   = 3 * time.Second
	onlineCacheSize = 4096
)

// Online is the online fallback resolver: it queries an HTTP endpoint for
// the country when enabled. Results — misses included — are cached in an
// LRU keyed by IP, so repeated state reports don't hit the network.
type Online struct {
	client *http.Client
	tmpl   string
	cache  *lru
}

// NewOnline builds the fallback resolver against an endpoint template that
// contains {ip}. timeout caps each request; cacheSize bounds the LRU.
func NewOnline(endpoint string, timeout time.Duration, cacheSize int) *Online {
	return &Online{
		client: &http.Client{Timeout: timeout},
		tmpl:   endpoint,
		cache:  newLRU(cacheSize),
	}
}

// Country implements Resolver.
func (o *Online) Country(ipStr string) (string, bool) {
	ip := net.ParseIP(ipStr)
	if ip == nil || unroutable(ip) {
		return "", false // no point asking an online API about these
	}
	if code, ok := o.cache.get(ipStr); ok {
		return code, code != ""
	}
	code, ok := o.query(ipStr)
	o.cache.put(ipStr, code)
	return code, ok
}

func (o *Online) query(ipStr string) (string, bool) {
	resp, err := o.client.Get(strings.ReplaceAll(o.tmpl, "{ip}", ipStr))
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var out struct {
		CountryCode string `json:"countryCode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil {
		return "", false
	}
	code := strings.ToUpper(strings.TrimSpace(out.CountryCode))
	if !isCountryCode(code) {
		return "", false
	}
	return code, true
}

// unroutable reports whether ip can't belong to a country: loopback, LAN,
// link-local, multicast or unspecified.
func unroutable(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

func isCountryCode(code string) bool {
	if len(code) != 2 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return false
		}
	}
	return true
}
