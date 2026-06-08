package engine

import "testing"

func TestExecResult_Fields(t *testing.T) {
	r := ExecResult{
		Result:    `{"ok":true}`,
		Suspended: true,
	}
	if r.Result != `{"ok":true}` {
		t.Errorf("Result = %q, want %q", r.Result, `{"ok":true}`)
	}
	if !r.Suspended {
		t.Error("Suspended = false, want true")
	}
}

func TestWazeroBackendImplementsWasmBackend(t *testing.T) {
	var _ WasmBackend = (*wazeroBackend)(nil)
}
