package engine

// cleat#3775, part 2: a deployment that seals query_state is searched over a WINDOW of the tenant's most
// recent runs, in the worker, and the result says how much it covered.
//
// The property that matters most is not that a match is found, it is that "no match" cannot be mistaken
// for "no such run": a window that was used up and an answer that is complete must read differently, and
// the boundary between them (a window exactly as large as the history) is where it is easiest to be off by
// one in the flattering direction.

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// sealedStore opens a PostgreSQL store that seals query_state, after proving it does: the whole point of
// the window is a column the database cannot read, so a store that quietly wrote plaintext would make every
// assertion below pass for the wrong reason.
func sealedStore(t *testing.T, db *sql.DB, tenant string) *PostgresStore {
	t.Helper()
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return NewPostgresStore(db).WithTenant(tenant).WithEncryption(enc, true)
}

func onDiskIsSealed(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT query_state::text FROM workflow_payloads WHERE id = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("UNMEASURED: reading query_state of %s: %v", id, err)
	}
	if !strings.HasPrefix(raw, `"`) || strings.Contains(raw, "order_id") {
		t.Fatalf("UNMEASURED: query_state on disk is %q, not a sealed string: the premise of the window does not hold", raw)
	}
}

func TestAWindowSearchSaysWhatItCoveredSoNoMatchIsNeverMistakenForNoSuchRun(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	store := sealedStore(t, db, DefaultTenantUUID)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	// Oldest first: the run being looked for is the OLDEST of six, so a window smaller than the history
	// cannot reach it, and a window as large as the history can.
	target := publishRunAs(t, store, DefaultTenantUUID, "test", "w-0", map[string]string{"order_id": "123", "status": "open"})
	onDiskIsSealed(t, db, target)
	for i := 1; i <= 5; i++ {
		publishRunAs(t, store, DefaultTenantUUID, "test", "w-"+string(rune('0'+i)), map[string]string{"order_id": "other", "status": "open"})
	}
	pairs := map[string]string{"order_id": "123"}

	for _, c := range []struct {
		name                     string
		window                   int
		wantRuns                 []string
		wantScanned              int
		wantExhausted            bool
		wantMatched, wantWindowN int
	}{
		{"a window smaller than the history cannot reach the oldest run, and says it ran out", 3, nil, 3, true, 0, 3},
		{"one short of the history still runs out", 5, nil, 5, true, 0, 5},
		{"a window exactly as large as the history reaches it and is NOT exhausted (the boundary)", 6, []string{target}, 6, false, 1, 6},
		{"a window larger than the history is not exhausted and scanned only what exists", 100, []string{target}, 6, false, 1, 100},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: pairs, Limit: 100}, c.window)
			if err != nil {
				t.Fatal(err)
			}
			if got := publishedIDs(res.Runs); !reflect.DeepEqual(got, c.wantRuns) && !(len(got) == 0 && len(c.wantRuns) == 0) {
				t.Errorf("runs = %v, want %v", got, c.wantRuns)
			}
			if res.Scanned != c.wantScanned || res.Exhausted != c.wantExhausted || res.Matched != c.wantMatched || res.Window != c.wantWindowN {
				t.Errorf("scanned=%d exhausted=%v matched=%d window=%d, want scanned=%d exhausted=%v matched=%d window=%d",
					res.Scanned, res.Exhausted, res.Matched, res.Window, c.wantScanned, c.wantExhausted, c.wantMatched, c.wantWindowN)
			}
			if res.Unreadable != 0 {
				t.Errorf("unreadable = %d, want 0", res.Unreadable)
			}
		})
	}

	t.Run("a value no run published is complete when the window covers the history, and exhausted when it does not", func(t *testing.T) {
		none := WorkflowFilter{QueryState: map[string]string{"order_id": "999"}, Limit: 100}
		full, err := store.SearchQueryStateWindow(ctx, none, 100)
		if err != nil || full.Matched != 0 || full.Exhausted {
			t.Errorf("complete window: %+v (err=%v), want no match and NOT exhausted: this is the answer that means there is no such run", full, err)
		}
		short, err := store.SearchQueryStateWindow(ctx, none, 2)
		if err != nil || short.Matched != 0 || !short.Exhausted {
			t.Errorf("short window: %+v (err=%v), want no match and exhausted: this is the answer that must not be read as 'no such run'", short, err)
		}
	})

	t.Run("it matches exactly: case, an empty value, and an absent key are what the SQL filter says they are", func(t *testing.T) {
		for _, c := range []struct {
			pairs map[string]string
			want  int
		}{
			{map[string]string{"ORDER_ID": "123"}, 0},
			{map[string]string{"order_id": "123", "status": "Open"}, 0},
			{map[string]string{"order_id": "123", "status": "open"}, 1},
			{map[string]string{"nope": ""}, 0},
			{map[string]string{"order_id": "other"}, 5},
		} {
			res, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: c.pairs, Limit: 100}, 100)
			if err != nil || res.Matched != c.want {
				t.Errorf("%v: matched %d (err=%v), want %d", c.pairs, res.Matched, err, c.want)
			}
		}
	})

	t.Run("it pages over the matches, newest first, and Matched is the total over the window", func(t *testing.T) {
		others := WorkflowFilter{QueryState: map[string]string{"order_id": "other"}}
		all, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: others.QueryState, Limit: 100}, 100)
		if err != nil || len(all.Runs) != 5 {
			t.Fatalf("all = %+v (err=%v)", all, err)
		}
		for i := 1; i < len(all.Runs); i++ {
			if all.Runs[i-1].CreatedAt.Before(all.Runs[i].CreatedAt) {
				t.Errorf("run %d is newer than run %d: the window search is not in the listing's order", i, i-1)
			}
		}
		p1, _ := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: others.QueryState, Limit: 2, Offset: 0}, 100)
		p2, _ := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: others.QueryState, Limit: 2, Offset: 2}, 100)
		p3, _ := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: others.QueryState, Limit: 2, Offset: 4}, 100)
		var paged []string
		for _, p := range []*QueryStateWindowResult{p1, p2, p3} {
			if p.Matched != 5 {
				t.Errorf("Matched = %d on a page, want the total over the window (5)", p.Matched)
			}
			paged = append(paged, publishedIDs2(p.Runs)...)
		}
		if want := publishedIDs2(all.Runs); !reflect.DeepEqual(paged, want) {
			t.Errorf("pages = %v, want %v: paging over the matches skipped or repeated a run", paged, want)
		}
		beyond, _ := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: others.QueryState, Limit: 2, Offset: 50}, 100)
		if len(beyond.Runs) != 0 || beyond.Matched != 5 {
			t.Errorf("an offset past the end = %d runs, matched %d; want none, and the total still 5", len(beyond.Runs), beyond.Matched)
		}
	})

	t.Run("the window is over the runs the OTHER filters select, not over everything", func(t *testing.T) {
		res, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: pairs, Status: "done", Limit: 100}, 100)
		if err != nil || res.Scanned != 0 || res.Matched != 0 || res.Exhausted {
			t.Errorf("status=done: %+v (err=%v), want a window that examined nothing", res, err)
		}
		res, err = store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: pairs, Status: "ready", Limit: 100}, 100)
		if err != nil || res.Matched != 1 {
			t.Errorf("status=ready: %+v (err=%v), want the one run", res, err)
		}
	})
}

