package engine

// cleat#3775, part 2: finding a run by what it published on a deployment that SEALS query_state.
//
// With --encrypt-sensitive-payloads the column holds ciphertext, so the database cannot match a key and a
// value in it (part 1 answers 422 there rather than an empty list). The only place the plaintext exists is
// the worker, and the worker already decrypts it: GetQueryState and ListQueryState do, per run, for any
// caller who names the run. This reads the same column the same way and tests the pairs in Go, so it
// exposes nothing those two do not -- it just does it for many runs of ONE tenant at once.
//
// IT IS A WINDOW, NOT A SEARCH OF EVERYTHING, and the result says so. The scan covers the tenant's most
// recent `window` runs in the listing's own order (created_at DESC, id DESC) and stops. "No match" is
// therefore two different answers, and the caller can tell them apart only because Exhausted is part of the
// result:
//
//	Exhausted == false: every run that could match was examined; "no match" means there is no such run.
//	Exhausted == true:  the window was used up and OLDER runs were not examined; "no match" means
//	                    "not among the most recent Scanned runs", and says nothing about the rest.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// QueryStateWindowResult is the answer to a window search.
type QueryStateWindowResult struct {
	// Runs is the requested page of matches (the filter's Limit and Offset applied over Matched), in the
	// listing's order, newest first.
	Runs []WorkflowInstance
	// Matched is how many runs inside the window matched, before paging. It is what X-Total-Count reports
	// for a window search: a total OVER THE WINDOW, never over the tenant's history.
	Matched int
	// Window is the window the caller asked for.
	Window int
	// Scanned is how many runs were examined, at most Window.
	Scanned int
	// Exhausted is true when the window was used up and there were older runs that were not examined.
	Exhausted bool
	// Unreadable counts runs inside the window whose published state is sealed and would not open (a key
	// that is gone, or a damaged value). They are counted, never matched, and never silently dropped: a
	// search that skipped them without saying so would answer "no such run" for a run it could not read.
	Unreadable int
}

// QueryStateWindowSearcher is implemented by a store that can search published state in a window. A store
// that does not implement it (every dialect but PostgreSQL, and the sharded wrapper) leaves a sealing
// deployment with part 1's refusal.
type QueryStateWindowSearcher interface {
	// SealsQueryState reports whether query_state is ciphertext in this store. The handler uses the window
	// search only when it is: where the column is plaintext, the SQL filter answers exactly and without a
	// window.
	SealsQueryState() bool
	// SearchQueryStateWindow examines the tenant's `window` most recent runs matching the filter's other
	// fields, and returns those whose published state holds every pair in filter.QueryState.
	SearchQueryStateWindow(ctx context.Context, filter WorkflowFilter, window int) (*QueryStateWindowResult, error)
}

var _ QueryStateWindowSearcher = (*PostgresStore)(nil)

// ErrQueryStateWindowInvalid is returned for a window that is not positive, or a search with no pair.
var ErrQueryStateWindowInvalid = errors.New("a query-state window search needs at least one key/value pair and a positive window")

// SealsQueryState reports whether this store writes query_state as ciphertext.
func (s *PostgresStore) SealsQueryState() bool {
	return s.encryption != nil && s.encryptSensitivePayloads
}

