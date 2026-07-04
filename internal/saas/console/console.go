package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

// maxBodyBytes bounds a console request body (key-create is the only writer with
// a body) so a caller cannot exhaust the BFF with an unbounded upload.
const maxBodyBytes = 64 << 10 // 64 KiB

// BillingReader is the narrow read seam the console uses for the billing view.
// The #212 billing service holds these behind public interfaces (the credit
// ledger, the status store, the spend-cap store); the console reads them
// directly rather than importing the whole Service so the BFF stays a thin,
// org-scoped read. Every method is org-scoped.
type BillingReader struct {
	Ledger billing.CreditLedger
	Status billing.StatusStore
	Caps   billing.SpendCapStore
	Rates  billing.Rates
}

// Deps wires the console BFF. The account service backs the keys and members
// views (and enforces membership exactly as the CLI verbs do); the usage store
// and price list back the usage/cost view; the billing reader backs the billing
// view; the sandbox control, template lister, and audit recorder are the seams.
// A nil seam is filled with its in-memory tested default so a caller can stand
// up a working, org-scoped BFF with just the account service.
type Deps struct {
	Accounts *saas.AccountService
	// Invitations is the org invitation seam (create/list/revoke/resend/
	// lookup/accept). Nil (the default) means invitations are NOT enabled on
	// this deployment: the invite endpoints return a clean "not enabled"
	// response instead of panicking. The production binary wires this from
	// the SAME saas.Store the AccountService uses.
	Invitations *saas.InvitationService
	Usage       usage.UsageStore
	Prices      usage.PriceList
	Billing     BillingReader
	Sandboxes   SandboxControl
	Templates   TemplateLister
	Audit       AuditRecorder
	Logs        LogStreamer
	Secrets     SecretStore
	Instruments InstrumentsSource
	ForkTree    ForkTreeSource
	Projects    ProjectStore
	Portal      PortalLinker
	// TopUp is the prepaid credit checkout seam. It starts a hosted checkout
	// session and returns the URL for the provider's payment page. Defaults to
	// noTopUp{} when billing is not configured; with an empty TopUpProductID the
	// endpoint returns 400 (top-up not configured) before the seam is called.
	TopUp TopUpLinker
	// TopUpProductID is the billing provider product that represents a credit
	// top-up. Empty means top-up is not enabled; the endpoint returns 400.
	TopUpProductID string
	// TopUpCurrency is the ISO 4217 currency code for top-up transactions.
	// Defaults to "EUR" when billing is configured.
	TopUpCurrency string
	// Retention is the per-org audit-retention policy seam. It stores and
	// exposes the retention window in days for each org; the GC sweep that
	// enforces the policy runs in the controller (issue #163). Defaults to the
	// in-memory fake so the BFF is safe to instantiate without a real store.
	Retention RetentionStore
	// DataRetention is the per-org data-retention policy seam. It stores and
	// exposes the multi-dimensional retention policy (sandbox metadata, logs,
	// usage) and a legal-hold flag for each org. The GC sweep that enforces the
	// policy runs in the controller (issue #163); a legal hold pauses all
	// automated deletion. Defaults to the in-memory fake so the BFF is safe to
	// instantiate without a real store.
	DataRetention DataRetentionStore
	// Sessions is the account-scoped session-listing seam. It is used by the
	// account-session endpoints (list, revoke one, revoke all) and defaults to a
	// no-op in-memory implementation so the BFF is safe to instantiate without a
	// real session store.
	Sessions SessionLister
	// Sinks is the org-scoped audit-sink registry. Defaults to an empty
	// in-memory registry so the BFF is safe to instantiate without a real sink
	// store. When set, New wraps deps.Audit with a DispatchingRecorder so every
	// audit event is best-effort forwarded to the org's enabled sinks.
	Sinks SinkRegistry
	// CustomRoles is the org-scoped custom-role definition store. It is used by
	// the permission resolver to look up custom role names that do not match any
	// built-in role. Defaults to an empty in-memory store so the BFF is safe to
	// instantiate without a real custom-role backend.
	CustomRoles CustomRoleStore
	// ProjectMembers is the per-org, per-project membership store. It backs the
	// GET/POST/DELETE /console/projects/{id}/members endpoints. Defaults to an
	// empty in-memory store so the BFF is safe to instantiate without a real
	// backend.
	ProjectMembers ProjectMembershipStore
	// ResourceProjects is the org-scoped store that maps resources (sandboxes,
	// etc.) to projects. It backs the PUT /console/sandboxes/{id}/project
	// endpoint and is consulted in list and inspect handlers to populate each
	// sandbox's project_id field. Defaults to an empty in-memory store so the
	// BFF is safe to instantiate without a real backend.
	ResourceProjects ResourceProjectStore
	// Capabilities is the deployment edition + feature flags the console
	// advertises at GET /console/capabilities. Left zero, it defaults to the
	// self-hosted community edition.
	Capabilities Capabilities
	Log          *slog.Logger
	Now          func() time.Time
}

