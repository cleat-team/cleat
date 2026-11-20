//go:build cgo

package csharp

import (
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4174, for a C# guest. --wasm-instance-timeout bounds GUEST EXECUTION, and a component guest's host calls were charged to it: a durable call whose
// service took longer than the instance timeout killed the run with "execution time limit exceeded". Measured for Python on a stock worker (a 25 s
// service completed, a 40 s service failed); the component path is shared, and this runs the same shape through a real C# component, not by inference.
//
// The fence is shortened (6 s) so the test is CI-sized: a 2 s service is under it, a 10 s service is over it. A single-attempt retry policy makes
// a_call_retry one durable call to a service that takes as long as the row says.
func TestCSharpASlowHostCallIsNotChargedToTheInstanceFence(t *testing.T) {
	const fence = 6 * time.Second
	for _, tc := range []struct {
		name    string
		service time.Duration
	}{
		{"under the fence (control)", 2 * time.Second},
		{"OVER the fence: the host call is not charged", 10 * time.Second},
	} {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			began := time.Now()
			res := runConformanceEntry(t, "a_call_retry", entryRun{
				Input:   retryInput("svc", "slow", 1),
				Backend: []engine.WasmtimeOption{engine.WithWasmtimeExecutionTimeout(fence)},
				Fail: func(_, _ string, _ int) error {
					time.Sleep(tc.service)
					return nil
				},
				Responses: map[string]string{"svc/slow": `{"ok":true}`},
			})
			elapsed := time.Since(began)
			t.Logf("fence %v, service %v -> err=%v result=%s after %.1fs", fence, tc.service, res.Err, res.Result, elapsed.Seconds())
			if res.Err != nil {
				t.Fatalf("the run failed: %v (a host call of %v is not guest execution time, and the fence is %v)", res.Err, tc.service, fence)
			}
			if !strings.Contains(res.Result, `"response"`) {
				t.Fatalf("result = %s, want the call's response", res.Result)
			}
			if elapsed < tc.service {
				t.Fatalf("finished after %v, before the service could have answered (%v)", elapsed, tc.service)
			}
		})
	}
}
