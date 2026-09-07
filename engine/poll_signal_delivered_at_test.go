package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryPollSignalSelectsDeliveredAt holds every dialect to populating the
// field the replay derivation depends on.
//
// execSession.PollSignal answers from SignalDelivery.DeliveredAtMs compared
// against the session's durable clock. A store that returns the row without
// that column leaves the field at zero, which signalIsVisibleNow treats as
// visible -- deliberately, so test doubles keep working, and dangerously,
// because a real store joining that population would restore #882 exactly
// while every test still passed.
//
// The failure would be silent in the worst way: the code compiles, the query
// succeeds, the poll returns a signal, and the only symptom is that the answer
// changes across a suspension again. That is the defect this replaced.
//
// So the check is on the SQL, per dialect, in the file each store lives in.
func TestEveryPollSignalSelectsDeliveredAt(t *testing.T) {
	// The four implementations and the file each is in. A map rather than a
	// glob: a glob would silently cover fewer files as things move, and this
	// guard's whole point is that silence is the failure mode.
	implementations := map[string]string{
		"PostgresStore": "store_signals.go",
		"MySQLStore":    "mysql_store.go",
		"MSSQLStore":    "mssql_signals_promises.go",
	}

	for receiver, file := range implementations {
		t.Run(receiver, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(".", file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}

			var body string
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "PollSignal" || fn.Recv == nil {
					continue
				}
				star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if id, ok := star.X.(*ast.Ident); ok && id.Name == receiver {
					body = string(src[fn.Pos()-1 : fn.End()-1])
				}
			}
			if body == "" {
				t.Fatalf("no PollSignal found on *%s in %s, so this guard checked "+
					"nothing. If the method moved, move this entry with it.", receiver, file)
			}

			if !strings.Contains(body, "delivered_at") {
				t.Errorf("%s.PollSignal does not select delivered_at, so "+
					"SignalDelivery.DeliveredAtMs stays zero and "+
					"signalIsVisibleNow treats every delivery as visible.\n\n"+
					"That is cleat#882 restored: a poll that answered 'nothing' "+
					"before a suspension answers 'here is the payload' after it. "+
					"The query succeeds and the code compiles, so nothing else "+
					"would report it.", receiver)
			}
			if !strings.Contains(body, "DeliveredAtMs") {
				t.Errorf("%s.PollSignal does not set DeliveredAtMs on the "+
					"SignalDelivery it returns. Selecting the column and then "+
					"dropping it on the floor fails exactly as if it had never "+
					"been selected.", receiver)
			}
		})
	}
}

// TestASignalDeliveredAfterTheDurableClockIsNotVisible is the derivation's own
// known-positive, on the comparison rather than on a database.
//
// Every value here is recorded state, which is the point: the same two inputs
// must give the same answer on every replay, and no query runs.
func TestASignalDeliveredAfterTheDurableClockIsNotVisible(t *testing.T) {
	s := &execSession{nowMs: 1_000_000}

	for _, tc := range []struct {
		name     string
		delivery SignalDelivery
		want     bool
		why      string
	}{
		{"delivered before the durable clock", SignalDelivery{DeliveredAtMs: 999_000}, true,
			"the original execution saw it, so every replay must"},
		{"delivered exactly at the durable clock", SignalDelivery{DeliveredAtMs: 1_000_000}, true,
			"at is not after; the boundary belongs to the past"},
		{"delivered after the durable clock", SignalDelivery{DeliveredAtMs: 1_000_001}, false,
			"the original execution could not have seen it -- this is #882"},
		{"no timestamp at all", SignalDelivery{DeliveredAtMs: 0}, true,
			"a test double, kept visible so doubles behave as before"},
	} {
		if got := s.signalIsVisibleNow(tc.delivery); got != tc.want {
			t.Errorf("%s: visible=%v, want %v -- %s", tc.name, got, tc.want, tc.why)
		}
	}
}