// Console is the org-scoped BFF. It reads the caller and org from the request
// context (attached by the gateway / session auth), and every endpoint returns
// ONLY the caller's org data. It never logs or returns a key value except the
// one-time raw key on create.
type Console struct {
	deps            Deps
	mux             *http.ServeMux
	inviteRateLimit *inviteRateLimiter
}

// New builds a Console, filling in in-memory seam defaults and a default price
// list / rate table / clock where the caller left them nil.
func New(deps Deps) *Console {
	if deps.Sandboxes == nil {
		deps.Sandboxes = NewMemSandboxControl()
	}
	if deps.Templates == nil {
		deps.Templates = NewMemTemplateLister()
	}
	if deps.Audit == nil {
		deps.Audit = NewMemAuditLog()
	}
	if deps.Secrets == nil {
		deps.Secrets = NewMemSecretStore()
	}
	if deps.Instruments == nil {
		deps.Instruments = NewMemInstruments()
	}
	if deps.ForkTree == nil {
		deps.ForkTree = NewMemForkTree()
	}
	if deps.Projects == nil {
		deps.Projects = NewMemProjectStore()
	}
	if deps.Portal == nil {
		deps.Portal = noPortal{}
	}
	if deps.TopUp == nil {
		deps.TopUp = noTopUp{}
	}
	if deps.TopUpCurrency == "" {
		deps.TopUpCurrency = "EUR"
	}
	if deps.Retention == nil {
		deps.Retention = NewMemRetentionStore()
	}
	if deps.DataRetention == nil {
		deps.DataRetention = NewMemDataRetentionStore()
	}
	if deps.Sessions == nil {
		deps.Sessions = noopSessionLister{}
	}
	if deps.Sinks == nil {
		deps.Sinks = NewMemSinkRegistry()
	}
	if deps.CustomRoles == nil {
		deps.CustomRoles = NewMemCustomRoleStore()
	}
	if deps.ProjectMembers == nil {
		deps.ProjectMembers = NewMemProjectMembershipStore()
	}
	if deps.ResourceProjects == nil {
		deps.ResourceProjects = NewMemResourceProjectStore()
	}
	if deps.Logs == nil {
		// Default to an authorizing streamer over the (already-defaulted)
		// sandbox control with an empty transport: it enforces org ownership and
		// streams nothing, so the endpoint is safe before the real forkd
		// transport is wired in.
		deps.Logs = NewAuthorizingLogStreamer(deps.Sandboxes, NewMemRawLogStreamer())
	}
	if (deps.Prices == usage.PriceList{}) {
		deps.Prices = usage.DefaultPriceList()
	}
	if (deps.Billing.Rates == billing.Rates{}) {
		deps.Billing.Rates = billing.DefaultRates()
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Capabilities.Edition == "" {
		deps.Capabilities = defaultCapabilities()
	}
	// Wrap the audit recorder with a DispatchingRecorder so every audit event
	// is best-effort forwarded to the org's enabled sinks. The webhook sink is
	// the default dispatcher; tests inject a sinkFunc via NewDispatchingRecorder.
	deps.Audit = NewDispatchingRecorder(deps.Audit, deps.Sinks, newWebhookSink()).
		withLog(deps.Log)
	c := &Console{deps: deps, inviteRateLimit: newInviteRateLimiter(inviteRateLimitPerOrg, inviteRateLimitWindow)}
	c.routes()
	return c
}

// routes registers the BFF endpoints. All are mounted under /console; the
// gateway / session middleware attaches the org and caller context before
// dispatch.
func (c *Console) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /console/capabilities", c.handleCapabilities)
	mux.HandleFunc("GET /console/keys", c.handleListKeys)
	mux.HandleFunc("POST /console/keys", c.handleCreateKey)
	mux.HandleFunc("POST /console/keys/{id}/revoke", c.handleRevokeKey)
	mux.HandleFunc("GET /console/usage", c.handleUsage)
	mux.Handle("GET /console/usage/api", c.usageAPIHandler())
	mux.HandleFunc("GET /console/billing", c.handleBilling)
	mux.HandleFunc("POST /console/billing/spend-cap", c.handleSetSpendCap)
	mux.HandleFunc("GET /console/billing/portal", c.handleBillingPortal)
	mux.HandleFunc("GET /console/billing/topup", c.handleBillingTopUp)
	mux.HandleFunc("GET /console/sandboxes", c.handleListSandboxes)
	mux.HandleFunc("POST /console/sandboxes", c.handleCreateSandbox)
	mux.HandleFunc("GET /console/sandboxes/{id}", c.handleInspectSandbox)
	mux.HandleFunc("DELETE /console/sandboxes/{id}", c.handleTerminateSandbox)
	mux.HandleFunc("POST /console/sandboxes/{id}/fork", c.handleForkSandbox)
	mux.HandleFunc("POST /console/sandboxes/{id}/exec", c.handleExecSandbox)
	mux.HandleFunc("GET /console/sandboxes/{id}/logs", c.handleSandboxLogs)
	mux.HandleFunc("GET /console/sandboxes/{id}/logs/stream", c.handleSandboxLogsStream)
	mux.HandleFunc("PUT /console/sandboxes/{id}/project", c.handleSetSandboxProject)
	mux.HandleFunc("GET /console/members", c.handleListMembers)
	mux.HandleFunc("DELETE /console/members/{accountID}", c.handleRemoveMember)
	mux.HandleFunc("GET /console/invites", c.handleListInvites)
	mux.HandleFunc("POST /console/invites", c.handleCreateInvite)
	mux.HandleFunc("DELETE /console/invites/{id}", c.handleRevokeInvite)
	mux.HandleFunc("POST /console/invites/{id}/resend", c.handleResendInvite)
	// NOTE: GET /console/invites/lookup is deliberately NOT registered here.
	// It is PUBLIC (pre-auth) and is mounted by the binary directly on the
	// top-level mux, outside this Console's session-middleware wrapping; see
	// Console.LookupInvite and cmd/console/main.go.
	mux.HandleFunc("POST /console/invites/accept", c.handleAcceptInvite)
	mux.HandleFunc("GET /console/audit", c.handleAudit)
	mux.HandleFunc("GET /console/audit/export", c.handleAuditExport)
	mux.HandleFunc("GET /console/audit/retention", c.handleGetRetention)
	mux.HandleFunc("PUT /console/audit/retention", c.handleSetRetention)
	mux.HandleFunc("GET /console/audit/sinks", c.handleListSinks)
	mux.HandleFunc("POST /console/audit/sinks", c.handleCreateSink)
	mux.HandleFunc("DELETE /console/audit/sinks/{id}", c.handleDeleteSink)
	mux.HandleFunc("GET /console/templates", c.handleListTemplates)
	mux.HandleFunc("GET /console/secrets", c.handleListSecrets)
	mux.HandleFunc("POST /console/secrets", c.handleCreateSecret)
	mux.HandleFunc("DELETE /console/secrets/{name}", c.handleDeleteSecret)
	mux.HandleFunc("GET /console/instruments", c.handleInstruments)
	mux.HandleFunc("GET /console/first-activity", c.handleFirstActivity)
	mux.HandleFunc("GET /console/forktree", c.handleForkTree)
	mux.HandleFunc("GET /console/projects", c.handleListProjects)
	mux.HandleFunc("POST /console/projects", c.handleCreateProject)
	mux.HandleFunc("GET /console/projects/{id}/members", c.handleListProjectMembers)
	mux.HandleFunc("POST /console/projects/{id}/members", c.handleAssignProjectMember)
	mux.HandleFunc("DELETE /console/projects/{id}/members/{accountID}", c.handleRevokeProjectMember)
	mux.HandleFunc("POST /console/members/{accountID}/role", c.handleSetMemberRole)
	mux.HandleFunc("GET /console/account", c.handleGetAccount)
	mux.HandleFunc("PATCH /console/account", c.handlePatchAccount)
	mux.HandleFunc("GET /console/account/sessions", c.handleListSessions)
	mux.HandleFunc("DELETE /console/account/sessions/{id}", c.handleRevokeSession)
	mux.HandleFunc("DELETE /console/account/sessions", c.handleRevokeAllSessions)
	mux.HandleFunc("GET /console/retention", c.handleGetDataRetention)
	mux.HandleFunc("PUT /console/retention", c.handleSetDataRetention)
	mux.HandleFunc("GET /console/roles", c.handleListRoles)
	mux.HandleFunc("POST /console/roles", c.handleUpsertRole)
	mux.HandleFunc("DELETE /console/roles/{name}", c.handleDeleteRole)
	c.mux = mux
}

