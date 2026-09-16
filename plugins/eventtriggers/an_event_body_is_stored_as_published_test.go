package eventtriggers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

// A published event body reaches storage, an awaiter, and a workflow input as
// the publisher wrote it. cleat#1641.
//
// WHY THIS TEST TOUCHES NO DATABASE, which is the point rather than a
// convenience. The defect was never in a column. eventtriggers decoded the
// event into a map[string]any and re-encoded it, so
// 123456789012345678901234567890 became 1.2345678901234568e+29 IN GO, before
// any database had a say. Converting the column -- which cleat#1622 did for
// nine sibling columns -- would have measured clean against a direct write and
// fixed nothing on the path a publisher actually takes. A test gated on a DSN
// would also have been skipped on most CI jobs, for a defect that is pure Go.
//
// EVERY ASSERTION COMPARES BYTES. Decoding the result to compare numbers is
// how this defect stayed invisible: a float64 round trip recovers 2^53 and
// smaller exactly, so a decoded comparison agrees with the broken code on
// every value a test author is likely to pick.
//
// The value below is ~1e29. Chosen because float64 cannot represent it and
// because it is not a boundary anyone special-cases -- 2^63 and 2^64 are
// handled by MySQL's JSON type (cleat#1622), which is a different layer with a
// different wrong answer. Note the two are not even the same wrong answer:
// MySQL yields 1.2345678901234566e29 and Go yields 1.2345678901234568e+29, so
// a test asserting a hardcoded degraded form passes on one path and fails on
// the other.
const (
	bigNumber  = `123456789012345678901234567890`
	tmplNumber = `987654321098765432109876543210`
	degraded   = `1.2345678901234568e+29`
)

