using System;
using CleatWorkflowWorld;
using Cleat.Runtime;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace Cleat;

/// <summary>How a wait for a promise ended (python-sdk <c>PromiseResult</c>).</summary>
/// <param name="Result">The resolved value, or the rejection reason; empty on a timeout.</param>
/// <param name="TimedOut">True if the timeout elapsed before anything settled the promise.</param>
/// <param name="Rejected">True if the promise was rejected; <paramref name="Result"/> is then the reason.</param>
public readonly record struct PromiseResult(string Result, bool TimedOut, bool Rejected);

// Group B (cleat#4080): durable promises. python-sdk/cleat_sdk/host_calls.py is the reference.
//
// AwaitPromiseMs is a dispatch point, as Python's await_promise_ms is: a pending workflow update is served before the wait. It was left
// out of group B because update handlers were group E, and no group PR may depend on another; cleat#4129 added it once all three were on
// develop.
public sealed partial class HostCalls
{
    /// <summary>Creates a durable promise and returns its id. A <paramref name="ttlMs"/> of zero means no expiry.</summary>
    public string CreatePromise(string name, ulong ttlMs = 0)
        => IDurablePromisesImports.DurableCreatePromise(name, ttlMs);

    /// <summary>
    /// Waits for a promise to settle, or for <paramref name="timeout"/>. A timeout of zero or less does NOT wait: an
    /// already-settled promise is returned, and otherwise the result is a timeout at once (cleat#4144). To wait without a limit
    /// pass <see cref="System.Threading.Timeout.InfiniteTimeSpan"/>. A fresh wait that has to wait throws
    /// <see cref="CleatSuspendException"/>; on replay the wait has already ended and it returns the outcome.
    /// </summary>
    public PromiseResult AwaitPromise(string id, TimeSpan timeout)
        => AwaitPromiseMs(id, WaitDuration.Ms(timeout));

    /// <summary><see cref="AwaitPromise(string, TimeSpan)"/> in milliseconds. Zero does not wait; pass <see cref="WaitForeverMs"/> to wait without a limit.</summary>
    public PromiseResult AwaitPromiseMs(string id, ulong timeoutMs)
    {
        DispatchUpdates(); // a dispatch point: python's await_promise_ms serves pending updates before it waits
        IDurablePromisesImports.AwaitOutcome outcome;
        try
        {
            outcome = IDurablePromisesImports.DurableAwaitPromise(id, timeoutMs);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            // A stop unwinds and a host failure raises. Neither is an outcome of the promise (cleat#3425).
            throw Translate("promise", "await_promise", f);
        }
        if (outcome.Tag == IDurablePromisesImports.AwaitOutcome.Tags.Resolved)
            return new PromiseResult(outcome.AsResolved, false, false);
        if (outcome.Tag == IDurablePromisesImports.AwaitOutcome.Tags.Rejected)
            return new PromiseResult(outcome.AsRejected, false, true);
        return new PromiseResult("", true, false);
    }

    /// <summary>Settles a promise with a value.</summary>
    public void ResolvePromise(string id, string value)
        => Packed.CheckHostResult(IDurablePromisesImports.DurableResolvePromise(id, value),
            "promise", "resolve_promise", $"resolve_promise(promise_id='{id}')");

    /// <summary>Settles a promise with an error; a waiter sees <see cref="PromiseResult.Rejected"/>.</summary>
    public void RejectPromise(string id, string error)
        => Packed.CheckHostResult(IDurablePromisesImports.DurableRejectPromise(id, error),
            "promise", "reject_promise", $"reject_promise(promise_id='{id}')");
}