// ServeHTTP dispatches to the registered endpoints.
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) { c.mux.ServeHTTP(w, r) }

// caller resolves the authenticated account id and org id from the request
// context. A request missing either is unauthenticated and is refused; the org
// is NEVER read from the query, path, or body, which is the cross-org isolation
// guarantee.
func (c *Console) caller(r *http.Request) (accountID, orgID string, err apierr.Error, ok bool) {
	orgID, hasOrg := OrgFromContext(r.Context())
	accountID, hasCaller := CallerFromContext(r.Context())
	if !hasOrg || !hasCaller {
		return "", "", sessionUnauthorized().
			WithCause("no authenticated org context is attached to the request"), false
	}
	// noErr is the zero value returned on the success path; callers ignore the
	// error whenever ok is true, so it is never surfaced. It is a declaration,
	// not an apierr.Error literal, because an all-zero error carries no code or
	// remediation and is not a real error (the #28 remediation lint targets
	// error literals that purport to be errors, not this no-error sentinel).
	var noErr apierr.Error
	return accountID, orgID, noErr, true
}

// --- Keys (over #210 key service via the membership-guarded AccountService) ---

// KeyView is the masked, safe-to-display shape of an API key. It NEVER carries a
// raw key value or the stored hash; only the masked prefix and metadata.
type KeyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
	Revoked   bool      `json:"revoked"`
}

