package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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
//
// cleat#2660: a typed CLIENT (cleat/pluginclients, above) is compiled
// against whatever SDK version a customer pinned, but calls a plugin
// running at whatever version the operator's worker deployed -- and
// nothing makes those two agree. DisallowUnknownFields, not plain
// Unmarshal, is what makes case (a) of that mismatch (a newer client
// sending a Req field this plugin's version does not declare) fail the
// call loudly instead of silently dropping the field and returning
// success. Decided as a plugin-side-only fix by the owner
// (cleat#2597, 2026-09-29, https://github.com/cleat-team/cleat/issues/2597#issuecomment-5894188198,
// via cleat#2671's "Plugin Clients" section in docs/reference/sdk-api.md):
// case (c), a renamed/removed Resp field silently zero-valued on the
// CLIENT's own decode (PluginCallTyped, cleat/plugin.go), is deliberately
// left lenient -- applying this same strictness there would turn every
// additive Resp change into a rollout hazard for no compatibility benefit,
// which is exactly the qualification point 1 of that section states. So
// this line is not a general "be stricter" change: it is the one place, of
// the two JSON decodes on either side of a plugin call, the owner decided
// should reject rather than tolerate.
//
// This also makes a Req field ADDITION no longer unconditionally safe
// (point 1's other qualification): a newer client's new field, sent to a
// plugin not yet upgraded to declare it, now fails the call instead of
// being dropped -- so a Req addition needs a plugin-first rollout. That is
// an operational ordering, not something this function can enforce; it is
// recorded here because it is the direct consequence of the line below.
func RegisterTyped[Req, Resp any](scope FuncRegistry, opts FuncOptions, fn func(ctx context.Context, req Req) (Resp, error)) error {
	wrapped := func(ctx context.Context, inputJSON string) (string, error) {
		var req Req
		if err := decodeStrict(inputJSON, &req); err != nil {
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

// decodeStrict is json.Unmarshal's exact strictness (rejects an unknown
// field AND rejects trailing data after the one JSON value, tolerating only
// trailing whitespace) obtained through json.Decoder, because
// DisallowUnknownFields exists only on Decoder. Decoder alone is looser
// than Unmarshal in a way that matters here: Decode reads one JSON value
// and stops, silently ignoring whatever follows it -- `{"a":1}garbage`
// decodes cleanly. Confirmed with dec.Token() reading exactly io.EOF next,
// the standard idiom for "nothing but whitespace remains".
func decodeStrict(inputJSON string, v any) error {
	dec := json.NewDecoder(strings.NewReader(inputJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after JSON value")
		}
		return err
	}
	return nil
}
