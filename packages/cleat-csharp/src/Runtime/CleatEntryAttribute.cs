using System;

namespace Cleat;

/// <summary>
/// Marks a <c>public static</c> method as a Cleat workflow entry point.
/// </summary>
/// <remarks>
/// <para>
/// The first parameter must be <see cref="HostCalls"/>. An optional second parameter receives the workflow
/// input: <see cref="System.Text.Json.JsonElement"/>, <see cref="System.Text.Json.Nodes.JsonNode"/> or a
/// <see cref="string"/> holding the raw input JSON need nothing more. Any other type is a typed entry and
/// needs <see cref="JsonContext"/>.
/// </para>
/// <para>
/// The method returns <c>T</c>, <c>Task&lt;T&gt;</c>, <c>ValueTask&lt;T&gt;</c> or nothing (the result is
/// <c>null</c>). <c>T</c> follows the same rule as the input: <c>JsonElement</c>, <c>JsonNode</c>,
/// <c>string</c> (serialised as a JSON string), <c>bool</c>, <c>int</c>, <c>long</c> and <c>double</c> need
/// nothing more, and anything else needs <see cref="JsonContext"/>.
/// </para>
/// <para>
/// <b>What a typed entry costs.</b> The artifact grows by about 2 MB and the first execution on a worker is
/// slower, because <c>System.Text.Json</c>'s source-generated serialiser is linked in (measured, cleat#4077;
/// the exact figures are in packages/cleat-csharp/README.md). Reflection-based JSON is not an alternative: it
/// throws <c>JsonSerializerIsReflectionDisabled</c> under NativeAOT.
/// </para>
/// </remarks>
[AttributeUsage(AttributeTargets.Method, AllowMultiple = false, Inherited = false)]
public sealed class CleatEntryAttribute : Attribute
{
    /// <summary>Registers the method under its own name.</summary>
    public CleatEntryAttribute() { }

    /// <summary>Registers the method under <paramref name="name"/>.</summary>
    public CleatEntryAttribute(string name) { Name = name; }

    /// <summary>The entry point's name, or <c>null</c> for the method's own name.</summary>
    public string? Name { get; }

    /// <summary>
    /// A <c>System.Text.Json.Serialization.JsonSerializerContext</c> subclass, declared by the caller with
    /// <c>[JsonSerializable]</c> for every type this entry reads or returns. Required exactly when a
    /// signature uses a type outside the list above.
    /// </summary>
    public Type? JsonContext { get; set; }
}
