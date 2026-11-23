package engine

import (
	"encoding/json"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// sameRequest says whether two request bodies are the same request (cleat#4424): the same bytes, or the same JSON VALUE.
//
// WHY NOT BYTES ALONE. ABI.md §6.7 promises that a history recorded by a workflow in one language replays against the same workflow in another, and the SDKs do not
// serialise one request alike: the Go SDK's map marshals with sorted keys and its structs in field order, the Rust SDK's serde_json::json! sorts at every level. The same
// logical request is therefore a different string, of the same length, depending on who wrote it (measured on tests/cross-language: two 64-byte bodies, two hashes, one request).
// Comparing the raw strings turned that guarantee into a divergence on the first call.
//
// WHAT COUNTS AS THE SAME. Equal bytes always do. Otherwise both must parse as exactly one JSON value, and then the two are the same request when their canonical forms are equal:
// object keys sorted at every level, no insignificant whitespace, strings by their decoded text (so "ü" and "ü" agree), and numbers by their EXACT value, so 1, 1.0 and
// 1e0 agree (two SDKs may render one number differently, which is the very problem above) while 9007199254740993 and 9007199254740992 do not (a float64 would call those equal,
// which is why this decodes with UseNumber and compares as rationals). Array order is significant.
//
// WHAT FALLS BACK TO BYTES, which is to say "different unless identical": a body that is not JSON (nothing is promised about how two SDKs spell a non-JSON request), a body with
// anything after its first value, a body with a DUPLICATE KEY (which value a duplicate means is a parser's choice, and two SDKs' parsers choose differently, so no canonical form
// exists), nesting past maxCanonicalDepth, and a number whose exponent is too large to expand safely, and a string or key that is not valid text: the decoder maps EVERY invalid
// byte and every lone-surrogate escape ("\ud800", which JSON.stringify and Python's json.dumps both emit) to U+FFFD, so the bytes \xff and \xfe, the escapes "\ud800" and "\udc00",
// and a literal U+FFFD would all canonicalise alike, and two different requests would match (found in review; fetch and plugin_call compared bytes exactly before this PR, so for
// them that was a regression). A body with a U+FFFD in any string or key has no canonical form. (Invalid bytes need no check of their own: inside a string the decoder turns each
// into a U+FFFD, which is refused as above, and anywhere else they are a syntax error. A separate utf8.ValidString check decided nothing, and a mutant that removed it stayed green.) A body that cannot be canonicalised can still match an identical copy of itself.
// So of two bodies neither of which parses, only identical ones match: they cannot "match each other" by both failing.
func sameRequest(a, b string) bool {
	if a == b {
		return true
	}
	ca, ok := canonicalJSON(a)
	if !ok {
		return false
	}
	cb, ok := canonicalJSON(b)
	return ok && ca == cb
}

// maxCanonicalDepth bounds the recursion on a hostile body. A real request is a few levels deep.
const maxCanonicalDepth = 256

// canonicalJSON is s as one canonical JSON value, or ok=false when s is not exactly one JSON value or has no canonical form (see sameRequest).
func canonicalJSON(s string) (canon string, ok bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var sb strings.Builder
	if !writeCanonical(dec, &sb, 0) {
		return "", false
	}
	// Nothing but whitespace may follow the one value.
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	return sb.String(), true
}

func writeCanonical(dec *json.Decoder, sb *strings.Builder, depth int) bool {
	if depth > maxCanonicalDepth {
		return false
	}
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			type pair struct{ key, val string }
			var pairs []pair
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return false
				}
				key, isStr := kt.(string)
				if !isStr || seen[key] || strings.ContainsRune(key, utf8.RuneError) {
					return false // a non-string key cannot happen from the decoder, a repeated one has no canonical form, and a U+FFFD may stand for an invalid byte or a lone surrogate
				}
				seen[key] = true
				var vb strings.Builder
				if !writeCanonical(dec, &vb, depth+1) {
					return false
				}
				pairs = append(pairs, pair{key, vb.String()})
			}
			if _, err := dec.Token(); err != nil { // the closing brace
				return false
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
			sb.WriteByte('{')
			for i, p := range pairs {
				if i > 0 {
					sb.WriteByte(',')
				}
				kb, _ := json.Marshal(p.key)
				sb.Write(kb)
				sb.WriteByte(':')
				sb.WriteString(p.val)
			}
			sb.WriteByte('}')
		case '[':
			sb.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					sb.WriteByte(',')
				}
				first = false
				if !writeCanonical(dec, sb, depth+1) {
					return false
				}
			}
			if _, err := dec.Token(); err != nil { // the closing bracket
				return false
			}
			sb.WriteByte(']')
		default:
			return false
		}
	case string:
		if strings.ContainsRune(t, utf8.RuneError) {
			return false // a U+FFFD may stand for an invalid byte or a lone surrogate escape: see sameRequest
		}
		b, _ := json.Marshal(t)
		sb.Write(b)
	case json.Number:
		n, ok := canonicalNumber(string(t))
		if !ok {
			return false
		}
		sb.WriteString(n)
	case bool:
		sb.WriteString(strconv.FormatBool(t))
	case nil:
		sb.WriteString("null")
	default:
		return false
	}
	return true
}

// maxNumberExponent bounds how large a power of ten canonicalNumber will expand: big.Rat would build 10^n, and a body is guest-controlled.
const maxNumberExponent = 1000

// canonicalNumber is a JSON number's exact value in lowest terms ("2", "5/2"), so 1, 1.0, 1e0 and 10e-1 are one number and 9007199254740993 is not 9007199254740992.
// ok is false for a literal whose exponent is too large to expand, which makes the body fall back to bytes.
func canonicalNumber(lit string) (string, bool) {
	if i := strings.IndexAny(lit, "eE"); i >= 0 {
		exp, err := strconv.Atoi(strings.TrimPrefix(lit[i+1:], "+"))
		if err != nil || exp > maxNumberExponent || exp < -maxNumberExponent {
			return "", false
		}
	}
	if len(lit) > 4096 {
		return "", false
	}
	r, ok := new(big.Rat).SetString(lit)
	if !ok {
		return "", false
	}
	return r.RatString(), true
}