func keyView(k saas.ApiKey) KeyView {
	return KeyView{
		ID:        k.ID,
		Name:      k.Name,
		Prefix:    k.Prefix,
		Scopes:    k.Scopes,
		CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt,
		RevokedAt: k.RevokedAt,
		Revoked:   k.IsRevoked(),
	}
}

func (c *Console) handleListKeys(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	keys, err := c.deps.Accounts.ListKeys(r.Context(), accountID, orgID)
	if err != nil {
		c.failAccount(w, err, "the keys could not be listed for this organization")
		return
	}
	out := make([]KeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyView(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "keys": out})
}

// createKeyRequest is the console key-create body. TTLSeconds of zero means the
// key never expires.
type createKeyRequest struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	TTLSeconds int64    `json:"ttl_seconds"`
}

func (c *Console) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.authorize(r, saas.PermUseResources)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req createKeyRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the key-create body is not valid JSON"))
		return
	}
	created, err := c.deps.Accounts.CreateKey(r.Context(), accountID, saas.CreateKeyRequest{
		OrgID:  orgID,
		Name:   req.Name,
		Scopes: req.Scopes,
		TTL:    time.Duration(req.TTLSeconds) * time.Second,
	})
	if err != nil {
		c.failAccount(w, err, "the key could not be created for this organization")
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "key.create",
		Target:     created.Record.ID,
		TargetType: "key",
		TargetName: created.Record.Name,
		Detail:     "created api key " + created.Record.Prefix,
		At:         c.deps.Now(),
	})
	// The raw key is returned EXACTLY ONCE here and is never stored, logged, or
	// returned again. Every later read shows only the masked prefix.
	c.deps.Log.Info("console key created", "org", orgID, "key_id", created.Record.ID, "key_prefix", created.Record.Prefix)
	writeJSON(w, http.StatusCreated, map[string]any{
		"org_id":  orgID,
		"raw_key": created.RawKey,
		"key":     keyView(created.Record),
	})
}

func (c *Console) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.authorize(r, saas.PermUseResources)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	keyID := r.PathValue("id")
	// Resolve the key's own name before revoking it, best-effort, so the audit
	// event's TargetName is more legible than the bare id. A lookup failure (or
	// the key already being gone) just leaves TargetName empty.
	var keyName string
	if keys, err := c.deps.Accounts.ListKeys(r.Context(), accountID, orgID); err == nil {
		for _, k := range keys {
			if k.ID == keyID {
				keyName = k.Name
				break
			}
		}
	}
	if err := c.deps.Accounts.RevokeKey(r.Context(), accountID, keyID); err != nil {
		c.failAccount(w, err, "the key could not be revoked for this organization")
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "key.revoke",
		Target:     keyID,
		TargetType: "key",
		TargetName: keyName,
		Detail:     "revoked api key " + keyID,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "revoked": keyID})
}

// --- Usage / cost (over #211) ---

