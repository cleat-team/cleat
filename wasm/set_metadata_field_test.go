package wasm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSetMetadataFieldRejectsANonObjectPayload pins that a payload which is valid
// JSON but not an object is an ERROR, not a panic.
//
// json.Unmarshal leaves its map target nil for the JSON literal `null`, and
// assigning into that nil map panics with "assignment to entry in nil map". It is
// reachable from `cleatctl deploy`: ReadMetadata accepts `null` as a zero
// Metadata, whose version (0) differs from the version being assigned, so the
// restamp calls this and the command crashed where it used to store the artifact
// unchanged (cleat#2944, found by cleat-review).
//
// A panic fails a Go test anyway, so the error assertions are what make the
// *reason* legible rather than just the absence of a crash.
//
// There are two error paths and the table says which case takes which. Only
// `null` unmarshals cleanly into a nil map and reaches the explicit check; an
// array, string, number or boolean is rejected by json.Unmarshal itself, because
// it cannot decode a non-object into map[string]json.RawMessage.
func TestSetMetadataFieldRejectsANonObjectPayload(t *testing.T) {
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	for _, tc := range []struct {
		name    string
		payload string
		wantMsg string // "" means any error will do
	}{
		{"null", `null`, "not a JSON object"},
		{"an array", `[1,2]`, ""},
		{"a bare string", `"hello"`, ""},
		{"a number", `42`, ""},
		{"a boolean", `true`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := writeCustomSection(header, sectionName, []byte(tc.payload))
			if err != nil {
				t.Fatalf("writeCustomSection: %v", err)
			}
			out, err := SetMetadataField(b, "workflow_version", json.RawMessage("2"))
			if err == nil {
				t.Fatalf("SetMetadataField accepted a payload of %s and returned %d bytes; "+
					"a non-object has no keys to patch and must be reported as such, not "+
					"patched or panicked on", tc.name, len(out))
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not say %q", err, tc.wantMsg)
			}
		})
	}
}

// The happy path, at the level the patch actually happens: only the named key
// changes, and every other key survives -- including one readCustomSection's
// struct reader cannot see.
func TestSetMetadataFieldChangesOneKeyAndKeepsTheRest(t *testing.T) {
	header := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	payload := `{"workflow_name":"provision","workflow_version":1,"abi_version":1,` +
		`"sdk_version":"0.3.2","created_at":"2026-10-02T00:00:00Z"}`
	b, err := writeCustomSection(header, sectionName, []byte(payload))
	if err != nil {
		t.Fatalf("writeCustomSection: %v", err)
	}

	out, err := SetMetadataField(b, "workflow_version", json.RawMessage("9"))
	if err != nil {
		t.Fatalf("SetMetadataField: %v", err)
	}

	got, err := readCustomSection(out, sectionName)
	if err != nil {
		t.Fatalf("readCustomSection on the patched binary: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("the patched payload is not valid JSON: %v", err)
	}

	if string(fields["workflow_version"]) != "9" {
		t.Errorf("workflow_version = %s, want 9", fields["workflow_version"])
	}
	for key, want := range map[string]string{
		"workflow_name": `"provision"`,
		"abi_version":   `1`,
		"sdk_version":   `"0.3.2"`,
		"created_at":    `"2026-10-02T00:00:00Z"`,
	} {
		gotVal, ok := fields[key]
		if !ok {
			t.Errorf("%s was dropped by the patch", key)
			continue
		}
		if string(gotVal) != want {
			t.Errorf("%s = %s, want %s", key, gotVal, want)
		}
	}
}
