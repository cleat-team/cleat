// cleat#3790. docs/tutorials/signals-and-human-loop.md was compiled by
// tutorial_content_guard_test.go and run by nothing. Its Go block compiled, and
// a reader who followed it still hit, in order: a 401 on every curl (no
// Authorization header), `no workflow entry points found` (the entry point was
// the unexported `submitExpense` under a `//go:export` marker the scaffold does
// not use), `unmarshal input: unexpected end of JSON input` (a struct
// parameter binds by name, not by the input object), a 400 on the signal (the
// payload was a JSON object, and the API wants a string), and response bodies
// that no worker returns. A compile check could see none of it.
//
// TWO TESTS, split by what each needs.
//
// TestSignalsTutorialCurlsCarryTheKeyAndAStringPayload needs nothing and always
// runs: it reads the page's bash blocks. It has a known-positive, because a
// checker that cannot fail is the thing this repository keeps recording.
//
// TestSignalsTutorialExpenseIsApprovedAndRejected RUNS the page, as
// TestTutorialQuickStartReachesADoneWorkflow runs the quick start, and shares
// its substitutions (a sandbox Postgres, a kernel-assigned port, the worker as
// a background process). What it takes from the page rather than retypes: the
// Go program, the entry-point name, the init/build/deploy commands, and the
// end-to-end bash script, which it executes with only the base URL replaced.
// It compares what the worker answered with the `# {...}` lines the page prints
// under each command, so a response shape that drifts fails here.
//
// What it does not run: the first two "Sending signals" blocks on their own
// (the end-to-end script carries the same approval request; the reject block
// IS run, on a second run), and `GET /api/workflows/{id}` beyond the final
// status. The start response's shape is stated in prose under the script and
// is checked only to the extent that the script needs its `id`.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const signalsTutorial = "docs/tutorials/signals-and-human-loop.md"

var (
	stGoFenceRe    = regexp.MustCompile("(?s)```go\n(.*?)```")
	stEntryRe      = regexp.MustCompile(`// @cleatEntry\(name="([^"]+)"\)`)
	stDocResponse  = regexp.MustCompile(`(?m)^# (\{.*\})\s*$`)
	stPayloadKeyRe = regexp.MustCompile(`"payload"\s*:\s*(.)`)
	stCurlRe       = regexp.MustCompile(`\bcurl\b`)
)

// signalsTutorialProblems returns what is wrong with the page's curl commands.
// It is a function of the text so the test can run it on a known-broken copy.
func signalsTutorialProblems(bash string) []string {
	var problems []string
	joined := joinLineContinuations(bash)
	for _, line := range strings.Split(joined, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") || !stCurlRe.MatchString(trim) {
			continue
		}
		// A command inside $( ... ) starts mid-line; the check is per line of the
		// continuation-joined text, which is per command.
		if !strings.Contains(line, `Authorization: Bearer $CLEAT_API_KEY`) {
			problems = append(problems, "curl without the Authorization header: "+trim)
		}
	}
	for _, m := range stPayloadKeyRe.FindAllStringSubmatch(joined, -1) {
		if m[1] != `"` {
			problems = append(problems, `"payload" is not a string (first character `+m[1]+`)`)
		}
	}
	return problems
}

func TestSignalsTutorialCurlsCarryTheKeyAndAStringPayload(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, signalsTutorial))
	if err != nil {
		t.Fatal(err)
	}
	bash := strings.Join(allBashBlocks(t, string(data)), "\n")

	if got := len(stCurlRe.FindAllString(joinLineContinuations(bash), -1)); got < 7 {
		t.Fatalf("found %d curl commands in %s, want at least the 7 it documents -- the page or the extractor drifted", got, signalsTutorial)
	}
	if problems := signalsTutorialProblems(bash); len(problems) > 0 {
		t.Errorf("%s:\n  %s", signalsTutorial, strings.Join(problems, "\n  "))
	}

	// Known-positives: the page as it was published, in both ways it was wrong.
	noHeader := strings.Replace(bash, `-H "Authorization: Bearer $CLEAT_API_KEY"`, "", 1)
	if len(signalsTutorialProblems(noHeader)) == 0 {
		t.Error("the checker did not notice a curl with its Authorization header removed")
	}
	objectPayload := strings.Replace(bash, `"payload": "{`, `"payload": {`, 1)
	if len(signalsTutorialProblems(objectPayload)) == 0 {
		t.Error("the checker did not notice a payload written as a JSON object")
	}
}

