package main

// cleat#3853: the worker-local skip set. A run whose sealed compaction state this
// worker cannot open is remembered for compactionSkipWindow and left out of the
// batch, so it stops consuming one of the ten slots every tick.
//
// The end-to-end proof -- ten poisoned runs and one fresh one, against a real
// PostgreSQL -- is ten_unopenable_compaction_states_do_not_starve_a_fresh_run_test.go.
// These are the parts a real key ring cannot vary cheaply: WHICH error is
// remembered, expiry, the tenant in the key, and the metrics.

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// boundedProbe is a store whose candidate query honours the limit it is given,
// the way the SQL does, and whose state load fails per workflow id. A probe that
// ignored the limit could not tell an over-fetch from a batch that was always
// going to contain the fresh run.
type boundedProbe struct {
	*mockStore
	mu        sync.Mutex
	limits    []int
	compacted []string
	attempts  []string
	cands     []string
}

func newBoundedProbe(cands []string, loadState func(id string) error) *boundedProbe {
	p := &boundedProbe{cands: cands}
	p.mockStore = &mockStore{}
	p.mockStore.getCompactionCandidatesFn = func(_ context.Context, _ int, limit int) ([]string, error) {
		p.mu.Lock()
		p.limits = append(p.limits, limit)
		p.mu.Unlock()
		n := limit
		if n > len(p.cands) {
			n = len(p.cands)
		}
		return append([]string(nil), p.cands[:n]...), nil
	}
	p.mockStore.getWorkflowByIDFn = func(_ context.Context, id string) (*engine.WorkflowInstance, error) {
		return &engine.WorkflowInstance{ID: id, DefName: "mock-wf", DefVersion: 1}, nil
	}
	p.mockStore.loadEventHistoryFn = func(context.Context, string) ([]engine.EventRecord, error) {
		events := make([]engine.EventRecord, compactTestThreshold*6)
		for i := range events {
			events[i] = engine.EventRecord{Step: i, EventType: "call"}
		}
		return events, nil
	}
	p.mockStore.loadCompactionStateFn = func(_ context.Context, id string) (*engine.CompactionState, error) {
		// Every attempt reaches here, and only a successful one reaches
		// compactHistoryFn -- which is why attempts are counted separately from
		// compactions.
		p.mu.Lock()
		p.attempts = append(p.attempts, id)
		p.mu.Unlock()
		if err := loadState(id); err != nil {
			return nil, err
		}
		return nil, nil
	}
	p.mockStore.compactHistoryFn = func(_ context.Context, id string, _ []byte, _, _ int) error {
		p.mu.Lock()
		p.compacted = append(p.compacted, id)
		p.mu.Unlock()
		return nil
	}
	return p
}

func (p *boundedProbe) snapshot() (limits []int, compacted []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.limits...), append([]string(nil), p.compacted...)
}

func (p *boundedProbe) attemptCount(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.attempts {
		if a == id {
			n++
		}
	}
	return n
}

func (p *boundedProbe) wasCompacted(id string) bool {
	_, done := p.snapshot()
	for _, d := range done {
		if d == id {
			return true
		}
	}
	return false
}

func poisonIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("poison-%d", i)
	}
	return out
}

func decryptionFailure(id string) error {
	if strings.HasPrefix(id, "poison-") {
		return fmt.Errorf("load compaction state: %w: cipher: message authentication failed", engine.ErrPayloadDecryption)
	}
	return nil
}

func newSkipTestWorker(p *boundedProbe) *Worker {
	w := newTestWorkerFromStore(p)
	w.storeTenantID = tenantSelf
	w.compactionThreshold = compactTestThreshold
	return w
}

