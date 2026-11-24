using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Example;

public static class Orders
{
    /// <summary>
    /// Charge a customer, wait, and record the order. Every host call is DURABLE: the first run records it in the workflow's history, and a
    /// replay (after a worker restart, or when the run resumes after the sleep) answers from that history instead of calling the service again.
    /// </summary>
    [CleatEntry("PlaceOrder")]
    public static JsonNode PlaceOrder(HostCalls h, JsonElement input)
    {
        // Registered first, so it runs LAST; a workflow that fails, or is terminated, still runs its defers.
        h.Defer("release-hold", () => h.Call("inventory", "ReleaseHold", input.GetRawText()));

        var charge = h.Call("payments", "Charge", input.GetRawText());   // durable: recorded, replayed from history
        h.Sleep(TimeSpan.FromSeconds(5));                                  // durable: the run suspends here and resumes later
        var recorded = h.Call("orders", "Record", charge);                 // durable: only called once, however often the run is replayed

        return new JsonObject
        {
            ["charge"] = JsonNode.Parse(charge),
            ["order"] = JsonNode.Parse(recorded),
        };
    }
}
