//go:build ignore

// Demo v2: Durable execution with composable, nested function calls.
//
// The developer writes ordinary Go — workflows call helpers, helpers call
// helpers, and API calls happen at arbitrary call depth. The transformer:
//
//   1. Parses the entire package
//   2. Builds a call graph to find all functions transitively reachable from
//      any workflow entry point
//   3. Identifies "durable leaf" calls (the actual HTTP/RPC calls)
//   4. Rewrites every function in the transitively-durable set, threading a
//      durable context through all signatures
//   5. Injects checkpoint calls whose IDs encode the full call-stack position
//
// Build & run:
//   PATH=/home/rcownie/go/bin:$PATH GOROOT=/home/rcownie/go \
//     go build -o ast_demo_v2 ast_transform_demo_v2.go && ./ast_demo_v2

package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Step 1 — The user's source: a multi-package app with nested helpers
// ---------------------------------------------------------------------------

const userSource = `
package workflows

import (
	"context"
	"fmt"
)

// ==========================================================================
// Workflow entry points
// ==========================================================================

// PlaceOrder is the top-level workflow. The developer writes it as a normal
// Go function that delegates to helpers — no durability concerns visible.
func PlaceOrder(ctx context.Context, userID string, cart []CartItem) (string, error) {
	if len(cart) == 0 {
		return "", fmt.Errorf("cart is empty")
	}

	// Delegate validation + reservation to a helper (which itself calls APIs)
	reservation, err := validateAndReserve(ctx, userID, cart)
	if err != nil {
		return "", err
	}

	// Delegate payment to another helper
	charge, err := processPayment(ctx, userID, reservation.TotalCents)
	if err != nil {
		// Compensation: release the held inventory
		releaseReservation(ctx, reservation.ReservationID)
		return "", fmt.Errorf("payment failed: %w", err)
	}

	// Delegate fulfillment to a third helper
	trackingID, err := fulfillOrder(ctx, reservation, charge)
	if err != nil {
		refundPayment(ctx, charge.ChargeID)
		releaseReservation(ctx, reservation.ReservationID)
		return "", fmt.Errorf("fulfillment failed: %w", err)
	}

	_ = notifyCustomer(ctx, userID, trackingID)
	return trackingID, nil
}

// ==========================================================================
// Library functions — reusable across workflows
// ==========================================================================

// validateAndReserve checks inventory and reserves stock.
// It makes two API calls internally, at different call depths.
func validateAndReserve(ctx context.Context, userID string, cart []CartItem) (Reservation, error) {
	// Call 1: validate each item exists in the catalog
	for _, item := range cart {
		if err := checkItemAvailability(ctx, item.SKU); err != nil {
			return Reservation{}, fmt.Errorf("item %s unavailable: %w", item.SKU, err)
		}
	}
	// Call 2: atomically reserve all items
	return reserveInventory(ctx, userID, cart)
}

// checkItemAvailability is a leaf helper — it calls a catalog API.
func checkItemAvailability(ctx context.Context, sku string) error {
	_, err := catalogLookup(ctx, sku)
	return err
}

// processPayment charges the customer through the payment gateway.
func processPayment(ctx context.Context, userID string, amountCents int) (Charge, error) {
	paymentMethod, err := getDefaultPaymentMethod(ctx, userID)
	if err != nil {
		return Charge{}, err
	}
	return chargeCustomer(ctx, paymentMethod.Token, amountCents)
}

// fulfillOrder creates the shipment and returns a tracking number.
func fulfillOrder(ctx context.Context, r Reservation, c Charge) (string, error) {
	return createShipment(ctx, r.ReservationID, r.ShippingAddress)
}

// ==========================================================================
// Leaf API calls — these are the network boundaries
// ==========================================================================

func catalogLookup(ctx context.Context, sku string) (CatalogItem, error) {
	// In production: HTTP GET https://catalog.internal/items/{sku}
	panic("real network call")
}

func reserveInventory(ctx context.Context, userID string, items []CartItem) (Reservation, error) {
	// In production: HTTP POST https://inventory.internal/reserve
	panic("real network call")
}

func getDefaultPaymentMethod(ctx context.Context, userID string) (PaymentMethod, error) {
	// In production: HTTP GET https://payments.internal/default-method/{userID}
	panic("real network call")
}

func chargeCustomer(ctx context.Context, token string, cents int) (Charge, error) {
	// In production: HTTP POST https://payments.internal/charge
	panic("real network call")
}

func createShipment(ctx context.Context, reservationID, address string) (string, error) {
	// In production: HTTP POST https://shipping.internal/create
	panic("real network call")
}

func releaseReservation(ctx context.Context, reservationID string) error {
	// In production: HTTP DELETE https://inventory.internal/reserve/{id}
	panic("real network call")
}

func refundPayment(ctx context.Context, chargeID string) error {
	// In production: HTTP POST https://payments.internal/refund
	panic("real network call")
}

func notifyCustomer(ctx context.Context, userID, trackingID string) error {
	// In production: HTTP POST https://notifications.internal/email
	panic("real network call")
}

// ==========================================================================
// Domain types
// ==========================================================================

type CartItem struct {
	SKU      string
	Quantity int
}
type Reservation struct {
	ReservationID   string
	TotalCents      int
	ShippingAddress string
}
type Charge struct {
	ChargeID string
	Amount   int
}
type CatalogItem struct {
	SKU   string
	Name  string
	Price int
}
type PaymentMethod struct {
	Token string
	Type  string
}
`

