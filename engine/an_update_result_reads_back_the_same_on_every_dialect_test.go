package engine

// cleat#3929. `result` is read through the same defect #3903 fixed for
// `payload`: PostgreSQL extracted it with `result #>> '{}'` and then ran
// json.Compact, so a result that was a JSON STRING whose content is JSON had
// that content compacted, while MySQL and SQL Server never unwrapped it at all.
//
// TWO THINGS THIS TEST DOES THAT A READER SHOULD KNOW.
//
// First, it CONSTRUCTS A STATE PRODUCTION CANNOT REACH. GetPendingUpdateRequests
// pins `status = 'pending'`, and the only statement that writes `result` sets
// `status = 'completed'` in the same statement, so every row this read sees has
// a NULL result today. The test inserts a pending row with a result directly to
// exercise the transform at all. It is therefore a test of the read PATH, not
// evidence that the path is live.
//
// Second, the assertion is AGREEMENT PLUS VALID JSON, not agreement alone. A
// result is JSON text handed onward -- the promise path compacts it and keeps a
// string's quotes (cleat#3807) -- so an implementation that unwrapped a string
// result would still "agree" across dialects while handing back `yes`, which no
// caller can unmarshal. json.Valid is what distinguishes the two conventions,
// and the string-result case is the one that catches it.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func TestAnUpdateResultReadsBackTheSameOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("no database in -short mode; every per-dialect subtest would skip")
	}

	results := []struct{ name, json string }{
		{"a JSON string", `"yes"`},
		{"a string holding an object", `"{\"a\": 1}"`},
		{"an object with spacing", `{"a": 1}`},
		{"the same object compact", `{"a":1}`},
		{"a number", `5`},
		{"a boolean", `true`},
		{"JSON null", `null`},
	}

	got := map[string]map[string]string{}
	var ran []string

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			base, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, base)
			truncateAll(t, base)
			ctx := context.Background()

			wf, _, err := base.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "result-parity", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			for i, tc := range results {
				name := fmt.Sprintf("r%d", i)
				if err := base.CreateUpdateRequest(ctx, wf, name, `{}`, "promise-"+name); err != nil {
					t.Fatalf("%s: CreateUpdateRequest: %v", tc.name, err)
				}
			}

			// Construct the unreachable state: a PENDING row carrying a result.
			db := rawDBOf(t, base)
			var update string
			switch base.(type) {
			case *PostgresStore:
				update = `UPDATE workflow_update_requests SET result = $1::jsonb WHERE workflow_id = $2 AND update_name = $3`
			case *MySQLStore:
				update = `UPDATE workflow_update_requests SET result = ? WHERE workflow_id = ? AND update_name = ?`
			case *MSSQLStore:
				update = `UPDATE workflow_update_requests SET result = @p1 WHERE workflow_id = @p2 AND update_name = @p3`
			default:
				t.Fatalf("%T is not a dialect this test knows", base)
			}
			for i, tc := range results {
				name := fmt.Sprintf("r%d", i)
				if _, err := db.ExecContext(ctx, update, tc.json, wf, name); err != nil {
					t.Fatalf("%s: writing the result directly: %v", tc.name, err)
				}
			}

			reqs, err := base.GetPendingUpdateRequests(ctx, wf)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests: %v", err)
			}
			byName := make(map[string]UpdateRequestInfo, len(reqs))
			for _, r := range reqs {
				byName[r.UpdateName] = r
			}

			got[backend.Name()] = make(map[string]string, len(results))
			for i, tc := range results {
				r, ok := byName[fmt.Sprintf("r%d", i)]
				if !ok {
					t.Fatalf("%s: the request is missing from the pending list", tc.name)
				}
				got[backend.Name()][tc.name] = r.Result
			}
			ran = append(ran, backend.Name())

			// The SEALED arm, PostgreSQL only. A result written sealed must read
			// back identically to the same value written plain -- and without
			// this, the decrypt-then-compact branch of the transform would be
			// untested: the cross-dialect comparison above never seals, and
			// #3852's test only asserts that a PENDING row carries no result on
			// either arm, so a pending row carrying a SEALED result is covered
			// here or nowhere.
			pg, isPG := base.(*PostgresStore)
			if !isPG {
				return
			}
			enc, err := NewPayloadEncryption(validKey(t))
			if err != nil {
				t.Fatalf("NewPayloadEncryption: %v", err)
			}
			sealed := pg.WithEncryption(enc, true)
			wfSealed, _, err := sealed.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "result-parity-sealed", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (sealed): %v", err)
			}
			for i, tc := range results {
				name := fmt.Sprintf("r%d", i)
				if err := sealed.CreateUpdateRequest(ctx, wfSealed, name, `{}`, "promise-"+name); err != nil {
					t.Fatalf("%s: CreateUpdateRequest (sealed): %v", tc.name, err)
				}
			}
			cipher, err := sealed.tenantCipherForWrite()
			if err != nil {
				t.Fatalf("tenant cipher: %v", err)
			}
			for i, tc := range results {
				name := fmt.Sprintf("r%d", i)
				sealedResult, err := encryptJSONColumnForStorage(tc.json, cipher)
				if err != nil {
					t.Fatalf("%s: sealing the result: %v", tc.name, err)
				}
				if _, err := db.ExecContext(ctx, update, jsonOrNull(sealedResult), wfSealed, name); err != nil {
					t.Fatalf("%s: writing the sealed result: %v", tc.name, err)
				}
			}

			// Precondition beside the verdict: the row really is ciphertext, so
			// a store that quietly stopped sealing cannot make this a tautology.
			var onDisk string
			if err := db.QueryRowContext(ctx,
				`SELECT result #>> '{}' FROM workflow_update_requests WHERE workflow_id = $1 AND update_name = 'r3'`,
				wfSealed).Scan(&onDisk); err != nil {
				t.Fatalf("read the sealed result's stored bytes: %v", err)
			}
			if strings.Contains(onDisk, `"a"`) {
				t.Fatalf("the result is stored as %q, so this arm is not reading a sealed row", onDisk)
			}

			sealedReqs, err := sealed.GetPendingUpdateRequests(ctx, wfSealed)
			if err != nil {
				t.Fatalf("GetPendingUpdateRequests (sealed): %v", err)
			}
			sealedByName := make(map[string]UpdateRequestInfo, len(sealedReqs))
			for _, r := range sealedReqs {
				sealedByName[r.UpdateName] = r
			}
			for i, tc := range results {
				r, ok := sealedByName[fmt.Sprintf("r%d", i)]
				if !ok {
					t.Fatalf("%s: the request is missing from the sealed pending list", tc.name)
				}
				if want := got[backend.Name()][tc.name]; r.Result != want {
					t.Errorf("%s: sealed reads back %q and unsealed %q for the same value", tc.name, r.Result, want)
				}
			}
		})
	}

	sort.Strings(ran)
	if len(ran) == 0 {
		t.Fatal("no backend ran, so this test measured nothing")
	}

	for _, tc := range results {
		for _, b := range ran {
			v := got[b][tc.name]
			if !json.Valid([]byte(v)) {
				t.Errorf("%s on %s: the result reads back as %q, which is not valid JSON; a result is JSON text handed onward, so a string result keeps its quotes", tc.name, b, v)
			}
		}
		for i := 1; i < len(ran); i++ {
			a, b := ran[0], ran[i]
			if got[a][tc.name] != got[b][tc.name] {
				t.Errorf("%s: GetPendingUpdateRequests returns %q on %s and %q on %s for the same stored result",
					tc.name, got[a][tc.name], a, got[b][tc.name], b)
			}
		}
	}
}
