package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Tenant provisioning, the store half. cleat#3552, B2b-2a. The HTTP routes that call it are B2b-2b.
//
// The shape of a provisioning handshake, and why it is not "create a tenant and return a key":
//
//  1. The creator asks for a tenant. The tenant is created and a PROVISIONAL key is minted with
//     it: it authenticates immediately, and it expires in ProvisionalKeyTTL.
//  2. The creator stores the key durably, then CONFIRMS. Confirming makes the key permanent.
//  3. If the response is lost, or the creator crashes before storing, the key lapses by itself.
//     Replaying the same create finds a tenant with no confirmed key, revokes what is left of
//     the provisional ones and issues a fresh one. Once a key is confirmed a replay returns the
//     tenant and no secret, so a replay can never be used to obtain a second working key.
//
// The creator, not the key, is what authorises a replay or a confirm: the principal that created
// the tenant (the operator key's principal, or "operator:<KeyID>" for a key with none). A tenant
// with no recorded creator (made offline, or before the column existed) can be replayed by no
// one, so this route can never mint a key for a tenant it did not create. That is the guard that
// stops "may create tenants" from quietly becoming "may act as any tenant".

// ProvisionalKeyTTL is how long a key minted by ProvisionTenant authenticates unconfirmed.
const ProvisionalKeyTTL = 15 * time.Minute

// ProvisionRequest is what a creator asks for. Exactly one of OrgID and OrgName must be set.
type ProvisionRequest struct {
	// TenantID is the caller-chosen id, the idempotency key of the create. uuid.Nil generates
	// one in Go. The nil UUID itself is the default tenant and cannot be created.
	TenantID uuid.UUID
	// Name is unique across the whole deployment, not per org. Empty means the canonical
	// string of the id, which cannot collide with another tenant's.
	Name        string
	DisplayName string
	OrgID       uuid.UUID
	// OrgName finds the org of that name or creates it.
	OrgName        string
	KeyDescription string
	// Creator is the principal making the request. Required.
	Creator string
}

type ProvisionOutcome string

const (
	// ProvisionCreated: the tenant did not exist and was created, with a provisional key.
	ProvisionCreated ProvisionOutcome = "created"
	// ProvisionReplayed: the tenant already existed with these parameters, made by this creator.
	// Key is set only when it had no confirmed key, and is then a fresh provisional one.
	ProvisionReplayed ProvisionOutcome = "replayed"
)

type ProvisionedTenant struct {
	ID          uuid.UUID
	Name        string
	DisplayName string
	OrgID       uuid.UUID
}

// ProvisionedKey is the one time a key's secret exists outside the database.
type ProvisionedKey struct {
	KeyID  uuid.UUID
	Secret string
	// ValidFor is how long it works unless confirmed.
	ValidFor time.Duration
}

type ProvisionResult struct {
	Outcome ProvisionOutcome
	Tenant  ProvisionedTenant
	Key     *ProvisionedKey
}

// Conflict reasons, stable strings an API can return.
const (
	ConflictNameTaken     = "name_taken"
	ConflictTenantDiffers = "tenant_differs"
	ConflictTenantExists  = "tenant_exists"
)

// ProvisionConflictError is a request that cannot succeed as stated and will not succeed on a retry.
type ProvisionConflictError struct {
	Reason string
	// Fields names which fields of an existing tenant differ from the request. Set only for
	// ConflictTenantDiffers.
	Fields []string
}

func (e *ProvisionConflictError) Error() string {
	switch e.Reason {
	case ConflictNameTaken:
		return "auth: the tenant name is already taken by another tenant"
	case ConflictTenantDiffers:
		return "auth: a tenant with this id exists and differs in: " + strings.Join(e.Fields, ", ")
	default:
		return "auth: a tenant with this id exists and was not created by this principal"
	}
}

