package auditlog

// Export and the verify endpoint against real databases (cleat#2047), on all three
// dialects, driven through the HTTP handlers the plugin registers.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// get calls a plugin route as tenant and returns the status and body.
func (e *chainEnv) get(p *Plugin, tenant uuid.UUID, target string) (int, string) {
	e.t.Helper()
	// uuid.Nil here means "no tenant in the context at all"; the default tenant, whose id is
	// the zero UUID, is getAs with authenticated true.
	return e.getAs(p, tenant, tenant != uuid.Nil, target)
}

func (e *chainEnv) getAs(p *Plugin, tenant uuid.UUID, authenticated bool, target string) (int, string) {
	e.t.Helper()
	mux := http.NewServeMux()
	if err := p.RegisterRoutes(mux); err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("GET", target, nil)
	if authenticated {
		req = req.WithContext(auth.WithTenantID(req.Context(), tenant))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// exportLines splits an export body into its records.
func exportLines(t *testing.T, body string) (events []exportEvent, cp *exportCheckpoint, raw []string) {
	t.Helper()
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if l == "" {
			continue
		}
		raw = append(raw, l)
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Fatalf("an export line is not JSON: %v\n%s", err, l)
		}
		switch probe.Type {
		case "event":
			var ev exportEvent
			if err := json.Unmarshal([]byte(l), &ev); err != nil {
				t.Fatal(err)
			}
			events = append(events, ev)
		case "checkpoint":
			cp = new(exportCheckpoint)
			if err := json.Unmarshal([]byte(l), cp); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown record type %q", probe.Type)
		}
	}
	return
}

func ids(evs []exportEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.ID)
	}
	sort.Strings(out)
	return out
}

// legacyRow inserts a row as it was written before the chain existed: no seq.
func (e *chainEnv) legacyRow(tenant uuid.UUID, daysAgo int) {
	e.t.Helper()
	ts := map[plugin.Dialect]string{
		plugin.DialectPostgres: fmt.Sprintf(`now() - interval '%d days'`, daysAgo),
		plugin.DialectMySQL:    fmt.Sprintf(`NOW(6) - INTERVAL %d DAY`, daysAgo),
		plugin.DialectMSSQL:    fmt.Sprintf(`DATEADD(DAY, -%d, SYSDATETIMEOFFSET())`, daysAgo),
	}[e.d.dialect]
	e.mustChange(tenant, `INSERT INTO audit_events (id, tenant_id, timestamp, method, path, status_code, user_id, ip_address, user_agent, duration_ms)
		VALUES ($1, $2, `+ts+`, 'GET', '/legacy', 200, 'old', '10.0.0.9', 'legacy-agent', 1)`, uuid.NewString(), tenant.String())
}

// The acceptance criterion: for a range, the export returns exactly the rows
// GET /audit/events returns.
func TestExportReturnsExactlyTheRowsEventsReturns(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant, other := uuid.New(), uuid.New()
		e.record(p, tenant, 30)
		e.record(p, other, 5)
		for _, d := range []int{200, 199, 198} {
			e.legacyRow(tenant, d)
		}

		eventIDs := func(target string) []string {
			code, body := e.get(p, tenant, target)
			if code != 200 {
				t.Fatalf("%s: %d %s", target, code, body)
			}
			var evs []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(body), &evs); err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, ev := range evs {
				out = append(out, ev.ID)
			}
			sort.Strings(out)
			return out
		}
		exportIDs := func(target string) ([]string, *exportCheckpoint) {
			code, body := e.get(p, tenant, target)
			if code != 200 {
				t.Fatalf("%s: %d %s", target, code, body)
			}
			evs, cp, _ := exportLines(t, body)
			return ids(evs), cp
		}

		// The whole log: 30 chained and 3 unchained, and none of the other tenant's.
		wantAll := eventIDs("/audit/events?limit=1000")
		gotAll, cp := exportIDs("/audit/export")
		if len(wantAll) != 33 || fmt.Sprint(gotAll) != fmt.Sprint(wantAll) {
			t.Fatalf("unbounded export has %d rows, /audit/events has %d (want 33 each, the same rows)", len(gotAll), len(wantAll))
		}
		if cp == nil || cp.HeadSeq != 30 || cp.Events != 33 || cp.TenantID != tenant.String() {
			t.Fatalf("checkpoint %+v, want head_seq 30, 33 events, this tenant", cp)
		}

		// A range that starts and ends inside the chained rows.
		from, to := e.tsOf(tenant, 10), e.tsOf(tenant, 20)
		q := "from=" + url.QueryEscape(from.Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339Nano))
		wantRange := eventIDs("/audit/events?limit=1000&" + q)
		gotRange, _ := exportIDs("/audit/export?" + q)
		if len(wantRange) != 11 || fmt.Sprint(gotRange) != fmt.Sprint(wantRange) {
			t.Fatalf("range export %d rows vs /audit/events %d rows (want 11, identical)\nexport: %v\nevents: %v", len(gotRange), len(wantRange), gotRange, wantRange)
		}
	})
}

