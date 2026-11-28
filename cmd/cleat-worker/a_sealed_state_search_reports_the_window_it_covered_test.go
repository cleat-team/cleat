package main

// cleat#3775, part 2. On a deployment that seals query_state the listing's state filter is answered by a
// window search in the worker. What the handler owes the caller is the truth about the window: how big it
// was, how much it covered, whether older runs were left unexamined. The mock store returns whatever it is
// told, so the assertions are on what the handler passes in and what it reports out.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// windowStore is a mockStore that can search a window. sealed says whether it claims to seal query_state.
type windowStore struct {
	*mockStore
	sealed bool
	result *engine.QueryStateWindowResult
	err    error

	calls      int
	gotFilter  engine.WorkflowFilter
	gotWindow  int
	listCalled bool
}

func (w *windowStore) SealsQueryState() bool { return w.sealed }
func (w *windowStore) SearchQueryStateWindow(_ context.Context, f engine.WorkflowFilter, window int) (*engine.QueryStateWindowResult, error) {
	w.calls++
	w.gotFilter, w.gotWindow = f, window
	return w.result, w.err
}

func newWindowStore(sealed bool, res *engine.QueryStateWindowResult, err error) *windowStore {
	ws := &windowStore{mockStore: &mockStore{}, sealed: sealed, result: res, err: err}
	ws.mockStore.listWorkflowsFn = func(context.Context, engine.WorkflowFilter) ([]engine.WorkflowInstance, error) {
		ws.listCalled = true
		return nil, nil
	}
	ws.mockStore.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) { return 0, nil }
	return ws
}

func serveList(t *testing.T, st engine.WorkflowStore, window int, url string) *http.Response {
	t.Helper()
	// storeFor hands back s.store itself when the request names no tenant and auth is off, so the wrapper
	// (and with it the type assertion the handler makes) is what the handler sees. newTestAPIServer takes a
	// *mockStore, hence building the server here.
	var base *mockStore
	switch v := st.(type) {
	case *windowStore:
		base = v.mockStore
	case *mockStore:
		base = v
	default:
		t.Fatalf("serveList: unsupported store %T", st)
	}
	api := &apiServer{
		store:                  st,
		worker:                 newTestWorker(base),
		maxBodySize:            1 << 20,
		factory:                &fakeStoreFactory{fallback: base},
		queryStateSearchWindow: window,
	}
	w := httptest.NewRecorder()
	api.handleWorkflowsList(w, httptest.NewRequest(http.MethodGet, url, nil))
	return w.Result()
}

func errBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	var e struct{ Error string }
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("not the API's error body: %q", b)
	}
	return e.Error
}

func TestASealedStateSearchReportsTheWindowItCovered(t *testing.T) {
	for _, c := range []struct {
		name string
		res  engine.QueryStateWindowResult
		want map[string]string
	}{
		{"a complete window", engine.QueryStateWindowResult{Matched: 1, Window: 5000, Scanned: 812, Exhausted: false},
			map[string]string{"X-Total-Count": "1", hdrQueryStateWindow: "5000", hdrQueryStateScanned: "812", hdrQueryStateExhausted: "false", hdrQueryStateUnreadable: "0"}},
		{"a window that was used up, with no match", engine.QueryStateWindowResult{Matched: 0, Window: 5000, Scanned: 5000, Exhausted: true},
			map[string]string{"X-Total-Count": "0", hdrQueryStateWindow: "5000", hdrQueryStateScanned: "5000", hdrQueryStateExhausted: "true", hdrQueryStateUnreadable: "0"}},
		{"runs that would not open", engine.QueryStateWindowResult{Matched: 2, Window: 5000, Scanned: 40, Unreadable: 3},
			map[string]string{"X-Total-Count": "2", hdrQueryStateWindow: "5000", hdrQueryStateScanned: "40", hdrQueryStateExhausted: "false", hdrQueryStateUnreadable: "3"}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res := c.res
			ws := newWindowStore(true, &res, nil)
			resp := serveList(t, ws, 5000, "/api/workflows?state.order_id=123&status=ready")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (%s)", resp.StatusCode, errBody(t, resp))
			}
			for h, want := range c.want {
				if got := resp.Header.Get(h); got != want {
					t.Errorf("%s = %q, want %q", h, got, want)
				}
			}
			var body []engine.WorkflowInstance
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body == nil {
				t.Errorf("the body must stay a bare, non-null array for existing callers: %v (null=%v)", err, body == nil)
			}
			if ws.gotWindow != 5000 || ws.gotFilter.QueryState["order_id"] != "123" || ws.gotFilter.Status != "ready" {
				t.Errorf("the search was asked window=%d filter=%+v; want the configured window and the request's pairs and status", ws.gotWindow, ws.gotFilter)
			}
			if ws.listCalled {
				t.Error("the SQL listing ran as well: the window search is the answer, not an addition to one")
			}
		})
	}
}