// SearchQueryStateWindow implements QueryStateWindowSearcher.
//
// It works on an unsealed store too (decryptPayloadJSON passes a plaintext object through), which is what
// lets the SQL filter be the oracle for it in tests: the same data, searched both ways, must agree.
func (s *PostgresStore) SearchQueryStateWindow(ctx context.Context, filter WorkflowFilter, window int) (*QueryStateWindowResult, error) {
	if window <= 0 || len(filter.QueryState) == 0 {
		return nil, ErrQueryStateWindowInvalid
	}
	if err := queryStateFilterError(filter); err != nil { // the cap on pairs, shared with the SQL filter
		return nil, err
	}
	if (filter.InputContains != "" || filter.ErrorContains != "") && s.SealsQueryState() {
		return nil, ErrSearchUnavailableUnderEncryption
	}

	tx, err := s.beginTxWithRLS(ctx)
	if err != nil {
		return nil, fmt.Errorf("query-state window search: begin: %w", err)
	}
	defer tx.Rollback()

	// The window: the tenant's most recent runs under the filter's OTHER fields, in the listing's total
	// order. The state pairs are deliberately not in the SQL -- they cannot be, the column is ciphertext --
	// so they come off the filter before it reaches applyWorkflowFilters. One more row than the window is
	// read, and only to learn whether anything older exists: it is never examined.
	d := s.dialect
	scanFilter := filter
	scanFilter.QueryState = nil
	qb := NewQueryBuilder(d,
		"SELECT l.id, p.query_state FROM workflow_leases l JOIN workflow_payloads p ON l.id = p.id WHERE 1=1")
	applyWorkflowFilters(qb, d, scanFilter, "l.id")
	// Named in the SQL, as the SQL filter does (part 1): the listing is scoped by row-level security alone,
	// and a connection that bypasses it would otherwise put another tenant's runs in this window.
	qb.AddCondition("p.tenant_id = %s", s.tenantID)
	qb.AddRaw(workflowListOrder("l.id"))
	qb.AddRaw(d.limitOffset(qb.NextPos(), 0, false))
	qb.AddArgs(window + 1)

	query, args := qb.SQL()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query-state window search: %w", err)
	}
	res := &QueryStateWindowResult{Window: window}
	var matched []string
	for rows.Next() {
		var id string
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("query-state window search: scan: %w", err)
		}
		if res.Scanned == window {
			res.Exhausted = true // the extra row: older runs exist and were not examined
			break
		}
		res.Scanned++
		state, ok := s.openQueryState(raw)
		if !ok {
			res.Unreadable++
			continue
		}
		if holdsEveryPair(state, filter.QueryState) {
			matched = append(matched, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("query-state window search: %w", err)
	}

	res.Matched = len(matched)
	start, end := pageBounds(len(matched), filter.Offset, clampWorkflowListLimit(filter.Limit))
	page := matched[start:end]
	if len(page) > 0 {
		pq2 := NewQueryBuilder(d,
			"SELECT "+postgresWorkflowListJoinColumns()+" FROM workflow_leases l JOIN workflow_payloads p ON l.id = p.id WHERE 1=1")
		pq2.AddCondition("p.tenant_id = %s", s.tenantID)
		pq2.AddRaw("AND l.id = ANY(" + d.placeholder(pq2.NextPos()) + ")")
		pq2.AddArgs(pq.Array(page))
		pq2.AddRaw(workflowListOrder("l.id"))
		pageSQL, pageArgs := pq2.SQL()
		pageRows, err := tx.QueryContext(ctx, pageSQL, pageArgs...)
		if err != nil {
			return nil, fmt.Errorf("query-state window search: page: %w", err)
		}
		defer pageRows.Close()
		if res.Runs, err = s.scanListedWorkflows(ctx, pageRows); err != nil {
			return nil, err
		}
	}
	return res, tx.Commit()
}

// openQueryState decrypts and decodes one run's published state. false means it could not be read.
func (s *PostgresStore) openQueryState(raw sql.NullString) (map[string]string, bool) {
	if raw.Valid {
		opened, err := s.decryptPayloadJSON(raw.String)
		if err != nil {
			return nil, false
		}
		raw.String = opened
	}
	state, err := decodeQueryState(raw)
	if err != nil {
		return nil, false
	}
	return state, true
}

// holdsEveryPair is the SQL filter's meaning, in Go: every pair present, key and value compared exactly
// (case-sensitive), an empty value a value, and an absent key not an empty value.
func holdsEveryPair(state, pairs map[string]string) bool {
	for k, want := range pairs {
		if got, ok := state[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// pageBounds returns the slice bounds of a page over n items.
func pageBounds(n, offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > n {
		offset = n
	}
	end := offset + limit
	if end > n {
		end = n
	}
	return offset, end
}
