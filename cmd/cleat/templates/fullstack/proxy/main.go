//go:build ignore

// Command proxy serves web/index.html and forwards the two calls that page makes to the cleat worker, adding the
// tenant API key on the way. cleat#2307.
//
// WHY IT EXISTS. A browser must never hold a tenant API key: the key identifies the whole tenant. The worker also sends
// no CORS headers, so a page served from anywhere but the worker's own origin cannot call it at all. Both problems have
// the same answer, which is what the page's comment used to tell you to build yourself: serve the page from your own
// backend and let that backend call cleat. This is that backend, as small as it can be, on the standard library only.
//
// WHAT IT WILL DO. Exactly these, and nothing else:
//
//	GET  /                                    the page (web/index.html)
//	POST /api/workflows/<workflow>/start      start a run          (JSON body, Content-Type: application/json)
//	GET  /api/workflows/<id>/query?key=status the run's published state
//
// Every other path is 404 and every other method on those paths is 405. It is a fixed allowlist, not a forwarder: the
// upstream URL is built here from validated pieces, never from the request's path, and a request cannot name another
// route, another query key, another workflow, or the worker's admin API.
//
// WHAT IT KEEPS FROM THE BROWSER. It forwards Content-Type and Idempotency-Key and nothing else. The browser's
// Authorization, Cookie and every other header stop here; the key is added from the environment. Upstream Set-Cookie and
// every response header other than Content-Type are dropped. Redirects are not followed, so the key cannot be sent to a
// host the worker names. The key is never logged, and neither is a request body.
//
// WHO CAN CALL IT. It listens on 127.0.0.1 by default. Anything that can reach this port acts as your tenant, so put it
// somewhere else only knowing that. A page on another origin cannot use it from your browser either: a POST needs
// Content-Type: application/json (which forces a preflight the proxy does not answer), an Origin that is not this
// proxy's own is refused, and, while it is bound to loopback, so is a Host header that is not a loopback name (which is
// what a DNS-rebinding page sends).
//
// THE KEY comes from CLEAT_API_KEY_FILE (a file holding the key, the form a real deployment mounts) or CLEAT_API_KEY;
// the file wins if both are set. With neither, it refuses to start.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	maxRequestBody  = 64 << 10 // a run's input; the worker enforces its own limit as well
	maxResponseBody = 1 << 20
)

// idPattern is what a run id, and a workflow name, may look like. Anything else never reaches the worker.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type config struct {
	upstream *url.URL
	workflow string
	apiKey   string
	page     string
	listen   string
	client   *http.Client
}

// loadKey reads the API key: CLEAT_API_KEY_FILE wins over CLEAT_API_KEY, and neither is an error rather than a proxy
// that forwards unauthenticated calls and answers 401 to all of them.
func loadKey(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if path := getenv("CLEAT_API_KEY_FILE"); path != "" {
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("CLEAT_API_KEY_FILE: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("CLEAT_API_KEY_FILE %s is empty", path)
		}
		return key, nil
	}
	if key := strings.TrimSpace(getenv("CLEAT_API_KEY")); key != "" {
		return key, nil
	}
	return "", errors.New("no API key: set CLEAT_API_KEY (or CLEAT_API_KEY_FILE to a file holding it). " +
		"The worker prints one on first start; see README.md")
}

// isLoopback reports whether host (no port) is a loopback name or address.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// allowedHosts is the set of Host header values a request may carry while the proxy is bound to loopback, or nil when
// it is not (a non-loopback bind cannot know the names it is reached by, and says so at start-up).
func allowedHosts(listen string) map[string]bool {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || !isLoopback(host) {
		return nil
	}
	return map[string]bool{
		"localhost:" + port: true, "127.0.0.1:" + port: true, "[::1]:" + port: true,
		net.JoinHostPort(host, port): true,
	}
}

