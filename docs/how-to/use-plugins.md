# How to use plugins from workflows

## Overview

Plugins are external services that workflows call via `h.PluginCall(pluginName, functionName, inputJSON)`. Unlike built-in `DurableCall` operations that target a service+operation pair registered with the worker, plugins are independently versioned WASM modules, designed so that installing, updating or deprecating one does not require restarting the worker.

> That is the design, and it is why the resolution path reads the database per
> call. **It is not yet true end to end**: the *installing* half is unwired, so
> a community plugin can be installed but not run. Everything this guide teaches
> uses the plugins bundled into the binary, which need no install at all -- see
> [Installing community plugins](#installing-community-plugins).

Plugins a workflow is most likely to call:

| Plugin name         | Purpose            | Functions                        |
|---------------------|--------------------|----------------------------------|
| `llm`               | LLM chat completion| `chat`                           |
| `slack-notify`      | Slack messaging    | `send_message`                   |
| `pagerduty-alert`   | Incident management| `trigger_incident`               |
| `webhook-ingest`    | Webhook sourcing   | `await_webhook`                  |

> This table used to be headed *"Available built-in plugins"*, which reads as
> the complete set. It is four of them: the worker links **22**. What the four
> have in common is that this guide uses them, not that they are all there are.
> For the set the binary in front of you actually carries, ask it --
> `cleat-worker --list-plugins` -- because that is a build-time fact and a number
> written here would be a census that drifts.

## Configuring plugins

Plugins are configured via a JSON file passed to the worker with the `--plugin-config` flag:

```bash
cleat-worker --db "$CLEAT_DATABASE_URL" --plugin-config ./plugins.json
```

The config file supports environment variable substitution with `${VAR}` syntax:

```json
{
  "llm": {
    "provider": "anthropic",
    "api_key": "${ANTHROPIC_API_KEY}",
    "default_model": "claude-sonnet-4-20250514"
  },
  "slack-notify": {
    "bot_token": "${SLACK_BOT_TOKEN}",
    "default_channel": "#workflow-alerts"
  },
  "pagerduty-alert": {
    "routing_key": "${PD_ROUTING_KEY}",
    "severity": "warning"
  },
  "webhook-ingest": {
    "base_url": "https://webhooks.example.com",
    "default_timeout_secs": 300
  }
}
```

To switch LLM providers, change the `provider` field:

```json
{
  "llm": {
    "provider": "openai",
    "api_key": "${OPENAI_API_KEY}",
    "default_model": "gpt-4o"
  }
}
```

The worker picks up the config at startup.

> Corrected (cleat#3027): this said *"a subset of plugins (like `llm`) are
> bundled ... others (like `slack-notify`) are installed via `cleat plugin
> install`"*, and the example chosen for the second half is bundled. Measured on
> a tree build, `cleat-worker --list-plugins` reports 22 linked plugins and
> `slack-notify` is one of them -- along with all four in the table above. So
> nothing in this guide requires an install.

## Calling a plugin

Use `h.PluginCall(pluginName, functionName, inputJSON)` from within any workflow entry point. The call is durable: it is recorded in the event history and replayed during recovery.

```go
func SummarizeOrder(h cleat.HostCalls, input string) error {
    var req struct {
        OrderID string `json:"order_id"`
        Details string `json:"order_details"`
        Prompt  string `json:"prompt"`
    }
    if err := json.Unmarshal([]byte(input), &req); err != nil {
        return fmt.Errorf("invalid input: %w", err)
    }

    chatInput := map[string]interface{}{
        "model": "claude-sonnet-4-20250514",
        "messages": []map[string]string{
            {"role": "system", "content": "You are an order processing assistant. Summarize orders concisely."},
            {"role": "user",   "content": req.Prompt},
        },
        "max_tokens": 500,
    }
    chatJSON, _ := json.Marshal(chatInput)

    resp, err := h.PluginCall("llm", "chat", string(chatJSON))
    if err != nil {
        return fmt.Errorf("LLM call failed: %w", err)
    }

    var chatResp struct {
        Content string `json:"content"`
        Model   string `json:"model"`
        Usage   struct {
            InputTokens  int `json:"input_tokens"`
            OutputTokens int `json:"output_tokens"`
        } `json:"usage"`
    }
    if err := json.Unmarshal([]byte(resp), &chatResp); err != nil {
        return fmt.Errorf("failed to parse LLM response: %w", err)
    }

    h.SetQueryState("summary", chatResp.Content)
    return nil
}
```

The JSON input format varies by plugin. Each plugin's manifest documents the expected request schema.

## Handling plugin errors

Plugins can fail for many reasons: the downstream API is down, rate limits are exceeded, or the input is invalid. Always check the error return and handle failures gracefully.

```go
func NotifyOnSlack(h cleat.HostCalls, input string) error {
    resp, err := h.PluginCall("slack-notify", "send_message", input)
    if err != nil {
        // No err.Error(): a call through an interface is unresolvable
        // dispatch to the analyzer (E008), and error is an interface.
        h.DurableLog("slack notification failed")

        // Optionally fall back to a different channel or method.
        fallbackInput := strings.Replace(input, "#workflow-alerts", "#ops-logs", 1)
        resp, err = h.PluginCall("slack-notify", "send_message", fallbackInput)
        if err != nil {
            // Last resort: log and continue. The error's text isn't lost by
            // dropping err.Error() here -- it would still be in the error if
            // this returned it instead of nil; it's discarded deliberately
            // below because a failed Slack notification is non-critical.
            h.DurableLog("fallback slack notification also failed")
            return nil // non-critical failure
        }
    }
    _ = resp
    return nil
}
```

For critical plugin calls, use the retry policy mechanism:

```go
policy := cleat.RetryPolicy{
    MaxAttempts:        3,
    InitialInterval:    1 * time.Second,
    BackoffCoefficient: 2.0,
}

resp, err := h.DurableCallWithOptions(
    cleat.CallOptions{Retry: &policy},
    "payments", "Charge", requestJSON,
)
```

Note that `DurableCallWithOptions` operates on plugin-backed service+operation pairs when the worker has a "payments" service registered. Plugin calls directly via `h.PluginCall` do not yet have built-in retry; implement retry logic in your workflow code if needed.

## Common plugin patterns

### LLM: chat completion

```go
func GenerateResponse(h cleat.HostCalls, input string) error {
    chatInput := map[string]interface{}{
        "model": "claude-sonnet-4-20250514",
        "messages": []map[string]string{
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user",   "content": input},
        },
        "max_tokens": 1000,
        "temperature": 0.7,
    }
    chatJSON, _ := json.Marshal(chatInput)

    resp, err := h.PluginCall("llm", "chat", string(chatJSON))
    if err != nil {
        return err
    }

    // Store the result as queryable state.
    h.SetQueryState("llm_response", resp)
    return nil
}
```

### Slack: send message with Block Kit

```go
func SendAlert(h cleat.HostCalls, input string) error {
    msgInput := map[string]interface{}{
        "channel": "#workflow-alerts",
        "blocks": []map[string]interface{}{
            {
                "type": "header",
                "text": map[string]string{"type": "plain_text", "text": "Workflow Alert"},
            },
            {
                "type": "section",
                "text": map[string]string{"type": "mrkdwn", "text": input},
            },
        },
    }
    msgJSON, _ := json.Marshal(msgInput)

    _, err := h.PluginCall("slack-notify", "send_message", string(msgJSON))
    return err
}
```

### PagerDuty: trigger incident on critical severity

```go
func EscalateToOnCall(h cleat.HostCalls, input string) error {
    var failure struct {
        WorkflowName string `json:"workflow_name"`
        InstanceID   string `json:"instance_id"`
        ErrorMessage string `json:"error_message"`
    }
    json.Unmarshal([]byte(input), &failure)

    incidentInput := map[string]interface{}{
        "title":        "Workflow failure: " + failure.WorkflowName,
        "service_name": "cleat-workflows",
        "dedup_key":    failure.InstanceID,
        "severity":     "critical",
        "body":         failure.ErrorMessage,
    }
    incidentJSON, _ := json.Marshal(incidentInput)

    _, err := h.PluginCall("pagerduty-alert", "trigger_incident", string(incidentJSON))
    return err
}
```

### Webhooks: register source and await event

```go
func AwaitWebhookEvent(h cleat.HostCalls, input string) error {
    registerInput := map[string]interface{}{
        "source_name":  "payment-webhook",
        "event_types":  []string{"payment.confirmed", "payment.failed"},
        "timeout_secs": 300,
    }
    registerJSON, _ := json.Marshal(registerInput)

    resp, err := h.PluginCall("webhook-ingest", "await_webhook", string(registerJSON))
    if err != nil {
        return fmt.Errorf("webhook registration failed: %w", err)
    }

    // resp contains the webhook payload once received.
    h.SetQueryState("webhook_result", resp)
    return nil
}
```

## Plugin response formats

Each plugin returns JSON. The exact schema is documented in the plugin's manifest, but common patterns are shown below.

### LLM response

```json
{
  "content": "Your order #12345 has been summarized:\n- Item: Widget A, Qty: 2\n- Item: Widget B, Qty: 1\n- Total: $49.99",
  "model": "claude-sonnet-4-20250514",
  "usage": {
    "input_tokens": 145,
    "output_tokens": 42
  },
  "finish_reason": "stop"
}
```

### Slack response

```json
{
  "ok": true,
  "channel": "C0123456789",
  "ts": "1712345678.000100",
  "message": {
    "text": "Workflow Alert",
    "type": "message"
  }
}
```

### PagerDuty response

```json
{
  "incident_id": "abc123",
  "incident_url": "https://example.pagerduty.com/incidents/abc123",
  "status": "triggered",
  "urgency": "high"
}
```

## Installing community plugins

Plugins from the community index are installed with the `cleat plugin` CLI:

```bash
# Search available plugins.
cleat plugin list --available

# Install a specific version.
cleat plugin install acme/salesforce@^1.0.0

# List installed plugins.
cleat plugin list

# Check for updates.
cleat plugin update --all
```

Installed plugins are recorded in the `plugin_defs` database table.

> **They are not executed yet** (found by cleat#3027's audit). The install path
> is real -- `cleat plugin install` resolves a definition and stores it, and
> `cleat plugin list` reads it back -- but the worker does not load a plugin
> from that table: `engine.PluginLoader.LoadPlugin` has no caller outside tests,
> and `cmd/cleat-worker` constructs no loader.
>
> You can check this without taking anyone's word for it -- `cleat plugin list`
> says the same thing about its own output:
>
> ```
> NOTE: STATUS above reflects plugin_defs only. No cleat-worker today loads an
> installed WASM plugin (IMPROVEMENT-PLAN 3.315); none of the rows above run.
> ```
>
> So the sentence this section used to end with contradicted the output of a
> command the page had already taught.
>
> The sentence that used to close this section said installed plugins are
> *"loaded by `cleat-worker` at startup"*. It was wrong twice over: it claimed a
> capability that is not wired, and it contradicted this page's own Overview,
> where installing without restarting the worker is given as a property. **The
> Overview is the side that matches the design** -- `PluginLoader.ResolvePlugin`
> queries `plugin_defs` live, per call -- but that is moot while nothing calls
> it.
>
> What works is everything above: **bundled** plugins are linked into the binary
> and callable, which is every plugin in this guide. The gap is only for plugins
> that are not bundled. See [the plugin
> system](../explanation/plugin-system.md) for the mechanism, and
> IMPROVEMENT-PLAN §3.315 for the repair.

## Next steps

- See the [plugin developer guide](../contributor/plugins/plugin-developer-guide.md) for writing custom plugins
- See the [plugin security guide](../contributor/plugins/plugin-security.md) for security considerations
- See the [common patterns guide](common-patterns.md) for combining plugins with Saga, signals, and child workflows
