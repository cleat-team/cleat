package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cleat#3659, owner decision D61 (option 2). When the scaffold's `go mod tidy` fails and the cause is a
// network or stream error that a re-run usually clears, init's closing line says so. The contract of
// cleat#3533 (D45) is unchanged and is what the other test in this package pins: exit 0, the project is
// created, and the LAST line is what is wrong and the command that fixes it. This adds a clause to that
// line for one class of cause, and prints the line it always printed for every other.
//
// THE POSITIVE FIXTURE IS A REAL LOG. It is copied from run 40907847227 (develop push, 2026-10-29,
// job "Tier 1 Gate -- rest shard 4/4"), the one confirmed instance, and not written to match the matcher: a
// fixture composed to satisfy the code under test proves the code agrees with its author.
const realTransientTidyLog = `go.yaml.in/yaml/v3@v3.0.5: verifying module: go.yaml.in/yaml/v3@v3.0.5: reading https://sum.golang.org/tile/8/0/x227/042: stream error: stream ID 129; INTERNAL_ERROR; received from peer`

func TestTransientTidyCauseRecognisesTheRealLogAndNothingBroader(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      bool
	}{
		{"the confirmed instance, verbatim", realTransientTidyLog, true},
		{"the same inside the output init prints around it", "go: finding module for package x\ngo: probe imports\n\t" + realTransientTidyLog + "\n", true},
		{"a fetch that timed out", `Get "https://proxy.golang.org/x/@v/list": dial tcp 142.250.0.1:443: i/o timeout`, true},
		{"a TLS handshake that timed out", `Get "https://proxy.golang.org/x/@v/list": net/http: TLS handshake timeout`, true},
		{"a reset connection", `reading https://proxy.golang.org/x/@v/v1.info: read tcp 10.0.0.2:5555->142.250.0.1:443: read: connection reset by peer`, true},
		{"a 5xx from the proxy", `reading https://proxy.golang.org/x/@v/list: 503 Service Unavailable`, true},
		{"upper case is matched", `READING HTTPS://PROXY.GOLANG.ORG/X/@V/LIST: 502 BAD GATEWAY`, true},

		// What it must NOT match. Each of these is something a re-run does not clear.
		{"empty output", "", false},
		{"a proxy that is down: connection refused", `Get "http://127.0.0.1:1/x/@v/list": dial tcp 127.0.0.1:1: connect: connection refused`, false},
		{"a disabled proxy", "cannot find module providing package x: module lookup disabled by GOPROXY=off", false},
		{"a module that does not exist", `reading https://proxy.golang.org/nope/@v/list: 404 Not Found`, false},
		{"a go.mod that does not parse: an EOF with no fetch on the line", "go.mod:3: unexpected EOF", false},

		// The same-line rule, which is the reason the match is per line: a URL on one line and a transient
		// phrase on another is two unrelated lines, and cleat#3644's matcher was fooled by exactly that
		// shape (a compile error's "unexpected EOF"). This case is synthetic by necessity -- it states the
		// rule -- and is labelled so; the real fixtures are the ones above and the end-to-end test below.
		{"a URL on one line and a phrase on another", "downloading https://proxy.golang.org/x\nmain.go:9: unexpected EOF\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientTidyCause(tc.out); got != tc.want {
				t.Errorf("transientTidyCause = %v, want %v for:\n%s", got, tc.want, tc.out)
			}
		})
	}
}

// The closing line is the line init ends with, through the real toolchain. Each case makes `go mod tidy`
// fail in a way the Go command itself reports, with no network: a local proxy that answers 503 (the one
// transient shape that can be produced here, and a real Go error line), a disabled proxy, and a refused
// connection. The 503 case is the known positive for the other two: if the closing line never carried the
// clause, "does not say transient" would hold for the wrong reason.
func TestInitSaysATransientTidyFailureIsTransientAndOnlyThen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	const clause = "looks like a transient network error"
	for _, tc := range []struct {
		name, goproxy string
		transient     bool
	}{
		{"a proxy answering 503", srv.URL, true},
		{"a disabled proxy", "off", false},
		{"a refused connection", "http://127.0.0.1:1", false},
	} {
		for _, template := range []string{"basic", "fullstack"} {
			t.Run(tc.name+"/"+template, func(t *testing.T) {
				t.Setenv("GOPROXY", tc.goproxy)
				t.Setenv("GOSUMDB", "off")
				t.Chdir(t.TempDir())
				name := "p_" + template
				out := captureInitOutput(t, "--template", template, name)

				if !strings.Contains(out, "could not resolve dependencies") {
					t.Fatalf("GOPROXY=%s did not make the scaffold's tidy fail, so this measured nothing:\n%s", tc.goproxy, out)
				}
				lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
				last := lines[len(lines)-1]

				// D45 is unchanged in both cases: the same two ends, one line, the project exists.
				if !strings.HasPrefix(last, "WARNING: "+name+" was created but will not build yet") ||
					!strings.HasSuffix(last, "Fix it with: cd "+name+" && go mod tidy") {
					t.Errorf("the last line is not the D45 line.\nlast: %q\nfull output:\n%s", last, out)
				}
				if n := strings.Count(out, "WARNING: "+name); n != 1 {
					t.Errorf("the closing line appears %d times, want 1:\n%s", n, out)
				}
				if _, err := os.Stat(filepath.Join(name, "go.mod")); err != nil {
					t.Errorf("the user must still get the project, but there is no go.mod: %v", err)
				}

				if got := strings.Contains(last, clause); got != tc.transient {
					t.Errorf("closing line mentions a transient error = %v, want %v.\nlast: %q\nfull output:\n%s", got, tc.transient, last, out)
				}
			})
		}
	}
}
