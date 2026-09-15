package agent

import (
	"net"
	"sort"

	"github.com/fobe-panel/fobe/internal/protocol"
)

// LocalIPs enumerates the host's addresses (design §14): loopback and
// link-local excluded, sorted public-first so the primary pick is stable.
// The server records the full set; `is_primary` marks the default candidate
// (first public IPv4, else first public IPv6) — the panel may override it.
func LocalIPs() []protocol.IPInfo {
	var out []protocol.IPInfo
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			family := 4
			scope := "private"
			s := ip.String()
			if ip.To4() == nil {
				family = 6
			}
			if ip.IsGlobalUnicast() && isPublic(ip) {
				scope = "public"
			}
			out = append(out, protocol.IPInfo{IP: s, Family: family, Scope: scope})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].Scope == "public", out[j].Scope == "public"
		if pi != pj {
			return pi
		}
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].IP < out[j].IP
	})
	if len(out) > 0 {
		out[0].IsPrimary = true // agent's suggestion; panel can repin
	}
	return out
}

// isPublic approximates RFC1918/ULA detection (GlobalUnicast alone also
// returns true for private ranges, so check them explicitly).
func isPublic(ip net.IP) bool {
	private := []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"100.64.0.0/10", // CGNAT
		"fc00::/7",
	}
	for _, cidr := range private {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
