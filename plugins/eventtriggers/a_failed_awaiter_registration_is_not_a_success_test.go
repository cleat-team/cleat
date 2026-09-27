package eventtriggers

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#1473: registerAwaiter logged its failure and returned nothing, so
// awaitEvent reported a successful `{"found": false}` whether or not the
// awaiter row was written. A workflow told "no event yet" waits for something
// nothing will deliver to it.
//
// THE DOUBLE RECORDS WHAT IT WAS ASKED, rather than returning a canned answer.
// A stub that failed every Exec regardless would pass against an implementation
// that never attempted the registration at all -- so this one asserts the
// upsert was actually issued, and asserts on the recorded query rather than on
// a call count.
type awaiterStubDB struct {
	execErr   error
	execQuery []string
}

func (d *awaiterStubDB) Exec(_ context.Context, q string, _ ...any) (int64, error) {
	d.execQuery = append(d.execQuery, q)
	return 0, d.execErr
}

// QueryRow returns no rows, which is the branch that registers an awaiter.
func (d *awaiterStubDB) QueryRow(_ context.Context, _ string, _ ...any) plugin.RowScanner {
	return noRowsScanner{}
}
func (d *awaiterStubDB) Query(_ context.Context, _ string, _ ...any) (plugin.Rows, error) {
	return nil, sql.ErrNoRows
}
func (d *awaiterStubDB) Begin(context.Context) (plugin.PluginTx, error) { return nil, sql.ErrConnDone }
func (d *awaiterStubDB) Ping(context.Context) error                     { return nil }

type noRowsScanner struct{}

func (noRowsScanner) Scan(...any) error { return sql.ErrNoRows }

func awaitEventCtx() context.Context {
	return plugin.WithCallContext(context.Background(), &plugin.CallContext{
		TenantID:   "tenant-1473",
		WorkflowID: "wf-1473",
	})
}

func newAwaiterPlugin(db plugin.PluginDB) *Plugin {
	return &Plugin{
		db:      db,
		dialect: plugin.DialectPostgres,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAFailedAwaiterRegistrationIsNotASuccess(t *testing.T) {
	t.Run("registration fails, awaitEvent must not report success", func(t *testing.T) {
		boom := errors.New("event_awaiters is unreachable")
		db := &awaiterStubDB{execErr: boom}

		out, err := newAwaiterPlugin(db).awaitEvent(awaitEventCtx(),
			`{"event_type":"order.created","timeout_ms":5000}`)

		if err == nil {
			t.Fatalf("awaitEvent returned success (%q) after the awaiter registration failed. "+
				"The workflow is told there is no event yet and settles down to wait, and the "+
				"row that would wake it was never written (cleat#1473).", out)
		}
		if !errors.Is(err, boom) {
			t.Errorf("error does not wrap the cause: %v", err)
		}
		if !strings.Contains(err.Error(), "register awaiter") {
			t.Errorf("error does not say which write failed: %v", err)
		}

		// The registration was actually attempted. Without this the test would
		// also pass against a version that never issued the upsert.
		if len(db.execQuery) == 0 {
			t.Fatal("no Exec was issued at all; the test proves nothing about registration")
		}
		if !strings.Contains(strings.ToLower(db.execQuery[0]), "event_awaiters") {
			t.Errorf("first Exec was %q, expected the awaiter upsert", db.execQuery[0])
		}
	})

	// THE CONTROL. Without it the fix could be "always return an error on this
	// branch", which would break every ordinary wait -- the far more common
	// path, and a worse defect than the one being fixed.
	t.Run("registration succeeds, awaitEvent still reports not-found", func(t *testing.T) {
		db := &awaiterStubDB{execErr: nil}

		out, err := newAwaiterPlugin(db).awaitEvent(awaitEventCtx(),
			`{"event_type":"order.created","timeout_ms":5000}`)

		if err != nil {
			t.Fatalf("awaitEvent failed on the ordinary no-event path: %v", err)
		}
		if !strings.Contains(out, `"found":false`) {
			t.Errorf("output %q does not report not-found", out)
		}
	})
}
