package wasm

import (
	"reflect"
	"strings"
	"testing"
)

// TestMetadataCarriesNoEntryPointParameterList pins the fact a CHANGELOG
// upgrade note depends on, so the note cannot go quietly wrong.
//
// WHY THIS IS A TEST AND NOT A SENTENCE. cleat#1065 made an absent declared
// entry-point parameter an error. cleat#1705 asked the obvious follow-up: a
// cron schedule's input is written once and replayed on every firing, so why
// is a schedule that can never bind not refused when it is REGISTERED, instead
// of failing once a minute forever with the reason in a worker log?
//
// The answer is that the host has nothing to check the input against. A
// schedule names a workflow and carries a JSON payload; the declared parameter
// list lives in generated guest code and in nothing the host can read. The
// `cleat.metadata` custom section is the one place a module describes itself to
// the host, and it carries deployment facts only -- name, versions, plugin
// deps, language.
//
// That is a structural reason rather than a decision deferred, and it is the
// reason the UPGRADE NOTES section gives an operator. If somebody adds a
// parameter list here -- which would be a REASONABLE thing to do, and is the
// obvious enabler for registration-time validation -- that sentence silently
// becomes false, and an operator reading it concludes validation is impossible
// when it has just become possible. Nothing else would notice: no count moves,
// no test fails, and prose is not re-read.
//
// So this test fails when the premise changes, and the failure message says
// what to go and fix. It is deliberately NOT a check that Metadata is frozen:
// adding an unrelated field is fine and must stay fine.
func TestMetadataCarriesNoEntryPointParameterList(t *testing.T) {
	// Substrings that would indicate a field describing an entry point's
	// parameters. Matched against both the Go field name and the JSON tag,
	// because a reader looking for either spelling should find this.
	//
	// "binding" IS DELIBERATELY NOT HERE, and it was on the first run. It
	// matched ChildBindingPolicy, which is about which VERSION of a child
	// workflow a parent resolves to -- nothing to do with binding a payload to
	// parameters. The word is overloaded in this repo and matching it produces
	// a failure that reads exactly like the real one, which is worse than
	// missing a field: it would send the next reader to rewrite a CHANGELOG
	// bullet that is still correct. "entrypoint"/"entry_point" already catch
	// the spellings a real parameter field would plausibly use.
	suspects := []string{"param", "arg", "entrypoint", "entry_point", "signature"}

	typ := reflect.TypeOf(Metadata{})
	var found []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.ToLower(f.Tag.Get("json"))
		name := strings.ToLower(f.Name)
		for _, s := range suspects {
			if strings.Contains(name, s) || strings.Contains(tag, s) {
				found = append(found, f.Name+" `"+f.Tag.Get("json")+"`")
				break
			}
		}
	}

	if len(found) > 0 {
		t.Fatalf("wasm.Metadata now carries what looks like entry-point parameter "+
			"information: %s\n\n"+
			"If that is what it is, the host CAN now check a stored payload against a\n"+
			"workflow's declared parameters, and two things say otherwise and are now\n"+
			"stale:\n"+
			"  - CHANGELOG.md, UPGRADE NOTES for cleat#1065 -- the bullet beginning\n"+
			"    \"A STORED payload is bound by whichever guest is current\", which tells\n"+
			"    operators a schedule cannot be validated at registration.\n"+
			"  - cleat#1705, which recorded that as the reason registration-time\n"+
			"    validation was out of scope.\n"+
			"Update both, then relax this test to name the field as expected.",
			strings.Join(found, ", "))
	}

	// A KNOWN-POSITIVE, because a scan that silently matches nothing reports
	// success in exactly the same way as one that correctly finds nothing --
	// and this scan would keep passing if `suspects` were emptied or the field
	// walk broke. Prove the matcher fires on a type that HAS such a field.
	type withParams struct {
		WorkflowName string   `json:"workflow_name"`
		EntryParams  []string `json:"entry_params"`
	}
	ptyp := reflect.TypeOf(withParams{})
	var hits int
	for i := 0; i < ptyp.NumField(); i++ {
		f := ptyp.Field(i)
		tag := strings.ToLower(f.Tag.Get("json"))
		name := strings.ToLower(f.Name)
		for _, s := range suspects {
			if strings.Contains(name, s) || strings.Contains(tag, s) {
				hits++
				break
			}
		}
	}
	if hits != 1 {
		t.Fatalf("the matcher in this test does not work: it found %d suspect fields "+
			"in a type that deliberately has exactly one (EntryParams). A scan that "+
			"cannot see what it looks for reports a clean result whatever the truth is.",
			hits)
	}
}
