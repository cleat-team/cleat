//go:build ignore

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "fake-proxy-key-for-tests"

// upstreamRecorder is a stand-in for the cleat worker. It records what reached it, so the tests can say what the
// proxy forwarded and, as importantly, what it did not.
type upstreamRecorder struct {
	srv  *httptest.Server
	got  []*http.Request
	body []string
}

func newUpstream(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.got = append(u.got, r)
		u.body = append(u.body, string(b))
		respond(w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func testProxy(t *testing.T, up *upstreamRecorder, listen string) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	page := filepath.Join(dir, "index.html")
	if err := os.WriteFile(page, []byte("<h1>page</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('app')"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(up.srv.URL)
	return newHandler(config{
		upstream: u, workflow: "my-fullstack-app", apiKey: testKey, page: page,
		script: filepath.Join(filepath.Dir(page), "app.js"), listen: listen,
		client: &http.Client{Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}), page
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Add("Set-Cookie", "session=abc")
	w.Header().Set("X-Internal", "leak")
	_, _ = w.Write([]byte(`{"id":"run-1"}`))
}

func do(h http.Handler, method, target, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if host != "" {
		r.Host = host
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const home = "127.0.0.1:3000"

var jsonHdr = map[string]string{"Content-Type": "application/json", "Idempotency-Key": "k-1"}

// A run starts, the key is added on the way, and only the two allowed request headers are forwarded. The browser's
// Authorization, Cookie and anything else stop at the proxy.
func TestStartAddsTheKeyAndForwardsOnlyContentTypeAndIdempotencyKey(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)

	hdr := map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": "k-1",
		"Authorization": "Bearer the-browsers-own", "Cookie": "sid=1", "X-Forwarded-For": "6.6.6.6",
		"Origin": "http://" + home,
	}
	w := do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home, hdr, `{"input":{"item":"widget"}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "run-1") {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	if len(up.got) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(up.got))
	}
	r := up.got[0]
	if r.URL.Path != "/api/workflows/my-fullstack-app/start" || r.Method != http.MethodPost {
		t.Errorf("upstream got %s %s", r.Method, r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("upstream Authorization = %q, want the proxy's own key, not the browser's", got)
	}
	for _, h := range []string{"Cookie", "X-Forwarded-For", "Origin"} {
		if r.Header.Get(h) != "" {
			t.Errorf("upstream received the browser's %s header", h)
		}
	}
	if r.Header.Get("Idempotency-Key") != "k-1" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type and Idempotency-Key should be forwarded: %v", r.Header)
	}
	if up.body[0] != `{"input":{"item":"widget"}}` {
		t.Errorf("upstream body = %q", up.body[0])
	}
}

// The response carries the status, Content-Type and body. Set-Cookie and every other upstream header stop here, and
// the API key appears nowhere in what the browser gets.
func TestTheResponseCarriesNoUpstreamHeadersButContentTypeAndNeverTheKey(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)
	w := do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home, jsonHdr, `{}`)
	if w.Header().Get("Set-Cookie") != "" || w.Header().Get("X-Internal") != "" {
		t.Errorf("an upstream header reached the browser: %v", w.Header())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", w.Header().Get("Content-Type"))
	}
	for name, vals := range w.Header() {
		for _, v := range vals {
			if strings.Contains(v, testKey) {
				t.Errorf("the key is in response header %s", name)
			}
		}
	}
	if strings.Contains(w.Body.String(), testKey) {
		t.Error("the key is in the response body")
	}
}

// Published state is read through the proxy, for the one key the page uses.
// Whatever the worker answers is served as JSON or as plain text, never as HTML: markup reflected from upstream would
// run on the proxy's own origin, and a script there can call the proxy same-origin.
func TestAnUpstreamHTMLResponseIsServedAsPlainTextUnderASandbox(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<script>fetch('/api/workflows/my-fullstack-app/start',{method:'POST'})</script>"))
	})
	h, _ := testProxy(t, up, home)
	w := do(h, http.MethodGet, "/api/workflows/run-1/query?key=status", home, nil, "")
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain for an upstream text/html", ct)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || strings.Contains(csp, "script-src") {
		t.Errorf("Content-Security-Policy = %q, want default-src 'none'; sandbox", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
	// And JSON stays JSON, which is what the page reads.
	up2 := newUpstream(t, ok)
	h2, _ := testProxy(t, up2, home)
	if ct := do(h2, http.MethodGet, "/api/workflows/run-1/query?key=status", home, nil, "").Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestQueryForwardsOnlyKeyStatus(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"value":"complete"}`)) })
	h, _ := testProxy(t, up, home)

	w := do(h, http.MethodGet, "/api/workflows/run-1/query?key=status", home, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "complete") {
		t.Fatalf("query = %d %s", w.Code, w.Body)
	}
	if up.got[0].URL.RawQuery != "key=status" || up.got[0].Header.Get("Authorization") != "Bearer "+testKey {
		t.Errorf("upstream got %s auth=%q", up.got[0].URL.RawQuery, up.got[0].Header.Get("Authorization"))
	}
	for _, q := range []string{"key=other", "key=status&x=1", "", "key=status%26admin"} {
		before := len(up.got)
		w := do(h, http.MethodGet, "/api/workflows/run-1/query?"+q, home, nil, "")
		if w.Code != 404 || len(up.got) != before {
			t.Errorf("query %q = %d (upstream calls +%d), want 404 and nothing forwarded", q, w.Code, len(up.got)-before)
		}
	}
}

