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
  /** §14: true = flag pinned from the edit page, survives IP changes. */
  country_manual: boolean;
  agent_version: string;
  tz: string;
  /** Linux distribution detected by the agent from /etc/os-release (§16). */
  distro_id: string;
  distro_version: string;
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
  billing_configured: boolean;
  next_due_at?: number | null;
  // Agent self-update state (design §5.5): what the panel needs to answer
  // "why did this probe not follow the server?".
  agent_target_version: string;
  agent_update_state: string;
  agent_update_attempts: number;
  agent_update_error?: string;
  /**
   * §5.5 stagger anchor: when the server first noticed this node was behind
   * (the same second for every node after a restart). Diagnostics only — the
   * panel shows agent_update_after.
   */
  agent_update_planned_at?: number | null;
  /** §5.5: when the pending plan becomes actionable = anchor + stagger offset. */
  agent_update_after?: number | null;
  agent_update_done_at?: number | null;
  /** Capability bit: false = built before §5.5, needs a manual reinstall. */
  agent_self_update: boolean;
  /** Whether the agent ever said hello (false = no caps reported yet). */
  agent_caps_seen: boolean;
  /**
   * §10: this node would render into a subscription (primary IP + sing-box
   * inbound port + reported certificate). The subscription node picker lists
   * only these.
   */
  singbox_ready: boolean;
}

/** GET /api/agent/update (design §5.5). */
export interface AgentUpdateStatus {
  enabled: boolean;
  reason?: string;
  server_version: string;
  artifact_present: boolean;
  stagger_seconds: number;
  nodes_total: number;
  nodes_behind: number;
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
}

export interface NodeTrafficCycle {
  cycle_type: 'none' | 'month' | 'year';
  next_reset_at: number | null;
}

export interface NodeInterface {
  name: string;
  default: boolean;
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
  /** Removal requested but not confirmed yet (§9.2). */
  desired_uninstall?: boolean;
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
  traffic_cycle?: NodeTrafficCycle | null;
  interfaces?: NodeInterface[];
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
  node_name?: string;
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

/** Scan-to-add session view (mirrors feishureg.Status). */
export interface FeishuQRStatus {
  state: 'idle' | 'qr_ready' | 'saving' | 'succeeded' | 'expired' | 'denied' | 'cancelled' | 'error';
  qr_url?: string;
  expires_at?: number;
  remaining_seconds?: number;
  bot_name?: string;
  error?: string;
}

/** POST /api/settings/notify/test — ok=false carries the upstream reason. */
export interface NotifyTestResult {
  ok: boolean;
  code?: string;
  detail?: string;
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
  /**
   * §10 实现修订 2026-09-16: true while the server can still reconstruct the
   * plaintext token. Rows created before that revision (or after a master-key
   * change) can only get a working URL by rotating.
   */
  link_available: boolean;
  /**
   * §10 实现修订 2026-09-16: pinned output format, '' = auto (the bound
   * template's format, otherwise the client's ?format= / User-Agent).
   */
  format: string;
}

/** Plaintext token + URL (create / rotate / link reveal). */
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
  /**
   * §10 实现修订 2026-09-16: '' = this fetch was served, otherwise why it was
   * refused (`ua_mismatch` / `disabled` / `render_error`). The client always saw
   * the same 404, so this column is the only explanation there is.
   */
  reason: string;
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
  /** Cached binary size in bytes (server-side artifact cache, §9.2). */
  size?: number;
  /** When the version was cached, unix seconds (0/absent = unknown). */
  downloaded_at?: number;
  /** Nodes whose desired_version still points at this version. */
  refs?: number;
}

export interface SingboxStatus {
  node_id: string;
  version: string;
  desired_version: string;
  /** Operator asked for removal; the probe's "absent" report clears it (§9.2). */
  desired_uninstall: boolean;
  config_hash: string;
  status: string;
  last_error: string;
  cert_sha256: string;
  cert_not_after: number;
  port: number;
  updated_at: number;
}

// --- nftables port forwarding (design §21) ---

/**
 * One port forward as it exists on the probe, in github.com/fonlan/nfpf's
 * layout (`ip nat prerouting` DNAT + `ip nat postrouting` masquerade). Rules
 * added by nfpf.sh or by hand show up here identically — the probe's ruleset is
 * the source of truth, not this panel.
 */