func TestExportPagesAndResumesFromAnyCursor(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 20)
		for _, d := range []int{200, 199, 198, 197, 196} {
			e.legacyRow(tenant, d)
		}
		big := func() (string, []exportEvent) {
			code, body := e.get(p, tenant, "/audit/export")
			if code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			evs, _, _ := exportLines(t, body)
			return body, evs
		}
		wholeBody, whole := big()

		// Pages of 3 cross the boundary between the unchained rows and the chained ones
		// and end mid-phase; the output must not change.
		old := exportPageSize
		exportPageSize = 3
		t.Cleanup(func() { exportPageSize = old })
		smallBody, small := big()
		if smallBody != wholeBody || len(whole) != 25 {
			t.Fatalf("paged export (%d records) differs from the unpaged one (%d)", len(small), len(whole))
		}

		// Resume after each of several records: the rest, exactly, and a checkpoint.
		for _, after := range []int{0, 4, 5, 6, 12, 23, 24} {
			code, body := e.get(p, tenant, "/audit/export?cursor="+url.QueryEscape(whole[after].Cursor))
			if code != 200 {
				t.Fatalf("resume after record %d: %d %s", after, code, body)
			}
			evs, cp, _ := exportLines(t, body)
			if fmt.Sprint(ids(evs)) != fmt.Sprint(ids(whole[after+1:])) || cp == nil || cp.Events != int64(len(whole)-after-1) {
				t.Errorf("resuming after record %d gave %d records (checkpoint %+v), want the %d that follow", after, len(evs), cp, len(whole)-after-1)
			}
			if len(evs) > 0 && evs[0].ID != whole[after+1].ID {
				t.Errorf("resuming after record %d starts at %s, want %s", after, evs[0].ID, whole[after+1].ID)
			}
		}
	})
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestExportRefusesWhatItCannotDoInsteadOfGuessing(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 3)
		for target, want := range map[string]int{
			"/audit/export?from=yesterday":                                    400,
			"/audit/export?to=2026-13-45T00:00:00Z":                           400,
			"/audit/export?from=2026-09-24T00:00:00Z&to=2026-09-23T00:00:00Z": 400,
			"/audit/export?cursor=not-a-cursor":                               400,
			"/audit/export?cursor=!!!":                                        400, // not base64 at all
			"/audit/export?cursor=" + b64("c:-5"):                             400, // a position that cannot exist
			"/audit/export?cursor=" + b64("c:x"):                              400,
			"/audit/export?cursor=" + b64("u:12:not-a-uuid"):                  400,
			"/audit/export?cursor=" + b64("z:1"):                              400,
			"/audit/export?format=csv":                                        400,
			"/audit/export?format=jsonl":                                      200,
		} {
			code, body := e.get(p, tenant, target)
			if code != want {
				t.Errorf("%s: %d, want %d\n%s", target, code, want, body)
			}
			if want == 400 && strings.Contains(body, `"type":"event"`) {
				t.Errorf("%s: a refused export still emitted records", target)
			}
		}
		if code, _ := e.get(p, uuid.Nil, "/audit/export"); code != 401 {
			t.Errorf("no tenant: %d, want 401", code)
		}
		if code, _ := e.get(p, uuid.Nil, "/audit/verify"); code != 401 {
			t.Errorf("verify with no tenant: %d, want 401", code)
		}
	})
}

// failingDB fails every query after the first `after` ones.
type failingDB struct {
	plugin.PluginDB
	after, n int
}

