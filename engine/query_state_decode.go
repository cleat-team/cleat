package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// decodeQueryState turns the stored query_state document into key -> value.
//
// ONE DECODER FOR ALL FOUR STORES. The single-key readers differ per dialect
// because each needs the database to reach into the document; reading the whole
// document does not, so the decode is shared and there is no per-dialect JSON
// behaviour to diverge.
//
// VALUES ARE DECODED AS json.RawMessage AND THEN UNQUOTED, rather than into
// map[string]string directly. SetQueryState stores whatever the guest
// published, and a guest that published a number or an object would make a
// map[string]string decode fail outright -- turning "show me what this run
// published" into an error precisely when the published value is the
// interesting part. A non-string value is rendered as its JSON instead, which
// is what GetQueryState's ->> already returns for the same row.
//
// A NULL or absent column is an empty map, not an error: a run that published
// nothing has an answer, and it is "nothing".
func decodeQueryState(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid {
		return map[string]string{}, nil
	}
	return queryStateFromJSON([]byte(raw.String))
}

// queryStateFromJSON is the decoder proper, split out so it can be tested
// without a database.
func queryStateFromJSON(data []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(data) == 0 || string(data) == "null" {
		return out, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("list query state: stored document is not a JSON object: %w", err)
	}
	for k, v := range doc {
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			// Note this also catches a JSON null, which unmarshals into a
			// string as "" rather than failing. That is deliberate and is the
			// consistent answer: PostgreSQL's ->> on a null yields SQL NULL,
			// which GetQueryState already surfaces as "". Two readers of one
			// column must not disagree about what is stored there.
			out[k] = str
			continue
		}
		// Not a JSON string -- a number, bool, array or object. Render it as
		// written rather than refusing the whole listing.
		out[k] = string(v)
	}
	return out, nil
}
