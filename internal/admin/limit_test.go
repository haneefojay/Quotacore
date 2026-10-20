package admin

import (
	"net/http"
	"testing"
)

// TestTrustedProxiesDecideTheClientIP is IP-09 D4's address rule: the peer,
// unless it is a trusted proxy, and then the right-most X-Forwarded-For hop no
// trusted proxy added.
func TestTrustedProxiesDecideTheClientIP(t *testing.T) {
	trusted, err := ParseProxies("10.0.0.0/8, 192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, peer, xff string
		proxies         Proxies
		want            string
	}{
		{"no proxies ignore the header", "203.0.113.5:4000", "198.51.100.1", nil, "203.0.113.5"},
		{"an untrusted peer ignores the header", "203.0.113.5:4000", "198.51.100.1", trusted, "203.0.113.5"},
		{"a trusted peer names the client", "10.1.2.3:4000", "198.51.100.1", trusted, "198.51.100.1"},
		{"a client prefix is not believed", "10.1.2.3:4000", "6.6.6.6, 198.51.100.1", trusted, "198.51.100.1"},
		{"trusted hops are walked past", "10.1.2.3:4000", "198.51.100.1, 192.0.2.1, 10.9.9.9", trusted, "198.51.100.1"},
		{"all hops trusted is the last one", "10.1.2.3:4000", "10.4.4.4", trusted, "10.4.4.4"},
		{"a malformed hop stops the walk", "10.1.2.3:4000", "198.51.100.1, nonsense", trusted, "10.1.2.3"},
		{"a trusted peer with no header is itself", "192.0.2.1:4000", "", trusted, "192.0.2.1"},
		{"a mapped IPv4 peer is IPv4", "[::ffff:203.0.113.5]:4000", "", nil, "203.0.113.5"},
	} {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientAddress(r, c.proxies); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if _, err := ParseProxies("10.0.0.0/33"); err == nil {
		t.Error("a malformed CIDR block parsed")
	}
}
