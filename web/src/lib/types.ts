export interface WorkflowInstance {
  id: string;
  def_name: string;
  def_version: number;
  status: string;
  input: string;
  result: string;
  error: string;
  created_at: string;
  updated_at: string;
  assigned_to: string;
  next_wake_at: string;
  namespace: string;
}

export interface EventRecord {
  step: number;
  type: string;
  service?: string;
  op?: string;
  request?: string;
  response?: string;
  err?: string;
  duration_ms?: number;
  signal_names?: string;
  signal_name?: string;
  signal_payload?: string;
  defer_description?: string;
  defer_id?: string;
  child_name?: string;
  child_input?: string;
  run_id?: string;
  new_input?: string;
  created_at?: string;
}

export interface DAGTask {
  name: string;
  fn?: string;
  parents: string[];
}

export interface DAGSpec {
  name: string;
  tasks: DAGTask[];
}

export interface DAGResponse {
  workflow_id: string;
  dag: DAGSpec;
}

export interface Schedule {
  name: string;
  cron_expression: string;
  def_name: string;
  entry_point: string;
  input: string;
  enabled: boolean;
  next_run_at: string;
  created_at: string;
  updated_at: string;
}

// ── Cost observability types ──────────────────

export interface TokenUsage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
}

export interface LlmCallInfo {
  step: number;
  model: string;
  provider: string;
  function_name: string;
  usage: TokenUsage;
  cost: number;
}

export interface CostBreakdown {
  byModel: Record<string, { tokens: TokenUsage; cost: number; calls: number }>;
  byProvider: Record<string, { tokens: TokenUsage; cost: number; calls: number }>;
  totalCost: number;
  totalTokens: TokenUsage;
  llmCalls: number;
}

export interface WorkflowCost {
  workflowId: string;
  workflowType: string;
  status: string;
  totalCost: number;
  totalTokens: TokenUsage;
  llmCalls: number;
  startedAt: string;
}

// ── Workflow definition & memory stats types ──────────────────

export interface WorkflowMemoryStats {
  def_name: string;
  min_bytes: number;
  avg_bytes: number;
  max_bytes: number;
  p10: number;
  p25: number;
  p50: number;
  p75: number;
  p90: number;
  p99: number;
  sample_count: number;
}

export interface WorkflowDefInfo {
  name: string;
  version: number;
  abi_version: number;
  min_version: number;
  created_at: string;
  deprecated: boolean;
  active_instances: number;
  memory: WorkflowMemoryStats | null;
}
