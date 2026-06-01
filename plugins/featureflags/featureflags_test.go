package featureflags

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/auth"
	"github.com/cleat-team/cleat/plugin"
)

func TestInfo(t *testing.T) {
	p := &Plugin{}
	info := p.Info()
	if info.Name != "feature-flags" {
		t.Errorf("expected Name 'feature-flags', got %q", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Errorf("expected Version '0.1.0', got %q", info.Version)
	}
	if info.Description == "" {
		t.Error("expected non-empty Description")
	}
}

func TestInit(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`{"default_rollout": 50}`),
	}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
	if p.config.DefaultRollout != 50 {
		t.Errorf("expected DefaultRollout 50, got %d", p.config.DefaultRollout)
	}
}

func TestInitDefaults(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{}
	err := p.Init(context.Background(), env)
	if err != nil {
		t.Fatalf("Init() returned error: %v", err)
	}
}

func TestInitInvalidConfig(t *testing.T) {
	p := &Plugin{}
	env := &plugin.Environment{
		Config: []byte(`not valid json`),
	}
	err := p.Init(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for invalid config, got nil")
	}
}

// ---- Evaluator tests ----

func TestEvaluateFlag_Disabled(t *testing.T) {
	flag := &Flag{
		Key:     "test-flag",
		Enabled: false,
	}
	ctx := EvaluationContext{UserID: "user-1"}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected disabled flag to return enabled=false")
	}
	if result.Key != "test-flag" {
		t.Errorf("expected key 'test-flag', got %q", result.Key)
	}
}

func TestEvaluateFlag_EnabledNoRules(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 0, // 0 means 100% rollout
	}
	ctx := EvaluationContext{UserID: "user-1"}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected enabled flag with no rules and 0% rollout to be enabled")
	}
	if result.Evaluation == nil {
		t.Fatal("expected evaluation detail")
	}
	if !result.Evaluation.RolloutHit {
		t.Error("expected rollout to be hit with 0% rollout (100% traffic)")
	}
}

func TestEvaluateFlag_EnabledNoRulesWithRollout(t *testing.T) {
	flag := &Flag{
		Key:               "rollout-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 100, // 100% means all traffic
	}
	ctx := EvaluationContext{UserID: "user-1"}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag with 100% rollout to be enabled")
	}
}

func TestEvaluateFlag_RuleEqMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "user_id", "operator": "eq", "value": "user-123"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-123", Attributes: map[string]interface{}{"region": "us-east"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when rule matches")
	}
	if result.Evaluation == nil || result.Evaluation.MatchedRule == nil {
		t.Fatal("expected matched rule in evaluation detail")
	}
	if result.Evaluation.MatchedRule.Attribute != "user_id" {
		t.Errorf("expected matched rule attribute 'user_id', got %q", result.Evaluation.MatchedRule.Attribute)
	}
}

func TestEvaluateFlag_RuleEqNoMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "user_id", "operator": "eq", "value": "user-123"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-456"}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when rule does not match")
	}
}

func TestEvaluateFlag_RuleNeqMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "plan", "operator": "neq", "value": "free"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "pro"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when neq matches")
	}
}

func TestEvaluateFlag_RuleContainsMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "email", "operator": "contains", "value": "@example.com"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"email": "test@example.com"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when contains matches")
	}
}

func TestEvaluateFlag_RuleContainsNoMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "email", "operator": "contains", "value": "@example.com"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"email": "test@other.com"}}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when contains does not match")
	}
}

func TestEvaluateFlag_RuleInMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "plan", "operator": "in", "value": ["pro", "enterprise"]}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "enterprise"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when in matches")
	}
}

func TestEvaluateFlag_RuleInNoMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "plan", "operator": "in", "value": ["pro", "enterprise"]}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "free"}}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when in does not match")
	}
}

func TestEvaluateFlag_RuleNotInMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "plan", "operator": "not_in", "value": ["free"]}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "pro"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when not_in matches (not in list)")
	}
}

func TestEvaluateFlag_AndLogicAllRulesMustMatch(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "plan", "operator": "eq", "value": "pro"}, {"attribute": "region", "operator": "eq", "value": "us-east"}]`),
		RolloutPercentage: 0,
	}
	// Both rules match.
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "pro", "region": "us-east"}}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when all rules match")
	}

	// Only one rule matches.
	ctx2 := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "pro", "region": "eu-west"}}
	result2 := EvaluateFlag(flag, ctx2)
	if result2.Enabled {
		t.Error("expected flag to be disabled when not all rules match")
	}
}

func TestEvaluateFlag_MissingAttribute(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "nonexistent", "operator": "eq", "value": "foo"}]`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{"plan": "pro"}}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when attribute is missing from context")
	}
}

