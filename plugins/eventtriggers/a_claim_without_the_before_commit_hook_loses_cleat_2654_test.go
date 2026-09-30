package eventtriggers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
)

// TestClaimOrRegisterAwaiterSkippingBeforeCommitLosesCleat2654IsRealFinding
// exists because ClaimOrRegisterAwaiter's beforeCommit parameter is
// OPTIONAL (nil is accepted) -- so nothing in the signature stops a future
// caller from doing what awaitEvent used to do before cleat#2654: build and
// marshal its own response AFTER the call returns, rather than inside the
// hook. cleat-review's finding on this PR: that omission is correct by
// construction today (both current callers use it) and only by cooperation
// tomorrow, and needs a test that can fail rather than relying on every
// future caller remembering.
//
// This proves the hazard is real, not hypothetical: calling
// ClaimOrRegisterAwaiter with beforeCommit=nil against a row whose
// event_data cannot round-trip through JSON -- the exact cleat#2654
// precondition -- commits the claim anyway. The caller's own marshal,
// attempted (correctly, as a caller SHOULD) after the call returns, then
// fails against an event that is now durably consumed and gone forever --
// which is precisely the bug cleat#2654 fixed, reopened by construction the
// moment a caller skips the hook.
//
// This is not "prove ClaimOrRegisterAwaiter is broken" -- it isn't; nil is a
// documented, legitimate choice for a caller with nothing that can fail
// after the claim. It is "prove what a caller loses by choosing it here",
// so the choice is visible in a running test rather than only in a comment.
//
// MSSQL-only, matching TestAwaitEventMarshalFailureLeavesEventUnconsumed's
// own reasoning exactly: Postgres and MySQL validate JSON syntax at INSERT
// and refuse this test's malformed literal before ClaimOrRegisterAwaiter
// ever sees it; MSSQL's event_data is NVARCHAR(MAX), a plain text column
// with no such check.
func TestClaimOrRegisterAwaiterSkippingBeforeCommitLosesCleat2654IsRealFinding(t *testing.T) {
	db := testutil.TestDB(t, testutil.DialectMSSQL)
	dialect := plugin.DialectMSSQL
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	p := &Plugin{dialect: dialect, logger: quiet}
	if err := plugin.RunMigrations(context.Background(), db, dialect, nil,
		[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	p.db = &engine.SQLDBAdapter{DB: db, Dialect: dialect}

	tenantID := uuid.New()
	eventID := uuid.New()
	seedCtx := plugin.ForTenant(context.Background(), tenantID)

	mustInsertIngestedEventWithData(t, seedCtx, p, eventID, tenantID,
		"order.corrupt", "not valid json{", time.Now().Add(-time.Hour))

	// beforeCommit: nil -- the exact choice this test exists to show the
	// cost of. A caller doing this must do ITS OWN post-return marshal, as
	// simulated below.
	claimed, err := ClaimOrRegisterAwaiter(seedCtx, p.db, dialect, quiet,
		tenantID.String(), "wf-skips-the-hook", "order.corrupt", nil, nil)
	if err != nil {
		t.Fatalf("UNMEASURED: ClaimOrRegisterAwaiter itself failed (%v) -- this run says "+
			"nothing about the hook, since the claim never reached a committed state to "+
			"lose anything from", err)
	}
	if claimed == nil {
		t.Fatalf("UNMEASURED: no event was claimed -- this run says nothing about the hook")
	}

	// The claim already committed (ClaimOrRegisterAwaiter returned nil error
	// and a non-nil claimed with no hook to have stopped it). A caller now
	// does what awaitEvent used to do pre-cleat#2654: marshal AFTER the
	// call returns.
	type output struct {
		EventData json.RawMessage `json:"event_data"`
	}
	_, marshalErr := json.Marshal(output{EventData: json.RawMessage(claimed.EventData)})
	if marshalErr == nil {
		t.Fatalf("UNMEASURED: the simulated post-return marshal succeeded -- this test's " +
			"precondition (corrupted, non-JSON event_data) did not hold, so it proves nothing")
	}

	// THE FINDING: despite that marshal failure, the row is already
	// consumed -- gone, unrecoverable, exactly cleat#2654's bug, reopened
	// by the beforeCommit=nil choice.
	var processed bool
	row := p.db.QueryRow(seedCtx,
		`SELECT processed FROM ingested_events WHERE id = $1`, eventID)
	if err := plugin.ScanRow(row, &processed); err != nil {
		t.Fatalf("query processed flag: %v", err)
	}
	if !processed {
		t.Fatalf("event was NOT marked processed -- ClaimOrRegisterAwaiter's contract changed " +
			"(beforeCommit=nil no longer commits unconditionally), which means this test's " +
			"premise is stale and needs re-deriving, not that the hazard is gone")
	}
	t.Logf("confirmed: beforeCommit=nil committed event %s despite its event_data being "+
		"unmarshalable (%v) -- the hook is where cleat#2654's protection lives; skipping it "+
		"loses that protection silently", eventID, marshalErr)

	if !strings.Contains(marshalErr.Error(), "invalid character") {
		t.Logf("note: marshal failed for a different reason than expected (%v) -- still a "+
			"real failure, but re-check this test's precondition if this persists", marshalErr)
	}
}