// THE PREDICATE, pinned by its falsifier. Both halves are needed:
//
//   - a decryption failure IS remembered, so the run behind it is reached;
//   - a non-decryption failure is NOT, so a transient error cannot park a healthy
//     run for compactionSkipWindow.
//
// Replace errors.Is(err, engine.ErrPayloadDecryption) with err != nil in
// compactStoreOnce and ONLY the second test fails: the fresh run would be
// compacted on tick 2 of the negative control, which is what that test asserts
// cannot happen.
func TestARunWhoseSealedStateCannotBeOpenedIsLeftOutOfTheBatch(t *testing.T) {
	cands := append(poisonIDs(compactionCandidatesPerTick), "fresh")
	p := newBoundedProbe(cands, decryptionFailure)
	w := newSkipTestWorker(p)
	defer w.cancel()

	w.compactOnce()
	if limits, _ := p.snapshot(); len(limits) != 1 || limits[0] != compactionCandidatesPerTick {
		t.Fatalf("first tick asked for %v candidates, want [%d]: there is nothing to skip yet", limits, compactionCandidatesPerTick)
	}
	if p.wasCompacted("fresh") {
		t.Fatal("the fresh run was compacted on the first tick, when the batch is full of runs that were not yet known to be unopenable")
	}
	for _, id := range poisonIDs(compactionCandidatesPerTick) {
		if !w.compactionSkipSetFor().skipped(tenantSelf, id) {
			t.Fatalf("%s failed with a decryption error and was not remembered", id)
		}
	}

	w.compactOnce()
	if !p.wasCompacted("fresh") {
		t.Fatal("the fresh run was never compacted: ten runs whose state this worker cannot open still fill the batch on every tick")
	}
	limits, _ := p.snapshot()
	if len(limits) != 2 || limits[1] != compactionCandidatesPerTick+compactionCandidatesPerTick {
		t.Errorf("the second tick asked for %v candidates, want the batch plus the ten remembered runs", limits)
	}
}

func TestANonDecryptionFailureIsNotRemembered(t *testing.T) {
	cands := append(poisonIDs(compactionCandidatesPerTick), "fresh")
	p := newBoundedProbe(cands, func(id string) error {
		if strings.HasPrefix(id, "poison-") {
			return errors.New("load compaction state: unmarshal compaction state: invalid character 'x' looking for beginning of value")
		}
		return nil
	})
	w := newSkipTestWorker(p)
	defer w.cancel()

	w.compactOnce()
	w.compactOnce()

	if p.wasCompacted("fresh") {
		t.Fatal("the fresh run was compacted behind runs that failed for a NON-decryption reason, so a transient error was remembered " +
			"and parked healthy runs for 30 minutes (the predicate must be errors.Is on ErrPayloadDecryption, not err != nil)")
	}
	for _, id := range poisonIDs(compactionCandidatesPerTick) {
		if w.compactionSkipSetFor().skipped(tenantSelf, id) {
			t.Fatalf("%s failed with a non-decryption error and was remembered", id)
		}
	}
	if limits, _ := p.snapshot(); len(limits) != 2 || limits[1] != compactionCandidatesPerTick {
		t.Errorf("the second tick asked for %v candidates, want [%d %d]: nothing was remembered, so the batch is not widened",
			limits, compactionCandidatesPerTick, compactionCandidatesPerTick)
	}
}

// A restored key must recover without a restart: the entry expires and the run is
// tried again on the next tick. The clock is injected; nothing sleeps.
func TestASkipExpiresAndTheRunIsTriedAgain(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	p := newBoundedProbe([]string{"poison-0"}, decryptionFailure)
	w := newSkipTestWorker(p)
	w.nowFunc = func() time.Time { return now }
	defer w.cancel()

	w.compactOnce()
	if !w.compactionSkipSetFor().skipped(tenantSelf, "poison-0") {
		t.Fatal("the decryption failure was not remembered")
	}
	if got := p.attemptCount("poison-0"); got != 1 {
		t.Fatalf("attempts on the first tick = %d, want 1", got)
	}

	now = now.Add(compactionSkipWindow + time.Minute)
	if w.compactionSkipSetFor().skipped(tenantSelf, "poison-0") {
		t.Fatal("the entry is still live after the skip window, so a restored key would need a restart")
	}
	w.compactOnce()
	if got := p.attemptCount("poison-0"); got != 2 {
		t.Fatalf("attempts after the skip expired = %d, want 2: an expired entry must be retried", got)
	}
}

