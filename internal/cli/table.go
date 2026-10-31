package cli

import (
	"encoding/json"
	"net/url"
	"strings"

	contract "github.com/quotacore/quotacore/api"
)

// Kind is how a command is carried out.
type Kind int

const (
	// Remote is one admin operation of the contract, over HTTP.
	Remote Kind = iota
	// Local runs in this process against the configuration, or needs nothing.
	Local
	// Composite is init, the one command that makes several calls (IP-14 D3).
	Composite
)

// Command is one row of the mapping table in docs/architecture/cli.md. A
// remote command names its operation by operationId, and the method and path
// are read from the contract, so the table cannot disagree with it.
type Command struct {
	Name    string   // "tenant override set"
	Kind    Kind     //
	Op      string   // the operationId of a Remote command
	Calls   []string // the operationIds a Composite command makes, in order
	Args    []string // the positional arguments, in the order the path takes them
	Summary string
	// List marks a paged read, which takes --limit, --cursor and --all.
	List bool
	// Writes marks a command whose call changes state; it is never retried.
	Writes bool
	setup  func(f *flags) build
}

// build turns the parsed flags and positional arguments into a request.
type build func(pos []string) (request, error)

// body is a generated request type: it encodes as the contract's JSON and
// validates as the contract does, so a body the service would refuse is
// refused before it is sent.
type body interface {
	MarshalJSON() ([]byte, error)
	UnmarshalJSON([]byte) error
	Validate() error
}

type request struct {
	query url.Values
	body  body
}

// Commands is the mapping table: every command the binary has, and nothing
// else. TestTheMappingTableIsTheBinary holds it to cli.md in both directions.
func Commands() []Command { return commands }

// Lookup finds the command args name, and the arguments after its name.
func Lookup(args []string) (Command, []string, bool) {
	for n := min(3, len(args)); n > 0; n-- {
		name := strings.Join(args[:n], " ")
		for _, c := range commands {
			if c.Name == name {
				return c, args[n:], true
			}
		}
	}
	return Command{}, nil, false
}

// groups are the first words that are not commands themselves.
func isGroup(word string) bool {
	for _, c := range commands {
		if strings.HasPrefix(c.Name, word+" ") {
			return true
		}
	}
	return false
}

