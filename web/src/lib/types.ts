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
