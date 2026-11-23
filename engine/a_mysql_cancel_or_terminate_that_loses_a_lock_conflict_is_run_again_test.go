package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// cleat#4340. On MySQL a cancel or a terminate addressed to a continued run can lose a lock conflict to a racing continue-as-new (error 1213, or 1205), and InnoDB has rolled the victim
// back entirely. It is run again, bounded. Measured on the race test before this existed: one of four racing cancels was handed the raw 1213 in one run of six. The race test itself
// proves the outcome under real contention; this pins the retry's own rules, which a real run cannot hold still: what it retries, what it does not, and where it stops.
func TestAMySQLVerbThatLosesALockConflictIsRunAgainButOnlyForThatAndOnlyEightTimes(t *testing.T) {
	s := &MySQLStore{}
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}
	timeout := &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}

	for _, tc := range []struct {
		name      string
		errs      []error // returned by successive attempts; the last repeats
		wantCalls int
		wantErr   bool
	}{
		{"succeeds first time", []error{nil}, 1, false},
		{"one deadlock then success", []error{deadlock, nil}, 2, false},
		{"one lock wait timeout then success", []error{timeout, nil}, 2, false},
		{"seven losses then success", []error{deadlock, deadlock, deadlock, deadlock, deadlock, deadlock, deadlock, nil}, 8, false},
		{"eight losses give up and say so", []error{deadlock}, 8, true},
		{"another MySQL error is not retried", []error{&mysql.MySQLError{Number: 1062, Message: "duplicate"}}, 1, true},
		{"an ordinary error is not retried", []error{ErrAdminStateConflict}, 1, true},
		{"not found is not retried", []error{ErrWorkflowNotFound}, 1, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := s.retryingLockConflict("cancelled workflow", func() error {
				e := tc.errs[len(tc.errs)-1]
				if calls < len(tc.errs) {
					e = tc.errs[calls]
				}
				calls++
				return e
			})
			if calls != tc.wantCalls {
				t.Errorf("ran %d time(s), want %d", calls, tc.wantCalls)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error=%v", err, tc.wantErr)
			}
			if err != nil && tc.wantCalls == 8 && !isDeadlockError(err) {
				t.Errorf("giving up must keep the cause visible to errors.As, got %v", err)
			}
			if err != nil && tc.wantCalls == 1 && !errors.Is(err, tc.errs[0]) && fmt.Sprint(err) != fmt.Sprint(tc.errs[0]) {
				t.Errorf("a non-retried error must come back as itself, got %v want %v", err, tc.errs[0])
			}
		})
	}
}
