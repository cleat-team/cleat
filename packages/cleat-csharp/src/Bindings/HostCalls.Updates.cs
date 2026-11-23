using System;
using CleatWorkflowWorld;
using Cleat.Runtime;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace Cleat;

// Group E (cleat#4080): workflow updates and queryable state. python-sdk/cleat_sdk/host_calls.py is the reference.
//
// NOT HERE, ON PURPOSE: durable-register-query-handler. Every SDK had a register_query_handler until 2026-08-09; it recorded a
// handler name with the host and no worker code ever routed an external query back to it. docs/determinism.md, "Why there is no
// RegisterQueryHandler", and docs/contributor/SDK_README_TEMPLATE.md say a new SDK should not have one: use SetQueryState.
// The function stays listed in tests/csharp/unbound.d/e.txt with that reason.
public sealed partial class HostCalls
{
    private readonly UpdateHandlers _updates = new();

    /// <summary>
    /// Sets a key in the workflow's queryable state, readable by any caller while the workflow runs and after it completes
    /// (<c>GET /api/workflows/:id/query?key=X</c>).
    /// </summary>
    public void SetQueryState(string key, string value)
        => Packed.CheckHostResult(IDurableStateImports.SetQueryState(key, value), "state", "set_query_state", $"set_query_state(key='{key}')");

    /// <summary>
    /// Registers a handler for update calls on this workflow. The handler takes the caller's payload (usually JSON) and returns the
    /// result; an exception it throws rejects the update with the exception's message. The optional <paramref name="validator"/>
    /// runs first and returns false to refuse the payload.
    /// </summary>
    /// <remarks>
    /// Registration is local first and the host is told second, as Python does it, so a replay that re-runs registration rebuilds
    /// the same table whatever the host answers. An update is served at the next dispatch point, not the instant it arrives: see
    /// <see cref="DispatchUpdates"/>.
    /// </remarks>
    public void RegisterUpdateHandler(string name, Func<string, string> handler, Func<string, bool>? validator = null)
    {
        _updates.Register(name, handler, validator);
        Packed.CheckHostResult(
            IDurableHandlersImports.DurableRegisterUpdateHandler(name), "handlers", "register_update_handler", $"register_update_handler(name='{name}')");
    }

    /// <summary>
    /// The next pending update as the host's JSON envelope (<c>{"name", "payload", "request_id"}</c>), or the empty string when
    /// there is none. Low level: prefer <see cref="DispatchUpdates"/>. Delivery is durable, so an update returned here is recorded
    /// as delivered whether or not it is completed.
    /// </summary>
    public string PollUpdate() => IDurableHandlersImports.DurablePollUpdate();

    /// <summary>
    /// Records an update handler's outcome and settles the caller's promise. A non-empty <paramref name="error"/> rejects; an empty
    /// one resolves, and an empty <paramref name="result"/> with an empty error resolves (an empty result is an outcome, not a
    /// missing one). Low level: prefer <see cref="DispatchUpdates"/>, which cannot forget to call this. <paramref name="requestId"/>
    /// is the opaque value <see cref="PollUpdate"/> returned, passed back unchanged.
    /// </summary>
    public void CompleteUpdate(string requestId, string result, string error)
        => Packed.CheckHostResult(
            IDurableHandlersImports.DurableCompleteUpdate(requestId, result, error), "handlers", "complete_update", $"complete_update(request_id='{requestId}')");

    /// <summary>
    /// Serves every update currently pending for this workflow. The SDK already calls this before each suspension point that has
    /// one wired, so an ordinary workflow needs no update-specific code; it is public for a workflow that wants to serve updates at
    /// more points.
    /// </summary>
    /// <remarks>
    /// An update handler is code held in the guest, so only the guest can run it: an arriving update cannot interrupt the workflow,
    /// and it is asked for at a fixed program position because replay matches host calls against history in order. The consequence,
    /// stated rather than hidden: an update is handled at the next dispatch point, not the instant it arrives. A handler may sleep or
    /// await; a nested dispatch from inside one returns at once.
    /// </remarks>
    public void DispatchUpdates() => _updates.Dispatch(PollUpdate, CompleteUpdate);
}
