package engine

import "encoding/json"

// guestErrorText renders what a guest passed to cleat_complete(status=1) as
// text fit for an error message and for the error_msg column.
//
// The Go guest JSON-encodes it before the call -- both emission sites in
// wasm/exports.go wrap the message with encodeJSONString -- so the bytes that
// arrive are a JSON string *literal*: surrounded by quotes, with every interior
// quote backslash-escaped. Formatting that into an error verbatim gives an
// operator
//
//	host: export "ThreeCharges" failed: "durable call payments.Ship: [0] ..."
//
// with the escapes still in it, which is exactly the kind of encoded blob that
// makes a message get skimmed past rather than read.
//
// The fallback is not defensive padding: completeErr is not always JSON. The
// _start panic recovery in backend_wasmtime.go writes a plain Go string
// directly into the same variable, and a guest that dies before it can encode
// anything can leave a fragment there. Anything that does not decode as a JSON
// string is passed through unchanged, which is strictly better than reporting
// a decode failure in place of the guest's own account of what went wrong.
func guestErrorText(raw string) string {
	var decoded string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}
	return decoded
}
