package webhookingest

import (
	"encoding/json"
	"strings"
	"testing"
)

// An inbound webhook payload reaches the published event as the sender wrote
// it. cleat#1641.
//
// The payload was decoded into an `any` and re-encoded by the caller's
// json.Marshal, so every number float64 cannot hold exactly was rewritten
// before the event existed. This asserts the whole hop the handler performs --
// webhookPayload, then the marshal of the map it goes into -- because the
// defect lived in the SEAM between them, not in either half.
//
// No database: the handler needs one, this property does not, and gating it on
// a DSN would skip it on most CI jobs for a defect that is pure Go.
func TestAWebhookPayloadIsPublishedAsReceived(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // what must appear, verbatim, in the marshalled event data
	}{
		{
			name: "an integer larger than float64 holds exactly",
			body: `{"order":123456789012345678901234567890}`,
			want: `"payload":{"order":123456789012345678901234567890}`,
		},
		{
			// 2^53+1 is the smallest integer float64 cannot represent. It
			// comes back as 2^53, a one-off error -- the kind that reconciles
			// against nothing and is never noticed as corruption.
			name: "2^53+1 is not rounded to 2^53",
			body: `{"id":9007199254740993}`,
			want: `"payload":{"id":9007199254740993}`,
		},
		{
			name: "a decimal past float64 precision keeps its digits",
			body: `{"rate":0.12345678901234567890123}`,
			want: `"payload":{"rate":0.12345678901234567890123}`,
		},
		{
			// The non-JSON branch must still be a JSON STRING in the output,
			// not raw bytes spliced into the document. Without this arm a
			// "fix" that returned json.RawMessage unconditionally would pass
			// every arm above and emit invalid JSON here.
			name: "a non-JSON body is carried as a quoted string",
			body: `not json at all "quoted"`,
			want: `"payload":"not json at all \"quoted\""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eventData := map[string]any{"payload": webhookPayload([]byte(tc.body))}

			out, err := json.Marshal(eventData)
			if err != nil {
				t.Fatalf("marshalling the event data: %v", err)
			}
			if !json.Valid(out) {
				t.Fatalf("the event data is not valid JSON: %s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("the payload was rewritten between the request and the event.\n"+
					"  received: %s\n  want:     %s\n  got:      %s\n\n"+
					"webhookPayload must return raw bytes for JSON. cleat#1641.",
					tc.body, tc.want, out)
			}
		})
	}
}
