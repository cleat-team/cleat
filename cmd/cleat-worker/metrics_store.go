package main

import (
	"context"
	"time"
)

// MetricsStore defines optional database methods used for observability metrics.
// Workers check whether the store implements this interface at runtime; if not,
// the corresponding metrics are simply unavailable.
type MetricsStore interface {
	CountStalledWorkflows(ctx context.Context, threshold time.Duration) (int, error)
	CountEventHistoryTotal(ctx context.Context) (int, error)
	EstimateEventHistorySize(ctx context.Context) (int64, error)
	CountActiveConcurrencyKeys(ctx context.Context) (int, error)
}
