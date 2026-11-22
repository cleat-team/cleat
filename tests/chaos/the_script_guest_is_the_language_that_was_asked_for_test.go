//go:build chaos

package chaos

import (
	"bytes"
	"testing"
)

// Every row that deploys through deploy(t, env.db, env.wasm, ...) runs the guest CLEAT_CHAOS_GUEST names, because build() makes env.wasm from it. That is a claim about
// the BYTES a row deploys, and it was being read off the env var; this reads the bytes. A WebAssembly binary starts \0asm then a 4-byte version/layer word: a core module is
// 01 00 00 00 and a component is 0d 00 01 00. So a "Python" row that was quietly running the Go core module (or the reverse) fails here instead of passing for the wrong guest
// (the kill-while-held row, whose Python arm was reported as always deploying the Go guest, is the row this was written for: it deploys env.wasm).
func TestChaosTheDeployedScriptGuestIsTheLanguageThatWasAskedFor(t *testing.T) {
	e := newEnv(t)
	if len(e.wasm) < 8 || !bytes.Equal(e.wasm[:4], []byte{0x00, 'a', 's', 'm'}) {
		t.Fatalf("UNMEASURED: env.wasm (%d bytes) is not a WebAssembly binary", len(e.wasm))
	}
	core, component := []byte{0x01, 0, 0, 0}, []byte{0x0d, 0, 0x01, 0}
	got := e.wasm[4:8]
	switch guestLanguage() {
	case "python":
		if !bytes.Equal(got, component) {
			t.Errorf("CLEAT_CHAOS_GUEST=python but the deployed guest's version word is % x, want a component (% x): the row would run the wrong guest", got, component)
		}
	default:
		if !bytes.Equal(got, core) {
			t.Errorf("CLEAT_CHAOS_GUEST=%q but the deployed guest's version word is % x, want a core module (% x)", guestLanguage(), got, core)
		}
	}
	t.Logf("guest=%s version word % x, %d bytes", guestLanguage(), got, len(e.wasm))
}
