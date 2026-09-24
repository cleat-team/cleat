package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
)

// inertDB satisfies plugin.PluginDB and does nothing else: any call panics. The middleware these plugins
// install must not touch the database to pass a GET through, and a panic here says which one does.
type inertDB struct{ plugin.PluginDB }

// GET /api/workflows/{id}/stream THROUGH THE REAL PLUGIN MIDDLEWARE CHAIN. cleat#2254.
//
// Every plugin middleware wraps the CORE mux (cleat#1569), so each http.ResponseWriter wrapper a plugin
// installs sits in front of every core handler. audit-log's wrapper embedded http.ResponseWriter and had
// no Flush, so on every default build this route answered 500 "streaming not supported by this server".
// It shipped because every stream test drives handleStreamWorkflow directly with a recorder that HAS a
// Flush, so nothing before it ever met a wrapper. This one serves a real HTTP request through
// wrapPluginMiddleware over the registered plugins that have middleware, on a real server.
func TestAStreamServedThroughThePluginChainIsNotRefused(t *testing.T) {
	loaded, err := plugin.Discover()
	if err != nil {
		t.Fatal(err)
	}
	// Init the middleware plugins directly, with a database handle, rather than through plugin.InitAll:
	// InitAll withholds the handle from a plugin that declares no database access, and tenant-quota is one
	// (it refuses to start without one), which would drop the second wrapper this test exists for.
	env := &plugin.Environment{
		DB:      inertDB{},
		Dialect: plugin.DialectPostgres,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, lp := range loaded {
		if _, ok := lp.Plugin.(plugin.HasMiddleware); ok {
			if err := lp.Plugin.Init(context.Background(), env); err != nil {
				lp.Healthy, lp.Error = false, err
			}
		}
	}

	// The plugins with middleware that came up. A chain that silently lacks audit-log tests nothing.
	inChain := map[string]bool{}
	for _, lp := range loaded {
		if _, ok := lp.Plugin.(plugin.HasMiddleware); ok && lp.Healthy {
			inChain[lp.Plugin.Info().Name] = true
		}
	}
	for _, want := range []string{"audit-log", "tenant-quota"} {
		if !inChain[want] {
			t.Fatalf("%s is not in the middleware chain (chain: %v), so this test would not exercise its response writer", want, inChain)
		}
	}

	wf := &engine.WorkflowInstance{ID: "wf-1", Status: "done", Generation: 3}
	f := newStreamFixture(t, wf, []engine.EventRecord{
		chunkRec(10, 0, "all ", false),
		chunkRec(11, 1, "done", true),
	})
	mux := http.NewServeMux()
	registerRoutes(mux, f.api)
	srv := httptest.NewServer(wrapPluginMiddleware(mux, loaded))
	defer srv.Close()

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(srv.URL + "/api/workflows/wf-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stream through the plugin chain (%v) = %d %s\nwant 200: a wrapper in the chain hides Flush", inChain, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	evs := parseSSE(string(body))
	if len(chunkEvents(evs)) != 2 {
		t.Errorf("got %d chunk events, want 2:\n%s", len(chunkEvents(evs)), body)
	}
	if _, ok := findEvent(evs, "end"); !ok {
		t.Errorf("no end event:\n%s", body)
	}
}
