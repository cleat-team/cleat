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
//
// ONE FIELD IS EXEMPTED, cleat#2066: EntryPoints []string names which WASM
// exports a caller may start at, in source order -- nothing about their
// parameters, types or signature. It does not weaken what this test
// protects, because "which exports exist" and "what a schedule's stored
// payload must look like to bind one" are different questions; the second is
// the one CHANGELOG's cleat#1065/#1705 notes say the host cannot answer, and
// this field still cannot answer it. The exemption is amended openly rather
// than dodged by naming the field to miss the substring scan (which is how
// this field was named the first time, and was wrong: a reader of Metadata
// should not have to guess what a field called something else entirely
// holds). The exemption is STRUCTURAL, not by name alone -- isExemptField
// below also requires the type to be exactly []string, so a later field
// that keeps the same name but grows into carrying types or defaults still
// fails, and nothing except this one exact field is let through.
func isExemptField(f reflect.StructField) bool {
	return f.Name == "EntryPoints" &&
		f.Tag.Get("json") == "entry_points,omitempty" &&
		f.Type == reflect.TypeOf([]string(nil))
}

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
		if isExemptField(f) {
			continue
		}
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

// TestTheEntryPointsExemptionIsStructuralNotByName proves isExemptField lets
// through exactly the one real field it names, and nothing that merely looks
// like it -- a name collision with a different type, and a sibling field
// that would carry the parameter information the exemption exists to keep
// out. Both are throwaway types local to this test; nothing here should ever
// need to change Metadata itself.
func TestTheEntryPointsExemptionIsStructuralNotByName(t *testing.T) {
	// The real field passes.
	realField, ok := reflect.TypeOf(Metadata{}).FieldByName("EntryPoints")
	if !ok {
		t.Fatal("wasm.Metadata no longer has an EntryPoints field -- update or remove this test")
	}
	if !isExemptField(realField) {
		t.Fatal("isExemptField does not exempt wasm.Metadata's own EntryPoints field")
	}

	// Same name, wrong type: the exemption must not fire on name alone.
	type wrongType struct {
		EntryPoints map[string]string `json:"entry_points,omitempty"`
	}
	if f, _ := reflect.TypeOf(wrongType{}).FieldByName("EntryPoints"); isExemptField(f) {
		t.Fatal("isExemptField exempted a map-typed EntryPoints -- the type check is not structural")
	}

	// A sibling field naming what the exemption is there to keep out. Not
	// exempt, and still caught by the suspects scan above.
	type withSiblingParams struct {
		EntryPoints       []string `json:"entry_points,omitempty"`
		EntryPointDefault string   `json:"entry_point_default,omitempty"`
	}
	styp := reflect.TypeOf(withSiblingParams{})
	var caught bool
	suspects := []string{"param", "arg", "entrypoint", "entry_point", "signature"}
	for i := 0; i < styp.NumField(); i++ {
		f := styp.Field(i)
		if isExemptField(f) {
			continue
		}
		tag := strings.ToLower(f.Tag.Get("json"))
		name := strings.ToLower(f.Name)
		for _, s := range suspects {
			if strings.Contains(name, s) || strings.Contains(tag, s) {
				caught = true
			}
		}
	}
	if !caught {
		t.Fatal("a sibling EntryPointDefault field next to the exempt EntryPoints was not caught -- " +
			"the exemption is leaking past the one field it should cover")
	}
}
