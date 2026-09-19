package engine

import (
	"database/sql"
	"testing"
)

// cleat#1571, reversing cleat#1119.
//
// cleat#1119 asked whether a run's published state should be enumerable and
// answered NO: a keyed-only reader means the caller must know what it is asking
// for, which keeps published state a contract rather than a bag. The HTTP 400
// was that decision, not an oversight.
//
// Reversed on the operational case cleat#1119 itself named -- "a run misbehaved
// and you do not know what it published" -- and on the owner's framing: query
// state is a semantically limited standard interface, and viewing it is part of
// that interface. Anything elaborate is app-specific and is not shoehorned in.
//
// These cover the decoder, which is where the interesting cases are. The store
// methods around it are one SELECT each.

func TestViewingPublishedStateSurvivesValuesAGuestActuallyPublishes(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want map[string]string
		why  string
	}{
		{"strings", `{"a":"1","b":"two"}`, map[string]string{"a": "1", "b": "two"},
			"the ordinary case"},
		{"a number", `{"count":42}`, map[string]string{"count": "42"},
			"SetQueryState forwards whatever the guest published; a number must not " +
				"turn the whole listing into an error"},
		{"a bool", `{"done":true}`, map[string]string{"done": "true"}, ""},
		{"an object", `{"o":{"k":"v"}}`, map[string]string{"o": `{"k":"v"}`},
			"rendered as written -- the same thing GetQueryState's ->> returns"},
		{"an array", `{"xs":[1,2]}`, map[string]string{"xs": `[1,2]`}, ""},
		{"a null value", `{"n":null}`, map[string]string{"n": ""},
			"EMPTY, not the text \"null\", because that is what the single-key reader " +
				"returns: PostgreSQL's '{\"n\":null}'::jsonb ->> 'n' is SQL NULL, which " +
				"GetQueryState surfaces as \"\". Two readers of one column must not " +
				"disagree about what is stored there"},
		{"the empty key", `{"":"v"}`, map[string]string{"": "v"},
			"a workflow CAN publish under \"\" and the column holds it; listing must " +
				"show it, since that is the one key the single-key reader makes awkward"},
		{"an empty document", `{}`, map[string]string{}, ""},
		{"a JSON null column", `null`, map[string]string{},
			"a run that published nothing has an answer, and it is nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := queryStateFromJSON([]byte(tc.doc))
			if err != nil {
				t.Fatalf("decode %s: %v -- %s", tc.doc, err, tc.why)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("decoded %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q -- %s", k, got[k], v, tc.why)
				}
			}
		})
	}
}

// A run that never published, and a run that does not exist, are the same
// answer as GetQueryState already gives for a key that is not there: empty, not
// an error.
func TestAnAbsentDocumentIsEmptyRatherThanAnError(t *testing.T) {
	got, err := decodeQueryState(sql.NullString{Valid: false})
	if err != nil {
		t.Fatalf("a NULL query_state produced an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a NULL query_state decoded to %v, want an empty map", got)
	}
}

// A stored document that is not an object IS an error, because it means the
// write path put something there that the read path cannot describe. Silently
// returning empty would report "this run published nothing" for a run that
// published something unreadable.
func TestADocumentThatIsNotAnObjectIsReportedRatherThanHidden(t *testing.T) {
	for _, doc := range []string{`"a string"`, `[1,2,3]`, `not json at all`} {
		if _, err := queryStateFromJSON([]byte(doc)); err == nil {
			t.Errorf("queryStateFromJSON(%q) returned no error; a document that is not "+
				"an object would be reported as 'published nothing', which is a "+
				"different fact", doc)
		}
	}
}
