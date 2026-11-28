package main

// cleat#3775. GET /api/workflows?state.<key>=<value> selects runs by what they published. The mock store
// returns the same rows whatever it is asked, so a response body cannot show a parameter was read; the
// filter the handler hands the store can, which is what listSpy records.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

func TestStateParametersReachTheStoreAsAFilter(t *testing.T) {
	for _, c := range []struct {
		name string
		url  string
		want map[string]string
	}{
		{"one pair", "/api/workflows?state.order_id=123", map[string]string{"order_id": "123"}},
		{"two pairs", "/api/workflows?state.order_id=123&state.status=open", map[string]string{"order_id": "123", "status": "open"}},
		{"an empty value is a value", "/api/workflows?state.status=", map[string]string{"status": ""}},
		{"an empty key is a key", "/api/workflows?state.=v", map[string]string{"": "v"}},
		{"a key and a value are percent-decoded", "/api/workflows?state.a%20b=%C3%A9%26", map[string]string{"a b": "é&"}},
		{"a key containing a dot keeps it", "/api/workflows?state.a.b=1", map[string]string{"a.b": "1"}},
		{"case is kept", "/api/workflows?state.ORDER_ID=Open", map[string]string{"ORDER_ID": "Open"}},
		{"at the cap", "/api/workflows?state.a=1&state.b=2&state.c=3&state.d=4", map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}},
		{"no state parameter is no filter", "/api/workflows?def_name=x", nil},
		{"a parameter that merely begins with state is not one", "/api/workflows?statement=1&states=2", nil},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sp := newListSpy(nil, 0)
			resp := get(t, sp, c.url)
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d (%s), want 200", resp.StatusCode, body)
			}
			if !reflect.DeepEqual(sp.got.QueryState, c.want) {
				t.Errorf("filter.QueryState = %#v, want %#v -- the handler did not read the parameter as written", sp.got.QueryState, c.want)
			}
		})
	}
}

// What the handler cannot honour it refuses, because an ignored filter is a wider answer read as a narrow
// one. Every case also asserts the store was never asked: a refusal that still runs the listing is a refusal
// that also leaks the unfiltered page.
func TestAStateFilterTheHandlerCannotHonourIsRefusedBeforeTheStoreIsAsked(t *testing.T) {
	for _, c := range []struct {
		name, url, mention string
	}{
		{"a bare state", "/api/workflows?state=123", "state.<key>"},
		{"the same key twice", "/api/workflows?state.order_id=1&state.order_id=2", "state.order_id was given 2 times"},
		{"five pairs", "/api/workflows?state.a=1&state.b=2&state.c=3&state.d=4&state.e=5", "at most"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sp := newListSpy([]engine.WorkflowInstance{{ID: "must-not-be-returned"}}, 1)
			asked := false
			sp.store.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
				asked = true
				return nil, nil
			}
			sp.store.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) {
				asked = true
				return 0, nil
			}
			resp := get(t, sp, c.url)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (%s)", resp.StatusCode, body)
			}
			// Decoded, because the encoder writes "<" as \u003c and the message is what a caller reads.
			var e struct{ Error string }
			if err := json.Unmarshal(body, &e); err != nil || !strings.Contains(e.Error, c.mention) {
				t.Errorf("error %q (%v) does not say %q, so the caller is not told what to change", e.Error, err, c.mention)
			}
			if asked {
				t.Errorf("the store was asked for a listing the handler had already refused")
			}
		})
	}
}

// A sealing deployment cannot search query_state (it is ciphertext), and says so with the flag to look at,
// as a 422 and not a 500: the caller asked for something this deployment does not do, which is not a fault
// and not worth a retry.
func TestAStateFilterOnADeploymentThatSealsQueryStateIsA422NamingTheFlag(t *testing.T) {
	sp := newListSpy(nil, 0)
	refuse := func(context.Context, engine.WorkflowFilter) error {
		return fmt.Errorf("listing: %w", engine.ErrQueryStateFilterUnavailableUnderEncryption)
	}
	sp.store.countWorkflowsFn = func(ctx context.Context, f engine.WorkflowFilter) (int, error) { return 0, refuse(ctx, f) }
	sp.store.listWorkflowsFn = func(ctx context.Context, f engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		return nil, refuse(ctx, f)
	}
	resp := get(t, sp, "/api/workflows?state.order_id=123")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d (%s), want 422", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "--encrypt-sensitive-payloads") {
		t.Errorf("body %q does not name --encrypt-sensitive-payloads", body)
	}
	var e struct{ Error string }
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		t.Errorf("the refusal is not the API's usual error body: %q (%v)", body, err)
	}
}

// And the mapping is by the sentinel, so any other store error is still a 500 and is not mistaken for a
// refusal of the caller's request.
func TestAnyOtherListingFailureIsStillA500(t *testing.T) {
	sp := newListSpy(nil, 0)
	sp.store.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) { return 0, errors.New("connection reset") }
	resp := get(t, sp, "/api/workflows?state.order_id=123")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// The parser directly, for the properties the handler cases above reach only through a URL.
func TestQueryStateFilterFromParams(t *testing.T) {
	q := url.Values{}
	q.Add("state.b", "2")
	q.Add("state.a", "1")
	q.Add("limit", "5")
	got, err := queryStateFilterFromParams(q)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"a": "1", "b": "2"}) {
		t.Fatalf("got %v, %v", got, err)
	}

	// Two rules broken at once report the same error every time (map iteration order is random).
	q = url.Values{}
	q.Add("state.z", "1")
	q.Add("state.z", "2")
	q.Add("state.a", "1")
	q.Add("state.a", "2")
	for i := 0; i < 20; i++ {
		if _, err := queryStateFilterFromParams(q); err == nil || !strings.Contains(err.Error(), "state.a was given 2 times") {
			t.Fatalf("run %d: err = %v, want the sorted-first duplicate (state.a)", i, err)
		}
	}

	q = url.Values{"state.a": {"1"}, "state.b": {"2"}, "state.c": {"3"}, "state.d": {"4"}, "state.e": {"5"}}
	if _, err := queryStateFilterFromParams(q); !errors.Is(err, engine.ErrQueryStateFilterTooManyPairs) {
		t.Errorf("five pairs: err = %v, want ErrQueryStateFilterTooManyPairs", err)
	}
}
