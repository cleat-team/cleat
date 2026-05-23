package wasm

import (
	"fmt"
)

// witToEnvImport maps WIT-style import (module, function) pairs to flat "env"
// module function names. Used to rewrite decomposed componentize-py core WASM
// binaries so they link against the existing host function registrations.
//
// The outer key is the WIT module name (the import module in the decomposed
// core WASM). The inner key is the WIT function name (the import field name).
// The value is the flat function name to use under the "env" module.
// WitToEnvImport maps WIT (module, function) pairs to flat "env" function names.
// Exported for use by the wasmtime backend to register host functions under
// WIT module names in addition to "env".
var WitToEnvImport = map[string]map[string]string{
	"cleat:host-calls/durable-call": {
		"durable-call":           "cleat_call",
		"durable-call-retry":     "cleat_call_retry",
		"durable-call-heartbeat": "cleat_call_heartbeat",
	},
	"cleat:host-calls/durable-sleep": {
		"durable-sleep":  "cleat_sleep",
		"durable-now":    "cleat_now",
		"durable-random": "cleat_random",
		"durable-log":    "cleat_log",
	},
	"cleat:host-calls/durable-version": {
		"durable-version":     "cleat_version",
		"durable-min-version": "cleat_min_version",
	},
	"cleat:host-calls/durable-lifecycle": {
		"durable-defer":             "cleat_defer",
		"durable-continue-as-new":   "cleat_continue_as_new",
		"durable-poll-cancellation": "cleat_poll_cancellation",
	},
	"cleat:host-calls/durable-signals": {
		"durable-await-signals":       "cleat_await_signals",
		"durable-poll-signal":         "cleat_poll_signal",
		"durable-send-signal-and-wait": "cleat_send_signal_and_wait",
		"durable-reply-to-signal":     "cleat_reply_to_signal",
		"durable-signal-workflow":     "cleat_signal_workflow",
	},
	"cleat:host-calls/durable-children": {
		"durable-child-workflow":           "cleat_child_workflow",
		"durable-await-child":              "cleat_await_child",
		"durable-await-all-children":       "cleat_await_all_children",
		"durable-child-workflow-with-options": "cleat_child_workflow_with_options",
	},
	"cleat:host-calls/durable-promises": {
		"durable-create-promise":  "cleat_create_promise",
		"durable-await-promise":   "cleat_await_promise",
		"durable-resolve-promise": "cleat_resolve_promise",
		"durable-reject-promise":  "cleat_reject_promise",
	},
	"cleat:host-calls/durable-state": {
		"set-query-state": "set_query_state",
	},
	"cleat:host-calls/durable-handlers": {
		"durable-register-update-handler": "cleat_register_update_handler",
		"durable-register-query-handler":  "cleat_register_query_handler",
	},
	"cleat:host-calls/durable-messaging": {
		"durable-send":            "cleat_send",
		"durable-schedule-invoke": "cleat_schedule_invoke",
	},
	"cleat:host-calls/durable-identity": {
		"durable-workflow-id": "cleat_workflow_id",
		"durable-run-id":      "cleat_run_id",
	},
	"cleat:host-calls/plugin": {
		"plugin-call":           "plugin_call",
		"plugin-call-streaming": "plugin_call_streaming",
	},
	"cleat:host-calls/durable-lock": {
		"durable-acquire-lock": "cleat_acquire_lock",
		"durable-release-lock": "cleat_release_lock",
	},
	"cleat:host-calls/durable-scope": {
		"set-scope": "cleat_set_scope",
		"get-scope": "cleat_get_scope",
		"uuid":      "cleat_uuid",
	},
	"cleat:host-calls/durable-stream-state": {
		"set-state":    "cleat_set_state",
		"get-state":    "cleat_get_state",
		"delete-state": "cleat_delete_state",
		"incr-state":   "cleat_incr_state",
		"has-state":    "cleat_has_state",
		"list-state":   "cleat_list_state",
	},
	"cleat:host-calls/durable-extended-lifecycle": {
		"continue-as-new-versioned": "cleat_continue_as_new_versioned",
		"side-effect":               "cleat_side_effect",
	},
	"cleat:host-calls/durable-extended-children": {
		"child-workflow-in-schema": "cleat_child_workflow_in_schema",
	},
	"cleat:host-calls/durable-fetch": {
		"fetch": "cleat_fetch",
	},
}

// envModule is the canonical "env" module name imported by the host.
const envModule = "env"

