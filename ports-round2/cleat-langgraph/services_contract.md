# LangGraph Host Service Contract

The Cleat-LangGraph bridge registers a host service (default name: `"langgraph"`)
that Cleat workflows call via `HostCalls.durable_call(service, operation, request)`.

## Operations

### `step`
Execute one node of a registered StateGraph and return the updated state.

**Request:**
```json
{
  "graph_name": "string (required)",
  "state": {"__cleat_langgraph_state__": true, "data": {...}}
}
```

**Response:**
```json
{
  "state": {"__cleat_langgraph_state__": true, "data": {...}},
  "next_node": "string | null",
  "node_executed": "string | null",
  "done": "boolean"
}
```

### `route`
Determine the next node to execute (routing only, no execution).

**Request:**
```json
{
  "graph_name": "string (required)",
  "state": {...}
}
```

**Response:**
```json
{
  "next_node": "string | '__end__'",
  "metadata": {"last_node": "string"}
}
```

### `execute_node`
Execute a specific named node.

**Request:**
```json
{
  "graph_name": "string (required)",
  "node_name": "string (required)",
  "state": {...}
}
```

**Response:**
```json
{
  "state": {...}
}
```

### `invoke_graph`
Execute a full StateGraph in a single call (coarse granularity).

**Request:**
```json
{
  "graph_name": "string (required)",
  "state": {...}
}
```

**Response:**
```json
{
  "state": {...},
  "result": "..."
}
```

### `execute_task`
Execute a Functional API task function.

**Request:**
```json
{
  "task_name": "string (required)",
  "...task_args": "..."
}
```

**Response:**
Task-specific result.

### `invoke_entrypoint`
Execute a Functional API entrypoint in a single call.

**Request:**
```json
{
  "entrypoint_name": "string (required)",
  "input": "..."
}
```

**Response:**
```json
{
  "result": "..."
}
```

### `list_nodes`
List all node names for a registered graph.

**Request:**
```json
{
  "graph_name": "string (required)"
}
```

**Response:**
```json
{
  "nodes": ["string", ...],
  "edges": [{"from": "string", "to": "string"}, ...],
  "conditional_edges": [...]
}
```

### `graph_info`
Return full metadata for a registered graph.

**Request:**
```json
{
  "graph_name": "string (required)"
}
```

**Response:**
Full graph metadata dict.
