package backendkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The refusals a Provisioner names. Each is returned wrapped alongside the *UpstreamStatusError, so
// errors.Is finds the sentinel and errors.As still finds the status and body.
var (
	// ErrTenantExists: a tenant with this id exists and this credential did not create it (or nobody
	// recorded who did, as for a tenant made with cleat-worker --create-tenant). The worker says nothing more
	// about it on purpose. Not retryable.
	ErrTenantExists = errors.New("a tenant with this id exists and this credential did not create it")

	// ErrNameTaken: another tenant already has this name. Tenant names are unique across the deployment, not
	// per org. Not retryable with the same name.
	ErrNameTaken = errors.New("another tenant already has this name")

	// ErrTenantDiffers: a tenant with this id was created by this credential but with different values. The
	// *TenantDiffersError in the chain names which. Not retryable unchanged.
	ErrTenantDiffers = errors.New("a tenant with this id exists and differs from the request")

	// ErrOrgNotFound: the request named an org_id that does not exist.
	ErrOrgNotFound = errors.New("no org has that id")

	// ErrNoProvisionalKey: a confirm named a key that is no longer the tenant's provisional key, because it
	// lapsed or because a repeated create replaced it. Repeat the create and confirm the key it returns.
	ErrNoProvisionalKey = errors.New("that key is not the tenant's provisional key (it lapsed or was replaced)")

	// ErrProvisioningUnavailable: this worker cannot create tenants (MySQL is single-tenant; a sharded
	// worker is not supported). Not retryable.
	ErrProvisioningUnavailable = errors.New("this worker cannot create tenants")
)

// TenantDiffersError says which fields of an existing tenant differ from a repeated create. It matches
// ErrTenantDiffers under errors.Is.
type TenantDiffersError struct {
	// Fields are the request fields that differ, in the worker's words (for example "name", "org_id").
	Fields []string
}

func (e *TenantDiffersError) Error() string {
	return "tenant differs in: " + strings.Join(e.Fields, ", ")
}

// Is makes errors.Is(err, ErrTenantDiffers) true for a *TenantDiffersError.
func (e *TenantDiffersError) Is(target error) bool { return target == ErrTenantDiffers }

// TenantRequest is what EnsureTenant creates.
type TenantRequest struct {
	// TenantID is a UUID YOU choose, and it is required. It is what makes the create idempotent: a retry
	// after a lost response names the same tenant. Without it the worker would invent a new id each time and
	// a retry would create a second tenant. The nil UUID is refused by the worker.
	TenantID string

	// Name is optional and unique across the whole deployment. Omitted, the worker uses the tenant id.
	Name string

	// DisplayName is optional and is not part of identity.
	DisplayName string

	// OrgID or OrgName, exactly one. OrgName finds the org or creates it.
	OrgID   string
	OrgName string

	// KeyDescription labels the tenant's first key.
	KeyDescription string
}

// Tenant is a tenant as the worker reports it.
type Tenant struct {
	ID          string
	Name        string
	DisplayName string
	OrgID       string

	// Created is true when this call created the tenant, false when it found one this credential had already
	// created.
	Created bool
}

// FirstKey is the tenant's first API key, provisional until ConfirmKey makes it permanent.
//
// Secret is the only copy: the worker stores its hash. Read the Secret field to store it, never a printed or
// encoded FirstKey. A FirstKey is redacted when it is printed with any fmt verb (directly, or as an exported
// field or element of a struct, slice or map), logged through slog (as an attribute, or inside a value the
// JSON handler encodes), or passed to json.Marshal.
//
// What this does NOT cover: fmt cannot call a method on an UNEXPORTED struct field, so a struct that holds a
// FirstKey in an unexported field prints the secret under %v; reflection reads it; and any copy of the Secret
// string is an ordinary string. Keep a FirstKey in an exported field, or keep only what you need of it.
type FirstKey struct {
	KeyID  string
	Secret string

	// ValidFor is how long the key works if it is never confirmed.
	ValidFor time.Duration
}

// Format prints the key without its secret, under every verb, so %v, %+v, %#v and %s on a FirstKey, or on a
// struct holding one in an exported field, cannot reveal it.
func (k FirstKey) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "backendkit.FirstKey{KeyID:%s, Secret:<redacted>, ValidFor:%s}", k.KeyID, k.ValidFor)
}

// LogValue keeps the secret out of slog when a FirstKey is an attribute's value.
func (k FirstKey) LogValue() slog.Value {
	return slog.GroupValue(slog.String("key_id", k.KeyID), slog.String("secret", "<redacted>"), slog.Duration("valid_for", k.ValidFor))
}

// MarshalJSON encodes the key without its secret. slog's JSON handler does not consult LogValue for a value
// nested in a struct; it encodes the struct with encoding/json, which would print every field. Encoding a
// FirstKey therefore never stores it: keep Secret yourself.
func (k FirstKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		KeyID           string `json:"key_id"`
		Secret          string `json:"secret"`
		ValidForSeconds int64  `json:"valid_for_seconds"`
	}{KeyID: k.KeyID, Secret: "<redacted>", ValidForSeconds: int64(k.ValidFor / time.Second)})
}

// Provisioner creates tenants on a worker (cleat#3552). It holds an operator credential with the provision
// scope, which can create tenants and nothing else: it cannot reach any tenant's data, and it can mint a key
// only inside the create handshake, for a tenant it created that has no confirmed key.
type Provisioner struct {
	c *Client
}

