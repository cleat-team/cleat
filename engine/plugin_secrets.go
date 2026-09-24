package engine

import (
	"context"
	"fmt"

	"github.com/cleat-team/cleat/internal/tenantctx"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// errNoTenantInContext is returned by the request-path methods when ctx
// carries no tenant -- either because the caller forgot plugin.ForTenant, or
// because it is a background loop that was never going to have one and
// should have reached for ForTenant on the Secrets/Payloads value instead.
// Named so both adapters below report it identically, since the underlying
// mistake is the same one either way.
func errNoTenantInContext(method string) error {
	return fmt.Errorf("plugin.%s: no tenant in context -- a background loop with no request "+
		"to derive one from must use ForTenant(tenantID) instead of the request-path methods", method)
}

// errCrossTenantContext is returned by every Secrets/Payloads method --
// request-path and ForTenant's alike -- when ctx already carries
// AcrossAllTenants's marker.
//
// beginTenantTx (engine/plugindb_tenant.go) checks the cross-tenant bypass
// BEFORE it checks for a tenant, by design: an admin endpoint that rebuilds
// an index for every tenant runs on a request context, and resolving the
// other order would quietly scope the sweep to whoever called it. That
// ordering has a cost here that #2141 already paid for the SQL side's
// per-tenant loops (plugin.IsCrossTenant): a request-path call on a marked
// ctx would run unscoped instead of failing, and ForTenant(id) layered on
// top of a marked ctx is a silent no-op -- the mark still wins, id is
// dropped, and the call runs under the bypass instead of scoped to id.
//
// Measured by cleat-review on #2163, rather than assumed: PostgreSQL's
// markCrossTenantOnTx does `SET LOCAL ROLE cleat_sweep`, a role granted
// WITH INHERIT FALSE, so it drops every grant cleat_app has for the
// remainder of the transaction; tenant_secrets is granted to cleat_app, not
// to cleat_sweep, so every Secrets/Payloads call fails 42501
// (insufficient_privilege) once a transaction has switched to it. SQL
// Server's markCrossTenantOnTx sets only a cross_tenant session-context key
// and never touches the tenant_id one, so a ForTenant(id) call on an
// already-marked ctx runs under whatever tenant_id SESSION_CONTEXT the
// underlying pooled connection happens to carry (stale, or none): Get reads
// as "not found" because the security policy's filter predicate compares
// against that stale key rather than id; Put on a name that already exists
// under id primary-key-violates, because the UPDATE arm is filtered out by
// the same stale-key predicate and the INSERT arm then collides with the
// PK-constraint-visible row; Put on a new name silently succeeds but writes
// a row invisible to id's own later reads; Retire affects zero rows with a
// nil error, indistinguishable from "no such secret".
//
// Refusing outright -- rather than reproducing any of those per-dialect
// failure modes -- is the same choice #2141 made for the SQL per-tenant
// loops via plugin.IsCrossTenant.
func errCrossTenantContext(method string) error {
	return fmt.Errorf("plugin.%s: ctx is cross-tenant-marked (plugin.AcrossAllTenants) -- "+
		"refused rather than silently mis-scoped or bypassed; see beginTenantTx's own doc "+
		"comment in engine/plugindb_tenant.go for why the bypass wins over a tenant marking", method)
}

// checkNotCrossTenant is the one check every Secrets/Payloads method makes
// before touching the store, request-path and ForTenant alike. See
// errCrossTenantContext for why: this is not a defense against a malicious
// ctx, it is a defense against the same one-token slip #2141 guards on the
// SQL side (a caller reusing a ctx that AcrossAllTenants already marked
// instead of building an unmarked one).
func checkNotCrossTenant(ctx context.Context, method string) error {
	if _, ok := tenantctx.CrossTenant(ctx); ok {
		return errCrossTenantContext(method)
	}
	return nil
}

// pluginSecrets adapts *SecretStore to plugin.Secrets. See plugin.Secrets'
// own doc comment for why the tenant comes from ctx rather than a parameter:
// in short, GetSecret/PutSecret/RetireSecret already require ctx to carry the
// tenant for beginTenantTx to scope the session's RLS/security-policy
// correctly (engine/plugindb_tenant.go), and additionally take a tenantID
// STRING for the WHERE-clause predicate and the crypto derivation -- so a
// caller in a position to pass a mismatched pair already exists at the
// engine-internal layer. This adapter closes that off by reading the tenant
// from ctx exactly once and using that single value for both.
type pluginSecrets struct {
	store *SecretStore
}

// NewPluginSecrets wraps store for use as a plugin.Environment.Secrets value.
// A nil store is valid input and produces a value whose methods all return
// ErrNoSecretDB/ErrNoSecretMasterKey via the store's own nil-safety -- callers
// that want plugin.Environment.Secrets left nil when no store is configured
// should check that before calling this, matching the field's own doc
// comment.
func NewPluginSecrets(store *SecretStore) plugin.Secrets {
	return &pluginSecrets{store: store}
}

func (s *pluginSecrets) tenant(ctx context.Context) (uuid.UUID, error) {
	if err := checkNotCrossTenant(ctx, "Secrets"); err != nil {
		return uuid.Nil, err
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return uuid.Nil, errNoTenantInContext("Secrets")
	}
	return tid, nil
}

func (s *pluginSecrets) Get(ctx context.Context, name string) (string, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return "", err
	}
	return s.store.GetSecret(ctx, tid.String(), name)
}

