package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// This file is the operator credential's vocabulary: what one looks like on the
// wire, how it is looked up, and what it is called once it has been. cleat#2169.
//
// A TENANT key authenticates a party inside the tenancy model -- it resolves to
// a tenant, and every route it reaches is filtered by that tenant. An operator
// key is the other thing: it authenticates the person who runs the deployment,
// resolves to no tenant, and is the only credential that may reach a route
// looking ACROSS tenants. They have the same shape and the same lifecycle (see
// migrations/{postgres/010,mysql/008,mssql/009}_operator_api_keys.sql) and are
// deliberately not interchangeable.
//
// What is NOT here is the middleware that accepts one. That is the security
// decision -- which routes an operator may reach, and what a request carrying
// an operator key is allowed to be -- and its shape depends on how it composes
// with MiddlewareWithMux, which cmd/cleat-worker/app.go owns and which cleat#2971
// is currently rewriting (R6). The vocabulary below does not depend on that
// composition, so it is settled first and the composition is placed once, in one
// file, when the route gating lands.

// OperatorKeyPrefix is the prefix every operator key carries, and it is a
// security boundary rather than a naming convention.
//
// A tenant key and an operator key are hashed and looked up the same way, so
// without a wire-level distinction the only thing separating them is which
// table the resolver consults -- and a lookup that misses in the tenant table
// returns "invalid key", which is exactly what a correct operator key would get
// if it reached the wrong resolver. The prefix makes the credential's type a
// property of the credential, checkable before any query runs, so neither key
// can be accepted by the other's path merely because the other table happened
// to hold a matching hash.
//
// It is also disjoint from every other bearer shape cleat accepts. The OAuth
// session-token gate (plugins/oauthprovider, looksLikeSessionToken) accepts a
// bare 64-character lowercase-hex string; "cleat_op_" is 9 characters of
// non-hex, so an operator key falls through that gate to auth.Middleware
// exactly as "cleat_sk_" does. TestAnOperatorKeyIsNotASessionToken pins the
// half of that this package can see.
const OperatorKeyPrefix = "cleat_op_"

// GenerateOperatorKey generates a random operator API key.
//
// Same entropy and same reasoning as GenerateAPIKey -- 32 bytes from randRead,
// hex-encoded; see sha256Hash's comment for why hashing a 256-bit random secret
// with SHA-256 is not the weak-KDF defect CodeQL reports. Only the prefix
// differs, and it differs so the two credential types are distinguishable
// before either is looked up.
func GenerateOperatorKey() string {
	b := make([]byte, 32)
	_, _ = randRead(b)
	return fmt.Sprintf("%s%x", OperatorKeyPrefix, b)
}

// IsOperatorKey reports whether key is an operator key, by its prefix.
//
// This is a routing predicate, not an authentication check: it says which
// resolver to ask and nothing about whether the key is live, unexpired or
// undisabled. A key that passes it is still only accepted once
// ResolveOperatorFromAPIKey finds it.
func IsOperatorKey(key string) bool {
	return strings.HasPrefix(key, OperatorKeyPrefix)
}

// Operator is the credential a request was authenticated as, as opposed to the
// tenant a request acts within.
type Operator struct {
	// KeyID is admin.operator_api_keys.key_id -- the credential's stable
	// identity. It is what an audit record carries, and it names the ROW, not
	// the person: two keys for one operator are two identities, and revoking
	// one must not be describable as revoking an operator. Revocation acts on
	// this value, so it must stay the key's own; a caller that wants to say
	// "all of this operator's keys" has to look that up, because nothing here
	// carries a human identity to group them by.
	KeyID string

	// Description is the operator-supplied label from the same row, for
	// reading and never for deciding: it is unvalidated text an operator
	// chose, it is not unique, and nothing may be authorised by comparing it.
	Description string

	// Disabled and ExpiresAt are the credential's STATE rather than its
	// identity, and only ListOperatorKeys fills them: a credential that
	// authenticated a request is live by definition, so
	// ResolveOperatorFromAPIKey leaves both zero and a handler must not read
	// them there. They are on this type rather than a separate row type
	// because a second near-identical struct is a second thing to keep in step
	// with the same table, and the two directions are already distinguishable
	// without one: a zero Operator is not a credential (OperatorFromContext
	// says so), while a listed one is always a row.
	Disabled bool

	// ExpiresAt is nil for a key that never expires, which is every key
	// created before this field existed and any permanent one since. Nil is
	// therefore "no expiry" and not "unknown".
	ExpiresAt *time.Time
}

