using System;
using System.Diagnostics.CodeAnalysis;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

// Group A of cleat#4080's host-call coverage: the call variants, the definition version, and ending a run to start the next one.
// durable-call-retry, durable-call-heartbeat, durable-min-version, durable-continue-as-new, continue-as-new-versioned.
public sealed partial class HostCalls
{
    // ---- durable-call: retry and heartbeat --------------------------------------------------------------------

    /// <summary>
    /// Makes a durable call that the HOST retries under <paramref name="policy"/>. One event is recorded however many attempts it took,
    /// so on replay the whole sequence is one answer. Returns the response JSON of the first attempt that succeeded.
    /// </summary>
    /// <exception cref="CleatCallException">The policy was exhausted (the last failure), a failure the policy marks non-retryable
    /// came back, or the host refused the policy (<see cref="CleatCallException.Code"/> is
    /// <see cref="RetryPolicy.RetryPolicyTooLongCode"/>; see <see cref="RetryPolicy"/>).</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow before the call was started.</exception>
    /// <exception cref="ArgumentOutOfRangeException">The policy is not one the host can read: see <see cref="RetryPolicy"/>.</exception>
    public string CallWithRetry(string service, string operation, string request, RetryPolicy policy)
    {
        ArgumentNullException.ThrowIfNull(policy);
        var w = policy.ToWire();
        try
        {
            return IDurableCallImports.DurableCallRetry(service, operation, request,
                w.MaxAttempts, w.InitialIntervalMs, w.BackoffCoefficient100x, w.MaxIntervalMs, w.NonRetryableErrorsJson);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate(service, operation, f);
        }
    }

    /// <summary>
    /// Makes a durable call and keeps the workflow's claim alive while it runs: the HOST heartbeats every
    /// <paramref name="heartbeatInterval"/>, so a call that outlives the ordinary lease is not reaped as a stale instance. The guest is
    /// inside this one host call for the whole duration, so there is no progress callback (the Python SDK had one until cleat#854, which
    /// could never be invoked).
    /// </summary>
    /// <exception cref="CleatCallException">The service call failed.</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow (including a defer segment) before or during the call.</exception>
    /// <exception cref="ArgumentOutOfRangeException"><paramref name="heartbeatInterval"/> is not positive. The host builds a ticker
    /// from it, and a ticker of zero is not constructible, so this is refused here rather than handed to it.</exception>
    public string CallWithHeartbeat(string service, string operation, string request, TimeSpan heartbeatInterval)
    {
        var ms = Math.Round(heartbeatInterval.TotalMilliseconds, MidpointRounding.ToEven);
        if (ms < 1 || ms >= long.MaxValue)
            throw new ArgumentOutOfRangeException(nameof(heartbeatInterval), heartbeatInterval,
                "the heartbeat interval must be at least one millisecond");
        try
        {
            return IDurableCallImports.DurableCallHeartbeat(service, operation, request, (ulong)ms);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate(service, operation, f);
        }
    }

    // ---- durable-version --------------------------------------------------------------------------------------

    /// <summary>The oldest workflow definition version this definition still supports. <see cref="Version"/> is the one this run executes.</summary>
    public ulong MinVersion() => IDurableVersionImports.DurableMinVersion();

    // ---- continue-as-new --------------------------------------------------------------------------------------

    /// <summary>
    /// Ends this run and starts the next with <paramref name="input"/> (history compaction): a long-running workflow calls this every so
    /// often so its history stays bounded. <b>This method never returns normally</b>: it records the continuation and then unwinds
    /// with <see cref="CleatSuspendException"/>, so nothing after it runs and the host restarts the workflow. (The Python SDK leaves the
    /// raise to the caller; here it cannot be forgotten.) The same unwinding happens on replay.
    /// </summary>
    /// <param name="input">The new run's input, as JSON.</param>
    /// <exception cref="CleatSuspendException">Always, on a successful call: that is how the run ends.</exception>
    /// <exception cref="CleatCallException">The host refused the continuation (it reports a non-zero error code).</exception>
    /// <exception cref="InvalidOperationException">Called from inside a <see cref="Defer(string, Action)"/> body: the run's outcome is
    /// already decided by then, so a recorded continuation is one the worker would never take.</exception>
    [DoesNotReturn]
    public void ContinueAsNew(string input)
    {
        RefuseInDeferBody("ContinueAsNew");
        EndRun(IDurableLifecycleImports.DurableContinueAsNew(input), "continue-as-new");
    }

    /// <summary>
    /// <see cref="ContinueAsNew(string)"/> with an explicit definition version for the next run. A <paramref name="newVersion"/> of 0
    /// keeps the current version, as the host reads it.
    /// </summary>
    /// <exception cref="CleatSuspendException">Always, on a successful call.</exception>
    /// <exception cref="CleatCallException">The host refused the continuation.</exception>
    /// <exception cref="InvalidOperationException">Called from inside a defer body.</exception>
    [DoesNotReturn]
    public void ContinueAsNew(string input, uint newVersion)
    {
        RefuseInDeferBody("ContinueAsNew");
        EndRun(IDurableExtendedLifecycleImports.ContinueAsNewVersioned(input, newVersion), "continue-as-new-versioned");
    }

    /// <summary>The refusal python-sdk/cleat_sdk/defer.py's <c>defer_phase_refusal</c> words for the same reason, before the host call.</summary>
    private void RefuseInDeferBody(string what)
    {
        if (_defers.InDeferPhase)
            throw new InvalidOperationException(
                $"cleat: {what} is not allowed from inside a defer body: the defer table is drained before the first body runs and the " +
                "workflow's result is already decided, so this would be recorded durably and never taken");
    }

    /// <summary>
    /// Reads the word a continuation returns and unwinds. <see cref="Packed.CheckHostResult"/> tests the stop sentinel first and raises the
    /// host's status as the base <see cref="CleatCallException"/>; a word that passes both is the continuation, and the run ends here.
    /// </summary>
    [DoesNotReturn]
    private static void EndRun(ulong word, string what)
    {
        Packed.CheckHostResult(word, "", what, what);
        throw new CleatSuspendException();
    }
}