func TestEvaluateFlag_InvalidRules(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`not valid json`),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{UserID: "user-1"}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when rules are invalid")
	}
}

func TestEvaluateFlag_EmptyUserID(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 0,
	}
	ctx := EvaluationContext{}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled with empty user_id and no rules")
	}
}

// ---- Rollout consistency tests ----

func TestHashPercentage_Deterministic(t *testing.T) {
	a := hashPercentage("tenant-1", "flag-a", "user-1")
	b := hashPercentage("tenant-1", "flag-a", "user-1")
	if a != b {
		t.Errorf("expected deterministic hash, got %d then %d", a, b)
	}
}

func TestHashPercentage_Range(t *testing.T) {
	for i := 0; i < 100; i++ {
		pct := hashPercentage("tenant-1", "flag-b", "user-1")
		if pct < 0 || pct >= 100 {
			t.Errorf("hash out of range: %d", pct)
		}
	}
}

func TestHashPercentage_DifferentUsers(t *testing.T) {
	// Different users should give different hash values (not guaranteed but
	// very likely for a good hash function).
	results := make(map[int]bool)
	for _, user := range []string{"user-1", "user-2", "user-3", "user-4", "user-5"} {
		pct := hashPercentage("tenant-1", "flag-a", user)
		results[pct] = true
	}
	// With 5 users and 100 buckets, odds of all being same are astronomically low.
	if len(results) < 2 {
		t.Log("warning: all users got same hash bucket (possible but unlikely)")
	}
}

func TestHashPercentage_DifferentTenants(t *testing.T) {
	r1 := hashPercentage("tenant-1", "flag-a", "user-1")
	r2 := hashPercentage("tenant-2", "flag-a", "user-1")
	if r1 == r2 {
		t.Log("note: different tenants mapped to same bucket (possible)")
	}
}

func TestRolloutConsistency_SameUserAlwaysSame(t *testing.T) {
	flag := &Flag{
		Key:               "rollout-test",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 50,
	}
	ctx := EvaluationContext{UserID: "user-42"}
	first := EvaluateFlag(flag, ctx)
	// Evaluate 100 times — should always give same result for same user.
	for i := 0; i < 100; i++ {
		result := EvaluateFlag(flag, ctx)
		if result.Enabled != first.Enabled {
			t.Fatalf("rollout inconsistent at iteration %d: was %v, now %v",
				i, first.Enabled, result.Enabled)
		}
	}
}

func TestRolloutConsistency_DifferentUsersGetVariedResults(t *testing.T) {
	flag := &Flag{
		Key:               "rollout-test",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 50,
	}
	enabled := 0
	total := 100
	for i := 0; i < total; i++ {
		ctx := EvaluationContext{UserID: string(rune('A' + i))}
		result := EvaluateFlag(flag, ctx)
		if result.Enabled {
			enabled++
		}
	}
	// With 50% rollout and 100 different users, we expect roughly 50 enabled.
	// Allow a wide margin (±30) to avoid flakiness.
	if enabled < 10 || enabled > 90 {
		t.Errorf("expected roughly 50 enabled users with 50%% rollout, got %d/%d", enabled, total)
	}
}

func TestParser_EmptyRules(t *testing.T) {
	rules, err := parseRules(nil)
	if err != nil {
		t.Fatalf("unexpected error parsing nil rules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected empty rules from nil, got %d", len(rules))
	}

	rules, err = parseRules(json.RawMessage(""))
	if err != nil {
		t.Fatalf("unexpected error parsing empty rules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected empty rules from empty, got %d", len(rules))
	}

	rules, err = parseRules(json.RawMessage("[]"))
	if err != nil {
		t.Fatalf("unexpected error parsing [] rules: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected empty rules from [], got %d", len(rules))
	}
}

