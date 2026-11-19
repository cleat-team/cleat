using System;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;

namespace Cleat.Runtime;

/// <summary>
/// The JSON conversions the generated entry adapters call. Nothing here uses reflection-based
/// <c>System.Text.Json</c>, which throws <c>JsonSerializerIsReflectionDisabled</c> under NativeAOT: the
/// built-in forms use <c>JsonElement</c>/<c>JsonNode</c>/<c>JsonValue</c>, and a typed entry goes through the
/// caller's own source-generated <see cref="JsonSerializerContext"/>.
/// </summary>
internal static class EntryJson
{
    /// <summary>
    /// The default encoder escapes <c>"</c> as <c>\u0022</c> and every non-ASCII character as <c>\uXXXX</c>: valid
    /// JSON, but a result nobody can read in the event history. The relaxed encoder writes what JSON allows
    /// literally. The output goes to the engine and the history, never into HTML, which is the only place the
    /// name's warning applies.
    /// </summary>
    internal static readonly JavaScriptEncoder Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping;

    private static readonly JsonSerializerOptions Options = new() { Encoder = Encoder };

    internal static string Element(JsonElement e) => e.GetRawText();
    internal static string Node(JsonNode? n) => n is null ? "null" : n.ToJsonString(Options);
    /// <summary>
    /// A JSON array of strings, written by hand like everything here (no reflection). Host calls take lists of names
    /// this way: signal names, child run ids.
    /// </summary>
    internal static string StringArray(System.Collections.Generic.IReadOnlyList<string> items)
    {
        using var ms = new System.IO.MemoryStream();
        using (var w = new Utf8JsonWriter(ms, new JsonWriterOptions { Encoder = Encoder }))
        {
            w.WriteStartArray();
            foreach (var item in items) w.WriteStringValue(item);
            w.WriteEndArray();
        }
        return System.Text.Encoding.UTF8.GetString(ms.ToArray());
    }

    internal static string String(string? s) => s is null ? "null" : JsonValue.Create(s)!.ToJsonString(Options);
    internal static string Bool(bool b) => b ? "true" : "false";
    internal static string Int(int v) => JsonValue.Create(v).ToJsonString(Options);
    internal static string Long(long v) => JsonValue.Create(v).ToJsonString(Options);
    internal static string Double(double v) => JsonValue.Create(v).ToJsonString(Options);
    internal const string Null = "null";

    internal static JsonNode? ToNode(JsonElement e) => JsonNode.Parse(e.GetRawText());

    internal static T Read<T>(JsonElement input, JsonSerializerContext ctx)
        => JsonSerializer.Deserialize(input, Info<T>(ctx))!;

    /// <summary>Throws if <paramref name="ctx"/> does not declare <typeparamref name="T"/>, without reading anything.</summary>
    internal static void Require<T>(JsonSerializerContext ctx) => _ = Info<T>(ctx);

    internal static T ReadText<T>(string json, JsonSerializerContext ctx)
        => JsonSerializer.Deserialize(json, Info<T>(ctx))!;

    internal static string Write<T>(T value, JsonSerializerContext ctx)
        => JsonSerializer.Serialize(value, Info<T>(ctx));

    private static JsonTypeInfo<T> Info<T>(JsonSerializerContext ctx)
        => ctx.GetTypeInfo(typeof(T)) as JsonTypeInfo<T>
           ?? throw new InvalidOperationException(
               $"{ctx.GetType().Name} does not declare [JsonSerializable(typeof({typeof(T).Name}))]; " +
               "add it, or use JsonElement/JsonNode for this entry");
}
