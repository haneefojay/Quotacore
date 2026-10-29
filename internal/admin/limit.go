package admin

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Proxies is QUOTACORE_TRUSTED_PROXIES: the networks whose X-Forwarded-For
// the admin rate limit believes. Without one, the header is ignored, so a
// caller cannot pick the address it is limited as (deployment.md section 4.1).
type Proxies []netip.Prefix

// ParseProxies reads a comma-separated list of CIDR blocks or addresses.
func ParseProxies(raw string) (Proxies, error) {
	var out Proxies
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("%q is not an address or a CIDR block", part)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a CIDR block", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (ps Proxies) trusts(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddress is the address the admin rate limit counts a request against
// (D4): the peer, or, when the peer is a trusted proxy, the right-most
// X-Forwarded-For entry that is not one. Walking from the right is what makes
// the answer unforgeable: every entry to the right of the first untrusted one
// was appended by a proxy the operator named, and anything to its left is
// whatever the client chose to send.
func ClientAddress(r *http.Request, trusted Proxies) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if len(trusted) == 0 || !trusted.trusts(peer) {
		return peer.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// A malformed hop ends the walk: nothing to its left can be
			// trusted, and the last trusted proxy is the address.
			return peer.String()
		}
		a = a.Unmap()
		if !trusted.trusts(a) {
			return a.String()
		}
		peer = a
	}
	return peer.String()
}
