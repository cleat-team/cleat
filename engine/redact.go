package engine

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// maxRedactDepth limits the recursion depth when redacting nested JSON values
// to prevent stack overflow from malicious deeply-nested input.
const maxRedactDepth = 50

// sensitivePatterns lists the field-name fragments whose values are redacted.
// Matching is case-insensitive and is done on the WORDS of a key, not on the
// raw substring: see isSensitiveField.
var sensitivePatterns = []string{
	"token",
	"secret",
	"password",
	"credential",
	"api_key",
	"authorization",
	"api-key",
}

// substringPatterns are matched anywhere inside a single word of the key, as
// the whole list used to be matched anywhere inside the key. They are long and
// specific enough that a word containing one is, in practice, the secret
// itself (clientsecret, passwordhash, dbpassword, usercredentials).
var substringPatterns = []string{"secret", "password", "credential", "authorization", "apikey"}

// descriptorWords end a key that merely DESCRIBES a secret: token_usage,
// password_policy, credential_type, secret_name, token_expires_at. The value
// under such a key is a count, a setting or a label, not the secret, and
// redacting it rewrites ordinary data (cleat#3581). Only the LAST word counts,
// and only when a sensitive word comes before it, so `type` alone, or
// `token_type_secret`, are unaffected.
var descriptorWords = map[string]bool{
	"count": true, "counts": true, "usage": true, "limit": true, "limits": true,
	"budget": true, "type": true, "types": true, "policy": true, "policies": true,
	"ttl": true, "expiry": true, "expires": true, "expiration": true,
	"length": true, "name": true, "names": true, "at": true,
}

// tokenCountWords, ADJACENT to the plural `tokens`, say that it is a number of
// LLM tokens (max_tokens, input_tokens, tokens_used) rather than a list of
// credentials. Adjacent, not merely present: max_access_tokens is a list of
// secrets that happens to carry `max`.
//
// Decided, because a reviewer asked which way these fell: `cached`, `used`,
// `remaining` and `total` ARE count words, since they are the usage fields an
// LLM API reports (cached_tokens, total_tokens) and #3581's motivating case is
// exactly those. `per` is not one on its own; tokens_per_minute passes through
// tokenRateUnits, and tokens_per_user does not, so it is redacted. A count that
// is redacted is the cheaper mistake.
var tokenCountWords = map[string]bool{
	"max": true, "min": true, "input": true, "output": true, "prompt": true,
	"completion": true, "total": true, "cached": true, "reasoning": true,
	"num": true, "number": true, "count": true, "limit": true, "budget": true,
	"remaining": true, "used": true, "usage": true, "estimated": true, "avg": true,
}

// tokenRateUnits follow "per" in tokens_per_minute.
var tokenRateUnits = map[string]bool{
	"second": true, "sec": true, "minute": true, "min": true, "hour": true,
	"day": true, "request": true, "call": true,
}

// isTokenCount reports whether the word at i, a plural `tokens`, is used as a
// count: a count word beside it, or "per <time unit>" after it.
func isTokenCount(words []string, i int) bool {
	if i > 0 && tokenCountWords[words[i-1]] {
		return true
	}
	if i+1 < len(words) {
		if tokenCountWords[words[i+1]] {
			return true
		}
		if words[i+1] == "per" && i+2 < len(words) && tokenRateUnits[words[i+2]] {
			return true
		}
	}
	return false
}

