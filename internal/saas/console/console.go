package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	Accounts    *saas.AccountService
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
	deps Deps
	mux  *http.ServeMux
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
	c := &Console{deps: deps}
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
	mux.HandleFunc("GET /console/billing", c.handleBilling)
	mux.HandleFunc("GET /console/billing/portal", c.handleBillingPortal)
	mux.HandleFunc("GET /console/sandboxes", c.handleListSandboxes)
	mux.HandleFunc("GET /console/sandboxes/{id}", c.handleInspectSandbox)
	mux.HandleFunc("DELETE /console/sandboxes/{id}", c.handleTerminateSandbox)
	mux.HandleFunc("GET /console/sandboxes/{id}/logs", c.handleSandboxLogs)
	mux.HandleFunc("GET /console/members", c.handleListMembers)
	mux.HandleFunc("GET /console/audit", c.handleAudit)
	mux.HandleFunc("GET /console/templates", c.handleListTemplates)
	mux.HandleFunc("GET /console/secrets", c.handleListSecrets)
	mux.HandleFunc("POST /console/secrets", c.handleCreateSecret)
	mux.HandleFunc("DELETE /console/secrets/{name}", c.handleDeleteSecret)
	mux.HandleFunc("GET /console/instruments", c.handleInstruments)
	mux.HandleFunc("GET /console/forktree", c.handleForkTree)
	mux.HandleFunc("GET /console/projects", c.handleListProjects)
	mux.HandleFunc("POST /console/projects", c.handleCreateProject)
	mux.HandleFunc("POST /console/members/{accountID}/role", c.handleSetMemberRole)
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
		return "", "", apierr.Get(apierr.CodeUnauthorized).
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
	accountID, orgID, e, ok := c.caller(r)
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
		OrgID:   orgID,
		ActorID: accountID,
		Action:  "key.create",
		Target:  created.Record.ID,
		Detail:  "created api key " + created.Record.Prefix,
		At:      c.deps.Now(),
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
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	keyID := r.PathValue("id")
	if err := c.deps.Accounts.RevokeKey(r.Context(), accountID, keyID); err != nil {
		c.failAccount(w, err, "the key could not be revoked for this organization")
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:   orgID,
		ActorID: accountID,
		Action:  "key.revoke",
		Target:  keyID,
		Detail:  "revoked api key " + keyID,
		At:      c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "revoked": keyID})
}

// --- Usage / cost (over #211) ---

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
	LedgerEntries []billing.LedgerEntry `json:"ledger_entries"`
}

func (c *Console) handleBilling(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	view := BillingView{OrgID: orgID, Status: billing.StatusActive}
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
		view.LedgerEntries = entries
	}
	if view.LedgerEntries == nil {
		view.LedgerEntries = []billing.LedgerEntry{}
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
	_, orgID, e, ok := c.caller(r)
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
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "sandboxes": boxes})
}

func (c *Console) handleInspectSandbox(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, r.PathValue("id"))
	if err != nil {
		c.failSandbox(w, err)
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
	if err := c.deps.Sandboxes.Terminate(r.Context(), orgID, id); err != nil {
		c.failSandbox(w, err)
		return
	}
	c.audit(r.Context(), AuditEvent{
		OrgID:   orgID,
		ActorID: accountID,
		Action:  "sandbox.terminate",
		Target:  id,
		Detail:  "terminated sandbox " + id,
		At:      c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "terminated": id})
}

// --- Org / members / audit (over #210 plus the audit seam) ---

// MemberView is the console shape of one org membership.
type MemberView struct {
	AccountID string    `json:"account_id"`
	OrgID     string    `json:"org_id"`
	Role      saas.Role `json:"role"`
	CreatedAt time.Time `json:"created_at"`
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
		out = append(out, MemberView{AccountID: m.AccountID, OrgID: m.OrgID, Role: m.Role, CreatedAt: m.CreatedAt})
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
	events, err := c.deps.Audit.List(r.Context(), orgID)
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
	accountID, orgID, e, ok := c.caller(r)
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
		OrgID:   orgID,
		ActorID: accountID,
		Action:  "project.create",
		Target:  p.ID,
		Detail:  "created project " + p.Name,
		At:      c.deps.Now(),
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
	c.audit(r.Context(), AuditEvent{
		OrgID:   orgID,
		ActorID: accountID,
		Action:  "member.role",
		Target:  targetID,
		Detail:  "set role " + string(req.Role),
		At:      c.deps.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID, "account_id": targetID, "role": req.Role})
}

// --- helpers ---

// audit records an event best-effort; an audit failure never fails the user
// action but is logged.
func (c *Console) audit(ctx context.Context, ev AuditEvent) {
	if c.deps.Audit == nil {
		return
	}
	if err := c.deps.Audit.Record(ctx, ev); err != nil {
		c.deps.Log.Warn("console audit record failed", "org", ev.OrgID, "action", ev.Action, "err", err.Error())
	}
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
// (which also covers a cross-org sandbox id) is not_found.
func (c *Console) failSandbox(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the sandbox does not exist or is not in this organization"))
		return
	}
	apierr.Encode(w, apierr.Get(apierr.CodeInternal).
		WithCause("the sandbox request could not be served"))
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