func TestAnEventBodyIsStoredAsPublished(t *testing.T) {
	// FLOOR: the constants above must actually be values that Go's default
	// decode destroys. If encoding/json ever round-tripped them, every arm
	// below would pass against the broken code and say nothing. This is the
	// known-positive -- the case already proven broken, which the arms are
	// then required to report.
	t.Run("the control: a default decode really does destroy this value", func(t *testing.T) {
		var m map[string]any
		if err := json.Unmarshal([]byte(`{"n":`+bigNumber+`}`), &m); err != nil {
			t.Fatalf("UNMEASURED: control input did not decode: %v", err)
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("UNMEASURED: control value did not re-marshal: %v", err)
		}
		if bytes.Contains(out, []byte(bigNumber)) {
			t.Fatalf("UNMEASURED: the default decode preserved %s, so this test "+
				"cannot distinguish a fixed pipeline from a broken one.\n"+
				"  re-marshalled: %s", bigNumber, out)
		}
		if !bytes.Contains(out, []byte(degraded)) {
			t.Fatalf("UNMEASURED: the default decode produced neither the "+
				"original nor the expected degraded form.\n  got: %s", out)
		}
	})

	t.Run("the stored bytes are the published bytes", func(t *testing.T) {
		db := newRecordingDB(t)
		body := json.RawMessage(`{"account":` + bigNumber + `,"note":"hi"}`)

		if _, err := PublishEvent(context.Background(), db, quietLogger(),
			&plugin.Environment{Dialect: plugin.DialectPostgres},
			uuid.New(), uuid.New(), "order.created", body); err != nil {
			t.Fatalf("PublishEvent: %v", err)
		}

		stored := db.insertedEventData(t)
		if stored != string(body) {
			t.Errorf("event_data is not what was published.\n"+
				"  published: %s\n  stored:    %s\n\n"+
				"PublishEvent must forward its argument, not re-encode it. "+
				"Anything that decodes the body into a map[string]any on the "+
				"way through rewrites a number this size. cleat#1641.",
				body, stored)
		}
	})

	t.Run("an awaiter is signalled with the published bytes", func(t *testing.T) {
		db := newRecordingDB(t)
		db.awaiters = []string{"wf-awaiting-1"}
		body := json.RawMessage(`{"account":` + bigNumber + `}`)

		var payload string
		var signalled int
		env := &plugin.Environment{
			Dialect: plugin.DialectPostgres,
			SignalWorkflow: func(_ context.Context, _, _, p string) error {
				payload, signalled = p, signalled+1
				return nil
			},
		}
		if _, err := PublishEvent(context.Background(), db, quietLogger(), env,
			uuid.New(), uuid.New(), "order.created", body); err != nil {
			t.Fatalf("PublishEvent: %v", err)
		}

		// FLOOR: without this, a fake that returned no awaiters would make the
		// comparison below vacuous and the arm would pass having measured
		// nothing.
		if signalled != 1 {
			t.Fatalf("UNMEASURED: expected exactly 1 signal, got %d", signalled)
		}
		if payload != string(body) {
			t.Errorf("the awaiter received a different value than was published.\n"+
				"  published: %s\n  delivered: %s\n\n"+
				"This is not a storage question: the signal payload never "+
				"touches a column. cleat#1641.", body, payload)
		}
	})

	t.Run("a workflow input keeps the event's number AND the template's", func(t *testing.T) {
		// The template degrades INDEPENDENTLY of the event, which is why both
		// appear here. A fix confined to the event path leaves a subscription
		// whose input_template carries a large number handing the workflow a
		// different one -- and a test that published a large number would not
		// have noticed, because the event half would be correct.
		tmpl := json.RawMessage(`{"ledger":` + tmplNumber + `,"src":"tmpl"}`)
		data := json.RawMessage(`{"account":` + bigNumber + `}`)

		merged, err := mergeInputAndTemplate(tmpl, data)
		if err != nil {
			t.Fatalf("mergeInputAndTemplate: %v", err)
		}
		for _, want := range []string{bigNumber, tmplNumber} {
			if !bytes.Contains(merged, []byte(want)) {
				t.Errorf("the workflow input lost %s.\n  got: %s", want, merged)
			}
		}
	})

	t.Run("the retry path does not re-narrow what storage kept", func(t *testing.T) {
		// An event stored exactly is read back and re-dispatched. Before
		// cleat#1641 both retry paths decoded the stored bytes into a
		// map[string]any first, so storage being correct bought nothing: the
		// number was destroyed again on every retry, and a test that only
		// checked the column would have reported the bug fixed.
		db := newRecordingDB(t)
		db.subscriptions = []fakeSubscription{{
			id:      uuid.New(),
			defName: "process-order",
			tmpl:    json.RawMessage(`{"ledger":` + tmplNumber + `}`),
			enabled: true,
		}}
		stored := json.RawMessage(`{"account":` + bigNumber + `}`)

		var input json.RawMessage
		var started int
		env := &plugin.Environment{
			Dialect: plugin.DialectPostgres,
			StartWorkflow: func(_ context.Context, req plugin.StartRequest) (string, error) {
				input, started = req.Input, started+1
				return "run-1", nil
			},
		}
		matched, err := triggerMatchingWorkflows(context.Background(), db, quietLogger(),
			env, uuid.New(), uuid.New(), "order.created", stored)
		if err != nil {
			t.Fatalf("triggerMatchingWorkflows: %v", err)
		}
		if started != 1 || matched != 1 {
			t.Fatalf("UNMEASURED: expected 1 workflow start, got started=%d matched=%d",
				started, matched)
		}
		for _, want := range []string{bigNumber, tmplNumber} {
			if !bytes.Contains(input, []byte(want)) {
				t.Errorf("re-dispatching stored bytes lost %s.\n  got: %s", want, input)
			}
		}
	})

	t.Run("the HTTP body is not decoded on the way in", func(t *testing.T) {
		// publishEventRequest.Data was a map[string]any, so the body was
		// already degraded before PublishEvent was called. Its type is the
		// fix; this arm is what fails if someone changes it back.
		db := newRecordingDB(t)
		p := &Plugin{db: db, logger: quietLogger(), dialect: plugin.DialectPostgres,
			env: &plugin.Environment{Dialect: plugin.DialectPostgres}}

		id := uuid.New()
		body := `{"id":"` + id.String() + `","event_type":"order.created",` +
			`"data":{"account":` + bigNumber + `}}`
		r := httptest.NewRequest(http.MethodPost, "/api/events/publish", strings.NewReader(body))
		r = r.WithContext(auth.WithTenantID(r.Context(), uuid.New()))
		w := httptest.NewRecorder()
		p.handlePublishEvent(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("UNMEASURED: handler returned %d: %s", w.Code, w.Body.String())
		}
		stored := db.insertedEventData(t)
		if !strings.Contains(stored, bigNumber) {
			t.Errorf("the published body was rewritten between the request and storage.\n"+
				"  sent:   {\"account\":%s}\n  stored: %s", bigNumber, stored)
		}
	})
}

// ---------------------------------------------------------------------------
// A recording plugin.PluginDB.
//
// It FATALS on a query it does not recognise rather than returning no rows.
// A fake that answers an unknown SELECT with an empty result makes every arm
// above pass vacuously -- no subscriptions means no workflow input to check,
// no awaiters means no signal to check -- so the fake would silently become
// the thing being measured the moment the SQL changed.
// ---------------------------------------------------------------------------

type fakeSubscription struct {
	id      uuid.UUID
	defName string
	tmpl    json.RawMessage
	filter  string
	enabled bool
}