// keyWords splits a field name into lowercase words at separators, at
// lower-to-upper case changes (apiKey), at an acronym's end (APIKey), and
// between letters and digits (token2).
func keyWords(name string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		isLetter := unicode.IsLetter(r)
		isDigit := unicode.IsDigit(r)
		if !isLetter && !isDigit {
			flush()
			continue
		}
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			switch {
			case unicode.IsLower(prev) && unicode.IsUpper(r):
				flush()
			case unicode.IsDigit(prev) != isDigit:
				flush()
			case unicode.IsUpper(prev) && unicode.IsUpper(r) && i+1 < len(rs) && unicode.IsLower(rs[i+1]):
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return words
}

// isFusedTokenCount reports whether w is a count word fused to "tokens"
// (maxtokens, inputtokens), the one fused form that is a count and not a list of
// credentials.
func isFusedTokenCount(w string) bool {
	return tokenCountWords[strings.TrimSuffix(w, "tokens")]
}

// isSensitiveWord reports whether words[i] marks the key as holding a secret.
func isSensitiveWord(words []string, i int) bool {
	w := words[i]
	for _, p := range substringPatterns {
		if strings.Contains(w, p) {
			return true
		}
	}
	if w == "api" && i+1 < len(words) && (words[i+1] == "key" || words[i+1] == "keys") {
		return true
	}
	switch {
	case strings.Contains(w, "tokeniz"):
		return false // tokenizer, tokenization: a text-processing term, never a credential
	case w == "tokens":
		return !isTokenCount(words, i)
	case strings.HasSuffix(w, "tokens") && isFusedTokenCount(w):
		return false // maxtokens, inputtokens
	case strings.Contains(w, "token"):
		// token, accesstoken, accesstokens, tokenvalue, tokenid, TOKENVALUE: any
		// other word holding "token" is the credential or names one. A FUSED word
		// is matched on its own, because a lowercase key has no separator to read.
		return true
	}
	return false
}

// isSensitiveField returns true if the field name marks its value as a secret.
//
// It reads the key as WORDS. It used to look for the substring "token" (and the
// others) anywhere in the key, which also caught max_tokens, input_tokens,
// token_usage, tokenizer and password_policy: counts and settings that every
// LLM workflow carries, replaced with "[REDACTED]" before the workflow saw them
// while the caller got a 2xx (cleat#3581). Any word holding `token` is still
// sensitive, fused or not (token, accesstoken, tokenvalue, accesstokens), except
// tokenizer-style words and the plural `tokens` when a count word is adjacent
// (max_tokens, tokens_used, tokens_per_minute); a key whose LAST word is a
// descriptor (token_usage, credential_type) is not sensitive.
//
// secret, password, credential, authorization and apikey keep their old
// meaning of "contained in a word", because a word containing one of them is
// nearly always the secret.
//
// Note: Unicode homoglyphs (e.g., Cyrillic 'o' vs Latin 'o') can bypass
// matching because normalization is not applied. This is acceptable
// because field names in this system are programmatically generated by SDKs
// and are always ASCII. Normalization via golang.org/x/text/unicode/norm is
// a future enhancement if non-ASCII field names become relevant.
func isSensitiveField(name string) bool {
	words := keyWords(name)
	last := len(words) - 1
	for i := range words {
		if !isSensitiveWord(words, i) {
			continue
		}
		if i < last && descriptorWords[words[last]] {
			continue
		}
		return true
	}
	return false
}

// looksLikeJWT returns true if s is a JSON Web Token: three dot-separated
// base64url segments of which the first two decode to JSON objects and the
// first carries an "alg". The test used to be "three non-empty base64url-ish
// segments", which is also semver (1.2.3), a file name (report.final.pdf) and a
// hostname (api.example.com), all of which were replaced with "[REDACTED]"
// (cleat#3581). A real token's header and claims are JSON objects, so requiring
// that keeps every real JWT and none of those.
//
// A JWS whose payload is not a JSON object (RFC 7515 allows any payload) is a
// token this does not match; a JWT's claims set is always an object (RFC 7519).
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 {
			return false
		}
		for _, c := range p {
			if !isBase64URLChar(c) {
				return false
			}
		}
	}
	var header struct {
		Alg *string `json:"alg"`
	}
	if !decodesToJSONObject(parts[0], &header) || header.Alg == nil {
		return false
	}
	var claims map[string]json.RawMessage
	return decodesToJSONObject(parts[1], &claims)
}

// decodesToJSONObject reports whether seg is base64url (padding optional) for
// a JSON object, decoding it into v.
func decodesToJSONObject(seg string, v any) bool {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

// isBase64URLChar returns true if c is a valid base64url character or '=' padding.
func isBase64URLChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '='
}

