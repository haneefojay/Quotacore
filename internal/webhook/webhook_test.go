package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quotacore/quotacore/internal/dataplane"
)

// The signing key is a fixture, not a secret.
var fixtureKey = []byte("test-only-webhook-signing-key-not-a-secret")

var secretShape = regexp.MustCompile(`^qcwh_[A-Za-z0-9_-]{43}$`)

func TestTheSecretIsDerivedAndChangesWithItsInputs(t *testing.T) {
	id := uuid.NewString()
	a := Secret(fixtureKey, id, 1)
	if !secretShape.MatchString(a) {
		t.Fatalf("the secret %q does not have the contract's shape", a)
	}
	if Secret(fixtureKey, id, 1) != a {
		t.Error("the same key, webhook and generation derived two secrets")
	}
	for name, other := range map[string]string{
		"another generation": Secret(fixtureKey, id, 2),
		"another webhook":    Secret(fixtureKey, uuid.NewString(), 1),
		"another key":        Secret(append([]byte("x"), fixtureKey...), id, 1),
	} {
		if other == a {
			t.Errorf("%s derived the same secret", name)
		}
	}
}

func delivery(t *testing.T) (string, string, []byte, time.Time) {
	t.Helper()
	return Secret(fixtureKey, uuid.NewString(), 1), uuid.NewString(), []byte(`{"event_id":"x","balance":10}`),
		time.Unix(1_790_000_000, 0)
}

// TestT20TheSignatureCoversBodyTimestampAndEventID is IP-21 DoD 5: changing
// any of the three, or signing with another secret, fails the check, and so
// does a timestamp outside the replay window either way.
func TestT20TheSignatureCoversBodyTimestampAndEventID(t *testing.T) {
	secret, id, body, at := delivery(t)
	h := Headers(secret, id, body, at)
	if err := Verify(secret, h, body, at); err != nil {
		t.Fatalf("a genuine delivery: %v", err)
	}
	if h.Get(HeaderDelivery) != id {
		t.Errorf("%s is %q, want the event_id", HeaderDelivery, h.Get(HeaderDelivery))
	}
	tampered := bytes.Clone(body)
	tampered[len(tampered)-2] = '1'
	cases := map[string]func() (http.Header, []byte){
		"a changed body": func() (http.Header, []byte) { return h, tampered },
		"a changed timestamp": func() (http.Header, []byte) {
			c := h.Clone()
			c.Set(HeaderTimestamp, strconv.FormatInt(at.Unix()+1, 10))
			return c, body
		},
		"a changed event_id": func() (http.Header, []byte) {
			c := h.Clone()
			c.Set(HeaderDelivery, uuid.NewString())
			return c, body
		},
		"another secret": func() (http.Header, []byte) {
			return Headers(Secret(fixtureKey, uuid.NewString(), 1), id, body, at), body
		},
	}
	for name, mk := range cases {
		hh, b := mk()
		if err := Verify(secret, hh, b, at); !errors.Is(err, ErrSignatureMismatch) {
			t.Errorf("%s: %v, want a mismatch", name, err)
		}
	}
	for _, skew := range []time.Duration{ReplayWindow + time.Second, -ReplayWindow - time.Second} {
		if err := Verify(secret, h, body, at.Add(skew)); !errors.Is(err, ErrOutsideWindow) {
			t.Errorf("a receiver %s away: %v, want outside the window", skew, err)
		}
	}
	for _, skew := range []time.Duration{ReplayWindow, -ReplayWindow} {
		if err := Verify(secret, h, body, at.Add(skew)); err != nil {
			t.Errorf("a receiver exactly %s away: %v", skew, err)
		}
	}
	for _, name := range []string{HeaderDelivery, HeaderTimestamp, HeaderSignature} {
		c := h.Clone()
		c.Del(name)
		if err := Verify(secret, c, body, at); !errors.Is(err, ErrMissingHeader) {
			t.Errorf("without %s: %v", name, err)
		}
	}
	// A later scheme sent beside v1 does not stop v1 verifying.
	c := h.Clone()
	c.Set(HeaderSignature, "v2=00,"+h.Get(HeaderSignature))
	if err := Verify(secret, c, body, at); err != nil {
		t.Errorf("v1 beside another scheme: %v", err)
	}
}