func (s *pluginSecrets) Put(ctx context.Context, name, value string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	return s.store.PutSecret(ctx, tid.String(), name, value)
}

func (s *pluginSecrets) Retire(ctx context.Context, name string) (int64, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return 0, err
	}
	return s.store.RetireSecret(ctx, tid.String(), name)
}

func (s *pluginSecrets) ForTenant(tenantID string) plugin.TenantSecrets {
	tid, err := uuid.Parse(tenantID)
	return &pluginTenantSecrets{store: s.store, tenantID: tid, parseErr: err}
}

// pluginTenantSecrets is Secrets.ForTenant's return value. It marks ctx with
// its own tenant before every call -- the SAME mechanism plugin.ForTenant
// uses for a plugin's own SQL -- so a background sweep gets the identical
// RLS/security-policy scoping a request-path call gets, rather than silently
// falling through beginTenantTx's no-transaction path on an unmarked ctx.
type pluginTenantSecrets struct {
	store    *SecretStore
	tenantID uuid.UUID
	// parseErr is set once, at ForTenant, rather than checked per call: an
	// invalid tenantID is a caller bug fixed by reading the return value of
	// ForTenant's parse, not something that should behave differently on the
	// first call versus the tenth.
	parseErr error
}

func (s *pluginTenantSecrets) ctx(ctx context.Context) context.Context {
	return tenantctx.With(ctx, s.tenantID)
}

func (s *pluginTenantSecrets) Get(ctx context.Context, name string) (string, error) {
	if s.parseErr != nil {
		return "", fmt.Errorf("plugin.Secrets.ForTenant: %w", s.parseErr)
	}
	if err := checkNotCrossTenant(ctx, "Secrets.ForTenant"); err != nil {
		return "", err
	}
	return s.store.GetSecret(s.ctx(ctx), s.tenantID.String(), name)
}

func (s *pluginTenantSecrets) Put(ctx context.Context, name, value string) error {
	if s.parseErr != nil {
		return fmt.Errorf("plugin.Secrets.ForTenant: %w", s.parseErr)
	}
	if err := checkNotCrossTenant(ctx, "Secrets.ForTenant"); err != nil {
		return err
	}
	return s.store.PutSecret(s.ctx(ctx), s.tenantID.String(), name, value)
}

func (s *pluginTenantSecrets) Retire(ctx context.Context, name string) (int64, error) {
	if s.parseErr != nil {
		return 0, fmt.Errorf("plugin.Secrets.ForTenant: %w", s.parseErr)
	}
	if err := checkNotCrossTenant(ctx, "Secrets.ForTenant"); err != nil {
		return 0, err
	}
	return s.store.RetireSecret(s.ctx(ctx), s.tenantID.String(), name)
}

// pluginPayloads adapts *PayloadEncryption to plugin.Payloads, the same shape
// as pluginSecrets: PayloadEncryption.SealForPlugin/OpenForPlugin take a
// tenantID string with no ctx involvement at all (there is no SQL, so no RLS
// to desync from), but the interface still reads it from ctx rather than a
// parameter, for consistency with Secrets and because the failure mode is not
// only RLS: a plugin under tenant B's request that mislabels a Seal as tenant
// A's would produce a value A's own decrypt can read but B's caller did not
// intend, and no parameter means no label to mislabel.
//
// SealForPlugin/OpenForPlugin, NOT Encrypt/Decrypt. Measured by cleat-review
// on #2163: Encrypt/Decrypt derive under payloadKeyInfo, the same HKDF info
// string encodeEventForStorage uses for a workflow's own event_history
// fields -- same master key, same AAD (tenantID), so a plugin's Open could
// decrypt engine's own event_history ciphertext outright. SealForPlugin/
// OpenForPlugin derive under pluginPayloadKeyInfo instead, and OpenForPlugin
// also does not fall back to the two legacy forms Decrypt does -- see both
// constants' and methods' own doc comments in engine/encryption.go.
type pluginPayloads struct {
	enc *PayloadEncryption
}

// NewPluginPayloads wraps enc for use as a plugin.Environment.Payloads value.
// A nil enc is valid input, same convention as NewPluginSecrets.
func NewPluginPayloads(enc *PayloadEncryption) plugin.Payloads {
	return &pluginPayloads{enc: enc}
}

func (p *pluginPayloads) tenant(ctx context.Context) (uuid.UUID, error) {
	if err := checkNotCrossTenant(ctx, "Payloads"); err != nil {
		return uuid.Nil, err
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return uuid.Nil, errNoTenantInContext("Payloads")
	}
	return tid, nil
}

func (p *pluginPayloads) Seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	tid, err := p.tenant(ctx)
	if err != nil {
		return nil, err
	}
	return p.enc.SealForPlugin(tid.String(), plaintext)
}

func (p *pluginPayloads) Open(ctx context.Context, sealed []byte) ([]byte, error) {
	tid, err := p.tenant(ctx)
	if err != nil {
		return nil, err
	}
	return p.enc.OpenForPlugin(tid.String(), sealed)
}
