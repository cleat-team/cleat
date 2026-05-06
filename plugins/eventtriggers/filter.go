package eventtriggers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// EvaluateFilter evaluates a filter against event data. It accepts two forms:
//
//  1. Structured JSON filter (when expr starts with '{'):
//     {"event.data.amount": {"$gt": 100}, "event.data.status": {"$in": ["active","pending"]}}
//  2. Text expression (otherwise): event.data.amount > 100
//
// If the expression is empty or "true", returns true without evaluation.
func EvaluateFilter(expr string, eventData map[string]interface{}) (bool, error) {
	if expr == "" || expr == "true" {
		return true, nil
	}

	// Auto-detect structured JSON filter.
	if strings.HasPrefix(strings.TrimSpace(expr), "{") {
		var filter map[string]interface{}
		if err := json.Unmarshal([]byte(expr), &filter); err != nil {
			return false, fmt.Errorf("event-triggers: invalid JSON filter: %w", err)
		}
		return matchStructured(filter, eventData)
	}

	// Fall back to text expression grammar.
	tokens, err := tokenize(expr)
	if err != nil {
		return false, fmt.Errorf("event-triggers: tokenize filter: %w", err)
	}

	ast, err := parse(tokens)
	if err != nil {
		return false, fmt.Errorf("event-triggers: parse filter: %w", err)
	}

	return evaluate(ast, eventData)
}

// ---- Structured filter (GraphQL/MongoDB-style) ----