func val(n int64) *int64 { return &n }

// TestT20AThresholdCrossingIsNotAnAbsoluteThreshold is IP-21 DoD 4 and DR-055:
// each fires on its own line, only on a fall, and both are labelled in one
// payload when one event crosses both.
func TestT20AThresholdCrossingIsNotAnAbsoluteThreshold(t *testing.T) {
	pct := Subscription{Type: ThresholdCrossing, FeatureKey: "f", Value: val(80)}
	abs := Subscription{Type: AbsoluteThreshold, FeatureKey: "f", Value: val(150)}
	cases := []struct {
		name               string
		s                  Subscription
		before, after, lim int64
		want               bool
	}{
		{"80% used is reached exactly", pct, 201, 200, 1000, true},
		{"80% used is passed", pct, 300, 100, 1000, true},
		{"still above 80% left of the line", pct, 300, 201, 1000, false},
		{"already past the line", pct, 200, 100, 1000, false},
		{"a refund back over the line", pct, 100, 300, 1000, false},
		{"100% is the balance reaching zero", Subscription{Type: ThresholdCrossing, Value: val(100)}, 1, 0, 1000, true},
		{"a zero limit has no fraction", pct, 1, 0, 0, false},
		{"150 left is reached exactly", abs, 151, 150, 1000, true},
		{"150 left is passed", abs, 400, 10, 1000, true},
		{"above 150", abs, 400, 151, 1000, false},
		{"0 left is the balance reaching zero", Subscription{Type: AbsoluteThreshold, Value: val(0)}, 5, 0, 10, true},
	}
	for _, c := range cases {
		if got := Crosses(c.s, c.before, c.after, c.lim); got != c.want {
			t.Errorf("%s: crossed %v, want %v", c.name, got, c.want)
		}
	}

	subs := []Subscription{pct, abs, {Type: ThresholdCrossing, FeatureKey: "other", Value: val(10)}}
	ev := dataplane.Event{ID: uuid.Must(uuid.NewV7()), FeatureKey: "f", Type: "consumed", Delta: -200, Balance: 100,
		OccurredAt: time.Now()}
	p, ok := Match(subs, ev, "acme", 1000)
	if !ok || len(p.Crossings) != 2 || p.Crossings[0] != (Crossing{ThresholdCrossing, 80}) || p.Crossings[1] != (Crossing{AbsoluteThreshold, 150}) {
		t.Fatalf("one event crossing both: %v %+v", ok, p)
	}
	if p.EventID != ev.ID.String() || p.TenantExternalID != "acme" || p.Balance != 100 || p.Delta != -200 {
		t.Errorf("the payload does not carry the event: %+v", p)
	}
	ev.Delta, ev.Balance = -50, 250 // from 300 to 250: past neither line
	if _, ok := Match(subs, ev, "acme", 1000); ok {
		t.Error("an event that crossed nothing matched")
	}
}

func TestPlanChangesAndRolloversMatchTheirSubscription(t *testing.T) {
	subs := []Subscription{{Type: PlanChange}, {Type: CycleRollover}}
	for typ, want := range map[string]bool{
		"plan_changed": true, "cycle_rolled_over": true, "force_rolled_over": true, "consumed": false, "granted": false,
	} {
		p, ok := Match(subs, dataplane.Event{ID: uuid.New(), Type: typ, Delta: 5, Balance: 5}, "t", 10)
		if ok != want {
			t.Errorf("%s matched %v, want %v", typ, ok, want)
		}
		raw, _ := json.Marshal(p)
		if !bytes.Contains(raw, []byte(`"crossings":[]`)) {
			t.Errorf("%s: crossings is not an empty array: %s", typ, raw)
		}
	}
	if _, ok := Match([]Subscription{{Type: CycleRollover}}, dataplane.Event{ID: uuid.New(), Type: "plan_changed"}, "t", 10); ok {
		t.Error("a rollover subscription matched a plan change")
	}
}