var commands = []Command{
	// Features.
	{Name: "feature list", Op: "listFeatures", List: true, Summary: "List features", setup: featureList},
	{Name: "feature get", Op: "getFeature", Args: []string{"feature_key"}, Summary: "Show one feature"},
	{Name: "feature create", Op: "createFeature", Writes: true, Summary: "Create a feature", setup: featureCreate},
	{Name: "feature update", Op: "updateFeature", Args: []string{"feature_key"}, Writes: true,
		Summary: "Edit a feature's name, description or unit", setup: featureUpdate},
	{Name: "feature delete", Op: "deleteFeature", Args: []string{"feature_key"}, Writes: true,
		Summary: "Delete a feature no plan references", setup: reasonOnly(func() body { return &contract.DeleteRequest{} })},
	{Name: "feature archive", Op: "archiveFeature", Args: []string{"feature_key"}, Writes: true,
		Summary: "Archive a feature, keeping its balances", setup: versionReason(func() body { return &contract.ArchiveRequest{} })},

	// Plans.
	{Name: "plan list", Op: "listPlans", List: true, Summary: "List plans"},
	{Name: "plan get", Op: "getPlan", Args: []string{"plan"}, Summary: "Show one plan, by id or plan key"},
	{Name: "plan create", Op: "createPlan", Writes: true, Summary: "Create a plan with its entitlements", setup: planCreate},
	{Name: "plan update", Op: "updatePlan", Args: []string{"plan"}, Writes: true,
		Summary: "Edit a plan; --entitlement replaces the whole set", setup: planUpdate},
	{Name: "plan delete", Op: "deletePlan", Args: []string{"plan"}, Writes: true,
		Summary: "Delete a plan no tenant is on", setup: reasonOnly(func() body { return &contract.DeleteRequest{} })},
	{Name: "plan archive", Op: "archivePlan", Args: []string{"plan"}, Writes: true,
		Summary: "Archive a plan", setup: versionReason(func() body { return &contract.ArchiveRequest{} })},
	{Name: "plan apply", Op: "applyPlan", Args: []string{"plan"}, Writes: true,
		Summary: "Re-anchor every tenant on the plan now (DR-046 asks for a token above the threshold)", setup: planApply},
	{Name: "plan impact", Op: "planImpact", Args: []string{"plan"}, Summary: "Preview how many tenants an apply would re-anchor"},

	// Tenants.
	{Name: "tenant list", Op: "listTenants", List: true, Summary: "List tenants", setup: tenantList},
	{Name: "tenant get", Op: "getTenant", Args: []string{"tenant"}, Summary: "Show one tenant, by id or external id"},
	{Name: "tenant create", Op: "createTenant", Writes: true, Summary: "Provision a tenant on a plan", setup: tenantCreate},
	{Name: "tenant update", Op: "updateTenant", Args: []string{"tenant"}, Writes: true,
		Summary: "Edit a tenant; a new --plan is a hard reset (DR-013)", setup: tenantUpdate},
	{Name: "tenant delete", Op: "deleteTenant", Args: []string{"tenant"}, Writes: true,
		Summary: "Soft-delete a tenant; there is no undelete", setup: reasonOnly(func() body { return &contract.DeleteRequest{} })},
	{Name: "tenant suspend", Op: "suspendTenant", Args: []string{"tenant"}, Writes: true,
		Summary: "Suspend a tenant", setup: versionReason(func() body { return &contract.ReasonRequest{} })},
	{Name: "tenant resume", Op: "resumeTenant", Args: []string{"tenant"}, Writes: true,
		Summary: "Resume a suspended tenant", setup: versionReason(func() body { return &contract.ReasonRequest{} })},
	{Name: "tenant override set", Op: "upsertTenantOverride", Args: []string{"tenant"}, Writes: true,
		Summary: "Set or replace one feature's override", setup: overrideSet},
	{Name: "tenant override clear", Op: "clearTenantOverride", Args: []string{"tenant", "feature_key"}, Writes: true,
		Summary: "Clear one feature's override", setup: overrideClear},

	// The three an operator reaches for in a dispute (DoD 2).
	{Name: "grant", Op: "grantTenant", Args: []string{"tenant"}, Writes: true,
		Summary: "Add bonus quota for the current cycle; a retried grant is a second grant", setup: grant},
	{Name: "set", Op: "setTenantBalance", Args: []string{"tenant"}, Writes: true,
		Summary: "Set a balance to an exact value", setup: setBalance},
	{Name: "rollover", Op: "forceRollover", Args: []string{"tenant"}, Writes: true,
		Summary: "Roll one feature's cycle over now", setup: rollover},
	{Name: "events", Op: "listUsageEvents", Args: []string{"tenant"}, List: true,
		Summary: "Read a tenant's usage ledger", setup: events},

	// Keys.
	{Name: "key list", Op: "listKeys", List: true, Summary: "List API keys; no secret is ever shown"},
	{Name: "key create", Op: "createKey", Writes: true,
		Summary: "Issue an API key; its plaintext is printed once", setup: keyCreate},
	{Name: "key revoke", Op: "revokeKey", Args: []string{"key_id"}, Writes: true, Summary: "Revoke an API key by its id"},
	{Name: "key mint", Kind: Local, Summary: "Print a new admin token for QUOTACORE_BOOTSTRAP_ADMIN_KEY; touches nothing"},

	// Reads.
	{Name: "audit", Op: "listAudit", List: true, Summary: "Read the audit log", setup: audit},
	{Name: "status", Op: "status", Summary: "Show the instance's status"},

	{Name: "init", Kind: Composite, Calls: []string{"createFeature", "createPlan", "createTenant", "createKey", "balance"},
		Writes: true, Summary: "Create a feature, a plan, a tenant and a runtime key, and show the first cycle"},

	// The process and the local operations (IP-11).
	{Name: "serve", Kind: Local, Summary: "Run the service; no argument does the same"},
	{Name: "healthcheck", Kind: Local, Summary: "Exit 0 if the service on this host is ready; the image's healthcheck"},
	{Name: "verify-tenant", Kind: Local, Args: []string{"tenant"},
		Summary: "Check one tenant's ledger against itself and its balances"},
	{Name: "verify-all", Kind: Local, Summary: "Check every tenant"},
	{Name: "reconcile-report", Kind: Local, Summary: "Print the recorded reconciliation findings"},
	{Name: "rebuild", Kind: Local, Summary: "Rebuild lost balance hashes from the ledger"},
	{Name: "config bump", Kind: Local, Writes: true, Summary: "Make every instance re-read the configuration now, after a Postgres restore"},
	{Name: "audit note", Kind: Local, Writes: true, Summary: "Record a note in the audit log, such as a restore's truncation of history"},
}