// Redact processes a JSON string and returns a redacted version where
// sensitive fields have their values replaced with "[REDACTED]". It handles:
//   - Nested objects recursively
//   - String values that look like JWTs
//   - Field names matching token, secret, password, credential, api_key,
//     authorization, and similar patterns (case-insensitive)
//
// If the input is not valid JSON, it is returned as-is.
func Redact(raw string) string {
	if !json.Valid([]byte(raw)) {
		// If it's not valid JSON, still check if the whole string looks like a JWT.
		if looksLikeJWT(raw) {
			// BARE, not quoted: this branch is not JSON, so the placeholder is
			// the whole value rather than a literal spliced into a document.
			// TestRedact_NonJSON_JWT caught exactly this distinction.
			return redactedPlain
		}
		return raw
	}

	spans, err := collectRedactSpans(raw)
	if err != nil || len(spans) == 0 {
		// Nothing to redact means the input is returned BYTE FOR BYTE. See
		// the note on collectRedactSpans for why that is the whole point.
		return raw
	}

	var b strings.Builder
	b.Grow(len(raw))
	prev := 0
	for _, sp := range spans {
		b.WriteString(raw[prev:sp.start])
		b.WriteString(redactedJSON)
		prev = sp.end
	}
	b.WriteString(raw[prev:])
	return b.String()
}

// redactedJSON is the placeholder as it appears in a JSON document: a string
// literal, quotes included, because it is spliced into text rather than
// marshalled from a Go value.
const redactedJSON = `"[REDACTED]"`

// redactedPlain is the same placeholder for a value that is not part of a JSON
// document.
const redactedPlain = "[REDACTED]"

// redactSpan is a half-open byte range of the input that must be replaced.
type redactSpan struct{ start, end int }

// collectRedactSpans finds the byte ranges to replace, WITHOUT reserialising
// the document.
//
// THIS FUNCTION EXISTS BECAUSE THE OBVIOUS IMPLEMENTATION CORRUPTS DATA.
// Redact used to json.Unmarshal into map[string]any, walk it, and json.Marshal
// the result. That round trip rewrites every payload it touches, whether or
// not anything was redacted, because a Go map has no key order and every JSON
// number becomes a float64:
//
//	{"zebra":1,"apple":2}        -> {"apple":2,"zebra":1}
//	{"id":12345678901234567890}  -> {"id":12345678901234567000}
//	{"id":9007199254740993}      -> {"id":9007199254740992}
//	{"amount":1.0}               -> {"amount":1}
//	{"big":1e300}                -> {"big":1e+300}
//
// The last four are not cosmetic. Any integer above 2^53 -- a snowflake id, a
// bigint primary key, a cents amount on a large ledger -- comes back a
// DIFFERENT NUMBER. Redact is applied on the read path to ten fields of every
// event record (store_events.go and its MySQL and SQL Server twins), and the
// replay path loads history through exactly that read, so a resumed workflow
// was handed values its first run never saw. That is the one thing a durable
// engine must not do, and it was being done by the engine itself.
//
// Collecting spans and splicing makes the no-redaction case the identity
// function and confines every change to the values actually being hidden.
func collectRedactSpans(raw string) ([]redactSpan, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var spans []redactSpan
	if err := scanRedactValue(dec, raw, 0, false, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

// scanRedactValue consumes one JSON value, appending spans for what must be
// hidden. redact is true when an enclosing key was sensitive, in which case
// the whole value becomes one span and its interior is not walked -- nested
// spans inside a span would splice twice.
func scanRedactValue(dec *json.Decoder, raw string, depth int, redact bool, spans *[]redactSpan) error {
	pre := int(dec.InputOffset())
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	start := redactValueStart(raw, pre)

	if d, ok := tok.(json.Delim); ok {
		if redact {
			if err := skipRedactComposite(dec); err != nil {
				return err
			}
			*spans = append(*spans, redactSpan{start, int(dec.InputOffset())})
			return nil
		}
		// Beyond the depth cap the subtree is left alone, which is what the
		// recursive implementation did on the same bound.
		if depth > maxRedactDepth {
			return skipRedactComposite(dec)
		}
		if d == '{' {
			for dec.More() {
				ktok, kerr := dec.Token()
				if kerr != nil {
					return kerr
				}
				key, _ := ktok.(string)
				if err := scanRedactValue(dec, raw, depth+1, isSensitiveField(key), spans); err != nil {
					return err
				}
			}
		} else {
			for dec.More() {
				if err := scanRedactValue(dec, raw, depth+1, false, spans); err != nil {
					return err
				}
			}
		}
		_, err := dec.Token() // the closing delimiter
		return err
	}

	end := int(dec.InputOffset())
	if redact {
		*spans = append(*spans, redactSpan{start, end})
		return nil
	}
	if str, ok := tok.(string); ok && looksLikeJWT(str) {
		*spans = append(*spans, redactSpan{start, end})
	}
	return nil
}

// skipRedactComposite consumes the remainder of an object or array whose
// opening delimiter has already been read.
func skipRedactComposite(dec *json.Decoder) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// redactValueStart walks forward from the end of the previous token to the
// first byte of the next value, stepping over the structural characters the
// decoder does not report as tokens.
//
// Only characters BEFORE the value are skipped, so a string value containing
// a colon or a comma is unaffected: the scan stops at its opening quote.
func redactValueStart(raw string, from int) int {
	for i := from; i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\n', '\r', ':', ',':
			continue
		}
		return i
	}
	return len(raw)
}