// usageAPIHandler mounts the #211 usage API (usage.NewUsageHandler) on the
// console so the SPA can query the org's records/totals/cost in the canonical
// usage-API shape, served from the SAME store and price list the console's own
// usage view reads. It is ORG-SCOPED via the gateway-verified identity: the org
// is taken from the console request context (attached by the session/gateway
// middleware, never from the client) and bridged into the usage handler's own
// org context, so a request authenticated as org A can never read org B. A
// request with no org context is refused before the usage handler runs.
func (c *Console) usageAPIHandler() http.Handler {
	inner := usage.NewUsageHandler(c.deps.Usage, c.deps.Prices)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, orgID, e, ok := c.caller(r)
		if !ok {
			apierr.Encode(w, e)
			return
		}
		// Re-derive the org ONLY from the verified console context and inject it
		// into the usage handler's context. The usage handler reads the org from
		// usage.OrgFromContext and ignores any org in the query, so the client can
		// never widen the scope.
		ctx := usage.WithOrg(r.Context(), orgID)
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (c *Console) handleUsage(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	from, err := parseTime(r.URL.Query().Get("from"))
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the from query parameter is not an RFC3339 timestamp"))
		return
	}
	to, err := parseTime(r.URL.Query().Get("to"))
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the to query parameter is not an RFC3339 timestamp"))
		return
	}
	records, err := c.deps.Usage.ListRecords(r.Context(), orgID, from, to)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the usage store could not list records"))
		return
	}
	totals := rollUp(records)
	writeJSON(w, http.StatusOK, usage.UsageResponse{
		OrgID:   orgID,
		Records: records,
		Totals:  totals,
		Cost:    c.deps.Prices.Cost(totals),
	})
}

// --- Billing (over #212) ---

// ledgerEntryView is the snake_case JSON view of a billing.LedgerEntry sent to
// the SPA. The underlying LedgerEntry struct has no json tags (PascalCase by
// default), so this view model maps the fields the SPA actually reads:
// ts (At), cents (Amount as int64), reason (Note). OrgID and Key are internal
// fields and must not appear on the wire.
type ledgerEntryView struct {
	Ts     time.Time `json:"ts"`
	Cents  int64     `json:"cents"`
	Reason string    `json:"reason"`
}

// BillingView is the console billing summary: the plan/dunning status, the
// current period spend, the credit balance, the dunning status, and the credit
// ledger entries (the closest thing to invoices in this slice; real Stripe
// invoices are a documented follow-up).
type BillingView struct {
	OrgID         string                `json:"org_id"`
	Status        billing.BillingStatus `json:"status"`
	BalanceCents  int64                 `json:"balance_cents"`
	SpendCents    int64                 `json:"spend_cents"`
	SoftCapCents  int64                 `json:"soft_cap_cents"`
	HardCapCents  int64                 `json:"hard_cap_cents"`
	LedgerEntries []ledgerEntryView     `json:"ledger_entries"`
	// TopUpAvailable is true when the prepaid credit top-up provider is
	// configured (a non-empty TopUpProductID). When false the SPA shows a calm
	// "adding credits is not available yet" state instead of offering a checkout
	// that would fail. The signup credit and balance are unaffected either way.
	TopUpAvailable bool `json:"topup_available"`
}

func (c *Console) handleBilling(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	view := BillingView{OrgID: orgID, Status: billing.StatusActive}
	// A top-up is available exactly when the product ID is configured, matching
	// the topup.go guard that returns 400 on an empty product ID.
	view.TopUpAvailable = c.deps.TopUpProductID != ""
	if c.deps.Billing.Status != nil {
		st, err := c.deps.Billing.Status.Status(r.Context(), orgID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the billing status could not be read"))
			return
		}
		view.Status = st
	}
	if c.deps.Billing.Ledger != nil {
		bal, err := c.deps.Billing.Ledger.Balance(r.Context(), orgID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the credit balance could not be read"))
			return
		}
		entries, err := c.deps.Billing.Ledger.Entries(r.Context(), orgID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the credit ledger could not be read"))
			return
		}
		view.BalanceCents = int64(bal)
		views := make([]ledgerEntryView, len(entries))
		for i, e := range entries {
			views[i] = ledgerEntryView{Ts: e.At, Cents: int64(e.Amount), Reason: e.Note}
		}
		view.LedgerEntries = views
	}
	if view.LedgerEntries == nil {
		view.LedgerEntries = []ledgerEntryView{}
	}
	if c.deps.Billing.Caps != nil {
		cap, has, err := c.deps.Billing.Caps.Get(r.Context(), orgID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the spend cap could not be read"))
			return
		}
		if has {
			view.SoftCapCents = int64(cap.SoftCap)
			view.HardCapCents = int64(cap.HardCap)
		}
	}
	// Current spend is the cost of the org's usage records, priced with the same
	// billing rate table the spend cap and the credit drawdown use, so the console
	// spend matches the bill.
	if c.deps.Usage != nil {
		records, err := c.deps.Usage.ListRecords(r.Context(), orgID, time.Time{}, time.Time{})
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the usage store could not list records"))
			return
		}
		var spend billing.Money
		for _, rec := range records {
			spend += c.deps.Billing.Rates.CostCents(rec)
		}
		view.SpendCents = int64(spend)
	}
	writeJSON(w, http.StatusOK, view)
}

