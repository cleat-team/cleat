using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// The defer registration against a KILLED run (cleat#4079; tests/csharp/defer_test.go drives these through the real engine). Entry
/// names are prefixed <c>defer_</c>.
/// </summary>
public static class GroupDefer
{
    /// <summary>
    /// One call, a defer whose body makes a call of its own, then a call after. The Go test replays the history cut after the defer
    /// registration as a defer segment: the registration is the run's last stoppable event, so the host answers it with a stop.
    /// </summary>
    [CleatEntry("defer_cleanup_after_kill")]
    public static JsonNode CleanupAfterKill(HostCalls h, JsonElement input)
    {
        h.Call("svc", "before", "{}");
        h.Defer("cleanup", () => { h.Call("svc", "cleanup", "{}"); });
        h.Call("svc", "after", "{}");
        return new JsonObject { ["ok"] = true };
    }
}
