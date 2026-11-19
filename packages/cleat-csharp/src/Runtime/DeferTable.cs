using System;
using System.Collections.Generic;
using System.Threading.Tasks;

namespace Cleat.Runtime;

/// <summary>
/// The guest's defer table: bodies registered under the id the host minted for them, drained LIFO.
/// </summary>
/// <remarks>
/// Behaviour follows python-sdk/cleat_sdk/defer.py's <c>run_deferred</c>:
/// <list type="bullet">
/// <item>The table is emptied BEFORE the first body runs, so a second drain runs nothing. Cleanup that runs
/// twice releases a lock twice or refunds a charge twice.</item>
/// <item>LIFO. A defer releases what the defer before it acquired.</item>
/// <item>An exception in one body does not stop the others and does not disturb the workflow's result, which
/// is already decided.</item>
/// <item>A suspend from a body depends on WHO drains: the guest's own drain at the end of a finished run
/// propagates it (suspension wins over the result); the host's drain (<c>run-deferred</c>) swallows it,
/// because the remaining bodies are already off the table and propagating would consume them unrun.</item>
/// <item>The drain is bracketed with <c>durable-defer-phase(true/false)</c> in a <c>finally</c>, so the engine
/// can tell a defer body's durable calls from the workflow body's (cleat#1155).</item>
/// </list>
/// One deliberate difference from Python: an empty table makes no <c>durable-defer-phase</c> calls. The phase
/// marks the events the drain produces, and an empty drain produces none.
/// </remarks>
internal sealed class DeferTable
{
    private readonly List<(string Id, Func<Task> Body)> _bodies = new();
    private readonly Action<bool> _phase;

    internal DeferTable(Action<bool> phase) { _phase = phase; }

    internal int Count => _bodies.Count;

    /// <summary>True while a drain is running bodies: a durable call made now is a defer body's, not the workflow body's. Calls that
    /// decide the run's outcome (continue-as-new) refuse in this window (python-sdk/cleat_sdk/defer.py, <c>in_defer_phase</c>).</summary>
    internal bool InDeferPhase { get; private set; }

    internal void Register(string id, Func<Task> body) => _bodies.Add((id, body));

    internal uint Drain(bool propagateSuspend)
    {
        if (_bodies.Count == 0) return 0;
        var taken = _bodies.ToArray();
        _bodies.Clear();

        _phase(true);
        InDeferPhase = true;
        try
        {
            uint ran = 0;
            for (var i = taken.Length - 1; i >= 0; i--)
            {
                ran++;
                try
                {
                    taken[i].Body().GetAwaiter().GetResult();
                }
                catch (Exception ex) when (ExceptionInfo.IsSuspend(ex))
                {
                    if (propagateSuspend) throw;
                }
                catch (Exception)
                {
                    // one bad cleanup must not stop the rest
                }
            }
            return ran;
        }
        finally
        {
            InDeferPhase = false;
            _phase(false);
        }
    }
}
