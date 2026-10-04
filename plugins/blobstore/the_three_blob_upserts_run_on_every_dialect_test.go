// The three blobstore upserts that cleat#2915 gave a HOLDLOCK hint to, and that
// nothing ran against a real SQL Server until this test: upsertBlobRef,
// upsertBlobContentData and upsertBlobIndexWithTTL.
//
// cleat#2920. WHY THEY WERE MISSED, because "some test calls blobPut" reads as
// coverage: blobPut DOES reach two of the three on every dialect, and both
// reach-arounds are guarded by something the existing per-dialect tests do not
// supply.
//   - upsertBlobRef sits behind `if wfID != ""` (host_functions.go:171), and
//     those tests build a CallContext with a TenantID and no WorkflowID.
//   - upsertBlobIndexWithTTL is the TTL branch only; they pass no TTL, so the
//     non-TTL upsertBlobIndex runs instead.
//
// upsertBlobContentData is reached by memoryBackend.Put, and those same tests
// inject newTestMemBackend() -- a different type with its own Put.
//
// WHY THE ASSERTION IS ON THE ROW, NOT ON THE CALL. upsertBlobRef's error is
// only LOGGED by blobPut, so a MERGE failing there still returns success to this
// caller; an assertion on the error would be vacuous for exactly the statement
// the issue flags as silent. And on SQL Server a tenant-scoped READ through
// be.DB is filtered to nothing SILENTLY (testutil.CrossTenantConn's comment on
// the kvstore fixture's DELETE that removed no rows and reported success), so a
// row check on be.DB would find the row on Postgres and nothing on SQL Server,
// for a reason having nothing to do with the statement. Every read below goes
// through CrossTenantConn.
//
// WHICH ASSERTION WOULD EXPOSE THAT IS NOT UNIFORM, and it is worth knowing
// before trusting a green: workflow_blob_refs has no tenant column and no
// policy, so reading it through be.DB works and assertion 1 would pass. Only
// the blob_index read in assertion 2 is tenant-scoped, so only that one
// reddens. Measured by cleat-review at review time on PostgreSQL + SQL Server.
package blobstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/plugintest"
)

func TestTheThreeBlobUpsertsRunOnEveryDialect(t *testing.T) {
	for _, be := range testutil.NewPluginTestBackends(t) {
		t.Run(be.Name, func(t *testing.T) {
			defer be.Cleanup()

			ctx := context.Background()
			dialect := plugin.Dialect(string(be.Dialect))

			testutil.SetupFullSchema(t, be.DB, be.Dialect)

			p := &Plugin{dialect: dialect, logger: slog.Default(), backend: newTestMemBackend()}
			if err := plugin.RunMigrations(ctx, be.DB, dialect, nil,
				[]*plugin.LoadedPlugin{{Plugin: p, Healthy: true}}); err != nil {
				t.Fatalf("migrations: %v", err)
			}
			p.db = &engine.SQLDBAdapter{DB: be.DB, Dialect: dialect}

			tenantID := uuid.MustParse(engine.DefaultTenantUUID)
			// WorkflowID is load-bearing: it is what makes blobPut take the
			// upsertBlobRef branch at all. With it empty, the statement this
			// test exists for is skipped and everything below still passes
			// for the other two -- which is how it went uncovered.
			cc := &plugin.CallContext{
				TenantID:   tenantID.String(),
				WorkflowID: uuid.New().String(),
			}
			hostCtx := plugin.WithCallContext(auth.WithTenantID(ctx, tenantID), cc)

			readConn := be.CrossTenantConn(t, ctx,
				"cleat#2920: reading the plugin tables these three upserts write, which SQL Server filters silently through be.DB")

			put := func(t *testing.T, in blobPutInput) {
				t.Helper()
				body, err := json.Marshal(in)
				if err != nil {
					t.Fatalf("marshal put input: %v", err)
				}
				if _, err := p.blobPut(hostCtx, string(body)); err != nil {
					t.Fatalf("blobPut(%s) on %s: %v", in.Key, be.Name, err)
				}
			}
			// QueryRowRebound, NOT QueryRowContext(ctx, plugin.Rebind(...)):
			// Rebind is the IDENTITY for MySQL (plugin/query.go:94) and the
			// $N -> ? rewrite lives in RebindArgs, which it never calls, so
			// the portable form reaches the server literally and every MySQL
			// run dies on "Unknown column '$1'" (cleat#2259).
			count := func(t *testing.T, query string, args ...any) int {
				t.Helper()
				var n int
				if err := plugintest.QueryRowRebound(t, ctx, readConn, dialect, query, args...).Scan(&n); err != nil {
					t.Fatalf("count on %s (%s): %v", be.Name, query, err)
				}
				return n
			}

			// ---- 1. upsertBlobRef, on blobPut's workflow-id branch.
			refKey := "upsert-ref-" + uuid.New().String()
			put(t, blobPutInput{Key: refKey, ContentType: "text/plain", Data: []byte("ref body")})
			if got := count(t, "SELECT COUNT(*) FROM workflow_blob_refs WHERE workflow_id = $1", cc.WorkflowID); got == 0 {
				t.Errorf("upsertBlobRef on %s: blobPut with WorkflowID set left no workflow_blob_refs row -- "+
					"the call is only logged by its caller, so this is the assertion that has to catch it", be.Name)
			}

			// ---- 2. upsertBlobIndexWithTTL, on blobPut's TTL branch.
			ttlKey := "upsert-ttl-" + uuid.New().String()
			put(t, blobPutInput{Key: ttlKey, ContentType: "text/plain", Data: []byte("ttl body"), TTL: "1h"})
			if got := count(t, "SELECT COUNT(*) FROM blob_index WHERE "+plugin.QuoteIdent("key", dialect)+
				" = $1 AND expires_at IS NOT NULL", ttlKey); got == 0 {
				t.Errorf("upsertBlobIndexWithTTL on %s: blobPut with TTL=1h left no blob_index row with expires_at set", be.Name)
			}

			// ---- 3. upsertBlobContentData, through the backend that calls it.
			// newTestMemBackend (used above) has its own Put and never reaches
			// this statement, which is why it went uncovered.
			sha := strings.Repeat("ab", 32) // 64 hex chars; Put hex-decodes it
			shaBytes, err := hex.DecodeString(sha)
			if err != nil {
				t.Fatalf("decode sha: %v", err)
			}
			mb := newMemoryBackend(p.db, dialect)
			if err := mb.Put(hostCtx, sha, []byte("content body"), ""); err != nil {
				t.Fatalf("memoryBackend.Put on %s: %v", be.Name, err)
			}
			if got := count(t, "SELECT COUNT(*) FROM blob_content WHERE sha256 = $1", shaBytes); got == 0 {
				t.Errorf("upsertBlobContentData on %s: memoryBackend.Put left no blob_content row for sha %s", be.Name, sha)
			}
		})
	}
}
