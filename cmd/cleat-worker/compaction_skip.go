package main

import (
	"sync"
	"time"
)

// cleat#3853: a compaction candidate whose sealed state this worker cannot open
// is offered again on every tick and fails on every tick. The candidate query
// orders by event count and takes a bounded batch, so enough such runs fill the
// batch and no other run is compacted at all.
//
// The store cannot fix this: PostgreSQL cannot tell which key the worker holds,
// and an exclusion list written by one worker into the database would suppress
// the run for the whole fleet, including a worker that holds the right key. So
// the memory is per worker, per run, and exists only here.
//
// THE BOUND, stated rather than implied: this reduces starvation, it does not
// eliminate it. Past compactionSkipBatchCap/compactionCandidatesPerTick times
// (thirty) simultaneously unopenable runs in one tenant's store, every fetched
// candidate is skipped and the batch is still full of them. That case is counted
// separately (the "skip_cap_hit" status), because it is the only signal that the
// escalation -- a per-call exclusion list on GetCompactionCandidates -- is needed.

// compactionSkipWindow is how long a run whose sealed compaction state this
// worker could not open stays out of the compaction batch.
//
// Thirty minutes is six ticks at the default --compaction-interval of 5m: long
// enough that a persistently unopenable run costs six attempts an hour instead of
// twelve, short enough that restoring the key to the ring recovers without a
// restart. A restart clears the set.
const compactionSkipWindow = 30 * time.Minute

// compactionSkipBatchCap hard-caps the over-fetch: the worker asks for at most
// this many candidates so that remembered runs cannot make one query unbounded.
//
// Three times the per-tick batch is the largest over-fetch that still leaves a
// full batch of live candidates when a third of the fetched rows are skipped.
const compactionSkipBatchCap = 3 * compactionCandidatesPerTick

// compactionSkipKey identifies a run for the skip set.
//
// THE TENANT IS PART OF THE KEY, and that is load-bearing. compactOnce runs
// compactStoreOnce once per store -- its own, then every other tenant's through
// sweepOtherTenants -- and workflow ids are only unique within a tenant in
// principle, even though this deployment mints UUIDs. Keying on the id alone
// would let one tenant's unopenable run suppress another tenant's candidate, and
// would leave the over-fetch with no per-tenant bound.
type compactionSkipKey struct {
	tenant   string
	workflow string
}

// compactionSkipSet remembers runs this worker could not open, with an expiry.
//
// The sweep is SERIAL today: compactOnce calls compactStoreOnce once per store,
// one tenant at a time (forEachOtherTenantStore). The mutex is for the map's own
// safety and for a future concurrent sweep, not because callers contend now.
type compactionSkipSet struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[compactionSkipKey]time.Time // key -> when the skip expires
}

func newCompactionSkipSet(now func() time.Time) *compactionSkipSet {
	if now == nil {
		now = time.Now
	}
	return &compactionSkipSet{now: now, entries: make(map[compactionSkipKey]time.Time)}
}

// remember records that this worker cannot open the run's state, and reports
// whether the entry is new (so the caller logs and counts it once, not every
// tick, which would be the log line the skip set exists to remove).
func (s *compactionSkipSet) remember(tenant, workflow string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := compactionSkipKey{tenant: tenant, workflow: workflow}
	now := s.now()
	if exp, ok := s.entries[k]; ok && now.Before(exp) {
		return false
	}
	s.entries[k] = now.Add(compactionSkipWindow)
	return true
}

// skipped reports whether the run is still being left out, and drops an expired
// entry on the way, so a restored key is retried without waiting for a prune.
func (s *compactionSkipSet) skipped(tenant, workflow string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := compactionSkipKey{tenant: tenant, workflow: workflow}
	exp, ok := s.entries[k]
	if !ok {
		return false
	}
	if !s.now().Before(exp) {
		delete(s.entries, k)
		return false
	}
	return true
}

// prune drops every expired entry.
func (s *compactionSkipSet) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.entries {
		if !now.Before(exp) {
			delete(s.entries, k)
		}
	}
}

// size reports how many runs of one tenant are currently being left out. It is
// what the over-fetch adds to the batch limit and what the gauge reports.
func (s *compactionSkipSet) size(tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for k, exp := range s.entries {
		if k.tenant == tenant && now.Before(exp) {
			n++
		}
	}
	return n
}

// compactionSkipSetFor returns this worker's set, creating it on first use so
// every construction path (main, tests, a resumed worker) has one without each
// having to remember to build it.
func (w *Worker) compactionSkipSetFor() *compactionSkipSet {
	w.compactionSkipsOnce.Do(func() {
		w.compactionSkips = newCompactionSkipSet(w.nowFunc)
	})
	return w.compactionSkips
}
