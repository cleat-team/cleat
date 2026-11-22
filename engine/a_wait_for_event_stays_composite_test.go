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
// wait_for_event.go may take from the session are the fields and methods named in waitForEventSessionUses, and it may mention no EventType constant at all. `isReplay` may be read in ONE place, the
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
	var v []string
	isReplayUses := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if strings.HasPrefix(x.Name, "EventType") {
				v = append(v, "mentions "+x.Name+": a record type of its own")
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "s" {
				if !waitForEventSessionUses[x.Sel.Name] && !own[x.Sel.Name] {
					v = append(v, "uses s."+x.Sel.Name+": not something a composite call takes from the session")
				}
				if x.Sel.Name == "isReplay" {
					isReplayUses++
				}
			}
		}
		return true
	})
	if isReplayUses > 1 {
		v = append(v, "reads s.isReplay more than once: a replay arm of its own")
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
		"a second replay decision": {"\tif s.isReplay { return 0 }", "reads s.isReplay more than once"},
		"counts a step":            {"\ts.stepCount++", "uses s.stepCount"},
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
