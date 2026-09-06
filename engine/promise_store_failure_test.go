package engine

import (
	"context"
	"errors"
	"testing"
)

// failingPromiseStore fails CreatePromise and nothing else, so the test
// exercises the one path under examination rather than a store that is broken
// everywhere.
type failingPromiseStore struct{}

func (f *failingPromiseStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	return errors.New("store is down")
}
func (f *failingPromiseStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	return nil
}
func (f *failingPromiseStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	return nil
}
func (f *failingPromiseStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	return "", "", "", errors.New("no such promise")
}

// TestCreatePromiseReportsAStoreFailure asserts that a promise the store did
// not accept is reported to the guest rather than returned as success.
//
// It used to be logged and swallowed, and the consequence was a HANG, not a
// disagreement. The event record written just above the store call already
// asserts the promise exists; the guest received an ID and errCode 0 and
// carried on; and the AwaitPromise that follows calls GetPromise, finds
// nothing, falls past both the resolved and the rejected branch, and SUSPENDS.
// It then waits for a promise no external caller can resolve, because the row
// they would resolve against was never written. The log line was the only
// trace. IMPROVEMENT-PLAN 3.218.
//
// The ABI always had somewhere to put this: cleat_create_promise returns
// errCode in bits 0-31 (ABI.md 2.34). The failure was not unreportable, it was
// unreported.
func TestCreatePromiseReportsAStoreFailure(t *testing.T) {
	s := newTestExecSession()
	s.engine.promiseStore = &failingPromiseStore{}

	// A nil module means writeResult is a no-op, so only the errCode half of
	// the packed result is meaningful here -- which is the half under test.
	got := s.CreatePromise(context.Background(), nil, "approval", 0, 0)

	if errCode := uint32(got); errCode == 0 {
		t.Fatalf("CreatePromise returned errCode 0 after the store refused the write.\n\n"+
			"The guest now holds an ID for a promise that does not exist, and the "+
			"AwaitPromise that follows will suspend forever: GetPromise finds nothing, "+
			"so neither the resolved nor the rejected branch is taken and the await "+
			"records and suspends. Nothing external can resolve a row never written.")
	}
}
