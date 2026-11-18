package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// A tenant's wasm_instance_timeout_ms is held to the same floor as the worker's --wasm-instance-timeout (cleat#3922, owner decision
// 2026-11-18). The write path is PostgresStore.WriteTenantSettings, which is the only Go writer of tenant_settings; see
// TestEveryWriterOfTenantSettingsIsKnown for how that is kept true on all three dialects.

func TestATenantInstanceTimeoutBelowTheFloorIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       time.Duration
		refused bool
	}{
		{"100ms, where the soft kill reached the guest 0 of 20 times", 100 * time.Millisecond, true},
		{"249ms, one under the floor", 249 * time.Millisecond, true},
		{"249.9ms, which would be stored as 249", 249*time.Millisecond + 900*time.Microsecond, true},
		{"500us, which would be stored as 0 and fail the table's CHECK", 500 * time.Microsecond, true},
		{"250ms, the floor itself", MinWasmInstanceTimeout, false},
		{"250.9ms", 250*time.Millisecond + 900*time.Microsecond, false},
		{"0, which clears the setting", 0, false},
		{"30s", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, teardown := postgresOnly(t)
			defer teardown()
			ctx := context.Background()

			_, rev, err := store.ReadTenantSettingsForUpdate(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			err = store.WriteTenantSettings(ctx, TenantSettings{WasmInstanceTimeout: tc.d, HostRetryBudget: 5 * time.Second}, rev)
			after, afterRev, readErr := store.ReadTenantSettingsForUpdate(ctx)
			if readErr != nil {
				t.Fatalf("read after: %v", readErr)
			}
			if tc.refused {
				if !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
					t.Fatalf("WriteTenantSettings(%v) = %v, want ErrWasmInstanceTimeoutBelowFloor", tc.d, err)
				}
				// Refused means NOTHING was written, not that an error came back after a partial write.
				if afterRev.Existed || after.HostRetryBudget != 0 {
					t.Errorf("a refused write left a row behind: existed=%v settings=%+v", afterRev.Existed, after)
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteTenantSettings(%v) = %v, want nil", tc.d, err)
			}
			if after.HostRetryBudget != 5*time.Second {
				t.Errorf("the accepted write did not land: %+v", after)
			}
		})
	}
}

func TestTheTenantRefusalSaysWhatTheWorkerFlagRefusalSays(t *testing.T) {
	err := CheckWasmInstanceTimeoutFloor(100*time.Millisecond, "tenant setting wasm_instance_timeout_ms", "clear it to use the operator's flag")
	if err == nil {
		t.Fatal("100ms was accepted")
	}
	for _, want := range []string{"tenant setting wasm_instance_timeout_ms", "100ms", "250ms", "defers", "ABI.md", "Raise tenant setting wasm_instance_timeout_ms to at least 250ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
	if !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
		t.Error("the refusal does not match ErrWasmInstanceTimeoutBelowFloor")
	}
	for _, ok := range []time.Duration{0, -time.Second, MinWasmInstanceTimeout, 30 * time.Second} {
		if e := CheckWasmInstanceTimeoutFloor(ok, "x", "y"); e != nil {
			t.Errorf("CheckWasmInstanceTimeoutFloor(%v) = %v, want nil", ok, e)
		}
	}
}

