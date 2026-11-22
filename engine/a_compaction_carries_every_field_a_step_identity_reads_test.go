package engine

// cleat#4325, cleat#4231. A step's Identity function compares what the call ASKED with what the record SAYS, so every EventRecord field it reads must still be on the record
// after the history went through compaction: a compacted record that comes back without one is not another call's, but the comparison reads it as one and fails every
// replay. Two defects of exactly that shape were found by reading (await_all_children's Request, signal_workflow's RunID), a third kind of reading is what this replaces: an
// audit of eighteen Identity functions by eye, which missed four of them to a regex that could not parse a named function.
//
// This test does not parse anything. An Identity is a closure, so it is PROBED: for each EventRecord field, run the spec's Identity on a record with the field zero and on one
// with it set, against a few argument shapes, and a field whose presence changes the answer is a field the Identity reads. Then each such field is carried through a real
// compaction round trip for the event type the spec checks. A spec whose Identity the probe sees nothing in is reported, not passed: a probe that finds nothing in an
// Identity that must read SOMETHING has not looked, and a check that cannot say so reads cleanest exactly where it measured least.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// probeValue is the non-zero value the probe sets a field to, by the field's kind; ok is false for a kind the probe cannot set.
func probeValue(f reflect.StructField) (v reflect.Value, ok bool) {
	switch f.Type.Kind() {
	case reflect.String:
		return reflect.ValueOf("probe-" + f.Name).Convert(f.Type), true
	case reflect.Int:
		return reflect.ValueOf(7).Convert(f.Type), true
	case reflect.Int64:
		return reflect.ValueOf(int64(7)).Convert(f.Type), true
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(f.Type), true
	}
	return reflect.Value{}, false
}

// identityProbeArgs are the call shapes the probe runs an Identity against. A field is read when SOME shape tells the record with it set from the record without: a zero call
// shows a comparison with an empty argument, and a call whose every argument is the probe value shows the comparison that passes only when the field matches.
func identityProbeArgs(valueOf string) []stepArgs {
	return []stepArgs{
		{},
		{s1: valueOf, s2: valueOf, s3: valueOf, s4: valueOf, i1: 7, i2: 7, i3: 7, i4: 7},
		{s1: "another", s2: "another", s3: "another", s4: "another", i1: 3, i2: 3, i3: 3, i4: 3},
	}
}

// identityReads names the EventRecord fields sp.Identity reads, found by probing; the second result is why it could not probe, when it could not.
func identityReads(sp *stepSpec) (reads []string, unmeasured string) {
	defer func() {
		if r := recover(); r != nil {
			reads, unmeasured = nil, fmt.Sprintf("the Identity panicked under a probe: %v", r)
		}
	}()
	rt := reflect.TypeOf(EventRecord{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || f.Name == "EventType" || f.Name == "Step" {
			continue
		}
		val, ok := probeValue(f)
		if !ok {
			continue // reported by TestEveryEventRecordFieldIsOfAKindTheIdentityProbeCanSet
		}
		base := EventRecord{EventType: sp.Want}
		with := base
		reflect.ValueOf(&with).Elem().Field(i).Set(val)
		valueOf := ""
		if val.Kind() == reflect.String {
			valueOf = val.String()
		}
		for _, a := range identityProbeArgs(valueOf) {
			b0, b1 := base, with
			if sp.Identity(a, &b0) != sp.Identity(a, &b1) {
				reads = append(reads, f.Name)
				break
			}
		}
	}
	return reads, ""
}

// eventTypeConstants maps each EventType constant's NAME to its value, read from types.go, so the source reading can say which record types an Identity names.
func eventTypeConstants(t *testing.T) map[string]EventType {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}
	out := map[string]EventType{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, sp := range gd.Specs {
			vs := sp.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "EventType" {
				continue
			}
			for i, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING && i < len(vs.Names) {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						out[vs.Names[i].Name] = EventType(s)
					}
				}
			}
		}
	}
	if len(out) < 20 {
		t.Fatalf("PRECONDITION FAILED: read %d EventType constants from types.go; the scan is not reading the declarations", len(out))
	}
	return out
}