var (
	ErrOrgNotFound = errors.New("auth: org not found")
	// ErrNoProvisionalKey: nothing to confirm, because the provisional key lapsed or was replaced
	// and no confirmed key exists. The creator replays the create to get a fresh one.
	ErrNoProvisionalKey = errors.New("auth: no live provisional key to confirm; replay the create")
)

// provisionStmts are the statements one dialect uses. A struct of strings so the shapes can be
// asserted without a database.
type provisionStmts struct {
	tenantByID, tenantByName, orgByName, orgExists              string
	insertOrg, insertTenant, insertKey                          string
	lockTenant, liveKeys, revokeProvisional, confirmProvisional string
}

func provisionStatements(dialect string) (provisionStmts, error) {
	switch dialect {
	case DialectPostgres:
		return provisionStmts{
			tenantByID:   `SELECT name, display_name, org_id::text, COALESCE(created_by, '') FROM admin.tenants WHERE tenant_id = $1`,
			tenantByName: `SELECT tenant_id::text FROM admin.tenants WHERE name = $1`,
			orgByName:    `SELECT org_id::text FROM admin.orgs WHERE name = $1`,
			orgExists:    `SELECT 1 FROM admin.orgs WHERE org_id = $1`,
			insertOrg:    `INSERT INTO admin.orgs (org_id, name) VALUES ($1, $2)`,
			insertTenant: `INSERT INTO admin.tenants (tenant_id, name, display_name, org_id, created_by) VALUES ($1, $2, $3, $4, $5)`,
			// The expiry is the DATABASE's clock plus a number of seconds, so the key lapses by the
			// same clock that judges it at authentication.
			insertKey:          `INSERT INTO admin.tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at, provisional) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5), true)`,
			lockTenant:         `SELECT COALESCE(created_by, '') FROM admin.tenants WHERE tenant_id = $1 FOR UPDATE`,
			liveKeys:           `SELECT COALESCE(SUM(CASE WHEN provisional THEN 0 ELSE 1 END), 0), COALESCE(SUM(CASE WHEN provisional THEN 1 ELSE 0 END), 0) FROM admin.tenant_api_keys WHERE tenant_id = $1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
			revokeProvisional:  `UPDATE admin.tenant_api_keys SET disabled_at = now(), updated_at = now() WHERE tenant_id = $1 AND provisional AND disabled_at IS NULL`,
			confirmProvisional: `UPDATE admin.tenant_api_keys SET provisional = false, expires_at = NULL, updated_at = now() WHERE tenant_id = $1 AND provisional AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
		}, nil
	case DialectMSSQL:
		return provisionStmts{
			tenantByID:         `SELECT name, display_name, LOWER(CONVERT(NVARCHAR(36), org_id)), ISNULL(created_by, N'') FROM admin.tenants WHERE tenant_id = @p1`,
			tenantByName:       `SELECT LOWER(CONVERT(NVARCHAR(36), tenant_id)) FROM admin.tenants WHERE name = @p1`,
			orgByName:          `SELECT LOWER(CONVERT(NVARCHAR(36), org_id)) FROM admin.orgs WHERE name = @p1`,
			orgExists:          `SELECT 1 FROM admin.orgs WHERE org_id = @p1`,
			insertOrg:          `INSERT INTO admin.orgs (org_id, name) VALUES (@p1, @p2)`,
			insertTenant:       `INSERT INTO admin.tenants (tenant_id, name, display_name, org_id, created_by) VALUES (@p1, @p2, @p3, @p4, @p5)`,
			insertKey:          `INSERT INTO admin.tenant_api_keys (key_id, tenant_id, key_hash, description, expires_at, provisional) VALUES (@p1, @p2, @p3, @p4, DATEADD(second, @p5, SYSUTCDATETIME()), 1)`,
			lockTenant:         `SELECT ISNULL(created_by, N'') FROM admin.tenants WITH (UPDLOCK, ROWLOCK) WHERE tenant_id = @p1`,
			liveKeys:           `SELECT COALESCE(SUM(CASE WHEN provisional = 1 THEN 0 ELSE 1 END), 0), COALESCE(SUM(CASE WHEN provisional = 1 THEN 1 ELSE 0 END), 0) FROM admin.tenant_api_keys WHERE tenant_id = @p1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`,
			revokeProvisional:  `UPDATE admin.tenant_api_keys SET disabled_at = SYSUTCDATETIME(), updated_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND provisional = 1 AND disabled_at IS NULL`,
			confirmProvisional: `UPDATE admin.tenant_api_keys SET provisional = 0, expires_at = NULL, updated_at = SYSUTCDATETIME() WHERE tenant_id = @p1 AND provisional = 1 AND disabled_at IS NULL AND (expires_at IS NULL OR expires_at > SYSUTCDATETIME())`,
		}, nil
	default:
		return provisionStmts{}, fmt.Errorf("auth: tenant provisioning is not supported for %s: multi-tenancy is PostgreSQL and SQL Server only", dialect)
	}
}

