using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// The wait points that serve pending workflow updates before they wait (cleat#4129; python-sdk's await_signals_ms, await_promise_ms
/// and await_child each call dispatch_updates first). tests/csharp/wait_dispatch_test.go drives these through the real engine. Entry
/// names are prefixed <c>dispatch_</c>.
/// </summary>
public static class GroupDispatch
{
    private static string S(JsonElement input, string name) => input.GetProperty(name).GetString() ?? "";

    /// <summary>
    /// Registers an <c>echo</c> update handler, enters the wait named by <c>wait</c> (<c>signals</c>, <c>promise</c> or <c>child</c>) with
    /// nothing that could satisfy it, then makes one service call. The Go test queues an update first: it is served only if the wait is
    /// a dispatch point, and the run then suspends AT the wait, so <c>svc/after_wait</c> is never made.
    /// </summary>
    [CleatEntry("dispatch_at_wait")]
    public static JsonNode AtWait(HostCalls h, JsonElement input)
    {
        h.RegisterUpdateHandler("echo", p => "echo:" + p);
        switch (S(input, "wait"))
        {
            case "signals": h.AwaitSignalsMs(new[] { "never" }, 60_000); break;
            case "promise": h.AwaitPromiseMs("never", 60_000); break;
            case "child": h.AwaitChild("never"); break;
            default: throw new System.ArgumentException("unknown wait: " + S(input, "wait"));
        }
        h.Call("svc", "after_wait", "{}");
        return new JsonObject { ["ok"] = true };
    }
}
