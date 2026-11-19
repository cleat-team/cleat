using System;
using System.Collections.Generic;
using System.Text.Json;
using Cleat.Runtime;

namespace Cleat
{
    /// <summary>One child workflow's outcome (python-sdk <c>ChildResult</c>).</summary>
    /// <param name="RunId">The child's run id.</param>
    /// <param name="Result">The child's output JSON; empty if the child failed.</param>
    /// <param name="Error">The failure message, or <c>null</c> if the child succeeded.</param>
    public readonly record struct ChildResult(string RunId, string Result, string? Error);
}

namespace Cleat.Runtime
{
    /// <summary>
    /// Reads the JSON documents the host writes for the child calls, as python-sdk/cleat_sdk/host_calls.py does. Pure, so the quirks
    /// are unit-tested: the three string-typed awaits cannot carry a stop, and an EMPTY answer means different things for each.
    /// </summary>
    /// <remarks>
    /// A pending child await on a component is not an empty answer at all: the engine traps the guest and lets the recorded suspension
    /// win (engine/component_callbacks.go, stopAtPendingChildAwait). An empty string reaches the guest ONLY from a defer segment or the
    /// guest's own defer drain, where a trap would lose the cleanup. So an empty answer here means "nothing finished, and you are
    /// cleaning up", and each call reads it the way Python does.
    /// </remarks>
    internal static class ChildDocuments
    {
        private static CleatCallException Failed(string call, string detail)
            => new("child", call, $"{call} failed: {detail}", 0);

        private static JsonElement Object(string call, string text, JsonDocument holder)
        {
            // The caller owns the JsonDocument; this only checks the shape. A host error can itself be plain text.
            return holder.RootElement.ValueKind == JsonValueKind.Object
                ? holder.RootElement
                : throw Failed(call, $"expected a JSON object, got {text}");
        }

        private static JsonDocument Parse(string call, string text)
        {
            try { return JsonDocument.Parse(text); }
            catch (JsonException) { throw Failed(call, text); }
        }

        private static string? Str(JsonElement obj, string name)
            => obj.TryGetProperty(name, out var v) && v.ValueKind == JsonValueKind.String ? v.GetString() : null;

        /// <summary><c>await_all_children</c>: an empty answer is no children, as Python's <c>if not results_json: return []</c>.</summary>
        internal static IReadOnlyList<ChildResult> ParseAll(string json)
        {
            if (json.Length == 0) return Array.Empty<ChildResult>();
            using var doc = Parse("await_all_children", json);
            if (doc.RootElement.ValueKind != JsonValueKind.Array)
                throw Failed("await_all_children", $"expected a JSON array, got {json}");
            var list = new List<ChildResult>();
            foreach (var item in doc.RootElement.EnumerateArray())
            {
                if (item.ValueKind != JsonValueKind.Object || Str(item, "run_id") is not { } id)
                    throw Failed("await_all_children", $"a result without a run_id: {item.GetRawText()}");
                list.Add(new ChildResult(id, Str(item, "result") ?? "", Str(item, "error")));
            }
            return list;
        }

        /// <summary>
        /// <c>await_any_child</c>: an empty answer is an ERROR, not "none won", since the host writes the winner here; a document
        /// with an <c>error</c> and NO <c>run_id</c> is the host refusing the call, while a WINNER that failed carries both.
        /// </summary>
        internal static ChildResult ParseAny(string json)
        {
            if (json.Length == 0)
                throw Failed("await_any_child", "the host returned nothing. It writes the winning child's result here, so an empty " +
                    "response means no child was awaited rather than that none won");
            using var doc = Parse("await_any_child", json);
            var obj = Object("await_any_child", json, doc);
            var id = Str(obj, "run_id");
            if (id is null && Str(obj, "error") is { Length: > 0 } hostError)
                throw Failed("await_any_child", hostError);
            return new ChildResult(id ?? "", Str(obj, "result") ?? "", Str(obj, "error"));
        }

        /// <summary>
        /// <c>poll_child</c>: no answer, or a document whose status is <c>running</c>, is "not finished" (the host reports a running
        /// child as a status document, so "not finished" is read from the payload, not inferred from emptiness). A finished child's
        /// document is <c>{"status","result","error"}</c> with no run_id, so the id comes from the caller, and an empty error is none.
        /// </summary>
        internal static ChildResult? ParsePoll(string runId, string json)
        {
            if (json.Length == 0) return null;
            using var doc = Parse("poll_child", json);
            var obj = Object("poll_child", json, doc);
            if (Str(obj, "status") == "running") return null;
            var error = Str(obj, "error");
            return new ChildResult(runId, Str(obj, "result") ?? "", string.IsNullOrEmpty(error) ? null : error);
        }
    }
}
