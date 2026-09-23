package main

// The worker's startup check for secrets (cleat#2123).
//
// checkSecretsUsable refuses a worker that has no master key and a database
// that holds secrets. Until cleat#2123 it treated a count that ERRORED as "cannot
// tell" and went on -- which is what let it stay silent for as long as its read
// could not see the rows (PostgreSQL raised, SQL Server returned 0). The engine's
// TestCountSecretsSeesEveryTenantsRowsOnEveryDialect covers the read; this covers
// what the caller does with a read that fails, which needs the failure injected
// because a healthy database will not produce one on demand.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSecretCounter struct {
	hasKey bool
	n      int
	err    error
	calls  int
}

func (f *fakeSecretCounter) HasMasterKey() bool { return f.hasKey }
func (f *fakeSecretCounter) CountSecrets(context.Context) (int, error) {
	f.calls++
	return f.n, f.err
}

func TestCheckSecretsUsableRefusesWhenTheCountCannotBeRead(t *testing.T) {
	boom := errors.New("cleat.tenant_id is not set -- tenant context required for RLS-scoped query")
	f := &fakeSecretCounter{hasKey: false, err: boom}

	err := checkSecretsUsable(context.Background(), f)
	if err == nil {
		t.Fatal("no key and an unreadable secrets table returned nil: the worker would start " +
			"and fail on its first plugin call, which is the behaviour cleat#2123 removes")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the refusal must carry the read's own error so an operator can act on it; got %v", err)
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("the refusal should say what it is doing; got %v", err)
	}
	if f.calls != 1 {
		t.Errorf("CountSecrets called %d times, want 1", f.calls)
	}
}

func TestCheckSecretsUsableTable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		c         *fakeSecretCounter
		wantErr   bool
		wantCalls int
		wantText  string
	}{
		{"key present: nothing to check, and the table is not read", &fakeSecretCounter{hasKey: true, n: 9}, false, 0, ""},
		{"no key, no secrets", &fakeSecretCounter{hasKey: false, n: 0}, false, 1, ""},
		{"no key, secrets stored", &fakeSecretCounter{hasKey: false, n: 3}, true, 1, "3 secret(s) are stored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSecretsUsable(context.Background(), tc.c)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantText)) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantText)
			}
			if tc.c.calls != tc.wantCalls {
				t.Errorf("CountSecrets called %d times, want %d", tc.c.calls, tc.wantCalls)
			}
		})
	}
}