// ---- builders --------------------------------------------------------------

func send(b body) (request, error) { return request{body: b}, nil }

func featureCreate(f *flags) build {
	key, name, unit := f.str("key", "the feature key, such as llm.tokens.output"), f.str("name", "a display name"), f.str("unit", "the unit, such as tokens")
	desc, kind := f.str("description", "free text"), f.str("kind", "metered (the default), boolean or rolling")
	f.need("key", "name", "unit")
	return func([]string) (request, error) {
		b := &contract.FeatureCreate{FeatureKey: contract.FeatureKey(*key), Name: *name, Unit: contract.Unit(*unit)}
		if f.has("description") {
			b.Description = contract.NewOptString(*desc)
		}
		if f.has("kind") {
			b.Kind = contract.NewOptFeatureCreateKind(contract.FeatureCreateKind(*kind))
		}
		return send(b)
	}
}

func featureUpdate(f *flags) build {
	version := f.str("version", "the version the last read showed (api-conventions.md section 10)")
	name, desc, unit := f.str("name", "a new display name"), f.str("description", "new free text; '' clears it"), f.str("unit", "a new unit")
	f.need("version")
	return func([]string) (request, error) {
		v, err := parseInt("version", *version)
		if err != nil {
			return request{}, err
		}
		b := &contract.FeatureUpdate{Version: contract.Version(v)}
		if f.has("name") {
			b.Name = contract.NewOptString(*name)
		}
		if f.has("description") {
			b.Description = contract.NewOptString(*desc)
		}
		if f.has("unit") {
			b.Unit = contract.NewOptUnit(contract.Unit(*unit))
		}
		return send(b)
	}
}

// reasonOnly is a body of one required reason.
func reasonOnly(newBody func() body) func(f *flags) build {
	return func(f *flags) build {
		reason := f.str("reason", "why; required and audited")
		f.need("reason")
		return func([]string) (request, error) {
			return send(withFields(newBody(), map[string]any{"reason": *reason}))
		}
	}
}

// versionReason is a body of a version and a reason.
func versionReason(newBody func() body) func(f *flags) build {
	return func(f *flags) build {
		version, reason := f.str("version", "the version the last read showed"), f.str("reason", "why; required and audited")
		f.need("version", "reason")
		return func([]string) (request, error) {
			v, err := parseInt("version", *version)
			if err != nil {
				return request{}, err
			}
			return send(withFields(newBody(), map[string]any{"version": v, "reason": *reason}))
		}
	}
}

// withFields fills a generated body from its JSON fields, which is how the
// small bodies that differ only in their type are built once.
func withFields(b body, fields map[string]any) body {
	raw, _ := json.Marshal(fields)
	if err := b.UnmarshalJSON(raw); err != nil {
		return invalid{err}
	}
	return b
}

// invalid is a body that could not be built; it refuses validation.
type invalid struct{ err error }

func (i invalid) MarshalJSON() ([]byte, error) { return nil, i.err }
func (i invalid) UnmarshalJSON([]byte) error   { return i.err }
func (i invalid) Validate() error              { return i.err }

func planCreate(f *flags) build {
	key, name, desc := f.str("key", "the plan key, such as starter"), f.str("name", "a display name"), f.str("description", "free text")
	ents := f.list("entitlement", "<feature_key>=<limit>/<interval>, <feature_key>=<limit>/<rolling window> for a rolling feature, or a bare <feature_key> for a boolean feature; repeat for each feature")
	f.need("key", "name", "entitlement")
	return func([]string) (request, error) {
		es, err := entitlements(*ents)
		if err != nil {
			return request{}, err
		}
		b := &contract.PlanCreate{PlanKey: contract.PlanKey(*key), Name: *name, Entitlements: es}
		if f.has("description") {
			b.Description = contract.NewOptString(*desc)
		}
		return send(b)
	}
}