// The SQL filter is the oracle: on a store that does NOT seal, the same data searched both ways must agree,
// whatever the case, the empty value, or the characters a LIKE-built form would get wrong.
func TestTheWindowSearchAgreesWithTheSQLFilterOnWhatItMatches(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	store := NewPostgresStore(db)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	const odd = "a\"b\\c é 100%_"
	for i, st := range []map[string]string{
		{"order_id": "123", "status": "open"}, {"order_id": "123", "status": "closed"}, {"order_id": "456", "status": "open"},
		nil, {"ORDER_ID": "123"}, {"order_id": "789", "status": "Open"}, {"note": ""}, {"": "v"}, {"note": odd},
	} {
		publishRunAs(t, store, DefaultTenantUUID, "test", "o-"+string(rune('a'+i)), st)
	}
	for _, pairs := range []map[string]string{
		{"order_id": "123"}, {"order_id": "123", "status": "open"}, {"order_id": "999"}, {"status": "open"},
		{"ORDER_ID": "123"}, {"status": "Open"}, {"note": ""}, {"nope": ""}, {"": "v"}, {"note": odd}, {"note": "a\"b\\c é 100%"},
	} {
		sqlRuns, err := store.ListWorkflows(ctx, WorkflowFilter{QueryState: pairs, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		win, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: pairs, Limit: 100}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := publishedIDs(sqlRuns), publishedIDs(win.Runs); !reflect.DeepEqual(a, b) && !(len(a) == 0 && len(b) == 0) {
			t.Errorf("%v: SQL filter found %v, window search found %v", pairs, a, b)
		}
	}
}

// The window is the tenant's, named in the SQL, on a connection that applies no row-level security. A
// sealed run cannot be matched across tenants (its ciphertext is bound to its tenant), so there the leak
// shows as another tenant's runs consuming THIS tenant's window and coming back unreadable; on plaintext it
// shows as a wrong match. Both are asserted, because each is blind to the other's form of the same bug.
func TestAWindowSearchIsOverItsOwnTenantEvenThroughARoleThatBypassesRLS(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	var bypass bool
	if err := db.QueryRow(`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil || !bypass {
		t.Fatalf("UNMEASURED: this test needs a connection that BYPASSES row-level security (err=%v)", err)
	}
	const tenantB = "f3775000-0000-4000-8000-0000000000cc"
	if _, err := db.Exec(`INSERT INTO admin.tenants (tenant_id, name, suspended) VALUES ($1, 'cleat-3775-window-b', false) ON CONFLICT (tenant_id) DO NOTHING`, tenantB); err != nil {
		t.Fatalf("registering tenant B: %v", err)
	}
	t.Cleanup(func() {
		testutil.CleanupPostgresTestData(t, db)
		_, _ = db.Exec(`DELETE FROM admin.tenants WHERE tenant_id = $1`, tenantB)
	})
	ctx := context.Background()

	for _, sealed := range []bool{true, false} {
		sealed := sealed
		name := "plaintext"
		if sealed {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			testutil.CleanupPostgresTestData(t, db)
			var a, b *PostgresStore
			if sealed {
				a, b = sealedStore(t, db, DefaultTenantUUID), sealedStore(t, db, tenantB)
			} else {
				a, b = NewPostgresStore(db).WithTenant(DefaultTenantUUID), NewPostgresStore(db).WithTenant(tenantB)
			}
			deployFaultTestDef(t, a)
			deployFaultTestDef(t, b)
			// B publishes the SAME state, and publishes LAST so it is the newest rows: a window with no
			// tenant predicate is filled by B's runs first.
			idA := publishRunAs(t, a, DefaultTenantUUID, "test", "iso-a", map[string]string{"order_id": "123"})
			for i := 0; i < 3; i++ {
				publishRunAs(t, b, tenantB, "test", "iso-b"+string(rune('0'+i)), map[string]string{"order_id": "123"})
			}
			res, err := a.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got := publishedIDs(res.Runs); !reflect.DeepEqual(got, []string{idA}) {
				t.Errorf("tenant A found %v, want only its own %v: the window read another tenant's run", got, []string{idA})
			}
			if res.Scanned != 1 || res.Unreadable != 0 || res.Matched != 1 {
				t.Errorf("scanned=%d unreadable=%d matched=%d, want 1/0/1: another tenant's runs were examined in A's window", res.Scanned, res.Unreadable, res.Matched)
			}
			// And a window of exactly A's one run is complete: B's three runs must not make it "exhausted".
			one, err := a.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}, 1)
			if err != nil || one.Exhausted || one.Matched != 1 {
				t.Errorf("a window of 1 over a tenant with 1 run = %+v (err=%v), want complete: another tenant's runs were counted as older runs", one, err)
			}
		})
	}
}

// A run whose state is sealed and will not open is counted and reported. Dropping it silently would answer
// "no such run" for a run the search could not read.
func TestARunWhoseStateWillNotOpenIsCountedNotSilentlyDropped(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	store := sealedStore(t, db, DefaultTenantUUID)
	deployFaultTestDef(t, store)
	ctx := context.Background()

	good := publishRunAs(t, store, DefaultTenantUUID, "test", "u-good", map[string]string{"order_id": "123"})
	bad := publishRunAs(t, store, DefaultTenantUUID, "test", "u-bad", map[string]string{"order_id": "123"})
	onDiskIsSealed(t, db, bad)
	// A sealed-form value (a JSON string of base64) that is not this store's ciphertext.
	if _, err := db.Exec(`UPDATE workflow_payloads SET query_state = to_jsonb(md5('a') || md5('b')) WHERE id = $1`, bad); err != nil {
		t.Fatalf("UNMEASURED: could not damage a sealed value: %v", err)
	}

	res, err := store.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}, 100)
	if err != nil {
		t.Fatalf("one unreadable run must not fail the search: %v", err)
	}
	if got := publishedIDs(res.Runs); !reflect.DeepEqual(got, []string{good}) {
		t.Errorf("runs = %v, want only the readable match %v", got, []string{good})
	}
	if res.Unreadable != 1 || res.Scanned != 2 {
		t.Errorf("unreadable=%d scanned=%d, want 1 and 2: the run that would not open was dropped without being counted", res.Unreadable, res.Scanned)
	}
}

// Runs written before the flag was turned on are plaintext objects. The sealing store reads them as they
// are, so a deployment that began sealing part-way through its history is searched across the seam.
func TestAWindowSearchReadsRunsWrittenBeforeSealingWasTurnedOn(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	plain := NewPostgresStore(db)
	deployFaultTestDef(t, plain)
	sealed := sealedStore(t, db, DefaultTenantUUID)
	ctx := context.Background()

	before := publishRunAs(t, plain, DefaultTenantUUID, "test", "m-before", map[string]string{"order_id": "123"})
	after := publishRunAs(t, sealed, DefaultTenantUUID, "test", "m-after", map[string]string{"order_id": "123"})
	onDiskIsSealed(t, db, after)

	res, err := sealed.SearchQueryStateWindow(ctx, WorkflowFilter{QueryState: map[string]string{"order_id": "123"}, Limit: 100}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := publishedIDs(res.Runs); !reflect.DeepEqual(got, sortedCopy([]string{before, after})) {
		t.Errorf("found %v, want both the plaintext run %s and the sealed run %s", got, before, after)
	}
	if res.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0: a plaintext run is not a failure", res.Unreadable)
	}
}

func TestAWindowSearchRefusesWhatItCannotAnswer(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() { testutil.CleanupPostgresTestData(t, db) })
	store := sealedStore(t, db, DefaultTenantUUID)
	ctx := context.Background()
	one := map[string]string{"a": "1"}

	for _, c := range []struct {
		name   string
		filter WorkflowFilter
		window int
		want   error
	}{
		{"a zero window", WorkflowFilter{QueryState: one}, 0, ErrQueryStateWindowInvalid},
		{"a negative window", WorkflowFilter{QueryState: one}, -5, ErrQueryStateWindowInvalid},
		{"no pair", WorkflowFilter{}, 10, ErrQueryStateWindowInvalid},
		{"five pairs", WorkflowFilter{QueryState: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}}, 10, ErrQueryStateFilterTooManyPairs},
		{"a payload substring search under sealing", WorkflowFilter{QueryState: one, InputContains: "x"}, 10, ErrSearchUnavailableUnderEncryption},
	} {
		if _, err := store.SearchQueryStateWindow(ctx, c.filter, c.window); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func publishedIDs2(wfs []WorkflowInstance) []string {
	out := make([]string, 0, len(wfs))
	for _, w := range wfs {
		out = append(out, w.ID)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// The handler finds the window search by asserting an interface on whatever the store factory hands back. A
// factory that returned a wrapper, or any type without the methods, would leave every sealing deployment on
// part 1's 422 while every handler test (which supplies its own store) stayed green. So this asks the real
// factory, built the way cmd/cleat-worker builds it.
func TestTheStoreTheFactoryOpensCanSearchAWindowExactlyWhenItSeals(t *testing.T) {
	db := testDB(t)
	t.Cleanup(func() { db.Close() })
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, c := range []struct {
		name    string
		factory *PostgresStoreFactory
		want    bool
	}{
		{"with --encrypt-sensitive-payloads", NewPostgresStoreFactory(db, "public").WithEncryption(enc, true), true},
		{"without it", NewPostgresStoreFactory(db, "public"), false},
	} {
		st, lease, err := c.factory.OpenStore(ctx, DefaultTenantUUID)
		if err != nil {
			t.Fatalf("%s: OpenStore: %v", c.name, err)
		}
		lease.Close()
		ws, ok := st.(QueryStateWindowSearcher)
		if !ok {
			t.Fatalf("%s: the opened store is a %T, which cannot search a window: a sealing deployment would be refused forever", c.name, st)
		}
		if ws.SealsQueryState() != c.want {
			t.Errorf("%s: SealsQueryState() = %v, want %v", c.name, ws.SealsQueryState(), c.want)
		}
	}
}