// runDocScript runs a bash script taken from the page, with the page's base URL
// replaced by the test worker's, and returns its stdout.
func runDocScript(t *testing.T, script, base, apiKey, runID string) string {
	t.Helper()
	script = strings.ReplaceAll(script, "http://localhost:8080", base)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "CLEAT_API_KEY="+apiKey)
	if runID != "" {
		cmd.Env = append(cmd.Env, "RUN_ID="+runID)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("documented script failed: %v\nstdout:\n%s\nstderr:\n%s\nscript:\n%s", err, out.String(), errOut.String(), script)
	}
	return out.String()
}

func TestSignalsTutorialExpenseIsApprovedAndRejected(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	root := repoRoot(t)
	page := filepath.Join(root, signalsTutorial)
	pageText, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	qs := parseTutorialQuickStart(t, page)

	goBlocks := stGoFenceRe.FindAllStringSubmatch(string(pageText), -1)
	if len(goBlocks) == 0 {
		t.Fatalf("no ```go block in %s", signalsTutorial)
	}
	program := goBlocks[0][1]
	entry := stEntryRe.FindStringSubmatch(program)
	if entry == nil {
		t.Fatalf("the page's program has no // @cleatEntry(name=\"...\") marker")
	}

	var e2e, reject string
	for _, b := range splitBashBlocks(string(pageText)) {
		switch {
		case strings.Contains(b, "RUN_ID=$(curl"):
			e2e = b
		case strings.Contains(b, `\"approved\": false`):
			reject = b
		}
	}
	if e2e == "" || reject == "" {
		t.Fatalf("could not find the end-to-end script (%v) or the reject command (%v) on the page", e2e != "", reject != "")
	}

	workerBin := buildWorkerBinaryOnce(t)
	ownerDSN, _ := startSandboxPostgres(t)
	if out, err := exec.Command(workerBin, "--migrate-only", "--db", ownerDSN).CombinedOutput(); err != nil {
		t.Fatalf("cleat-worker --migrate-only: %v\n%s", err, out)
	}

	// The page's own steps: init, replace main.go, point cleat.yaml's
	// entry_points at the marker, build, deploy.
	parent := t.TempDir()
	if out, err := runCleatIn(t, parent, "init", qs.scaffoldName); err != nil {
		t.Fatalf("cleat init %s: %v\n%s", qs.scaffoldName, err, out)
	}
	proj := filepath.Join(parent, qs.scaffoldName)
	if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(proj, "cleat.yaml")
	yamlText, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := regexp.MustCompile(`(?m)^(\s*-\s*)hello\s*$`).ReplaceAllString(string(yamlText), "${1}"+entry[1])
	if patched == string(yamlText) {
		t.Fatalf("the scaffold's cleat.yaml no longer lists `hello`; the page's manual step needs rewording:\n%s", yamlText)
	}
	if err := os.WriteFile(yamlPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	buildOut := filepath.Join(proj, qs.buildOutDir)
	if out, err := runCleatIn(t, proj, "build", "-o", buildOut, "."); err != nil {
		t.Fatalf("cleat build -o %s .: %v\n%s", qs.buildOutDir, err, out)
	}
	if out, err := runCleatIn(t, root, "deploy", "--db", ownerDSN, "--name", qs.deployName,
		filepath.Join(buildOut, qs.wasmBasename)); err != nil {
		t.Fatalf("cleat deploy --name %s: %v\n%s", qs.deployName, err, out)
	}

	odb, err := sql.Open("postgres", ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer odb.Close()
	if _, err := odb.Exec(`ALTER ROLE cleat_app LOGIN PASSWORD 'cleat_app'`); err != nil {
		t.Fatalf("ALTER ROLE cleat_app: %v", err)
	}
	appDSN := strings.Replace(ownerDSN, "cleat:cleat@", "cleat_app:cleat_app@", 1)
	if appDSN == ownerDSN {
		t.Fatalf("could not derive the app DSN from %q", ownerDSN)
	}
	base, worker := startWorkerWithDiscoveredPort(t, workerBin, appDSN)
	t.Cleanup(func() {
		if worker.Process != nil {
			worker.Process.Kill()
			worker.Wait()
		}
	})
	waitForHealthz(t, base+"/healthz")

	keyOut, err := exec.Command(workerBin, "--db", appDSN,
		"--generate-api-key", "00000000-0000-0000-0000-000000000000").CombinedOutput()
	if err != nil {
		t.Fatalf("--generate-api-key: %v\n%s", err, keyOut)
	}
	km := regexp.MustCompile(`Key:\s+(cleat_sk_[0-9a-f]+)`).FindStringSubmatch(string(keyOut))
	if km == nil {
		t.Fatalf("--generate-api-key printed no key:\n%s", keyOut)
	}
	apiKey := km[1]

	// --- the approval path: the page's whole script, then its printed responses.
	out := runDocScript(t, e2e, base, apiKey, "")
	runLine := regexp.MustCompile(`(?m)^run: (\S+)$`).FindStringSubmatch(out)
	if runLine == nil {
		t.Fatalf("the script printed no `run: <id>` line:\n%s", out)
	}
	approvedRun := runLine[1]

	var want []map[string]any
	for _, m := range stDocResponse.FindAllStringSubmatch(e2e, -1) {
		var doc map[string]any
		if err := json.Unmarshal([]byte(m[1]), &doc); err != nil {
			t.Fatalf("a `# {...}` line on the page is not JSON: %s: %v", m[1], err)
		}
		want = append(want, doc)
	}
	rest := strings.TrimSpace(strings.Replace(out, runLine[0], "", 1))
	d := json.NewDecoder(strings.NewReader(rest))
	var got []map[string]any
	for d.More() {
		var o map[string]any
		if err := d.Decode(&o); err != nil {
			t.Fatalf("the script's output after the run id is not a stream of JSON objects: %v\n%s", err, rest)
		}
		got = append(got, o)
	}
	if len(want) == 0 {
		t.Fatalf("the script on the page documents no responses (`# {...}` lines)")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the worker's answers differ from the responses the page prints.\npage:   %v\nworker: %v", want, got)
	}

	if st := pollWorkflowStatus(t, base+"/api/workflows/"+approvedRun, apiKey); st.Status != "done" {
		t.Errorf("approved run %s ended %q (error %q), want done", approvedRun, st.Status, st.Error)
	}

	// --- the rejection path: the script up to the point it has a run waiting,
	// then the page's reject command.
	cut := strings.Index(e2e, "# 2.")
	if cut < 0 {
		t.Fatalf("the script has no `# 2.` step to cut at")
	}
	prefix := runDocScript(t, e2e[:cut], base, apiKey, "")
	rm := regexp.MustCompile(`(?m)^run: (\S+)$`).FindStringSubmatch(prefix)
	if rm == nil {
		t.Fatalf("the script's first step printed no run id:\n%s", prefix)
	}
	rejectedRun := rm[1]
	runDocScript(t, reject, base, apiKey, rejectedRun)

	var value string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
			base+"/api/workflows/"+rejectedRun+"/query?key=status", nil)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var q struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(b, &q)
			value = q.Value
		}
		if value == "rejected" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if value != "rejected" {
		t.Errorf("rejected run %s: status = %q, want rejected", rejectedRun, value)
	}
}

// splitBashBlocks returns each ```bash block as its own text (allBashBlocks
// returns the lines of all of them, joined).
func splitBashBlocks(markdown string) []string {
	var blocks []string
	var cur []string
	in := false
	for _, line := range strings.Split(markdown, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case !in && tqsFenceOpenRe.MatchString(trim):
			in, cur = true, nil
		case in && tqsFenceCloseRe.MatchString(trim):
			in = false
			blocks = append(blocks, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, line)
		}
	}
	return blocks
}