export interface ForwardRule {
  proto: string; // tcp | udp
  src_port: number;
  /** Inbound interface; '' = all interfaces. */
  iface: string;
  dst_ip: string;
  dst_port: number;
  /**
   * Free-form label written onto the DNAT rule the way nfpf.sh does it. A
   * double quote cannot be represented in an nft string literal at all, so the
   * server rejects it.
   */
  comment: string;
  /** nft rule handle, sent back on edit/delete to target that exact rule. */
  handle: number;
  /** The rule carries match terms the panel does not model: show, never edit. */
  extra_match: boolean;
}

export interface ForwardRuleInput {
  proto: string;
  src_port: number;
  iface: string;
  dst_ip: string;
  dst_port: number;
  comment?: string;
  handle?: number;
}

export interface ForwardsStatus {
  /** The probe can manage forwards at all (nft present, agent root). */
  supported: boolean;
  /** `table ip nat` + both chains exist; the first add creates them. */
  initialized: boolean;
  /** Why not supported: nft_missing | need_root | no_permission | chain_mismatch | … */
  code: string;
  /** Raw nft output for the hint line. */
  message: string;
  /** Last report time (unix seconds); 0 = this probe never reported. */
  reported_at: number;
  /** The probe's agent knows §21 — false means it needs an update. */
  agent_supported: boolean;
  forwards: ForwardRule[];
  /** This payload came from a probe round trip, not the stored snapshot. */
  live: boolean;
  /** The command is queued (probe offline or slow); TTL 10 minutes. */
  queued: boolean;
  warnings?: string[];
}

// --- server-side artifact cache + one-click batch update (design §9.2) ---

/** GET /api/singbox/cache — one cached release on the server's disk. */
export interface SingboxCacheVersion {
  version: string;
  sha256?: string;
  /** Binary size in bytes. */
  size: number;
  /** When the version was cached, unix seconds. */
  downloaded_at: number;
  /** Nodes whose desired_version points at this version. */
  refs: number;
  /** True for the newest cached release (the one "update" defaults to). */
  is_latest: boolean;
}

/** Startup auto-download state written by the server (settings singbox.cache_status). */
export interface SingboxCacheStatus {
  state: 'ok' | 'disabled' | 'failed' | 'pending' | string;
  version?: string;
  updated_at?: number;
  error?: string;
  auto_download: boolean;
}

/** Per-node bucket of a batch update (mirrors internal/server/singboxupdate). */
export type SingboxUpdateOutcome = 'already_current' | 'pushed' | 'offline_pending' | 'failed';

export interface SingboxUpdateNodeResult {
  node_id: string;
  name?: string;
  online: boolean;
  outcome: SingboxUpdateOutcome | string;
  reason?: string;
  version?: string;
  desired_version?: string;
  /** Set by the 15-minute convergence check. */
  converged?: boolean;
  checked_at?: number;
}

export interface SingboxUpdateCounts {
  total: number;
  already_current: number;
  pushed: number;
  offline_pending: number;
  failed: number;
}

/** One distribution batch (settings singbox.last_update). */
export interface SingboxUpdateJob {
  id: string;
  state: 'pending' | 'downloading' | 'pushing' | 'done' | 'failed' | string;
  requested?: string;
  target_version?: string;
  started_at: number;
  finished_at?: number;
  /** When the convergence re-check is due, unix seconds. */
  deadline?: number;
  error?: string;
  actor?: string;
  source_ip?: string;
  nodes: SingboxUpdateNodeResult[];
  counts: SingboxUpdateCounts;
  convergence_checked: boolean;
  checked_at?: number;
  /** Node ids that had not converged at the deadline. */
  stale?: string[];
  stale_alerted?: boolean;
}

/** One affected node in the pre-flight confirmation list. */
export interface SingboxImpactNode {
  node_id: string;
  name?: string;
  online: boolean;
  status?: string;
  version?: string;
  desired_version?: string;
  already_current: boolean;
}

/** GET /api/singbox/update/impact — the confirmation dialog's data. */
export interface SingboxImpact {
  target_version: string;
  requested?: string;
  cached: boolean;
  download_needed: boolean;
  count: number;
  online: number;
  offline: number;
  already_current: number;
  nodes: SingboxImpactNode[];
}

/**
 * Live artifact install progress. Rides /ws/events as `singbox_download` and is
 * also part of GET /api/singbox/cache, so a page loaded mid-download shows the
 * same bar. It is never persisted: the byte counter would hammer SQLite.
 */
