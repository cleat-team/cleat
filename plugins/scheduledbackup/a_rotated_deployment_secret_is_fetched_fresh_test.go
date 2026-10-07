package scheduledbackup

import (
	"context"
	"testing"
)

// TestBackupDSNIsFetchedFreshNotCached is cleat#2246's test-gap item.
//
// backupDSN's own doc comment has said "fetched fresh on every backup
// attempt rather than cached" since cleat#1992 part 1b, but nothing
// verified it: every existing test sets one DSN value and never changes it,
// so a version of backupDSN that cached the first successful lookup would
// leave every one of them green. Rotating the secret between two calls --
// simulating an operator running `cleatctl set-deployment-secret` between
// two backup attempts -- is the only thing that can tell "fetched fresh"
// from "cached at first success" apart.
func TestBackupDSNIsFetchedFreshNotCached(t *testing.T) {
	fake := &fakeBackupDeploymentSecrets{dsn: "postgres://first"}
	p := &Plugin{deploymentSecrets: fake}
	ctx := context.Background()

	got, err := p.backupDSN(ctx)
	if err != nil {
		t.Fatalf("backupDSN (before rotation): %v", err)
	}
	if got != "postgres://first" {
		t.Fatalf("backupDSN (before rotation) = %q, want %q", got, "postgres://first")
	}

	// Rotate the secret. In production this is `cleatctl set-deployment-secret
	// --name scheduledbackup.dsn` running against the live database between
	// two of Run's attempts; here it is a direct mutation of the fake, which
	// is the same event from backupDSN's point of view -- it never knows or
	// cares which process changed the row it reads.
	fake.dsn = "postgres://second"

	got, err = p.backupDSN(ctx)
	if err != nil {
		t.Fatalf("backupDSN (after rotation): %v", err)
	}
	if got != "postgres://second" {
		t.Fatalf("backupDSN (after rotation) = %q, want %q -- backupDSN returned the "+
			"pre-rotation value, which means it is caching rather than fetching fresh",
			got, "postgres://second")
	}
}
