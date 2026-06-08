package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPostgresStore_CountStalledWorkflows_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*) FROM workflow_instances", data: [][]driver.Value{{int64(7)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountStalledWorkflows(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("CountStalledWorkflows: %v", err)
	}
	if count != 7 {
		t.Errorf("expected 7, got %d", count)
	}
}

func TestPostgresStore_CountStalledWorkflows_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*) FROM workflow_instances", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountStalledWorkflows(context.Background(), time.Minute)
	if err == nil {
		t.Fatal("expected error from QueryRowContext error")
	}
	if count != 0 {
		t.Errorf("expected count=0 on error, got %d", count)
	}
	if !strings.Contains(err.Error(), "count stalled workflows") {
		t.Errorf("expected 'count stalled workflows' in error, got: %v", err)
	}
}

func TestPostgresStore_CountEventHistoryTotal_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*) FROM event_history", data: [][]driver.Value{{int64(42)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	count, err := store.CountEventHistoryTotal(context.Background())
	if err != nil {
		t.Fatalf("CountEventHistoryTotal: %v", err)
	}
	if count != 42 {
		t.Errorf("expected 42, got %d", count)
	}
}

func TestPostgresStore_CountEventHistoryTotal_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "SELECT COUNT(*) FROM event_history", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.CountEventHistoryTotal(context.Background())
	if err == nil {
		t.Fatal("expected error from QueryRowContext error")
	}
}

func TestPostgresStore_EstimateEventHistorySize_Success(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "pg_total_relation_size", data: [][]driver.Value{{int64(8192)}}},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	size, err := store.EstimateEventHistorySize(context.Background())
	if err != nil {
		t.Fatalf("EstimateEventHistorySize: %v", err)
	}
	if size != 8192 {
		t.Errorf("expected 8192, got %d", size)
	}
}

func TestPostgresStore_EstimateEventHistorySize_QueryError(t *testing.T) {
	db := newMockDBForPostgres(t, []mockRowsResult{
		{match: "pg_total_relation_size", err: errors.New("query failed")},
	}, nil)
	defer db.Close()

	store := NewPostgresStore(db)
	_, err := store.EstimateEventHistorySize(context.Background())
	if err == nil {
		t.Fatal("expected error from QueryRowContext error")
	}
}