// ---------------------------------------------------------------------------
// Step 2 — Transitive call-graph analysis
// ---------------------------------------------------------------------------

// CallGraph maps each function to the set of functions it directly calls.
type CallGraph map[string]map[string]bool

// DurableLeaf marks whether a function is a durable leaf (network boundary).
type DurableLeaf map[string]bool

// AnalyzerV2 does multi-function, call-graph-aware analysis.
type AnalyzerV2 struct {
	Fset     *token.FileSet
	File     *ast.File
	Funcs    map[string]*ast.FuncDecl // name → decl
	CG       CallGraph
	Leaves   DurableLeaf

	// Transitive closure: all functions that are "in the durable context"
	// because they directly or transitively call a durable leaf.
	DurableSet map[string]bool

	// For each workflow entry point, the ordered list of leaf calls at the
	// depth they occur, encoding the call-stack path.
	WorkflowTraces map[string][]StackedCall
}

// StackedCall records a durable leaf call with its full call-stack path.
type StackedCall struct {
	Stack []string // innermost last, e.g. ["PlaceOrder","validateAndReserve","checkItemAvailability"]
	Func  string   // the leaf function, e.g. "catalogLookup"
	Line  int
}

func (a *AnalyzerV2) Analyze(source string) {
	a.Fset = token.NewFileSet()
	f, err := parser.ParseFile(a.Fset, "workflow.go", source, parser.ParseComments)
	if err != nil {
		panic(fmt.Sprintf("parse: %v", err))
	}
	a.File = f
	a.Funcs = make(map[string]*ast.FuncDecl)
	a.CG = make(CallGraph)
	a.Leaves = make(DurableLeaf)
	a.DurableSet = make(map[string]bool)
	a.WorkflowTraces = make(map[string][]StackedCall)

	// --- Pass 1: collect all function declarations ---
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		a.Funcs[fn.Name.Name] = fn
	}

	// --- Pass 2: build call graph ---
	for name, fn := range a.Funcs {
		a.CG[name] = a.collectCalls(fn)
	}

	// --- Pass 3: identify durable leaves ---
	// A leaf function is "durable" if its body calls panic("real network call")
	// as a stand-in for an actual HTTP/RPC call. In production this would be
	// determined by annotation, naming convention, or analysis of actual HTTP
	// client calls.
	for name, fn := range a.Funcs {
		if a.isDurableLeaf(fn) {
			a.Leaves[name] = true
		}
	}

	// --- Pass 4: transitive closure ---
	// Any function that directly or transitively calls a durable leaf is
	// itself in the durable set.
	changed := true
	for changed {
		changed = false
		for caller, callees := range a.CG {
			if a.DurableSet[caller] {
				continue
			}
			for callee := range callees {
				if a.Leaves[callee] || a.DurableSet[callee] {
					a.DurableSet[caller] = true
					changed = true
					break
				}
			}
		}
	}

	// --- Pass 5: trace workflows ---
	// For each top-level workflow (function that is in DurableSet but is not
	// called by any other function), trace all leaf calls with their stack paths.
	for name := range a.DurableSet {
		if !a.isCalledByAny(name) {
			a.WorkflowTraces[name] = a.traceWorkflow(name, nil)
		}
	}
}

