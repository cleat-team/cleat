package main

// The worker's startup check for secrets (cleat#2123, cleat#1991).
//
// checkSecretsUsable refuses a worker that would fail on its first secret, in
// two ways: no key while secrets exist, and a key ring that lacks a key_version
// still present in the table. Until cleat#2123 it treated a count that ERRORED
// as "cannot tell" and went on -- which is what let it stay silent for as long
// as its read could not see the rows (PostgreSQL raised, SQL Server returned 0).
// The engine's tests cover the READS on all three dialects; this covers what the
// caller does with them, and needs the failures injected because a healthy
// database will not produce one on demand.
//
// The ring-aware half is the boot check of specs/CleatKeyRotation.tla: with it
// switched off, a worker deployed with the old key already retired serves a row
// it cannot open (S1, four states). TestCheckSecretsUsableRefusesARingThatLacksAVersionInTheTable
// is the Go form of that trace.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

type fakeSecretStore struct {
	hasKey   bool
	n        int
	countErr error
	chk      engine.SecretKeyCheck
	chkErr   error

	countCalls int
	chkCalls   int
}

func (f *fakeSecretStore) HasMasterKey() bool { return f.hasKey }
func (f *fakeSecretStore) CountSecrets(context.Context) (int, error) {
	f.countCalls++
	return f.n, f.countErr
}
func (f *fakeSecretStore) CheckKeyRing(context.Context) (engine.SecretKeyCheck, error) {
	f.chkCalls++
	return f.chk, f.chkErr
}

func TestCheckSecretsUsableRefusesWhenTheCountCannotBeRead(t *testing.T) {
	boom := errors.New("cleat.tenant_id is not set -- tenant context required for RLS-scoped query")
	f := &fakeSecretStore{hasKey: false, countErr: boom}

	err := checkSecretsUsable(context.Background(), f, nil)
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
	if f.countCalls != 1 {
		t.Errorf("CountSecrets called %d times, want 1", f.countCalls)
	}
}

// With a ring the census is unreadable: the same rule, on the other read. A
// worker that cannot tell whether its keys open every row must not assume they do.
func TestCheckSecretsUsableRefusesWhenTheKeyCensusCannotBeRead(t *testing.T) {
	boom := errors.New("the census could not be read")
	f := &fakeSecretStore{hasKey: true, chkErr: boom}

	err := checkSecretsUsable(context.Background(), f, nil)
	if err == nil {
		t.Fatal("a key ring and an unreadable census returned nil")
	}
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("want the read's error and a stated refusal; got %v", err)
	}
}

// THE BOOT CHECK. The ring holds key version 2 only; the table still has rows
// under version 1, which is what happens when the previous key is removed before
// reseal-secrets has finished. The refusal names the version, how many rows carry
// it and what IS configured, because those are what an operator needs to put the
// right key back.
func TestCheckSecretsUsableRefusesARingThatLacksAVersionInTheTable(t *testing.T) {
	f := &fakeSecretStore{hasKey: true, chk: engine.SecretKeyCheck{
		Total: 5, Unopenable: map[int]int{1: 3}, Configured: []int{2},
	}}

	err := checkSecretsUsable(context.Background(), f, nil)
	if err == nil {
		t.Fatal("a worker whose ring lacks key_version 1, with 3 rows still on it, was allowed to start " +
			"(specs/CleatKeyRotation.tla, S1 with BootCheck = FALSE)")
	}
	for _, want := range []string{"3 secret(s) under key_version 1", "[2]", "reseal-secrets"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should contain %q; got %v", want, err)
		}
	}
	if f.countCalls != 0 {
		t.Errorf("with a key present the plain count is not needed; CountSecrets was called %d times", f.countCalls)
	}
}

func TestCheckSecretsUsableNamesEveryMissingVersionInOrder(t *testing.T) {
	f := &fakeSecretStore{hasKey: true, chk: engine.SecretKeyCheck{
		Unopenable: map[int]int{3: 1, 1: 2}, Configured: []int{2},
	}}
	err := checkSecretsUsable(context.Background(), f, nil)
	if err == nil {
		t.Fatal("two unopenable versions returned nil")
	}
	i1 := strings.Index(err.Error(), "key_version 1")
	i3 := strings.Index(err.Error(), "key_version 3")
	if i1 < 0 || i3 < 0 || i1 > i3 {
		t.Errorf("both versions should be named, ascending; got %v", err)
	}
}

// Rows on the previous key still work, so they are a warning and not a
// refusal -- but they are what has to be resealed before that key can go, and
// the warning says so.
func TestCheckSecretsUsableWarnsAboutRowsStillOnThePreviousKey(t *testing.T) {
	f := &fakeSecretStore{hasKey: true, chk: engine.SecretKeyCheck{
		Total: 4, OnPrevious: map[int]int{1: 4}, Configured: []int{1, 2},
	}}
	var warned []string
	if err := checkSecretsUsable(context.Background(), f, func(m string) { warned = append(warned, m) }); err != nil {
		t.Fatalf("rows the ring CAN open must not refuse: %v", err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "4 secret(s)") ||
		!strings.Contains(warned[0], "key_version 1") || !strings.Contains(warned[0], "reseal-secrets") {
		t.Errorf("want one warning naming 4 secrets, key_version 1 and reseal-secrets; got %v", warned)
	}
}

func TestCheckSecretsUsableTable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		f             *fakeSecretStore
		wantErr       bool
		wantCount     int
		wantCensus    int
		wantErrText   string
		wantWarnCount int
	}{
		{"no key, no secrets", &fakeSecretStore{hasKey: false, n: 0}, false, 1, 0, "", 0},
		{"no key, secrets stored", &fakeSecretStore{hasKey: false, n: 3}, true, 1, 0, "3 secret(s) are stored", 0},
		{"a ring and an empty table", &fakeSecretStore{hasKey: true, chk: engine.SecretKeyCheck{Configured: []int{1}}}, false, 0, 1, "", 0},
		{"a ring and every row on the current key", &fakeSecretStore{hasKey: true, chk: engine.SecretKeyCheck{Total: 9, Configured: []int{2}}}, false, 0, 1, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warned := 0
			err := checkSecretsUsable(context.Background(), tc.f, func(string) { warned++ })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrText)) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErrText)
			}
			if tc.f.countCalls != tc.wantCount || tc.f.chkCalls != tc.wantCensus {
				t.Errorf("CountSecrets x%d, CheckKeyRing x%d; want x%d and x%d",
					tc.f.countCalls, tc.f.chkCalls, tc.wantCount, tc.wantCensus)
			}
			if warned != tc.wantWarnCount {
				t.Errorf("warned %d times, want %d", warned, tc.wantWarnCount)
			}
		})
	}
}
