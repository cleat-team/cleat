//go:build ignore

// Demo: Go AST source-to-source transformation for durable execution.
//
// This is a standalone proof-of-concept. Build and run it:
//   go build -o ast_demo ast_transform_demo.go && ./ast_demo
//
// It shows:
//   1. A user-written workflow in "near-standard" Go
//   2. Parsing it with go/parser
//   3. Walking the AST to find API calls, checkpoints, and branches
//   4. Resynthesizing a durable version with injected machinery

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// ---------------------------------------------------------------------------
// Step 1 — The user's source code (what a developer would write)
// ---------------------------------------------------------------------------

const userSource = `
package workflows

import (
	"context"
	"fmt"
)

// OrderWorkflow orchestrates placing an order. The developer writes this in
// near-standard Go — no Temporal SDK imports, no special annotations.
func OrderWorkflow(ctx context.Context, userID string, items []string) (string, error) {
	// -- Validate input --
	if len(items) == 0 {
		return "", fmt.Errorf("no items in order")
	}

	// -- Call inventory service to reserve stock --
	inventoryResult, err := ReserveInventory(ctx, userID, items)
	if err != nil {
		return "", fmt.Errorf("inventory reservation failed: %w", err)
	}
	if inventoryResult.Status != "reserved" {
		// Compensating action: release the partial reservation
		ReleaseInventory(ctx, inventoryResult.ReservationID)
		return "", fmt.Errorf("inventory status %q, expected reserved", inventoryResult.Status)
	}

	// -- Call payment service --
	charge, err := ChargeCustomer(ctx, userID, inventoryResult.TotalCents)
	if err != nil {
		// Compensate
		ReleaseInventory(ctx, inventoryResult.ReservationID)
		return "", fmt.Errorf("payment failed: %w", err)
	}

	// -- Create shipment --
	trackingID, err := CreateShipment(ctx, inventoryResult.ReservationID, userID)
	if err != nil {
		// Compensate: refund and release
		RefundCustomer(ctx, charge.ChargeID)
		ReleaseInventory(ctx, inventoryResult.ReservationID)
		return "", fmt.Errorf("shipping failed: %w", err)
	}

	// -- Send confirmation email (fire-and-forget, best-effort) --
	_ = SendConfirmation(ctx, userID, trackingID)

	return trackingID, nil
}
`

// ---------------------------------------------------------------------------
// Step 2 — AST types we inject into the transformed output
// ---------------------------------------------------------------------------

// A checkpoint marks a position in the workflow where we save state.
// After transformation, every significant step is bracketed by checkpoints.
type Checkpoint struct {
	ID   string
	Line int
}

// An API call that we want to cache/memoize so replay skips the real call.
type CachedCall struct {
	FuncName string
	ArgExprs []string // stringified argument expressions
	ResultTo string   // variable the result is assigned to
}

// A compensating action (rollback) associated with a completed step.
type Compensation struct {
	TriggerStep string // the step whose failure triggers this
	Action      string // the compensating call
}

// ---------------------------------------------------------------------------
// Step 3 — The analyzer: walk the AST and extract the workflow structure
// ---------------------------------------------------------------------------

type Analyzer struct {
	Checkpoints  []Checkpoint
	CachedCalls  []CachedCall
	Compensations []Compensation
	Errors       []string
	fset         *token.FileSet
}

func (a *Analyzer) Analyze(source string) {
	a.fset = token.NewFileSet()
	f, err := parser.ParseFile(a.fset, "workflow.go", source, parser.ParseComments)
	if err != nil {
		a.Errors = append(a.Errors, fmt.Sprintf("parse error: %v", err))
		return
	}

	// Find every function declaration — each is a potential workflow entry point.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		a.analyzeFunc(fn)
	}
}

func (a *Analyzer) analyzeFunc(fn *ast.FuncDecl) {
	if fn.Body == nil {
		return
	}

	var lastCheckpointID string
	checkpointCounter := 0

	// Walk every statement in the function body.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {

		// --- Assignment statements (call results) ---
		case *ast.AssignStmt:
			for i, rhs := range stmt.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok {
					continue
				}
				funcName := exprToString(call.Fun)
				if !isAPICall(funcName) {
					continue
				}

				// Record the cached call.
				var lhsName string
				if i < len(stmt.Lhs) {
					lhsName = exprToString(stmt.Lhs[i])
				}

				checkpointCounter++
				cpID := fmt.Sprintf("%s_cp%d", fn.Name.Name, checkpointCounter)

				var argStrs []string
				for _, arg := range call.Args {
					argStrs = append(argStrs, exprToString(arg))
				}

				a.Checkpoints = append(a.Checkpoints, Checkpoint{ID: cpID, Line: a.fset.Position(call.Pos()).Line})
				a.CachedCalls = append(a.CachedCalls, CachedCall{
					FuncName: funcName,
					ArgExprs: argStrs,
					ResultTo: lhsName,
				})

				// Detect compensation pattern: if there's a previous successful step
				// and this is the next step, link them for rollback.
				if lastCheckpointID != "" {
					comp := findCompensation(funcName, lastCheckpointID)
					if comp != "" {
						a.Compensations = append(a.Compensations, Compensation{
							TriggerStep: cpID,
							Action:      comp,
						})
					}
				}
				lastCheckpointID = cpID
			}

		// --- Expression statements (bare calls) ---
		case *ast.ExprStmt:
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				break
			}
			funcName := exprToString(call.Fun)
			if !isAPICall(funcName) {
				break
			}
			checkpointCounter++
			cpID := fmt.Sprintf("%s_cp%d", fn.Name.Name, checkpointCounter)
			a.Checkpoints = append(a.Checkpoints, Checkpoint{ID: cpID, Line: a.fset.Position(call.Pos()).Line})
			a.CachedCalls = append(a.CachedCalls, CachedCall{
				FuncName: funcName,
				ResultTo: "_", // fire-and-forget
			})
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Step 4 — The resynthesizer: generate the durable version
// ---------------------------------------------------------------------------

