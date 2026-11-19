using System;
using System.Collections.Generic;
using System.Text;
using System.Text.Json;
using CleatWorkflowWorld;
using Cleat.Runtime;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace Cleat;

/// <summary>How a wait for signals ended (python-sdk <c>SignalResult</c>).</summary>
/// <param name="Name">The signal that arrived; empty on a timeout.</param>
/// <param name="Payload">Its payload, with a request/reply envelope unwrapped; empty on a timeout.</param>
/// <param name="TimedOut">True if the timeout elapsed first.</param>
/// <param name="ReplyTo">The address a request/reply signal asked to be answered at; empty otherwise.</param>
public readonly record struct SignalResult(string Name, string Payload, bool TimedOut, string ReplyTo);

// Group B (cleat#4080): signals. python-sdk/cleat_sdk/host_calls.py is the reference.
public sealed partial class HostCalls
{
    /// <summary>
    /// Waits for one of the named signals, or for <paramref name="timeout"/>. A timeout of zero or less waits
    /// indefinitely, and the host suspends the workflow until a matching signal arrives. A fresh wait that has to
    /// wait throws <see cref="CleatSuspendException"/>.
    /// </summary>
    public SignalResult AwaitSignals(IReadOnlyList<string> names, TimeSpan timeout)
        => AwaitSignalsMs(names, timeout <= TimeSpan.Zero ? 0UL : (ulong)timeout.TotalMilliseconds);

    /// <summary><see cref="AwaitSignals(IReadOnlyList{string}, TimeSpan)"/> in milliseconds; zero waits indefinitely.</summary>
    public SignalResult AwaitSignalsMs(IReadOnlyList<string> names, ulong timeoutMs)
    {
        // The host reads i64::MAX as "no timeout" (INFINITE_TIMEOUT_MS in the Python SDK).
        var effective = timeoutMs == 0 ? (ulong)long.MaxValue : timeoutMs;
        IDurableSignalsImports.SignalOutcome outcome;
        try
        {
            outcome = IDurableSignalsImports.DurableAwaitSignals(EntryJson.StringArray(names), effective);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            // suspended is the engine stopping the workflow: a stop site, so a fresh await inside a defer segment
            // would otherwise leave a terminated workflow waiting for a signal instead of finishing its cleanup.
            throw Translate("signal", "await_signals", f);
        }
        if (outcome.Tag == IDurableSignalsImports.SignalOutcome.Tags.TimedOut)
            return new SignalResult("", "", true, "");
        var sig = outcome.AsReceived;
        // Strip the reply envelope, if this is a request/reply signal, so the receiver reads its payload exactly as the
        // sender passed it and gets the address separately.
        return SignalEnvelope.TryDecode(sig.payload, out var replyTo, out var unwrapped)
            ? new SignalResult(sig.name, unwrapped, false, replyTo)
            : new SignalResult(sig.name, sig.payload, false, "");
    }

    /// <summary>
    /// Checks once, without waiting, for a pending signal of this name. The host answers an EMPTY string for "none",
    /// so a signal sent with an empty payload reads as not found: that is the contract of
    /// <c>durable-poll-signal</c> and Python's <c>poll_signal</c>, not a limitation of this wrapper.
    /// </summary>
    public (string Payload, bool Found) PollSignal(string name)
    {
        var payload = IDurableSignalsImports.DurablePollSignal(name);
        return (payload, payload.Length != 0);
    }

    /// <summary>
    /// Sends a signal to another workflow, fire-and-forget; a recorded (journaled) call. The payload is the raw
    /// text the receiver will read, usually JSON.
    /// </summary>
    public void SignalWorkflow(string targetRunId, string signalName, string payload)
        => Packed.CheckHostResult(
            IDurableSignalsImports.DurableSignalWorkflow(targetRunId, signalName, payload),
            "signal", "signal_workflow", $"signal_workflow(target_run_id={Quote(targetRunId)}, signal_name={Quote(signalName)})");

    private static string Quote(string s) => "'" + s + "'";
}
