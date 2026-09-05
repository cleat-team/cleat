package pluginharness

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/plugin"
	"github.com/cleat-team/cleat/plugins/blobstore"
)

// TestBlobstore_S3 exercises the blobstore plugin end to end against a real
// S3-compatible server: bytes to object storage, index rows to PostgreSQL.
//
// It is named for the -run pattern in plugin-harness-ci.yml's Layer 4, which
// named this test before the test existed. That job provisioned a MinIO service
// container and five CLEAT_TEST_S3_* variables, and then ran
// `go test ./... -run 'TestBlobstore_S3'` -- which matched nothing, printed
// "ok ... [no tests to run]", and exited 0. It had never run a test.
// See IMPROVEMENT-PLAN 3.310 for how that was found and 3.311 for this.
//
// What this covers that plugins/blobstore/blobstore_s3_backend_test.go cannot:
// those tests construct the unexported s3Backend with a mock RoundTripper, so
// they assert the plugin's own call sequence and can never see how a real
// server answers it. They also live in the root module, so Layer 4's
// working-directory could not have reached them. This drives the plugin's
// registered host functions -- the surface a workflow actually calls -- with
// a genuine endpoint underneath.
func TestBlobstore_S3(t *testing.T) {
	endpointURL := os.Getenv("CLEAT_TEST_S3_ENDPOINT")
	if endpointURL == "" {
		t.Skip("CLEAT_TEST_S3_ENDPOINT not set -- needs an S3-compatible server " +
			"(the Layer 4 job provisions MinIO; locally: docker run -p 9000:9000 minio/minio server /data)")
	}
	pgConn := os.Getenv("CLEAT_TEST_POSTGRES")
	if pgConn == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set -- blobstore stores its index in SQL, " +
			"so the S3 backend cannot be exercised through the plugin without one")
	}

	bucket := os.Getenv("CLEAT_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "cleat-test-harness"
	}
	accessKey := os.Getenv("CLEAT_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("CLEAT_TEST_S3_SECRET_KEY")

	// The plugin passes Config.Endpoint straight to minio.New, which wants
	// host:port and not a URL. The environment variable carries a scheme
	// (the job sets http://localhost:9000), so strip it and let the scheme
	// decide TLS unless CLEAT_TEST_S3_USE_SSL overrides.
	host := endpointURL
	secure := false
	if u, err := url.Parse(endpointURL); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}
	if v := os.Getenv("CLEAT_TEST_S3_USE_SSL"); v != "" {
		secure = v == "true" || v == "1"
	}

	ctx := context.Background()

	// Ensure the bucket exists. MinIO starts empty, so a missing bucket is the
	// normal first-run state rather than a failure.
	admin, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		t.Fatalf("connecting to the S3 endpoint at %s: %v", host, err)
	}
	exists, err := admin.BucketExists(ctx, bucket)
	if err != nil {
		t.Fatalf("BucketExists(%s): %v -- CLEAT_TEST_S3_ENDPOINT is set, so this is a "+
			"real connection failure rather than a missing configuration", bucket, err)
	}
	if !exists {
		if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("MakeBucket(%s): %v", bucket, err)
		}
	}

	// Index storage: a throwaway schema, dropped on the way out.
	db, schemaName := OpenTestDB(t, plugin.DialectPostgres, pgConn)
	defer CleanupTestDB(t, db, plugin.DialectPostgres, schemaName)

	// Only blobstore's own migrations. The engine's core workflow tables are
	// not touched here, and RunCoreMigrations would drag in an unrelated
	// dependency: 033_completed_workflow_retention_indexes.sql needs the
	// pg_trgm extension, so a database without it fails this test for a reason
	// that has nothing to do with S3. RunPluginMigrations creates its own
	// tracking table, so it stands alone.
	bp := blobstore.New()
	loaded := []*plugin.LoadedPlugin{{Plugin: bp, Healthy: true}}
	RunPluginMigrations(t, db, plugin.DialectPostgres, loaded)

	cfg, err := json.Marshal(map[string]any{
		"backend":           "s3",
		"bucket":            bucket,
		"region":            "us-east-1",
		"endpoint":          host,
		"access_key_id":     accessKey,
		"secret_access_key": secretKey,
		"secure":            secure,
	})
	if err != nil {
		t.Fatalf("marshal blobstore config: %v", err)
	}
	if err := bp.Init(ctx, &plugin.Environment{
		DB:      &engine.SQLDBAdapter{DB: db},
		Dialect: plugin.DialectPostgres,
		Config:  cfg,
	}); err != nil {
		t.Fatalf("blobstore Init with the s3 backend: %v", err)
	}

	// Register the plugin's host functions the way the harness does, so this
	// drives the same surface a workflow reaches through cleat_plugin_call.
	reg := engine.NewPluginRegistry()
	hf, ok := bp.(plugin.HasHostFunctions)
	if !ok {
		t.Fatal("blobstore no longer implements plugin.HasHostFunctions")
	}
	if err := hf.RegisterHostFunctions(&hostFuncAdapter{pluginName: "blobstore", registry: reg}); err != nil {
		t.Fatalf("RegisterHostFunctions: %v", err)
	}

	// Random payload so repeated runs against a shared bucket cannot pass by
	// reading an object an earlier run left behind. The assertions are on
	// round-trip equality, so the randomness does not make the outcome vary.
	payload := make([]byte, 4096)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generating payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	sha256Hex := hex.EncodeToString(sum[:])
	key := "harness/" + sha256Hex[:16]

	// blobstore requires a tenant; every plugin call in the WASM harness fails
	// with "blobstore: no tenant context" for want of one.
	// Both must be UUIDs: the index columns are typed uuid, and a readable
	// label like "tenant-blobstore-s3" fails with 22P02 at the insert, well
	// after the object has already been written to S3.
	callCtx := plugin.WithCallContext(ctx, &plugin.CallContext{
		TenantID:   "b10b5106-0000-4000-8000-000000000001",
		WorkflowID: "b10b5106-0000-4000-8000-000000000002",
	})

	putFn, _, found := reg.Lookup("blobstore", "put")
	if !found {
		t.Fatal("blobstore/put is not registered")
	}
	putIn, err := json.Marshal(map[string]any{
		"key":          key,
		"content_type": "application/octet-stream",
		"data":         payload,
	})
	if err != nil {
		t.Fatalf("marshal put input: %v", err)
	}
	putOutJSON, err := putFn(callCtx, string(putIn))
	if err != nil {
		t.Fatalf("blobstore/put against %s: %v", host, err)
	}
	var putOut struct {
		Key    string `json:"key"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(putOutJSON), &putOut); err != nil {
		t.Fatalf("unmarshal put output %q: %v", putOutJSON, err)
	}
	if putOut.SHA256 != sha256Hex {
		t.Errorf("put reported sha256 %s, want %s", putOut.SHA256, sha256Hex)
	}
	if putOut.Size != int64(len(payload)) {
		t.Errorf("put reported size %d, want %d", putOut.Size, len(payload))
	}

	// The object must be in the bucket, at the content-addressed name.
	//
	// This is the assertion that makes this an S3 test rather than a round-trip
	// test. Config.Backend defaults to "memory", so a plugin that ignored or
	// failed to parse the config above would still put and get successfully --
	// out of a map, with nothing in the bucket. Reading through the plugin
	// cannot tell those apart; reading the bucket directly can.
	stat, err := admin.StatObject(ctx, bucket, sha256Hex, minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("the blob is not in the bucket at %s/%s: %v\n"+
			"put succeeded, so the bytes went somewhere else -- the memory backend is the "+
			"default and would produce exactly this", bucket, sha256Hex, err)
	}
	if stat.Size != int64(len(payload)) {
		t.Errorf("object in bucket is %d bytes, want %d", stat.Size, len(payload))
	}
	defer func() {
		_ = admin.RemoveObject(context.Background(), bucket, sha256Hex, minio.RemoveObjectOptions{})
	}()

	getFn, _, found := reg.Lookup("blobstore", "get")
	if !found {
		t.Fatal("blobstore/get is not registered")
	}
	getIn, err := json.Marshal(map[string]any{"key": key})
	if err != nil {
		t.Fatalf("marshal get input: %v", err)
	}
	getOutJSON, err := getFn(callCtx, string(getIn))
	if err != nil {
		t.Fatalf("blobstore/get: %v", err)
	}
	var getOut struct {
		Key         string `json:"key"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		ContentType string `json:"content_type"`
		Data        []byte `json:"data"`
	}
	if err := json.Unmarshal([]byte(getOutJSON), &getOut); err != nil {
		t.Fatalf("unmarshal get output: %v", err)
	}
	if !bytes.Equal(getOut.Data, payload) {
		t.Errorf("round trip through a real S3 server returned %d bytes, want %d (sha256 %s vs %s)",
			len(getOut.Data), len(payload), func() string {
				s := sha256.Sum256(getOut.Data)
				return hex.EncodeToString(s[:])
			}(), sha256Hex)
	}
	if getOut.SHA256 != sha256Hex {
		t.Errorf("get reported sha256 %s, want %s", getOut.SHA256, sha256Hex)
	}
	if !strings.HasPrefix(getOut.ContentType, "application/octet-stream") {
		t.Errorf("get reported content type %q, want application/octet-stream", getOut.ContentType)
	}
}
