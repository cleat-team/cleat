using System;
using System.Collections.Generic;
using System.Text.Json;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

/// <summary>One recurring schedule as <see cref="HostCalls.ListCrons"/> reports it.</summary>
/// <param name="ScheduleId">The id <see cref="HostCalls.ScheduleCron"/> returned.</param>
/// <param name="WorkflowName">The definition the schedule starts.</param>
/// <param name="CronExpr">The five-field cron expression.</param>
/// <param name="Timezone">The IANA zone it is evaluated in (<c>UTC</c> when none was given).</param>
/// <param name="Input">The JSON each run is started with.</param>
/// <param name="Enabled"><c>false</c> for a schedule that was retired.</param>
public sealed record CronSchedule(string ScheduleId, string WorkflowName, string CronExpr, string Timezone, string Input, bool Enabled);

// Group F of cleat#4080's host-call coverage: fire-and-forget requests and recurring schedules.
// durable-send, durable-schedule-invoke, durable-schedule-cron, durable-delete-cron, durable-list-crons.
public sealed partial class HostCalls
{
    // ---- fire-and-forget --------------------------------------------------------------------------------------

    /// <summary>
    /// Sends a request to a service and does not wait for the answer: the request is recorded, handed to the service by the host in the
    /// background, and the workflow carries on. Nothing about the outcome comes back, so use <see cref="Call(string, string, string)"/> when
    /// the answer matters or the request must not be lost. The record is what a replay reads; a replay does NOT send again.
    /// </summary>
    /// <exception cref="CleatCallException">The host refused the send (the base type, <see cref="CleatCallException.Code"/> 0; in practice
    /// a replay divergence: this call is not the one the history recorded at this step).</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow before it started new work.</exception>
    public void Send(string service, string operation, string request)
    {
        ArgumentNullException.ThrowIfNull(service);
        ArgumentNullException.ThrowIfNull(operation);
        ArgumentNullException.ThrowIfNull(request);
        Packed.CheckHostResult(IDurableMessagingImports.DurableSend(service, operation, request), service, operation, "send");
    }

    /// <summary>
    /// Like <see cref="Send"/>, delivered after <paramref name="delay"/>. The wait is the HOST's, in the background, and holds no worker
    /// slot: this method returns at once. A delay under one millisecond is rounded up to one; zero sends at once.
    /// </summary>
    /// <exception cref="ArgumentOutOfRangeException"><paramref name="delay"/> is negative, or longer than the host can count (about 292
    /// years: past that the host's nanosecond arithmetic wraps and the request would be delivered at once, not never).</exception>
    public void ScheduleInvoke(string service, string operation, string request, TimeSpan delay)
    {
        ArgumentNullException.ThrowIfNull(service);
        ArgumentNullException.ThrowIfNull(operation);
        ArgumentNullException.ThrowIfNull(request);
        var ms = Math.Ceiling(delay.TotalMilliseconds);
        if (!(ms >= 0) || ms > MaxScheduleDelayMs)
            throw new ArgumentOutOfRangeException(nameof(delay), delay, "the delay must be between zero and about 292 years");
        Packed.CheckHostResult(IDurableMessagingImports.DurableScheduleInvoke(service, operation, request, (ulong)ms), service, operation, "schedule-invoke");
    }

    // time.Duration is int64 nanoseconds in the host: 9.22e18 ns / 1e6 = 9.22e12 ms before the product wraps negative.
    private const double MaxScheduleDelayMs = long.MaxValue / 1_000_000L;

    // ---- cron -------------------------------------------------------------------------------------------------

    /// <summary>
    /// Creates a recurring trigger that starts <paramref name="workflowName"/> on <paramref name="cronExpr"/>, and returns the schedule's id.
    /// The id is derived from this workflow and this call's position, so a replay or a retry of the same call returns the same id and does
    /// not create a second schedule.
    /// </summary>
    /// <param name="workflowName">The definition to start. Must not be empty.</param>
    /// <param name="cronExpr">Five fields (<c>0 0 * * *</c> is midnight daily).</param>
    /// <param name="timezone">An IANA zone name, or <c>""</c> for UTC.</param>
    /// <param name="inputJson">The JSON input of each run; <c>""</c> means <c>{}</c>.</param>
    /// <exception cref="CleatCallException">The host refused: an empty name, a bad expression or zone, input that is not JSON, the schedule
    /// quota, or no workflow store. The message is the host's.</exception>
    public string ScheduleCron(string workflowName, string cronExpr, string timezone, string inputJson)
    {
        ArgumentNullException.ThrowIfNull(workflowName);
        ArgumentNullException.ThrowIfNull(cronExpr);
        ArgumentNullException.ThrowIfNull(timezone);
        ArgumentNullException.ThrowIfNull(inputJson);
        try
        {
            return IDurableCronImports.DurableScheduleCron(workflowName, cronExpr, timezone, inputJson);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("cron", "schedule_cron", f);
        }
    }

    /// <summary>
    /// Removes a recurring trigger. Deleting one that is not there succeeds (a retry of a delete must not fail the workflow).
    /// <b>A delete the host refuses or fails cannot be reported</b>: the WIT function has no return value at all, so this method returns
    /// normally either way (cleat#4083 records the same for the other SDKs). Check with <see cref="ListCrons"/> when it matters.
    /// </summary>
    public void DeleteCron(string scheduleId)
    {
        ArgumentNullException.ThrowIfNull(scheduleId);
        IDurableCronImports.DurableDeleteCron(scheduleId);
    }

    /// <summary>
    /// The tenant's recurring schedules, ordered by id. The list is recorded like any other read of outside state, so a replay returns
    /// the list the first run saw, not the schedules that exist now.
    /// </summary>
    /// <exception cref="CleatCallException">The host could not list them. The WIT function returns a string with no failure channel, and the
    /// host puts the error text where the JSON would be; text that is not a JSON array is raised here as the host's message instead of being
    /// parsed as an empty list.</exception>
    public IReadOnlyList<CronSchedule> ListCrons()
    {
        var text = IDurableCronImports.DurableListCrons();
        if (text.Length == 0)
            return Array.Empty<CronSchedule>();
        try
        {
            using var doc = JsonDocument.Parse(text);
            if (doc.RootElement.ValueKind != JsonValueKind.Array)
                throw new JsonException("not an array");
            var list = new List<CronSchedule>();
            foreach (var e in doc.RootElement.EnumerateArray())
                list.Add(new CronSchedule(
                    e.GetProperty("schedule_id").GetString() ?? "",
                    e.GetProperty("workflow_name").GetString() ?? "",
                    e.GetProperty("cron_expr").GetString() ?? "",
                    e.GetProperty("timezone").GetString() ?? "",
                    e.GetProperty("input").GetString() ?? "",
                    e.GetProperty("enabled").GetBoolean()));
            return list;
        }
        catch (Exception ex) when (ex is JsonException or KeyNotFoundException or InvalidOperationException)
        {
            throw new CleatCallException("cron", "list_crons", text, 0);
        }
    }
}
