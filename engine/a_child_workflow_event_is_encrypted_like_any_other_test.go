package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAChildWorkflowEventIsEncryptedLikeAnyOther is cleat#2328's regression
// test: PostgresStore.StartChildWorkflowAtomic used to build its own
// plaintext INSERT for the parent's child_workflow event, bypassing
// encodeEventForStorage entirely -- so a child's input, and the
// parent_workflow_id/parent_close_policy carried in `payload`, were never
// covered by --encrypt-sensitive-payloads despite child_input being listed
// in EncryptedEventColumns.
//
// This goes through the real StartChildWorkflowAtomic call the live
// ChildWorkflow host call uses (engine/children.go:244), against a real
// database, and reads the RAW row back with a second connection that never
// goes through the store's decryption path -- the same shape as
// TestAnEmptyFieldIsNotADecryptionFailure, and for the same reason: a test
// that only calls LoadEventHistory would pass on plaintext, because
// LoadEventHistory's decryptField treats a value it cannot decrypt as
// "never encrypted" and returns it unchanged (empty ciphertext aside).
func TestAChildWorkflowEventIsEncryptedLikeAnyOther(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}
	store := NewPostgresStore(db).WithEncryption(enc, true)
	ctx := context.Background()
	parentID := appendChainWorkflow(t, store)

	// A marker distinctive enough that it cannot appear in ciphertext by
	// chance, and that would be obviously wrong to find in plaintext on
	// disk -- the same technique cleat#2312's live measurement used.
	marker := fmt.Sprintf("cleat2328-marker-%d", time.Now().UnixNano())
	childInput := fmt.Sprintf(`{"secret":%q}`, marker)

	event := EventRecord{
		Step:              0,
		EventType:         EventTypeChildWorkflow,
		ChildName:         "test",
		ChildInput:        childInput,
		ParentWorkflowID:  parentID,
		ParentClosePolicy: "ABANDON",
		TimestampMs:       time.Now().UnixMilli(),
	}

	// def_name/def_version "test"/1 is the definition appendChainWorkflow's
	// deployFaultTestDef already deployed -- reused here so the child
	// instance's own FOREIGN KEY (def_name, def_version) is satisfied
	// without deploying a second definition.
	childID, err := store.StartChildWorkflowAtomic(ctx, "", parentID, "test", childInput, 1, "ABANDON", event, 0)
	if err != nil {
		t.Fatalf("StartChildWorkflowAtomic: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, childID)
	})

	// The bug, measured directly: read the columns nothing has decrypted.
	var rawChildInput, rawPayload string
	if err := db.QueryRow(
		`SELECT child_input, COALESCE(payload::text, '') FROM event_history WHERE workflow_id = $1 AND step = 0`,
		parentID,
	).Scan(&rawChildInput, &rawPayload); err != nil {
		t.Fatalf("read raw row: %v", err)
	}

	if rawChildInput == childInput {
		t.Errorf("child_input is stored as plaintext: %q", rawChildInput)
	}
	if strings.Contains(rawChildInput, marker) {
		t.Errorf("child_input on disk contains the plaintext marker: %q", rawChildInput)
	}
	if strings.Contains(rawPayload, marker) {
		t.Errorf("payload on disk contains the plaintext marker: %q", rawPayload)
	}
	if rawPayload == "" {
		t.Fatalf("PRECONDITION FAILED: payload column is empty, so parent_workflow_id/" +
			"parent_close_policy were never written and nothing below is measuring them")
	}

	// The control this test's own PRECONDITION rests on: a store WITHOUT
	// encryption stores this event as plaintext, so if the assertions above
	// somehow passed against an unencrypted write, that would prove nothing.
	// Confirmed the other direction, not just asserted: EncryptString/sealJSON
	// can never return "", so ciphertext this long is not an artifact of an
	// empty seal -- see decryptField's doc for why that property matters.
	if len(rawChildInput) < len(childInput) {
		t.Fatalf("PRECONDITION FAILED: stored child_input (%d bytes) is shorter than the "+
			"plaintext (%d bytes) it supposedly seals -- AES-256-GCM ciphertext is always "+
			"longer than its plaintext, so this is not measuring encryption",
			len(rawChildInput), len(childInput))
	}

	// The read path must still round-trip: this is not just "write
	// ciphertext", it is "write ciphertext AND read it back correctly".
	got, err := store.LoadEventHistory(ctx, parentID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("PRECONDITION FAILED: loaded %d events, want 1", len(got))
	}
	if got[0].ChildInput != childInput {
		t.Errorf("LoadEventHistory: ChildInput = %q, want %q (round trip failed)", got[0].ChildInput, childInput)
	}
	if got[0].ParentWorkflowID != parentID {
		t.Errorf("LoadEventHistory: ParentWorkflowID = %q, want %q (payload round trip failed)",
			got[0].ParentWorkflowID, parentID)
	}
}
