package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3597: publishing a library bundle, through a real login that holds
// cleat_library_publisher and nothing else, on PostgreSQL and SQL Server.
//
// The connection is the point. The superuser can write anywhere, so a publish that passed
// through it would not show that the statements need only SELECT and INSERT, which is all the
// publisher role is granted: PostgreSQL's row-locking reads need UPDATE, and an upsert does
// too. A login with no membership is the negative control for the same grants.

type publishDialect struct {
	name     string
	tdialect testutil.Dialect
	edialect Dialect
}

var publishDialects = []publishDialect{
	{"postgres", testutil.DialectPostgres, DialectPostgres},
	{"mssql", testutil.DialectMSSQL, DialectMSSQL},
}

func pubMember(name string, version int, body string, children map[string]int) PublishLibraryMember {
	return PublishLibraryMember{
		Name: name, Version: version, WasmBytes: []byte(body), ABIVersion: 1, MinVersion: 1,
		PluginDeps: map[string]string{"slack-notify": ">=1.0.0"}, ChildVersions: children,
		EntryPoints: []string{"handle_" + name}, Exposure: ExposureAuth,
	}
}

func uniqueLibrary(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000_000)
}

func TestTheValidationRefusesABundleThatIsWrongOnItsFace(t *testing.T) {
	ok := func() PublishLibraryRequest {
		return PublishLibraryRequest{Library: "acme", BundleVersion: "1.0.0", Members: []PublishLibraryMember{
			pubMember("parent", 1, "p", map[string]int{"child": 2}),
			pubMember("child", 2, "c", nil),
		}}
	}
	if err := ValidatePublishLibraryRequest(ok()); err != nil {
		t.Fatalf("a valid bundle was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*PublishLibraryRequest)
		want   string
	}{
		{"a pin names a member the bundle does not have", func(r *PublishLibraryRequest) { r.Members[0].ChildVersions = map[string]int{"ghost": 1} }, `no member named "ghost"`},
		{"a pin names the wrong version", func(r *PublishLibraryRequest) { r.Members[0].ChildVersions = map[string]int{"child": 9} }, `the bundle's "child" is version 2`},
		{"a name appears twice", func(r *PublishLibraryRequest) { r.Members = append(r.Members, pubMember("child", 3, "c3", nil)) }, `appears twice`},
		{"an empty bundle", func(r *PublishLibraryRequest) { r.Members = nil }, `at least one member`},
		{"a member with no bytes", func(r *PublishLibraryRequest) { r.Members[1].WasmBytes = nil }, `has no bytes`},
		{"a stamp with no version", func(r *PublishLibraryRequest) { r.Members[1].Version = 0 }, `workflow_version of at least 1`},
		{"a name with a slash", func(r *PublishLibraryRequest) { r.Members[1].Name = "a/b" }, `is not valid`},
		{"a library with a space", func(r *PublishLibraryRequest) { r.Library = "my lib" }, `not a valid name`},
		{"an unknown exposure", func(r *PublishLibraryRequest) { r.Members[1].Exposure = "world" }, `not one of auth, internal or public`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ok()
			tc.mutate(&r)
			err := ValidatePublishLibraryRequest(r)
			if !errors.Is(err, ErrLibraryPublishRefused) || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestPublishingOnMySQLIsRefusedAsUnsupported(t *testing.T) {
	req := PublishLibraryRequest{Library: "acme", BundleVersion: "1", Members: []PublishLibraryMember{pubMember("a", 1, "a", nil)}}
	if _, err := PublishLibraryBundle(context.Background(), nil, DialectMySQL, req); !errors.Is(err, ErrLibrariesUnsupported) {
		t.Fatalf("publish on MySQL: %v, want ErrLibrariesUnsupported", err)
	}
	if _, err := ListLibraryBundles(context.Background(), nil, DialectMySQL, ""); !errors.Is(err, ErrLibrariesUnsupported) {
		t.Fatalf("list on MySQL: %v, want ErrLibrariesUnsupported", err)
	}
}

func TestABundleIsPublishedByTheOperatorsRoleAndNoOneElses(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			pub := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, true)
			reader := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, false)
			ctx := context.Background()
			lib := uniqueLibrary("pub")
			cleanupLibrary(t, owner, tc.edialect, lib)

			req := PublishLibraryRequest{Library: lib, BundleVersion: "1.0.0", PublishedBy: "operator-x", Members: []PublishLibraryMember{
				pubMember("parent", 1, "parent-bytes", map[string]int{"child": 2}),
				pubMember("child", 2, "child-bytes", nil),
			}}

			// The negative control first: a login that is not the publisher writes nothing.
			if _, err := PublishLibraryBundle(ctx, reader, tc.edialect, req); err == nil {
				t.Fatal("a login without cleat_library_publisher published a bundle")
			}
			if n := countLibraryRows(t, owner, tc.edialect, lib); n != 0 {
				t.Fatalf("the refused publish left %d row(s) behind", n)
			}

			res, err := PublishLibraryBundle(ctx, pub, tc.edialect, req)
			if err != nil {
				t.Fatalf("publish as the publisher role: %v", err)
			}
			if res.MembersWritten != 2 || res.MembersAlreadyPublished != 0 || res.BundleAlreadyPublished {
				t.Fatalf("first publish: %+v, want 2 written, 0 already, bundle new", res)
			}
			if n := countLibraryRows(t, owner, tc.edialect, lib); n != 5 { // 2 members + 1 bundle + 2 listing rows
				t.Fatalf("after publish the library has %d row(s) across the three tables, want 5", n)
			}

			// Running the same publish again is a no-op, not an error: it is how a stopped publish is repaired.
			again, err := PublishLibraryBundle(ctx, pub, tc.edialect, req)
			if err != nil {
				t.Fatalf("publishing the identical bundle again: %v", err)
			}
			if !again.BundleAlreadyPublished || again.MembersWritten != 0 || again.MembersAlreadyPublished != 2 {
				t.Fatalf("second publish: %+v, want the bundle already published and nothing written", again)
			}
			if n := countLibraryRows(t, owner, tc.edialect, lib); n != 5 {
				t.Fatalf("an idempotent publish changed the row count to %d", n)
			}
		})
	}
}

