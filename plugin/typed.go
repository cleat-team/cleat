package plugin

import (
	"context"
	"encoding/json"
	"fmt"
)

// RegisterTyped registers a host function whose request and response are Go
// types rather than raw JSON strings. It wraps fn in exactly the
// json.Unmarshal / json.Marshal pair every hand-written PluginFunc already
// writes by hand at the top and bottom of its body (see
// plugins/slacknotify/host_functions.go's sendMessage for the shape this
// replaces) and calls scope.Register with an ordinary PluginFunc -- so
// everything Register already does (SecretOnlyFields validation, event
// history recording, deterministic replay) applies unchanged.
// RegisterTyped is a marshaling convenience, not a second registration
// path.
//
// cleat#2626: this is also what makes a typed CALLER possible. A generator
// producing cleat.NewPluginFunc[Req,Resp] declarations for a workflow author
// to call reads the Req/Resp type arguments off RegisterTyped call sites in
// the plugin's own source -- the plugin's real, compiler-checked
// registration is the one and only source of truth. Deliberately never a
// hand-maintained plugin.json manifest: see cleat#2656 for a manifest that
// drifted from its plugin's real types with nothing catching it.
func RegisterTyped[Req, Resp any](scope FuncRegistry, opts FuncOptions, fn func(ctx context.Context, req Req) (Resp, error)) error {
	wrapped := func(ctx context.Context, inputJSON string) (string, error) {
		var req Req
		if err := json.Unmarshal([]byte(inputJSON), &req); err != nil {
			return "", fmt.Errorf("%s: invalid input: %w", opts.Name, err)
		}
		resp, err := fn(ctx, req)
		if err != nil {
			return "", err
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return "", fmt.Errorf("%s: marshaling output: %w", opts.Name, err)
		}
		return string(out), nil
	}
	return scope.Register(opts, wrapped)
}
