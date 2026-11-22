package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// cleat#4152, G3: cleat_wait_for_event NEEDS NO stepSpec because it is composite: a loop over PluginCall and DurableAwaitSignals, which are on the step dispatcher. That stays true only while the file
// writes no record of its own and decides nothing about replay. This test fails the day it does, and says what to do instead (give the new record a spec, which means moving the call).
//
// THE RULE, as a whitelist rather than a blacklist, because a list of forbidden names is the limit of the check and a new way to write a record would not be on it: the only things
// wait_for_event.go may take from the session (held under ANY name: every *execSession receiver or parameter counts) are the fields and methods named in waitForEventSessionUses, it may not
// hand the session to another function or alias it, and it may mention no EventType constant at all. `isReplay` may be read in ONE place, the
// guard at the top of WaitForEvent, and nowhere else. A record is written through the session (recordEvent, history, stepCount, ...) or through an EventType, and neither is on the list.

var waitForEventSessionUses = map[string]bool{
	"nowMs":               true, // the durable clock the deadline is derived from
	"suspendErr":          true, // whether the await suspended the run
	"PluginCall":          true, // the dispatcher's plugin call
	"DurableAwaitSignals": true, // the dispatcher's await
	"writeResult":         true, // the failure message into the caller's buffer
	"stopBeforeNewWork":   true, // the one guard before fresh work
	"isReplay":            true, // allowed ONCE: see waitForEventCompositeViolations
}

// waitForEventCompositeViolations reads Go source and returns what breaks the composite rule, sorted.
//
// THE SESSION IS WHATEVER IS TYPED *execSession, not whatever is called `s`: in each function, every receiver or parameter of that type is a session identifier (cleat-review3 found that
// a helper `func note(x *execSession) { x.history = ... }`, called from WaitForEvent, passed a scan that only looked at `s`). Two things are then refused: a selector on a session identifier
// that is not on the whitelist, and a session identifier used as a VALUE (passed to a call, assigned, returned), because what a function handed the session does with it is not in this file's
// text. THE LIMIT: the whitelist is by NAME, so `s.PluginCall` is allowed because PluginCall is on it, whatever it does elsewhere; the scan reads this file, not the files it calls into.
func waitForEventCompositeViolations(src string) []string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "wait_for_event.go", src, 0)
	if err != nil {
		return []string{"UNMEASURED: the source does not parse: " + err.Error()}
	}
	// The file's own helper methods on the session are the file's, not something it takes from the session: allowed, because declaring one cannot write a record without naming something below.
	own := map[string]bool{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil {
			own[fd.Name.Name] = true
		}
	}
	isSession := func(e ast.Expr) bool {
		st, ok := e.(*ast.StarExpr)
		if !ok {
			return false
		}
		id, ok := st.X.(*ast.Ident)
		return ok && id.Name == "execSession"
	}
	var v []string
	isReplayUses := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		// The session identifiers of this function, and the declaring idents that must not be read as uses.
		session := map[string]bool{}
		declaring := map[*ast.Ident]bool{}
		collect := func(fl *ast.FieldList) {
			if fl == nil {
				return
			}
			for _, fld := range fl.List {
				for _, n := range fld.Names {
					declaring[n] = true
					if isSession(fld.Type) {
						session[n.Name] = true
					}
				}
			}
		}
		collect(fd.Recv)
		collect(fd.Type.Params)
		// Selector bases are checked below; a bare use of a session identifier is anything else.
		bases := map[*ast.Ident]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if x, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := x.X.(*ast.Ident); ok {
					bases[id] = true
				}
			}
			return true
		})
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if strings.HasPrefix(x.Name, "EventType") {
					v = append(v, "mentions "+x.Name+": a record type of its own")
				}
				if session[x.Name] && !bases[x] && !declaring[x] {
					v = append(v, "uses the session as a value ("+x.Name+" in "+fd.Name.Name+"): passed, assigned or returned, so what is done with it is not in this file")
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && session[id.Name] {
					if !waitForEventSessionUses[x.Sel.Name] && !own[x.Sel.Name] {
						v = append(v, "uses "+id.Name+"."+x.Sel.Name+": not something a composite call takes from the session")
					}
					if x.Sel.Name == "isReplay" {
						isReplayUses++
					}
				}
			}
			return true
		})
	}
	if isReplayUses > 1 {
		v = append(v, "reads isReplay more than once: a replay arm of its own")
	}
	sort.Strings(v)
	return v
}

func TestWaitForEventStaysComposite(t *testing.T) {
	b, err := os.ReadFile("wait_for_event.go")
	if err != nil {
		t.Fatalf("UNMEASURED: %v", err)
	}
	if v := waitForEventCompositeViolations(string(b)); len(v) > 0 {
		t.Errorf("wait_for_event.go is no longer composite:\n  %s\n\nIt has been a loop over PluginCall and DurableAwaitSignals, with no record type and no replay arm of its own, "+
			"which is why it has no stepSpec (docs/design/replay-step-dispatcher.md). A record or a replay decision here is a step: give it a spec and move the call.", strings.Join(v, "\n  "))
	}
}

// The known positives: a source that breaks the rule each way must be reported, and the clean shape must not. Without these the scan could pass a file it cannot see into.
func TestTheCompositeScanSeesEachWayToBreakTheRule(t *testing.T) {
	head := "package engine\nfunc (s *execSession) WaitForEvent() int64 {\n\tif !s.isReplay && s.stopBeforeNewWork() { return 0 }\n"
	tail := "\n\treturn s.PluginCall() + s.DurableAwaitSignals() + int64(s.nowMs)\n}\n"
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"clean":                    {"", ""},
		"records an event":         {"\ts.recordEvent(rec)", "uses s.recordEvent"},
		"reads the history":        {"\t_ = s.history", "uses s.history"},
		"names an event type":      {"\t_ = EventTypePluginCall", "mentions EventTypePluginCall"},
		"a second replay decision": {"\tif s.isReplay { return 0 }", "reads isReplay more than once"},
		"counts a step":            {"\ts.stepCount++", "uses s.stepCount"},
		// cleat-review3's case: the write is in a helper whose session parameter is not called s.
		"a helper with another name": {"\tnoteWaitForEvent(s)\n}\nfunc noteWaitForEvent(x *execSession) {\n\tx.history = append(x.history, EventRecord{})", "uses x.history"},
		"passes the session on":      {"\tnoteWaitForEvent(s)", "uses the session as a value (s in WaitForEvent)"},
		"aliases the session":        {"\tt := s\n\t_ = t", "uses the session as a value (s in WaitForEvent)"},
		"another receiver name":      {"\t_ = 0\n}\nfunc (x *execSession) helper() {\n\tx.recordEvent(EventRecord{})", "uses x.recordEvent"},
	} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(waitForEventCompositeViolations(head+tc.body+tail), "\n")
			if tc.want == "" && got != "" {
				t.Errorf("the clean shape was reported: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("a source that %s was not reported (want %q); the scan said: %q", name, tc.want, got)
			}
		})
	}
	// And the real file is the control: the scan runs over it and finds nothing, so it is not a scan that can only say no.
	if strings.Join(waitForEventCompositeViolations(head+tail), "") != "" {
		t.Error("the minimal clean shape was reported")
	}
}
