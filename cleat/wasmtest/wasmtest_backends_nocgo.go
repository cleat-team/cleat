//go:build !cgo

package wasmtest

import "github.com/cleat-team/cleat/engine"

func wasmtimeBackendOptions() []engine.EngineOption { return nil }
