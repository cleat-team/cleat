// Package redacted names a body without printing it (cleat#4390, after cleat#4321).
//
// A plugin's output, a request or a payload that goes into an error is handed to the guest and from there reaches a stored run error, a log line and an API
// response, and by the time a workflow sees it the engine may have decrypted it. Egress redaction reads a JSON document and leaves other text alone, so a
// body embedded in a sentence is out of its reach and the sentence has to be built without it.
//
// Body gives the body's size and the start of its sha256. The same body gives the same text, so two messages can still be compared; the hash is a comparison
// aid and not a secret, and a body with little in it can be guessed from it.
//
// THIS IS A SECOND COPY, on purpose. engine/helpers.go has redactedBody, and this module (github.com/cleat-team/cleat/cleat) must keep building against the
// PUBLISHED root version, so it cannot import a package the root module added after that version. The two are held equal by tests that assert the same
// LITERAL expected values (engine's TestRedactedBodyIsItsSizeAndAShortHashAndNothingElse and this package's TestBodyIsItsSizeAndAShortHashAndNothingElse),
// not by shared code: change one and the other's test names the difference.
package redacted

import (
	"crypto/sha256"
	"fmt"
)

// Body is "[redacted: <n> bytes, sha256=<first 8 bytes in hex>]" for s: n is the WHOLE length in bytes, and none of s is in the text.
func Body(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("[redacted: %d bytes, sha256=%x]", len(s), h[:8])
}
