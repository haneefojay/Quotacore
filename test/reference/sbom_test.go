package reference_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const distroless = "gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"

func scan(t *testing.T, img, sbom string) (int, string) {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		if pwsh, err = exec.LookPath("powershell"); err != nil {
			t.Fatal("neither pwsh nor powershell is on PATH, so the licence scan cannot run")
		}
	}
	args := []string{"-NoProfile", "-File", "tools/license-scan.ps1", "-Image", img}
	if sbom != "" {
		args = append(args, "-SbomPath", sbom)
	}
	c := exec.Command(pwsh, args...)
	c.Dir = repoRoot(t)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

// TestTheSbomCoversTheShippedImage (IP-15 DoD 11, NFR-C3, NFR-C4): the scan
// of the image itself passes, and its SBOM names the image, its base and the
// builder by digest, and every module of the binary inside it.
func TestTheSbomCoversTheShippedImage(t *testing.T) {
	requireReference(t)
	sbom := filepath.Join(t.TempDir(), "sbom.cdx.json")
	code, out := scan(t, image(), sbom)
	if code != 0 {
		t.Fatalf("the licence scan of %s exited %d:\n%s", image(), code, out)
	}
	var doc struct {
		Metadata struct {
			Component  struct{ Type, Name string }
			Properties []struct{ Name, Value string }
		}
		Components []struct{ Type, Name, Purl string }
	}
	b, err := os.ReadFile(sbom)
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("SBOM unreadable: %v", err)
	}
	if doc.Metadata.Component.Type != "container" || doc.Metadata.Component.Name != image() {
		t.Errorf("the SBOM's subject is %+v, not the image", doc.Metadata.Component)
	}
	if len(doc.Components) == 0 || doc.Components[0].Type != "container" || doc.Components[0].Name != "scratch" {
		t.Errorf("the SBOM does not name the base image first: %+v", doc.Components[:min(1, len(doc.Components))])
	}
	props := map[string]string{}
	for _, p := range doc.Metadata.Properties {
		props[p.Name] = p.Value
	}
	if !strings.Contains(props["quotacore:image:builder"], "@sha256:") {
		t.Errorf("the builder is not named by digest: %q", props["quotacore:image:builder"])
	}
	modules := 0
	for _, c := range doc.Components {
		if strings.HasPrefix(c.Purl, "pkg:golang/") {
			modules++
		}
	}
	if modules < 10 {
		t.Errorf("the SBOM lists %d modules of the binary", modules)
	}
}

// TestTheImageScanRefusesACopyleftBase is the scan's negative control: the
// same binary on the distroless base the specification first named fails,
// naming the packages that carry GPL-2 and MPL-2.0 text (Q2).
func TestTheImageScanRefusesACopyleftBase(t *testing.T) {
	requireReference(t)
	dir := t.TempDir()
	df := "FROM " + image() + " AS app\nFROM " + distroless + "\nCOPY --from=app /usr/local/bin/quotacore /usr/local/bin/quotacore\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "quotacore-scan-control:test"
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("building the control image: %v\n%s", err, out)
	}
	defer exec.Command("docker", "image", "rm", "-f", tag).Run()
	code, out := scan(t, tag, "")
	if code != 1 {
		t.Fatalf("the scan of a distroless image exited %d:\n%s", code, out)
	}
	for _, pkg := range []string{"base-files", "netbase", "ca-certificates"} {
		if !strings.Contains(out, "OS package "+pkg) {
			t.Errorf("the scan did not name %s:\n%s", pkg, out)
		}
	}
}
