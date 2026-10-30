package engine

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3597, after owner decision D55: publishing a library bundle to a SHARDED deployment writes
// it to every shard, the first being the admin shard, with the discipline definitions already
// follow (engine/sharded_store_defs.go): admin shard first, a failure there stops everything,
// every write idempotent, the error naming what was written, what failed and what was not tried,
// and RUNNING THE SAME PUBLISH AGAIN as the repair.
//
// The shards are three REAL databases of one dialect, created for the test, each with the shipped
// schema and each reached through its own publisher login, the way an operator would reach them.
// A fixture that called TestDB three times would hold one database under three names, and none of
// this would be measured. The partial failures are real ones: a shard whose login is not a member
// of cleat_library_publisher refuses the write with the database's own "permission denied".

type libraryShardFixture struct {
	owner  [3]*sql.DB // the superuser connection, for counting rows
	pub    [3]*sql.DB // the publisher login of each shard
	reader [3]*sql.DB // a login that is NOT a publisher, on each shard
}

func newLibraryShardFixture(t *testing.T, tc publishDialect) *libraryShardFixture {
	t.Helper()
	f := &libraryShardFixture{}
	for i, prefix := range []string{"libshard_a", "libshard_b", "libshard_c"} {
		f.owner[i] = testutil.ScratchDatabase(t, tc.tdialect, prefix)
		f.pub[i] = testutil.OpenLibraryLoginDB(t, f.owner[i], tc.tdialect, true)
		f.reader[i] = testutil.OpenLibraryLoginDB(t, f.owner[i], tc.tdialect, false)
	}
	return f
}

// shards builds the shard list from one connection per shard, in shard order.
func shardList(conns ...*sql.DB) []LibraryShard {
	names := []string{"shard-1", "shard-2", "shard-3"}
	out := make([]LibraryShard, len(conns))
	for i, c := range conns {
		out[i] = LibraryShard{Name: names[i], DB: c}
	}
	return out
}

func shardedReq(lib string) PublishLibraryRequest {
	return PublishLibraryRequest{Library: lib, BundleVersion: "1.0.0", PublishedBy: "operator-x", Members: []PublishLibraryMember{
		pubMember("parent", 1, "parent-bytes", map[string]int{"child": 2}),
		pubMember("child", 2, "child-bytes", nil),
	}}
}