func (f *failingDB) Query(ctx context.Context, q string, a ...any) (plugin.Rows, error) {
	if f.n++; f.n > f.after {
		return nil, errors.New("injected: the database went away")
	}
	return f.PluginDB.Query(ctx, q, a...)
}

// A failure after records were sent must not end as a tidy, complete-looking stream.
func TestAnExportThatFailsMidStreamDoesNotLookComplete(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 9)
		old := exportPageSize
		exportPageSize = 3
		t.Cleanup(func() { exportPageSize = old })
		// Query 1 is the unchained phase (empty), 2 is the first chained page; fail the third.
		p.db = &failingDB{PluginDB: p.db, after: 2}

		mux := http.NewServeMux()
		_ = p.RegisterRoutes(mux)
		req := httptest.NewRequest("GET", "/audit/export", nil).WithContext(auth.WithTenantID(context.Background(), tenant))
		rec := httptest.NewRecorder()
		var aborted any
		func() {
			defer func() { aborted = recover() }()
			mux.ServeHTTP(rec, req)
		}()
		if aborted != http.ErrAbortHandler {
			t.Fatalf("the handler ended with %v, want it to abort the connection (http.ErrAbortHandler)", aborted)
		}
		evs, cp, _ := exportLines(t, rec.Body.String())
		if len(evs) != 3 || cp != nil {
			t.Fatalf("%d records and checkpoint %v; want the 3 that were sent and NO checkpoint", len(evs), cp)
		}
	})
}

func TestVerifyEndpointReportsTheChain(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 8)
		type resp struct {
			OK      bool        `json:"ok"`
			Checked int64       `json:"checked"`
			Break   *ChainBreak `json:"break"`
		}
		var r resp
		code, body := e.get(p, tenant, "/audit/verify")
		if err := json.Unmarshal([]byte(body), &r); err != nil || code != 200 || !r.OK || r.Checked != 8 {
			t.Fatalf("an untouched chain: %d %s", code, body)
		}
		e.mustChange(tenant, `UPDATE audit_events SET path = '/edited' WHERE tenant_id = $1 AND seq = 4`, tenant.String())
		code, body = e.get(p, tenant, "/audit/verify")
		r = resp{}
		if err := json.Unmarshal([]byte(body), &r); err != nil || code != 200 || r.OK || r.Break == nil || r.Break.Kind != BreakEdited || r.Break.Seq != 4 {
			t.Fatalf("an edited row: %d %s, want 200, ok false, edited at seq 4", code, body)
		}
		// A floor moved over rows that have not expired is reported, because the endpoint
		// passes the plugin's own retention_days.
		floor := uuid.New()
		e.record(p, floor, 6)
		var h3 string
		e.scan(floor, `SELECT row_hash FROM audit_events WHERE tenant_id = $1 AND seq = 3`, []any{floor.String()}, &h3)
		ts3 := e.tsOf(floor, 3).UnixMicro()
		e.mustChange(floor, `DELETE FROM audit_events WHERE tenant_id = $1 AND seq <= 3`, floor.String())
		e.mustChange(floor, `UPDATE audit_chain_heads SET floor_seq = 3, floor_hash = $1, floor_ts = $2 WHERE tenant_id = $3`, strings.TrimSpace(h3), ts3, floor.String())
		code, body = e.get(p, floor, "/audit/verify")
		r = resp{}
		if err := json.Unmarshal([]byte(body), &r); err != nil || code != 200 || r.OK || r.Break == nil || r.Break.Kind != BreakFloorUnexpired {
			t.Fatalf("a floor over unexpired rows: %d %s, want ok false, %s", code, body, BreakFloorUnexpired)
		}
		// Another tenant's verify is not affected, and cannot see this one's rows.
		other := uuid.New()
		e.record(p, other, 2)
		code, body = e.get(p, other, "/audit/verify")
		if r = (resp{}); json.Unmarshal([]byte(body), &r) != nil || code != 200 || !r.OK || r.Checked != 2 {
			t.Fatalf("another tenant: %d %s", code, body)
		}
	})
}