// It is an allowlist of routes AND of methods on those routes.
func TestAnythingButTheThreeRoutesIsRefusedAndNothingIsForwarded(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/admin/drain", 404},
		{"POST", "/api/admin/drain", 404},
		{"GET", "/api/workflows", 404},
		{"GET", "/api/workflows/", 404},
		{"POST", "/api/workflows/some-other-workflow/start", 404},
		{"GET", "/api/workflows/run-1", 404},
		{"GET", "/api/workflows/run-1/cancel", 404},
		{"GET", "/api/workflows/run-1/query/extra", 404},
		{"GET", "/api/workflows/../admin/drain", 404},
		{"GET", "/api/workflows/%2e%2e/query?key=status", 404},
		{"GET", "/api/workflows/run%2F1/query?key=status", 404},
		{"GET", "/healthz", 404},
		{"GET", "/index.html/", 404},
		// A listed path with an unlisted method.
		{"DELETE", "/api/workflows/run-1/query?key=status", 405},
		{"POST", "/api/workflows/run-1/query?key=status", 405},
		{"PUT", "/api/workflows/my-fullstack-app/start", 405},
		{"GET", "/api/workflows/my-fullstack-app/start", 405},
		{"DELETE", "/api/workflows/my-fullstack-app/start", 405},
		{"OPTIONS", "/api/workflows/my-fullstack-app/start", 405},
		{"POST", "/", 405},
	} {
		w := do(h, c.method, c.path, home, jsonHdr, `{}`)
		if w.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
	if len(up.got) != 0 {
		t.Errorf("%d refused requests were forwarded upstream", len(up.got))
	}
}

