package eventtriggers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

// TestPublishEventRejectsInvalidJSON is cleat#2666's fix, part 1: PublishEvent
// must refuse a malformed eventData BEFORE the INSERT, on every dialect, not
// rely on the column type to catch it.
//
// WHY THIS MATTERS EVEN THOUGH NO PRODUCTION CALLER SENDS BAD JSON TODAY.
// handlePublishEvent decodes json.RawMessage from a request body, and
// kafkaconnect/webhookingest both pass json.Marshal output -- so every
// current caller is already valid by construction. But PostgreSQL's JSONB
// and MySQL's JSON column type reject invalid JSON at INSERT regardless;
// ingested_events.event_data on SQL Server is plain NVARCHAR(MAX) with no
// equivalent (migrations.go has never added an ISJSON check there, unlike
// event_subscriptions.input_template's JSON_VALID CHECK). Without a Go-level
// check, the safety this function appears to have is actually the column
// type's, and it is uneven across dialects -- a gap this test closes by
// making PublishEvent enforce it itself, so nothing downstream depends on
// which dialect happens to be configured.
//
// AND WHY A POISON ROW IS WORSE THAN AN ORDINARY BAD REQUEST. cleat#2665
// moved the marshal that reads a stored row's event_data to BEFORE
// tx.Commit(), so a row that fails to marshal now rolls back and stays
// unprocessed -- strictly better than the silent loss it replaced. But the
// claim query is `... AND NOT processed ORDER BY received_at`, so a poison
// row stays the OLDEST unprocessed row forever: every subsequent awaitEvent
// for that (tenant, event_type) re-claims it, fails the same way, rolls
// back, and errors -- and no newer event of that type is ever delivered.
// There is no exit short of manual SQL. Rejecting the row before it is ever
// written is what prevents that state from being reachable at all, which is
// strictly better than any remedy for a row that already exists.
//
// USES A PANICKING PluginDB, NOT A REAL DATABASE, on purpose. The whole
// claim is that PublishEvent refuses BEFORE touching storage -- so the
// regression this test exists to catch is exactly "the check moved after
// the INSERT, or was removed, and now reaches the database with an invalid
// value". A panicking fake proves that directly: if the fix is intact, the
// fake is never called; if this test regresses to the old behaviour, it
// panics loudly rather than needing a live DB error to notice a query ran
// at all.
func TestPublishEventRejectsInvalidJSON(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := &panicOnAnyCallDB{t: t}

	eventID := uuid.New()
	tenantID := uuid.New()

	matched, err := PublishEvent(context.Background(), db, quiet,
		&plugin.Environment{Dialect: plugin.DialectPostgres, Logger: quiet},
		eventID, tenantID, "order.created", json.RawMessage(`{"not valid json`), nil)

	if err == nil {
		t.Fatal("PublishEvent accepted malformed JSON -- a row like this, once written, " +
			"becomes the permanent head of its (tenant, event_type) claim queue and blocks " +
			"every later event of that type (cleat#2666)")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("PublishEvent error = %q, want it to name the actual problem (invalid JSON)", err)
	}
	if matched != 0 {
		t.Errorf("matched = %d, want 0 -- nothing should have been dispatched", matched)
	}
}

// TestPublishEventAllowsValidJSON is the negative control for the test
// above: an ordinary, well-formed event must not be caught by the same
// check. Also uses the panicking fake, so a DB call past validation fails
// this test the same way an over-eager rejection would fail the positive
// case -- both directions are observable without a real database.
func TestPublishEventAllowsValidJSON(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := &panicOnAnyCallDB{t: t, allowExec: true}

	eventID := uuid.New()
	tenantID := uuid.New()

	_, err := PublishEvent(context.Background(), db, quiet,
		&plugin.Environment{Dialect: plugin.DialectPostgres, Logger: quiet},
		eventID, tenantID, "order.created", json.RawMessage(`{"amount":100}`), nil)

	// allowExec makes Exec return (0, nil) rather than panicking, so
	// PublishEvent takes the "already ingested" early return -- the point is
	// only that validation did not itself refuse a well-formed body.
	if err != nil {
		t.Fatalf("PublishEvent refused valid JSON: %v", err)
	}
}

// panicOnAnyCallDB is a plugin.PluginDB that panics on every method unless
// allowExec permits a single no-op Exec. It exists to prove WHERE in
// PublishEvent's control flow the JSON check runs, not to simulate storage.
type panicOnAnyCallDB struct {
	t         *testing.T
	allowExec bool
}

func (p *panicOnAnyCallDB) Begin(ctx context.Context) (plugin.PluginTx, error) {
	p.t.Fatal("panicOnAnyCallDB.Begin called -- PublishEvent reached storage before (or without) validating eventData")
	return nil, nil
}

func (p *panicOnAnyCallDB) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if p.allowExec {
		return 0, nil
	}
	p.t.Fatal("panicOnAnyCallDB.Exec called -- PublishEvent reached storage before validating eventData")
	return 0, nil
}

func (p *panicOnAnyCallDB) Query(ctx context.Context, query string, args ...any) (plugin.Rows, error) {
	p.t.Fatal("panicOnAnyCallDB.Query called -- PublishEvent reached storage before (or without) validating eventData")
	return nil, nil
}

func (p *panicOnAnyCallDB) QueryRow(ctx context.Context, query string, args ...any) plugin.RowScanner {
	p.t.Fatal("panicOnAnyCallDB.QueryRow called -- PublishEvent reached storage before (or without) validating eventData")
	return nil
}

func (p *panicOnAnyCallDB) Ping(ctx context.Context) error {
	p.t.Fatal("panicOnAnyCallDB.Ping called -- PublishEvent reached storage before (or without) validating eventData")
	return nil
}