// boundedOwn is the worker's own store: a boundedProbe that can enumerate the
// tenants it serves, which is what a real store does through engine.TenantLister.
type boundedOwn struct {
	*boundedProbe
	tenants []string
}

func (s *boundedOwn) ListTenantIDs(context.Context) ([]string, error) { return s.tenants, nil }

// The tenant is part of the key. Two tenants can each have a workflow with the
// same id (ids are only unique within a tenant in principle), and one tenant's
// unopenable run must not suppress the other's.
func TestASkipInOneTenantDoesNotSuppressAnotherTenantsRun(t *testing.T) {
	const shared = "shared-workflow-id"
	own := &boundedOwn{
		boundedProbe: newBoundedProbe([]string{shared}, func(string) error {
			return fmt.Errorf("load compaction state: %w", engine.ErrPayloadDecryption)
		}),
		tenants: []string{tenantSelf, tenantOther},
	}
	healthyTenant := newBoundedProbe([]string{shared}, func(string) error { return nil })
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: healthyTenant}}
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	w.compactionThreshold = compactTestThreshold
	defer w.cancel()

	w.compactOnce()

	if !healthyTenant.wasCompacted(shared) {
		t.Fatal("the other tenant's run with the same id was suppressed by the worker's own tenant's skip: the key must be (tenant, workflow)")
	}
	if _, done := healthyTenant.snapshot(); len(done) != 1 {
		t.Errorf("the healthy tenant compacted %v, want [%s]", done, shared)
	}
}

// The counter and the gauge are part of the fix: a lost key must get noisier, not
// quieter. Both are read from the worker's own scrape output rather than from a
// helper, so a renamed or unwired instrument fails here.
func TestTheSkipIsCountedAndTheSkipSetSizeIsGauged(t *testing.T) {
	cands := append(poisonIDs(compactionCandidatesPerTick), "fresh")
	p := newBoundedProbe(cands, decryptionFailure)
	w := newSkipTestWorker(p)
	defer w.cancel()

	w.compactOnce()

	body := scrapeMetrics(t, w)
	if got := metricValue(t, body, `cleat_background_loops_total`, `status="sealed_skipped"`); got != compactionCandidatesPerTick {
		t.Errorf("cleat_background_loops_total{status=\"sealed_skipped\"} = %d, want %d: each new skip must be counted, and existing runs must not be re-counted",
			got, compactionCandidatesPerTick)
	}
	if got := metricValue(t, body, `cleat_background_loop_items_processed`, `loop_name="compaction_skipped"`); got != compactionCandidatesPerTick {
		t.Errorf("cleat_background_loop_items_processed{loop_name=\"compaction_skipped\"} = %d, want %d: the gauge must report how many runs are being left out",
			got, compactionCandidatesPerTick)
	}

	// A second tick must not re-count the entries it already knows. The count that
	// would grow every five minutes is the log line the skip set exists to remove.
	w.compactOnce()
	if got := metricValue(t, scrapeMetrics(t, w), `cleat_background_loops_total`, `status="sealed_skipped"`); got != compactionCandidatesPerTick {
		t.Errorf("after a second tick cleat_background_loops_total{status=\"sealed_skipped\"} = %d, want %d: only NEW entries are counted", got, compactionCandidatesPerTick)
	}
}

