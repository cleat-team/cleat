// Package gobench is the Go twin of ../workflows/bench.ts: n recorded side-effect host calls, then an optional sleep. It is the
// control the TypeScript numbers are compared against, so it must do exactly what that workflow does.
package gobench

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type StepsInput struct {
	N       int `json:"n"`
	SleepMs int `json:"sleep_ms"`
}

func BenchSteps(h cleat.HostCalls, in StepsInput) (string, error) {
	for i := 0; i < in.N; i++ {
		i := i
		if _, err := h.SideEffect(func() (string, error) { return strconv.Itoa(i), nil }); err != nil {
			return "", err
		}
	}
	if in.SleepMs > 0 {
		h.DurableSleep(time.Duration(in.SleepMs) * time.Millisecond)
	}
	out, _ := json.Marshal(map[string]int{"sum": in.N})
	return string(out), nil
}
