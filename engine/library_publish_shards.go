package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Publishing a workflow library to a SHARDED deployment (cleat#3597, after owner decision D55).
//
// A library's members must sit in the same database as the tenant rows that reference them: the
// install reads a member in one statement and the foreign keys cannot cross databases. So a
// bundle is published to EVERY shard, which is the rule definitions already follow
// (engine/sharded_store_defs.go), and this follows that file's discipline rather than inventing a
// second one:
//
//	the ADMIN shard (the first, the one the worker already uses for administration) is written
//	  first, and a failure there stops everything, so it leaves nothing changed anywhere
//	every other shard is then written; one failing does not stop the rest
//	every write is idempotent, because PublishLibraryBundle is: members are content-addressed and a
//	  bundle published again with the same members is a no-op, so RUNNING THE SAME PUBLISH AGAIN
//	  IS THE REPAIR for a shard that was down or unreachable
//	the error names the shards that were written, the ones that failed and the ones not tried
//
// The admin shard is also what arbitrates two publishes of one bundle version running at once
// with DIFFERENT content. Measured (cleat-review, 30 rounds per dialect, three real shards): one
// wins everywhere, and the other fails at the first shard with a conflict and writes nowhere
// else, so every shard holds the winner's bytes. That holds because both name the same shard
// first; two publishers that disagree about which shard is first are not covered by anything
// here or measured.
//
// THERE IS NO CROSS-DATABASE TRANSACTION, so a fan-out can stop half way, and this does not
// pretend otherwise. What a conflict means is different from what a failure means: a shard that
// already holds DIFFERENT content under a member or bundle key refuses, published content is
// immutable, and running it again is refused again. That case says so, so nobody retries it.
//
// This is an operator tool and it takes the publisher login of each shard, one per database,
// because the role is granted per database. It is not given the worker's shard list: that carries
// the application role, which cannot write the library schema.

// LibraryShard is one shard's publisher connection. Name is what an error calls it.
type LibraryShard struct {
	Name string
	DB   *sql.DB
}

// ShardPublishResult is what publishing to one shard did.
type ShardPublishResult struct {
	Shard string `json:"shard"`
	PublishLibraryResult
}

// ShardedPublishResult is what a publish to every shard did, in shard order.
type ShardedPublishResult struct {
	Library       string               `json:"library"`
	BundleVersion string               `json:"bundle_version"`
	Shards        []ShardPublishResult `json:"shards"`
}

// ShardedLibraryPublishError reports a publish that did not reach every shard.
type ShardedLibraryPublishError struct {
	Library       string
	BundleVersion string
	Succeeded     []string
	Failed        []string
	NotAttempted  []string
	Errs          []error
	// Repair says what to do about it, which differs between a shard that could not be reached and
	// a shard that refused: the first is repaired by running the publish again, the second is not.
	Repair string
}

func (e *ShardedLibraryPublishError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "publishing library %q bundle %q did not reach every shard: written to [%s], failed on [%s]",
		e.Library, e.BundleVersion, strings.Join(e.Succeeded, ", "), strings.Join(e.Failed, ", "))
	if len(e.NotAttempted) > 0 {
		fmt.Fprintf(&b, ", not attempted on [%s]", strings.Join(e.NotAttempted, ", "))
	}
	b.WriteString(". ")
	b.WriteString(e.Repair)
	for _, err := range e.Errs {
		b.WriteString("; ")
		b.WriteString(err.Error())
	}
	return b.String()
}

// Unwrap lets errors.Is and errors.As see each shard's own error, so a caller can still ask whether
// one of them was a member or bundle conflict.
func (e *ShardedLibraryPublishError) Unwrap() []error { return e.Errs }

// PublishLibraryBundleToShards publishes one bundle to every shard, the first being the admin shard.
// shards must be in the worker's shard order, and every database must be on dialect d. The request
// is validated once, before any shard is touched.
func PublishLibraryBundleToShards(ctx context.Context, shards []LibraryShard, d Dialect, req PublishLibraryRequest) (ShardedPublishResult, error) {
	res := ShardedPublishResult{Library: req.Library, BundleVersion: req.BundleVersion}
	if len(shards) == 0 {
		return res, publishRefused("no shard to publish to")
	}
	seen := map[string]bool{}
	for i, sh := range shards {
		switch {
		case sh.DB == nil:
			return res, publishRefused("shard %d has no database connection", i+1)
		case sh.Name == "":
			return res, publishRefused("shard %d has no name, and an error could not say which shard it was", i+1)
		case seen[sh.Name]:
			return res, publishRefused("two shards are both named %q, so an error could not say which one failed", sh.Name)
		}
		seen[sh.Name] = true
	}
	if err := ValidatePublishLibraryRequest(req); err != nil {
		return res, err
	}

	fail := &ShardedLibraryPublishError{Library: req.Library, BundleVersion: req.BundleVersion}
	for i, sh := range shards {
		one, err := PublishLibraryBundle(ctx, sh.DB, d, req)
		if err != nil {
			fail.Failed = append(fail.Failed, sh.Name)
			fail.Errs = append(fail.Errs, fmt.Errorf("shard %q: %w", sh.Name, err))
			if i == 0 {
				// The admin shard failed: nothing was written anywhere, and the rest were not tried.
				for _, rest := range shards[1:] {
					fail.NotAttempted = append(fail.NotAttempted, rest.Name)
				}
				break
			}
			continue
		}
		fail.Succeeded = append(fail.Succeeded, sh.Name)
		res.Shards = append(res.Shards, ShardPublishResult{Shard: sh.Name, PublishLibraryResult: one})
	}
	if len(fail.Failed) == 0 {
		return res, nil
	}
	fail.Repair = shardedPublishRepair(fail)
	return res, fail
}

// shardedPublishRepair says what to do, which depends on what failed.
func shardedPublishRepair(e *ShardedLibraryPublishError) string {
	conflict := false
	for _, err := range e.Errs {
		if errors.Is(err, ErrLibraryMemberConflict) || errors.Is(err, ErrLibraryBundleConflict) {
			conflict = true
		}
	}
	var b strings.Builder
	if len(e.Succeeded) == 0 {
		b.WriteString("The admin shard failed, so no shard was written and none of the others was tried. ")
	}
	b.WriteString("Run the same publish again: every shard's write is idempotent, so a shard that already holds " +
		"the bundle is left as it is and the shards that failed are completed.")
	if conflict {
		b.WriteString(" A conflict is NOT repaired by running it again: that shard already holds different content " +
			"under the same key, and published content is immutable. Publish the changed build under a new version.")
	}
	return b.String()
}
