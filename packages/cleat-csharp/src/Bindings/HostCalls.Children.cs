using System;
using System.Collections.Generic;
using CleatWorkflowWorld;
using Cleat.Runtime;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace Cleat;

/// <summary>What happens to a running child when its parent closes (cleat#936).</summary>
/// <remarks>
/// An enum, not a string, on purpose. The engine matches the policy EXACTLY and in upper case, so a mis-cased value is refused at the
/// write boundary (engine/parent_close_policy.go); the Java SDK documents lower-case spellings that the engine would reject. Mapping
/// here means a caller cannot spell it wrong.
/// </remarks>
public enum ParentClosePolicy
{
    /// <summary>The engine's default, which is <c>ABANDON</c>.</summary>
    Default = 0,
    /// <summary>The child keeps running on its own.</summary>
    Abandon,
    /// <summary>The child is terminated when the parent closes.</summary>
    Terminate,
    /// <summary>The child is asked to cancel when the parent closes.</summary>
    RequestCancel,
}

/// <summary>Options for <see cref="HostCalls.ChildWorkflowWithOptions"/> (python-sdk <c>ChildWorkflowOptions</c>, plus the close policy).</summary>
/// <param name="Version">An explicit workflow definition version; 0 resolves the default (the parent's).</param>
/// <param name="Priority">Scheduling priority; 0 is highest, and a lower number is picked first.</param>
/// <param name="ParentClosePolicy">What happens to the child when the parent closes.</param>
public sealed record ChildWorkflowOptions(ulong Version = 0, ulong Priority = 0, ParentClosePolicy ParentClosePolicy = ParentClosePolicy.Default);

// Group C (cleat#4080): child workflows. python-sdk/cleat_sdk/host_calls.py is the reference.
//
// AwaitChild is a dispatch point, as Python's await_child is: a pending workflow update is delivered before the wait. AwaitAllChildren and
// AwaitAnyChild are not, in Python either. It was left out of group C because update handlers were group E, and no group PR may depend on
// another; cleat#4129 added it once all three were on develop.
public sealed partial class HostCalls
{
    private static string Spelling(ParentClosePolicy p) => p switch
    {
        ParentClosePolicy.Default => "",
        ParentClosePolicy.Abandon => "ABANDON",
        ParentClosePolicy.Terminate => "TERMINATE",
        ParentClosePolicy.RequestCancel => "REQUEST_CANCEL",
        _ => throw new ArgumentOutOfRangeException(nameof(p), p, "not a ParentClosePolicy"),
    };

    /// <summary>Starts a child workflow and returns its run id. <paramref name="inputJson"/> is the raw JSON the child will read.</summary>
    public string ChildWorkflow(string name, string inputJson)
    {
        try { return IDurableChildrenImports.DurableChildWorkflow(name, inputJson); }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f) { throw Translate("child-workflow", name, f); }
    }

    /// <summary>Starts a child workflow with a version, a priority and a parent-close policy.</summary>
    public string ChildWorkflowWithOptions(string name, string inputJson, ChildWorkflowOptions? options = null)
    {
        // Built per call, not as a default argument value: nothing shared between calls to mutate.
        options ??= new ChildWorkflowOptions();
        try
        {
            return IDurableChildrenImports.DurableChildWorkflowWithOptions(
                name, inputJson, options.Version, options.Priority, Spelling(options.ParentClosePolicy));
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f) { throw Translate("child-workflow", name, f); }
    }

    /// <summary>
    /// Waits for a child to finish and returns the host's answer as text. A pending wait stops the guest and does not return here.
    /// The answer is empty ONLY inside a defer segment or the guest's own defer drain, where a stop would lose the cleanup.
    /// </summary>
    public string AwaitChild(string runId)
    {
        DispatchUpdates(); // a dispatch point: python's await_child serves pending updates before it waits
        return IDurableChildrenImports.DurableAwaitChild(runId);
    }

    /// <summary>Waits for every child. An empty answer (a defer body with children still pending) is an empty list.</summary>
    public IReadOnlyList<ChildResult> AwaitAllChildren(IReadOnlyList<string> runIds)
        => ChildDocuments.ParseAll(IDurableChildrenImports.DurableAwaitAllChildren(EntryJson.StringArray(runIds)));

    /// <summary>
    /// Waits for the first child to finish. An empty answer, which only a defer body can see, throws: the host writes the
    /// winner here, so nothing was awaited rather than nobody won.
    /// </summary>
    public ChildResult AwaitAnyChild(IReadOnlyList<string> runIds)
        => ChildDocuments.ParseAny(IDurableChildrenImports.DurableAwaitAnyChild(EntryJson.StringArray(runIds)));

    /// <summary>Checks a child without waiting: <c>null</c> while it is still running, otherwise its outcome.</summary>
    public ChildResult? PollChild(string runId)
        => ChildDocuments.ParsePoll(runId, IDurableChildrenImports.DurablePollChild(runId));
}
