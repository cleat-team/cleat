package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The HTTP layer can only answer 409 if the store hands it a typed error, and
// each dialect has to recognise its own uniqueness violation to do that:
// PostgreSQL SQLSTATE 23505, MySQL 1062, SQL Server 2601/2627.
//
// A unit test at the boundary cannot check any of that -- it can only check
// what happens once the wrapping has already occurred. This drives a real
// duplicate INSERT against each backend, which is the only thing that proves
// the three detectors agree.
func TestASecondScheduleUnderOneNameIsErrScheduleExists(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			// CreateSchedule is on WorkflowStore, which every registered
			// backend implements -- no type assertion needed.
			sched := store

			sch := Schedule{
				Name:           "nightly-report",
				DefName:        "test-workflow",
				EntryPoint:     "Handle",
				CronExpression: "0 3 * * *",
				Input:          json.RawMessage(`{}`),
				Enabled:        true,
				NextRunAt:      time.Now().Add(time.Hour),
			}

			if err := sched.CreateSchedule(ctx, sch); err != nil {
				t.Fatalf("first create: %v", err)
			}

			err := sched.CreateSchedule(ctx, sch)
			if err == nil {
				t.Fatal("a second schedule under one name was accepted; name is the " +
					"PRIMARY KEY, so this should be a uniqueness violation")
			}
			if !errors.Is(err, ErrScheduleExists) {
				t.Errorf("second create returned %v\n\nwant it to wrap ErrScheduleExists. "+
					"Without that the HTTP layer cannot tell a taken name from a store "+
					"fault, and answers 500 with the driver's own text -- which is "+
					"cleat#996. Each dialect detects this while the error is still "+
					"typed; if this backend's detector does not recognise its own "+
					"uniqueness violation, that is the gap.", err)
			}
		})
	}
}
