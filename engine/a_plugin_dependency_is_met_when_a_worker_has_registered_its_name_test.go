package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// fakePluginSource answers from a fixed set and counts how often it was asked, so a test can
// say both what a check concluded and whether it needed to look.
type fakePluginSource struct {
	known map[string]bool
	err   error
	calls int
	asked []string
	// overrides are the records written through RecordPluginDepOverride; recordErr makes it fail.
	overrides []PluginDepOverride
	recordErr error
}

func (f *fakePluginSource) RecordPluginDepOverride(_ context.Context, o PluginDepOverride) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.overrides = append(f.overrides, o)
	return nil
}

func (f *fakePluginSource) KnownPlugins(_ context.Context, names []string) (map[string]bool, error) {
	f.calls++
	f.asked = append(f.asked, names...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, n := range names {
		if f.known[n] {
			out[n] = true
		}
	}
	return out, nil
}

func missingNames(t *testing.T, err error) []string {
	t.Helper()
	var unmet *UnmetPluginDepsError
	if !errors.As(err, &unmet) {
		t.Fatalf("%v is not an *UnmetPluginDepsError", err)
	}
	return unmet.MissingNames()
}

// cleat#3600 (owner D52): "met" is "some worker has registered a plugin of that name".
func TestCheckPluginDepsNamesEveryUnregisteredPluginAndWhichDeclarationNamedIt(t *testing.T) {
	src := &fakePluginSource{known: map[string]bool{"registered": true}}
	binary := map[string]string{"registered": ">=1", "only-binary": ">=2.0", "both": ">=3"}
	row := map[string]string{"registered": ">=1", "only-row": "1.x", "both": ">=3.1"}

	err := CheckPluginDeps(context.Background(), src, binary, row)
	if !errors.Is(err, ErrUnmetPluginDeps) {
		t.Fatalf("err = %v, want one wrapping ErrUnmetPluginDeps", err)
	}
	var unmet *UnmetPluginDepsError
	errors.As(err, &unmet)
	got := map[string]MissingPlugin{}
	for _, m := range unmet.Missing {
		got[m.Name] = m
	}
	if len(got) != 3 {
		t.Fatalf("missing = %v, want exactly only-binary, only-row and both (and not the registered one)", unmet.MissingNames())
	}
	for name, wantBy := range map[string]string{"only-binary": DeclaredByBinary, "only-row": DeclaredByRow, "both": DeclaredByBinaryAndRow} {
		if got[name].DeclaredBy != wantBy {
			t.Errorf("%s declared by %q, want %q", name, got[name].DeclaredBy, wantBy)
		}
	}
	if got["both"].Constraint != ">=3; >=3.1" {
		t.Errorf("both declared with differing constraints shows %q, want both of them", got["both"].Constraint)
	}
	// Every missing name is in the one message, so one refusal says all of it.
	for _, n := range []string{"only-binary", "only-row", "both"} {
		if !strings.Contains(err.Error(), `"`+n+`"`) {
			t.Errorf("the error text %q does not name %s", err.Error(), n)
		}
	}
	if src.calls != 1 {
		t.Errorf("the source was asked %d times, want once for every name", src.calls)
	}
}

func TestCheckPluginDepsPassesWhenEveryDeclaredPluginIsKnown(t *testing.T) {
	src := &fakePluginSource{known: map[string]bool{"a": true, "b": true}}
	if err := CheckPluginDeps(context.Background(), src, map[string]string{"a": ""}, map[string]string{"b": ">=1"}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// Nothing declared means nothing to ask, so a store with no registry is not consulted and
// not refused. This is what keeps every definition without plugin_deps deploying as before.
func TestCheckPluginDepsWithNothingDeclaredDoesNotTouchTheSource(t *testing.T) {
	src := &fakePluginSource{}
	for _, tc := range []struct{ binary, row map[string]string }{{nil, nil}, {map[string]string{}, map[string]string{}}} {
		if err := CheckPluginDeps(context.Background(), src, tc.binary, tc.row); err != nil {
			t.Fatalf("err = %v", err)
		}
	}
	if err := CheckPluginDeps(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("a nil source with nothing declared must pass, got %v", err)
	}
	if src.calls != 0 {
		t.Errorf("the source was asked %d times for nothing", src.calls)
	}
}

// A store that cannot say which plugins are known must not read as "all of them".
func TestCheckPluginDepsFailsClosedWithoutASource(t *testing.T) {
	err := CheckPluginDeps(context.Background(), nil, map[string]string{"x": ""}, nil)
	if !errors.Is(err, ErrPluginSourceUnavailable) {
		t.Fatalf("err = %v, want ErrPluginSourceUnavailable", err)
	}
}

func TestCheckPluginDepsReportsAnUnreadableRegistryAsThatAndNotAsAMissingPlugin(t *testing.T) {
	boom := errors.New("connection reset")
	err := CheckPluginDeps(context.Background(), &fakePluginSource{err: boom}, map[string]string{"x": ""}, nil)
	if !errors.Is(err, boom) || errors.Is(err, ErrUnmetPluginDeps) {
		t.Fatalf("err = %v, want the read error and not ErrUnmetPluginDeps", err)
	}
}

func TestEnforcePluginDepsAllowMissingRecordsLogsAndPassesButNotTheOtherFailures(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	deps := map[string]string{"never-registered": ">=1", "also-never": ""}
	about := PluginDepOverride{TenantID: "t-1", Definition: "wf", Version: 3, Via: "rest", Actor: "alice"}
	opts := func(allow bool, rec OverrideRecorder) EnforceOptions {
		return EnforceOptions{AllowMissing: allow, Log: log, Recorder: rec, Override: about}
	}

	// Without the override: refused, nothing logged, nothing recorded.
	src := &fakePluginSource{}
	if err := EnforcePluginDeps(context.Background(), src, deps, nil, opts(false, src)); !errors.Is(err, ErrUnmetPluginDeps) {
		t.Fatalf("without the override err = %v, want ErrUnmetPluginDeps", err)
	}
	if buf.Len() != 0 || len(src.overrides) != 0 {
		t.Fatalf("a refusal logged %q and recorded %d overrides; only an override does either", buf.String(), len(src.overrides))
	}

	// With it: passes, the use is RECORDED with who, what and which names, and logged at WARN.
	if err := EnforcePluginDeps(context.Background(), src, deps, nil, opts(true, src)); err != nil {
		t.Fatalf("with the override err = %v, want nil", err)
	}
	if len(src.overrides) != 1 {
		t.Fatalf("recorded %d overrides, want exactly one", len(src.overrides))
	}
	rec := src.overrides[0]
	if rec.TenantID != "t-1" || rec.Definition != "wf" || rec.Version != 3 || rec.Via != "rest" || rec.Actor != "alice" {
		t.Errorf("the record = %+v, lost who or what it is about", rec)
	}
	names := map[string]bool{}
	for _, m := range rec.Missing {
		names[m.Name] = true
	}
	if len(names) != 2 || !names["never-registered"] || !names["also-never"] {
		t.Errorf("the record's missing = %+v, want both plugin names", rec.Missing)
	}
	out := buf.String()
	for _, want := range []string{"level=WARN", "never-registered", "also-never", "allow-missing-plugins", "definition=wf", "via=rest"} {
		if !strings.Contains(out, want) {
			t.Errorf("the override's log line %q lacks %q", out, want)
		}
	}

	// An override that cannot be recorded is not honoured: no recorder, and a recorder that fails.
	if err := EnforcePluginDeps(context.Background(), &fakePluginSource{}, deps, nil, opts(true, nil)); !errors.Is(err, ErrOverrideNotRecordable) {
		t.Errorf("an override with no recorder = %v, want ErrOverrideNotRecordable", err)
	}
	boomRec := errors.New("audit table unwritable")
	failing := &fakePluginSource{recordErr: boomRec}
	if err := EnforcePluginDeps(context.Background(), failing, deps, nil, opts(true, failing)); !errors.Is(err, boomRec) {
		t.Errorf("an override whose record fails = %v, want that error (the override must not be honoured)", err)
	}

	// The override does not cover "the check could not be made", and records nothing then.
	none := &fakePluginSource{}
	if err := EnforcePluginDeps(context.Background(), nil, deps, nil, opts(true, none)); !errors.Is(err, ErrPluginSourceUnavailable) {
		t.Errorf("a nil source under the override = %v, want ErrPluginSourceUnavailable", err)
	}
	boom := errors.New("registry unreadable")
	if err := EnforcePluginDeps(context.Background(), &fakePluginSource{err: boom}, deps, nil, opts(true, none)); !errors.Is(err, boom) {
		t.Errorf("an unreadable registry under the override = %v, want that error", err)
	}
	if len(none.overrides) != 0 {
		t.Errorf("recorded %d overrides for a check that could not be made", len(none.overrides))
	}

	// Registered plugins need no override and leave no record even when it is asked for.
	ok := &fakePluginSource{known: map[string]bool{"never-registered": true, "also-never": true}}
	if err := EnforcePluginDeps(context.Background(), ok, deps, nil, opts(true, ok)); err != nil {
		t.Fatalf("registered plugins = %v, want nil", err)
	}
	if len(ok.overrides) != 0 {
		t.Errorf("recorded %d overrides for a deploy that needed none", len(ok.overrides))
	}
}

func TestCheckMemberPluginDepsNamesEachMemberThatDeclaredAMissingPlugin(t *testing.T) {
	src := &fakePluginSource{known: map[string]bool{"known": true}}
	err := CheckMemberPluginDeps(context.Background(), src, "lib", []MemberPluginDeps{
		{Name: "a", Version: 1, Raw: []byte(`{"missing":">=1","known":"1"}`)},
		{Name: "b", Version: 2, Raw: []byte(`{"missing":"2"}`)},
		{Name: "c", Version: 1, Raw: []byte(`{}`)},
		{Name: "d", Version: 1, Raw: nil},
		{Name: "e", Version: 1, Raw: []byte(`null`)},
	})
	var unmet *UnmetPluginDepsError
	if !errors.As(err, &unmet) {
		t.Fatalf("err = %v, want an *UnmetPluginDepsError", err)
	}
	if len(unmet.Missing) != 2 {
		t.Fatalf("missing = %+v, want one entry per declaring member (a and b), none for the known plugin", unmet.Missing)
	}
	for i, wantBy := range []string{`member "a" version 1 of library "lib"`, `member "b" version 2 of library "lib"`} {
		if unmet.Missing[i].Name != "missing" || unmet.Missing[i].DeclaredBy != wantBy {
			t.Errorf("entry %d = %+v, want missing declared by %s", i, unmet.Missing[i], wantBy)
		}
	}
	if src.calls != 1 {
		t.Errorf("the source was asked %d times, want once for the whole bundle", src.calls)
	}
}

func TestCheckMemberPluginDepsRefusesAnUnreadableDeclarationAndNamesTheMember(t *testing.T) {
	err := CheckMemberPluginDeps(context.Background(), &fakePluginSource{}, "lib", []MemberPluginDeps{
		{Name: "broken", Version: 3, Raw: []byte(`["not","an","object"]`)},
	})
	if err == nil || !strings.Contains(err.Error(), `"broken"`) || errors.Is(err, ErrUnmetPluginDeps) {
		t.Fatalf("err = %v, want a parse error naming the member (an unreadable declaration is not 'no dependencies')", err)
	}
}

func TestCheckMemberPluginDepsWithNoDeclarationsDoesNotTouchTheSource(t *testing.T) {
	src := &fakePluginSource{}
	if err := CheckMemberPluginDeps(context.Background(), src, "lib", []MemberPluginDeps{{Name: "a", Version: 1, Raw: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := CheckMemberPluginDeps(context.Background(), nil, "lib", []MemberPluginDeps{{Name: "a", Version: 1, Raw: []byte(`{}`)}}); err != nil {
		t.Fatalf("a nil source with nothing declared must pass, got %v", err)
	}
	if src.calls != 0 {
		t.Errorf("asked %d times for nothing", src.calls)
	}
}
