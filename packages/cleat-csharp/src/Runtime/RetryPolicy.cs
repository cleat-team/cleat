using System;
using System.Collections.Generic;
using System.IO;
using System.Text.Json;

namespace Cleat;

/// <summary>
/// The retry policy of <see cref="HostCalls.CallWithRetry"/>: the host retries the call itself, records ONE event whatever the number
/// of attempts, and returns either the first success or the last failure.
/// </summary>
/// <remarks>
/// <para>
/// The defaults are python-sdk/cleat_sdk/host_calls.py's <c>RetryPolicy</c>: three attempts, a one second first interval, doubling,
/// capped at thirty seconds.
/// </para>
/// <para>
/// <b>The host may refuse a policy.</b> A policy whose worst-case total backoff exceeds the host's retry budget is refused, and the
/// refusal is itself recorded; the call then throws a <see cref="CleatCallException"/> whose <see cref="CleatCallException.Code"/> is
/// <see cref="RetryPolicyTooLongCode"/>. Nothing was retried and no attempt was spent. The Python, Java and AssemblyScript SDKs surface
/// it as an error exactly like this; Go and Rust fall back to retrying in the guest. This SDK does not fall back: to run a long policy
/// yourself, loop over <see cref="HostCalls.Call"/> and <see cref="HostCalls.Sleep(TimeSpan)"/>, which is durable between attempts.
/// </para>
/// </remarks>
public sealed record RetryPolicy
{
    /// <summary>The <see cref="CleatCallException.Code"/> of a refused policy (<c>callErrorRetryPolicyTooLong</c>, engine/memory.go).</summary>
    public const uint RetryPolicyTooLongCode = 6;

    /// <summary>The most attempts, the original call included. At least 1.</summary>
    public int MaxAttempts { get; init; } = 3;

    /// <summary>The wait after the first failure.</summary>
    public TimeSpan InitialInterval { get; init; } = TimeSpan.FromSeconds(1);

    /// <summary>The factor each wait is multiplied by. The host works in hundredths, so only two decimals mean anything: 1.5 and 1.504
    /// are the same policy. A coefficient of 1 or less keeps the wait constant.</summary>
    public double BackoffCoefficient { get; init; } = 2.0;

    /// <summary>The longest single wait. <see cref="TimeSpan.Zero"/> means no cap (the host's reading of 0).</summary>
    public TimeSpan MaxInterval { get; init; } = TimeSpan.FromSeconds(30);

    /// <summary>
    /// Error codes that must NOT be retried: the call fails at once with the first one of these. A failure is matched by the CODE the
    /// service gave it (exact string equality), never by its message; a failure with no code is always retried.
    /// </summary>
    public IReadOnlyList<string> NonRetryableErrors { get; init; } = Array.Empty<string>();

    /// <summary>The five wire values of <c>durable-call-retry</c>, after validating the policy.</summary>
    /// <exception cref="ArgumentOutOfRangeException">The policy cannot mean anything to the host: no attempts, a negative or non-finite number.</exception>
    internal (ulong MaxAttempts, ulong InitialIntervalMs, ulong BackoffCoefficient100x, ulong MaxIntervalMs, string NonRetryableErrorsJson) ToWire()
    {
        if (MaxAttempts < 1)
            throw new ArgumentOutOfRangeException(nameof(MaxAttempts), MaxAttempts, "a retry policy needs at least one attempt (the original call)");
        if (!double.IsFinite(BackoffCoefficient) || BackoffCoefficient < 0 || BackoffCoefficient * 100 >= 9.2e18)
            throw new ArgumentOutOfRangeException(nameof(BackoffCoefficient), BackoffCoefficient, "the backoff coefficient must be finite and not negative");
        return ((ulong)MaxAttempts,
                Milliseconds(InitialInterval, nameof(InitialInterval)),
                // Round-half-to-even, as Python's round() does for the same conversion.
                (ulong)Math.Round(BackoffCoefficient * 100, MidpointRounding.ToEven),
                Milliseconds(MaxInterval, nameof(MaxInterval)),
                EncodeNonRetryable(NonRetryableErrors));
    }

    private static ulong Milliseconds(TimeSpan t, string name)
    {
        if (t < TimeSpan.Zero)
            throw new ArgumentOutOfRangeException(name, t, "an interval cannot be negative");
        return (ulong)Math.Round(t.TotalMilliseconds, MidpointRounding.ToEven);
    }

    /// <summary>
    /// The JSON array the host reads (<c>json.dumps(retry.non_retryable_errors)</c> in the Python SDK). An unreadable list is REFUSED by
    /// the host rather than read as an empty one, so this always writes a well-formed array, including <c>[]</c> for none.
    /// </summary>
    internal static string EncodeNonRetryable(IReadOnlyList<string>? codes)
    {
        using var ms = new MemoryStream();
        using (var w = new Utf8JsonWriter(ms, new JsonWriterOptions { Encoder = Runtime.EntryJson.Encoder }))
        {
            w.WriteStartArray();
            if (codes is not null)
                foreach (var c in codes)
                    w.WriteStringValue(c ?? throw new ArgumentException("a non-retryable error code cannot be null", nameof(codes)));
            w.WriteEndArray();
        }
        return System.Text.Encoding.UTF8.GetString(ms.ToArray());
    }
}