func TestParser_ValidRules(t *testing.T) {
	raw := json.RawMessage(`[{"attribute": "plan", "operator": "eq", "value": "pro"}]`)
	rules, err := parseRules(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Attribute != "plan" {
		t.Errorf("expected attribute 'plan', got %q", rules[0].Attribute)
	}
	if rules[0].Operator != "eq" {
		t.Errorf("expected operator 'eq', got %q", rules[0].Operator)
	}
}

func TestEvaluateFlag_50PercentRolloutThreshold(t *testing.T) {
	// Verify the rollout threshold boundary where hash < rollout_percentage.
	// With rollout_percentage = 50, users with hash < 50 get enabled.
	flag := &Flag{
		Key:               "boundary-test",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 50,
	}

	// For a specific user, check the hash and verify the result is consistent.
	for _, user := range []string{"user-aaa", "user-bbb", "user-ccc", "user-ddd", "user-eee"} {
		pct := hashPercentage("tenant-1", "boundary-test", user)
		ctx := EvaluationContext{UserID: user}
		result := EvaluateFlag(flag, ctx)
		expectedEnabled := pct < 50
		if result.Enabled != expectedEnabled {
			t.Errorf("user %s: hash=%d, expected enabled=%v, got %v",
				user, pct, expectedEnabled, result.Enabled)
		}
	}
}

// ---- Exported interface tests ----

func TestMigrations(t *testing.T) {
	p := &Plugin{}
	migrations := p.Migrations()
	if len(migrations) != 1 {
		t.Fatalf("expected 1 migration, got %d", len(migrations))
	}
	if migrations[0].Version != 1 {
		t.Errorf("expected version 1, got %d", migrations[0].Version)
	}
	if !strings.Contains(migrations[0].Up, "feature_flags") {
		t.Error("expected migration to mention feature_flags")
	}
	if !strings.Contains(migrations[0].Down, "DROP TABLE") {
		t.Error("expected Down to contain DROP TABLE")
	}
}

func TestRegisterRoutes(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	err := p.RegisterRoutes(mux)
	if err != nil {
		t.Fatalf("RegisterRoutes() returned error: %v", err)
	}

	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/features/flags"},
		{"GET", "/features/flags"},
		{"GET", "/features/flags/550e8400-e29b-41d4-a716-446655440000"},
		{"PUT", "/features/flags/550e8400-e29b-41d4-a716-446655440000"},
		{"DELETE", "/features/flags/550e8400-e29b-41d4-a716-446655440000"},
		{"POST", "/features/evaluate"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("no handler matched %s %s", tt.method, tt.path)
		}
	}
}

func TestRegisterRoutesNilMux(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterRoutes(nil)
	if err == nil {
		t.Fatal("expected error for nil mux, got nil")
	}
}

func TestRegisterHostFunctions(t *testing.T) {
	p := &Plugin{}
	scope := &testFuncRegistry{}
	err := p.RegisterHostFunctions(scope)
	if err != nil {
		t.Fatalf("RegisterHostFunctions() returned error: %v", err)
	}
	if _, ok := scope.funcs["evaluate_flag"]; !ok {
		t.Error("expected 'evaluate_flag' function to be registered")
	}
}

func TestRegisterHostFunctionsNilScope(t *testing.T) {
	p := &Plugin{}
	err := p.RegisterHostFunctions(nil)
	if err == nil {
		t.Fatal("expected error for nil scope, got nil")
	}
}

// testFuncRegistry implements plugin.FuncRegistry for testing.
type testFuncRegistry struct {
	funcs map[string]plugin.FuncOptions
}

func (r *testFuncRegistry) Register(opts plugin.FuncOptions, fn plugin.PluginFunc) error {
	if r.funcs == nil {
		r.funcs = make(map[string]plugin.FuncOptions)
	}
	r.funcs[opts.Name] = opts
	return nil
}

// ---- Hash percentage edge cases ----

func TestHashPercentageEmptyKeys(t *testing.T) {
	// hash with no keys should not panic and return a value in [0, 100).
	pct := hashPercentage()
	if pct < 0 || pct >= 100 {
		t.Errorf("hashPercentage() = %d, want in [0, 100)", pct)
	}
}

func TestHashPercentageSingleKey(t *testing.T) {
	pct := hashPercentage("single-key")
	if pct < 0 || pct >= 100 {
		t.Errorf("hashPercentage('single-key') = %d, want in [0, 100)", pct)
	}
}

// ---- evaluateRule edge cases ----

func TestEvaluateRuleContainsNonStringValue(t *testing.T) {
	// When rule.Value is not a string for "contains", the rule should not match.
	result := evaluateRule(Rule{Attribute: "attr", Operator: "contains", Value: 42}, "hello world")
	if result {
		t.Error("expected contains with non-string value to return false")
	}
}

func TestEvaluateRuleInNonArrayValue(t *testing.T) {
	// When rule.Value is not an array for "in", the rule should not match.
	result := evaluateRule(Rule{Attribute: "attr", Operator: "in", Value: "not-an-array"}, "value")
	if result {
		t.Error("expected in with non-array value to return false")
	}
}

func TestEvaluateRuleNotInNonArrayValue(t *testing.T) {
	// When rule.Value is not an array for "not_in", the rule should match (no match possible).
	result := evaluateRule(Rule{Attribute: "attr", Operator: "not_in", Value: "not-an-array"}, "value")
	if !result {
		t.Error("expected not_in with non-array value to return true (no match possible)")
	}
}

func TestEvaluateRuleUnknownOperator(t *testing.T) {
	// Unknown operator should not match.
	result := evaluateRule(Rule{Attribute: "attr", Operator: "unknown", Value: "val"}, "val")
	if result {
		t.Error("expected unknown operator to return false")
	}
}