func (a *Analyzer) Resynthesize(source string) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workflow.go", source, parser.ParseComments)
	if err != nil {
		return fmt.Sprintf("// ERROR: %v\n%s", err, source)
	}

	var buf bytes.Buffer

	// --- Emit the package header with our runtime import ---
	buf.WriteString(`// Code generated by durable-workflow transformer. DO NOT EDIT.
// This is the durable version of the workflow — every external call is cached,
// and every significant step is checkpointed for resume-after-failure.

package workflows

import (
	"context"
	"fmt"

	durable "github.com/example/durable-runtime"
)

`)

	// We walk the original AST and emit transformed code.
	// For brevity, this demo hand-generates the key transformations rather
	// than doing a full AST-printer rewrite (which would be ~10x more code).

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		a.emitTransformedFunc(&buf, fn)
	}

	return buf.String()
}

func (a *Analyzer) emitTransformedFunc(buf *bytes.Buffer, fn *ast.FuncDecl) {
	// --- Signature: keep the same, we're transparent to callers ---
	buf.WriteString(fmt.Sprintf("func %s(ctx context.Context, userID string, items []string) (string, error) {\n", fn.Name.Name))

	// --- Prologue: restore state if resuming ---
	buf.WriteString(`	// --- Durable prologue (injected) ---
	state, isReplay := durable.LoadCheckpoint(ctx, "` + fn.Name.Name + `")
	if isReplay {
		// Rebuild local state from the checkpoint.
		// In a real implementation this deserializes all live variables.
		_ = state
	}
`)

	// --- Walk the body and emit transformed statements ---
	if fn.Body != nil {
		for _, stmt := range fn.Body.List {
			a.emitStmt(buf, stmt, fn.Name.Name)
		}
	}

	buf.WriteString("}\n\n")
}

func (a *Analyzer) emitStmt(buf *bytes.Buffer, stmt ast.Stmt, funcName string) {
	switch s := stmt.(type) {

	case *ast.AssignStmt:
		for _, rhs := range s.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok || !isAPICall(exprToString(call.Fun)) {
				// Not an API call — emit as-is (via go/format would be better)
				buf.WriteString(fmt.Sprintf("\t// (original: %s)\n", formatNode(s)))
				continue
			}

			funcNameStr := exprToString(call.Fun)
			cacheKey := fmt.Sprintf("%s_%s", funcName, argsKey(call.Args))

			// --- The injected durability wrapper ---
			buf.WriteString(fmt.Sprintf(`
	// --- Durable call to %s (injected) ---
	var __result interface{}
	var __err error
	if cached, ok := durable.GetCached(ctx, "%s"); ok {
		// Replay: use the cached result, skip the real API call.
		__result = cached.Result
		__err = cached.Err
	} else {
		// First execution: make the real call and cache the result.
		__result, __err = durable.CallAndCache(ctx, "%s", func() (interface{}, error) {
`, funcNameStr, cacheKey, cacheKey))

			// The actual call (wrapped in a closure)
			buf.WriteString(fmt.Sprintf("\t\t\treturn %s(%s)\n", funcNameStr, argsStr(call.Args)))
			buf.WriteString(`		})
	}
`)

			// Result assignment
			if len(s.Lhs) > 0 {
				lhsName := exprToString(s.Lhs[0])
				buf.WriteString(fmt.Sprintf("\t%s := __result.(%s)\n", lhsName, inferType(funcNameStr, 0)))
			}
			buf.WriteString("\t_ = __err // error handling preserved in real impl\n")

			// --- Checkpoint after the call ---
			buf.WriteString(fmt.Sprintf(`
	// --- Checkpoint (injected) ---
	if err := durable.SaveCheckpoint(ctx, "%s", map[string]interface{}{
		// In production this serializes every live local variable.
		"last_result": __result,
	}); err != nil {
		return "", fmt.Errorf("checkpoint failed: %%w", err)
	}
`, funcName+"_cp"))
		}

	case *ast.ExprStmt:
		call, ok := s.X.(*ast.CallExpr)
		if !ok || !isAPICall(exprToString(call.Fun)) {
			buf.WriteString(fmt.Sprintf("\t// (original expr: %s)\n", formatNode(s)))
			return
		}
		buf.WriteString(fmt.Sprintf(`
	// --- Durable fire-and-forget call to %s (injected) ---
	durable.CallBestEffort(ctx, "%s", func() error {
		return %s(%s)
	})
`, exprToString(call.Fun), exprToString(call.Fun), exprToString(call.Fun), argsStr(call.Args)))

	case *ast.IfStmt:
		// Emit the condition as-is, transform the body
		buf.WriteString(fmt.Sprintf("\tif %s {\n", formatNode(s.Cond)))
		if s.Body != nil {
			for _, bs := range s.Body.List {
				a.emitStmt(buf, bs, funcName)
			}
		}
		buf.WriteString("\t}\n")

	case *ast.ReturnStmt:
		buf.WriteString(fmt.Sprintf(`
	// --- Final checkpoint before return (injected) ---
	durable.MarkComplete(ctx, "%s")
	return nil, nil // transformed
`, funcName))

	default:
		buf.WriteString(fmt.Sprintf("\t// (stmt type %T preserved in real impl)\n", s))
	}
}

