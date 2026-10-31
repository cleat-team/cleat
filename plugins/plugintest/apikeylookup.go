package plugintest

import (
	"database/sql/driver"
	"fmt"
)

// APIKeyLookupColumns is the column list the tenant API-key lookup returns, in
// order: the tenant the key belongs to, then the key's own id (cleat#3623 added
// the second). It is here, once, so that a change to that statement's shape
// breaks one place and not the copy in every plugin's fake driver (cleat#3636).
var APIKeyLookupColumns = []string{"tenant_id", "key_id"}

// APIKeyLookupTestKeyID is the key id APIKeyLookupRows reports for a key it
// knows. Every copy this replaces used this same value.
const APIKeyLookupTestKeyID = "11111111-1111-1111-1111-111111111111"

// APIKeyLookupRows answers the tenant API-key lookup for a plugin's fake
// database/sql driver: apiKeys maps the hex of a key hash to a tenant id, and
// keyHash is the bound argument of the lookup. A key it does not know answers
// zero rows, as the real query does for an unknown or revoked key.
//
// A helper rather than a whole fake driver because what the copies shared was
// this answer; each package's driver still decides which statement it is
// answering (it matches the SQL text in its own dispatch, and some serve more
// than the key lookup), and still owns its own row type.
//
// A caller builds its own rows from the result:
//
//	cols, data := plugintest.APIKeyLookupRows(c.store.apiKeys, keyHash)
//	return &fakeRows{columns: cols, data: data}, nil
func APIKeyLookupRows(apiKeys map[string]string, keyHash []byte) (columns []string, data [][]driver.Value) {
	columns = append([]string(nil), APIKeyLookupColumns...)
	tid, ok := apiKeys[fmt.Sprintf("%x", keyHash)]
	if !ok {
		return columns, nil
	}
	return columns, [][]driver.Value{{tid, APIKeyLookupTestKeyID}}
}