func newHandler(cfg config) http.Handler {
	hosts := allowedHosts(cfg.listen)
	// A plain handler, not a ServeMux: the mux cleans the path and redirects, and this proxy would rather answer
	// what it was asked (a 404) than help a request find a route it did not name.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hosts != nil && !hosts[r.Host] {
			http.Error(w, "unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		setSecurityHeaders(w)
		route, id, ok := matchRoute(r.URL.Path, cfg.workflow)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if !methodAllowed(route, r.Method) {
			w.Header().Set("Allow", allowFor(route))
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch route {
		case routePage:
			servePage(w, cfg.page)
			return
		case routeQuery:
			if r.URL.RawQuery != "key=status" {
				http.NotFound(w, r)
				return
			}
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		if route == routeStart {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
			if k := r.Header.Get("Idempotency-Key"); len(k) > 128 || strings.ContainsAny(k, "\r\n") {
				http.Error(w, "bad Idempotency-Key", http.StatusBadRequest)
				return
			}
		}
		forward(w, r, cfg, route, id)
	})
}

type route int

const (
	routeNone route = iota
	routePage
	routeStart
	routeQuery
)

// matchRoute classifies a request path. The workflow name and run id are returned only if they match idPattern.
func matchRoute(p, workflow string) (route, string, bool) {
	if p == "/" || p == "/index.html" {
		return routePage, "", true
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "workflows" && idPattern.MatchString(parts[2]) {
		switch {
		case parts[3] == "start" && parts[2] == workflow:
			return routeStart, parts[2], true
		case parts[3] == "query":
			return routeQuery, parts[2], true
		}
	}
	return routeNone, "", false
}

func methodAllowed(rt route, m string) bool {
	switch rt {
	case routePage:
		return m == http.MethodGet || m == http.MethodHead
	case routeQuery:
		return m == http.MethodGet
	case routeStart:
		return m == http.MethodPost
	}
	return false
}

func allowFor(rt route) string {
	if rt == routeStart {
		return http.MethodPost
	}
	return http.MethodGet
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
}

func servePage(w http.ResponseWriter, page string) {
	b, err := os.ReadFile(page)
	if err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func forward(w http.ResponseWriter, r *http.Request, cfg config, rt route, id string) {
	target := *cfg.upstream
	var body io.Reader
	switch rt {
	case routeStart:
		target.Path = "/api/workflows/" + id + "/start"
		body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	case routeQuery:
		target.Path = "/api/workflows/" + id + "/query"
		target.RawQuery = "key=status"
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The allowlist of request headers: these two, plus the key. Nothing the browser sent is copied wholesale.
	if ct := r.Header.Get("Content-Type"); rt == routeStart && ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if k := r.Header.Get("Idempotency-Key"); rt == routeStart && k != "" {
		req.Header.Set("Idempotency-Key", k)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)

	resp, err := cfg.client.Do(req)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("%s %s -> upstream unreachable", r.Method, r.URL.Path)
		http.Error(w, "the cleat worker is not reachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		log.Printf("upstream answered %d: the API key this proxy holds was refused", resp.StatusCode)
	}
	// Status, a Content-Type and the body. Not Set-Cookie, not anything else. The Content-Type is narrowed to two
	// values: a worker (or anything answering as it) that returned text/html would otherwise have its markup run on
	// the proxy's own origin, from which a script could call this proxy same-origin with the key attached.
	ct := "text/plain; charset=utf-8"
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mt == "application/json" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxResponseBody))
	log.Printf("%s %s -> %d", r.Method, r.URL.Path, resp.StatusCode)
}

func run(args []string, getenv func(string) string, readFile func(string) ([]byte, error)) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:3000", "address to listen on; anything that can reach it acts as your tenant")
	upstream := fs.String("upstream", "http://localhost:8080", "the cleat worker's API address")
	workflow := fs.String("workflow", "my-fullstack-app", "the one workflow name the page may start")
	page := fs.String("page", "web/index.html", "the page to serve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key, err := loadKey(getenv, readFile)
	if err != nil {
		return err
	}
	u, err := url.Parse(*upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-upstream %q is not an http(s) URL", *upstream)
	}
	if !idPattern.MatchString(*workflow) {
		return fmt.Errorf("-workflow %q is not a valid workflow name", *workflow)
	}
	cfg := config{
		upstream: u, workflow: *workflow, apiKey: key, page: *page, listen: *listen,
		client: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if allowedHosts(*listen) == nil {
		log.Printf("WARNING: listening on %s, which is not a loopback address: anyone who can reach it acts as your tenant "+
			"and the Host check is off", *listen)
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           newHandler(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Printf("open http://%s/  (forwarding to %s)", *listen, u.Host)
	return srv.ListenAndServe()
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.ReadFile); err != nil {
		fmt.Fprintln(os.Stderr, "proxy:", err)
		os.Exit(1)
	}
}