// NewProvisioner returns a Provisioner that calls the worker through c. c must carry the provisioner's operator
// key (WithAPIKey); a tenant key is refused by the worker with a 403.
func NewProvisioner(c *Client) *Provisioner { return &Provisioner{c: c} }

type ensureTenantBody struct {
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	OrgID       string `json:"org_id,omitempty"`
	OrgName     string `json:"org_name,omitempty"`
	Key         struct {
		Description string `json:"description,omitempty"`
	} `json:"key"`
}

type ensureTenantReply struct {
	Outcome string `json:"outcome"`
	Tenant  struct {
		TenantID    string `json:"tenant_id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		OrgID       string `json:"org_id"`
	} `json:"tenant"`
	Key *struct {
		KeyID           string `json:"key_id"`
		Secret          string `json:"secret"`
		ValidForSeconds int64  `json:"valid_for_seconds"`
	} `json:"key"`
}

// EnsureTenant creates the tenant, or finds the one this credential already created with the same values.
//
// The handshake has two halves and the app owns the order between them:
//
//  1. EnsureTenant returns a provisional key. It works at once and lapses after FirstKey.ValidFor.
//  2. Store the key durably, then call ConfirmKey with its id.
//
// If the response is lost, or the app crashes before storing, do nothing special: repeat the same
// EnsureTenant. While no key has been confirmed it returns a fresh provisional key (and revokes the previous
// one). Once a key is confirmed, a repeat returns the tenant and a nil *FirstKey, so a repeat can never be used
// to obtain a second working key.
func (p *Provisioner) EnsureTenant(ctx context.Context, req TenantRequest) (Tenant, *FirstKey, error) {
	if req.TenantID == "" {
		return Tenant{}, nil, errors.New("backendkit: TenantRequest.TenantID is required: it is the idempotency key, and without " +
			"one a retry after a lost response would create a second tenant")
	}
	if (req.OrgID == "") == (req.OrgName == "") {
		return Tenant{}, nil, errors.New("backendkit: give exactly one of TenantRequest.OrgID and TenantRequest.OrgName")
	}
	body := ensureTenantBody{TenantID: req.TenantID, Name: req.Name, DisplayName: req.DisplayName, OrgID: req.OrgID, OrgName: req.OrgName}
	body.Key.Description = req.KeyDescription

	var reply ensureTenantReply
	if err := p.c.doJSON(ctx, "provision tenant", http.MethodPost, "/api/admin/tenants", body, &reply); err != nil {
		return Tenant{}, nil, classifyProvisionError(err)
	}
	t := Tenant{
		ID: reply.Tenant.TenantID, Name: reply.Tenant.Name, DisplayName: reply.Tenant.DisplayName,
		OrgID: reply.Tenant.OrgID, Created: reply.Outcome == "created",
	}
	var k *FirstKey
	if reply.Key != nil {
		k = &FirstKey{KeyID: reply.Key.KeyID, Secret: reply.Key.Secret, ValidFor: time.Duration(reply.Key.ValidForSeconds) * time.Second}
	}
	return t, k, nil
}

// ConfirmKey makes the tenant's provisional key permanent. Call it after the key is stored, not before: a
// confirmed key is the only one the worker will not replace, and a key nobody stored is a tenant nobody can
// reach. It names the key because a repeated EnsureTenant can replace the provisional one, and a confirm that
// took no key id would make permanent whichever happened to be live. Confirming an already confirmed key is
// not an error.
//
// A 404 arrives as a plain *UpstreamStatusError and is not given a sentinel, because it means one of three
// things the worker answers identically on purpose: no such tenant, a tenant this credential did not create,
// or a worker with the admin API switched off.
func (p *Provisioner) ConfirmKey(ctx context.Context, tenantID, keyID string) error {
	if tenantID == "" || keyID == "" {
		return errors.New("backendkit: ConfirmKey needs the tenant id and the key id EnsureTenant returned")
	}
	path := "/api/admin/tenants/" + url.PathEscape(tenantID) + "/confirm"
	if err := p.c.doJSON(ctx, "confirm tenant key", http.MethodPost, path, map[string]string{"key_id": keyID}, nil); err != nil {
		return classifyProvisionError(err)
	}
	return nil
}

// classifyProvisionError names the refusals the worker's provisioning routes document, and leaves every other
// error as it came. It reads what the worker said (the machine-readable reason, or for the one 404 that is
// distinguishable the message, which is all it carries), never the status alone: a 404 can also mean the route is absent.
func classifyProvisionError(err error) error {
	var up *UpstreamStatusError
	if !errors.As(err, &up) {
		return err
	}
	var b struct {
		Error  string   `json:"error"`
		Reason string   `json:"reason"`
		Fields []string `json:"fields"`
	}
	_ = json.Unmarshal(up.Body, &b)

	switch up.Status {
	case http.StatusConflict:
		switch b.Reason {
		case "name_taken":
			return fmt.Errorf("%w: %w", ErrNameTaken, err)
		case "tenant_exists":
			return fmt.Errorf("%w: %w", ErrTenantExists, err)
		case "tenant_differs":
			return fmt.Errorf("%w: %w", &TenantDiffersError{Fields: b.Fields}, err)
		case "no_provisional_key":
			return fmt.Errorf("%w: %w", ErrNoProvisionalKey, err)
		}
	case http.StatusNotFound:
		switch b.Error {
		case "org not found":
			return fmt.Errorf("%w: %w", ErrOrgNotFound, err)
		}
	case http.StatusNotImplemented:
		return fmt.Errorf("%w: %w", ErrProvisioningUnavailable, err)
	}
	return err
}
