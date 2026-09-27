package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// cleat#2307. The fullstack template ships a proxy that holds the tenant API key (proxy/main.go), and its own unit
// tests (proxy/main_test.go) are the fast statement of what it will and will not forward. Those tests run inside
// TestFullstackTemplateRunStartsAWorkflow, which needs Docker; this one runs them alone, on the scaffold a
// newcomer gets, with the Go toolchain and nothing else -- so a change to the proxy is checked wherever this
// package's tests run, and `go vet` covers what the tests do not.
func TestFullstackTemplateProxyPassesItsOwnVetAndTests(t *testing.T) {
	if testing.Short() || cleatBinary == "" {
		t.Skip("needs the cleat binary, which TestMain does not build in short mode")
	}
	root := t.TempDir()
	if out, err := runCleatIn(t, root, "init", "--template", "fullstack", "my-fullstack-app"); err != nil {
		t.Fatalf("cleat init --template fullstack: %v\n%s", err, out)
	}
	proj := filepath.Join(root, "my-fullstack-app")

	for _, args := range [][]string{
		{"vet", "./proxy/"},
		{"test", "./proxy/", "-count=1", "-timeout", "120s"},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = proj
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v in the scaffold: %v\n%s", args, err, out)
		}
	}
}
