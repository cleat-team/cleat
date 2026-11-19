using System;
using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group E (cleat#4080, PR 5): update handlers and queryable state. python-sdk's dispatch_updates is the model. Updates are SERVED
/// at a program position the guest chooses, so an entry registers its handlers, serves, and then makes one service call so a Go test
/// can tell "the guest ran on" from "the guest stopped" (a handler that sleeps stops the whole run).
/// </summary>
/// <remarks>
/// As in the other groups, an entry catches <see cref="CleatCallException"/> and NEVER <see cref="CleatSuspendException"/>.
/// </remarks>
public static class GroupE
{
    private static string S(JsonElement input, string name) => input.GetProperty(name).GetString() ?? "";

    private static JsonObject Raised(CleatCallException e)
        => new() { ["raised"] = e.GetType().Name, ["message"] = e.Message, ["code"] = e.Code };

    /// <summary>The handlers every dispatch entry registers, one per ending the dispatcher has.</summary>
    private static void RegisterAll(HostCalls h)
    {
        h.RegisterUpdateHandler("echo", p => "echo:" + p);
        h.RegisterUpdateHandler("empty", p => "");
        h.RegisterUpdateHandler("strict", p => p, p => p != "bad");
        h.RegisterUpdateHandler("boom", p => throw new InvalidOperationException("boom!"));
        // A handler that waits: its sleep suspends the run, which must NOT complete the update.
        h.RegisterUpdateHandler("naps", p => { h.Sleep(TimeSpan.FromHours(1)); return "woke"; });
        // A handler that calls the service: its call lands between the update's received and completed events.
        h.RegisterUpdateHandler("calls", p => { h.Call("svc", "from_handler", "{}"); return "called"; });
    }

    [CleatEntry("e_dispatch")]
    public static JsonNode Dispatch(HostCalls h, JsonElement input)
    {
        try { RegisterAll(h); h.DispatchUpdates(); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_dispatch", "{}");
        return new JsonObject { ["ok"] = true };
    }

    [CleatEntry("e_sleep_serves_updates")]
    public static JsonNode SleepServesUpdates(HostCalls h, JsonElement input)
    {
        RegisterAll(h);
        h.SleepMs(0); // sleep is a dispatch point: it serves pending updates before it waits
        h.Call("svc", "after_sleep", "{}");
        return new JsonObject { ["ok"] = true };
    }

    [CleatEntry("e_set_query_state")]
    public static JsonNode SetQueryState(HostCalls h, JsonElement input)
    {
        try { h.SetQueryState(S(input, "key"), S(input, "value")); }
        catch (CleatCallException e) { return Raised(e); }
        return new JsonObject { ["ok"] = true };
    }

    [CleatEntry("e_poll_update")]
    public static JsonNode PollUpdate(HostCalls h, JsonElement input)
        => new JsonObject { ["envelope"] = h.PollUpdate() };

    [CleatEntry("e_complete_update")]
    public static JsonNode CompleteUpdate(HostCalls h, JsonElement input)
    {
        try { h.CompleteUpdate(S(input, "request_id"), S(input, "result"), S(input, "error")); }
        catch (CleatCallException e) { return Raised(e); }
        return new JsonObject { ["ok"] = true };
    }
}
