using System.Text.Json;

namespace Cleat.Runtime;

/// <summary>
/// The request/reply envelope a signal can carry: <c>{"cleat_reply_to": "&lt;address&gt;", "payload": "&lt;text&gt;"}</c>
/// (python-sdk/cleat_sdk/signal_envelope.py, <c>decode_signal_envelope</c>).
/// </summary>
/// <remarks>
/// The discriminator is the object's SHAPE, not the presence of a string anywhere in the text: exactly the two
/// keys, both string values, a non-empty address. An ordinary payload that merely carries a <c>cleat_reply_to</c>
/// field among others is not mistaken for one. A substring search cannot tell a thing from a mention of the thing.
/// </remarks>
internal static class SignalEnvelope
{
    internal const string ReplyKey = "cleat_reply_to";

    /// <summary>True, with the address and the unwrapped payload, if <paramref name="raw"/> is an envelope.</summary>
    internal static bool TryDecode(string raw, out string replyTo, out string payload)
    {
        replyTo = "";
        payload = "";
        try
        {
            using var doc = JsonDocument.Parse(raw);
            if (doc.RootElement.ValueKind != JsonValueKind.Object) return false;
            string? reply = null, body = null;
            var count = 0;
            foreach (var prop in doc.RootElement.EnumerateObject())
            {
                count++;
                if (prop.Value.ValueKind != JsonValueKind.String) return false;
                if (prop.Name == ReplyKey) reply = prop.Value.GetString();
                else if (prop.Name == "payload") body = prop.Value.GetString();
                else return false;
            }
            // Two properties, both of the two names: a repeated key leaves one name unset and is refused, as a
            // dict with one key is in Python.
            if (count != 2 || reply is null || body is null || reply.Length == 0) return false;
            replyTo = reply;
            payload = body;
            return true;
        }
        catch (JsonException)
        {
            return false;
        }
    }
}
