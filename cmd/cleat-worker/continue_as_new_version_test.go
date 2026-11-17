package main

// cleat#4041: the version a continue_as_new asked for is the version the next run starts on.
//
// Until the fix the worker passed wf.DefVersion to ContinueAsNew unconditionally, so continue_as_new_versioned was
// accepted, recorded, replayed and then ignored. These tests are on the DECISION (continuationVersion), which is
// where the behaviour is, and run against a mock store so each outcome is reachable: a real database can serve
// "deployed" and "not deployed" but not "the read failed". The end-to-end claim, that a chain on a real worker
// and each dialect's store ends on the requested version, is asserted by the TypeScript real-worker test.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cleat-team/cleat/engine"
)

type versionStore struct {
	deployed map[int]bool
	err      error
	asked    []int
}

func (v *versionStore) ValidateVersion(_ context.Context, _ string, version int) (bool, error) {
	v.asked = append(v.asked, version)
	return v.deployed[version], v.err
}

func TestContinuationVersion(t *testing.T) {
	wf := &engine.WorkflowInstance{ID: "r1", DefName: "chain", DefVersion: 1}
	cases := []struct {
		name        string
		requested   int
		deployed    map[int]bool
		readErr     error
		wantVersion int
		wantRefusal string // substring; "" means none
		wantErr     bool
		wantAsked   int // how many times the store was consulted
	}{
		{name: "plain continue stays on the current version", requested: 0, wantVersion: 1},
		{name: "the same version needs no lookup", requested: 1, wantVersion: 1},
		{name: "a deployed version is honoured", requested: 2, deployed: map[int]bool{2: true}, wantVersion: 2, wantAsked: 1},
		{name: "an undeployed version is refused by name", requested: 3, deployed: map[int]bool{2: true}, wantRefusal: `version 3 of "chain"`, wantAsked: 1},
		{name: "a failed read is an error, not a refusal", requested: 2, readErr: errors.New("connection reset"), wantErr: true, wantAsked: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &versionStore{deployed: c.deployed, err: c.readErr}
			v, refusal, err := continuationVersion(context.Background(), st, wf, c.requested)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if refusal != "" {
					t.Fatalf("a failed read produced a refusal %q: it would fail a run over a transient error", refusal)
				}
				return
			}
			if c.wantRefusal != "" {
				if !strings.Contains(refusal, c.wantRefusal) {
					t.Fatalf("refusal = %q, want it to name %s", refusal, c.wantRefusal)
				}
				if !strings.Contains(refusal, "rather than continued on version 1") {
					t.Errorf("refusal %q does not say it declined to fall back to the current version", refusal)
				}
				return
			}
			if refusal != "" {
				t.Fatalf("unexpected refusal %q", refusal)
			}
			if v != c.wantVersion {
				t.Errorf("version = %d, want %d", v, c.wantVersion)
			}
			if len(st.asked) != c.wantAsked {
				t.Errorf("the store was consulted %d times, want %d", len(st.asked), c.wantAsked)
			}
		})
	}
}