// The exported strings are what an offline verifier hashes. The reference verifier is
// written in Python from the prose contract and shares no code with chain.go.
func TestAnExportVerifiesOfflineWithTheReferenceImplementation(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed, so the reference verifier cannot run")
	}
	verify := func(stream string) (int, string) {
		cmd := exec.Command(py, "testdata/audit_chain_reference.py", "verify-export")
		cmd.Stdin = strings.NewReader(stream)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("running the reference verifier: %v", err)
			}
			code = ee.ExitCode()
		}
		return code, out.String()
	}
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 15) // awkward text and an empty user id, on every dialect
		e.legacyRow(tenant, 200)
		_, body := e.get(p, tenant, "/audit/export")

		if code, out := verify(body); code != 0 || !strings.Contains(out, "15 chained verified, 1 unchained") {
			t.Fatalf("the reference verifier rejects an untouched export: exit %d\n%s", code, out)
		}
		// Known positives: an edit, and a truncation, must each be refused, differently.
		edited := strings.Replace(body, `"/api/café/3/`, `"/api/cafe/3/`, 1)
		if edited == body {
			t.Fatal("the edit changed nothing, so the control tests nothing")
		}
		if code, out := verify(edited); code != 1 || !strings.Contains(out, "EDITED") {
			t.Errorf("an edited record: exit %d\n%s\nwant exit 1 and EDITED", code, out)
		}
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if code, out := verify(strings.Join(lines[:len(lines)-1], "\n") + "\n"); code != 2 || !strings.Contains(out, "TRUNCATED") {
			t.Errorf("an export with no checkpoint: exit %d\n%s\nwant exit 2 and TRUNCATED", code, out)
		}
	})
}

// An export is a snapshot of the chain as it stood when it began. Rows appended while it
// runs belong to the next export: otherwise the checkpoint's head would not be the head
// the events end at, and a consumer could not tell a complete export from a stale one.
func TestRowsAppendedDuringAnExportBelongToTheNextOne(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		tenant := uuid.New()
		e.record(p, tenant, 9)
		old := exportPageSize
		exportPageSize = 3
		t.Cleanup(func() { exportPageSize = old })
		writer := e.plugin()
		p.db = &hookDB{PluginDB: p.db, at: 3, fn: func() { e.record(writer, tenant, 5) }}

		code, body := e.get(p, tenant, "/audit/export")
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		evs, cp, _ := exportLines(t, body)
		if len(evs) != 9 || cp == nil || cp.HeadSeq != 9 || *evs[len(evs)-1].Seq != 9 || cp.Events != 9 {
			t.Fatalf("%d events ending at seq %d, checkpoint %+v: want the 9 rows that existed when the export began, head_seq 9", len(evs), *evs[len(evs)-1].Seq, cp)
		}
		// And the next export, from the cursor, has the rest, from where this one ended.
		code, body = e.get(e.plugin(), tenant, "/audit/export?cursor="+url.QueryEscape(evs[len(evs)-1].Cursor))
		next, cp2, _ := exportLines(t, body)
		if code != 200 || len(next) != 5 || *next[0].Seq != 10 || cp2 == nil || cp2.HeadSeq != 14 {
			t.Fatalf("the next export: %d, %d events, checkpoint %+v; want the 5 appended rows from seq 10, head_seq 14", code, len(next), cp2)
		}
	})
}

// The seeded default tenant is the zero UUID, and it is a tenant like any other: comparing
// the id to uuid.Nil instead of checking whether the request was authenticated rejects its own
// valid API key with a 401 (cleat#2183, which fixed /audit/events the same way).
func TestTheDefaultTenantMayExportAndVerifyItself(t *testing.T) {
	forEachChainDialect(t, func(t *testing.T, e *chainEnv) {
		p := e.plugin()
		def := uuid.Nil
		e.record(p, def, 4)
		code, body := e.getAs(p, def, true, "/audit/export")
		evs, cp, _ := exportLines(t, body)
		if code != 200 || len(evs) != 4 || cp == nil || cp.HeadSeq != 4 {
			t.Fatalf("the default tenant's export: %d, %d events, checkpoint %+v", code, len(evs), cp)
		}
		code, body = e.getAs(p, def, true, "/audit/verify")
		if code != 200 || !strings.Contains(body, `"ok":true`) {
			t.Fatalf("the default tenant's verify: %d %s", code, body)
		}
		if code, _ := e.getAs(p, def, false, "/audit/export"); code != 401 {
			t.Fatalf("an unauthenticated request: %d, want 401", code)
		}
	})
}
