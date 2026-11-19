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

    /// <summary>
    /// Decodes a packed status word, as Python's <c>_check_host_result</c> does: the stop first, then the error,
    /// then the upper 32 bits (which some calls use as a length or flag).
    /// </summary>
    /// <remarks>
    /// The stop is tested FIRST and by MASK, before any field is read. Bit 31 is free in every layout the host
    /// packs for these calls, but "free" means the host cannot produce it as an ordinary value, not that a field
    /// decode would ignore it: a decoder that reads fields first turns a stop into an ordinary result and carries on
    /// past a stop it was told to obey. This is not <see cref="SleepMustSuspend"/>'s whole-word test, which exists
    /// because a sleep word's DURATION field can legitimately set bit 31.
    /// </remarks>
    internal static uint CheckHostResult(ulong result, string service, string operation, string what)
    {
        if ((result & SuspendSentinel) != 0) throw new Cleat.CleatSuspendException();
        var code = (uint)(result & 0xFF);
        // The BASE exception, with no classification claimed. These words are not call-failure values: the engine answers
        // a generic 1 for ANY failure of resolve/reject (packSimpleResult(1, 0), engine/promises.go), so mapping the number
        // through the call-error table would turn "promise not found" into a TIMEOUT, the retryable kind. Python does the
        // same by raising a plain RuntimeError here and borrowing the table only for its message; this does not borrow
        // the names, because the names are wrong for these words.
        if (code != 0)
            throw new Cleat.CleatCallException(service, operation, $"{what} failed: host status code {code}", 0);
        return (uint)(result >> 32);
    }

    /// <summary>True if a <c>durable-sleep</c> result means the workflow must suspend.</summary>
    internal static bool SleepMustSuspend(ulong result)
        => result == SuspendSentinel || (byte)(result >> 56) == SleepStatusSuspend;
}