// matchStructured evaluates a structured filter object against event data.
// Each key is a dotted path into the event. Each value is either:
//
//	"value"              — shorthand for {"$eq": "value"}
//	{"$eq": value}       — equals
//	{"$ne": value}       — not equals
//	{"$gt": number}      — greater than
//	{"$lt": number}      — less than
//	{"$gte": number}     — greater than or equal
//	{"$lte": number}     — less than or equal
//	{"$in": [...]}       — value is in list
//	{"$nin": [...]}      — value is not in list
//	{"$exists": bool}    — path exists (or not)
//
// Multiple keys are ANDed together.
func matchStructured(filter map[string]interface{}, eventData map[string]interface{}) (bool, error) {
	for path, cond := range filter {
		val, found := getPath(eventData, path)
		matched, err := matchCondition(path, val, found, cond)
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func matchCondition(path string, val interface{}, found bool, cond interface{}) (bool, error) {
	switch c := cond.(type) {
	case map[string]interface{}:
		// Operator form: {"$gt": 100}
		return matchOperators(path, val, found, c)
	default:
		// Shorthand: "value" means {"$eq": "value"}
		if !found {
			return false, nil
		}
		return valuesEqual(val, c), nil
	}
}

func matchOperators(path string, val interface{}, found bool, ops map[string]interface{}) (bool, error) {
	for op, operand := range ops {
		switch op {
		case "$exists":
			exists, ok := operand.(bool)
			if !ok {
				return false, fmt.Errorf("event-triggers: $exists requires bool, got %T", operand)
			}
			if found != exists {
				return false, nil
			}
		case "$eq":
			if !found || !valuesEqual(val, operand) {
				return false, nil
			}
		case "$ne":
			if found && valuesEqual(val, operand) {
				return false, nil
			}
		case "$gt":
			if !found || !compareNumeric(val, operand) > 0 {
				return false, nil
			}
		case "$lt":
			if !found || !compareNumeric(val, operand) < 0 {
				return false, nil
			}
		case "$gte":
			if !found || !compareNumeric(val, operand) >= 0 {
				return false, nil
			}
		case "$lte":
			if !found || !compareNumeric(val, operand) <= 0 {
				return false, nil
			}
		case "$in":
			list, ok := operand.([]interface{})
			if !ok {
				return false, fmt.Errorf("event-triggers: $in requires array, got %T", operand)
			}
			if !found {
				return false, nil
			}
			matched := false
			for _, item := range list {
				if valuesEqual(val, item) {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		case "$nin":
			list, ok := operand.([]interface{})
			if !ok {
				return false, fmt.Errorf("event-triggers: $nin requires array, got %T", operand)
			}
			if found {
				for _, item := range list {
					if valuesEqual(val, item) {
						return false, nil
					}
				}
			}
		default:
			return false, fmt.Errorf("event-triggers: unknown operator %q", op)
		}
	}
	return true, nil
}

// getPath resolves a dotted path like "event.data.user.name" or "event.data.items[0]"
// in a nested map.
func getPath(data map[string]interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var current interface{} = data
	for _, part := range parts {
		// Check for array index: "field[N]"
		if idx := strings.IndexByte(part, '['); idx >= 0 && strings.HasSuffix(part, "]") {
			fieldName := part[:idx]
			indexStr := part[idx+1 : len(part)-1]
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return nil, false
			}
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, false
			}
			arr, ok := m[fieldName].([]interface{})
			if !ok || index < 0 || index >= len(arr) {
				return nil, false
			}
			current = arr[index]
			continue
		}
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		val, ok := m[part]
		if !ok {
			return nil, false
		}
		current = val
	}
	return current, true
}

func valuesEqual(a, b interface{}) bool {
	// Normalize numeric types for comparison.
	aNum, aIsNum := toFloat64(a)
	bNum, bIsNum := toFloat64(b)
	if aIsNum && bIsNum {
		return aNum == bNum
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func compareNumeric(val, operand interface{}) int {
	a, aOK := toFloat64(val)
	b, bOK := toFloat64(operand)
	if !aOK || !bOK {
		return 0
	}
	if a > b {
		return 1
	}
	if a < b {
		return -1
	}
	return 0
}

func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type tokenType int

const (
	tokError tokenType = iota
	tokEOF
	tokIdent
	tokString
	tokNumber
	tokTrue
	tokFalse
	tokNull
	tokEq   // ==
	tokNeq  // !=
	tokGt   // >
	tokLt   // <
	tokGte  // >=
	tokLte  // <=
	tokDot
	tokLBrack // [
	tokRBrack // ]
	tokLParen // (
	tokRParen // )
	tokComma
)

type token struct {
	typ tokenType
	val string
}

type lexer struct {
	input string
	pos   int
}

func (l *lexer) skipWhitespace() {
	for l.pos < len(l.input) {
		switch l.input[l.pos] {
		case ' ', '\t', '\n', '\r':
			l.pos++
		default:
			return
		}
	}
}

func (l *lexer) next() token {
	l.skipWhitespace()
	if l.pos >= len(l.input) {
		return token{typ: tokEOF}
	}

	ch := l.input[l.pos]

	// String literal.
	if ch == '"' {
		l.pos++ // skip opening quote
		start := l.pos
		for l.pos < len(l.input) && l.input[l.pos] != '"' {
			l.pos++
		}
		if l.pos >= len(l.input) {
			return token{typ: tokError, val: "unterminated string literal"}
		}
		val := l.input[start:l.pos]
		l.pos++ // skip closing quote
		return token{typ: tokString, val: val}
	}

	// Number literal.
	if ch >= '0' && ch <= '9' || ch == '-' {
		start := l.pos
		if l.input[l.pos] == '-' {
			l.pos++
		}
		for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
			l.pos++
		}
		if l.pos < len(l.input) && l.input[l.pos] == '.' {
			l.pos++
			for l.pos < len(l.input) && l.input[l.pos] >= '0' && l.input[l.pos] <= '9' {
				l.pos++
			}
		}
		return token{typ: tokNumber, val: l.input[start:l.pos]}
	}

	// Identifier or keyword.
	if isIdentStart(ch) {
		start := l.pos
		for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
			l.pos++
		}
		val := l.input[start:l.pos]
		switch val {
		case "true":
			return token{typ: tokTrue, val: val}
		case "false":
			return token{typ: tokFalse, val: val}
		case "null":
			return token{typ: tokNull, val: val}
		default:
			return token{typ: tokIdent, val: val}
		}
	}

	// Operators and punctuation.
	switch ch {
	case '=':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return token{typ: tokEq, val: "=="}
		}
		return token{typ: tokError, val: "unexpected '=' (did you mean '==')"}
	case '!':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return token{typ: tokNeq, val: "!="}
		}
		return token{typ: tokError, val: "unexpected '!' (did you mean '!=')"}
	case '>':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return token{typ: tokGte, val: ">="}
		}
		l.pos++
		return token{typ: tokGt, val: ">"}
	case '<':
		if l.pos+1 < len(l.input) && l.input[l.pos+1] == '=' {
			l.pos += 2
			return token{typ: tokLte, val: "<="}
		}
		l.pos++
		return token{typ: tokLt, val: "<"}
	case '.':
		l.pos++
		return token{typ: tokDot}
	case '[':
		l.pos++
		return token{typ: tokLBrack}
	case ']':
		l.pos++
		return token{typ: tokRBrack}
	case '(':
		l.pos++
		return token{typ: tokLParen}
	case ')':
		l.pos++
		return token{typ: tokRParen}
	case ',':
		l.pos++
		return token{typ: tokComma}
	}

	return token{typ: tokError, val: fmt.Sprintf("unexpected character %q", ch)}
}

func isIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isIdentPart(ch byte) bool {
	return isIdentStart(ch) || (ch >= '0' && ch <= '9')
}

func tokenize(input string) ([]token, error) {
	l := &lexer{input: input}
	var tokens []token
	for {
		tok := l.next()
		if tok.typ == tokError {
			return nil, fmt.Errorf("%s", tok.val)
		}
		tokens = append(tokens, tok)
		if tok.typ == tokEOF {
			break
		}
	}
	return tokens, nil
}

