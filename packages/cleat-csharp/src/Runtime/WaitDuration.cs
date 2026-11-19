using System;

namespace Cleat.Runtime;

/// <summary>
/// A wait's <see cref="TimeSpan"/> as the milliseconds the host is given (cleat#4144).
/// </summary>
internal static class WaitDuration
{
    /// <summary>
    /// Zero or less stays 0, the caller's own non-positive value. A POSITIVE span rounds UP, so one under a millisecond is 1 and never
    /// the 0 that <c>AwaitSignalsMs</c> reads as "wait indefinitely": it truncated before, so <c>TimeSpan.FromTicks(5)</c> waited
    /// for ever (cleat#4138 item 1). The schedule-delay and lock-TTL calls (<c>HostCalls.F.cs</c>, <c>HostCalls.D.cs</c>) already round up with <see cref="Math.Ceiling(double)"/>.
    /// </summary>
    internal static ulong Ms(TimeSpan timeout)
        => timeout <= TimeSpan.Zero ? 0UL : (ulong)Math.Ceiling(timeout.TotalMilliseconds);
}