// --- Live sandboxes (over the SandboxControl seam) ---

func (c *Console) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	boxes, err := c.deps.Sandboxes.List(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the live sandboxes could not be listed"))
		return
	}
	if boxes == nil {
		boxes = []SandboxView{}
	}
	// Resolve each box's project tag. The tag gates access (an assigned box is
	// restricted), so a lookup error must fail the request, never silently fall
	// back to the unassigned/org-wide path (that would re-grant access).
	for i := range boxes {
		pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", boxes[i].ID)
		if err != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
			return
		}
		boxes[i].ProjectID = pid
	}
	// Filter to sandboxes the caller can read. On a real access-check error we
	// fail the whole request (500) rather than silently drop boxes.
	visible := boxes[:0]
	for _, box := range boxes {
		canSee, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, box.ProjectID, saas.PermReadOnly)
		if accessErr != nil {
			apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
			return
		}
		if canSee {
			visible = append(visible, box)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "sandboxes": visible})
}

func (c *Console) handleInspectSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, r.PathValue("id"))
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	// The project tag gates access; a lookup error must fail closed, not fall
	// back to the unassigned/org-wide path.
	pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", sb.ID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
		return
	}
	sb.ProjectID = pid
	// Check per-project access. Return 404 (not 403) to avoid leaking existence.
	canSee, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, sb.ProjectID, saas.PermReadOnly)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
		return
	}
	if !canSee {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the sandbox does not exist or is not accessible"))
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (c *Console) handleTerminateSandbox(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	// Fetch the sandbox first to resolve its project assignment (needed for the
	// per-project access check). ErrNotFound is treated as not-found (404).
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, id)
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	// The project tag gates access; a lookup error must fail closed, not fall
	// back to the unassigned/org-wide path.
	pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", sb.ID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
		return
	}
	sb.ProjectID = pid
	// Check that the caller may use resources on this sandbox. Return 403 so the
	// caller knows the sandbox exists but they lack permission (the spec says 403
	// for terminate, not 404).
	canAct, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, sb.ProjectID, saas.PermUseResources)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
		return
	}
	if !canAct {
		apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller's role does not grant access to this sandbox"))
		return
	}
	if err := c.deps.Sandboxes.Terminate(r.Context(), orgID, id); err != nil {
		c.failSandbox(w, err)
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "sandbox.terminate",
		Target:     id,
		TargetType: "sandbox",
		Detail:     "terminated sandbox " + id,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "terminated": id})
}

// --- Org / members / audit (over #210 plus the audit seam) ---