type tenantRow struct {
	name, displayName, orgID, createdBy string
}

// ProvisionTenant creates a tenant and its provisional key, or replays an earlier create. See the
// file comment for the handshake. It is safe to call concurrently for the same id.
func (s *TenantStore) ProvisionTenant(ctx context.Context, req ProvisionRequest) (ProvisionResult, error) {
	st, err := provisionStatements(s.dialect)
	if err != nil {
		return ProvisionResult{}, err
	}
	if strings.TrimSpace(req.Creator) == "" {
		return ProvisionResult{}, errors.New("auth: a provisioning request needs a creator")
	}
	if (req.OrgID == uuid.Nil) == (req.OrgName == "") {
		return ProvisionResult{}, errors.New("auth: a provisioning request needs exactly one of an org id and an org name")
	}
	if req.OrgName != "" && len(req.OrgName) > 255 {
		return ProvisionResult{}, errors.New("auth: the org name is longer than 255 characters")
	}
	tid := req.TenantID
	if tid == uuid.Nil {
		tid = uuid.New()
	}
	name := req.Name
	if name == "" {
		name = tid.String()
	}
	if len(name) > 255 {
		return ProvisionResult{}, errors.New("auth: the tenant name is longer than 255 characters")
	}

	orgID, err := s.resolveOrg(ctx, st, req)
	if err != nil {
		return ProvisionResult{}, err
	}
	want := ProvisionedTenant{ID: tid, Name: name, DisplayName: req.DisplayName, OrgID: orgID}

	// Twice: the second pass is for a racing create that landed between our reads and our insert,
	// which turns this call into a replay of it.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, again, err := s.provisionOnce(ctx, st, req, want)
		if !again {
			return res, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("a concurrent create did not settle")
	}
	return ProvisionResult{}, fmt.Errorf("auth: provisioning tenant %s: %w", tid, lastErr)
}

// resolveOrg returns the org the tenant belongs under. By name it is find-or-create, and the
// insert's own error is ignored when the re-read finds the org: a racing creator of the same
// name made it, which is the outcome wanted.
func (s *TenantStore) resolveOrg(ctx context.Context, st provisionStmts, req ProvisionRequest) (uuid.UUID, error) {
	if req.OrgID != uuid.Nil {
		var one int
		if err := s.db.QueryRowContext(ctx, st.orgExists, req.OrgID.String()).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return uuid.Nil, ErrOrgNotFound
			}
			return uuid.Nil, err
		}
		return req.OrgID, nil
	}
	find := func() (uuid.UUID, error) {
		var raw string
		if err := s.db.QueryRowContext(ctx, st.orgByName, req.OrgName).Scan(&raw); err != nil {
			return uuid.Nil, err
		}
		return uuid.Parse(raw)
	}
	if id, err := find(); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, err
	}
	_, insertErr := s.db.ExecContext(ctx, st.insertOrg, uuid.New().String(), req.OrgName)
	id, err := find()
	if err != nil {
		if insertErr != nil {
			return uuid.Nil, insertErr
		}
		return uuid.Nil, err
	}
	return id, nil
}