func TestAPublishedMemberAndBundleAreImmutable(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			pub := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, true)
			ctx := context.Background()
			lib := uniqueLibrary("imm")
			cleanupLibrary(t, owner, tc.edialect, lib)

			base := PublishLibraryRequest{Library: lib, BundleVersion: "1", Members: []PublishLibraryMember{
				pubMember("a", 1, "a-bytes", nil), pubMember("b", 1, "b-bytes", nil),
			}}
			if _, err := PublishLibraryBundle(ctx, pub, tc.edialect, base); err != nil {
				t.Fatalf("first publish: %v", err)
			}
			before := countLibraryRows(t, owner, tc.edialect, lib)

			// Different bytes under an existing (library, name, version): refused, naming both digests,
			// and NOTHING from the request is written, including the new member listed before it.
			changed := PublishLibraryRequest{Library: lib, BundleVersion: "2", Members: []PublishLibraryMember{
				pubMember("fresh", 1, "fresh-bytes", nil), pubMember("a", 1, "a-CHANGED", nil),
			}}
			_, err := PublishLibraryBundle(ctx, pub, tc.edialect, changed)
			var mc *LibraryMemberConflictError
			if !errors.As(err, &mc) || !errors.Is(err, ErrLibraryMemberConflict) {
				t.Fatalf("different bytes under a published member: %v, want a LibraryMemberConflictError", err)
			}
			if mc.Name != "a" || mc.Stored == mc.Supplied || len(mc.Stored) != 64 {
				t.Fatalf("the conflict does not name the member and two different digests: %+v", mc)
			}
			if after := countLibraryRows(t, owner, tc.edialect, lib); after != before {
				t.Fatalf("a refused publish left rows behind: %d before, %d after (the fresh member must be rolled back with it)", before, after)
			}

			// The same bundle version with a different member set: refused, and nothing is written.
			other := PublishLibraryRequest{Library: lib, BundleVersion: "1", Members: []PublishLibraryMember{
				pubMember("a", 1, "a-bytes", nil), pubMember("c", 1, "c-bytes", nil),
			}}
			_, err = PublishLibraryBundle(ctx, pub, tc.edialect, other)
			var bc *LibraryBundleConflictError
			if !errors.As(err, &bc) || !errors.Is(err, ErrLibraryBundleConflict) {
				t.Fatalf("a different member set under a published bundle version: %v, want a LibraryBundleConflictError", err)
			}
			joined := strings.Join(bc.Differences, "; ")
			if !strings.Contains(joined, "c@1") || !strings.Contains(joined, "b@1") {
				t.Fatalf("the conflict does not say what differs (c@1 supplied, b@1 stored): %v", bc.Differences)
			}
			if after := countLibraryRows(t, owner, tc.edialect, lib); after != before {
				t.Fatalf("a refused bundle left rows behind: %d before, %d after (member c must be rolled back)", before, after)
			}
		})
	}
}

