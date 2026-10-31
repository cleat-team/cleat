package plugintest

import (
	"reflect"
	"testing"
)

// The helper is what every converted plugin test now stands on, so what it
// answers is pinned here once rather than inferred from a plugin's tests.
func TestAPIKeyLookupRows(t *testing.T) {
	keys := map[string]string{"0a0b": "tenant-1"}

	cols, data := APIKeyLookupRows(keys, []byte{0x0a, 0x0b})
	if want := []string{"tenant_id", "key_id"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(data) != 1 || data[0][0] != "tenant-1" || data[0][1] != APIKeyLookupTestKeyID {
		t.Errorf("known key: data = %v, want one row [tenant-1 %s]", data, APIKeyLookupTestKeyID)
	}

	cols, data = APIKeyLookupRows(keys, []byte{0xff})
	if len(data) != 0 {
		t.Errorf("unknown key: data = %v, want no rows", data)
	}
	if len(cols) != 2 {
		t.Errorf("unknown key still answers the column list, got %v", cols)
	}

	// The result must not alias the exported column list: a caller that appends
	// to or edits its copy must not change the next test's.
	cols, _ = APIKeyLookupRows(keys, []byte{0xff})
	cols[0] = "edited"
	if APIKeyLookupColumns[0] != "tenant_id" {
		t.Error("APIKeyLookupRows returned the shared column slice, not a copy")
	}
}
