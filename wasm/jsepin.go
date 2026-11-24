package wasm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// JSEnginePin is the payload of the `cleat.js-engine` custom section that a
// TypeScript build stamps into the module (cleat#3830). It is written by
// packages/cleat-ts/build.sh and appended verbatim by cmd/cleat-ts-meta.
//
// WHICH FIELDS THE WORKER CAN ACT ON. Only PinVersion. The rest are facts
// about the module's own embedded runtime and build, not about the worker:
//
//   - Javy and RQuickJSCore describe the Javy and rquickjs the module carries
//     INSIDE it (the plugin is linked in), so there is no worker-side value to
//     compare them against -- the worker runs wasmtime and does not embed Javy
//     at all.
//   - PluginSHA256 and SDK256 are hashes of the cleat JS plugin and the
//     TypeScript SDK the module was built from. The worker implements the host
//     ABI that plugin binds to; it does not carry a copy, so again there is
//     nothing to compare against.
//
// Recording them here is still the point: they are what a human reads when a
// warning needs explaining, and a future worker that DOES carry one of these
// (a plugin registry, an SDK pin table) can start comparing it without
// re-stamping every module. PinVersion is the schema version of this payload,
// and that is a property of the pair -- the module's claim against what this
// worker can parse -- which is exactly the shape cleat#1054 established for
// abi_version.
type JSEnginePin struct {
	PinVersion         int    `json:"pin_version"`
	Javy               string `json:"javy"`
	RQuickJSCore       string `json:"rquickjs_core"`
	PluginSHA256       string `json:"plugin_sha256"`
	SDK256             string `json:"sdk_sha256"`
	DeterministicBuild bool   `json:"deterministic_build"`
}

// jsEnginePinSection is the custom section name. It matches the constant in
// cmd/cleat-ts-meta/main.go, which is the only writer; a rename there needs one
// here, or every pin becomes invisible in the direction that stays silent.
const jsEnginePinSection = "cleat.js-engine"

// CurrentJSEnginePinVersion is the highest cleat.js-engine pin schema this
// build understands.
//
// 1 is the schema build.sh writes today. Bump this when the payload gains a
// field whose ABSENCE changes how an older worker must treat the module -- not
// merely when a field is added, because a worker that ignores an unknown field
// is already correct for an additive change.
const CurrentJSEnginePinVersion = 1

// ReadJSEnginePin extracts the `cleat.js-engine` section.
//
// found=false is not an error: a module built before the pin existed, a
// non-TypeScript module, and a hand-written fixture all legitimately carry no
// pin, and the caller's job is to stay quiet for them rather than to complain.
// A present-but-unparseable section IS an error, because that is a module that
// claims to carry a pin and does not carry a readable one.
func ReadJSEnginePin(wasmBytes []byte) (pin *JSEnginePin, found bool, err error) {
	payload, err := readCustomSection(wasmBytes, jsEnginePinSection)
	if err != nil {
		var missing *sectionNotFoundError
		if errors.As(err, &missing) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var p JSEnginePin
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, false, fmt.Errorf("%s: invalid JSON: %w", jsEnginePinSection, err)
	}
	return &p, true, nil
}
