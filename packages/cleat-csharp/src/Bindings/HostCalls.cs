using System;
using System.Threading.Tasks;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

/// <summary>
/// The workflow's handle on the durable host calls.
/// </summary>
/// <remarks>
/// <para>
/// Every method is <b>synchronous</b>. A workflow may be written with <c>async</c>/<c>await</c>, because every
/// task it awaits over these methods is already complete (measured, cleat#4077), but nothing here yields: the
/// runtime has no threads and no scheduler. <c>Task.Delay</c>, <c>Task.Run</c> and a blocking
/// wait on an incomplete task throw <see cref="PlatformNotSupportedException"/> (<c>Task.Yield</c> returns). To wait, use
/// <see cref="Sleep(TimeSpan)"/>, which is durable.
/// </para>
/// <para>
/// A method throws <see cref="CleatSuspendException"/> when the host has stopped the workflow, and
/// <see cref="CleatCallException"/> (or a subclass) when a call failed. See the notes on
/// <see cref="CleatSuspendException"/> before writing a <c>catch</c>.
/// </para>
/// <para>
/// This type holds the host calls an entry point needs first. The rest of the WIT world's functions are added
/// to it as the SDK's coverage grows; see packages/cleat-csharp/README.md for the list.
/// </para>
/// </remarks>
public sealed partial class HostCalls
{
    private readonly DeferTable _defers;

    internal HostCalls(DeferTable defers) { _defers = defers; }

    /// <summary>The host half of <see cref="DeferTable"/>'s phase bracket.</summary>
    internal static void SetDeferPhase(bool on) => IDurableLifecycleImports.DurableDeferPhase(on);

    // ---- durable-call -----------------------------------------------------------------------------------------

    /// <summary>
    /// Makes a durable call: recorded, and on replay answered from history. Returns the response JSON.
    /// </summary>
    public string Call(string service, string operation, string request)
    {
        try
        {
            return IDurableCallImports.DurableCall(service, operation, request);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate(service, operation, f);
        }
    }

    /// <summary>
    /// A <c>call-failure</c> is a case of a type, not text a service could have returned. A stop unwinds; a
    /// failure is an error carrying the host's classification (python-sdk/cleat_sdk/host_calls.py,
    /// <c>_raise_call_failure</c>).
    /// </summary>
    private static Exception Translate(string service, string operation, IOutcomesImports.CallFailure f)
    {
        if (f.Tag == IOutcomesImports.CallFailure.Tags.Suspended)
            return new CleatSuspendException();
        var err = f.AsFailed;
        return CleatCallException.For(service, operation, err.message, err.code);
    }

    // ---- durable-sleep ----------------------------------------------------------------------------------------

    /// <summary>
    /// Waits durably. On a fresh execution the host suspends the workflow and this throws
    /// <see cref="CleatSuspendException"/>; on replay the wait has already happened and it returns.
    /// </summary>
    public void Sleep(TimeSpan duration) => SleepMs((ulong)Math.Max(0, duration.TotalMilliseconds));

    /// <summary><see cref="Sleep(TimeSpan)"/> in milliseconds.</summary>
    public void SleepMs(ulong milliseconds)
    {
        DispatchUpdates(); // a dispatch point: python's sleep_ms serves pending updates before it suspends
        if (Packed.SleepMustSuspend(IDurableSleepImports.DurableSleep(milliseconds)))
            throw new CleatSuspendException();
    }

    /// <summary>The workflow's clock: replayed, so it returns the same instant on every replay. Use this, not
    /// <c>DateTime.UtcNow</c>.</summary>
    public DateTimeOffset Now() => DateTimeOffset.FromUnixTimeMilliseconds((long)IDurableSleepImports.DurableNow());

    /// <summary>A replayed random 64-bit value. Use this, not <c>Random</c> or <c>Guid.NewGuid()</c>.</summary>
    public ulong Random() => IDurableSleepImports.DurableRandom();

    /// <summary>Writes a message to the workflow's event history.</summary>
    public void Log(string message) => IDurableSleepImports.DurableLog(message);

    // ---- durable-identity / durable-version -------------------------------------------------------------------

    public string WorkflowId() => IDurableIdentityImports.DurableWorkflowId();

    public string RunId() => IDurableIdentityImports.DurableRunId();

    /// <summary>The workflow definition version this run executes.</summary>
    public ulong Version() => IDurableVersionImports.DurableVersion();

    // ---- durable-lifecycle ------------------------------------------------------------------------------------

    /// <summary>
    /// Registers cleanup to run when the workflow exits, last registered first. The body runs on the guest's
    /// own drain when the workflow finishes or fails, and on the host's drain (<c>run-deferred</c>) when the
    /// workflow is terminated. A body that throws does not stop the others.
    /// </summary>
    public void Defer(string description, Action body)
        => Defer(description, () => { body(); return Task.CompletedTask; });

    /// <inheritdoc cref="Defer(string, Action)"/>
    public void Defer(string description, Func<Task> body)
        => _defers.Register(() => HostDefer(description), body);

    /// <summary>
    /// The host half of <see cref="Defer(string, Func{Task})"/>: <c>durable-defer</c> returns <c>result&lt;string,
    /// call-failure&gt;</c> since cleat#4079, and a stop arrives as the <c>suspended</c> case. It becomes
    /// <see cref="CleatSuspendException"/> like every other stop, and <see cref="DeferTable"/> decides what each case
    /// does to the table.
    /// </summary>
    private static string HostDefer(string description)
    {
        try
        {
            return IDurableLifecycleImports.DurableDefer(description);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("workflow", "defer", f);
        }
    }

    /// <summary>The cancellation reason if cancellation has been requested, or an empty string.</summary>
    public string PollCancellation() => IDurableLifecycleImports.DurablePollCancellation();
}