// identityRead is what the source reading found in one Identity: the EventRecord fields it reads and the record types it names (the constants it mentions).
type identityRead struct {
	fields map[string]bool
	types  map[EventType]bool
}

// identitySourceReads is the SECOND reading, and it is deliberately the loose one: it parses the engine's non-test files, finds each stepSpec literal that names an Identity,
// and collects every `<param>.Field` selector in that function's body where <param> is the record parameter, following a call that passes the record to a package function
// one level into that function (settlementIdentity, lockIdentity and awaitChildIdentity are such). It over-matches on purpose: a field it names that the Identity only touches
// under a condition is still a field compaction has to keep, so over-reading demands more survival and never less. The probe cannot do that, because an Identity that is gated
// on a record's Service and Op (signal_workflow's) returns the same answer for every probe that does not set them.
func identitySourceReads(t *testing.T) map[string]*identityRead {
	t.Helper()
	typeNames := eventTypeConstants(t)
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("PRECONDITION FAILED: no engine source files to read (%v, %d files)", err, len(files))
	}
	var parsed []*ast.File
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: cannot parse %s: %v", f, err)
		}
		parsed = append(parsed, af)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, af := range parsed {
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	eventRecord := reflect.TypeOf(EventRecord{})
	// recParam is the name of a function's second parameter, which is the record in every Identity signature (a stepArgs, then the *EventRecord).
	recParam := func(ft *ast.FuncType) string {
		var names []string
		for _, fl := range ft.Params.List {
			for _, n := range fl.Names {
				names = append(names, n.Name)
			}
		}
		if len(names) >= 2 {
			return names[1]
		}
		return ""
	}
	var collect func(body *ast.BlockStmt, param string, seen map[string]bool, out *identityRead)
	collect = func(body *ast.BlockStmt, param string, seen map[string]bool, out *identityRead) {
		if body == nil || param == "" {
			return
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if et, ok := typeNames[x.Name]; ok {
					out.types[et] = true
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == param {
					if _, real := eventRecord.FieldByName(x.Sel.Name); real {
						out.fields[x.Sel.Name] = true
					}
				}
			case *ast.CallExpr:
				callee, ok := x.Fun.(*ast.Ident)
				if !ok {
					break
				}
				fd := funcs[callee.Name]
				if fd == nil || seen[callee.Name] {
					break
				}
				// the record passed as an argument becomes the callee's parameter at the same position
				for i, arg := range x.Args {
					if id, ok := arg.(*ast.Ident); ok && id.Name == param {
						var names []string
						for _, fl := range fd.Type.Params.List {
							for _, nm := range fl.Names {
								names = append(names, nm.Name)
							}
						}
						if i < len(names) {
							seen[callee.Name] = true
							collect(fd.Body, names[i], seen, out)
						}
					}
				}
			}
			return true
		})
	}
	result := map[string]*identityRead{}
	for _, af := range parsed {
		ast.Inspect(af, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "stepSpec" {
				return true
			}
			var name string
			var ident ast.Expr
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch k := kv.Key.(*ast.Ident); k.Name {
				case "Name":
					if bl, ok := kv.Value.(*ast.BasicLit); ok {
						name = strings.Trim(bl.Value, `"`)
					}
				case "Identity":
					ident = kv.Value
				}
			}
			if name == "" || ident == nil {
				return true
			}
			out := &identityRead{fields: map[string]bool{}, types: map[EventType]bool{}}
			switch v := ident.(type) {
			case *ast.FuncLit:
				collect(v.Body, recParam(v.Type), map[string]bool{}, out)
			case *ast.Ident:
				if fd := funcs[v.Name]; fd != nil {
					collect(fd.Body, recParam(fd.Type), map[string]bool{v.Name: true}, out)
				}
			}
			result[name] = out
			return true
		})
	}
	return result
}