func TestASecondBundleSharesTheMembersThatDidNotChange(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			pub := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, true)
			ctx := context.Background()
			lib := uniqueLibrary("share")
			cleanupLibrary(t, owner, tc.edialect, lib)

			v1 := PublishLibraryRequest{Library: lib, BundleVersion: "1", Members: []PublishLibraryMember{pubMember("a", 1, "a1", nil), pubMember("b", 1, "b1", nil)}}
			v2 := PublishLibraryRequest{Library: lib, BundleVersion: "2", Members: []PublishLibraryMember{pubMember("a", 1, "a1", nil), pubMember("b", 2, "b2", nil)}}
			if _, err := PublishLibraryBundle(ctx, pub, tc.edialect, v1); err != nil {
				t.Fatalf("bundle 1: %v", err)
			}
			res, err := PublishLibraryBundle(ctx, pub, tc.edialect, v2)
			if err != nil {
				t.Fatalf("bundle 2: %v", err)
			}
			if res.MembersWritten != 1 || res.MembersAlreadyPublished != 1 {
				t.Fatalf("bundle 2: %+v, want a@1 shared (already published) and only b@2 written", res)
			}
		})
	}
}

func TestListingShowsWhatIsPublishedWithoutTheBytes(t *testing.T) {
	for _, tc := range publishDialects {
		t.Run(tc.name, func(t *testing.T) {
			owner := testutil.TestDB(t, tc.tdialect)
			t.Cleanup(func() { owner.Close() })
			testutil.SetupFullSchema(t, owner, tc.tdialect)
			pub := testutil.OpenLibraryLoginDB(t, owner, tc.tdialect, true)
			ctx := context.Background()
			lib, other := uniqueLibrary("list"), uniqueLibrary("listother")
			cleanupLibrary(t, owner, tc.edialect, lib)
			cleanupLibrary(t, owner, tc.edialect, other)

			if _, err := PublishLibraryBundle(ctx, pub, tc.edialect, PublishLibraryRequest{Library: lib, BundleVersion: "1", PublishedBy: "op", Members: []PublishLibraryMember{pubMember("a", 1, "aaaa", nil)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := PublishLibraryBundle(ctx, pub, tc.edialect, PublishLibraryRequest{Library: lib, BundleVersion: "2", Members: []PublishLibraryMember{pubMember("a", 1, "aaaa", nil), pubMember("b", 1, "bb", nil)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := PublishLibraryBundle(ctx, pub, tc.edialect, PublishLibraryRequest{Library: other, BundleVersion: "9", Members: []PublishLibraryMember{pubMember("z", 1, "z", nil)}}); err != nil {
				t.Fatal(err)
			}

			got, err := ListLibraryBundles(ctx, pub, tc.edialect, lib)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("listing one library returned %d bundle(s), want 2: %+v", len(got), got)
			}
			byVersion := map[string]LibraryBundleSummary{}
			for _, b := range got {
				if b.Library != lib {
					t.Errorf("the library filter let %q through", b.Library)
				}
				byVersion[b.BundleVersion] = b
			}
			if b := byVersion["1"]; len(b.Members) != 1 || b.PublishedBy != "op" || b.Members[0].WasmLength != 4 || len(b.Members[0].WasmSHA256) != 64 || b.Members[0].Exposure != ExposureAuth {
				t.Errorf("bundle 1 is listed wrongly: %+v", b)
			}
			if b := byVersion["2"]; len(b.Members) != 2 || b.Members[0].Name != "a" || b.Members[1].Name != "b" {
				t.Errorf("bundle 2 is listed wrongly (want members a and b, in name order): %+v", b)
			}
			if none, err := ListLibraryBundles(ctx, pub, tc.edialect, "no-such-library"); err != nil || len(none) != 0 {
				t.Errorf("listing an unknown library: %v, %+v, want an empty list", err, none)
			}
		})
	}
}

// countLibraryRows counts the rows one library has across the three library tables, as the owner.
func countLibraryRows(t *testing.T, owner *sql.DB, d Dialect, lib string) int {
	t.Helper()
	total := 0
	for _, table := range []string{"library.members", "library.bundles", "library.bundle_members"} {
		var n int
		if err := owner.QueryRow(`SELECT count(*) FROM `+table+` WHERE library = `+d.placeholder(1), lib).Scan(&n); err != nil {
			t.Fatalf("count %s for %s: %v", table, lib, err)
		}
		total += n
	}
	return total
}

// cleanupLibrary removes a test's library as the owner, listing first and members last, which
// is the order the foreign keys allow.
func cleanupLibrary(t *testing.T, owner *sql.DB, d Dialect, lib string) {
	t.Helper()
	t.Cleanup(func() {
		for _, table := range []string{"library.bundle_members", "library.bundles", "library.members"} {
			_, _ = owner.Exec(`DELETE FROM `+table+` WHERE library = `+d.placeholder(1), lib)
		}
	})
}
