using System;

namespace Cleat;

/// <summary>
/// Thrown from a <see cref="HostCalls"/> method when the host has stopped the workflow: a sleep that must
/// suspend, or <c>call-failure::suspended</c>.
/// </summary>
/// <remarks>
/// The entry boundary turns it into <c>run-outcome::suspended</c>. <b>It can be caught</b> (.NET cannot make
/// an exception uncatchable), and catching it is almost always a mistake. Measured on the real engine
/// (cleat#4077): the engine records the suspension on the host side, so a <c>catch (Exception)</c> that
/// swallows this exception does not change the outcome, which stays "suspended". What it does is let the
/// workflow keep running after the suspend, and the host calls it then makes are real and are recorded. The
/// same happens when the exception is captured in a <see cref="System.Threading.Tasks.Task"/> that is not yet
/// awaited. Do not catch <see cref="Exception"/> without rethrowing this one.
/// </remarks>
public sealed class CleatSuspendException : Exception
{
    public CleatSuspendException() : base("the workflow is suspended") { }
}

/// <summary>A durable host call failed. <see cref="Code"/> is the host's own classification.</summary>
public class CleatCallException : Exception
{
    public CleatCallException(string service, string operation, string message, uint code)
        : base(message)
    {
        Service = service;
        Operation = operation;
        Code = code;
    }

    public string Service { get; }
    public string Operation { get; }

    /// <summary>The host's <c>callErrorCode</c>; 0 means the host offered no classification.</summary>
    public uint Code { get; }

    /// <summary>
    /// Picks the exception class for <paramref name="code"/>, with the same mapping as the Python SDK's
    /// <c>_CALL_ERROR_CODE_MAP</c> (python-sdk/cleat_sdk/host_calls.py), which mirrors the Go SDK's
    /// <c>CallErrorCode</c> constants: 1 timeout, 2 unavailable, 3 not found, 4 invalid request,
    /// 5 permission denied. An unknown code is the base class, as in Python.
    /// </summary>
    internal static CleatCallException For(string service, string operation, string message, uint code) => code switch
    {
        1 => new CleatCallTimeoutException(service, operation, message, code),
        2 => new CleatCallTransientException(service, operation, message, code),
        3 or 4 or 5 => new CleatCallPermanentException(service, operation, message, code),
        _ => new CleatCallException(service, operation, message, code),
    };
}

/// <summary>A failure that may succeed if the call is made again.</summary>
public class CleatCallTransientException : CleatCallException
{
    public CleatCallTransientException(string service, string operation, string message, uint code)
        : base(service, operation, message, code) { }
}

/// <summary>The call exceeded its deadline. Transient.</summary>
public sealed class CleatCallTimeoutException : CleatCallTransientException
{
    public CleatCallTimeoutException(string service, string operation, string message, uint code)
        : base(service, operation, message, code) { }
}

/// <summary>A failure that will not succeed if the call is made again (bad request, not found, denied).</summary>
public sealed class CleatCallPermanentException : CleatCallException
{
    public CleatCallPermanentException(string service, string operation, string message, uint code)
        : base(service, operation, message, code) { }
}
