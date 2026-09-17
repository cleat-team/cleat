package engine

import (
	"reflect"
	"testing"
)

// TestEncryptedEventColumnsIsComplete pins EncryptedEventColumns against what
// encodeEventForStorage actually seals, behaviourally rather than textually.
//
// WHY IT MATTERS AND WHY IT IS NOT A RESTATEMENT. `cleatctl reseal-payloads`
// rewrites exactly the columns in that list. If an eleventh sealed column is
// added to the encoder and not to the list, the sweep skips it and still
// reports zero remaining -- unbound ciphertext left on disk under a green
// result, which is the failure mode the whole of cleat#1776/#1794 is about.
// A comment asking the next author to remember would not fail.
//
// It counts by REFLECTION over storedEvent rather than naming the fields,
// because naming them is the same coupling one level down: a test that lists
// ten fields cannot notice an eleventh either.
func TestEncryptedEventColumnsIsComplete(t *testing.T) {
	enc, err := NewPayloadEncryption(validKey(t))
	if err != nil {
		t.Fatalf("NewPayloadEncryption: %v", err)
	}

	// Every string field of EventRecord that the encoder might seal, set to a
	// distinct non-empty value -- the encoder skips empty fields, so an empty
	// one would look unsealed and hide a column.
	const marker = "cleat1794-plaintext"
	rec := EventRecord{
		Step: 0, EventType: EventTypeCall, Service: "s", Op: "o",
		Request: marker, Response: marker, Err: marker,
		SignalPayload: marker, ChildInput: marker, NewInput: marker,
		PluginInput: marker, PluginOutput: marker,
		PromiseResult: marker, PromiseError: marker,
	}

	plain, err := encodeEventForStorage(rec, enc, false, tenantA)
	if err != nil {
		t.Fatalf("encode without encryption: %v", err)
	}
	sealed, err := encodeEventForStorage(rec, enc, true, tenantA)
	if err != nil {
		t.Fatalf("encode with encryption: %v", err)
	}

	// A field is "sealed" if turning encryption on changed it. Payload is a
	// NullString; compare its String and require it Valid in both, or the
	// comparison is between two zero values and proves nothing.
	changed := 0
	pv, sv := reflect.ValueOf(plain), reflect.ValueOf(sealed)
	for i := 0; i < pv.NumField(); i++ {
		switch f := pv.Type().Field(i); f.Type.Kind() {
		case reflect.String:
			a, b := pv.Field(i).String(), sv.Field(i).String()
			if a != b {
				changed++
			}
		default:
			if f.Name == "Payload" {
				a := plain.Payload
				b := sealed.Payload
				if !a.Valid || !b.Valid {
					t.Fatalf("UNMEASURED: payload is not Valid in both encodings "+
						"(plain=%v sealed=%v), so the comparison below is between "+
						"zero values", a.Valid, b.Valid)
				}
				if a.String != b.String {
					changed++
				}
			}
		}
	}

	if changed == 0 {
		t.Fatalf("UNMEASURED: turning encryption on changed no field at all, so this " +
			"test is not observing the encoder")
	}
	if changed != len(EncryptedEventColumns) {
		t.Errorf("encodeEventForStorage seals %d fields but EncryptedEventColumns names %d.\n\n"+
			"They must agree: `cleatctl reseal-payloads` rewrites exactly the columns in "+
			"that list, so a sealed column missing from it is skipped by the sweep and "+
			"still counted as zero remaining -- unbound ciphertext on disk under a green "+
			"result. Add the new column to EncryptedEventColumns in "+
			"engine/event_storage_encoding.go.",
			changed, len(EncryptedEventColumns))
	}
}
