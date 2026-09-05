// Package allhostcalls calls every method on cleat.HostCalls.
//
// It exists to be COMPILED, not run. Its only job is to make `cleat build`
// generate a host adapter covering every entry in wasm/adapter_metadata.go and
// then hand that generated Go to the compiler.
//
// Nothing else does that. Every test in ./wasm/ inspects the generated source
// as a string, so a generated identifier that does not exist, or a closure
// whose signature the HostCallsOptions struct will not accept, passes all of
// them. Four host calls shipped broken that way -- locks, promises and
// SideEffect could not be used from any Go WASM workflow. See
// IMPROVEMENT-PLAN.md 3.204.
//
// When adding a host call, add it here too. TestEveryHostCallIsExercised fails
// if a HostCalls method is missing from this file.
package allhostcalls

import (
	"time"

	"github.com/cleat-team/cleat/cleat"
)

type payload struct {
	Field string `json:"field"`
}

// Entry is the workflow entry point. The build only needs one.
func Entry(h cleat.HostCalls, input string) (string, error) {
	var out payload

	// ---- durable calls ----
	_, _ = h.Call("svc", "op", "{}")
	_, _ = h.DurableCall("svc", "op", "{}")
	_ = h.DurableCallJSON("svc", "op", "{}", &out)
	_ = h.DurableCallTyped("svc", "op", payload{}, &out)
	_, _ = h.DurableCallWithRetry("svc", "op", "{}", cleat.RetryPolicy{})
	_, _ = h.DurableCallWithOptions(cleat.CallOptions{}, "svc", "op", "{}")
	_ = h.DurableCallTypedWithOptions(cleat.CallOptions{}, "svc", "op", payload{}, &out)
	_ = h.DurableCallJSONWithOptions(cleat.CallOptions{}, "svc", "op", "{}", &out)
	_ = h.DurableSend("svc", "op", "{}")
	_ = h.ScheduleInvoke("svc", "op", "{}", 1000)

	// ---- http ----
	_, _, _ = h.DurableFetch("http://x", "GET", map[string]string{"k": "v"}, "")
	_ = h.DurableFetchJSON("http://x", "GET", nil, "", &out)
	_, _, _ = h.FetchGet("http://x")
	_ = h.FetchGetJSON("http://x", &out)

	// ---- plugins ----
	_, _ = h.PluginCall("p", "f", "{}")
	_, _ = h.PluginCallStreaming("p", "f", "{}")

	// ---- crons ----
	_, _ = h.ScheduleCron("wf", "* * * * *", "UTC", "{}")
	_ = h.DeleteCron("sched-1")
	_, _ = h.ListCrons()

	// ---- locks ----
	_, _ = h.AcquireLock("k", time.Second)
	_, _ = h.AcquireLockMs("k", 1000)
	_ = h.ReleaseLock("k")

	// ---- side effects, defer, sleep ----
	_, _ = h.SideEffect(func() (string, error) { return "x", nil })
	_, _ = h.DurableDefer("cleanup")
	h.DurableSleep(time.Second)
	h.DurableSleepMs(1000)

	// ---- promises ----
	pid, _ := h.CreatePromise("p")
	_, _, _ = h.AwaitPromise(pid, time.Second)
	_, _, _ = h.AwaitPromiseMs(pid, 1000)
	_ = h.ResolvePromise(pid, "v")
	_ = h.RejectPromise(pid, "e")

	// ---- children ----
	rid, _ := h.ChildWorkflow("child", "{}")
	_, _ = h.ChildWorkflowTyped("child", payload{})
	_, _ = h.ChildWorkflowWithOptions("child", "{}", cleat.ChildWorkflowOptions{})
	_, _ = h.AwaitChild(rid)
	_ = h.AwaitChildTyped(rid, &out)
	_, _ = h.AwaitAllChildren([]string{rid})
	_, _, _ = h.AwaitAnyChild([]string{rid})
	_, _, _ = h.PollChild(rid)

	// ---- signals ----
	_ = h.AwaitSignals([]string{"s"}, time.Second)
	_, _ = h.AwaitSignalsWithQuorum([]string{"s"}, 1, 0, time.Second)
	_, _, _, _ = h.DurableAwaitSignals([]string{"s"}, 1000)
	_, _, _ = h.PollSignal("s")
	_ = h.PollSignals([]string{"s"})
	_ = h.SignalWorkflow(rid, "s", "{}")
	_, _ = h.SendSignalAndWait(rid, "s", "{}", time.Second)
	_ = h.ReplyToSignal("corr", "{}")

	// ---- lifecycle ----
	_, _ = h.PollCancellation()
	_ = h.ContinueAsNew("{}")
	_ = h.ContinueAsNewWithVersion("{}", 2)

	// ---- state ----
	h.SetState("k", "v")
	_ = h.GetState("k", &out)
	_ = h.HasState("k")
	_ = h.IncrState("k", 1)
	_ = h.ListState("pre")
	h.DeleteState("k")
	h.SetQueryState("k", "v")

	// ---- scope ----
	_ = h.SetScope("obj", "key")
	_, _ = h.GetScope()
	_ = h.ClearScope()

	// ---- updates ----
	_, _ = h.HandleUpdate("upd", "{}")

	// ---- determinism sources ----
	_ = h.Now()
	_ = h.NowMs()
	_ = h.Random()
	_ = h.UUID("seed")
	_ = h.NewUUID()
	_ = h.NewUUIDv7()
	_ = h.WorkflowID()
	_ = h.RunID()
	_ = h.Version()
	_ = h.MinVersion()

	// ---- heartbeat, defer closure, update handler ----
	_, _ = h.DurableCallWithHeartbeat("svc", "op", "{}", time.Second, func(progressJSON string) {})
	_, _ = h.DurableDeferFunc(func() {})
	h.RegisterUpdateHandler("upd",
		func(payloadJSON string) (string, error) { return payloadJSON, nil },
		func(payloadJSON string) error { return nil })

	// ---- logging ----
	h.Log("m")
	h.LogKV("m", "k", "v")
	h.DurableLog("m")

	return input, nil
}