func planUpdate(f *flags) build {
	version := f.str("version", "the version the last read showed")
	name, desc := f.str("name", "a new display name"), f.str("description", "new free text; '' clears it")
	ents := f.list("entitlement", "<feature_key>=<limit>/<interval>, <feature_key>=<limit>/<rolling window> for a rolling feature, or a bare <feature_key> for a boolean feature; the set given replaces the whole set")
	f.need("version")
	return func([]string) (request, error) {
		v, err := parseInt("version", *version)
		if err != nil {
			return request{}, err
		}
		b := &contract.PlanUpdate{Version: contract.Version(v)}
		if f.has("name") {
			b.Name = contract.NewOptString(*name)
		}
		if f.has("description") {
			b.Description = contract.NewOptString(*desc)
		}
		if f.has("entitlement") {
			if b.Entitlements, err = entitlements(*ents); err != nil {
				return request{}, err
			}
		}
		return send(b)
	}
}

func planApply(f *flags) build {
	reason := f.str("reason", "why; required and audited")
	token := f.str("confirmation-token", "the token a 409 confirmation_required returned, to re-issue with")
	f.need("reason")
	return func([]string) (request, error) {
		b := &contract.PlanApplyRequest{Reason: *reason}
		if f.has("confirmation-token") {
			b.ConfirmationToken = contract.NewOptString(*token)
		}
		return send(b)
	}
}

func featureList(f *flags) build {
	kind := f.str("kind", "only features of this kind: metered, boolean or rolling")
	return func([]string) (request, error) {
		q := url.Values{}
		f.query(q, map[string]*string{"kind": kind})
		return request{query: q}, nil
	}
}

func tenantList(f *flags) build {
	plan := f.str("plan", "only tenants on this plan key")
	return func([]string) (request, error) {
		q := url.Values{}
		f.query(q, map[string]*string{"plan": plan})
		return request{query: q}, nil
	}
}

func metadata(v string) (contract.TenantMetadata, error) {
	var m contract.TenantMetadata
	if err := m.UnmarshalJSON([]byte(v)); err != nil {
		return nil, usagef("--metadata is not a JSON object: %v", err)
	}
	return m, nil
}

func tenantCreate(f *flags) build {
	ext, name, plan := f.str("external-id", "your identifier for the tenant"), f.str("name", "a display name"), f.str("plan", "the plan key")
	zone, meta := f.str("timezone", "an IANA zone; UTC by default"), f.str("metadata", "a JSON object")
	f.need("external-id", "name", "plan")
	return func([]string) (request, error) {
		b := &contract.TenantCreate{ExternalID: contract.ExternalId(*ext), DisplayName: *name, PlanKey: contract.PlanKey(*plan)}
		if f.has("timezone") {
			b.Timezone = contract.NewOptString(*zone)
		}
		if f.has("metadata") {
			m, err := metadata(*meta)
			if err != nil {
				return request{}, err
			}
			b.Metadata = contract.NewOptTenantMetadata(m)
		}
		return send(b)
	}
}

func tenantUpdate(f *flags) build {
	version := f.str("version", "the version the last read showed")
	name, plan := f.str("name", "a new display name"), f.str("plan", "a new plan key; a hard reset (DR-013)")
	zone, meta := f.str("timezone", "a new IANA zone"), f.str("metadata", "a JSON object replacing the metadata")
	f.need("version")
	return func([]string) (request, error) {
		v, err := parseInt("version", *version)
		if err != nil {
			return request{}, err
		}
		b := &contract.TenantUpdate{Version: contract.Version(v)}
		if f.has("name") {
			b.DisplayName = contract.NewOptString(*name)
		}
		if f.has("plan") {
			b.PlanKey = contract.NewOptPlanKey(contract.PlanKey(*plan))
		}
		if f.has("timezone") {
			b.Timezone = contract.NewOptString(*zone)
		}
		if f.has("metadata") {
			m, err := metadata(*meta)
			if err != nil {
				return request{}, err
			}
			b.Metadata = contract.NewOptTenantMetadata(m)
		}
		return send(b)
	}
}

