using System.Collections.Generic;
using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group C (cleat#4080, PR 5): child workflows. Each entry runs ONE SDK call and reports what the SDK handed back. As in group B, an entry
/// catches <see cref="CleatCallException"/> and NEVER <see cref="CleatSuspendException"/>, and the awaits make one service call afterwards
/// so a test can tell "the guest ran on" from "the guest stopped at the wait".
/// </summary>
public static class GroupC
{
    private static string S(JsonElement input, string name) => input.GetProperty(name).GetString() ?? "";

    private static JsonObject Raised(CleatCallException e)
        => new() { ["raised"] = e.GetType().Name, ["message"] = e.Message, ["code"] = e.Code };

    private static List<string> Ids(JsonElement input)
    {
        var ids = new List<string>();
        foreach (var n in input.GetProperty("run_ids").EnumerateArray()) ids.Add(n.GetString() ?? "");
        return ids;
    }

    private static JsonObject Doc(ChildResult r)
        => new() { ["run_id"] = r.RunId, ["result"] = r.Result, ["error"] = r.Error };

    [CleatEntry("c_child_workflow")]
    public static JsonNode Start(HostCalls h, JsonElement input)
    {
        try { return new JsonObject { ["run_id"] = h.ChildWorkflow(S(input, "name"), S(input, "input")) }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    [CleatEntry("c_child_with_options")]
    public static JsonNode StartWithOptions(HostCalls h, JsonElement input)
    {
        var policy = S(input, "policy") switch
        {
            "abandon" => ParentClosePolicy.Abandon,
            "terminate" => ParentClosePolicy.Terminate,
            "request_cancel" => ParentClosePolicy.RequestCancel,
            _ => ParentClosePolicy.Default,
        };
        var options = new ChildWorkflowOptions(input.GetProperty("version").GetUInt64(), input.GetProperty("priority").GetUInt64(), policy);
        try { return new JsonObject { ["run_id"] = h.ChildWorkflowWithOptions(S(input, "name"), S(input, "input"), options) }; }
        catch (CleatCallException e) { return Raised(e); }
    }

    [CleatEntry("c_await_child")]
    public static JsonNode AwaitChild(HostCalls h, JsonElement input)
    {
        string answer;
        try { answer = h.AwaitChild(S(input, "run_id")); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_await", "{}");
        return new JsonObject { ["answer"] = answer };
    }

    [CleatEntry("c_await_all_children")]
    public static JsonNode AwaitAll(HostCalls h, JsonElement input)
    {
        IReadOnlyList<ChildResult> all;
        try { all = h.AwaitAllChildren(Ids(input)); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_await", "{}");
        var arr = new JsonArray();
        foreach (var r in all) arr.Add(Doc(r));
        return new JsonObject { ["results"] = arr };
    }

    [CleatEntry("c_await_any_child")]
    public static JsonNode AwaitAny(HostCalls h, JsonElement input)
    {
        ChildResult r;
        try { r = h.AwaitAnyChild(Ids(input)); }
        catch (CleatCallException e) { return Raised(e); }
        h.Call("svc", "after_await", "{}");
        return Doc(r);
    }

    [CleatEntry("c_poll_child")]
    public static JsonNode Poll(HostCalls h, JsonElement input)
    {
        ChildResult? r;
        try { r = h.PollChild(S(input, "run_id")); }
        catch (CleatCallException e) { return Raised(e); }
        return r is { } c
            ? new JsonObject { ["found"] = true, ["run_id"] = c.RunId, ["result"] = c.Result, ["error"] = c.Error }
            : new JsonObject { ["found"] = false };
    }
}
