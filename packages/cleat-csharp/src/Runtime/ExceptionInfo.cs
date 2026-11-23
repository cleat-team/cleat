using System;

namespace Cleat.Runtime;

internal static class ExceptionInfo
{
    /// <summary>
    /// Peels <see cref="AggregateException"/> wrappers that hold exactly one inner exception, which is what
    /// <c>Task.Result</c>, <c>Task.Wait()</c> and <c>Task.WhenAll</c> produce. <c>GetAwaiter().GetResult()</c>
    /// does not wrap, but user code may call the others.
    /// </summary>
    internal static Exception Unwrap(Exception ex)
    {
        while (ex is AggregateException agg && agg.InnerExceptions.Count == 1)
            ex = agg.InnerExceptions[0];
        return ex;
    }

    internal static bool IsSuspend(Exception ex) => Unwrap(ex) is CleatSuspendException;

    /// <summary>
    /// What a <c>run-outcome::failed</c> carries: the exception's type and message, or the type alone when the
    /// message is empty. The type is included for the reason python-sdk/cleat_sdk/entry.py gives: a message
    /// can be empty, and a failed run with no text tells an operator nothing.
    /// </summary>
    internal static string Describe(Exception ex)
    {
        ex = Unwrap(ex);
        var kind = ex.GetType().Name;
        return string.IsNullOrEmpty(ex.Message) ? kind : kind + ": " + ex.Message;
    }
}