export interface SingboxDownload {
  /** True while an install holds the downloader (including queued behind one). */
  active: boolean;
  version?: string;
  phase?: 'waiting' | 'resolving' | 'downloading' | 'verifying' | 'extracting' | 'publishing' | 'done' | 'failed' | string;
  downloaded: number;
  /** 0/undefined when upstream sends no Content-Length. */
  total?: number;
  percent?: number;
  /** Bytes per second over the last sample (download phase only). */
  speed?: number;
  started_at?: number;
  updated_at?: number;
  error?: string;
}

/** GET /api/singbox/cache — everything the settings section renders. */
export interface SingboxCache {
  dl_dir: string;
  versions: SingboxCacheVersion[];
  latest_cached: string;
  auto_download: boolean;
  mount_ok: boolean;
  mount_applicable: boolean;
  cache_status: SingboxCacheStatus | null;
  last_update: SingboxUpdateJob | null;
  download?: SingboxDownload | null;
}

/**
 * One upstream release in the "download a new version" picker.
 * GET /api/singbox/releases — the only endpoint that talks upstream while an
 * operator watches; it answers from a 10-minute server-side cache.
 */
export interface SingboxRelease {
  version: string;
  /** Raw upstream tag (v1.12.0) — tooltip material only. */
  tag?: string;
  /** Betas/rcs: downloadable, but never preselected. */
  prerelease: boolean;
  /** Upstream publication time, unix seconds (0 = upstream did not say). */
  published_at?: number;
  /** Already on the server's disk. */
  cached: boolean;
  /** The newest non-prerelease entry of the listing. */
  latest_stable: boolean;
}

/** GET /api/singbox/releases. */
export interface SingboxReleases {
  releases: SingboxRelease[];
  fetched_at: number;
  /** True when the refresh failed and this is the last good listing. */
  stale: boolean;
  error?: string | null;
}

/** POST /api/singbox/versions/{version}/download. */
export interface SingboxDownloadAccepted {
  ok: boolean;
  version: string;
  /** True when the version was already on disk (the call was a no-op). */
  cached: boolean;
  download?: SingboxDownload | null;
}

export interface WsEvent {
  kind: string;
  ref: string;
  ts: number;
  /** Optional payload (singbox_update carries the full Job snapshot). */
  data?: unknown;
}

// Browser terminal WebSocket envelopes. The server stamps the authoritative
// session_id into payloads as frames pass through to the agent.
export type TerminalEnvelopeType = 'terminal_open' | 'terminal_input' | 'terminal_resize' | 'terminal_close' | 'terminal_output' | 'terminal_closed';

export interface TerminalEnvelope<TPayload = unknown> {
  v: number;
  type: TerminalEnvelopeType;
  id?: string;
  ts: number;
  payload?: TPayload;
}

export interface TerminalOpenPayload {
  session_id?: string;
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

/** One GeoIP database update in flight (or the last one that ran), pushed as
 *  the `geoip_update` event and mirrored by GET /api/geoip/status. */
export interface GeoIPDownload {
  active: boolean;
  /** connecting | downloading | verifying | done | failed */
  phase?: string;
  /** Mirror URL being tried. */
  source?: string;
  downloaded: number;
  /** 0/absent when the mirror sends no Content-Length (no percentage then). */
  total?: number;
  percent?: number;
  speed?: number;
  started_at?: number;
  updated_at?: number;
  error?: string;
}

/** GET /api/geoip/status: the file on disk, the live resolver and the §14.1
 *  update policy in one flat object. */
export interface GeoIPStatus {
  path: string;
  /** false when FOBE_GEOIP_MMDB is empty: no upload, no update. */
  configured: boolean;
  exists: boolean;
  size_bytes?: number;
  mod_time?: number;
  /** When the provider built the data (unix seconds) — the honest data age. */
  build_epoch?: number;
  database_type?: string;
  /** true when the resolver is actually serving a database. */
  live: boolean;
  /** ok | disabled | pending | failed */
  state: string;
  source?: string;
  updated_at?: number;
  checked_at?: number;
  error?: string;
  auto_update: boolean;
  /** true when FOBE_GEOIP_AUTO_UPDATE=0 overrides the panel switch. */
  env_locked: boolean;
  max_age_days: number;
  download: GeoIPDownload;
}

/** POST /api/geoip/update: accepted=false means one was already running. */
export interface GeoIPUpdateAccepted {
  accepted: boolean;
  running: boolean;
}

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
