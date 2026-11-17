package main

import (
	"context"
	"strings"
	"testing"
)

func writeFetchFixture(t *testing.T, body string) string {
	t.Helper()
	return writeFixture(t, t.TempDir(), "fetch.json", body)
}

// A scripted request is answered in the shape the guest decodes -- {status, headers, body} -- and is recorded. The
// shape matters: the SDK wrapper turns this text into a FetchResult, so an answer carrying a bare body would fail
// inside the guest rather than here.
func TestScriptedFetchAnswersAndRecordsTheRequest(t *testing.T) {
	path := writeFetchFixture(t, `{"GET https://example.test/a": {"status": 200, "body": "pong"}}`)
	spec, err := newScriptedFetch(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Option == nil {
		t.Fatal("the store contributed no engine option; the engine would refuse the call")
	}
	f := &scriptedFetcher{script: map[string]fetchScript{
		"GET https://example.test/a": {Status: 200, Body: "pong"},
	}}
	out, err := f.Fetch(context.Background(), "GET", "https://example.test/a", "{}", "")
	if err != nil {
		t.Fatalf("scripted fetch failed: %v", err)
	}
	for _, want := range []string{`"status":200`, `"body":"pong"`, `"headers":{}`} {
		if !strings.Contains(out, want) {
			t.Errorf("answer %s is missing %s", out, want)
		}
	}
	if got := f.requestList(); len(got) != 1 || got[0] != "GET https://example.test/a" {
		t.Fatalf("requests = %v, want the one this test made", got)
	}
}

// A scripted ERROR is what an egress refusal looks like from the engine's side: an error, never a status. A store that
// turned it into a 403 answer would make a refused request indistinguishable from a server that answered.
func TestAScriptedFetchErrorIsAnError(t *testing.T) {
	f := &scriptedFetcher{script: map[string]fetchScript{
		"POST https://blocked.test/x": {Error: "egress policy refuses https://blocked.test"},
	}}
	_, err := f.Fetch(context.Background(), "POST", "https://blocked.test/x", "{}", `{}`)
	if err == nil || !strings.Contains(err.Error(), "egress policy refuses") {
		t.Fatalf("err = %v, want the scripted refusal", err)
	}
}

// An UNSCRIPTED request must fail. Answering something plausible would let a workflow pass on a request the fixture
// never described -- the defect this store exists to make impossible.
func TestAnUnscriptedFetchFailsAndNamesTheScriptedOnes(t *testing.T) {
	f := &scriptedFetcher{script: map[string]fetchScript{
		"GET https://example.test/a": {Status: 200},
	}}
	_, err := f.Fetch(context.Background(), "GET", "https://example.test/other", "{}", "")
	if err == nil {
		t.Fatal("an unscripted request was answered")
	}
	if !strings.Contains(err.Error(), "https://example.test/a") {
		t.Errorf("err = %v, want it to name the scripted requests", err)
	}
}

// The fixture's own refusals, at load time: an ambiguous key or answer would otherwise be resolved silently.
func TestFetchScriptRefusesAmbiguity(t *testing.T) {
	for name, body := range map[string]string{
		"key without a method": `{"https://x": {"status": 200}}`,
		"status and error":     `{"GET https://x": {"status": 200, "error": "no"}}`,
		"neither":              `{"GET https://x": {}}`,
		"empty":                `{}`,
	} {
		if _, err := newScriptedFetch(writeFetchFixture(t, body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
