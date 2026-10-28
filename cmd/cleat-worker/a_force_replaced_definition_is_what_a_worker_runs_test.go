package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"
	"github.com/cleat-team/cleat/wasm"
)

// cleat#3516, end to end: a real PostgreSQL, a real wasmtime guest and a real
// worker whose cache is warm. The definition is replaced the way cleat#3497's
// `--force` replaces it, with bytes of the SAME LENGTH, and the next run must
// execute the replacement.
//
// WHY THE REPLACEMENT HAS THE SAME LENGTH. Length was the only thing the
// worker's cache checked, so a different-length replacement was noticed by
// accident and a same-length one was served stale for the life of the process.
// The guest says which build it is by the operation of the one call it makes:
// MARKER-AAAAAA or MARKER-BBBBBB. The test patches the compiled module's data
// segment rather than building twice, which is what keeps the two the same
// length and otherwise identical.
func TestAForceReplacedDefinitionIsWhatAWarmWorkerRuns(t *testing.T) {
	if os.Getenv("CLEAT_TEST_POSTGRES") == "" && os.Getenv("CLEAT_TEST_DB") == "" {
		t.Skip("CLEAT_TEST_POSTGRES not set, skipping the database-backed hot-fix acceptance test")
	}
	old := wasmRevalidateInterval
	wasmRevalidateInterval = 0
	t.Cleanup(func() { wasmRevalidateInterval = old })

	for _, withDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk cache=%v", withDisk), func(t *testing.T) {
			ctx := context.Background()

			calls := &recordedCalls{}
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.add(strings.TrimPrefix(r.URL.Path, "/call/"))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ok":true}`)
			}))
			defer svc.Close()
			oldSvcURL := *benchSvcURL
			*benchSvcURL = svc.URL
			defer func() { *benchSvcURL = oldSvcURL }()

			db := testutil.SuiteTestDB(t, "cleat_worker")
			store := engine.NewPostgresStore(db)

			wasmA := buildMarkerFixture(t)
			wasmB := bytes.ReplaceAll(wasmA, []byte("MARKER-AAAAAA"), []byte("MARKER-BBBBBB"))
			if bytes.Equal(wasmA, wasmB) || len(wasmA) != len(wasmB) {
				t.Fatalf("UNMEASURED: the fixture's marker is not in the module, or the patch changed its length (%d vs %d bytes)", len(wasmA), len(wasmB))
			}
			meta, err := wasm.ReadMetadata(wasmA)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}

			defName := fmt.Sprintf("hotfix-marker-%d", time.Now().UnixNano())
			deploy := func(b []byte, force bool) error {
				return store.DeployWorkflowDef(ctx, &engine.WorkflowDef{
					Name: defName, Version: meta.WorkflowVersion, WASMBytes: b,
					ABIVersion: meta.ABIVersion, MinVersion: meta.MinCompatibleVersion,
					ReplaceUnderOpenRuns: force,
				})
			}
			start := func() string {
				id := fmt.Sprintf("%s-run-%d", defName, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, defName, meta.WorkflowVersion,
					json.RawMessage(`{"__entry_point":"marker"}`), "", engine.DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				return id
			}

			logs := &syncBuffer{}
			w := newRealDeferPhaseWorker(t, db, store, logs)
			w.egress = &engine.EgressGuard{
				AllowLoopback:  true,
				TenantOptional: func(context.Context) bool { return true },
			}
			if withDisk {
				w.wasmDiskCache = engine.NewWasmDiskCache(t.TempDir(), 100)
			}

			// Build A and run it once: the worker's caches now hold A.
			if err := deploy(wasmA, false); err != nil {
				t.Fatalf("deploy A: %v", err)
			}
			runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, start()))
			if !calls.has("marker/MARKER-AAAAAA") {
				t.Fatalf("UNMEASURED: the first run did not make the build-A call; the service saw %v (logs:\n%s)", calls.all(), logs.String())
			}

			// A run is open on the version, so replacing its bytes is refused
			// unless forced. That is the situation --force exists for, and it
			// is checked rather than assumed: without an open run the plain
			// deploy below would succeed and the force would be untested.
			open := start()
			if err := deploy(wasmB, false); !errors.Is(err, engine.ErrWorkflowDefInUse) {
				t.Fatalf("UNMEASURED: a plain deploy of different bytes under an open run returned %v, want ErrWorkflowDefInUse", err)
			}
			if err := deploy(wasmB, true); err != nil {
				t.Fatalf("forced deploy of the replacement: %v", err)
			}
			stored, err := store.LoadWASM(ctx, defName, meta.WorkflowVersion)
			if err != nil || !bytes.Equal(stored, wasmB) {
				t.Fatalf("UNMEASURED: the store does not hold the replacement after a forced deploy (err=%v)", err)
			}

			before := len(calls.all())
			runExecuteWorkflow(w, claimOne(t, ctx, store, w.id, open))
			ran := calls.all()[before:]
			if !calls.has("marker/MARKER-BBBBBB") {
				t.Fatalf("the worker ran the OLD build after a forced replacement: the run made %v, want marker/MARKER-BBBBBB (logs:\n%s)", ran, logs.String())
			}
		})
	}
}

// buildMarkerFixture compiles testdata/hotfixmarker the way buildDeferFixture
// compiles its fixture.
func buildMarkerFixture(t *testing.T) []byte {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	projectRoot := filepath.Dir(filepath.Dir(cwd))
	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, "testdata", "hotfixmarker"))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			b, err := os.ReadFile(filepath.Join(tmpDir, e.Name()))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			return b
		}
	}
	t.Fatalf("no .wasm file found in %s", tmpDir)
	return nil
}
