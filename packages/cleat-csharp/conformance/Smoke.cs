using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Conformance;

/// <summary>
/// The foundation's own entry point (PR 5.0 of cleat#4080): the smallest workflow that proves the whole path, the build, the stamp,
/// the engine and the SDK's entry wrapper, before any group adds a host call. Groups add their entries in their own Group files and
/// name them <c>&lt;group&gt;_&lt;name&gt;</c>, so entry names never collide across groups.
/// </summary>
public static class Smoke
{
    [CleatEntry("smoke")]
    public static JsonNode Run(HostCalls h, JsonElement input)
        => new JsonObject
        {
            ["id"] = h.WorkflowId(),
            ["echo"] = JsonNode.Parse(input.GetRawText()),
        };
}
