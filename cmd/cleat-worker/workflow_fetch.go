package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cleat-team/cleat/engine"
)

// defaultWorkflowFetchMaxBody is the --workflow-fetch-max-response-bytes
// default. It equals the guest's output buffer (ABI.md 2.48 documents 1048576),
// so a body the cap admits is one the guest has room for before JSON escaping.
const defaultWorkflowFetchMaxBody = 1 << 20

// workflowFetcher is the engine.Fetcher a stock worker installs when started
// with --enable-workflow-fetch. cleat#3377, IMPROVEMENT-PLAN.d 3.317 option 2.
//
// IT ADDS NO EGRESS POLICY OF ITS OWN. Fetch hands the request to
// dbServiceCaller.guardedFetch, the function behind DurableCall("http",
// "fetch"), so cleat_fetch is refused exactly where http.fetch is: the floor
// (loopback, link-local, RFC1918, never exemptible for a guest), the operator's
// --egress-allowlist, and the requesting tenant's own list, which is EMPTY by
// default and permits nothing. The flag therefore grants the capability to
// attempt a fetch; it does not grant any destination. A second spelling of the
// guard here would be the thing to distrust.
//
// What differs from http.fetch is three things, all because the call shape
// differs. The engine's replay-stable Idempotency-Key is not sent, since the
// Fetcher interface carries no step; ${secret:name} references are not resolved,
// since that substitution lives in dbServiceCaller.call and not in guardedFetch;
// and an oversized response is an error that names the cap rather than a silent
// cut.
type workflowFetcher struct {
	caller  *dbServiceCaller
	maxBody int64
}

// Fetch implements engine.Fetcher. The response is the same JSON as http.fetch:
// {"status":N,"headers":{...},"body":"..."}.
func (f *workflowFetcher) Fetch(ctx context.Context, method, url, headersJSON, body string) (string, error) {
	req := fetchRequest{URL: url, Method: method, Body: body}
	if headersJSON != "" {
		if err := json.Unmarshal([]byte(headersJSON), &req.Headers); err != nil {
			return "", engine.NewPermanentError("http.fetch", "", fmt.Errorf(
				"headers must be a JSON object of string values: %w", err))
		}
	}
	return f.caller.guardedFetch(ctx, req, "", f.maxBody, true)
}

// workflowFetcherFor returns the fetcher a worker should install, or nil when
// --enable-workflow-fetch is off. The nil is the whole of "off by default": the
// engine then takes its no-Fetcher branch, whose message names the flag.
func (w *Worker) workflowFetcherFor(caller *dbServiceCaller) engine.Fetcher {
	if !w.workflowFetch {
		return nil
	}
	max := w.workflowFetchMaxBody
	if max <= 0 {
		max = defaultWorkflowFetchMaxBody
	}
	return &workflowFetcher{caller: caller, maxBody: max}
}