// The batch counter reports what was ATTEMPTED, not what the over-fetch returned.
// On the second tick with ten remembered runs the worker fetches twenty and
// attempts ten; reporting the fetched count would say the loop processed twice the
// work it did -- and on a cap-hit pass where every fetched run was skipped it
// would report thirty while nothing was compacted.
func TestTheBatchCounterReportsAttemptsNotTheOverFetch(t *testing.T) {
	fresh := make([]string, compactionCandidatesPerTick)
	for i := range fresh {
		fresh[i] = fmt.Sprintf("fresh-%d", i)
	}
	p := newBoundedProbe(append(poisonIDs(compactionCandidatesPerTick), fresh...), decryptionFailure)
	w := newSkipTestWorker(p)
	defer w.cancel()

	w.compactOnce() // all ten failures are remembered
	if got := metricValue(t, scrapeMetrics(t, w), `cleat_background_loop_items_processed`, `loop_name="compaction"`); got != compactionCandidatesPerTick {
		t.Fatalf("after tick 1 items processed = %d, want %d", got, compactionCandidatesPerTick)
	}

	w.compactOnce() // fetches twenty (batch + ten remembered), attempts ten
	limits, compacted := p.snapshot()
	if len(limits) != 2 || limits[1] <= compactionCandidatesPerTick {
		t.Fatalf("the second tick fetched %v candidates, want more than the batch so the over-fetch is real", limits)
	}
	if len(compacted) != compactionCandidatesPerTick {
		t.Fatalf("the second tick compacted %d runs, want %d", len(compacted), compactionCandidatesPerTick)
	}
	if got := metricValue(t, scrapeMetrics(t, w), `cleat_background_loop_items_processed`, `loop_name="compaction"`); got != compactionCandidatesPerTick {
		t.Errorf("items processed = %d, want %d: the counter must report attempts, not the %d runs the over-fetch returned",
			got, compactionCandidatesPerTick, limits[1])
	}
}

// The skip-set gauge covers every tenant the worker serves. It is one unlabelled
// series set once per tenant per tick, so setting it from one tenant's size would
// publish whichever tenant was visited last -- and could read zero while another
// tenant holds a full skip set.
func TestTheSkipSetGaugeCoversEveryTenant(t *testing.T) {
	own := &boundedOwn{
		boundedProbe: newBoundedProbe(poisonIDs(4), decryptionFailure),
		tenants:      []string{tenantSelf, tenantOther},
	}
	other := newBoundedProbe([]string{"other-poison-0", "other-poison-1", "other-poison-2"},
		func(string) error { return fmt.Errorf("load compaction state: %w", engine.ErrPayloadDecryption) })
	f := &perTenantFactory{stores: map[string]engine.WorkflowStore{tenantOther: other}}
	w := newTestWorkerFromStore(own)
	w.storeTenantID = tenantSelf
	w.claimAcrossTenants = true
	w.storeFactory = f
	w.compactionThreshold = compactTestThreshold
	defer w.cancel()

	w.compactOnce()

	body := scrapeMetrics(t, w)
	if got := metricValue(t, body, `cleat_background_loop_items_processed`, `loop_name="compaction_skipped"`); got != 7 {
		t.Errorf("the skip-set gauge = %d, want 7 (4 own + 3 other): a per-tenant set would report the last tenant visited", got)
	}
	if got := metricValue(t, body, `cleat_background_loops_total`, `status="sealed_skipped"`); got != 7 {
		t.Errorf("the sealed_skipped counter = %d, want 7 across both tenants", got)
	}
}

func scrapeMetrics(t *testing.T, w *Worker) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	w.Metrics.ServeHTTP().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("scrape returned %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// metricValue returns the value of the sample line for metric whose labels
// contain wantLabels, or -1 when there is no such line (so "instrument missing"
// is distinguishable from "value zero").
func metricValue(t *testing.T, body, metric, wantLabels string) int64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(metric) + `\{([^}]*)\}\s+([0-9.e+-]+)$`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], wantLabels) {
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				t.Fatalf("%s: parsing %q: %v", metric, m[2], err)
			}
			return int64(v)
		}
	}
	t.Fatalf("no sample of %s carrying %s in the scrape output; the instrument is missing or its labels changed:\n%s",
		metric, wantLabels, body)
	return -1
}

