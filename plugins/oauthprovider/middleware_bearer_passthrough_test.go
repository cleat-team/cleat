package oauthprovider

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// noSessionDB answers every lookup with sql.ErrNoRows, which is what
// oauth_sessions returns for a token that is not an OAuth session -- a cleat
// API key, for instance.
type noSessionDB struct{ plugin.PluginDB }

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

func (noSessionDB) QueryRow(_ context.Context, _ string, _ ...any) plugin.RowScanner {
	return errRow{sql.ErrNoRows}
}

// sessionDB answers as though the token IS a live OAuth session.
type sessionDB struct{ plugin.PluginDB }

type okRow struct{}

func (okRow) Scan(dest ...any) error {
	// id, tenant_id, user_email, expires_at
	if len(dest) != 4 {
		return sql.ErrNoRows
	}
	if p, ok := dest[0].(*uuid.UUID); ok {
		*p = uuid.New()
	}
	if p, ok := dest[1].(*uuid.UUID); ok {
		*p = uuid.New()
	}
	if p, ok := dest[2].(*sql.NullString); ok {
		*p = sql.NullString{String: "someone@example.com", Valid: true}
	}
	if p, ok := dest[3].(*sql.NullTime); ok {
		*p = sql.NullTime{Valid: false}
	}
	return nil
}

func (sessionDB) QueryRow(_ context.Context, _ string, _ ...any) plugin.RowScanner {
	return okRow{}
}

// TestABearerTokenThatIsNotAnOAuthSessionFallsThrough.
//
// # The regression
//
// This middleware intercepts ANY `Authorization: Bearer …`, looks the token up
// in oauth_sessions, and returned 401 when it was absent. A cleat API key is
// presented exactly that way, so once #891 linked oauthprovider -- and
// cmd/cleat-worker/main.go wraps the whole handler chain in every plugin
// implementing HasMiddleware -- every authenticated request answered
// `401 {"error":"invalid session"}` before reaching auth.Middleware.
//
// `--require-auth` defaults to true, so a default deployment served an API
// where no key worked at all (#912).
//
// # Why its own tests passed
//
// They exercise it with OAuth tokens, and with an OAuth token it is correct.
// It only becomes wrong sitting in front of a handler that accepts a DIFFERENT
// bearer scheme, which is exactly what linking it did. A middleware cannot tell
// "this is not my token" from "this is an invalid token of mine" unless it is
// written to, and this one treated the first as the second.
//
// # What it must do instead
//
// Fall through. Refusing an unauthenticated request is auth.Middleware's
// decision and it still makes it -- a request with no valid credential is
// refused there. Nothing is loosened: what changes is WHICH component decides,
// and only the component that owns every accepted scheme can decide correctly.
func TestABearerTokenThatIsNotAnOAuthSessionFallsThrough(t *testing.T) {
	p := &Plugin{db: noSessionDB{}, logger: slog.Default()}

	reached := false
	h := p.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil)
	req.Header.Set("Authorization", "Bearer cleat_api_key_not_an_oauth_session")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached {
		t.Errorf("the request never reached the next handler; the OAuth middleware answered %d %q.\n\n"+
			"A bearer token absent from oauth_sessions is not necessarily invalid -- it is most "+
			"likely a credential for another scheme, and a cleat API key is presented exactly "+
			"this way. Rejecting here means auth.Middleware never runs and no API key works "+
			"anywhere. Fall through and let the component that owns every scheme decide.",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (the next handler's answer)", rec.Code)
	}
}

// TestAnOAuthSessionStillGetsItsContext is the other direction, and it is what
// stops the fix above being "delete the middleware": a token that IS an OAuth
// session must still be recognised and still inject SessionInfo.
func TestAnOAuthSessionStillGetsItsContext(t *testing.T) {
	p := &Plugin{db: sessionDB{}, logger: slog.Default()}

	var got *SessionInfo
	h := p.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = SessionFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil)
	req.Header.Set("Authorization", "Bearer ddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd4")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got == nil {
		t.Fatal("a valid OAuth session no longer injects SessionInfo; the fall-through went too far " +
			"and the middleware now does nothing at all")
	}
	if got.UserEmail != "someone@example.com" {
		t.Errorf("UserEmail = %q, want someone@example.com", got.UserEmail)
	}
}

// TestAFreshlyGeneratedSessionTokenLooksLikeOne ties looksLikeSessionToken to
// the generator it describes.
//
// The predicate encodes a format defined in generateSessionToken, and a
// predicate describing a format defined elsewhere rots silently: if the
// generator changes and this does not, the plugin stops recognising its OWN
// sessions and every OAuth login falls through to a generic 401. Generating a
// real token and asserting the predicate accepts it is the cheapest way to make
// that a red test rather than a mystery.
func TestAFreshlyGeneratedSessionTokenLooksLikeOne(t *testing.T) {
	for i := 0; i < 20; i++ {
		tok, err := generateSessionToken()
		if err != nil {
			t.Fatalf("generateSessionToken: %v", err)
		}
		if !looksLikeSessionToken(tok) {
			t.Fatalf("looksLikeSessionToken rejected a token this plugin just issued: %q (len %d).\n\n"+
				"The generator and the predicate have diverged, so the middleware no longer "+
				"recognises its own sessions and every OAuth request now falls through.", tok, len(tok))
		}
	}

	// And the other direction: a cleat API key must NOT look like one, or the
	// fix for #912 is undone and API keys are claimed again.
	for _, notOurs := range []string{
		"cleat_sk_testvalidkey123",
		"nonexistent-token",
		"",
		strings.Repeat("g", 64), // right length, not hex
		strings.Repeat("a", 63), // hex, wrong length
	} {
		if looksLikeSessionToken(notOurs) {
			t.Errorf("looksLikeSessionToken claimed %q, which this plugin did not issue", notOurs)
		}
	}
}
