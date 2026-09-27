package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newUpdateRequestID mints the identity of one update request.
//
// WHY THE ROW NEEDS AN IDENTITY OF ITS OWN. cleat#1416 made an update name
// reusable -- an update is a request, and a request can be made twice -- so
// (workflow_id, update_name) stopped identifying a row. Nothing else in the row
// could take over:
//
//   - promise_id is nullable and means "someone is waiting". engine/updater.go
//     has an explicit branch for a request with no promise and three tests
//     create one, so making it the key would render a caller-less request
//     unrepresentable.
//   - created_at ties identity to a clock, and two requests can share a
//     microsecond.
//
// Generated here rather than by the database so all three dialects behave
// identically and the value exists before the INSERT, which is what lets
// CreateUpdateRequest report it without a round trip.
//
// 16 bytes from crypto/rand, the same shape and source as the promise id in
// cmd/cleat-worker. A collision would have to occur within one workflow, which
// is the scope the primary key covers.
func newUpdateRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate update request id: %w", err)
	}
	return "ureq-" + hex.EncodeToString(b), nil
}