// ---------------------------------------------------------------------------
// AST types
// ---------------------------------------------------------------------------

type pathStep struct {
	field   string
	index   int
	isIndex bool // true = array index access; false = field access
}

type literal struct {
	isString bool
	strVal   string
	isNumber bool
	numVal   float64
	isBool   bool
	boolVal  bool
	isNull   bool
}

type expr interface {
	exprMarker()
}

type trueExpr struct{}

func (trueExpr) exprMarker() {}

type comparisonExpr struct {
	path []pathStep
	op   string
	lit  literal
}

func (comparisonExpr) exprMarker() {}

type membershipExpr struct {
	path []pathStep
	lits []literal
}

func (membershipExpr) exprMarker() {}

// ---------------------------------------------------------------------------
// Recursive-descent parser
// ---------------------------------------------------------------------------

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return token{typ: tokEOF}
}

func (p *parser) advance() token {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *parser) expect(typ tokenType) (token, error) {
	tok := p.peek()
	if tok.typ != typ {
		return tok, fmt.Errorf("expected %d but got %q", typ, tok.val)
	}
	return p.advance(), nil
}

// expr = comparison | membership | "true"
func (p *parser) parseExpr() (expr, error) {
	tok := p.peek()

	// "true" literal expression.
	if tok.typ == tokTrue {
		p.advance()
		return trueExpr{}, nil
	}

	// Must be a path expression.
	steps, err := p.parsePath()
	if err != nil {
		return nil, err
	}

	// Check if it is a membership expression (path "in" (...) ).
	if p.peek().typ == tokIdent && p.peek().val == "in" {
		p.advance() // consume "in"
		return p.parseMembership(steps)
	}

	// Otherwise it is a comparison.
	return p.parseComparison(steps)
}

// path = "event.data." ident ("." ident | "[" int "]")*
func (p *parser) parsePath() ([]pathStep, error) {
	// "event"
	tok := p.advance()
	if tok.typ != tokIdent || tok.val != "event" {
		return nil, fmt.Errorf("expected 'event' at start of path, got %q", tok.val)
	}
	// "."
	if p.peek().typ != tokDot {
		return nil, fmt.Errorf("expected '.' after 'event'")
	}
	p.advance()

	// "data"
	tok = p.advance()
	if tok.typ != tokIdent || tok.val != "data" {
		return nil, fmt.Errorf("expected 'data' in path, got %q", tok.val)
	}

	var steps []pathStep

	for {
		if p.peek().typ == tokDot {
			p.advance() // consume '.'
			tok = p.advance()
			if tok.typ != tokIdent {
				return nil, fmt.Errorf("expected field name after '.', got %q", tok.val)
			}
			steps = append(steps, pathStep{field: tok.val})
		} else if p.peek().typ == tokLBrack {
			p.advance() // consume '['
			tok = p.advance()
			if tok.typ != tokNumber {
				return nil, fmt.Errorf("expected number in brackets, got %q", tok.val)
			}
			idx, err := strconv.Atoi(tok.val)
			if err != nil {
				return nil, fmt.Errorf("invalid array index %q", tok.val)
			}
			if p.peek().typ != tokRBrack {
				return nil, fmt.Errorf("expected ']' after array index")
			}
			p.advance() // consume ']'
			steps = append(steps, pathStep{index: idx, isIndex: true})
		} else {
			break
		}
	}

	return steps, nil
}

// comparison = path op literal
func (p *parser) parseComparison(path []pathStep) (comparisonExpr, error) {
	ex := comparisonExpr{path: path}

	tok := p.advance()
	switch tok.typ {
	case tokEq:
		ex.op = "=="
	case tokNeq:
		ex.op = "!="
	case tokGt:
		ex.op = ">"
	case tokLt:
		ex.op = "<"
	case tokGte:
		ex.op = ">="
	case tokLte:
		ex.op = "<="
	case tokEOF:
		return ex, fmt.Errorf("unexpected end of filter expression (expected comparison operator)")
	default:
		return ex, fmt.Errorf("expected comparison operator, got %q", tok.val)
	}

	lit, err := p.parseLiteral()
	if err != nil {
		return ex, err
	}
	ex.lit = lit

	return ex, nil
}

// membership = path "in" "(" literal ("," literal)* ")"
func (p *parser) parseMembership(path []pathStep) (membershipExpr, error) {
	ex := membershipExpr{path: path}

	if _, err := p.expect(tokLParen); err != nil {
		return ex, fmt.Errorf("expected '(' after 'in': %w", err)
	}

	// Parse at least one literal.
	for {
		lit, err := p.parseLiteral()
		if err != nil {
			return ex, err
		}
		ex.lits = append(ex.lits, lit)

		if p.peek().typ == tokComma {
			p.advance()
		} else {
			break
		}
	}

	if _, err := p.expect(tokRParen); err != nil {
		return ex, fmt.Errorf("expected ')' after membership list: %w", err)
	}

	return ex, nil
}

