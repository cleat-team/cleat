using System;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

// Group D of cleat#4080's host-call coverage: locks, the virtual-object scope, and deterministic ids.
// durable-acquire-lock, durable-release-lock, set-scope, get-scope, uuid.
public sealed partial class HostCalls
{
    // ---- durable-lock -----------------------------------------------------------------------------------------

    /// <summary>
    /// Tries to take the named lock for <paramref name="ttl"/>. Returns <c>true</c> if this workflow now holds it and <c>false</c> if another
    /// workflow does; it never waits. The answer is recorded, so a replay returns the same answer without asking the store again.
    /// </summary>
    /// <remarks>
    /// A lock the workflow never releases expires after the TTL; it is also released when the workflow ends. A <c>false</c> is an ordinary
    /// answer, not a failure: loop over <see cref="Sleep(TimeSpan)"/> and this method to wait for the lock durably.
    /// </remarks>
    /// <param name="key">The lock's name, shared by every workflow of the tenant that wants the same lock.</param>
    /// <param name="ttl">How long the lock may be held, at least one millisecond (a positive TTL shorter than that is rounded UP to it).
    /// A lock that expired the moment it was taken would report success and exclude nobody, so zero and negative are refused.</param>
    /// <exception cref="ArgumentOutOfRangeException"><paramref name="ttl"/> is not positive or is too large to count in milliseconds.</exception>
    /// <exception cref="CleatCallException">The host could not take the lock (a store failure, or the per-workflow lock quota), reported as
    /// the base type with <see cref="CleatCallException.Code"/> 0.</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow, which it does for an acquire in a defer segment's body
    /// (a defer body's own acquire is allowed).</exception>
    public bool AcquireLock(string key, TimeSpan ttl)
    {
        ArgumentNullException.ThrowIfNull(key);
        var ms = Math.Ceiling(ttl.TotalMilliseconds);
        if (!(ms >= 1) || ms >= long.MaxValue)
            throw new ArgumentOutOfRangeException(nameof(ttl), ttl, "a lock's TTL must be at least one millisecond");
        var word = (ulong)IDurableLockImports.DurableAcquireLock(key, (long)ms);
        // The stop sentinel and the error byte first, in that order: the 'acquired' flag sits at bit 8, so a decoder that read the fields
        // before looking for a stop would take a stopped workflow's word for "someone else holds it" and run on without the lock
        // (python-sdk/cleat_sdk/host_calls.py, acquire_lock_ms; IMPROVEMENT-PLAN 3.301).
        Packed.CheckHostResult(word, "", "acquire-lock", "acquire-lock");
        return ((word >> 8) & 1) != 0;
    }

    /// <summary>
    /// Releases a lock this workflow holds. Releasing a lock that is not held (its TTL passed, or another workflow took it after) is NOT an
    /// error: the host records it and returns, because there is nothing the caller could do about it.
    /// </summary>
    /// <exception cref="CleatCallException">The store failed (the base type, <see cref="CleatCallException.Code"/> 0).</exception>
    public void ReleaseLock(string key)
    {
        ArgumentNullException.ThrowIfNull(key);
        Packed.CheckHostResult((ulong)IDurableLockImports.DurableReleaseLock(key), "", "release-lock", "release-lock");
    }

    // ---- durable-scope ----------------------------------------------------------------------------------------

    /// <summary>
    /// Makes this workflow the holder of the virtual object <paramref name="objectType"/>/<paramref name="instanceKey"/> (at most one
    /// workflow runs under a scope at a time; the scope key is held for up to 24 hours or until it is cleared or the workflow ends).
    /// Setting a scope while another is set releases the first. Passing <c>""</c> and <c>""</c> clears it. Returns the PREVIOUS scope's
    /// prefix as the host spells it (<c>vo:{type}:{key}:</c>), or <c>""</c> when there was none.
    /// </summary>
    /// <remarks>
    /// <b>This call does not tell you that the object was busy, and the guest is not stopped.</b> The host answers contention by recording
    /// the refusal and suspending the workflow AT THE END OF THE SEGMENT; the string it returns is the same as for success, and later host
    /// calls in the segment are not refused. So a normal return does not mean the scope is held, and effectful calls made right after it
    /// can run without it (cleat#3916, open; the C# SDK measures the same as the Python and TypeScript ones, whose WIT function has no
    /// failure channel either). If mutual exclusion matters before the first effect, take a lock with <see cref="AcquireLock"/>, whose
    /// answer is a value you can branch on.
    /// </remarks>
    /// <exception cref="ArgumentException">Exactly one of the two is empty: the host would take a scope no one can name, and the Python
    /// SDK reads that combination as "no scope".</exception>
    public string SetScope(string objectType, string instanceKey)
    {
        ArgumentNullException.ThrowIfNull(objectType);
        ArgumentNullException.ThrowIfNull(instanceKey);
        if ((objectType.Length == 0) != (instanceKey.Length == 0))
            throw new ArgumentException("a scope needs both an object type and an instance key, or neither to clear it");
        return IDurableScopeImports.SetScope(objectType, instanceKey);
    }

    /// <summary>The scope this workflow runs under, or two empty strings when none is set. Reading it cannot fail and cannot stop.</summary>
    public (string ObjectType, string InstanceKey) GetScope()
    {
        var s = IDurableScopeImports.GetScope();
        return (s.objectType, s.instanceKey);
    }

    // ---- uuid -------------------------------------------------------------------------------------------------

    /// <summary>
    /// A deterministic UUID: the same <paramref name="seed"/> gives the same text for this workflow instance, on replay and across segments,
    /// and a different seed or a different workflow gives a different one. Lower-case <c>8-4-4-4-12</c> hex with the version nibble 5 and
    /// the variant bits 10. Use it for ids that must survive replay; it is NOT a secret and NOT random.
    /// </summary>
    public string Uuid(string seed)
    {
        ArgumentNullException.ThrowIfNull(seed);
        return IDurableScopeImports.Uuid(seed);
    }
}