// An Identity that reads a field the probe cannot set would be invisible to it, so a new field of a kind the probe cannot set must stop here instead of passing quietly.
func TestEveryEventRecordFieldIsOfAKindTheIdentityProbeCanSet(t *testing.T) {
	rt := reflect.TypeOf(EventRecord{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || f.Name == "EventType" || f.Name == "Step" || f.Name == "CreatedAt" {
			continue
		}
		if _, ok := probeValue(f); !ok {
			t.Errorf("EventRecord.%s is a %s, which the Identity probe cannot set, so an Identity that read it would not be seen; teach probeValue the kind", f.Name, f.Type)
		}
	}
}

// The probe finds what it is supposed to find. Each row is a spec whose Identity reads a field we know (from its source) and a field it is known NOT to read, and the probe
// must say both: finding nothing is as wrong as finding everything, and a table of known answers is what shows the probe is not blind.
func TestTheIdentityProbeSeesWhatKnownIdentitiesRead(t *testing.T) {
	byName := map[string]*stepSpec{}
	for _, sp := range stepSpecs {
		byName[sp.Name] = sp
	}
	for _, row := range []struct {
		spec          string
		reads, absent []string
	}{
		{"await_all_children", []string{"Request"}, []string{"LockKey", "PromiseID", "RunID"}},
		{"acquire_lock", []string{"LockKey"}, []string{"Request", "RunID"}},
		{"await_child", []string{"RunID"}, []string{"LockKey", "Request"}},
		{"await_promise", []string{"PromiseID"}, []string{"LockKey", "RunID"}},
		{"fetch", []string{"FetchURL"}, []string{"LockKey", "RunID"}},
	} {
		sp, ok := byName[row.spec]
		if !ok || sp.Identity == nil {
			t.Fatalf("PRECONDITION FAILED: no registered spec %q with an Identity; the table of known answers is stale", row.spec)
		}
		got, why := identityReads(sp)
		if why != "" {
			t.Fatalf("%s: UNMEASURED: %s", row.spec, why)
		}
		set := map[string]bool{}
		for _, g := range got {
			set[g] = true
		}
		for _, w := range row.reads {
			if !set[w] {
				t.Errorf("%s: the probe did not see %s, which its Identity reads (it saw %v): the probe is blind to it", row.spec, w, got)
			}
		}
		for _, a := range row.absent {
			if set[a] {
				t.Errorf("%s: the probe reports reading %s, which its Identity does not read (it saw %v): the probe over-reports", row.spec, a, got)
			}
		}
	}
}

// The guard: every field an Identity reads survives a compaction round trip for the event type the spec checks. The fields are the UNION of the two readings above, and a field
// one of them names that the other does not is reported, because the two are meant to disagree only where the source reading over-matches (a field read under a condition).
func TestCompactionCarriesEveryFieldAStepIdentityReads(t *testing.T) {
	source := identitySourceReads(t)
	type found struct {
		spec, field string
		// types is the record types the field was tried on: the ones the Identity names, plus the one the spec records.
		types string
	}
	var checked, bad []found
	var unmeasured, refused, probeOnly []string
	specs := 0
	for _, sp := range stepSpecs {
		if sp.Identity == nil {
			continue
		}
		specs++
		probed, why := identityReads(sp)
		if why != "" {
			unmeasured = append(unmeasured, sp.Name+": "+why)
			continue
		}
		srcRead, ok := source[sp.Name]
		if !ok || len(srcRead.fields) == 0 {
			unmeasured = append(unmeasured, sp.Name+": the source reading found no Identity or no field in it, so this test measured nothing about it")
			continue
		}
		src := srcRead.fields
		union := map[string]bool{}
		for f := range src {
			union[f] = true
		}
		for _, f := range probed {
			if !src[f] {
				// the probe saw a field the source reading did not: that is the loose reading being less loose than the probe, which means one of them is wrong
				probeOnly = append(probeOnly, sp.Name+"."+f)
			}
			union[f] = true
		}
		delete(union, "EventType") // structural, recovered by codeToEventType
		delete(union, "Step")
		if _, ok := compactionRefusedEventTypes[sp.Want]; ok {
			refused = append(refused, sp.Name)
			continue
		}
		names := make([]string, 0, len(union))
		for f := range union {
			names = append(names, f)
		}
		sort.Strings(names)
		rt := reflect.TypeOf(EventRecord{})
		for _, name := range names {
			f, _ := rt.FieldByName(name)
			val, ok := probeValue(f)
			if !ok {
				unmeasured = append(unmeasured, fmt.Sprintf("%s: reads EventRecord.%s, a %s the probe cannot set", sp.Name, name, f.Type))
				continue
			}
			// The types the Identity names, plus the one the spec records: a field it reads only for ANOTHER type (await_signals reads SignalNames on an await_signals record and
			// SignalName on a signal_received one) is carried when it survives for at least one of them. That is weaker than attributing each field to the branch that reads it, and
			// it is the honest limit: a field carried for one named type and dropped for another that reads it is not seen here.
			types := map[EventType]bool{sp.Want: true}
			for et := range srcRead.types {
				if _, refusedType := compactionRefusedEventTypes[et]; !refusedType {
					types[et] = true
				}
			}
			survived, measured := false, false
			for et := range types {
				rec := EventRecord{Step: 0, EventType: et}
				reflect.ValueOf(&rec).Elem().FieldByName(name).Set(val)
				cs, err := extractCompactionStateFrom(nil, []EventRecord{rec})
				if err != nil {
					continue
				}
				out := buildFullHistoryFromCompaction(nil, cs)
				if len(out) != 1 {
					continue
				}
				measured = true
				if reflect.DeepEqual(reflect.ValueOf(out[0]).FieldByName(name).Interface(), val.Interface()) {
					survived = true
					break
				}
			}
			if !measured {
				unmeasured = append(unmeasured, fmt.Sprintf("%s: no record type it names could be compacted with %s set", sp.Name, name))
				continue
			}
			tried := make([]string, 0, len(types))
			for et := range types {
				tried = append(tried, string(et))
			}
			sort.Strings(tried)
			checked = append(checked, found{sp.Name, name, ""})
			if !survived {
				bad = append(bad, found{sp.Name, name, strings.Join(tried, ", ")})
			}
		}
	}
	if specs < 15 {
		t.Fatalf("PRECONDITION FAILED: %d registered specs carry an Identity; this test is not reading the registry", specs)
	}
	sort.Strings(refused)
	sort.Strings(probeOnly)
	for _, b := range bad {
		t.Errorf("spec %s: its Identity reads EventRecord.%s and none of the record types it names (%s) keeps it through compaction, so every replay of a compacted record that carried it "+
			"is read as another call's and diverges (cleat#4325 was await_all_children's Request, cleat#4231 signal_workflow's RunID). Carry the field in compaction.go (capture and expansion).",
			b.spec, b.field, b.types)
	}
	for _, u := range unmeasured {
		t.Errorf("UNMEASURED (this is a failure of the test, not a finding about compaction): %s", u)
	}
	for _, p := range probeOnly {
		t.Errorf("the PROBE saw %s read and the SOURCE reading did not: one of the two readings is wrong about it (the source reading is meant to over-match, never under-match)", p)
	}
	lostIn := map[string]bool{}
	for _, b := range bad {
		lostIn[b.spec] = true
	}
	t.Logf("%d specs with an Identity; %d (spec, field) pairs round-tripped; %d refused-type specs not compacted: [%s]; %d pair(s) not carried, in %d spec(s)", specs, len(checked), len(refused),
		strings.Join(refused, " "), len(bad), len(lostIn))
}
