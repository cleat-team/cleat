package engine

import (
	"database/sql"
	"fmt"
)

// storedEvent is the on-disk encoding of one event: the ten sensitive column
// values and the payload JSON, in the form that goes into event_history.
//
// ONE ENCODING, ONE PLACE. Five writers insert into event_history --
// engine/flush.go's per-step insert, AdaptiveFlusher.prepareEntry, and the
// three stores' appendEventsInTx -- and until this function existed each
// carried its own copy of the encoding. Two of the five encrypted and three
// did not, and that was not visible from any one of them.
//
// So --encrypt-sensitive-payloads covered the per-event path and left
// FinalizeWorkflowSegment, ContinueAsNew, the defer phase and the audit
// writes storing plaintext, on the one dialect where the feature is
// supported. Measured before the fix, engine configured as
// cmd/cleat-worker configures it, one ordinary call event, columns read back
// on the privileged handle:
//
//	flushEvent           request = IFrsBnFXnQG8Lo1F9A7Di81/w0XtgZFlo...  (ciphertext)
//	AppendEventHistory   child_input = {"ssn":"123-45-6789"}             (plaintext)
//
// The two that did encrypt also disagreed with the read path about empty
// values -- seven of the ten fields skipped when empty, three not, and the
// read path skipping none (cleat#1377). One encoding makes that kind of
// divergence impossible rather than merely fixed.
//
// SCOPE. MySQL and SQL Server are deliberately not routed through this.
// Encryption is not supported on those dialects -- cmd/cleat-worker refuses
// --encrypt-sensitive-payloads unless --driver=postgres, and
// engine/mysql_events.go says the read-side guard "will never be true" --
// and making it real there needs their read paths first: between them they
// have four populateFromPayload call sites that read the payload column with
// no decryption at all, so encrypting their writes without that would turn an
// unread payload into silent field loss. Their appendEventsInTx still carry
// their own copies of the plaintext encoding.
//
// See cleat#1306.
type storedEvent struct {
	Request       string
	Response      string
	Err           string
	SignalPayload string
	ChildInput    string
	NewInput      string
	PluginInput   string
	PluginOutput  string
	PromiseResult string
	PromiseError  string

	// Payload is invalid rather than empty when the event type contributes no
	// payload keys, which is what every writer stored before this existed.
	Payload sql.NullString

	// Encoding is the payload_encoding column value for this row: the encoding
	// of Request and Response as they are about to be written.
	//
	// It lives here, on the result of the function that DID the encoding,
	// rather than being recomputed from the plaintext record at each INSERT.
	// Five writers call encodeEventForStorage (cleat#1380 gave them all the
	// same encoding after four of them had drifted); a value recomputed at the
	// call site can disagree with the bytes beside it, and nothing would fail
	// if it did -- the read path would simply decode with the wrong rule.
	//
	// `any` rather than int16 because NULL is a distinct, meaningful value:
	// see payloadEncodingFor (cleat#1319).
	Encoding any
}