// ---------------------------------------------------------------------------
// Step 5 — Helpers
// ---------------------------------------------------------------------------

func isAPICall(name string) bool {
	// Heuristic: calls that start with a capital letter and are not builtins
	// are assumed to be external API calls that need caching.
	apiCalls := map[string]bool{
		"ReserveInventory":  true,
		"ReleaseInventory":  true,
		"ChargeCustomer":    true,
		"RefundCustomer":    true,
		"CreateShipment":    true,
		"SendConfirmation":  true,
	}
	return apiCalls[name]
}

func exprToString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprToString(x.X) + "." + x.Sel.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.CallExpr:
		return exprToString(x.Fun) + "(...)"
	default:
		return fmt.Sprintf("<%T>", e)
	}
}

func argsKey(args []ast.Expr) string {
	var parts []string
	for _, a := range args {
		parts = append(parts, exprToString(a))
	}
	return strings.Join(parts, "_")
}

func argsStr(args []ast.Expr) string {
	var parts []string
	for _, a := range args {
		parts = append(parts, exprToString(a))
	}
	return strings.Join(parts, ", ")
}

func findCompensation(stepName string, prevCheckpoint string) string {
	// Simple lookup table — a real implementation would analyze the AST
	// to find the compensating action associated with each step.
	compensations := map[string]string{
		"ChargeCustomer": "RefundCustomer",
		"CreateShipment": "RefundCustomer + ReleaseInventory",
	}
	return compensations[stepName]
}

func inferType(funcName string, resultIndex int) string {
	types := map[string]string{
		"ReserveInventory": "InventoryResult",
		"ChargeCustomer":   "ChargeResult",
		"CreateShipment":   "string",
	}
	if t, ok := types[funcName]; ok {
		return t
	}
	return "interface{}"
}

func formatNode(n ast.Node) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, token.NewFileSet(), n); err != nil {
		return fmt.Sprintf("<%T>", n)
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// Step 6 — Main: tie it together
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Durable Workflow — AST Transformation Demo                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ---- Phase 1: Parse and analyze ----
	fmt.Println("── Phase 1: Parsing user source with go/parser ──")
	fmt.Println()

	a := &Analyzer{}
	a.Analyze(userSource)

	if len(a.Errors) > 0 {
		for _, e := range a.Errors {
			fmt.Printf("  ERROR: %s\n", e)
		}
		return
	}

	fmt.Printf("  ✓ Parsed successfully\n")
	fmt.Printf("  ✓ Found %d API calls to cache\n", len(a.CachedCalls))
	fmt.Printf("  ✓ Found %d checkpoints to inject\n", len(a.Checkpoints))
	fmt.Printf("  ✓ Found %d compensation relationships\n", len(a.Compensations))
	fmt.Println()

	// ---- Phase 2: Show what we found ----
	fmt.Println("── Phase 2: Analyzed workflow structure ──")
	fmt.Println()
	fmt.Println("  Cached API calls:")
	for _, c := range a.CachedCalls {
		fmt.Printf("    %s(%s) → %s\n", c.FuncName, strings.Join(c.ArgExprs, ", "), c.ResultTo)
	}
	fmt.Println()
	fmt.Println("  Checkpoints:")
	for _, cp := range a.Checkpoints {
		fmt.Printf("    %s (line %d)\n", cp.ID, cp.Line)
	}
	fmt.Println()
	fmt.Println("  Compensation chain:")
	for _, comp := range a.Compensations {
		fmt.Printf("    if %s fails → %s\n", comp.TriggerStep, comp.Action)
	}
	fmt.Println()

	// ---- Phase 3: Resynthesize ----
	fmt.Println("── Phase 3: Resynthesized durable code ──")
	fmt.Println()
	fmt.Println(a.Resynthesize(userSource))
}
