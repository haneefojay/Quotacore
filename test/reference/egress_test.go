package reference_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// canary is TEST-NET-1 (RFC 5737): an address that is never anyone's.
const canary = "192.0.2.1"

const captureImage = "nicolaka/netshoot:v0.14@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"

// opened matches a tcpdump line for a packet that opens a flow: a TCP SYN
// without ACK, or any UDP datagram. The capture is read with a filter that
// keeps only those, so every line is a connection somebody started.
var opened = regexp.MustCompile(`^\S+ (\S+\s+)?(\S+\s+)?(IP6?) (\S+) > (\S+):`)

// TestTheServiceMakesNoOutboundConnection (IP-15 DoD 2, NFR-S3, NFR-C5):
// every packet in the service's network namespace is captured from before the
// process starts, through the quickstart's init, consumes, a status read, a
// verify-all and a rebuild dry run, to a clean stop. Every flow the namespace
// opens must be to Postgres, to the data store, to the namespace itself or to
// loopback, which is where Docker's resolver answers. Anything else is egress,
// and the test names it.
func TestTheServiceMakesNoOutboundConnection(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	s := newStack(t, withOverride("compose.egress.yml"), withEnv("QUOTACORE_CAPTURE_DIR", dir))

	if code, _, errw := s.adminCLI("init", "--yes"); code != 0 {
		t.Fatalf("init: exit %d: %s", code, errw)
	}
	token := s.runtimeKey()
	for range 20 {
		if r := consume(token, "local-dev", "llm.tokens.output", 3, newIdemKey()); r.status != 200 {
			t.Fatalf("consume: %d %s", r.status, r.body)
		}
	}
	s.status()
	if code, out, errw := s.cli("verify-all"); code != 0 {
		t.Fatalf("verify-all: %d %s %s", code, out, errw)
	}
	s.cli("rebuild")
	// The addresses, read while the containers still have them.
	ips := map[string]string{}
	for _, svc := range []string{"postgres", "valkey", "capture"} {
		id := strings.TrimSpace(s.compose("ps", "-q", svc))
		out, err := exec.Command("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", id).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, ip := range strings.Fields(string(out)) {
			ips[ip] = svc
		}
	}

	// The negative control: one connection from the service's namespace to a
	// documentation address that routes nowhere. The test must find it, or a
	// clean result means only that the capture saw nothing.
	exec.Command("docker", "run", "--rm", "--network", "container:"+strings.TrimSpace(s.compose("ps", "-q", "capture")),
		captureImage, "nc", "-z", "-w", "1", canary, "9").Run()

	s.compose("stop", "quotacore")
	time.Sleep(time.Second)
	s.compose("stop", "capture")

	out, err := exec.Command("docker", "run", "--rm", "-v", dir+":/capture:ro", captureImage,
		"tcpdump", "-nn", "-r", "/capture/egress.pcap",
		"(tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack == 0) or udp or (ip6 and not ip6 host ::1)").CombinedOutput()
	if err != nil {
		t.Fatalf("reading the capture: %v\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(dir, "egress.pcap")); err != nil || fi.Size() < 1000 {
		t.Fatalf("the capture is empty or missing: %v", err)
	}
	flows, toStores, canaries := 0, 0, 0
	for _, line := range strings.Split(string(out), "\n") {
		m := opened.FindStringSubmatch(line)
		if m == nil || strings.Contains(line, " In ") {
			// Not a flow, or one the host opened towards the published port.
			continue
		}
		host, port := splitDst(strings.TrimSuffix(m[5], ":"))
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLinkLocalMulticast() {
			// The kernel's own IPv6 neighbour discovery and multicast listener
			// reports, to ff02::/16: a link-local multicast never leaves the
			// link, and no process sent it.
			continue
		}
		flows++
		switch {
		case ip == nil:
			t.Errorf("unparsed destination in %q", line)
		case host == canary:
			canaries++
		case ip.IsLoopback():
		case ips[host] == "capture":
			// The namespace itself: the published port, reached from the host.
		case ips[host] == "postgres" && port == "5432", ips[host] == "valkey" && port == "6379":
			toStores++
		default:
			t.Errorf("an outbound flow to %s:%s, which is neither store nor loopback: %s", host, port, line)
		}
	}
	if canaries == 0 {
		t.Fatalf("the capture did not see the control connection to %s, so a clean capture proves nothing:\n%s", canary, out)
	}
	if toStores == 0 {
		t.Fatalf("the capture saw %d flows and none to the stores, so it did not capture the service:\n%s", flows, out)
	}
	t.Logf("%d flows opened, %d of them to the two stores", flows, toStores)
}

// splitDst splits tcpdump's "a.b.c.d.port" (or "addr.port" for IPv6) into
// the address and the port.
func splitDst(s string) (string, string) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}
