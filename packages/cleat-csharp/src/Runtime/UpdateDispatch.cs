using System;
using System.Collections.Generic;
using System.Text.Json;

namespace Cleat.Runtime;

/// <summary>What one poll of the host's update queue handed back.</summary>
internal enum UpdateEnvelopeKind
{
    /// <summary>Nothing pending (the empty string).</summary>
    None,

    /// <summary>
    /// Not an envelope this SDK can act on: not JSON, not an object, or a name or request id that is missing, not a
    /// string, or empty. The host writes the envelope, so this is not a caller error, and the dispatcher stops rather
    /// than loop on a delivery that would decode the same way next time.
    /// </summary>
    Malformed,

    /// <summary>A usable update.</summary>
    Update,
}

/// <summary>
/// Decodes the envelope <c>durable-poll-update</c> returns: <c>{"name", "payload", "request_id"}</c>, all strings
/// (python-sdk <c>dispatch_updates</c> is the reference). <c>request_id</c> is an opaque key the host composes; it is
/// handed back to <c>durable-complete-update</c> exactly as received and never parsed here.
/// </summary>
internal static class UpdateEnvelope
{
    internal static UpdateEnvelopeKind TryDecode(string raw, out string name, out string requestId, out string payload)
    {
        name = requestId = payload = "";
        if (string.IsNullOrEmpty(raw)) return UpdateEnvelopeKind.None;
        JsonDocument doc;
        try { doc = JsonDocument.Parse(raw); }
        catch (JsonException) { return UpdateEnvelopeKind.Malformed; }
        using (doc)
        {
            var root = doc.RootElement;
            if (root.ValueKind != JsonValueKind.Object) return UpdateEnvelopeKind.Malformed;
            // A key lookup alone cannot tell "absent" from "present but not a string", and an empty request id would make
            // complete_update address nothing.
            if (!root.TryGetProperty("name", out var n) || n.ValueKind != JsonValueKind.String) return UpdateEnvelopeKind.Malformed;
            if (!root.TryGetProperty("request_id", out var r) || r.ValueKind != JsonValueKind.String) return UpdateEnvelopeKind.Malformed;
            var nameValue = n.GetString() ?? "";
            var requestValue = r.GetString() ?? "";
            if (nameValue.Length == 0 || requestValue.Length == 0) return UpdateEnvelopeKind.Malformed;
            name = nameValue;
            requestId = requestValue;
            // A payload that is absent or not a string is the empty payload, as Python reads it.
            payload = root.TryGetProperty("payload", out var p) && p.ValueKind == JsonValueKind.String ? p.GetString() ?? "" : "";
            return UpdateEnvelopeKind.Update;
        }
    }
}

/// <summary>
/// The registered update handlers and the loop that serves them (python-sdk <c>dispatch_updates</c> / <c>_run_update</c>).
/// Pure over two delegates, the host's poll and complete, so the whole policy is unit-testable without a host.
/// </summary>
/// <remarks>
/// <para>
/// A handler is code in the guest, so only the guest can run it: an update cannot interrupt a workflow. It is asked for at fixed
/// program positions, not at a moment in time, because replay re-executes the workflow and matches host calls against the
/// recorded history in order (engine/updater.go).
/// </para>
/// <para>
/// EVERY path completes the request: an unregistered name, a validator that refuses and a handler that throws are all answers
/// the caller is owed, and leaving one uncompleted would leave the caller holding a promise nothing settles. The one exception
/// is a STOP: a <see cref="CleatSuspendException"/> from a handler (its own sleep or await suspended) propagates and completes
/// nothing, because the segment is ending and the update is redelivered on the next one. Completing it with a message would
/// answer the caller with an error for work the host has only deferred.
/// </para>
/// </remarks>
internal sealed class UpdateHandlers
{
    private readonly Dictionary<string, (Func<string, string> Handler, Func<string, bool>? Validator)> _handlers = new();
    private bool _dispatching;

    internal void Register(string name, Func<string, string> handler, Func<string, bool>? validator)
        => _handlers[name] = (handler, validator);

    internal bool IsRegistered(string name) => _handlers.ContainsKey(name);

    /// <summary>Serves every update the host has pending, one at a time, until it reports none.</summary>
    /// <param name="poll">The host's <c>durable-poll-update</c>.</param>
    /// <param name="complete">The host's <c>durable-complete-update</c>: request id, result, error.</param>
    internal void Dispatch(Func<string> poll, Action<string, string, string> complete)
    {
        // Reentrancy guard. Every dispatch point is a suspension point, and a handler is ordinary workflow code that may sleep or
        // await, so without this a handler doing either would re-enter and recurse. Nesting would also be wrong if it terminated:
        // the inner dispatch would interleave a second update's events inside the first one's.
        if (_dispatching) return;
        _dispatching = true;
        try
        {
            while (true)
            {
                if (UpdateEnvelope.TryDecode(poll(), out var name, out var requestId, out var payload) != UpdateEnvelopeKind.Update)
                    return;
                Run(name, payload, requestId, complete);
            }
        }
        finally { _dispatching = false; }
    }

    private void Run(string name, string payload, string requestId, Action<string, string, string> complete)
    {
        if (!_handlers.TryGetValue(name, out var entry))
        {
            complete(requestId, "", $"cleat: no update handler registered for '{name}'");
            return;
        }
        string result;
        try
        {
            if (entry.Validator is { } validate && !validate(payload))
            {
                complete(requestId, "", $"update '{name}' failed validation");
                return;
            }
            result = entry.Handler(payload);
        }
        catch (CleatSuspendException) { throw; }
        catch (Exception e)
        {
            // A non-empty error is what makes this a rejection. An exception with an empty message must still reject, or the
            // caller would be told the update SUCCEEDED with an empty result.
            complete(requestId, "", string.IsNullOrEmpty(e.Message) ? e.GetType().Name : e.Message);
            return;
        }
        complete(requestId, result ?? "", "");
    }
}
