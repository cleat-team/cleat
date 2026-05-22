//go:build !cgo

package wasmtest

import "github.com/cleat-team/cleat/internal/host"

func wasmtimeBackendOptions() []host.EngineOption { return nil }
