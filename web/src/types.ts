// API data shapes (docs/design.md §16; contract implemented in internal/server/httpapi).

export interface Me {
  must_change_password: boolean;
  version: string;
}

export interface NodeView {
  id: string;
  name: string;
  status: string;
  online: boolean;
  note: string;
  os: string;
  arch: string;
  hostname: string;
  cpu_cores: number;
  primary_ip: string;
  country_code: string;
  agent_version: string;
  last_seen?: number | null;
  cpu: number;
  mem_used: number;
  mem_total: number;
  disk_used: number;
  disk_total: number;
  net_rx_rate: number;
  net_tx_rate: number;
  iface: string;
  mode: string;
  quota_bytes?: number | null;
  period_used: number;
  period_start?: number | null;
  period_pct: number; // -1 = no quota
  today_rx: number;
  today_tx: number;
  next_due_at?: number | null;
}

export interface NodeIP {
  ip: string;
  family: number;
  scope: string;
  is_primary: boolean;
}

export interface NodeNetwork {
  iface: string;
  mode: string; // in | out | both | max
  quota_bytes: number | null;
  cycle_days: number | null;
  anchor_at: number | null;
  tz: string;
}

export interface NodeBilling {
  cycle_type: string;
  cycle_days: number | null;
  next_due_at: number | null;
  note: string;
}

export interface SingboxInfo {
  version: string;
  desired_version: string;
  status: string;
  last_error: string;
  port: number;
  cert_sha256: string;
  cert_not_after: number;
}

export interface NodeDetailData {
  node: NodeView;
  ips: NodeIP[];
  online_now: boolean;
  singbox?: SingboxInfo | null;
  network?: NodeNetwork | null;
  billing?: NodeBilling | null;
  latency_targets?: LatencyTarget[];
}

export interface MetricsSample {
  ts: number;
  cpu: number;
  mem_used: number;
  mem_total: number;
  disk_used: number;
  disk_total: number;
  net_rx_rate: number;
  net_tx_rate: number;
  load1: number;
  uptime: number;
}

export interface TrafficDay {
  date: string; // YYYY-MM-DD
  rx_bytes: number;
  tx_bytes: number;
}

export interface TrafficResp {
  daily: TrafficDay[];
  period_rx: number;
  period_tx: number;
}

export interface LatencySample {
  target_id: number;
  ts: number;
  icmp_ms: number | null;
  tcp_ms: number | null;
  loss: number;
}

export interface LatencyTarget {
  id: number;
  name: string;
  kind: string; // icmp | tcp
  host: string;
  port: number;
}

export interface CommandRow {
  id: string;
  node_id: string;
  kind: string;
  payload: string;
  status: string; // pending | sent | ok | failed | timeout
  created_at: number;
  sent_at: number | null;
  finished_at: number | null;
  result: string;
}

export interface RegTokenInfo {
  token: string;
  install_command: string;
  ttl: number;
}

export interface RegTokenRow {
  id: number;
  note: string;
  created_at: number;
  expires_at: number;
  used_at?: number | null;
  used_by?: string | null;
}

export interface BlacklistRow {
  ip: string;
  reason: string;
  fail_count: number;
  created_at: number;
  expires_at: number; // 0 = counter only
}

export interface SessionRow {
  id: string;
  created_at: number;
  last_seen: number;
  ua: string;
  ip: string;
  revoked: boolean;
}

export interface AuditRow {
  ts: number;
  actor: string;
  node_id?: string;
  action: string;
  command?: string;
  risk?: string;
  source_ip?: string;
  ai_session_id?: string;
}

export interface AlertRow {
  id: number;
  kind: string;
  node_id?: string;
  payload?: string;
  created_at: number;
  delivered_at?: number | null;
  recovered_at?: number | null;
}

export interface SettingView {
  key: string;
  value: string;
  sensitive: boolean;
  set: boolean;
}

// --- subscriptions & templates (design §10) ---