func TestABundleIsPublishedToEveryShardAndAReRunIsTheRepair(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			f := newLibraryShardFixture(t, tc)
			ctx := context.Background()
			rows := func(i int, lib string) int { return countLibraryRows(t, f.owner[i], tc.edialect, lib) }

			t.Run("a bundle reaches every shard, and publishing it again is a no-op everywhere", func(t *testing.T) {
				lib := uniqueLibrary("shardall")
				res, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
				if err != nil {
					t.Fatalf("publish to three shards: %v", err)
				}
				if len(res.Shards) != 3 {
					t.Fatalf("got results for %d shard(s), want 3", len(res.Shards))
				}
				for i, s := range res.Shards {
					if s.Shard != shardList(f.pub[0], f.pub[1], f.pub[2])[i].Name || s.MembersWritten != 2 || s.BundleAlreadyPublished {
						t.Errorf("shard %d: %+v, want its own name, 2 members written and a new bundle", i+1, s)
					}
					if n := rows(i, lib); n != 5 { // 2 members + 1 bundle + 2 listing rows
						t.Errorf("shard %d holds %d row(s) of the library, want 5", i+1, n)
					}
					listed, err := ListLibraryBundles(ctx, f.pub[i], tc.edialect, lib)
					if err != nil || len(listed) != 1 || len(listed[0].Members) != 2 {
						t.Errorf("shard %d lists %+v (%v), want the one bundle with its two members", i+1, listed, err)
					}
				}
				again, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
				if err != nil {
					t.Fatalf("publishing it again: %v", err)
				}
				for i, s := range again.Shards {
					if !s.BundleAlreadyPublished || s.MembersWritten != 0 {
						t.Errorf("shard %d on the second publish: %+v, want the bundle already published and nothing written", i+1, s)
					}
					if n := rows(i, lib); n != 5 {
						t.Errorf("shard %d changed to %d row(s) on an idempotent publish", i+1, n)
					}
				}
			})

			t.Run("a shard that fails does not stop the rest, and the same publish again completes it", func(t *testing.T) {
				lib := uniqueLibrary("shardpart")
				// Shard 2 is reached through a login that is not a publisher: the database refuses.
				res, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.reader[1], f.pub[2]), tc.edialect, shardedReq(lib))
				var se *ShardedLibraryPublishError
				if !errors.As(err, &se) {
					t.Fatalf("got %v, want a ShardedLibraryPublishError", err)
				}
				if got := strings.Join(se.Succeeded, ","); got != "shard-1,shard-3" {
					t.Errorf("written to [%s], want shard-1 and shard-3: a failing shard must not stop the ones after it", got)
				}
				if got := strings.Join(se.Failed, ","); got != "shard-2" || len(se.NotAttempted) != 0 {
					t.Errorf("failed on [%s], not attempted on %v; want shard-2 failed and nothing skipped", got, se.NotAttempted)
				}
				if !strings.Contains(err.Error(), "Run the same publish again") || strings.Contains(err.Error(), "NOT repaired") {
					t.Errorf("the repair text is wrong for a plain failure: %v", err)
				}
				if len(res.Shards) != 2 {
					t.Errorf("the result carries %d shard(s), want the two that were written", len(res.Shards))
				}
				for i, want := range []int{5, 0, 5} {
					if n := rows(i, lib); n != want {
						t.Errorf("after the partial failure shard %d holds %d row(s), want %d", i+1, n, want)
					}
				}

				// The repair: the same publish, with the failing shard reachable now.
				fixed, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
				if err != nil {
					t.Fatalf("the same publish again: %v", err)
				}
				if len(fixed.Shards) != 3 {
					t.Fatalf("the repair returned %d shard result(s), want 3", len(fixed.Shards))
				}
				if s := fixed.Shards[0]; !s.BundleAlreadyPublished || s.MembersWritten != 0 {
					t.Errorf("shard 1 on the repair: %+v, want it left as it was", s)
				}
				if s := fixed.Shards[1]; s.MembersWritten != 2 || s.BundleAlreadyPublished {
					t.Errorf("shard 2 on the repair: %+v, want 2 members written", s)
				}
				if s := fixed.Shards[2]; !s.BundleAlreadyPublished || s.MembersWritten != 0 {
					t.Errorf("shard 3 on the repair: %+v, want it left as it was", s)
				}
				for i := range f.owner {
					if n := rows(i, lib); n != 5 {
						t.Errorf("after the repair shard %d holds %d row(s), want 5", i+1, n)
					}
				}
			})

			t.Run("a failure on the admin shard leaves nothing changed anywhere", func(t *testing.T) {
				lib := uniqueLibrary("shardadmin")
				_, err := PublishLibraryBundleToShards(ctx, shardList(f.reader[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
				var se *ShardedLibraryPublishError
				if !errors.As(err, &se) {
					t.Fatalf("got %v, want a ShardedLibraryPublishError", err)
				}
				if len(se.Succeeded) != 0 || strings.Join(se.Failed, ",") != "shard-1" || strings.Join(se.NotAttempted, ",") != "shard-2,shard-3" {
					t.Errorf("written %v failed %v not attempted %v; want nothing written, shard-1 failed, shard-2 and shard-3 not tried",
						se.Succeeded, se.Failed, se.NotAttempted)
				}
				if !strings.Contains(err.Error(), "admin shard failed") {
					t.Errorf("the error does not say the admin shard failed: %v", err)
				}
				for i := range f.owner {
					if n := rows(i, lib); n != 0 {
						t.Errorf("shard %d holds %d row(s) after an admin-shard failure, want 0: nothing may be written first", i+1, n)
					}
				}
			})

			t.Run("a conflict is named as one, and a re-run is not offered as its repair", func(t *testing.T) {
				lib := uniqueLibrary("shardconf")
				// Shard 2 already holds DIFFERENT bytes under parent@1, from some earlier publish.
				stale := shardedReq(lib)
				stale.Members[0] = pubMember("parent", 1, "parent-STALE-bytes", map[string]int{"child": 2})
				if _, err := PublishLibraryBundle(ctx, f.pub[1], tc.edialect, stale); err != nil {
					t.Fatalf("UNMEASURED: seeding the stale member on shard 2: %v", err)
				}
				_, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1], f.pub[2]), tc.edialect, shardedReq(lib))
				var se *ShardedLibraryPublishError
				if !errors.As(err, &se) {
					t.Fatalf("got %v, want a ShardedLibraryPublishError", err)
				}
				if !errors.Is(err, ErrLibraryMemberConflict) {
					t.Errorf("the shard's own conflict is not visible through the error: %v", err)
				}
				if strings.Join(se.Failed, ",") != "shard-2" || !strings.Contains(err.Error(), "NOT repaired by running it again") {
					t.Errorf("failed %v; error %q; want shard-2 failed and the conflict called out as not repaired by a re-run", se.Failed, err)
				}
				if n := rows(0, lib); n != 5 {
					t.Errorf("shard 1 holds %d row(s), want 5: the other shards are still written", n)
				}
			})

			t.Run("a bundle that is wrong on its face is refused before any shard is touched", func(t *testing.T) {
				lib := uniqueLibrary("shardbad")
				bad := shardedReq(lib)
				bad.Members[0].ChildVersions = map[string]int{"ghost": 1}
				_, err := PublishLibraryBundleToShards(ctx, shardList(f.pub[0], f.pub[1]), tc.edialect, bad)
				if !errors.Is(err, ErrLibraryPublishRefused) {
					t.Fatalf("got %v, want a publish refusal", err)
				}
				// The request is wrong, not a shard: reporting "failed on shard-1" would send the
				// operator to a database that did nothing wrong.
				var se *ShardedLibraryPublishError
				if errors.As(err, &se) {
					t.Errorf("a refused request was reported as a shard failure: %v", err)
				}
				for i := 0; i < 2; i++ {
					if n := rows(i, lib); n != 0 {
						t.Errorf("shard %d holds %d row(s) after a refused request, want 0", i+1, n)
					}
				}
			})

			t.Run("a shard list that could not name its failures is refused", func(t *testing.T) {
				lib := uniqueLibrary("shardnames")
				for name, shards := range map[string][]LibraryShard{
					"no shard":             nil,
					"a missing connection": {{Name: "shard-1", DB: nil}},
					"an unnamed shard":     {{Name: "", DB: f.pub[0]}},
					"two shards, one name": {{Name: "same", DB: f.pub[0]}, {Name: "same", DB: f.pub[1]}},
				} {
					if _, err := PublishLibraryBundleToShards(ctx, shards, tc.edialect, shardedReq(lib)); !errors.Is(err, ErrLibraryPublishRefused) {
						t.Errorf("%s: got %v, want a publish refusal", name, err)
					}
				}
				for i := 0; i < 2; i++ {
					if n := rows(i, lib); n != 0 {
						t.Errorf("shard %d holds %d row(s) after refused shard lists, want 0", i+1, n)
					}
				}
			})
		})
	}
}
