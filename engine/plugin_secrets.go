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
	return s.store.GetSecret(s.ctx(ctx), s.tenantID.String(), name)
}

func (s *pluginTenantSecrets) Put(ctx context.Context, name, value string) error {
	if s.parseErr != nil {
		return fmt.Errorf("plugin.Secrets.ForTenant: %w", s.parseErr)
	}
	return s.store.PutSecret(s.ctx(ctx), s.tenantID.String(), name, value)
}

func (s *pluginTenantSecrets) Retire(ctx context.Context, name string) (int64, error) {
	if s.parseErr != nil {
		return 0, fmt.Errorf("plugin.Secrets.ForTenant: %w", s.parseErr)
	}
	return s.store.RetireSecret(s.ctx(ctx), s.tenantID.String(), name)
}

// pluginPayloads adapts *PayloadEncryption to plugin.Payloads, the same shape
// as pluginSecrets: PayloadEncryption.Encrypt/Decrypt take a tenantID string
// with no ctx involvement at all (there is no SQL, so no RLS to desync from),
// but the interface still reads it from ctx rather than a parameter, for
// consistency with Secrets and because the failure mode is not only RLS: a
// plugin under tenant B's request that mislabels a Seal as tenant A's would
// produce a value A's own decrypt can read but B's caller did not intend, and
// no parameter means no label to mislabel.
type pluginPayloads struct {
	enc *PayloadEncryption
}

// NewPluginPayloads wraps enc for use as a plugin.Environment.Payloads value.
// A nil enc is valid input, same convention as NewPluginSecrets.
func NewPluginPayloads(enc *PayloadEncryption) plugin.Payloads {
	return &pluginPayloads{enc: enc}
}

func (p *pluginPayloads) tenant(ctx context.Context) (uuid.UUID, error) {
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
	return p.enc.Encrypt(tid.String(), plaintext)
}

func (p *pluginPayloads) Open(ctx context.Context, sealed []byte) ([]byte, error) {
	tid, err := p.tenant(ctx)
	if err != nil {
		return nil, err
	}
	return p.enc.Decrypt(tid.String(), sealed)
}
