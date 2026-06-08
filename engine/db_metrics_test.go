package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

func newMetricsStore(t *testing.T, rows []mockRowsResult) *PostgresStore {
	t.Helper()
	db := newMockDBForPostgres(t, rows, nil)
	return &PostgresStore{db: db, tenantID: "test-tenant"}
}

func TestPostgresStore_CountStalledWorkflows_Success(t *testing.T) {
	store := newMetricsStore(t, []mockRowsResult{
		{match: "COUNT(*) FROM workflow_instances", data: [][]driver.Value{{int64(5)}}},
	})

	count, err := store.CountStalledWorkflows(context.Background(), 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 5 {
		t.Errorf("got %d, want 5", count)
	}
}

func TestPostgresStore_CountStalledWorkflows_Error(t *testing.T) {
	queryErr := errors.New("connection refused")
	store := newMetricsStore(t, []mockRowsResult{
		{match: "COUNT(*) FROM workflow_instances", err: queryErr},
	})

	_, err := store.CountStalledWorkflows(context.Background(), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPostgresStore_CountEventHistoryTotal_Success(t *testing.T) {
	store := newMetricsStore(t, []mockRowsResult{
		{match: "COUNT(*) FROM event_history", data: [][]driver.Value{{int64(42)}}},
	})

	count, err := store.CountEventHistoryTotal(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 42 {
		t.Errorf("got %d, want 42", count)
	}
}

func TestPostgresStore_CountEventHistoryTotal_Error(t *testing.T) {
	store := newMetricsStore(t, []mockRowsResult{
		{match: "COUNT(*) FROM event_history", err: errors.New("timeout")},
	})

	_, err := store.CountEventHistoryTotal(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPostgresStore_EstimateEventHistorySize_Success(t *testing.T) {
	store := newMetricsStore(t, []mockRowsResult{
		{match: "pg_total_relation_size", data: [][]driver.Value{{int64(1024000)}}},
	})

	size, err := store.EstimateEventHistorySize(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 1024000 {
		t.Errorf("got %d, want 1024000", size)
	}
}

func TestPostgresStore_EstimateEventHistorySize_Error(t *testing.T) {
	store := newMetricsStore(t, []mockRowsResult{
		{match: "pg_total_relation_size", err: errors.New("no such table")},
	})

	_, err := store.EstimateEventHistorySize(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
