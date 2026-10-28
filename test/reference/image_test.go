package reference_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// maxImageBytes is deployment.md section 3's target, enforced rather than
// hoped for: a smaller image is a faster pull on the customer's connection.
const maxImageBytes = 30 << 20

// TestTheImageRunsUnprivileged (IP-15 DoD 10, NFR-S6): the running container
// is uid 65532, on a read-only root, with no capability and no way to gain
// one, publishing only 8080 on host loopback, and its healthcheck is the
// binary's own (D4). The process half is IP-13's.
func TestTheImageRunsUnprivileged(t *testing.T) {
	s := newStack(t)
	id := strings.TrimSpace(s.compose("ps", "-q", "quotacore"))
	out, err := exec.Command("docker", "inspect", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	var c []struct {
		Config struct {
			User        string
			Healthcheck struct{ Test []string }
			Entrypoint  []string
		}
		HostConfig struct {
			ReadonlyRootfs bool
			CapDrop        []string
			CapAdd         []string
			SecurityOpt    []string
			Privileged     bool
			PortBindings   map[string][]struct{ HostIp, HostPort string }
			Tmpfs          map[string]string
		}
		State struct{ Health struct{ Status string } }
	}
	if err := json.Unmarshal(out, &c); err != nil || len(c) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	got := c[0]
	if got.Config.User != "65532:65532" {
		t.Errorf("user %q, want 65532:65532", got.Config.User)
	}
	if !got.HostConfig.ReadonlyRootfs {
		t.Error("the root filesystem is writable")
	}
	if strings.Join(got.HostConfig.CapDrop, ",") != "ALL" || len(got.HostConfig.CapAdd) != 0 || got.HostConfig.Privileged {
		t.Errorf("capabilities: drop %v, add %v, privileged %v", got.HostConfig.CapDrop, got.HostConfig.CapAdd, got.HostConfig.Privileged)
	}
	if !strings.Contains(strings.Join(got.HostConfig.SecurityOpt, ","), "no-new-privileges:true") {
		t.Errorf("security options %v lack no-new-privileges", got.HostConfig.SecurityOpt)
	}
	if _, ok := got.HostConfig.Tmpfs["/tmp"]; !ok {
		t.Error("/tmp is not a tmpfs")
	}
	if len(got.HostConfig.PortBindings) != 1 {
		t.Errorf("published ports %v, want 8080 alone", got.HostConfig.PortBindings)
	}
	for port, binds := range got.HostConfig.PortBindings {
		if port != "8080/tcp" {
			t.Errorf("publishes %s", port)
		}
		for _, b := range binds {
			if b.HostIp != "127.0.0.1" {
				t.Errorf("%s is published on %q, not host loopback", port, b.HostIp)
			}
		}
	}
	if strings.Join(got.Config.Healthcheck.Test, " ") != "CMD /usr/local/bin/quotacore healthcheck" {
		t.Errorf("healthcheck %v", got.Config.Healthcheck.Test)
	}
	if got.State.Health.Status != "healthy" {
		t.Errorf("health %q after up --wait", got.State.Health.Status)
	}
	if strings.Join(got.Config.Entrypoint, " ") != "/usr/local/bin/quotacore" {
		t.Errorf("entrypoint %v, want the binary (IP-14 DoD 7's image half)", got.Config.Entrypoint)
	}
	// The process really is unprivileged, as the kernel sees it.
	if top := s.compose("top", "quotacore"); !strings.Contains(top, "65532") {
		t.Errorf("docker compose top shows no process as uid 65532:\n%s", top)
	}
	// And the CLI in the image is the same binary that serves (IP-14 DoD 7).
	if code, out, errw := s.cli("status"); code != 0 || !strings.Contains(out, `"mode": "normal"`) {
		t.Errorf("quotacore status in the container: exit %d, %s %s", code, out, errw)
	}
}

func TestTheImageIsSmallAndStatic(t *testing.T) {
	requireReference(t)
	out, err := exec.Command("docker", "image", "inspect", image(), "--format", "{{.Size}} {{.Os}} {{len .RootFS.Layers}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	var size, layers int
	var osName string
	if _, err := fmt.Sscan(string(out), &size, &osName, &layers); err != nil {
		t.Fatalf("inspect %q: %v", out, err)
	}
	if size > maxImageBytes {
		t.Errorf("the image is %d bytes, over deployment.md section 3's %d", size, maxImageBytes)
	}
	// scratch plus the binary and /tmp: no base layers to carry a package
	// (Q2, NFR-C4).
	if layers > 2 {
		t.Errorf("the image has %d layers; a scratch image with the binary and /tmp has 2", layers)
	}
}