// literal = string | number | "true" | "false" | "null"
func (p *parser) parseLiteral() (literal, error) {
	tok := p.advance()
	switch tok.typ {
	case tokString:
		return literal{isString: true, strVal: tok.val}, nil
	case tokNumber:
		num, err := strconv.ParseFloat(tok.val, 64)
		if err != nil {
			return literal{}, fmt.Errorf("invalid number literal %q", tok.val)
		}
		return literal{isNumber: true, numVal: num}, nil
	case tokTrue:
		return literal{isBool: true, boolVal: true}, nil
	case tokFalse:
		return literal{isBool: true, boolVal: false}, nil
	case tokNull:
		return literal{isNull: true}, nil
	default:
		return literal{}, fmt.Errorf("expected literal, got %q", tok.val)
	}
}

func parse(tokens []token) (expr, error) {
	p := &parser{tokens: tokens, pos: 0}
	ex, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	// Ensure no trailing tokens.
	if p.peek().typ != tokEOF {
		return nil, fmt.Errorf("unexpected token %q after expression", p.peek().val)
	}
	return ex, nil
}

// ---------------------------------------------------------------------------
// Evaluator
// ---------------------------------------------------------------------------

func evaluate(ex expr, data map[string]interface{}) (bool, error) {
	switch e := ex.(type) {
	case trueExpr:
		return true, nil
	case comparisonExpr:
		return evalComparison(e, data)
	case membershipExpr:
		return evalMembership(e, data)
	default:
		return false, fmt.Errorf("unknown expression type %T", ex)
	}
}

func evalComparison(ce comparisonExpr, data map[string]interface{}) (bool, error) {
	val, err := evalPath(data, ce.path)
	if err != nil {
		return false, err
	}
	return compareValues(val, ce.lit, ce.op)
}

func evalMembership(me membershipExpr, data map[string]interface{}) (bool, error) {
	val, err := evalPath(data, me.path)
	if err != nil {
		return false, err
	}
	for _, lit := range me.lits {
		ok, err := compareValues(val, lit, "==")
		if err != nil {
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// evalPath walks the parsed path steps into the event data map.
// The path grammar always starts with "event.data." which is validated
// during parsing; the steps slice contains only the field/index accessors
// after that prefix.
func evalPath(data map[string]interface{}, steps []pathStep) (interface{}, error) {
	current := interface{}(data)

	for _, step := range steps {
		if step.isIndex {
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("cannot index into non-array value")
			}
			if step.index < 0 || step.index >= len(arr) {
				return nil, fmt.Errorf("array index %d out of bounds (len=%d)", step.index, len(arr))
			}
			current = arr[step.index]
		} else {
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("cannot access field %q of non-object value", step.field)
			}
			v, ok := m[step.field]
			if !ok {
				return nil, fmt.Errorf("field %q not found in event data", step.field)
			}
			current = v
		}
	}

	return current, nil
}

func compareValues(a interface{}, lit literal, op string) (bool, error) {
	switch v := a.(type) {
	case float64:
		if !lit.isNumber {
			return false, fmt.Errorf("type mismatch: expected number")
		}
		switch op {
		case "==":
			return v == lit.numVal, nil
		case "!=":
			return v != lit.numVal, nil
		case ">":
			return v > lit.numVal, nil
		case "<":
			return v < lit.numVal, nil
		case ">=":
			return v >= lit.numVal, nil
		case "<=":
			return v <= lit.numVal, nil
		}

	case string:
		if !lit.isString {
			return false, fmt.Errorf("type mismatch: expected string")
		}
		switch op {
		case "==":
			return v == lit.strVal, nil
		case "!=":
			return v != lit.strVal, nil
		case ">":
			return v > lit.strVal, nil
		case "<":
			return v < lit.strVal, nil
		case ">=":
			return v >= lit.strVal, nil
		case "<=":
			return v <= lit.strVal, nil
		}

	case bool:
		if !lit.isBool {
			return false, fmt.Errorf("type mismatch: expected bool")
		}
		switch op {
		case "==":
			return v == lit.boolVal, nil
		case "!=":
			return v != lit.boolVal, nil
		default:
			return false, fmt.Errorf("operator %q not supported for boolean values", op)
		}

	case nil:
		if !lit.isNull {
			return false, fmt.Errorf("type mismatch: expected null")
		}
		switch op {
		case "==":
			return true, nil
		case "!=":
			return false, nil
		default:
			return false, fmt.Errorf("operator %q not supported for null values", op)
		}

	default:
		return false, fmt.Errorf("unsupported value type %T", a)
	}

	return false, fmt.Errorf("unsupported comparison")
}