// redactValueDepth recursively redacts sensitive values with a recursion depth
// limit to prevent stack overflow from deeply-nested input.
func redactValueDepth(v any, depth int) any {
	if depth > maxRedactDepth {
		return v
	}
	switch val := v.(type) {
	case map[string]any:
		return redactMapDepth(val, depth)
	case []any:
		return redactSliceDepth(val, depth)
	case string:
		if looksLikeJWT(val) {
			return "[REDACTED]"
		}
		return val
	default:
		return val
	}
}

// redactMap redacts sensitive fields in a JSON object.
func redactMap(m map[string]any) map[string]any {
	return redactMapDepth(m, 0)
}

// redactMapDepth redacts sensitive fields in a JSON object with a recursion
// depth limit.
func redactMapDepth(m map[string]any, depth int) map[string]any {
	result := make(map[string]any, len(m))
	for k, v := range m {
		if isSensitiveField(k) {
			result[k] = "[REDACTED]"
		} else {
			result[k] = redactValueDepth(v, depth+1)
		}
	}
	return result
}

// redactSlice recursively redacts each element of a JSON array.
func redactSlice(arr []any) []any {
	return redactSliceDepth(arr, 0)
}

// redactSliceDepth recursively redacts each element of a JSON array with a
// recursion depth limit.
func redactSliceDepth(arr []any, depth int) []any {
	result := make([]any, len(arr))
	for i, v := range arr {
		result[i] = redactValueDepth(v, depth+1)
	}
	return result
}

// RedactMap is like Redact but operates on a map[string]any in-place,
// returning the same map. This is useful when the input is already parsed.
func RedactMap(m map[string]any) {
	for k, v := range m {
		if isSensitiveField(k) {
			m[k] = "[REDACTED]"
		} else {
			switch nested := v.(type) {
			case map[string]any:
				m[k] = redactMapDepth(nested, 1)
			case []any:
				m[k] = redactSliceDepth(nested, 1)
			}
		}
	}
}

// customPatterns holds additional sensitive-field patterns loaded from a file.
var customPatterns []string

// patternsFileLoaded is true after LoadRedactPatterns succeeds. It prevents
// the custom patterns from being overwritten by a nil/empty file.
var patternsFileLoaded bool

// LoadRedactPatterns reads a newline-separated list of sensitive-field
// substrings from path and appends them to the built-in patterns. Blank lines
// and lines starting with '#' are ignored. Returns an error if the file cannot
// be read.
func LoadRedactPatterns(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("redact patterns: open %s: %w", path, err)
	}
	defer f.Close()

	var added int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Avoid duplicates.
		found := false
		for _, p := range sensitivePatterns {
			if strings.EqualFold(p, line) {
				found = true
				break
			}
		}
		if !found {
			for _, p := range customPatterns {
				if strings.EqualFold(p, line) {
					found = true
					break
				}
			}
		}
		if !found {
			customPatterns = append(customPatterns, line)
		}
		added++
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("redact patterns: read %s: %w", path, err)
	}
	patternsFileLoaded = true
	return nil
}

// RedactOnRead applies retroactive redaction to a string that was loaded from
// persistent storage. It behaves identically to Redact but is named differently
// to clarify the caller's intent: this is read-path redaction for data that may
// have been stored before redaction was mandatory.
func RedactOnRead(data string) string {
	return Redact(data)
}

// ResetPatterns clears all custom patterns (for testing only).
func ResetPatterns() {
	customPatterns = nil
	patternsFileLoaded = false
}