func overrideSet(f *flags) build {
	feature := f.str("feature", "the feature key")
	limit := f.str("limit", "the overriding limit, or inherit to keep the plan's")
	interval := f.str("interval", "the overriding reset interval, or inherit to keep the plan's")
	applyAt := f.str("apply-at", "immediate or next_boundary")
	version := f.str("version", "the override's version, when replacing one")
	f.need("feature", "limit", "interval")
	return func([]string) (request, error) {
		b := &contract.OverrideUpsert{FeatureKey: contract.FeatureKey(*feature)}
		if *limit == "inherit" {
			b.LimitValue.SetToNull()
		} else {
			n, err := parseInt("limit", *limit)
			if err != nil {
				return request{}, err
			}
			b.LimitValue.SetTo(contract.Limit(n))
		}
		if *interval == "inherit" {
			b.Interval.SetToNull()
		} else {
			b.Interval.SetTo(contract.ResetInterval(*interval))
		}
		if f.has("apply-at") {
			b.ApplyAt = contract.NewOptOverrideApplyAt(contract.OverrideApplyAt(*applyAt))
		}
		if f.has("version") {
			v, err := parseInt("version", *version)
			if err != nil {
				return request{}, err
			}
			b.Version = contract.NewOptVersion(contract.Version(v))
		}
		return send(b)
	}
}

func overrideClear(f *flags) build {
	applyAt := f.str("apply-at", "immediate or next_boundary")
	f.need("apply-at")
	return func([]string) (request, error) {
		return send(&contract.OverrideClearRequest{ApplyAt: contract.OverrideApplyAt(*applyAt)})
	}
}

func grant(f *flags) build {
	feature, amount, reason := f.str("feature", "the feature key"), f.str("amount", "how much to add"), f.str("reason", "why; required and audited")
	f.need("feature", "amount", "reason")
	return func([]string) (request, error) {
		n, err := parseInt("amount", *amount)
		if err != nil {
			return request{}, err
		}
		return send(&contract.GrantRequest{FeatureKey: contract.FeatureKey(*feature), Amount: contract.Amount(n), Reason: *reason})
	}
}

func setBalance(f *flags) build {
	feature, value, reason := f.str("feature", "the feature key"), f.str("value", "the balance after this call"), f.str("reason", "why; required and audited")
	f.need("feature", "value", "reason")
	return func([]string) (request, error) {
		n, err := parseInt("value", *value)
		if err != nil {
			return request{}, err
		}
		return send(&contract.SetRequest{FeatureKey: contract.FeatureKey(*feature), Value: contract.Amount(n), Reason: *reason})
	}
}

func rollover(f *flags) build {
	feature, reason := f.str("feature", "the feature key"), f.str("reason", "why; required and audited")
	f.need("feature", "reason")
	return func([]string) (request, error) {
		return send(&contract.ForceRolloverRequest{FeatureKey: contract.FeatureKey(*feature), Reason: *reason})
	}
}

func events(f *flags) build {
	names := map[string]*string{
		"feature":         f.str("feature", "only this feature key"),
		"type":            f.str("type", "only this event type"),
		"source":          f.str("source", "only events from this source"),
		"idempotency-key": f.str("idempotency-key", "only events recorded under this Idempotency-Key"),
		"from":            f.str("from", "from this RFC 3339 instant"),
		"to":              f.str("to", "to this RFC 3339 instant"),
	}
	return func([]string) (request, error) {
		q := url.Values{}
		f.query(q, names)
		return request{query: q}, nil
	}
}

func keyCreate(f *flags) build {
	name, scope, expires := f.str("name", "what the key is for; the audit actor"), f.str("scope", "runtime or admin"), f.str("expires-at", "an RFC 3339 instant")
	f.need("name", "scope")
	return func([]string) (request, error) {
		b := &contract.KeyCreate{Name: *name, Scope: contract.KeyScope(*scope)}
		if f.has("expires-at") {
			t, err := parseTime("expires-at", *expires)
			if err != nil {
				return request{}, err
			}
			b.ExpiresAt = contract.NewOptTimestamp(contract.Timestamp(t))
		}
		return send(b)
	}
}

func audit(f *flags) build {
	names := map[string]*string{
		"tenant": f.str("tenant", "only this tenant's external id"),
		"from":   f.str("from", "from this RFC 3339 instant"),
		"to":     f.str("to", "to this RFC 3339 instant"),
	}
	return func([]string) (request, error) {
		q := url.Values{}
		f.query(q, names)
		return request{query: q}, nil
	}
}