// A key reload is the moment a run that could not be opened may become openable,
// so the set is cleared on the reload's SUCCESS path -- and only there: a refused
// or reverted reload returns before the clear.
func TestASIGHUPReloadRetriesRunsWhoseCompactionStateCouldNotBeOpened(t *testing.T) {
	e := newSighupEnv(t, nil)
	ctx := context.Background()
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1}); err != nil {
		t.Fatalf("registering the worker: %v", err)
	}
	skips := e.w.compactionSkipSetFor()
	skips.remember("tenant-a", "wf-1")
	skips.remember("tenant-a", "wf-2")

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	for _, id := range []string{"wf-1", "wf-2"} {
		if skips.skipped("tenant-a", id) {
			t.Fatalf("%s is still skipped after a successful key reload: a newly available key would not be tried until the window expired", id)
		}
	}
	if !strings.Contains(e.log.String(), "compaction_skips_cleared") {
		t.Error("the reload cleared the skip set without saying so, so an operator cannot tell the runs were retried")
	}
}

// The OTHER half of the SIGHUP clear: a reload that is refused must NOT clear the
// set. The success path clears it so a newly available key is retried at once;
// the refused path must keep it, or every run this worker cannot open is retried
// on the next tick, once per failed SIGHUP -- undoing the property the skip set
// exists for, and doing it only when something is already going wrong.
//
// This is the negative half of
// TestASIGHUPReloadRetriesRunsWhoseCompactionStateCouldNotBeOpened, and it pins
// control flow that was previously asserted only by reading the function: the
// clear sits after the early return, not before it.
func TestARefusedKeyReloadKeepsTheCompactionSkips(t *testing.T) {
	v1 := engine.VersionedKey{Version: 1, Key: sighupKey(0x11)}
	v2 := engine.VersionedKey{Version: 2, Key: sighupKey(0x22)}
	e := newSighupEnv(t, sighupRing(t, v1))
	ctx := context.Background()

	// A secret still sealed under v1 makes the reload to v2-only REFUSE (the same
	// fixture as TestReloadKeyRingsOnSIGHUP_SecretsRefusedWhenACandidateCannotOpenAStoredSecret).
	const refusalSecretName = "cleat-3853-sighup-refusal.key"
	if err := e.w.deploymentSecrets.PutDeploymentSecret(ctx, refusalSecretName, "value"); err != nil {
		t.Fatalf("seed a deployment secret under v1: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM deployment_secrets WHERE name = $1`, refusalSecretName) })
	if err := e.reg.Register(ctx, engine.WorkerRegistration{WorkerID: e.id, Hostname: "h", PID: 1, SecretKeyVersions: []int{1}}); err != nil {
		t.Fatal(err)
	}

	skips := e.w.compactionSkipSetFor()
	skips.remember("tenant-a", "wf-1")

	t.Setenv("CLEAT_SECRET_MASTER_KEY", mustB64(v2.Key))
	t.Setenv("CLEAT_SECRET_MASTER_KEY_VERSION", "2")

	e.w.reloadKeyRingsOnSIGHUP(ctx)

	// PRECONDITION: the reload really was refused, so the assertion below is not
	// passing because nothing happened for an unrelated reason.
	if got := e.w.secretsRing.Load().Current().Version; got != 1 {
		t.Fatalf("the reload was not refused (secrets ring is on version %d): this test would then be measuring the success path", got)
	}
	if !skips.skipped("tenant-a", "wf-1") {
		t.Fatal("a REFUSED key reload cleared the compaction skip set: every run this worker cannot open would be retried on the next " +
			"compaction tick, once per failed SIGHUP -- the clear must sit on the success path, after the refused reload returns")
	}
}
