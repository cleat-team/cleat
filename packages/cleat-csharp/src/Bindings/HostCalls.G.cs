using System;
using System.Collections.Generic;
using System.IO;
using System.Text.Json;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

/// <summary>What <see cref="HostCalls.Fetch"/> got back.</summary>
/// <param name="Status">The HTTP status, or <c>0</c> when the host's reply carried none. Never defaulted to 200: a reply the SDK cannot read
/// as a status is not a success the host reported (python-sdk <c>_decode_fetch_response</c>, cleat#3413).</param>
/// <param name="Body">The response body as text; empty when the reply had none.</param>
/// <param name="Headers">The response headers the host reported, as text; empty when none.</param>
public sealed record FetchResponse(int Status, string Body, IReadOnlyDictionary<string, string> Headers);

// Group G of cleat#4080's host-call coverage: HTTP, recording a computed value, and starting an independent run.
// fetch, side-effect, durable-run-detached.
public sealed partial class HostCalls
{
    // ---- fetch ------------------------------------------------------------------------------------------------

    /// <summary>
    /// Makes a durable HTTP request through the host. The response is recorded, so a replay returns the recorded response and sends nothing.
    /// </summary>
    /// <remarks>
    /// A worker serves this only when it was started with <c>--enable-workflow-fetch</c> (an embedding host with <c>engine.WithFetcher</c>);
    /// otherwise every call fails with a message that names the flag. The host's egress policy decides which URLs are reachable.
    /// A non-2xx status is NOT an exception: it is a <see cref="FetchResponse"/> with that status, and what to do about it is the workflow's.
    /// </remarks>
    /// <param name="url">The URL.</param>
    /// <param name="method">An HTTP method name (letters, digits, <c>.</c>, <c>_</c>, <c>-</c>); anything else is refused by the host.</param>
    /// <param name="headers">Request headers, or <c>null</c> for none.</param>
    /// <param name="body">The request body, or <c>""</c>.</param>
    /// <exception cref="CleatCallException">The host refused or could not make the request (no fetcher configured, egress denied, a transport
    /// failure, a reply that is not JSON). The message is the host's.</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow (a defer segment's body may not make new requests).</exception>
    public FetchResponse Fetch(string url, string method = "GET", IReadOnlyDictionary<string, string>? headers = null, string body = "")
    {
        ArgumentNullException.ThrowIfNull(url);
        ArgumentNullException.ThrowIfNull(method);
        ArgumentNullException.ThrowIfNull(body);
        string reply;
        try
        {
            reply = IDurableFetchImports.Fetch(method, url, EncodeHeaders(headers), body);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("http", "fetch", f);
        }
        return DecodeFetch(reply);
    }

    private static string EncodeHeaders(IReadOnlyDictionary<string, string>? headers)
    {
        using var ms = new MemoryStream();
        using (var w = new Utf8JsonWriter(ms, new JsonWriterOptions { Encoder = EntryJson.Encoder }))
        {
            w.WriteStartObject();
            if (headers is not null)
                foreach (var (k, v) in headers)
                    w.WriteString(k, v ?? throw new ArgumentException($"header '{k}' has a null value", nameof(headers)));
            w.WriteEndObject();
        }
        return System.Text.Encoding.UTF8.GetString(ms.ToArray());
    }

    /// <summary>
    /// Reads the host's <c>{"status":N,"headers":{...},"body":"..."}</c>. A host an embedder wired up with its own fetcher may say
    /// <c>status_code</c>, so that key is the fallback. A missing or non-numeric status is 0, never 200.
    /// </summary>
    internal static FetchResponse DecodeFetch(string reply)
    {
        try
        {
            using var doc = JsonDocument.Parse(reply);
            var root = doc.RootElement;
            if (root.ValueKind != JsonValueKind.Object)
                throw new JsonException("not an object");
            var status = 0;
            foreach (var key in new[] { "status", "status_code" })
                if (root.TryGetProperty(key, out var s) && s.ValueKind == JsonValueKind.Number && s.TryGetInt32(out var n))
                {
                    status = n;
                    break;
                }
            var body = root.TryGetProperty("body", out var b) && b.ValueKind == JsonValueKind.String ? b.GetString() ?? "" : "";
            var headers = new Dictionary<string, string>();
            if (root.TryGetProperty("headers", out var h) && h.ValueKind == JsonValueKind.Object)
                foreach (var p in h.EnumerateObject())
                    headers[p.Name] = p.Value.ValueKind == JsonValueKind.String ? p.Value.GetString() ?? "" : p.Value.GetRawText();
            return new FetchResponse(status, body, headers);
        }
        catch (JsonException)
        {
            throw new CleatCallException("http", "fetch", "the fetch reply is not a JSON object: " + (reply.Length > 200 ? reply[..200] + "..." : reply), 0);
        }
    }

    // ---- side-effect ------------------------------------------------------------------------------------------

    /// <summary>
    /// Records <paramref name="value"/>, a result you computed, in the workflow's history, and returns what the host recorded.
    /// </summary>
    /// <remarks>
    /// <b>This does not freeze a non-deterministic value.</b> The host's replay compares the value you pass with the one it recorded and
    /// fails the replay with a divergence error when they differ; it does not hand you the recorded value in place of a new one. So a value
    /// computed from a clock or a random source is a replay failure waiting to happen. Use it for a value you can recompute the same way
    /// each time and want checked. (This differs from the Python docstring, which says the call lets a workflow "capture a random value";
    /// see cleat#4113.)
    /// </remarks>
    /// <exception cref="CleatCallException">On replay, the value differs from the one recorded (the message says so), or the history has a
    /// different event at this step.</exception>
    public string SideEffect(string value)
    {
        ArgumentNullException.ThrowIfNull(value);
        try
        {
            return IDurableExtendedLifecycleImports.SideEffect(value);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("lifecycle", "side_effect", f);
        }
    }

    // ---- run-detached -----------------------------------------------------------------------------------------

    /// <summary>
    /// Starts the workflow definition <paramref name="name"/> as an independent run: not a child, not awaited, and not affected when this
    /// workflow completes or is cancelled. Nothing comes back, not even the run id.
    /// </summary>
    /// <remarks>
    /// <b>A start that failed is not reported.</b> The host records the call and answers success even when its child-workflow store refused
    /// the start or there is no store at all, and the run id it records is then a made-up one (cleat#4114). Check for the run by other means
    /// when it matters.
    /// </remarks>
    /// <param name="name">The definition's name: letters, digits, <c>.</c>, <c>_</c>, <c>-</c>.</param>
    /// <param name="inputJson">The new run's input as JSON.</param>
    /// <exception cref="CleatCallException">The host refused the name, or this call is not the one the history recorded at this step (a replay
    /// divergence): the base type, <see cref="CleatCallException.Code"/> 0.</exception>
    public void RunDetached(string name, string inputJson)
    {
        ArgumentNullException.ThrowIfNull(name);
        ArgumentNullException.ThrowIfNull(inputJson);
        Packed.CheckHostResult(IDurableExtendedLifecycleImports.DurableRunDetached(name, inputJson), "", "run-detached", "run-detached");
    }
}