// String is the audit attribution recorded for an action this operator took.
//
// The prefix is load-bearing. The value lands in AdminActionEvent.Operator
// (engine/store_admin.go), which held a TENANT UUID before operator keys
// existed -- api_admin.go's operatorFromContext still falls back to one -- and
// a key_id is also a UUID. Bare, the two would be indistinguishable in the same
// audit field, so a reader could not tell a force-complete taken from inside
// the tenant from one taken by the person running the deployment. "operator:"
// is the smallest thing that keeps those two answers apart; it is also short
// enough that it cannot be mistaken for a UUID by anything parsing the column.
func (o Operator) String() string {
	return "operator:" + o.KeyID
}

// OperatorResolver is the only thing resolving an operator key needs from a
// store: turning a key hash into the credential it names.
//
// Narrowed for the reason TenantResolver documents at more length: on MySQL
// tenant isolation is one database per tenant, so the store serving a
// TENANT-scoped request looks for keys in that tenant's database, while
// operator keys -- which belong to no tenant -- live in the base one. An
// operator lookup must therefore never be handed a tenant-scoped store. This
// interface is what makes that visible rather than assumed: any implementation
// satisfies it, but the one that is correct is not tenant-scoped.
//
// Expiry and disabled_at are the implementation's to enforce, and the caveat
// TenantResolver records applies here too -- nothing in this interface
// guarantees it, and only a contract test would. The PostgreSQL migration's
// index over key_hash WHERE disabled_at IS NULL is likewise only useful to a
// query that filters on that column; the index is not itself a check.
type OperatorResolver interface {
	ResolveOperatorFromAPIKey(ctx context.Context, keyHash []byte) (Operator, error)
}

// OperatorFromAPIKey looks up an operator credential by key hash.
//
// It refuses an empty KeyID rather than passing one through. A resolver that
// matched a row always has one -- key_id is that table's primary key -- so an
// empty value means the implementation built an Operator without reading the
// row, and the cost of not noticing is an audit record reading "operator:"
// that names no credential at all. Failing closed costs one comparison and
// keeps that record sayable.
func OperatorFromAPIKey(ctx context.Context, store OperatorResolver, keyHash []byte) (Operator, error) {
	op, err := store.ResolveOperatorFromAPIKey(ctx, keyHash)
	if err != nil {
		return Operator{}, err
	}
	if op.KeyID == "" {
		return Operator{}, fmt.Errorf("operator resolver returned a credential with no key id")
	}
	return op, nil
}

// operatorContextKey is unexported for the reason subjectContextKey is (see
// WithSubject): an identical struct declared in another package is a DIFFERENT
// key that silently reads nothing.
type operatorContextKey struct{}

// WithOperator sets the authenticated operator credential in the context.
//
// A request carries an operator OR a tenant, never both, and this does not have
// to clear one to achieve that: the two are set by different branches of the
// authentication path, and a request that reached the operator branch never had
// a tenant to clear.
//
// Deliberately not folded into WithSubject. A subject is a neutral identity
// string from whatever method authenticated the request, for a plugin to
// attribute an action to; an Operator is a specific credential with a key id
// that revocation acts on. A plugin that only reads subjects must keep working
// unchanged when the caller is an operator, and it must not be able to start
// authorising on a value that only the operator path populates -- which is what
// a shared key would eventually invite.
func WithOperator(ctx context.Context, op Operator) context.Context {
	return context.WithValue(ctx, operatorContextKey{}, op)
}

// OperatorFromContext extracts the operator credential set by WithOperator.
//
// ok is false for every request authenticated any other way -- a tenant key, a
// session token, or no credential at all -- so a caller asking "am I talking to
// an operator?" must read ok, not the zero Operator. An Operator{} with an
// empty KeyID is not a credential and is never a positive answer; that is also
// why this returns a struct rather than a string, since a bare string has no
// way to say "nothing here" that a caller cannot confuse with a real value.
func OperatorFromContext(ctx context.Context) (Operator, bool) {
	op, ok := ctx.Value(operatorContextKey{}).(Operator)
	return op, ok
}
