using System.Text.Json;
using System.Text.Json.Serialization;
using Cleat;

namespace Cleat.FixtureTyped;

#if !NONE
// One record in and out: the smallest typed entry.
public record Address(string Street, string City);
public record Order(string Id, int Qty, decimal Amount, DateTimeOffset At, List<string> Tags, Address Ship);

#if MANY
// Nine more, with the shapes real workflows have: nesting, collections, a dictionary, an enum, nullable members,
// a Guid, a raw JsonElement member, a recursive type. The measurement asks whether the cost of source-generated
// JSON grows with the number of types or is mostly fixed (cleat#4080).
public enum Status { New, Paid, Shipped, Refunded }
public record Customer(Guid Id, string Name, Address Home, List<Address> Others);
public record LineItem(string Sku, int Qty, decimal Price);
public record Invoice(Customer Who, List<LineItem> Items, DateTimeOffset Due, Dictionary<string, string> Meta);
public record Shipment(Status State, DateTime? Left, double Weight, int[] Boxes);
public record Refund(string OrderId, decimal Amount, string? Reason);
public record Event(string Kind, System.Text.Json.JsonElement Payload);
public record Node(string Name, List<Node> Children);
public record Settings(Dictionary<string, int> Limits, bool? Enabled, string[] Regions);
public record Receipt(string OrderId, Status State, long Total);
#endif

[JsonSourceGenerationOptions(PropertyNamingPolicy = JsonKnownNamingPolicy.CamelCase)]
[JsonSerializable(typeof(Order))]
#if MANY
[JsonSerializable(typeof(Invoice))]
[JsonSerializable(typeof(Shipment))]
[JsonSerializable(typeof(Refund))]
[JsonSerializable(typeof(Event))]
[JsonSerializable(typeof(Node))]
[JsonSerializable(typeof(Settings))]
[JsonSerializable(typeof(Receipt))]
#endif
public partial class Json : JsonSerializerContext { }

#endif

public static class Workflows
{
#if NONE
    /// <summary>The baseline for the size measurement: the same entry name with no typed JSON, so no serialiser is linked.</summary>
    [CleatEntry("Typed")]
    public static JsonElement Typed(HostCalls h, JsonElement input) => input;
#else
    /// <summary>A typed entry: the engine's JSON becomes an Order, and the returned Order is serialised back.</summary>
    [CleatEntry("Typed", JsonContext = typeof(Json))]
    public static Order Typed(HostCalls h, Order order)
        => order with { Qty = order.Qty * 2, Tags = new List<string>(order.Tags) { "seen" } };
#endif
}
