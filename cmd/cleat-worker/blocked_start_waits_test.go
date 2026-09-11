package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
)

// A start blocked by a concurrency key is accepted and waits. It is not refused.
//
// cleat#1186's whole subject: "a limit throttles how many run at once, not
// whether a request is accepted". Before this, the second start under a held
// key got
//
//	409 {"error":"workflow already running with key K"}
//
// and its run was terminated -- so work was dropped rather than queued, and a
// holder that hung blocked every later start under that key permanently.
//
// # Why this is a database-backed test and not a mock
//
// The deferral is enforced by the claim path, not by the handler: the handler's
// job is now to NOT refuse, and the claim's job is to not pick the run up while
// the key is held. A mock store proves neither -- and the existing mock-based
// tests in concurrency_conflict_test.go deliberately still assert the 409,
// because a store that cannot record the key keeps the old behaviour. Both are
// correct, and only a real store distinguishes them.
func TestABlockedStartIsAcceptedAndWaits(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed blocked-start test")
	}
	db := testutil.SuiteTestDB(t, "cleat_worker")
	ctx := context.Background()

	store := engine.NewPostgresStore(db)
	const defName = "blocked-start-def"
	if err := store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
		Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}

	api := &apiServer{
		store:       store,
		worker:      newTestWorker(&mockStore{}),
		maxBodySize: 1 << 20,
		factory:     &fakeStoreFactory{fallback: store},
	}

	// Unique per run, and this is not incidental. SuiteTestDB shares one
	// database across the whole package and across repeated runs, so a fixed
	// key inherits any concurrency_keys row an earlier execution left behind --
	// and a leftover holder makes BOTH starts wait, which reads as "0 of 2
	// claimed" and looks like the fix is broken. It did exactly that once.
	key := fmt.Sprintf("only-one-at-a-time-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM concurrency_keys WHERE key_text = $1`, key)
	})
	start := func() (int, map[string]string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/workflows/"+defName+"/start", strings.NewReader(`{"input":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cleat-Concurrency-Key", key)
		resp := httptest.NewRecorder()
		api.handleStartWorkflow(resp, req, defName)
		var body map[string]string
		_ = json.Unmarshal(resp.Body.Bytes(), &body)
		return resp.Code, body
	}

	code1, body1 := start()
	if code1 != 201 {
		t.Fatalf("first start = %d, want 201 (body %v)", code1, body1)
	}

	// The second start is the one that used to be refused.
	code2, body2 := start()
	if code2 == 409 {
		t.Fatalf("second start under a held key answered 409.\n\n"+
			"This is cleat#1186: the request is refused and its work is dropped, when what "+
			"the caller asked for was for it to run later. body=%v", body2)
	}
	if code2 != 201 {
		t.Fatalf("second start = %d, want 201 (body %v)", code2, body2)
	}
	if body2["id"] == "" || body2["id"] == body1["id"] {
		t.Fatalf("second start returned id %q (first was %q); it must be a NEW run that waits, "+
			"not a reference to the first", body2["id"], body1["id"])
	}

	// And it really is waiting rather than running: claiming takes one of them,
	// never both. This is the assertion that stops "accepted" from silently
	// meaning "accepted and run concurrently", which would be worse than the
	// 409 it replaced.
	claimed, err := store.ClaimWorkflows(ctx, "worker-blocked-start", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	got := 0
	for _, wf := range claimed {
		if wf.ID == body1["id"] || wf.ID == body2["id"] {
			got++
		}
	}
	if got != 1 {
		t.Errorf("%d of the two runs sharing key %q were claimed, want exactly 1.\n\n"+
			"2 means the key is not excluding anything and accepting the second start made "+
			"things worse than refusing it. 0 means neither can run at all.", got, key)
	}
}
