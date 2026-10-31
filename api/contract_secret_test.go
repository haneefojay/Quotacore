package api

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestOnlyKeyCreatedCarriesASecret is IP-09 DoD 2 (INV-K3) at the contract
// level: the plaintext key is a property of KeyCreated, the create's response,
// and no other schema has a property that could carry a secret, so no route
// added later can return one without this test being edited. IP-21 edited it
// once, for the webhook secret a create and a rotation return.
func TestOnlyKeyCreatedCarriesASecret(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(Document, &doc); err != nil {
		t.Fatal(err)
	}
	secretish := func(name string) bool {
		n := strings.ToLower(name)
		// apply-now's confirmation token is a single-use nonce, not a
		// credential (DR-046).
		return n == "key" || n == "key_hash" || n == "hash" || n == "api_key" ||
			strings.Contains(n, "secret") || strings.Contains(n, "plaintext") ||
			(strings.Contains(n, "token") && n != "confirmation_token")
	}
	found := false
	for name, s := range doc.Components.Schemas {
		for prop := range s.Properties {
			if !secretish(prop) {
				continue
			}
			if name == "KeyCreated" && prop == "key" {
				found = true
				continue
			}
			// A webhook's signing secret is returned by its create and its rotation, through
			// the one schema they share, and is derived rather than stored (Q-40). Its
			// generation is a counter that names the secret without carrying it.
			if name == "WebhookWithSecret" && prop == "secret" {
				continue
			}
			if (name == "Webhook" || name == "WebhookWithSecret") && prop == "secret_generation" {
				continue
			}
			t.Errorf("%s.%s could carry a secret, and only KeyCreated.key may", name, prop)
		}
	}
	if !found {
		t.Error("KeyCreated.key is gone, so the create no longer returns the key")
	}
}