// encodeEventForStorage maps a plaintext EventRecord to the values that go
// into event_history.
//
// TWO THINGS ARE DERIVED FROM THE PLAINTEXT RECORD AND MUST STAY THAT WAY,
// and getting either wrong is silent rather than loud:
//
//   - The payload JSON. eventRecordToPayload is called on rec, and the whole
//     payload is encrypted once. Building it from an already-encrypted record
//     would put ciphertext field values INSIDE the encrypted payload, and
//     populateFromPayload -- which runs after decryption on the read path --
//     would write them back over the correctly decrypted columns. That is the
//     real hazard behind the "would double-encrypt" note that used to sit on
//     PostgresStore.encryption, and it is an ordering constraint rather than a
//     reason not to encrypt: no caller of appendEventsInTx passes a record
//     whose fields are already ciphertext, because flush.go encrypts into
//     locals and never mutates rec.
//
//   - The checksum. Every writer computes it over the plaintext record and
//     VerifyWorkflowEvents recomputes it from the DECRYPTED record it loads,
//     so a checksum over ciphertext would make every encrypted workflow
//     verify as corrupt. It stays with the callers, which already do this,
//     rather than moving here -- the chain needs the previous checksum and
//     this function has no business knowing about the chain.
//
// Empty values are stored empty and never encrypted. EncryptString always
// returns at least a nonce and a GCM tag, so it cannot produce "", which is
// what lets decryptField read an empty stored value as "never encrypted"
// rather than as a decryption failure (cleat#1377).
func encodeEventForStorage(rec EventRecord, enc *PayloadEncryption, encrypt bool) (storedEvent, error) {
	out := storedEvent{
		Request:       tryEncodeBase64(rec.Request),
		Response:      tryEncodeBase64(rec.Response),
		Encoding:      payloadEncodingFor(rec),
		Err:           rec.Err,
		SignalPayload: rec.SignalPayload,
		ChildInput:    rec.ChildInput,
		NewInput:      rec.NewInput,
		PluginInput:   rec.PluginInput,
		PluginOutput:  rec.PluginOutput,
		PromiseResult: rec.PromiseResult,
		PromiseError:  rec.PromiseError,
	}

	// From the plaintext record, before anything below runs.
	payload, err := eventRecordToPayload(rec)
	if err == nil && len(payload) > 0 {
		out.Payload = sql.NullString{String: string(payload), Valid: true}
	}

	if !encrypt || enc == nil {
		return out, nil
	}

	// Request and Response are stored as base64 of the ciphertext, not as
	// base64 of base64: EncryptString already base64-encodes, and the read
	// path's tryDecodeBase64 undoes exactly one layer before handing the
	// bytes to Decrypt.
	for _, f := range []struct {
		name  string
		plain string
		dst   *string
	}{
		{"request", rec.Request, &out.Request},
		{"response", rec.Response, &out.Response},
		{"error", rec.Err, &out.Err},
		{"signal_payload", rec.SignalPayload, &out.SignalPayload},
		{"child_input", rec.ChildInput, &out.ChildInput},
		{"new_input", rec.NewInput, &out.NewInput},
		{"plugin_input", rec.PluginInput, &out.PluginInput},
		{"plugin_output", rec.PluginOutput, &out.PluginOutput},
		{"promise_result", rec.PromiseResult, &out.PromiseResult},
		{"promise_error", rec.PromiseError, &out.PromiseError},
	} {
		if f.plain == "" {
			continue
		}
		ciphertext, err := enc.EncryptString(f.plain)
		if err != nil {
			return storedEvent{}, fmt.Errorf("encode event for storage: encrypt %s: %w", f.name, err)
		}
		*f.dst = ciphertext
	}

	out.Payload, err = encodePayloadForStorage(out.Payload.String, enc, encrypt)
	if err != nil {
		return storedEvent{}, fmt.Errorf("encode event for storage: %w", err)
	}

	return out, nil
}

// encodePayloadForStorage is the payload half of encodeEventForStorage, split
// out for the call-intent path.
//
// That path does not build its own payload: CompleteCallIntent and
// ResolveCallIntent are handed one by the caller, which built it from the same
// plaintext record it computed the checksum from. So they need this and not
// the whole of encodeEventForStorage, and taking it from here rather than
// writing `if enc != nil && on { EncryptJSON }` at each site is the whole
// point of the file -- there were five copies of that before #1380.
//
// An empty payload stays empty and invalid rather than becoming the ciphertext
// of nothing, which is what every writer stored before encryption existed and
// is what decryptPayloadJSON's own `payloadStr != ""` guard expects.
func encodePayloadForStorage(payload string, enc *PayloadEncryption, encrypt bool) (sql.NullString, error) {
	if payload == "" {
		return sql.NullString{}, nil
	}
	if !encrypt || enc == nil {
		return sql.NullString{String: payload, Valid: true}, nil
	}
	encrypted, err := enc.EncryptJSON([]byte(payload))
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encrypt payload: %w", err)
	}
	return sql.NullString{String: string(encrypted), Valid: true}, nil
}

// payloadEncodingFor is the payload_encoding value for a row being written.
//
// Always base64 today, because tryEncodeBase64 encodes every non-empty value
// and EncryptString -- which replaces it when --encrypt-sensitive-payloads is
// on -- stores base64(ciphertext). Either way the stored bytes are base64, and
// the read path decodes before it decrypts.
//
// A function rather than a constant at the call sites so that the day one of
// those stops being true, there is one place that has to be told (cleat#1319).
func payloadEncodingFor(rec EventRecord) any {
	if rec.Request == "" && rec.Response == "" {
		// Nothing was encoded, so recording an encoding would be a claim about
		// bytes that do not exist. NULL here is the same "no answer" the legacy
		// rows carry, and decodePayload returns "" for an empty value before it
		// consults the column at all.
		return nil
	}
	return payloadEncodingBase64
}
