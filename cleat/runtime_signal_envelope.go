package cleat

import "encoding/json"

// signalReplyKey is the reserved envelope key carrying a request/reply
// signal's reply address. The address is a promise ID: SendSignalAndWait
// creates a promise, sends its ID under this key, and awaits it; the receiver
// answers by resolving that promise. IMPROVEMENT-PLAN 3.220.
//
// A payload sent with SignalWorkflow is delivered verbatim and never carries
// this key, so a receiver can tell a request that wants a reply from one that
// does not: SignalResult.ReplyTo is empty for the latter.
const signalReplyKey = "cleat_reply_to"

type signalEnvelope struct {
	ReplyTo string `json:"cleat_reply_to"`
	Payload string `json:"payload"`
}

// encodeSignalEnvelope wraps a payload with the address to reply to.
//
// The caller's payload is carried as a JSON *string* rather than spliced into
// it as an extra key. Splicing is what cleattest did before this change, and
// it has two failure modes a wrapper does not: it silently sends no
// correlation ID at all when the payload is not a JSON object -- so the
// receiver cannot reply and the sender waits out its whole timeout -- and it
// collides with a user key of the same name. A string round-trips any
// payload unchanged: object, array, bare scalar, or empty.
func encodeSignalEnvelope(replyTo, payload string) (string, error) {
	b, err := json.Marshal(signalEnvelope{ReplyTo: replyTo, Payload: payload})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeSignalEnvelope reports whether raw is a reply envelope, and if so
// returns the reply address and the caller's original payload.
//
// It requires exactly the two envelope keys and a non-empty address, so an
// ordinary payload that merely happens to carry a "cleat_reply_to" field
// among others is not mistaken for one. That is a discriminator on the
// object's shape, not a substring search: the distinction this repo keeps
// relearning is that a text match cannot tell a thing from a mention of it.
func decodeSignalEnvelope(raw string) (replyTo, payload string, ok bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return "", "", false
	}
	if len(fields) != 2 {
		return "", "", false
	}
	if _, has := fields[signalReplyKey]; !has {
		return "", "", false
	}
	if _, has := fields["payload"]; !has {
		return "", "", false
	}
	var env signalEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return "", "", false
	}
	if env.ReplyTo == "" {
		return "", "", false
	}
	return env.ReplyTo, env.Payload, true
}

// unwrapSignalResult moves a reply envelope's address into ReplyTo and
// restores Payload to what the sender passed. A non-envelope payload is
// returned untouched with an empty ReplyTo.
func unwrapSignalResult(r SignalResult) SignalResult {
	if replyTo, inner, ok := decodeSignalEnvelope(r.Payload); ok {
		r.ReplyTo = replyTo
		r.Payload = inner
	}
	return r
}