// The window path is a new way out for run data, so it goes through writeWorkflows like every other.
func TestTheWindowSearchRedactsWhenAskedLikeEveryOtherEgress(t *testing.T) {
	requireRedactable(t, egressSecret)
	res := &engine.QueryStateWindowResult{Matched: 1, Window: 100, Scanned: 1,
		Runs: []engine.WorkflowInstance{{ID: "wf-1", Status: "done", Input: json.RawMessage(egressSecret), Result: egressSecret}}}
	for _, c := range []struct {
		suffix   string
		redacted bool
	}{{"", false}, {"&redact=true", true}} {
		resp := serveList(t, newWindowStore(true, res, nil), 100, "/api/workflows?state.order_id=1"+c.suffix)
		b, _ := io.ReadAll(resp.Body)
		if got := !strings.Contains(string(b), "sk-live-SECRET123"); got != c.redacted {
			t.Errorf("suffix %q: redacted=%v, want %v:\n%s", c.suffix, got, c.redacted, b)
		}
	}
}

func TestTheWindowSearchIsConsultedOnlyWhereItIsNeeded(t *testing.T) {
	t.Run("a store that does not seal is answered by the SQL filter, with no window", func(t *testing.T) {
		ws := newWindowStore(false, &engine.QueryStateWindowResult{}, nil)
		resp := serveList(t, ws, 5000, "/api/workflows?state.order_id=123")
		if resp.StatusCode != http.StatusOK || ws.calls != 0 || !ws.listCalled {
			t.Errorf("status=%d windowCalls=%d listCalled=%v; want the ordinary listing and no window search", resp.StatusCode, ws.calls, ws.listCalled)
		}
		if h := resp.Header.Get(hdrQueryStateWindow); h != "" {
			t.Errorf("an exact answer carried a window header %q, which would read as a limit that does not exist", h)
		}
	})
	t.Run("no state parameter never reaches the window search, even on a sealing store", func(t *testing.T) {
		ws := newWindowStore(true, &engine.QueryStateWindowResult{}, nil)
		serveList(t, ws, 5000, "/api/workflows?status=ready")
		if ws.calls != 0 || !ws.listCalled {
			t.Errorf("windowCalls=%d listCalled=%v; an ordinary listing must not become a decrypting scan", ws.calls, ws.listCalled)
		}
	})
}

func TestAWindowOfZeroRefusesAndNamesTheFlagOnlyWhereTheFlagWouldHelp(t *testing.T) {
	t.Run("a store that could search, with the search off, says how to turn it on", func(t *testing.T) {
		ws := newWindowStore(true, &engine.QueryStateWindowResult{}, nil)
		resp := serveList(t, ws, 0, "/api/workflows?state.order_id=123")
		msg := errBody(t, resp)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(msg, "--query-state-search-window") || !strings.Contains(msg, "--encrypt-sensitive-payloads") {
			t.Errorf("status=%d msg=%q; want 422 naming both flags", resp.StatusCode, msg)
		}
		if ws.calls != 0 {
			t.Error("the search ran although it is switched off")
		}
	})
	t.Run("a store that cannot search keeps part 1's refusal and does not offer a flag that would not help", func(t *testing.T) {
		ms := &mockStore{}
		ms.countWorkflowsFn = func(context.Context, engine.WorkflowFilter) (int, error) {
			return 0, fmt.Errorf("shard %q: %w", "s1", engine.ErrQueryStateFilterUnavailableUnderEncryption)
		}
		resp := serveList(t, ms, 5000, "/api/workflows?state.order_id=123")
		msg := errBody(t, resp)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(msg, "--encrypt-sensitive-payloads") || strings.Contains(msg, "--query-state-search-window") {
			t.Errorf("status=%d msg=%q; want 422 naming only the sealing flag", resp.StatusCode, msg)
		}
	})
}

func TestAFailedWindowSearchIsMappedByWhatWentWrong(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"too many pairs is the caller's", engine.ErrQueryStateFilterTooManyPairs, http.StatusBadRequest},
		{"a payload substring search under sealing is refused, not a fault", engine.ErrSearchUnavailableUnderEncryption, http.StatusUnprocessableEntity},
		{"anything else is a 500", errors.New("connection reset"), http.StatusInternalServerError},
	} {
		resp := serveList(t, newWindowStore(true, nil, c.err), 5000, "/api/workflows?state.order_id=123")
		if resp.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestTheSearchWindowFlagIsBounded(t *testing.T) {
	for _, c := range []struct {
		n  int
		ok bool
	}{{0, true}, {1, true}, {defaultQueryStateSearchWindow, true}, {maxQueryStateSearchWindow, true}, {-1, false}, {maxQueryStateSearchWindow + 1, false}} {
		if err := validateQueryStateSearchWindow(c.n); (err == nil) != c.ok {
			t.Errorf("validateQueryStateSearchWindow(%d) = %v, want ok=%v", c.n, err, c.ok)
		}
	}
	if defaultQueryStateSearchWindow < 1 || defaultQueryStateSearchWindow > maxQueryStateSearchWindow {
		t.Errorf("the default %d is outside the bound %d", defaultQueryStateSearchWindow, maxQueryStateSearchWindow)
	}
}
