using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// Group A of cleat#4080's host-call coverage (tests/csharp/group_a_test.go drives these through the real engine): the call variants
/// (retry, heartbeat), the definition versions, and continue-as-new. Entry names are prefixed <c>a_</c>.
/// </summary>
/// <remarks>
/// An entry that can fail by design reports the failure as data (type, code, message) instead of letting it escape, so the Go side can
/// assert what the SDK translated the host's failure into. A <see cref="CleatSuspendException"/> is never caught here: it is how a run ends.
/// </remarks>
public static class GroupA
{
    private static JsonNode Failure(CleatCallException e) => new JsonObject
    {
        ["failed"] = true,
        ["type"] = e.GetType().Name,
        ["code"] = (int)e.Code,
        ["message"] = e.Message,
    };

    /// <summary>Input: service, operation, request, maxAttempts, initialMs, backoff, maxMs, nonRetryable (array of codes).</summary>
    [CleatEntry("a_call_retry")]
    public static JsonNode CallRetry(HostCalls h, JsonElement input)
    {
        var policy = new RetryPolicy
        {
            MaxAttempts = input.GetProperty("maxAttempts").GetInt32(),
            InitialInterval = TimeSpan.FromMilliseconds(input.GetProperty("initialMs").GetInt64()),
            BackoffCoefficient = input.GetProperty("backoff").GetDouble(),
            MaxInterval = TimeSpan.FromMilliseconds(input.GetProperty("maxMs").GetInt64()),
            NonRetryableErrors = input.GetProperty("nonRetryable").EnumerateArray().Select(x => x.GetString()!).ToList(),
        };
        try
        {
            var response = h.CallWithRetry(input.GetProperty("service").GetString()!, input.GetProperty("operation").GetString()!,
                input.GetProperty("request").GetRawText(), policy);
            return new JsonObject { ["response"] = response };
        }
        catch (CleatCallException e)
        {
            return Failure(e);
        }
        catch (ArgumentOutOfRangeException e)
        {
            return new JsonObject { ["invalid"] = e.ParamName };
        }
    }

    /// <summary>Input: service, operation, request, intervalMs.</summary>
    [CleatEntry("a_call_heartbeat")]
    public static JsonNode CallHeartbeat(HostCalls h, JsonElement input)
    {
        try
        {
            var response = h.CallWithHeartbeat(input.GetProperty("service").GetString()!, input.GetProperty("operation").GetString()!,
                input.GetProperty("request").GetRawText(), TimeSpan.FromMilliseconds(input.GetProperty("intervalMs").GetInt64()));
            return new JsonObject { ["response"] = response };
        }
        catch (CleatCallException e)
        {
            return Failure(e);
        }
        catch (ArgumentOutOfRangeException e)
        {
            return new JsonObject { ["invalid"] = e.ParamName };
        }
    }

    [CleatEntry("a_min_version")]
    public static JsonNode MinVersion(HostCalls h, JsonElement input)
        => new JsonObject { ["min"] = (long)h.MinVersion(), ["version"] = (long)h.Version() };

    /// <summary>Input: next (the next run's input). Makes one call first, then continues as new, then (unreachable) makes another.</summary>
    [CleatEntry("a_continue_as_new")]
    public static JsonNode ContinueAsNew(HostCalls h, JsonElement input)
    {
        h.Call("svc", "before", "{}");
        h.ContinueAsNew(input.GetProperty("next").GetRawText());
#pragma warning disable CS0162 // unreachable: that is the property under test
        h.Call("svc", "after", "{}");
        return new JsonObject { ["reached"] = "after continue-as-new" };
#pragma warning restore CS0162
    }

    /// <summary>Input: next, version.</summary>
    [CleatEntry("a_continue_as_new_versioned")]
    public static JsonNode ContinueAsNewVersioned(HostCalls h, JsonElement input)
    {
        h.ContinueAsNew(input.GetProperty("next").GetRawText(), input.GetProperty("version").GetUInt32());
#pragma warning disable CS0162
        return new JsonObject { ["reached"] = "after continue-as-new" };
#pragma warning restore CS0162
    }

    /// <summary>
    /// Continue-as-new from inside a defer body must be REFUSED before the host call. The body reports the refusal through a service call
    /// ("svc"/"refused" carrying the message), which the recording caller sees; a body that wrongly got through would have recorded a
    /// continuation instead and turned the run into a suspension.
    /// </summary>
    [CleatEntry("a_continue_in_defer")]
    public static JsonNode ContinueInDefer(HostCalls h, JsonElement input)
    {
        h.Defer("try to continue as new", () =>
        {
            try
            {
                h.ContinueAsNew("{}");
            }
            catch (InvalidOperationException e)
            {
                h.Call("svc", "refused", JsonValue.Create(e.Message)!.ToJsonString());
            }
        });
        return new JsonObject { ["done"] = true };
    }
}