// MemberView is the console shape of one org membership. Email and
// DisplayName are joined from the member's account (best-effort, the same
// lookup the audit actor/target names use); a lookup failure leaves both
// empty rather than failing the whole list, so one bad row never breaks the
// members view.
type MemberView struct {
	AccountID   string    `json:"account_id"`
	OrgID       string    `json:"org_id"`
	Role        saas.Role `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
}

func (c *Console) handleListMembers(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	members, err := c.deps.Accounts.ListMembers(r.Context(), accountID, orgID)
	if err != nil {
		c.failAccount(w, err, "the members could not be listed for this organization")
		return
	}
	out := make([]MemberView, 0, len(members))
	for _, m := range members {
		mv := MemberView{AccountID: m.AccountID, OrgID: m.OrgID, Role: m.Role, CreatedAt: m.CreatedAt}
		if acct, err := c.deps.Accounts.GetAccount(r.Context(), m.AccountID); err == nil {
			mv.Email = acct.Email
			mv.DisplayName = acct.DisplayName
		}
		out = append(out, mv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "members": out})
}

func (c *Console) handleAudit(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	// The audit log is org-scoped; reading it requires org membership, enforced
	// through the account service exactly as the members view is.
	if _, err := c.deps.Accounts.ListMembers(r.Context(), accountID, orgID); err != nil {
		c.failAccount(w, err, "the audit log could not be read for this organization")
		return
	}
	events, err := c.deps.Audit.List(r.Context(), orgID, DefaultAuditListLimit)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the audit log could not be read"))
		return
	}
	if events == nil {
		events = []AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "events": events})
}

// --- Templates (over the TemplateLister read seam, ties to #220) ---

func (c *Console) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	tmpls, err := c.deps.Templates.List(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the templates could not be listed"))
		return
	}
	if tmpls == nil {
		tmpls = []TemplateView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "templates": tmpls})
}

// --- Fork tree (over the ForkTreeSource seam, ties to #33 CoW metering) ---

func (c *Console) handleForkTree(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	tree, err := c.deps.ForkTree.Tree(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the fork tree could not be read"))
		return
	}
	if tree.Nodes == nil {
		tree.Nodes = []ForkNode{}
	}
	writeJSON(w, http.StatusOK, tree)
}

// --- Projects (org-scoped project containers) ---

func (c *Console) handleListProjects(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	projects, err := c.deps.Projects.List(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the projects could not be listed"))
		return
	}
	if projects == nil {
		projects = []Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "projects": projects})
}

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (c *Console) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageProjects)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req createProjectRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the project-create body is not valid JSON"))
		return
	}
	p, err := c.deps.Projects.Create(r.Context(), orgID, req.Name, req.Description)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the project could not be created"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "project.create",
		Target:     p.ID,
		TargetType: "project",
		TargetName: p.Name,
		Detail:     "created project " + p.Name,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusCreated, p)
}

// --- Member role management ---

type setMemberRoleRequest struct {
	Role saas.Role `json:"role"`
}

func (c *Console) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	targetID := r.PathValue("accountID")
	var req setMemberRoleRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the role body is not valid JSON"))
		return
	}
	if err := c.deps.Accounts.SetMemberRole(r.Context(), accountID, orgID, targetID, req.Role); err != nil {
		switch {
		case errors.Is(err, saas.ErrForbidden):
			apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
				WithCause("the caller does not have permission to change member roles"))
		case errors.Is(err, saas.ErrNotFound):
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the target account is not a member of this organization"))
		default:
			c.failAccount(w, err, "the member role could not be changed")
		}
		return
	}
	// Resolve the TARGET account's own name (not the actor's) best-effort, so
	// the audit sentence can read "changed Carol's role" instead of a bare
	// account id.
	var targetName string
	if acct, err := c.deps.Accounts.GetAccount(r.Context(), targetID); err == nil {
		targetName = displayNameOrEmail(acct)
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "member.role",
		Target:     targetID,
		TargetType: "member",
		TargetName: targetName,
		Detail:     "set role " + string(req.Role),
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "account_id": targetID, "role": req.Role})
}

// --- Audit sinks (GET/POST/DELETE /console/audit/sinks) ---

// handleListSinks returns the org's configured audit-sink destinations.
func (c *Console) handleListSinks(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	cfgs := c.deps.Sinks.List(r.Context(), orgID)
	if cfgs == nil {
		cfgs = []SinkConfig{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "sinks": cfgs})
}

// createSinkRequest is the body of POST /console/audit/sinks.
type createSinkRequest struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
}

// handleCreateSink adds a new audit-sink destination for the caller's org and
// audits the action.
func (c *Console) handleCreateSink(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageSettings)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	var req createSinkRequest
	if err := decodeBody(r, &req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the sink-create body is not valid JSON"))
		return
	}
	if err := validateSinkRequest(req); err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).WithCause(err.Error()))
		return
	}
	cfg, err := c.deps.Sinks.Add(r.Context(), orgID, req.Type, req.Endpoint)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the audit sink could not be created"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "audit.sink.create",
		Target:     cfg.ID,
		TargetType: "sink",
		// TargetName is the sink TYPE, never its Endpoint: the endpoint URL may
		// carry an opaque token in its path or query (see webhookSink's own
		// no-log rule), so it must never land in the audit trail.
		TargetName: cfg.Type,
		Detail:     "created audit sink type " + cfg.Type,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusCreated, cfg)
}

// handleDeleteSink removes an audit-sink destination from the caller's org.
// A sink belonging to a different org returns not_found.
func (c *Console) handleDeleteSink(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.authorize(r, saas.PermManageSettings)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	// Resolve the sink's type before deleting it, best-effort, for TargetName
	// (never the Endpoint; see the create handler's comment on why).
	var targetName string
	for _, cfg := range c.deps.Sinks.List(r.Context(), orgID) {
		if cfg.ID == id {
			targetName = cfg.Type
			break
		}
	}
	if err := c.deps.Sinks.Delete(r.Context(), orgID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
				WithCause("the audit sink does not exist or does not belong to this organization"))
			return
		}
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the audit sink could not be deleted"))
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:      orgID,
		ActorID:    accountID,
		Action:     "audit.sink.delete",
		Target:     id,
		TargetType: "sink",
		TargetName: targetName,
		Detail:     "deleted audit sink " + id,
		At:         c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "deleted": id})
}

// allowedSinkTypes is the set of sink type values accepted by handleCreateSink.
// Any type not in this set is rejected with a 400 before the registry is called.
var allowedSinkTypes = map[string]bool{
	"webhook": true,
	"s3":      true,
	"splunk":  true,
	"datadog": true,
}

// validateSinkRequest checks that the sink type is in the allowed set and that
// the endpoint is a non-empty https URL. It returns an actionable, non-secret
// error message suitable for use as an apierr cause.
func validateSinkRequest(req createSinkRequest) error {
	if !allowedSinkTypes[req.Type] {
		return errors.New("unknown sink type: must be one of webhook, s3, splunk, datadog")
	}
	if req.Endpoint == "" {
		return errors.New("the sink endpoint must be an https URL")
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("the sink endpoint must be an https URL")
	}
	return nil
}

// --- helpers ---

// audit records an event best-effort; an audit failure never fails the user
// action but is logged. It fills in ActorType (defaulting to "user") and, when
// ActorName is unset, resolves it from the actor's account (display name,
// falling back to email) via a single best-effort lookup. A lookup failure
// (account deleted, store hiccup) leaves ActorName empty rather than failing
// the action: the audit event is always recorded.
func (c *Console) audit(ctx context.Context, ev AuditEvent) {
	if c.deps.Audit == nil {
		return
	}
	if ev.ActorType == "" {
		ev.ActorType = "user"
	}
	if ev.ActorName == "" && ev.ActorID != "" && c.deps.Accounts != nil {
		if acct, err := c.deps.Accounts.GetAccount(ctx, ev.ActorID); err == nil {
			ev.ActorName = displayNameOrEmail(acct)
		}
	}
	if err := c.deps.Audit.Record(ctx, ev); err != nil {
		c.deps.Log.Warn("console audit record failed", "org", ev.OrgID, "action", ev.Action, "err", err.Error())
	}
}

// displayNameOrEmail returns acct's DisplayName, falling back to its Email
// when no display name has been set (the common case right after sign-up).
func displayNameOrEmail(acct saas.Account) string {
	if acct.DisplayName != "" {
		return acct.DisplayName
	}
	return acct.Email
}

// failAccount maps an account-service error to the public envelope. A wrong-org /
// not-a-member error is forbidden (the caller is authenticated but not entitled
// to this org); a not-found is not_found; anything else is internal. The cause is
// non-secret.
func (c *Console) failAccount(w http.ResponseWriter, err error, _ string) {
	switch {
	case errors.Is(err, saas.ErrKeyWrongOrg):
		apierr.Encode(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the caller is not a member of this organization"))
	case errors.Is(err, saas.ErrNotFound):
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the requested record does not exist"))
	default:
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the request could not be served"))
	}
}

// failSandbox maps a sandbox-control error to the public envelope. A not-found
// (which also covers a cross-org sandbox id) is not_found; ErrUnsupported (the
// verb has no real backend on this deployment yet, a documented follow-up, not
// a fabricated success) is 501 so the SPA can show an honest state instead of a
// silent no-op.
func (c *Console) failSandbox(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the sandbox does not exist or is not in this organization"))
	case errors.Is(err, ErrUnsupported):
		// Built directly rather than via apierr.Catalogue: that catalogue is
		// normative for the forkd/sandbox-server runtime API (doc-synced by
		// TestDocCatalogueIsInSyncWithCode), a different surface than this
		// console BFF's own error shape. Reusing apierr.Error/Encode here just
		// keeps the wire envelope consistent with every other console error.
		apierr.Encode(w, apierr.Error{
			Code:        "not_implemented",
			Message:     "this operation is not available on this deployment yet",
			Cause:       err.Error(),
			Remediation: "This is a documented follow-up, not a misconfiguration; check docs/saas/console.md for status or ask the operator.",
			Status:      http.StatusNotImplemented,
		})
	default:
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox request could not be served"))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// rollUp sums usage records into per-unit totals, mirroring the #211 usage API
// roll-up so the console usage view matches the usage API exactly.
func rollUp(records []usage.UsageRecord) usage.Totals {
	var t usage.Totals
	for _, r := range records {
		t.VCPUSeconds += r.VCPUSeconds
		t.MemGiBSeconds += r.MemGiBSeconds
		t.StorageGiBHours += r.StorageGiBHours
		t.EgressBytes += r.EgressBytes
		t.GPUSeconds += r.GPUSeconds
	}
	return t
}