// A cross-origin page cannot use the key from a visitor's browser.
func TestCrossOriginAndRebindingRequestsAreRefusedBeforeAnythingIsForwarded(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)

	w := do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home,
		map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}, `{}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("a foreign Origin = %d, want 403", w.Code)
	}
	// text/plain is a "simple" request: no preflight, so the browser would just send it.
	w = do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home,
		map[string]string{"Content-Type": "text/plain"}, `{}`)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", w.Code)
	}
	w = do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", "rebind.evil.example:3000", jsonHdr, `{}`)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("a non-loopback Host while bound to loopback = %d, want 421", w.Code)
	}
	w = do(h, http.MethodGet, "/", "rebind.evil.example:3000", nil, "")
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("the page under a foreign Host = %d, want 421", w.Code)
	}
	if len(up.got) != 0 {
		t.Errorf("%d refused requests were forwarded upstream", len(up.got))
	}
	// The same names are fine.
	for _, host := range []string{"localhost:3000", "127.0.0.1:3000", "[::1]:3000"} {
		if w := do(h, http.MethodGet, "/", host, nil, ""); w.Code != 200 {
			t.Errorf("Host %s = %d, want 200", host, w.Code)
		}
	}
}

func TestThePageIsServedWithHardeningHeaders(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)
	w := do(h, http.MethodGet, "/", home, nil, "")
	if w.Code != 200 || w.Body.String() != "<h1>page</h1>" {
		t.Fatalf("page = %d %q", w.Code, w.Body)
	}
	for hdr, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
		if w.Header().Get(hdr) != want {
			t.Errorf("%s = %q, want %q", hdr, w.Header().Get(hdr), want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("Content-Security-Policy = %q, want scripts from 'self' only and no inline script", csp)
	}
	// The script is its own file, served with its own type, so the page needs no inline script.
	sw := do(h, http.MethodGet, "/app.js", home, nil, "")
	if sw.Code != 200 || sw.Body.String() != "console.log('app')" || !strings.HasPrefix(sw.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("/app.js = %d %q %q", sw.Code, sw.Body, sw.Header().Get("Content-Type"))
	}
	if c := do(h, http.MethodPost, "/app.js", home, nil, "").Code; c != 405 {
		t.Errorf("POST /app.js = %d, want 405", c)
	}
	if c := do(h, http.MethodGet, "/app.js/x", home, nil, "").Code; c != 404 {
		t.Errorf("GET /app.js/x = %d, want 404", c)
	}
}

// A redirect from the worker is handed back, not followed: following it would send the key to whatever host it names.
func TestARedirectFromTheWorkerIsNotFollowed(t *testing.T) {
	leak := newUpstream(t, ok)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leak.srv.URL+"/steal", http.StatusFound)
	})
	h, _ := testProxy(t, up, home)
	w := do(h, http.MethodGet, "/api/workflows/run-1/query?key=status", home, nil, "")
	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want the 302 handed back", w.Code)
	}
	if len(leak.got) != 0 {
		t.Errorf("the proxy followed a redirect and sent %d request(s) with its key to another host", len(leak.got))
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	up := newUpstream(t, ok)
	h, _ := testProxy(t, up, home)
	w := do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home, jsonHdr, strings.Repeat("x", maxRequestBody+10))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a %d byte body = %d, want 413", maxRequestBody+10, w.Code)
	}
	if len(up.got) != 0 {
		t.Errorf("an oversized body reached the worker (%d call(s)); it must be refused before any upstream call", len(up.got))
	}
	// A body exactly at the limit is fine.
	if w := do(h, http.MethodPost, "/api/workflows/my-fullstack-app/start", home, jsonHdr, strings.Repeat("x", maxRequestBody)); w.Code != 200 {
		t.Errorf("a body at the limit = %d, want 200", w.Code)
	}
}

// The proxy refuses to start without a key, and the file form wins.
func TestTheKeyComesFromTheFileOrTheEnvironmentAndItsAbsenceIsAnError(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if k, err := loadKey(env(map[string]string{"CLEAT_API_KEY": "from-env"}), os.ReadFile); err != nil || k != "from-env" {
		t.Errorf("env key = %q, %v", k, err)
	}
	if k, err := loadKey(env(map[string]string{"CLEAT_API_KEY_FILE": keyFile}), os.ReadFile); err != nil || k != "from-file" {
		t.Errorf("file key = %q, %v", k, err)
	}
	if k, _ := loadKey(env(map[string]string{"CLEAT_API_KEY": "from-env", "CLEAT_API_KEY_FILE": keyFile}), os.ReadFile); k != "from-file" {
		t.Errorf("with both set the file should win, got %q", k)
	}
	if _, err := loadKey(env(nil), os.ReadFile); err == nil || !strings.Contains(err.Error(), "CLEAT_API_KEY") {
		t.Errorf("no key at all: err = %v, want a message naming CLEAT_API_KEY", err)
	}
	if _, err := loadKey(env(map[string]string{"CLEAT_API_KEY_FILE": filepath.Join(dir, "missing")}), os.ReadFile); err == nil {
		t.Error("a missing key file was not an error")
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	if _, err := loadKey(env(map[string]string{"CLEAT_API_KEY_FILE": empty}), os.ReadFile); err == nil {
		t.Error("an empty key file was not an error")
	}
	if err := run([]string{"-listen", "127.0.0.1:0"}, env(nil), os.ReadFile); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Errorf("run without a key = %v, want it to refuse to start", err)
	}
}

// A non-loopback address needs an explicit flag, so a stray -listen 0.0.0.0 cannot hand the tenant to the network.
func TestANonLoopbackListenAddressNeedsAnExplicitFlag(t *testing.T) {
	env := func(k string) string {
		if k == "CLEAT_API_KEY" {
			return testKey
		}
		return ""
	}
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:0"} {
		// In a goroutine: if the refusal were missing, run would start listening and never return.
		done := make(chan error, 1)
		go func() { done <- run([]string{"-listen", addr}, env, os.ReadFile) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "-allow-remote") {
				t.Errorf("-listen %s = %v, want a refusal naming -allow-remote", addr, err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("-listen %s: run did not refuse; it is serving on a non-loopback address", addr)
		}
	}
}

func TestOnlyLoopbackBindsEnforceTheHostCheck(t *testing.T) {
	if allowedHosts("127.0.0.1:3000") == nil || allowedHosts("localhost:3000") == nil || allowedHosts("[::1]:3000") == nil {
		t.Error("a loopback bind must enforce the Host check")
	}
	if allowedHosts("0.0.0.0:3000") != nil || allowedHosts(":3000") != nil || allowedHosts("192.168.1.5:3000") != nil {
		t.Error("a non-loopback bind cannot know its names; the Host check is off and start-up warns")
	}
}
