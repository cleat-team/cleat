using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group F of cleat#4080's host-call coverage (tests/csharp/group_f_test.go): fire-and-forget requests and recurring schedules.
/// Entry names are prefixed <c>f_</c>. An entry that can fail by design reports the failure as data; a
/// <see cref="CleatSuspendException"/> is never caught, because it is how a run ends.
/// </summary>
public static class GroupF
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["code"] = (int)e.Code,
        ["service"] = e.Service,
        ["operation"] = e.Operation,
        ["message"] = e.Message,
    };

    /// <summary>Input: service, operation, request.</summary>
    [CleatEntry("f_send")]
    public static JsonNode Send(HostCalls h, JsonElement input)
    {
        h.Send(input.GetProperty("service").GetString()!, input.GetProperty("operation").GetString()!, input.GetProperty("request").GetRawText());
        return new JsonObject { ["sent"] = true };
    }

    /// <summary>Input: service, operation, request, delayMs (a number, may be fractional).</summary>
    [CleatEntry("f_schedule_invoke")]
    public static JsonNode ScheduleInvoke(HostCalls h, JsonElement input)
    {
        try
        {
            h.ScheduleInvoke(input.GetProperty("service").GetString()!, input.GetProperty("operation").GetString()!,
                input.GetProperty("request").GetRawText(), TimeSpan.FromMilliseconds(input.GetProperty("delayMs").GetDouble()));
            return new JsonObject { ["scheduled"] = true };
        }
        catch (ArgumentOutOfRangeException e) { return new JsonObject { ["invalid"] = e.ParamName }; }
    }

    private static JsonNode Schedules(HostCalls h)
    {
        var a = new JsonArray();
        foreach (var s in h.ListCrons())
            a.Add(new JsonObject
            {
                ["id"] = s.ScheduleId, ["workflow"] = s.WorkflowName, ["expr"] = s.CronExpr,
                ["tz"] = s.Timezone, ["input"] = s.Input, ["enabled"] = s.Enabled,
            });
        return a;
    }

    /// <summary>Input: workflow, expr, tz, input (string). Creates one schedule and reports its id, or the failure.</summary>
    [CleatEntry("f_cron_create")]
    public static JsonNode CronCreate(HostCalls h, JsonElement input)
    {
        try
        {
            var id = h.ScheduleCron(input.GetProperty("workflow").GetString()!, input.GetProperty("expr").GetString()!,
                input.GetProperty("tz").GetString()!, input.GetProperty("input").GetString()!);
            return new JsonObject { ["id"] = id };
        }
        catch (CleatCallException e) { return Failure(e); }
    }

    /// <summary>Input: workflow, expr, tz, input. Lists, creates, lists, deletes it, lists, deletes it AGAIN, lists.</summary>
    [CleatEntry("f_cron_cycle")]
    public static JsonNode CronCycle(HostCalls h, JsonElement input)
    {
        var o = new JsonObject { ["before"] = Schedules(h) };
        var id = h.ScheduleCron(input.GetProperty("workflow").GetString()!, input.GetProperty("expr").GetString()!,
            input.GetProperty("tz").GetString()!, input.GetProperty("input").GetString()!);
        o["id"] = id;
        o["afterCreate"] = Schedules(h);
        h.DeleteCron(id);
        o["afterDelete"] = Schedules(h);
        h.DeleteCron(id);
        o["afterSecondDelete"] = Schedules(h);
        return o;
    }

    [CleatEntry("f_list_crons")]
    public static JsonNode ListCrons(HostCalls h, JsonElement input)
    {
        try { return new JsonObject { ["schedules"] = Schedules(h) }; }
        catch (CleatCallException e) { return Failure(e); }
    }
}
