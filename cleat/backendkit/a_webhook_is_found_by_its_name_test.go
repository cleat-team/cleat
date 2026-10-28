package backendkit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/cleat/backendkit"
)

// cleat#3538. WebhookByName lets an app find its own webhook by a stable name. The server side (uniqueness,
// the ?name= filter, the 400 for a name that cannot exist) is tested in plugins/notifications against every
// dialect; this is the client's half: that it asks for the right thing, rides the client's transport, and
// tells "none" from "refused" from "the worker misbehaved".
func TestWebhookByNameAsksForTheNameAndDecodesTheWebhook(t *testing.T) {
	var gotMethod, gotPath, gotName, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotName, gotAuth = r.Method, r.URL.Path, r.URL.Query().Get("name"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"w1","tenant_id":"t","url":"https://clew.example/dispatch","secret_configured":true,` +
			`"events":["dispatch"],"enabled":true,"name":"clew-dispatch","created_at":"2026-10-28T00:00:00Z"}]`))
	}))
	defer srv.Close()

	c := backendkit.New(srv.URL, backendkit.WithAPIKey("test-key"))
	// A name with characters that matter in a query string: it must arrive intact, not split into a second
	// parameter. (The worker would refuse this one as not a valid name; the client's job is only to carry it.)
	const name = "a&b=c d"
	w, err := c.WebhookByName(context.Background(), name)
	if err != nil {
		t.Fatalf("WebhookByName: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/webhooks" {
		t.Errorf("request was %s %s, want GET /webhooks", gotMethod, gotPath)
	}
	if gotName != name {
		t.Errorf("the server received name %q, want %q: the query string was not escaped", gotName, name)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization %q: the request did not ride the client's own transport", gotAuth)
	}
	if w.ID != "w1" || w.Name != "clew-dispatch" || w.URL != "https://clew.example/dispatch" ||
		!w.Enabled || !w.SecretConfigured || len(w.Events) != 1 || w.Events[0] != "dispatch" {
		t.Errorf("decoded %+v", *w)
	}
}

func TestWebhookByNameSaysWhenThereIsNoSuchWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	w, err := backendkit.New(srv.URL).WebhookByName(context.Background(), "nothing")
	if !errors.Is(err, backendkit.ErrWebhookNotFound) || w != nil {
		t.Fatalf("got %v, %v; want nil and ErrWebhookNotFound", w, err)
	}
}

// A refusal is not "none": a 400 for a name that cannot exist, or a 401, must not read as a webhook that is
// merely absent, because an app that treats absent as "create it" would then create one on every start.
func TestWebhookByNameDoesNotMistakeARefusalForAnAbsentWebhook(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"refused"}`, status)
		}))
		_, err := backendkit.New(srv.URL).WebhookByName(context.Background(), "x")
		srv.Close()

		var up *backendkit.UpstreamStatusError
		if err == nil || errors.Is(err, backendkit.ErrWebhookNotFound) || !errors.As(err, &up) || up.Status != status {
			t.Errorf("status %d: got %v, want an *UpstreamStatusError with that status and not ErrWebhookNotFound", status, err)
		}
	}
}

// The worker's unique index makes two webhooks with one name impossible. If one ever answers with two, the
// helper must not pick: the app would deliver through whichever it guessed.
func TestWebhookByNameRefusesToPickBetweenTwo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a","name":"n"},{"id":"b","name":"n"}]`))
	}))
	defer srv.Close()

	w, err := backendkit.New(srv.URL).WebhookByName(context.Background(), "n")
	if err == nil || w != nil || errors.Is(err, backendkit.ErrWebhookNotFound) {
		t.Fatalf("got %v, %v; want an error that is not ErrWebhookNotFound", w, err)
	}
}