export interface SubscriptionRow {
  id: string;
  name: string;
  enabled: boolean;
  created_at: number;
  template_id?: string | null;
  /** §10 UA allow-list: comma-separated substrings; empty = every client. */
  ua_filter: string;
  node_ids: string[];
}

/** One-time plaintext token response (create / rotate). */
export interface SubscriptionToken {
  id?: string;
  name?: string;
  token: string;
  url?: string;
}

export interface SubAccessRow {
  ts: number;
  ip: string;
  ua: string;
}

export interface TemplateRow {
  id: string;
  name: string;
  format: 'singbox' | 'clash';
  content: string;
  created_at: number;
  updated_at: number;
}

// --- sing-box management (design §9) ---

export interface SingboxVersion {
  version: string;
  sha256?: string;
}

export interface SingboxStatus {
  node_id: string;
  version: string;
  desired_version: string;
  config_hash: string;
  status: string;
  last_error: string;
  rollback_version: string;
  cert_sha256: string;
  cert_not_after: number;
  port: number;
  updated_at: number;
}

export interface WsEvent {
  kind: string;
  ref: string;
  ts: number;
}

// Browser terminal WebSocket envelopes. The server stamps the authoritative
// session_id into payloads as frames pass through to the agent.
export type TerminalEnvelopeType = 'terminal_open' | 'terminal_input' | 'terminal_resize' | 'terminal_close' | 'terminal_output' | 'terminal_closed';

/** Terminal backend: SSH to the node's own sshd (default) or local PTY fallback. */
export type TerminalMode = 'ssh' | 'pty';

export interface TerminalEnvelope<TPayload = unknown> {
  v: number;
  type: TerminalEnvelopeType;
  id?: string;
  ts: number;
  payload?: TPayload;
}

export interface TerminalOpenPayload {
  session_id?: string;
  mode: TerminalMode;
  cols: number;
  rows: number;
}

export interface TerminalInputPayload {
  session_id?: string;
  data: string;
}

export interface TerminalResizePayload {
  session_id?: string;
  cols: number;
  rows: number;
}

export interface TerminalClosePayload {
  session_id?: string;
}

export interface TerminalOutputPayload {
  session_id?: string;
  data: string;
}

export interface TerminalClosedPayload {
  session_id?: string;
  reason?: string;
}

// --- per-node SSH credentials (design §11; secrets are write-only) ---

/** GET /api/nodes/{id}/ssh — never contains secret material. */
export interface NodeSSHView {
  user: string;
  port: number;
  password_set: boolean;
  privkey_set: boolean;
}

/**
 * PUT /api/nodes/{id}/ssh — an omitted field or an empty string keeps the
 * stored value; the "!" prefix clears it; anything else replaces the secret.
 */
export interface NodeSSHUpdate {
  user?: string;
  port?: number;
  password?: string;
  private_key?: string;
}

export interface AIChatRequest {
  node_id: string;
  terminal_session_id?: string;
  message: string;
  include_logs?: boolean;
  session_id?: string;
}

/** Mirrors the backend tool_call map: a run_shell request the model made. */
export interface AIToolCall {
  id?: string;
  name?: string;
  arguments?: { command?: string; reason?: string; risky?: boolean };
  status?: 'queued' | 'needs_confirmation' | 'blocked' | 'invalid' | 'internal' | string;
  action_id?: string;
  command_id?: string;
  reason?: string;
}

export interface AIChatResponse {
  session_id?: string;
  message: string;
  tool_call?: AIToolCall;
}

export interface AIConfirmResponse {
  session_id: string;
  action_id: string;
  status: string;
  command_id: string;
}

// --- panel export / import & GeoIP upload (design §17 / §14) ---

/** Server-side import summary: merged counts, idempotent by machine_id/name. */
export interface ImportStats {
  nodes_created: number;
  nodes_updated: number;
  latency_targets_created: number;
  subscriptions_created: number;
  subscriptions_updated: number;
  templates_created: number;
  templates_updated: number;
  settings_imported: number;
}
