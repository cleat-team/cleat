using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Text.Json.Serialization;
using CleatWorkflowWorld;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;
using Cleat.Runtime;

namespace Cleat;

/// <summary>One chunk of a streamed plugin response, as <see cref="HostCalls.PluginCallStreaming"/> returns it.</summary>
/// <param name="Index">The chunk's position in the stream, from 0.</param>
/// <param name="Content">The chunk's text.</param>
/// <param name="Finish"><c>true</c> on the last chunk.</param>
public sealed record StreamChunk(int Index, string Content, bool Finish);

// Group H of cleat#4080's host-call coverage: calling plugins.
// plugin-call, plugin-call-streaming.
public sealed partial class HostCalls
{
    /// <summary>
    /// Calls a function of a host plugin and returns its response JSON. The call is recorded, so a replay returns the recorded response; whether
    /// a replay may run the plugin again is the plugin's own registration, not the caller's.
    /// </summary>
    /// <param name="pluginName">The plugin's name: letters, digits, <c>.</c>, <c>_</c>, <c>-</c> (the host refuses anything else).</param>
    /// <param name="functionName">The function's name, same rule.</param>
    /// <param name="inputJson">The input as JSON.</param>
    /// <exception cref="CleatCallException">The plugin is not registered, refused the call, or failed. Here the host classifies the failure
    /// (<see cref="CleatCallException.Code"/>): a function that is not registered or a plugin that is unhealthy is a different class from a
    /// plugin that ran and returned an error.</exception>
    /// <exception cref="CleatSuspendException">The host stopped the workflow (a defer segment's body is allowed to call plugins, its workflow
    /// body is not).</exception>
    public string PluginCall(string pluginName, string functionName, string inputJson)
    {
        ArgumentNullException.ThrowIfNull(pluginName);
        ArgumentNullException.ThrowIfNull(functionName);
        ArgumentNullException.ThrowIfNull(inputJson);
        try
        {
            return IPluginImports.PluginCall(pluginName, functionName, inputJson);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("plugin:" + pluginName, functionName, f);
        }
    }

    /// <summary>
    /// <see cref="PluginCall(string, string, string)"/> with typed request and response. NativeAOT has no reflection-based JSON, so the
    /// types go through <paramref name="context"/>, a <c>JsonSerializerContext</c> of yours that declares
    /// <c>[JsonSerializable(typeof(TRequest))]</c> and <c>[JsonSerializable(typeof(TResponse))]</c>. Using a type the context does not declare
    /// is an <see cref="InvalidOperationException"/> that names the context and the type, before the plugin is called.
    /// </summary>
    /// <remarks>
    /// <b>What "a response of the right type" means is what <paramref name="context"/>'s serializer says, and no more.</b> Member names are
    /// matched by the context's own naming policy, <b>case-sensitively</b>, so a plugin that replies in <c>snake_case</c> (the cleat plugins do,
    /// for example <c>config_id</c>) needs a context declared with
    /// <c>[JsonSourceGenerationOptions(PropertyNamingPolicy = JsonKnownNamingPolicy.SnakeCaseLower)]</c>; with the default names a reply whose
    /// members do not match deserializes <b>without error</b> to a response whose members are their defaults, and so does a reply of <c>{}</c>.
    /// Members the type does not declare are ignored. What IS refused: a reply that is not JSON, one whose member has the wrong JSON type, and a
    /// reply of <c>null</c>, which cannot be a <typeparamref name="TResponse"/> and would otherwise surface as a
    /// <see cref="NullReferenceException"/> at the caller's first use of it.
    /// </remarks>
    /// <exception cref="CleatCallException">As <see cref="PluginCall(string, string, string)"/>, or the plugin's response is not a
    /// <typeparamref name="TResponse"/>: it is not JSON, a member has the wrong type, or it is <c>null</c> (the message says so and carries the
    /// serializer's, <see cref="CleatCallException.Code"/> 0).</exception>
    public TResponse PluginCall<TRequest, TResponse>(string pluginName, string functionName, TRequest request, JsonSerializerContext context)
    {
        ArgumentNullException.ThrowIfNull(context);
        var input = EntryJson.Write(request, context);
        // Resolve the response type BEFORE calling: a missing declaration must fail before a side effect has been recorded.
        EntryJson.Require<TResponse>(context);
        var reply = PluginCall(pluginName, functionName, input);
        try
        {
            return EntryJson.ReadText<TResponse>(reply, context)
                ?? throw new JsonException("the reply is null");
        }
        catch (JsonException e)
        {
            throw new CleatCallException("plugin:" + pluginName, functionName,
                $"the plugin's response is not a {typeof(TResponse).Name}: {e.Message}", 0);
        }
    }

    /// <summary>
    /// Calls a streaming plugin function and returns the chunks it produced, in order. The host collects the whole stream before it answers,
    /// so this returns when the stream has ended: it is not incremental. A stream with no chunks is an empty list.
    /// </summary>
    /// <remarks>
    /// <b>A stream of exactly one finished chunk used to fail on replay</b> (cleat#4117, measured, fixed in the host): replay read one chunk
    /// with <c>Finish</c> set as its own stream-level error record, so a plugin that answered with a single final chunk succeeded on the first
    /// run and raised a <see cref="CleatCallException"/> carrying the chunk's text when the run was replayed. A chunk recorded by a host with
    /// the fix replays as the success it was. A run whose single-chunk stream was recorded by an older host still replays as the failure
    /// (nothing recorded then tells the two apart), so against such a host have the plugin end a stream with a chunk after at least one other,
    /// or use <see cref="PluginCall(string, string, string)"/> for one-shot answers.
    /// </remarks>
    /// <exception cref="CleatCallException">The stream function is not registered, was refused, or failed (classified by the host).</exception>
    public IReadOnlyList<StreamChunk> PluginCallStreaming(string pluginName, string functionName, string inputJson)
    {
        ArgumentNullException.ThrowIfNull(pluginName);
        ArgumentNullException.ThrowIfNull(functionName);
        ArgumentNullException.ThrowIfNull(inputJson);
        string reply;
        try
        {
            reply = IPluginImports.PluginCallStreaming(pluginName, functionName, inputJson);
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            throw Translate("plugin:" + pluginName, functionName, f);
        }
        return DecodeChunks(pluginName, functionName, reply);
    }

    /// <summary>The host answers <c>[{"i":0,"c":"text","f":false},…]</c>, and <c>null</c> for a stream that produced nothing.</summary>
    internal static IReadOnlyList<StreamChunk> DecodeChunks(string pluginName, string functionName, string reply)
    {
        if (reply.Length == 0)
            return Array.Empty<StreamChunk>();
        try
        {
            using var doc = JsonDocument.Parse(reply);
            if (doc.RootElement.ValueKind == JsonValueKind.Null)
                return Array.Empty<StreamChunk>();
            if (doc.RootElement.ValueKind != JsonValueKind.Array)
                throw new JsonException("not an array");
            var chunks = new List<StreamChunk>();
            foreach (var e in doc.RootElement.EnumerateArray())
                chunks.Add(new StreamChunk(e.GetProperty("i").GetInt32(), e.GetProperty("c").GetString() ?? "", e.GetProperty("f").GetBoolean()));
            return chunks;
        }
        catch (Exception ex) when (ex is JsonException or KeyNotFoundException or InvalidOperationException or FormatException)
        {
            throw new CleatCallException("plugin:" + pluginName, functionName,
                "the stream reply is not a list of chunks: " + (reply.Length > 200 ? reply[..200] + "..." : reply), 0);
        }
    }
}
