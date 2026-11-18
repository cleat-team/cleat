namespace Cleat.Runtime;

/// <summary>
/// Decoding of the packed <c>u64</c> status words some host calls still return.
/// </summary>
/// <remarks>
/// One decoder, in one place. <c>durable-sleep</c> returns a word whose top byte is a status
/// (<c>packSleepResult</c>, engine/helpers.go: <c>status &lt;&lt; 56 | durationMs</c>), and a stopped guest can
/// instead receive exactly <c>1 &lt;&lt; 31</c> (<c>callSuspendSentinel</c>, engine/memory.go). Both mean
/// "suspend". The sentinel is tested FIRST and by whole-word equality, as Python's <c>sleep_ms</c> does: bit 31
/// is free in every layout the host packs, but it is not free in every layout's FIELDS.
/// </remarks>
internal static class Packed
{
    /// <summary>The sentinel the host returns to a body that must unwind (<c>callSuspendSentinel</c>).</summary>
    internal const ulong SuspendSentinel = 1UL << 31;

    /// <summary><c>sleepStatusSuspend</c> in the top byte of a sleep result.</summary>
    internal const byte SleepStatusSuspend = 1;

    /// <summary>True if a <c>durable-sleep</c> result means the workflow must suspend.</summary>
    internal static bool SleepMustSuspend(ulong result)
        => result == SuspendSentinel || (byte)(result >> 56) == SleepStatusSuspend;
}