// recipe is the verification program webhooks.md section 4 shows, between
// its markers.
func recipe(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "webhooks.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	_, rest, ok := strings.Cut(text, "<!-- verify-recipe:start -->\n```go\n")
	if !ok {
		t.Fatal("webhooks.md has no verify-recipe block")
	}
	code, _, ok := strings.Cut(rest, "```\n<!-- verify-recipe:end -->")
	if !ok {
		t.Fatal("the verify-recipe block is not closed")
	}
	return code
}

// TestT20TheDocumentedRecipeVerifiesARealSignature is IP-21 DoD 6: the
// snippet in the customer's document, run as written, accepts a delivery this
// package signed, and refuses it with one byte of the body changed and with a
// timestamp outside the window it states.
func TestT20TheDocumentedRecipeVerifiesARealSignature(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a program")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "verify.go")
	if err := os.WriteFile(file, []byte(recipe(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	bin := filepath.Join(dir, "verify")
	build := exec.Command(gobin, "build", "-o", bin, file)
	build.Dir = dir
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the recipe does not build: %v\n%s", err, out)
	}
	secret := Secret(fixtureKey, uuid.NewString(), 1)
	id := uuid.Must(uuid.NewV7()).String()
	body := []byte(`{"event_id":"` + id + `","event_type":"consumed","delta":-10,"balance":90,"crossings":[]}`)
	run := func(body []byte, at time.Time) (int, string) {
		h := Headers(secret, id, body, at)
		cmd := exec.Command(bin)
		cmd.Stdin = bytes.NewReader(body)
		cmd.Env = []string{
			"QUOTACORE_WEBHOOK_SECRET=" + secret,
			"QUOTACORE_DELIVERY=" + h.Get(HeaderDelivery),
			"QUOTACORE_TIMESTAMP=" + h.Get(HeaderTimestamp),
			"QUOTACORE_SIGNATURE=" + h.Get(HeaderSignature),
		}
		out, err := cmd.Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), strings.TrimSpace(string(out))
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, strings.TrimSpace(string(out))
	}
	if code, out := run(body, time.Now()); code != 0 || out != "verified" {
		t.Fatalf("a genuine delivery: exit %d, %q", code, out)
	}
	// The signature is computed over the original; the body sent differs in
	// one byte.
	h := Headers(secret, id, body, time.Now())
	tampered := bytes.Clone(body)
	tampered[len(tampered)-3] = '1'
	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(tampered)
	cmd.Env = []string{"QUOTACORE_WEBHOOK_SECRET=" + secret, "QUOTACORE_DELIVERY=" + id,
		"QUOTACORE_TIMESTAMP=" + h.Get(HeaderTimestamp), "QUOTACORE_SIGNATURE=" + h.Get(HeaderSignature)}
	if err := cmd.Run(); err == nil {
		t.Error("the recipe accepted a body with one byte changed")
	}
	if code, out := run(body, time.Now().Add(-ReplayWindow-time.Minute)); code != 1 || out != "rejected" {
		t.Errorf("a delivery signed outside the window: exit %d, %q", code, out)
	}
	// The window the recipe enforces is the one the package and the document state.
	if !strings.Contains(recipe(t), "const window = "+strconv.Itoa(int(ReplayWindow/time.Second))) {
		t.Errorf("the recipe's window is not ReplayWindow, %s", ReplayWindow)
	}
}

// TestTheDocumentStatesTheWindow is the other half of IP-21 DoD 5: the
// anti-replay window is in the document a customer reads, as the number the
// code uses.
func TestTheDocumentStatesTheWindow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "webhooks.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "**" + strconv.Itoa(int(ReplayWindow/time.Second)) + " seconds**"
	if !strings.Contains(string(raw), want) {
		t.Errorf("webhooks.md does not state the window as %s", want)
	}
}
