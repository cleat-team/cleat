// Package featureflags provides feature flag evaluation with targeting rules
// and gradual rollout. Flags can target specific tenants, a percentage of
// traffic, or specific user attributes. Workflows can evaluate flags via
// host functions.
package featureflags

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
)

// Flag represents a single feature flag with its targeting rules and rollout
// configuration.
type Flag struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	Key               string          `json:"key"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Enabled           bool            `json:"enabled"`
	Rules             json.RawMessage `json:"rules,omitempty"`
	RolloutPercentage int             `json:"rollout_percentage"`
}

// Rule is a single targeting rule with attribute, operator, and value.
type Rule struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Value     any    `json:"value"`
}

// EvaluationContext holds the context for evaluating a feature flag, including
// the user ID and arbitrary attributes.
type EvaluationContext struct {
	UserID     string         `json:"user_id"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// EvaluationResult is the result of evaluating a feature flag.
type EvaluationResult struct {
	Enabled    bool              `json:"enabled"`
	Key        string            `json:"key"`
	Evaluation *EvaluationDetail `json:"evaluation,omitempty"`
}

// EvaluationDetail provides details about the evaluation, including which
// rule matched and whether the rollout was hit.
type EvaluationDetail struct {
	MatchedRule *MatchedRule `json:"matched_rule,omitempty"`
	RolloutHit  bool         `json:"rollout_hit"`
}

// MatchedRule describes which rule matched during evaluation.
type MatchedRule struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Value     any    `json:"value"`
}

// hashPercentage computes a stable hash-based percentage for rollout
// evaluation. It produces a value in [0, 100) by hashing the concatenation
// of the provided keys using FNV-1a 32-bit.
func hashPercentage(keys ...string) int {
	h := fnv.New32a()
	for _, k := range keys {
		h.Write([]byte(k))
	}
	return int(h.Sum32() % 100)
}

// evaluateRules checks whether the given context matches all rules (AND logic).
// If there are no rules, it returns true (match by default).
func evaluateRules(rules []Rule, ctx map[string]any) (*MatchedRule, bool) {
	for _, rule := range rules {
		attrVal, ok := ctx[rule.Attribute]
		if !ok {
			return nil, false
		}

		matched := evaluateRule(rule, attrVal)
		if !matched {
			return nil, false
		}
	}
	// All rules matched. Return the first rule as the "matched rule" for
	// evaluation detail purposes, or nil if no rules.
	if len(rules) > 0 {
		return &MatchedRule{
			Attribute: rules[0].Attribute,
			Operator:  rules[0].Operator,
			Value:     rules[0].Value,
		}, true
	}
	return nil, true
}

// evaluateRule checks a single rule against an attribute value.
func evaluateRule(rule Rule, attrVal any) bool {
	strVal := fmt.Sprintf("%v", attrVal)

	switch rule.Operator {
	case "eq":
		return fmt.Sprintf("%v", rule.Value) == strVal
	case "neq":
		return fmt.Sprintf("%v", rule.Value) != strVal
	case "contains":
		ruleStr, ok := rule.Value.(string)
		if !ok {
			return false
		}
		return strings.Contains(strVal, ruleStr)
	case "in":
		arr, ok := rule.Value.([]any)
		if !ok {
			return false
		}
		for _, item := range arr {
			if fmt.Sprintf("%v", item) == strVal {
				return true
			}
		}
		return false
	case "not_in":
		arr, ok := rule.Value.([]any)
		if !ok {
			return true // if not an array, treat as not-in (no match possible)
		}
		for _, item := range arr {
			if fmt.Sprintf("%v", item) == strVal {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// parseRules parses a JSONB rules array into a slice of Rule structs.
// Returns an empty slice if the input is nil or empty.
func parseRules(raw json.RawMessage) ([]Rule, error) {
	if len(raw) == 0 {
		return []Rule{}, nil
	}
	// Normalize JSON array — wrap single objects in an array if needed.
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 {
		return []Rule{}, nil
	}

	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	return rules, nil
}

// EvaluateFlag evaluates a feature flag given its configuration, context, and
// identifiers for tenant and flag key. It follows the evaluation logic:
//
//  1. If flag is not enabled, return {enabled: false}.
//  2. Evaluate rules against context attributes (all rules must match = AND).
//  3. If rules match (or no rules defined), check rollout percentage.
//  4. Return result with evaluation details.
func EvaluateFlag(flag *Flag, ctx EvaluationContext) EvaluationResult {
	result := EvaluationResult{
		Enabled: false,
		Key:     flag.Key,
	}

	if !flag.Enabled {
		return result
	}

	// Build context map from user_id and attributes.
	contextMap := make(map[string]any)
	if ctx.UserID != "" {
		contextMap["user_id"] = ctx.UserID
	}
	for k, v := range ctx.Attributes {
		contextMap[k] = v
	}

	// Parse and evaluate rules.
	rules, err := parseRules(flag.Rules)
	if err != nil {
		// If rules are malformed, flag is not enabled.
		return result
	}

	matchedRule, matched := evaluateRules(rules, contextMap)
	if !matched {
		return result
	}

	// Check rollout percentage.
	rolloutHit := false
	if flag.RolloutPercentage > 0 {
		rolloutUser := ctx.UserID
		if rolloutUser == "" {
			rolloutUser = "default"
		}
		pct := hashPercentage(flag.TenantID, flag.Key, rolloutUser)
		rolloutHit = pct < flag.RolloutPercentage
	} else {
		// No rollout percentage means 100% (all traffic hits).
		rolloutHit = true
	}

	result.Enabled = rolloutHit
	result.Evaluation = &EvaluationDetail{
		MatchedRule: matchedRule,
		RolloutHit:  rolloutHit,
	}

	return result
}