type recordingDB struct {
	t             *testing.T
	execs         [][]any
	execQueries   []string
	subscriptions []fakeSubscription
	awaiters      []string
}

func newRecordingDB(t *testing.T) *recordingDB { return &recordingDB{t: t} }

func (d *recordingDB) Exec(_ context.Context, query string, args ...any) (int64, error) {
	d.execQueries = append(d.execQueries, query)
	d.execs = append(d.execs, args)
	if strings.Contains(query, "INSERT INTO ingested_events") {
		return 1, nil
	}
	return 0, nil
}

func (d *recordingDB) Query(_ context.Context, query string, _ ...any) (plugin.Rows, error) {
	switch {
	case strings.Contains(query, "FROM event_subscriptions"):
		return &subscriptionRows{subs: d.subscriptions, i: -1}, nil
	case strings.Contains(query, "FROM event_awaiters"):
		return &awaiterRows{ids: d.awaiters, i: -1}, nil
	}
	d.t.Fatalf("recordingDB: unrecognised query, so this test would have "+
		"measured nothing:\n%s", query)
	return nil, nil
}

func (d *recordingDB) QueryRow(_ context.Context, query string, _ ...any) plugin.RowScanner {
	d.t.Fatalf("recordingDB: unexpected QueryRow:\n%s", query)
	return nil
}

func (d *recordingDB) Begin(context.Context) (plugin.PluginTx, error) {
	d.t.Fatal("recordingDB: unexpected Begin")
	return nil, nil
}

func (d *recordingDB) Ping(context.Context) error { return nil }

// insertedEventData returns the event_data argument of the one INSERT into
// ingested_events, failing if there was not exactly one.
func (d *recordingDB) insertedEventData(t *testing.T) string {
	t.Helper()
	var found []string
	for i, q := range d.execQueries {
		if !strings.Contains(q, "INSERT INTO ingested_events") {
			continue
		}
		args := d.execs[i]
		if len(args) != 4 {
			t.Fatalf("UNMEASURED: the ingested_events INSERT took %d args, want 4", len(args))
		}
		s, ok := args[3].(string)
		if !ok {
			t.Fatalf("UNMEASURED: event_data argument is %T, not a string", args[3])
		}
		found = append(found, s)
	}
	if len(found) != 1 {
		t.Fatalf("UNMEASURED: expected exactly 1 insert into ingested_events, got %d", len(found))
	}
	return found[0]
}

type subscriptionRows struct {
	subs []fakeSubscription
	i    int
}

func (r *subscriptionRows) Next() bool   { r.i++; return r.i < len(r.subs) }
func (r *subscriptionRows) Close() error { return nil }
func (r *subscriptionRows) Err() error   { return nil }

func (r *subscriptionRows) Scan(dest ...any) error {
	s := r.subs[r.i]
	return assignRow(dest, []any{
		s.id.String(), uuid.Nil.String(), "order.created", s.defName, "main",
		[]byte(s.tmpl), s.filter, s.enabled, time.Unix(0, 0).UTC(), int64(3),
	})
}

type awaiterRows struct {
	ids []string
	i   int
}

func (r *awaiterRows) Next() bool   { r.i++; return r.i < len(r.ids) }
func (r *awaiterRows) Close() error { return nil }
func (r *awaiterRows) Err() error   { return nil }
func (r *awaiterRows) Scan(dest ...any) error {
	return assignRow(dest, []any{r.ids[r.i]})
}

// assignRow copies src into dest the way database/sql would, including the
// sql.Scanner path that plugin.ScanRow's *plugin.GUID substitution relies on.
func assignRow(dest, src []any) error {
	if len(dest) != len(src) {
		return errColumnCount{got: len(dest), want: len(src)}
	}
	for i := range dest {
		if sc, ok := dest[i].(sql.Scanner); ok {
			if err := sc.Scan(src[i]); err != nil {
				return err
			}
			continue
		}
		switch d := dest[i].(type) {
		case *string:
			*d = src[i].(string)
		case *bool:
			*d = src[i].(bool)
		case *int:
			*d = int(src[i].(int64))
		case *int64:
			*d = src[i].(int64)
		case *time.Time:
			*d = src[i].(time.Time)
		case *[]byte:
			*d = src[i].([]byte)
		default:
			return errUnsupportedDest{}
		}
	}
	return nil
}

type errColumnCount struct{ got, want int }

func (e errColumnCount) Error() string {
	return "fake row: column count mismatch"
}

type errUnsupportedDest struct{}

func (errUnsupportedDest) Error() string { return "fake row: unsupported destination type" }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