// Existing values are not rewritten and not newly refused. A tenant that already holds a sub-floor value must still be able to
// change its OTHER settings, because this write replaces the whole row and sends the old value back with them.
func TestAnExistingSubFloorValueIsCarriedThroughButNeverNewlyWritten(t *testing.T) {
	store, teardown := postgresOnly(t)
	defer teardown()
	ctx := context.Background()

	// Seed the legacy state the way it arose: straight into the table, past any check.
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO tenant_settings (tenant_id, wasm_instance_timeout_ms, updated_at) VALUES ($1, 100, now())`, store.tenantID); err != nil {
		t.Fatalf("seeding a legacy 100ms row: %v", err)
	}

	cur, rev, err := store.ReadTenantSettingsForUpdate(ctx)
	if err != nil || cur.WasmInstanceTimeout != 100*time.Millisecond {
		t.Fatalf("seed read: %+v, %v", cur, err)
	}

	// 1. Changing a different setting carries the legacy value through, and it is still 100ms afterwards.
	cur.HostRetryBudget = 7 * time.Second
	if err := store.WriteTenantSettings(ctx, cur, rev); err != nil {
		t.Fatalf("changing host_retry_budget on a tenant with a legacy 100ms instance timeout was refused: %v", err)
	}
	cur, rev, _ = store.ReadTenantSettingsForUpdate(ctx)
	if cur.WasmInstanceTimeout != 100*time.Millisecond || cur.HostRetryBudget != 7*time.Second {
		t.Fatalf("carry-through changed a value: %+v", cur)
	}

	// 2. Moving it to ANOTHER sub-floor value is a new write and is refused.
	moved := cur
	moved.WasmInstanceTimeout = 150 * time.Millisecond
	if err := store.WriteTenantSettings(ctx, moved, rev); !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
		t.Fatalf("100ms -> 150ms returned %v, want ErrWasmInstanceTimeoutBelowFloor", err)
	}

	// 3. Raising it to the floor is accepted, and from then on the old value is gone: lowering it again is refused.
	moved.WasmInstanceTimeout = MinWasmInstanceTimeout
	if err := store.WriteTenantSettings(ctx, moved, rev); err != nil {
		t.Fatalf("raising 100ms to the floor was refused: %v", err)
	}
	cur, rev, _ = store.ReadTenantSettingsForUpdate(ctx)
	cur.WasmInstanceTimeout = 100 * time.Millisecond
	if err := store.WriteTenantSettings(ctx, cur, rev); !errors.Is(err, ErrWasmInstanceTimeoutBelowFloor) {
		t.Fatalf("250ms -> 100ms returned %v, want ErrWasmInstanceTimeoutBelowFloor (a value that is no longer stored must not be carried)", err)
	}
}

// The tenant setting has ONE Go writer, and it is Postgres-only: cleatctl set-tenant-setting is declared {"postgres"} in ported.go and
// MySQLStore / MSSQLStore have a GetTenantSettings and nothing that writes. So the "three dialects" for this floor are one writer and
// two readers, and this test is what keeps that true: a second writer (a MySQL or SQL Server store method, an HTTP handler, a new
// INSERT) fails here until it carries the floor, instead of being discovered by a tenant that reaches the cliff.
//
// Source scan, over non-test Go in engine/ and every directory that can reach the table. It is a TEXT scan of a format it does not
// model, so it has a known-positive: the one writer that exists must be found, or the scan proves nothing.
func TestEveryWriterOfTenantSettingsIsKnown(t *testing.T) {
	root := ".."
	writerMethod := regexp.MustCompile(`func \([a-zA-Z]+ \*?[A-Za-z]+\) WriteTenantSettings\(`)
	writerSQL := regexp.MustCompile(`(?i)(INSERT\s+INTO|UPDATE|MERGE\s+(INTO\s+)?)\s+(dbo\.)?tenant_settings\b`)

	var methods, sqls []string
	for _, dir := range []string{"engine", "cmd", "plugins", "plugin", "auth", "cleat", "internal", "web"} {
		_ = filepath.Walk(filepath.Join(root, dir), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if writerMethod.Match(b) {
				methods = append(methods, filepath.ToSlash(rel))
			}
			if writerSQL.Match(b) {
				sqls = append(sqls, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	const known = "engine/store_tenant_settings_write.go"
	// The known-positive: the scan must see the writer that exists.
	if len(methods) != 1 || methods[0] != known {
		t.Errorf("WriteTenantSettings is defined in %v, want exactly [%s]. A new writer needs engine.CheckWasmInstanceTimeoutFloor on its "+
			"wasm_instance_timeout_ms path and an entry here; an EMPTY list means this scan stopped seeing the file it exists for.", methods, known)
	}
	if len(sqls) != 1 || sqls[0] != known {
		t.Errorf("tenant_settings is written by SQL in %v, want exactly [%s]. A new writer needs engine.CheckWasmInstanceTimeoutFloor on its "+
			"wasm_instance_timeout_ms path and an entry here.", sqls, known)
	}
}
