package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An AssemblyScript parameter with a declared default is OPTIONAL: an absent
// key binds the default instead of the getter's zero value. cleat#1065.
//
// WHY A DEFAULT IS THE MECHANISM AND NOT A NEW SYNTAX. The entry-point contract
// decided on cleat#1065 is "an absent declared parameter is an error, unless
// the parameter is declared optional". Python spells that with a parameter
// default; Rust with Option<T> or #[serde(default)]. AssemblyScript has neither
// nullable primitives nor a tag convention -- but it DOES have default
// parameter values, the same spelling as Python. The mechanism already existed
// in the language and this transform was discarding it.
//
// WHAT WAS WRONG BEFORE. The generated wrapper calls the inner function with
// EVERY argument explicitly, so AssemblyScript's own default never fires. An
// author writing `note: string = "FALLBACK"` got "" when the key was absent,
// silently, with their declared default dropped on the floor. Measured by
// driving _generateWrappers with a defaulted parameter: the emitted wrapper did
// not mention the default at all.
//
// ADDITIVE. Without a default the emitted code is byte-identical, so an absent
// parameter still binds zero until the contract flips.
func TestAnAssemblyScriptDefaultMakesAParameterOptional(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AS compilation in short mode")
	}
	if exec.Command("node", "--version").Run() != nil || exec.Command("npx", "--version").Run() != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node and npx are expected on CI runners")
		}
		t.Skip("requires node and npx")
	}

	// IT MUST COMPILE, which the generation-level check below cannot tell you.
	// The emitted binding is a ternary spliced into a `let` initialiser, and a
	// default is arbitrary source text lifted out of the author's file -- so
	// "the transform produced something" and "asc accepted it" are different
	// claims and this asserts the second.
	//
	// Both a reference type and a value type, because they take different arms
	// of _getDeserializeCode: a string default and an i32 default are wired
	// separately and only one of them being right would be invisible here
	// otherwise.
	fx := compileASFixture(t, `
import { HostCalls, cleatEntry } from "@cleat/sdk";

@cleatEntry()
function myWorkflow(h: HostCalls, userID: string, note: string = "FALLBACK", count: i32 = 7): string {
  return "{\"u\":\"" + userID + "\",\"n\":\"" + note + "\",\"c\":" + count.toString() + "}";
}
`)

	if fx.err != nil {
		t.Fatalf("asc failed to compile an entry point with defaulted parameters: %v\n\n%s\n\n"+
			"A compilation failure here is a FAILURE, not a skip: the transform emits\n"+
			"  let note: string = _t_note == -1 ? (\"FALLBACK\") : (_parser.getString(...));\n"+
			"and if that is malformed -- a bad splice, an unparenthesised default, a default\n"+
			"lifted with the wrong source range -- this is the only thing that would say so.",
			fx.err, fx.out)
	}
	if _, statErr := os.Stat(fx.wasmPath); statErr != nil {
		t.Fatalf("asc reported success but produced no .wasm at %s: %v\n\n%s",
			fx.wasmPath, statErr, fx.out)
	}

	// THE CONTROL. Without it, "a defaulted entry compiles" is also satisfied by
	// a transform that silently ignored the defaults again -- which is exactly
	// the state this change is fixing, and it compiles perfectly well.
	t.Run("the emitted binding actually consults the default", func(t *testing.T) {
		cmd := exec.Command("node", "-e", `
const T = require(process.env.CLEAT_AS_TRANSFORM);
const t = new (T.default || T)();
// A mock built to model the PARSER, not the implementation: the real
// initializer shape was dumped from an asc run and carries
// {kind, range, literalKind, value} with range.source.text. cleat#1067 is in
// that file because a mock shaped to the implementation could never disagree
// with it.
const stmt = {
  name: { text: "wf" },
  signature: {
    parameters: [
      { name: { text: "h" },      type: { name: { identifier: { text: "HostCalls" } } } },
      // TWO user parameters, deliberately. A lone string parameter takes the
      // transform's whole-payload fast path -- the payload IS the parameter --
      // so no named binding is emitted and a default there means nothing. With
      // one parameter this check reports IGNORES_DEFAULT against a correct
      // transform, which is how it read before this comment existed.
      { name: { text: "userID" }, type: { name: { identifier: { text: "string" } } } },
      { name: { text: "note" },   type: { name: { identifier: { text: "string" } } },
        initializer: { kind: 0, literalKind: 1, value: "FALLBACK",
          range: { start: 0, end: 10, source: { text: '"FALLBACK"' } } } }
    ],
    returnType: { text: "string" }
  },
  decorators: [ { name: { text: "cleatEntry" } } ]
};
const code = t._generateWrappers([t._extractEntryInfo(stmt)]);
process.stdout.write(code.indexOf('_t_note == -1 ? ("FALLBACK")') >= 0 ? "USES_DEFAULT" : "IGNORES_DEFAULT");
`)
		cmd.Env = append(os.Environ(),
			"CLEAT_AS_TRANSFORM="+filepath.Join(transformDir(t), "index.js"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("driving the transform: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "USES_DEFAULT" {
			t.Errorf("the emitted binding does not consult the declared default (%s).\n\n"+
				"cleat#1065: an AssemblyScript author writing `note: string = \"FALLBACK\"` "+
				"gets \"\" for an absent key, because the generated wrapper passes every "+
				"argument explicitly and AS's own default never fires.", got)
		}
	})
}