// RewriteWitImports rewrites WIT-style import module names in a core WASM
// binary to flat "env" module names. This allows componentize-py compiled
// Python workflows (decomposed to core WASM) to link against the existing host
// function registrations.
//
// Returns nil if the binary has no WIT-style imports that need rewriting.
// Returns the modified binary on success, or an error if the binary is invalid.
func RewriteWitImports(wasmBytes []byte) ([]byte, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("not a valid WASM binary (too short)")
	}
	if string(wasmBytes[0:4]) != "\x00asm" {
		return nil, fmt.Errorf("not a valid WASM binary (bad magic)")
	}

	// Parse sections.
	type section struct {
		id      byte
		payload []byte
	}
	var sections []section
	pos := 8 // skip magic + version
	for pos < len(wasmBytes) {
		if pos >= len(wasmBytes) {
			return nil, fmt.Errorf("corrupt WASM: unexpected end at section start")
		}
		id := wasmBytes[pos]
		pos++
		if pos >= len(wasmBytes) {
			return nil, fmt.Errorf("corrupt WASM: unexpected end at section size")
		}
		size, n := decodeULEB128(wasmBytes[pos:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM: failed to decode section size")
		}
		pos += n
		if int(size) > len(wasmBytes)-pos {
			return nil, fmt.Errorf("corrupt WASM: section size %d overflows binary", size)
		}
		sections = append(sections, section{id: id, payload: wasmBytes[pos : pos+int(size)]})
		pos += int(size)
	}

	// Find the import section.
	importIdx := -1
	for i := range sections {
		if sections[i].id == 0x02 {
			importIdx = i
			break
		}
	}
	if importIdx < 0 {
		// No import section -- nothing to rewrite.
		return nil, nil
	}

	oldPayload := sections[importIdx].payload
	pos2 := 0
	importCount, n := decodeULEB128(oldPayload[pos2:])
	if n <= 0 {
		return nil, fmt.Errorf("corrupt WASM import section: failed to decode count")
	}
	pos2 += n

	// Rebuild the import section payload.
	var newPayload []byte
	newPayload = append(newPayload, encodeULEB128(importCount)...)

	anyRewrite := false
	for i := uint32(0); i < importCount; i++ {
		entryStart := pos2

		// Module name.
		modLen, n := decodeULEB128(oldPayload[pos2:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM import %d: failed to decode module name length", i)
		}
		pos2 += n
		if pos2+int(modLen) > len(oldPayload) {
			return nil, fmt.Errorf("corrupt WASM import %d: module name overflows section", i)
		}
		moduleName := string(oldPayload[pos2 : pos2+int(modLen)])
		pos2 += int(modLen)

		// Field name.
		fieldLen, n := decodeULEB128(oldPayload[pos2:])
		if n <= 0 {
			return nil, fmt.Errorf("corrupt WASM import %d: failed to decode field name length", i)
		}
		pos2 += n
		if pos2+int(fieldLen) > len(oldPayload) {
			return nil, fmt.Errorf("corrupt WASM import %d: field name overflows section", i)
		}
		fieldName := string(oldPayload[pos2 : pos2+int(fieldLen)])
		pos2 += int(fieldLen)

		// Import kind byte.
		if pos2 >= len(oldPayload) {
			return nil, fmt.Errorf("corrupt WASM import %d: missing kind byte", i)
		}
		kind := oldPayload[pos2]
		pos2++

		// Skip kind-specific descriptor to advance pos2 past the entire entry.
		switch kind {
		case 0x00: // func: type index (ULEB128)
			_, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return nil, fmt.Errorf("corrupt WASM import %d: bad func type index", i)
			}
			pos2 += n

		case 0x01: // table: reftype(1) + limits(flags:1, min:ULEB128, [max:ULEB128])
			if pos2 >= len(oldPayload) {
				return nil, fmt.Errorf("corrupt WASM import %d: truncated table import", i)
			}
			pos2++ // reftype
			if pos2 >= len(oldPayload) {
				return nil, fmt.Errorf("corrupt WASM import %d: truncated table limits", i)
			}
			flags := oldPayload[pos2]
			pos2++
			_, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return nil, fmt.Errorf("corrupt WASM import %d: bad table min", i)
			}
			pos2 += n
			if flags&0x01 != 0 { // has max
				_, n := decodeULEB128(oldPayload[pos2:])
				if n <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad table max", i)
				}
				pos2 += n
			}

		case 0x02: // memory: limits(flags:1, min:ULEB128, [max:ULEB128])
			if pos2 >= len(oldPayload) {
				return nil, fmt.Errorf("corrupt WASM import %d: truncated memory limits", i)
			}
			flags := oldPayload[pos2]
			pos2++
			_, n := decodeULEB128(oldPayload[pos2:])
			if n <= 0 {
				return nil, fmt.Errorf("corrupt WASM import %d: bad memory min", i)
			}
			pos2 += n
			if flags&0x01 != 0 { // has max
				_, n := decodeULEB128(oldPayload[pos2:])
				if n <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: bad memory max", i)
				}
				pos2 += n
			}

		case 0x03: // global: valtype(1) + mut(1)
			if pos2+2 > len(oldPayload) {
				return nil, fmt.Errorf("corrupt WASM import %d: truncated global import", i)
			}
			pos2 += 2

		default:
			return nil, fmt.Errorf("corrupt WASM import %d: unknown kind 0x%02x", i, kind)
		}

		// Now pos2 points past the entire import entry; entryStart points to the
		// beginning of this entry (ULEB128 length prefix for module name).
		entryEnd := pos2

		// Check if this import matches a WIT-style module/function pair.
		if witFuncs, ok := WitToEnvImport[moduleName]; ok {
			if newFieldName, ok2 := witFuncs[fieldName]; ok2 {
				anyRewrite = true

				// Write module name as "env".
				newPayload = append(newPayload, encodeULEB128(uint32(len(envModule)))...)
				newPayload = append(newPayload, []byte(envModule)...)

				// Write rewritten field name.
				newPayload = append(newPayload, encodeULEB128(uint32(len(newFieldName)))...)
				newPayload = append(newPayload, []byte(newFieldName)...)

				// Copy kind byte + descriptor unchanged from original.
				// The kind byte is at position entryEnd - (descriptor size + 1).
				// Rather than recompute, we track it via the original offset.
				// We need the position of the kind byte. Since we read module name
				// and field name, the kind byte was at:
				//   entryStart + modNameULEB128size + modLen + fieldNameULEB128size + fieldLen
				// But we already advanced pos2 past it. Let's use the known offsets.
				//
				// The kind byte is at position:
				//   entryStart
				//     + <ULEB128 size of modLen>
				//     + modLen
				//     + <ULEB128 size of fieldLen>
				//     + fieldLen
				//
				// We don't store these sizes, so we recompute by scanning forward
				// from entryStart to find the kind byte, OR we look backward from entryEnd.
				//
				// Simpler: compute kindPos from the positions we already tracked.
				// After reading module name: pos2 was at (entryStart + modULEBsize + modLen),
				// then advanced by fieldLEBsize + fieldLen to reach kind byte.
				//
				// We've already advanced pos2 past everything including the descriptor,
				// so entryEnd = pos2. The descriptor spans from kindByte+1 to entryEnd.
				// We need to find kindByte from the original data.
				//
				// Approach: scan forward from entryStart to find where the kind byte is.
				// Re-read the ULEB128 sizes.
				scanPos := entryStart
				ml, nn := decodeULEB128(oldPayload[scanPos:])
				if nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: reparse failed", i)
				}
				scanPos += nn // past module name ULEB128
				scanPos += int(ml) // past module name bytes
				fl, nn := decodeULEB128(oldPayload[scanPos:])
				if nn <= 0 {
					return nil, fmt.Errorf("corrupt WASM import %d: reparse failed", i)
				}
				scanPos += nn // past field name ULEB128
				scanPos += int(fl) // past field name bytes
				kindPos := scanPos
				// kind byte at kindPos, descriptor from kindPos+1 to entryEnd
				newPayload = append(newPayload, oldPayload[kindPos:entryEnd]...)
				continue
			}
		}

		// No rewrite needed: copy the entire original import entry unchanged.
		newPayload = append(newPayload, oldPayload[entryStart:entryEnd]...)
	}

	if !anyRewrite {
		// No WIT-style imports found that match our mapping.
		return nil, nil
	}

	// Rebuild the binary with the patched import section payload.
	sections[importIdx].payload = newPayload

	totalSize := 8 // magic + version
	for _, s := range sections {
		totalSize += 1 // section id
		totalSize += len(encodeULEB128(uint32(len(s.payload))))
		totalSize += len(s.payload)
	}

	newRaw := make([]byte, 0, totalSize)
	newRaw = append(newRaw, wasmBytes[0:8]...) // magic + version
	for _, s := range sections {
		newRaw = append(newRaw, s.id)
		newRaw = append(newRaw, encodeULEB128(uint32(len(s.payload)))...)
		newRaw = append(newRaw, s.payload...)
	}
	return newRaw, nil
}