func TestParseRulesInvalidJSON(t *testing.T) {
	_, err := parseRules(json.RawMessage("not valid json"))
	if err == nil {
		t.Error("expected error for invalid JSON in parseRules")
	}
}

// ---- Empty evaluation context edge cases ----

func TestEvaluateFlagEmptyAttributes(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "custom_attr", "operator": "eq", "value": "value"}]`),
		RolloutPercentage: 0,
	}
	// Empty attributes map — rule references custom_attr which doesn't exist.
	ctx := EvaluationContext{UserID: "user-1", Attributes: map[string]interface{}{}}
	result := EvaluateFlag(flag, ctx)
	if result.Enabled {
		t.Error("expected flag to be disabled when attribute is missing")
	}
}

func TestEvaluateFlagNilAttributes(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage(`[{"attribute": "user_id", "operator": "eq", "value": "user-1"}]`),
		RolloutPercentage: 0,
	}
	// Nil attributes — user_id is in ctx.UserID but the rule system uses contextMap, not UserID directly.
	ctx := EvaluationContext{UserID: "user-1"}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled when user_id matches via EvaluationContext")
	}
}

func TestEvaluateFlagNoUserIDWithRollout(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 100,
	}
	ctx := EvaluationContext{}
	result := EvaluateFlag(flag, ctx)
	if !result.Enabled {
		t.Error("expected flag to be enabled with no user_id and 100% rollout")
	}
}

func TestEvaluateFlagRolloutDefaultUser(t *testing.T) {
	flag := &Flag{
		Key:               "test-flag",
		TenantID:          "tenant-1",
		Enabled:           true,
		Rules:             json.RawMessage("[]"),
		RolloutPercentage: 50,
	}
	// Empty user ID uses "default" for rollout hash.
	// The result should be deterministic.
	ctx := EvaluationContext{}
	first := EvaluateFlag(flag, ctx)
	for i := 0; i < 20; i++ {
		result := EvaluateFlag(flag, ctx)
		if result.Enabled != first.Enabled {
			t.Fatalf("rollout inconsistent for default user: was %v, now %v", first.Enabled, result.Enabled)
		}
	}
}

// ---- evaluateFlag host function (via direct call) ----

func TestEvaluateFlagHostNoTenant(t *testing.T) {
	p := &Plugin{}
	_, err := p.evaluateFlag(context.Background(), `{"key":"test","context":{"user_id":"u1"}}`)
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
	if !strings.Contains(err.Error(), "no tenant context") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestEvaluateFlagHostInvalidJSON(t *testing.T) {
	p := &Plugin{}
	cc := &plugin.CallContext{TenantID: uuid.New().String(), WorkflowID: "wf-1"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.evaluateFlag(ctx, `not json`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestEvaluateFlagHostMissingKey(t *testing.T) {
	p := &Plugin{}
	cc := &plugin.CallContext{TenantID: uuid.New().String(), WorkflowID: "wf-1"}
	ctx := plugin.WithCallContext(context.Background(), cc)
	_, err := p.evaluateFlag(ctx, `{"context":{"user_id":"u1"}}`)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

// ---- Route handler error path tests (pre-DB) ----

func TestHandleCreateMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/features/flags", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleCreateInvalidBody(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/features/flags", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid body, got %d", rec.Code)
	}
}

func TestHandleCreateMissingKey(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest("POST", "/features/flags", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing key, got %d", rec.Code)
	}
}

func TestHandleGetMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/features/flags/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleGetInvalidID(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/features/flags/not-a-uuid", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid id, got %d", rec.Code)
	}
}

func TestHandleUpdateMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("PUT", "/features/flags/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleUpdateInvalidID(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("PUT", "/features/flags/not-a-uuid", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid id, got %d", rec.Code)
	}
}

func TestHandleUpdateInvalidBody(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("PUT", "/features/flags/11111111-1111-1111-1111-111111111111", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid body, got %d", rec.Code)
	}
}

func TestHandleDeleteMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/features/flags/11111111-1111-1111-1111-111111111111", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleDeleteInvalidID(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/features/flags/not-a-uuid", nil).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid id, got %d", rec.Code)
	}
}

func TestHandleEvaluateMissingTenant(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest("POST", "/features/evaluate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("expected 401 for missing tenant, got %d", rec.Code)
	}
}

func TestHandleEvaluateInvalidBody(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`not json`)
	req := httptest.NewRequest("POST", "/features/evaluate", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for invalid body, got %d", rec.Code)
	}
}

func TestHandleEvaluateMissingKey(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest("POST", "/features/evaluate", body).WithContext(
		auth.WithTenantID(context.Background(), uuid.New()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("expected 400 for missing key, got %d", rec.Code)
	}
}