func (s *TenantStore) readTenant(ctx context.Context, st provisionStmts, id uuid.UUID) (*tenantRow, error) {
	var r tenantRow
	err := s.db.QueryRowContext(ctx, st.tenantByID, id.String()).Scan(&r.name, &r.displayName, &r.orgID, &r.createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// provisionOnce is one pass. again is true when the pass lost a race and a second read could
// change the answer.
func (s *TenantStore) provisionOnce(ctx context.Context, st provisionStmts, req ProvisionRequest, want ProvisionedTenant) (res ProvisionResult, again bool, err error) {
	existing, err := s.readTenant(ctx, st, want.ID)
	if err != nil {
		return ProvisionResult{}, false, err
	}
	if existing != nil {
		res, err = s.replay(ctx, st, req, want, existing)
		return res, false, err
	}

	var holder string
	switch err := s.db.QueryRowContext(ctx, st.tenantByName, want.Name).Scan(&holder); {
	case err == nil:
		// The holder can be the very tenant being created: a racing create committed between the
		// read of the id above and this read of the name. That is a replay, not a taken name, and
		// the second pass will treat it as one.
		if held, perr := uuid.Parse(holder); perr == nil && held == want.ID {
			return ProvisionResult{}, true, nil
		}
		return ProvisionResult{}, false, &ProvisionConflictError{Reason: ConflictNameTaken}
	case !errors.Is(err, sql.ErrNoRows):
		return ProvisionResult{}, false, err
	}

	secret := GenerateAPIKey()
	keyID := uuid.New()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProvisionResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, st.insertTenant, want.ID.String(), want.Name, want.DisplayName, want.OrgID.String(), req.Creator); err != nil {
		// A failed insert is not necessarily a failure: a racing create of the same id or name is the
		// ordinary cause, and the re-read decides. Anything the re-read does not explain is returned.
		_ = tx.Rollback()
		if again, readErr := s.somethingAppeared(ctx, st, want); readErr == nil && again {
			return ProvisionResult{}, true, err
		}
		return ProvisionResult{}, false, err
	}
	if err := insertProvisionalKey(ctx, tx, st, keyID, want.ID, secret, req.KeyDescription); err != nil {
		return ProvisionResult{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProvisionResult{}, false, err
	}
	return ProvisionResult{
		Outcome: ProvisionCreated,
		Tenant:  want,
		Key:     &ProvisionedKey{KeyID: keyID, Secret: secret, ValidFor: ProvisionalKeyTTL},
	}, false, nil
}

// somethingAppeared reports whether a tenant with the wanted id or name exists now.
func (s *TenantStore) somethingAppeared(ctx context.Context, st provisionStmts, want ProvisionedTenant) (bool, error) {
	row, err := s.readTenant(ctx, st, want.ID)
	if err != nil || row != nil {
		return row != nil, err
	}
	var holder string
	switch err := s.db.QueryRowContext(ctx, st.tenantByName, want.Name).Scan(&holder); {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

func insertProvisionalKey(ctx context.Context, tx *sql.Tx, st provisionStmts, keyID, tenantID uuid.UUID, secret, description string) error {
	// Through sha256Hash, not a second sha256.Sum256 of its own: that is the one place a key becomes a
	// lookup hash, and its comment records why SHA-256 is right for it (the input is 256 bits from
	// crypto/rand, never a password) and that the static-analysis alert on it was reviewed and
	// dismissed. A new call site would be a new alert for the same reasoning.
	hash := sha256Hash(secret)
	_, err := tx.ExecContext(ctx, st.insertKey, keyID.String(), tenantID.String(), hash, description, int64(ProvisionalKeyTTL/time.Second))
	return err
}

// replay answers a create for an id that exists.
func (s *TenantStore) replay(ctx context.Context, st provisionStmts, req ProvisionRequest, want ProvisionedTenant, row *tenantRow) (ProvisionResult, error) {
	// The creator is checked FIRST, before anything about the tenant is compared or revealed: a
	// principal that did not create this tenant learns only that the id is taken.
	if row.createdBy != req.Creator {
		return ProvisionResult{}, &ProvisionConflictError{Reason: ConflictTenantExists}
	}
	var differs []string
	if row.name != want.Name {
		differs = append(differs, "name")
	}
	if !strings.EqualFold(row.orgID, want.OrgID.String()) {
		differs = append(differs, "org_id")
	}
	if len(differs) > 0 {
		return ProvisionResult{}, &ProvisionConflictError{Reason: ConflictTenantDiffers, Fields: differs}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProvisionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// The row lock serialises replays and confirms of this tenant, so two of them cannot each
	// decide there is no confirmed key and both mint one.
	var locked string
	if err := tx.QueryRowContext(ctx, st.lockTenant, want.ID.String()).Scan(&locked); err != nil {
		return ProvisionResult{}, err
	}
	var confirmed, provisional int64
	if err := tx.QueryRowContext(ctx, st.liveKeys, want.ID.String()).Scan(&confirmed, &provisional); err != nil {
		return ProvisionResult{}, err
	}
	res := ProvisionResult{Outcome: ProvisionReplayed, Tenant: want}
	res.Tenant.DisplayName = row.displayName
	if confirmed == 0 {
		if _, err := tx.ExecContext(ctx, st.revokeProvisional, want.ID.String()); err != nil {
			return ProvisionResult{}, err
		}
		secret := GenerateAPIKey()
		keyID := uuid.New()
		if err := insertProvisionalKey(ctx, tx, st, keyID, want.ID, secret, req.KeyDescription); err != nil {
			return ProvisionResult{}, err
		}
		res.Key = &ProvisionedKey{KeyID: keyID, Secret: secret, ValidFor: ProvisionalKeyTTL}
	}
	if err := tx.Commit(); err != nil {
		return ProvisionResult{}, err
	}
	return res, nil
}

// ConfirmResult says what a confirm did.
type ConfirmResult struct {
	// Confirmed is how many provisional keys became permanent. 0 with AlreadyConfirmed true is a
	// repeat of a confirm that already happened.
	Confirmed        int64
	AlreadyConfirmed bool
}

// ConfirmProvisionedTenant makes the creator's provisional key permanent. Only the creator may; for
// anyone else, and for an id that does not exist, the answer is the same ErrTenantNotFound.
func (s *TenantStore) ConfirmProvisionedTenant(ctx context.Context, tenantID uuid.UUID, creator string) (ConfirmResult, error) {
	st, err := provisionStatements(s.dialect)
	if err != nil {
		return ConfirmResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfirmResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var createdBy string
	if err := tx.QueryRowContext(ctx, st.lockTenant, tenantID.String()).Scan(&createdBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfirmResult{}, ErrTenantNotFound
		}
		return ConfirmResult{}, err
	}
	if createdBy == "" || createdBy != creator {
		return ConfirmResult{}, ErrTenantNotFound
	}
	res, err := tx.ExecContext(ctx, st.confirmProvisional, tenantID.String())
	if err != nil {
		return ConfirmResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ConfirmResult{}, err
	}
	out := ConfirmResult{Confirmed: n}
	if n == 0 {
		var confirmed, provisional int64
		if err := tx.QueryRowContext(ctx, st.liveKeys, tenantID.String()).Scan(&confirmed, &provisional); err != nil {
			return ConfirmResult{}, err
		}
		if confirmed == 0 {
			return ConfirmResult{}, ErrNoProvisionalKey
		}
		out.AlreadyConfirmed = true
	}
	if err := tx.Commit(); err != nil {
		return ConfirmResult{}, err
	}
	return out, nil
}
