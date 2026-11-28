using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group B (cleat#4080, PR 5): signals and promises. Each entry runs ONE SDK call and reports what the SDK handed back, so a Go test
/// can drive the engine into each ending and read the answer. python-sdk's await_outcome fixture is the model.
/// </summary>
/// <remarks>
/// An entry catches <see cref="CleatCallException"/> and reports it as <c>raised</c>, and NEVER catches
/// <see cref="CleatSuspendException"/>: a stop must reach the SDK's entry wrapper, which is what turns it into a suspended run.
/// Where the shape matters, an entry makes one service call after the SDK call, so a test can tell "the guest ran on" from "the guest
/// stopped at the wait".
/// </remarks>
public static class GroupB
{
    private static string S(JsonElement input, string name) => input.GetProperty(name).GetString() ?? "";

    private static JsonObject Raised(CleatCallException e)
        => new() { ["raised"] = e.GetType().Name, ["message"] = e.Message, ["code"] = e.Code };

    [CleatEntry("b_await_promise")]
    public static JsonNode AwaitPromise(HostCalls h, JsonElement input)
    {
        PromiseResult r;
        try { r = h.AwaitPromiseMs(S(input, "promise_id"), input.GetProperty("timeout_ms").GetUInt64()); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_await", "{}");
        return new JsonObject { ["result"] = r.Result, ["timed_out"] = r.TimedOut, ["rejected"] = r.Rejected };
    }

    [CleatEntry("b_create_promise")]
    public static JsonNode CreatePromise(HostCalls h, JsonElement input)
    {
        try { return new JsonObject { ["id"] = h.CreatePromise(S(input, "name"), input.GetProperty("ttl_ms").GetUInt64()) }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    [CleatEntry("b_resolve_promise")]
    public static JsonNode ResolvePromise(HostCalls h, JsonElement input)
    {
        try { h.ResolvePromise(S(input, "id"), S(input, "value")); return new JsonObject { ["ok"] = true }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    [CleatEntry("b_reject_promise")]
    public static JsonNode RejectPromise(HostCalls h, JsonElement input)
    {
        try { h.RejectPromise(S(input, "id"), S(input, "error")); return new JsonObject { ["ok"] = true }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    [CleatEntry("b_await_signals")]
    public static JsonNode AwaitSignals(HostCalls h, JsonElement input)
    {
        var names = new List<string>();
        foreach (var n in input.GetProperty("names").EnumerateArray()) names.Add(n.GetString() ?? "");
        SignalResult r;
        try { r = h.AwaitSignalsMs(names, input.GetProperty("timeout_ms").GetUInt64()); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_await", "{}");
        return new JsonObject { ["name"] = r.Name, ["payload"] = r.Payload, ["timed_out"] = r.TimedOut, ["reply_to"] = r.ReplyTo };
    }

    [CleatEntry("b_poll_signal")]
    public static JsonNode PollSignal(HostCalls h, JsonElement input)
    {
        var (payload, found) = h.PollSignal(S(input, "name"));
        return new JsonObject { ["payload"] = payload, ["found"] = found };
    }

    [CleatEntry("b_signal_workflow")]
    public static JsonNode SignalWorkflow(HostCalls h, JsonElement input)
    {
        try { h.SignalWorkflow(S(input, "target"), S(input, "name"), S(input, "payload")); return new JsonObject { ["ok"] = true }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    /// <summary>
    /// A defer, a promise, a charge, a SETTLEMENT, then a call after it. tests/csharp/settle_stop_test.go cuts the history after the
    /// settlement and replays it as a defer segment, where the settlement is the run's last stoppable event: the host answers it with
    /// the stop (bit 31 of the packed result) instead of settling again. <c>how</c> is <c>resolve</c> or <c>reject</c>.
    /// The defer body reports what the guest had done by naming it in the OPERATION of its call (<c>saw-charged</c>), as python-sdk's
    /// killed_with_defers entries do: the call after the settlement is refused in a segment either way, so only a record of the guest's
    /// own steps can tell "the settlement stopped the guest" from "the next call did".
    /// </summary>
    [CleatEntry("b_settle_then_call")]
    public static JsonNode SettleThenCall(HostCalls h, JsonElement input)
    {
        var effects = new List<string>();
        h.Defer("cleanup", () => { h.Call("svc", "saw-" + string.Join("-", effects), "{}"); });
        var id = h.CreatePromise("order");
        h.Call("svc", "charge", "{}");
        effects.Add("charged");
        if (S(input, "how") == "reject") h.RejectPromise(id, "declined");
        else h.ResolvePromise(id, "\"done\"");
        effects.Add("settled");  // unreachable in a segment that stops at the settlement
        h.Call("svc", "after", "{}");
        return new JsonObject { ["ok"] = true };
    }
}