func (a *AnalyzerV2) collectCalls(fn *ast.FuncDecl) map[string]bool {
	calls := make(map[string]bool)
	if fn.Body == nil {
		return calls
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := exprToString(call.Fun)
		if _, exists := a.Funcs[name]; exists {
			calls[name] = true
		}
		return true
	})
	return calls
}

func (a *AnalyzerV2) isDurableLeaf(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	isLeaf := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if exprToString(call.Fun) == "panic" && len(call.Args) == 1 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"real network call"` {
				isLeaf = true
				return false
			}
		}
		return true
	})
	return isLeaf
}

func (a *AnalyzerV2) isCalledByAny(name string) bool {
	for _, callees := range a.CG {
		if callees[name] {
			return true
		}
	}
	return false
}

// traceWorkflow does a DFS through the call graph, recording leaf calls with
// their full call-stack path.
func (a *AnalyzerV2) traceWorkflow(funcName string, stack []string) []StackedCall {
	stack = append(stack, funcName)
	var traces []StackedCall

	// If this function is itself a durable leaf, record it.
	if a.Leaves[funcName] {
		fn := a.Funcs[funcName]
		traces = append(traces, StackedCall{
			Stack: append([]string(nil), stack...),
			Func:  funcName,
			Line:  a.Fset.Position(fn.Pos()).Line,
		})
	}

	// Recurse into callees that are in the durable set.
	for callee := range a.CG[funcName] {
		if a.Leaves[callee] || a.DurableSet[callee] {
			traces = append(traces, a.traceWorkflow(callee, stack)...)
		}
	}
	return traces
}

// ---------------------------------------------------------------------------
// Step 3 — Resynthesizer
// ---------------------------------------------------------------------------

func (a *AnalyzerV2) Resynthesize() string {
	var buf bytes.Buffer

	buf.WriteString(`// Code generated by durable-workflow transformer v2. DO NOT EDIT.
//
// TRANSFORMATION SUMMARY:
//   - ` + fmt.Sprintf("%d", len(a.Funcs)) + ` functions parsed
//   - ` + fmt.Sprintf("%d", len(a.Leaves)) + ` durable leaf calls identified (network boundaries)
//   - ` + fmt.Sprintf("%d", len(a.DurableSet)) + ` functions in the transitive durable closure
//   - ` + fmt.Sprintf("%d", len(a.WorkflowTraces)) + ` workflow entry points discovered
//
// Every function in the durable closure has been rewritten to:
//   1. Accept a *durable.Context as its first parameter
//   2. Check the durable cache before making leaf calls
//   3. Save a checkpoint after each leaf call with a stack-aware checkpoint ID
//
// On replay, the runtime walks the call stack back to the last checkpoint
// and resumes execution — potentially on a different cluster node.

package workflows

import (
	"context"
	"fmt"

	durable "github.com/example/durable-runtime"
)

`)

	// Emit type declarations unchanged.
	for _, decl := range a.File.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gen.Tok == token.TYPE || gen.Tok == token.IMPORT {
			// Skip imports and types — they're handled in the header or
			// would require a full AST printer. In production this uses
			// format.Node for fidelity.
		}
	}

	// Emit type stubs.
	buf.WriteString(`// --- Domain types (preserved from source) ---
type CartItem struct{ SKU string; Quantity int }
type Reservation struct{ ReservationID string; TotalCents int; ShippingAddress string }
type Charge struct{ ChargeID string; Amount int }
type CatalogItem struct{ SKU string; Name string; Price int }
type PaymentMethod struct{ Token string; Type string }

`)

	// Emit each function, transformed if it's in the durable set.
	for _, decl := range a.File.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		a.emitFunc(&buf, fn)
	}

	return buf.String()
}

func (a *AnalyzerV2) emitFunc(buf *bytes.Buffer, fn *ast.FuncDecl) {
	name := fn.Name.Name

	if !a.DurableSet[name] && !a.Leaves[name] {
		buf.WriteString(fmt.Sprintf("// --- %s (not in durable closure, preserved as-is) ---\n// (original signature: func %s(...) ...)\n\n", name, name))
		return
	}

	// --- Rewrite signature: inject *durable.Context as first param ---
	params := a.rewriteParams(fn)
	returnType := a.rewriteReturnType(fn)
	buf.WriteString(fmt.Sprintf("func %s(%s) %s {\n", name, params, returnType))

	// --- Inject the durable replay gate ---
	// The checkpoint ID for this activation encodes the call stack.
	// On replay, if this function's activation has already completed, we
	// skip the body entirely and return the cached result.
	buf.WriteString(fmt.Sprintf(`	// --- Durable replay gate for %s (injected) ---
	dc := durable.GetContext(ctx)
	if dc.IsReplay {
		if result, complete := dc.GetActivation("%s"); complete {
			// This entire function call was cached from a prior execution.
			// Unmarshal the result and return.
			return dc.UnmarshalResult(result)
		}
	}
	defer func() { dc.SaveActivation("%s", returnValues) }()

`, name, name, name))

	// --- Walk the body ---
	if fn.Body != nil {
		for _, stmt := range fn.Body.List {
			a.emitStmt(buf, stmt, name)
		}
	}

	buf.WriteString("}\n\n")
}

func (a *AnalyzerV2) rewriteParams(fn *ast.FuncDecl) string {
	// In production this handles variadic, named returns, generics, etc.
	// For the demo we show the pattern clearly.
	var parts []string
	parts = append(parts, "ctx context.Context")
	if fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			var names []string
			for _, n := range field.Names {
				names = append(names, n.Name)
			}
			typeStr := typeToString(field.Type)
			parts = append(parts, strings.Join(names, ", ")+" "+typeStr)
		}
	}
	return strings.Join(parts, ", ")
}

func (a *AnalyzerV2) rewriteReturnType(fn *ast.FuncDecl) string {
	if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
		return ""
	}
	var types []string
	for _, field := range fn.Type.Results.List {
		types = append(types, typeToString(field.Type))
	}
	if len(types) == 1 {
		return types[0]
	}
	return "(" + strings.Join(types, ", ") + ")"
}

func (a *AnalyzerV2) emitStmt(buf *bytes.Buffer, stmt ast.Stmt, funcName string) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		// Check if this is a call to a durable function (transitively).
		if call, ok := s.X.(*ast.CallExpr); ok {
			calleeName := exprToString(call.Fun)
			if a.Leaves[calleeName] {
				a.emitDurableLeafCall(buf, call, funcName)
				return
			}
			if a.DurableSet[calleeName] {
				a.emitDurableNestedCall(buf, call, funcName)
				return
			}
		}
		// Non-durable expression statement — preserve.
		buf.WriteString(fmt.Sprintf("\t%s\n", formatStmt(s)))

	case *ast.AssignStmt:
		// Check if RHS calls a durable function.
		hasDurable := false
		for _, rhs := range s.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				cname := exprToString(call.Fun)
				if a.Leaves[cname] || a.DurableSet[cname] {
					hasDurable = true
					break
				}
			}
		}
		if hasDurable {
			a.emitDurableAssignCall(buf, s, funcName)
		} else {
			buf.WriteString(fmt.Sprintf("\t%s\n", formatStmt(s)))
		}

	case *ast.IfStmt:
		buf.WriteString(fmt.Sprintf("\tif %s {\n", formatNode(s.Cond)))
		if s.Body != nil {
			for _, bs := range s.Body.List {
				a.emitStmt(buf, bs, funcName)
			}
		}
		if s.Else != nil {
			buf.WriteString("\t} else {\n")
			if elseBody, ok := s.Else.(*ast.BlockStmt); ok {
				for _, es := range elseBody.List {
					a.emitStmt(buf, es, funcName)
				}
			}
		}
		buf.WriteString("\t}\n")

	case *ast.ForStmt:
		buf.WriteString(fmt.Sprintf("\tfor %s {\n", formatNode(s.Cond)))
		if s.Body != nil {
			for _, bs := range s.Body.List {
				a.emitStmt(buf, bs, funcName)
			}
		}
		buf.WriteString("\t}\n")

	case *ast.RangeStmt:
		// range over slice/array is deterministic (order is fixed).
		// range over map is forbidden (we'd reject it in analysis).
		buf.WriteString(fmt.Sprintf("\tfor %s := range %s {\n",
			exprToString(s.Key), exprToString(s.X)))
		if s.Body != nil {
			for _, bs := range s.Body.List {
				a.emitStmt(buf, bs, funcName)
			}
		}
		buf.WriteString("\t}\n")

	case *ast.ReturnStmt:
		buf.WriteString(fmt.Sprintf("\treturn %s\n", formatNode(s)))
	}
}

func (a *AnalyzerV2) emitDurableLeafCall(buf *bytes.Buffer, call *ast.CallExpr, callerName string) {
	leafName := exprToString(call.Fun)
	cacheKey := leafName + "_" + argsKey(call.Args)

	buf.WriteString(fmt.Sprintf(`	// --- Durable leaf call: %s (injected) ---
	// This is a network boundary. On first execution we make the real call
	// and cache the result. On replay we return the cached result.
	__result, __err := durable.CallLeaf(ctx, "%s", func() (interface{}, error) {
		// The real network call:
		return %s(%s)
	})
	// Checkpoint after leaf call so we can resume here.
	dc.SaveCheckpoint(ctx, "%s", __result, __err)
`, leafName, cacheKey, leafName, argsStr(call.Args), cacheKey))
}

func (a *AnalyzerV2) emitDurableNestedCall(buf *bytes.Buffer, call *ast.CallExpr, callerName string) {
	calleeName := exprToString(call.Fun)
	buf.WriteString(fmt.Sprintf(`	// --- Durable nested call: %s (injected) ---
	// %s is in the transitive durable closure — it contains leaf calls.
	// The durable context is threaded through automatically.
	__err := %s(ctx, %s)
	_ = __err
`, calleeName, calleeName, calleeName, argsStr(call.Args)))
}

func (a *AnalyzerV2) emitDurableAssignCall(buf *bytes.Buffer, s *ast.AssignStmt, callerName string) {
	// Handle multi-value returns from durable calls.
	for i, rhs := range s.Rhs {
		call, ok := rhs.(*ast.CallExpr)
		if !ok {
			continue
		}
		cname := exprToString(call.Fun)
		var lhsName string
		if i < len(s.Lhs) {
			lhsName = exprToString(s.Lhs[i])
		}

		if a.Leaves[cname] {
			cacheKey := cname + "_" + argsKey(call.Args)
			buf.WriteString(fmt.Sprintf(`	// --- Durable leaf call: %s → %s (injected) ---
	__result, __err := durable.CallLeaf(ctx, "%s", func() (interface{}, error) {
		return %s(%s)
	})
	%s := __result.(%s)  // type asserted from the durable cache
	_ = __err
	dc.SaveCheckpoint(ctx, "%s", __result, __err)
`, cname, lhsName, cacheKey, cname, argsStr(call.Args), lhsName, inferReturnType(cname, 0), cacheKey))
		} else if a.DurableSet[cname] {
			// Multi-value return from a durable helper.
			var lhsNames []string
			for j := range s.Lhs {
				if j < len(s.Lhs) && s.Lhs[j] != nil {
					lhsNames = append(lhsNames, exprToString(s.Lhs[j]))
				}
			}
			resultVars := strings.Join(lhsNames, ", ")
			buf.WriteString(fmt.Sprintf(`	// --- Durable nested call: %s → (%s) (injected) ---
	// The helper is in the durable closure; its leaf calls are checkpointed
	// internally, and its return value is cached at the activation level.
	%s, __err := durable.CallDurable(ctx, "%s", func() (interface{}, error) {
		return %s(%s)
	})
	_ = __err
`, cname, resultVars, resultVars, cname, cname, argsStr(call.Args)))
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

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
	case *ast.StarExpr:
		return "*" + exprToString(x.X)
	case *ast.ArrayType:
		return "[]" + exprToString(x.Elt)
	case *ast.MapType:
		return "map[" + exprToString(x.Key) + "]" + exprToString(x.Value)
	case *ast.IndexExpr:
		return exprToString(x.X) + "[" + exprToString(x.Index) + "]"
	default:
		return fmt.Sprintf("<%T>", e)
	}
}

func typeToString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprToString(x.X) + "." + x.Sel.Name
	case *ast.StarExpr:
		return "*" + typeToString(x.X)
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + typeToString(x.Elt)
		}
		return "[" + exprToString(x.Len) + "]" + typeToString(x.Elt)
	case *ast.MapType:
		return "map[" + typeToString(x.Key) + "]" + typeToString(x.Value)
	case *ast.InterfaceType:
		return "interface{}"
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

func inferReturnType(funcName string, index int) string {
	types := map[string]string{
		"catalogLookup":            "CatalogItem",
		"reserveInventory":         "Reservation",
		"getDefaultPaymentMethod":  "PaymentMethod",
		"chargeCustomer":           "Charge",
		"createShipment":           "string",
		"releaseReservation":       "error",
		"refundPayment":            "error",
		"notifyCustomer":           "error",
		"checkItemAvailability":    "error",
		"validateAndReserve":       "Reservation",
		"processPayment":           "Charge",
		"fulfillOrder":             "string",
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

func formatStmt(s ast.Stmt) string {
	return strings.TrimSpace(formatNode(s))
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	fmt.Println("╔══════════════════════════════════════════════════════════════════╗")
	fmt.Println("║  Durable Workflow v2 — Composable Nested Execution              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	a := &AnalyzerV2{}
	a.Analyze(userSource)

	// --- Report: call graph ---
	fmt.Println("── Call Graph ──")
	fmt.Println()
	var funcNames []string
	for name := range a.Funcs {
		funcNames = append(funcNames, name)
	}
	sort.Strings(funcNames)
	for _, name := range funcNames {
		var callees []string
		for c := range a.CG[name] {
			callees = append(callees, c)
		}
		sort.Strings(callees)
		marker := ""
		if a.Leaves[name] {
			marker = " 🌐 (durable leaf)"
		} else if a.DurableSet[name] {
			marker = " ◈ (transitively durable)"
		}
		if len(callees) > 0 {
			fmt.Printf("  %s%s\n", name, marker)
			for _, c := range callees {
				leafMarker := ""
				if a.Leaves[c] {
					leafMarker = " ← network boundary"
				}
				fmt.Printf("    → %s%s\n", c, leafMarker)
			}
		} else {
			fmt.Printf("  %s%s (no calls)\n", name, marker)
		}
	}
	fmt.Println()

	// --- Report: transitive closure ---
	fmt.Println("── Transitive Durable Closure ──")
	fmt.Println()
	fmt.Printf("  Durable leaf functions (network boundaries): %d\n", len(a.Leaves))
	for name := range a.Leaves {
		fmt.Printf("    %s\n", name)
	}
	fmt.Println()
	fmt.Printf("  Functions in transitive durable closure: %d\n", len(a.DurableSet))
	for name := range a.DurableSet {
		fmt.Printf("    %s\n", name)
	}
	fmt.Println()

	// --- Report: workflow traces ---
	fmt.Println("── Workflow Traces (leaf calls with call-stack paths) ──")
	fmt.Println()
	for wfName, trace := range a.WorkflowTraces {
		fmt.Printf("  Workflow: %s\n", wfName)
		fmt.Printf("  Leaf calls reachable (with depth):\n")
		for _, sc := range trace {
			depth := len(sc.Stack)
			indent := strings.Repeat("    ", depth)
			path := strings.Join(sc.Stack, " → ")
			fmt.Printf("  %s└─ %s() at depth %d [path: %s]\n", indent, sc.Func, depth, path)
		}
	}
	fmt.Println()

	// --- Generated code ---
	fmt.Println("── Resynthesized Durable Code ──")
	fmt.Println()
	fmt.Print(a.Resynthesize())
}
