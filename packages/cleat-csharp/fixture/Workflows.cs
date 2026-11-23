using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Fixture;

/// <summary>
/// Entry points that exercise the SDK's boundary against the real engine (cleat#4080). Each one is named for the
/// path it covers; the engine test in engine/ drives them.
/// </summary>
public static class Workflows
{
    /// <summary>A simple-result host call, a durable call, and the input echoed back: JsonElement in, JsonNode out.</summary>
    [CleatEntry("Hello")]
    public static JsonNode Hello(HostCalls h, JsonElement input)
    {
        var id = h.WorkflowId();
        var reply = h.Call("payments", "Charge", input.GetRawText());
        return new JsonObject
        {
            ["id"] = id,
            ["reply"] = JsonNode.Parse(reply),
            ["input"] = JsonNode.Parse(input.GetRawText()),
        };
    }

    /// <summary>
    /// Suspension by exception, replay from history, and an <c>async</c> entry awaiting work over host calls.
    /// The first segment suspends in <c>Sleep</c>; the second completes from history.
    /// </summary>
    [CleatEntry("Sleep")]
    public static async Task<JsonNode> SleepThenCall(HostCalls h)
    {
        var first = await Charge(h, "before");
        await SleepDeep(h, 20);
        var second = await Charge(h, "after");
        return new JsonObject { ["first"] = JsonNode.Parse(first), ["second"] = JsonNode.Parse(second) };
    }

    private static async Task<string> Charge(HostCalls h, string tag)
    {
        await Task.CompletedTask;
        return h.Call("payments", "Charge", "{\"tag\":\"" + tag + "\"}");
    }

    private static async Task SleepDeep(HostCalls h, int frames)
    {
        await Task.CompletedTask;
        if (frames == 0) h.Sleep(TimeSpan.FromMilliseconds(400));
        else await SleepDeep(h, frames - 1);
    }

    /// <summary>A defer registered before a suspension: the host drains it on the same instance (run-deferred).</summary>
    [CleatEntry("Defers")]
    public static void Defers(HostCalls h)
    {
        h.Defer("refund", () => h.Call("payments", "Refund", "{\"x\":1}"));
        h.Sleep(TimeSpan.FromMilliseconds(400));
    }

    /// <summary>A body that throws: the guest drains its own defers, then reports a failure.</summary>
    [CleatEntry("Fails")]
    public static void Fails(HostCalls h)
    {
        h.Defer("cleanup", () => h.Call("payments", "Refund", "{\"x\":2}"));
        throw new InvalidOperationException("boom");
    }

    /// <summary>The host's replayed clock and entropy must be stable across replays.</summary>
    [CleatEntry("Clock")]
    public static JsonNode Clock(HostCalls h)
    {
        h.Call("payments", "Charge", "{}");   // anchors the clock to history
        return new JsonObject { ["now"] = h.Now().ToUnixTimeMilliseconds(), ["rand"] = h.Random().ToString() };
    }
}
