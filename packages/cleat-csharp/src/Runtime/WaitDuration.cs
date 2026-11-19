using System;

namespace Cleat.Runtime;

/// <summary>
/// A wait's <see cref="TimeSpan"/> as the milliseconds the host is given (cleat#4144).
/// </summary>
internal static class WaitDuration
{
    /// <summary>The count that means "wait without a limit" (cleat#4144): the largest value the host's signed 64-bit timeout holds, which it clamps (cleat#4105).</summary>
    internal const ulong Forever = (ulong)long.MaxValue;

    /// <summary>
    /// <see cref="System.Threading.Timeout.InfiniteTimeSpan"/> is the explicit wait without a limit and becomes <see cref="Forever"/>.
    /// Any other zero or negative span stays 0, which the host reads as DO NOT WAIT. A POSITIVE span rounds UP, so one under a
    /// millisecond is 1 and never that 0: it truncated before, so <c>TimeSpan.FromTicks(5)</c> waited for ever (cleat#4138 item 1).
    /// The schedule-delay and lock-TTL calls (<c>HostCalls.F.cs</c>, <c>HostCalls.D.cs</c>) already round up with <see cref="Math.Ceiling(double)"/>.
    /// </summary>
    internal static ulong Ms(TimeSpan timeout)
        => timeout == System.Threading.Timeout.InfiniteTimeSpan ? Forever
         : timeout <= TimeSpan.Zero ? 0UL
         : (ulong)Math.Ceiling(timeout.TotalMilliseconds);
}
