package engine

import (
	"strings"
	"testing"
	"time"
)

// cleat#4424. A request is the same request when it is the same bytes or the same JSON value (sameRequest). Each row says which, and the "different" rows are the ones that matter:
// a comparison that called everything equal would pass every "same" row. The traps are the ones a value comparison falls into.
func TestARequestIsTheSameWhenItIsTheSameBytesOrTheSameJSONValue(t *testing.T) {
	type row struct {
		name string
		a, b string
		same bool
	}
	rows := []row{
		// the same request, serialised differently: the cross-language case (ABI.md §6.7), measured on tests/cross-language with two 64-byte bodies
		{"identical bytes", `{"a":1}`, `{"a":1}`, true},
		{"top-level key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"the Go and Rust bodies of the cross-language workflow", `{"cart":[{"sku":"SKU-001","quantity":2}],"user_id":"cross-test"}`, `{"cart":[{"quantity":2,"sku":"SKU-001"}],"user_id":"cross-test"}`, true},
		{"nested key order", `{"a":{"x":1,"y":[{"p":1,"q":2}]}}`, `{"a":{"y":[{"q":2,"p":1}],"x":1}}`, true},
		{"insignificant whitespace", "{ \"a\" :\n 1 ,\t\"b\": [ 1 , 2 ] }\n", `{"a":1,"b":[1,2]}`, true},
		{"a string spelled with an escape", `{"a":"ü"}`, `{"a":"ü"}`, true},
		{"1, 1.0 and 1e0 are one number", `{"n":1}`, `{"n":1.0}`, true},
		{"1e0 is 1", `[1e0]`, `[1]`, true},
		{"10e-1 is 1", `[10e-1]`, `[1]`, true},
		{"-0 is 0", `[-0]`, `[0]`, true},
		{"a fraction", `[0.5]`, `[5e-1]`, true},

		// a different request must diverge
		{"a different value", `{"a":1}`, `{"a":2}`, false},
		{"a different string", `{"a":"x"}`, `{"a":"y"}`, false},
		{"an extra key", `{"a":1}`, `{"a":1,"b":2}`, false},
		{"a missing key", `{"a":1,"b":2}`, `{"a":1}`, false},
		{"array order is significant", `[1,2]`, `[2,1]`, false},
		{"a number and the string of it", `{"a":1}`, `{"a":"1"}`, false},
		{"null and false", `[null]`, `[false]`, false},
		{"true and false", `[true]`, `[false]`, false},
		{"a key and a value swapped", `{"a":"b"}`, `{"b":"a"}`, false},
		// THE FLOAT64 TRAP: json.Unmarshal into any makes both of these 9007199254740992.
		{"two integers above 2^53", `[9007199254740993]`, `[9007199254740992]`, false},
		{"1.5 and 1.6", `[1.5]`, `[1.6]`, false},
		{"a tiny difference past float64 precision", `[0.10000000000000000001]`, `[0.1]`, false},

		// a body that is not one JSON value is compared as bytes: identical matches itself, nothing else does
		{"non-JSON, identical", `not json`, `not json`, true},
		{"non-JSON, one space apart", `not json`, `not json `, false},
		{"non-JSON, different", `abc`, `abd`, false},
		{"two bodies that both fail to parse do not match each other", `{"a":`, `{ "a":`, false},
		{"trailing data after the value", `{"a":1} x`, `{"a":1}`, false},
		{"two values", `1 2`, `1  2`, false},
		{"empty", ``, ` `, false},
		{"a JSON body and a non-JSON one", `{"a":1}`, `{"a":1`, false},

		// DUPLICATE KEYS have no canonical form: which value wins is a parser's choice, so the body matches only an identical copy of itself
		{"a duplicate key, identical copy", `{"a":1,"a":2}`, `{"a":1,"a":2}`, true},
		{"a duplicate key against the last value alone", `{"a":1,"a":2}`, `{"a":2}`, false},
		{"a duplicate key against the first value alone", `{"a":1,"a":2}`, `{"a":1}`, false},
		{"a duplicate key reordered", `{"a":1,"a":2,"b":3}`, `{"b":3,"a":1,"a":2}`, false},
	}
	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			if got := sameRequest(r.a, r.b); got != r.same {
				t.Errorf("sameRequest(%q, %q) = %v, want %v", r.a, r.b, got, r.same)
			}
			// symmetric: a replay compares recorded with live, a test might not
			if got := sameRequest(r.b, r.a); got != r.same {
				t.Errorf("sameRequest(%q, %q) = %v, want %v (it must not depend on the order)", r.b, r.a, got, r.same)
			}
		})
	}
}

// A body is guest-controlled, so the comparison must cost what a body costs: a huge exponent, a deep nest and a very long number must come back quickly and as "not the same unless identical".
func TestAHostileRequestBodyIsComparedQuicklyAndAsBytes(t *testing.T) {
	// 100,000 deep is past the JSON decoder's own limit (10,000); 1,000 deep is inside it and past this comparison's bound (maxCanonicalDepth), so that bound is exercised by itself.
	deep := strings.Repeat("[", 100_000) + strings.Repeat("]", 100_000)
	deep1000 := strings.Repeat("[", 1000) + strings.Repeat("]", 1000)
	for _, c := range []struct{ name, a, b string }{
		{"an enormous exponent", `[1e999999999]`, `[1e999999998]`},
		{"an enormous negative exponent", `[1e-999999999]`, `[1e-999999998]`},
		{"an exponent just past the bound", `[1e1001]`, `[1e1002]`},
		// Go's big.Rat refuses the absurd exponents itself, so the bound that matters is the band it still allows: one such number is cheap, two thousand of them in one body are not.
		{"two thousand numbers with an exponent Go still expands", `[` + strings.Repeat("1e500000,", 1999) + `1e500000]`, `[` + strings.Repeat("1e500000,", 1999) + `1e500000] `},
		{"a very long number", `[` + strings.Repeat("9", 100_000) + `]`, `[` + strings.Repeat("9", 99_999) + `8]`},
		{"nesting past the decoder's own limit", deep, deep + " "},
		{"nesting past the comparison's depth bound", deep1000, deep1000 + " "},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			done := make(chan bool, 1)
			go func() { done <- sameRequest(c.a, c.b) }()
			select {
			case same := <-done:
				if same {
					t.Errorf("two different bodies were called the same")
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the comparison did not return in 5s: a body can make a replay expensive")
			}
			if !sameRequest(c.a, c.a) {
				t.Errorf("a body does not match an identical copy of itself")
			}
		})
	}
}

// Numbers inside the bound are still compared exactly, so the bound does not make the comparison coarser where it matters.
func TestANumberWithinTheExponentBoundIsComparedExactly(t *testing.T) {
	if !sameRequest(`[1e1000]`, `[1E+1000]`) {
		t.Errorf("1e1000 and 1E+1000 are one number")
	}
	if sameRequest(`[1e1000]`, `[1e999]`) {
		t.Errorf("1e1000 and 1e999 are two numbers")
	}
}
