package engine

// cleat#3807. A jsonb result column is read on PostgreSQL with `result #>> '{}'`, which TEXT-EXTRACTS the value.
// That is what the sealed path needs (a sealed value is a JSON string scalar wrapping base64, and extracting it
// leaves the bare base64 the decrypt step reads), but for an UNSEALED scalar it is lossy:
//
//	stored jsonb          #>> '{}'     what the caller needed
//	"yes"                 yes          "yes"
//	"a\"b"                a"b          "a\"b"
//	""                    (empty)      ""
//	null                  (SQL NULL)   null
//
// A string result lost its quotes and its escapes, and null became an empty result -- so a Promise[string].Await
// ran json.Unmarshal on `yes` and failed, and GET /api/workflows/:id returned `result: yes`, which is not JSON.
// Numbers, booleans, objects and arrays survive extraction unchanged, which is why this went unnoticed. MySQL and
// SQL Server store the JSON text and return it as written.
//
// The fix keeps the extraction for the decrypt step and also reads the exact JSON text (`result::text`).

// exactJSONText chooses what a result column reads back as.
//
// extracted is the `#>> '{}'` value before any decrypt, decrypted is that value after the decrypt step, and raw is
// `result::text`. All three are "" for a column that is SQL NULL (a pending promise, an unfinished run).
//
//   - raw is empty: nothing was stored, so decrypted (empty) stands.
//   - the decrypt step changed the value: it was sealed and decrypted is the original JSON text, which already
//     carries its quotes. Untouched.
//   - otherwise the value was not sealed (or encryption is off, or the row predates it), so extraction is the only
//     thing that altered it and the exact text is returned. This includes JSON null, which extracts to SQL NULL
//     (extracted == "") and whose raw form is `null`.
func exactJSONText(extracted, decrypted, raw string) string {
	if raw == "" {
		return decrypted
	}
	if extracted == "" || decrypted == extracted {
		return raw
	}
	return decrypted
}
