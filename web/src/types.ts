// API data shapes (docs/design.md §16; contract implemented in internal/server/httpapi).

export interface Me {
  must_change_password: boolean;
  version: string;
}

export interface NodeView {
  id: string;
  name: string;
  /** §10.2 subscription display name; '' = use `name`. */
  sub_name: string;
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
  /** Operator-entered 费用 (node_billing.note); '' = unset, no card tag. */
  billing_note?: string;
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
  cycle_type: 'none' | 'month' | 'quarter' | 'year';
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
  /** All listeners the probe saw in config.json (§17g), no credentials. */
  singbox_inbounds?: SingboxInboundRow[];
  network?: NodeNetwork | null;
  traffic_cycle?: NodeTrafficCycle | null;
  interfaces?: NodeInterface[];
  billing?: NodeBilling | null;
  latency_targets?: LatencyTarget[];
}

/** Row of node_singbox_inbounds: one listener lifecycle record. */
export interface SingboxInboundRow {
  port: number;
  type: string;
  tag: string;
  status: string; // pending | running | deleting
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

/** 常用命令：终端页右栏的命令片段（2026-09-19）。 */
export interface QuickCommand {
  id: number;
  name: string;
  command: string;
  sort_order: number;
  created_at: number;
  updated_at: number;
}

export interface RegTokenInfo {
  token: string;
  install_command: string;
  ttl: number;
}

export interface BlacklistRow {
  ip: string;
  reason: string;
  fail_count: number;
  created_at: number;
  expires_at: number; // 0 = counter only
}

export interface SessionRow {
  /** Short fingerprint, not the session id: that id is the cookie value. */
  id_short: string;
  created_at: number;
  last_seen: number;
  ua: string;
  ip: string;
  revoked: boolean;
}

export interface AuditRow {
  /** audit_logs primary key; used as the React key for a page of rows. */
  id: number;
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

/** `GET /api/audit`: one numbered page plus what the pager needs to render. */
export interface AuditPage {
  entries: AuditRow[];
  total: number;
  page: number;
  limit: number;
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
  /**
   * §10.2: how many *enabled* entries this subscription emits. `node_ids` (the
   * legacy direct projection) cannot express a node appearing twice — once
   * directly and once through a relay.
   */
  entry_count: number;
}

/**
 * One §10.2 subscription entry (GET /api/subscriptions/{id}/entries): a target
 * node reached either directly or through a relay's nftables DNAT. The identity
 * is (node_id, relay_node_id, proto, src_port, iface); relay_node_id === ''
 * means one of the node's own inbounds.
 *
 * Since §9.3/§10.2 实现修订 2026-09-18 a direct entry names a single *inbound*
 * by its port, so a server running two anytls listeners is two rows — and two
 * outbounds — each one nameable on its own.
 */
export interface SubscriptionEntry {
  node_id: string;
  node_name: string;
  /** '' = direct ingress, otherwise the relay node's id. */
  relay_node_id: string;
  relay_name: string;
  /** 'tcp' for relayed entries (the derived rule's protocol); '' when direct. */
  proto: string;
  /**
   * The port the client dials: the node's own inbound port for a direct entry,
   * the relay's forwarding source port for a relayed one. 0 = a legacy
   * node-level row (the whole node, which the server splits by itself).
   */
  src_port: number;
  iface: string;
  /**
   * The wire protocols this entry renders as (anytls / vless / …), read from
   * the probe's reported config — the running config is the authority. Empty
   * = unknown right now (nothing reported yet, or the inbound is gone); the
   * normalizer fills the missing key.
   */
  protocols: string[];
  /** The name the renderer derives when `alias` is empty. */
  auto_name: string;
  /** Operator override; '' = use auto_name. */
  alias: string;
  /** Bound to this subscription and enabled. */
  selected: boolean;
  /** false = listed right now, but it would render nothing. */
  available: boolean;
  /** Why available is false: not_ready | relay_not_ready | relay_gone | inbound_gone. */
  reason: string;
  /** Non-fatal code, currently only `shadowed` (§21.9). */
  warning: string;
  /** True = derived from a live forward rule on the probe. */
  discovered: boolean;
  /** The forward rule's comment, so a rule can be told from its twin. */
  source: string;
}

/** GET /api/subscriptions/{id}/entries. */
export interface SubscriptionEntries {
  entries: SubscriptionEntry[];
  /** §10.2 auto-enrolment of freshly detected relay entries (global setting). */
  relay_auto_include: boolean;
  relay_name_format: string;
}

/**
 * PUT /api/subscriptions/{id}/nodes body: exactly the seven fields the server
 * reads. The picker sends every entry it displayed, unchecked ones included —
 * an unchecked entry that was bound becomes a tombstone, which is the only
 * thing that stops the reconciler from auto-adding it again.
 */
export interface SubscriptionEntryInput {
  node_id: string;
  relay_node_id: string;
  proto: string;
  src_port: number;
  iface: string;
  alias: string;
  selected: boolean;
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
  /** What sing-box the probe already runs outside fobe (§9.3 实现修订). */
  local: SingboxLocalDiscovery | null;
}

/** One inbound found in the probe's own config.json. Never carries the
 *  credential — only whether one is there (the server keeps secrets
 *  encrypted and hands them back solely inside a pushed config). */
export interface SingboxLocalInbound {
  type: string;
  tag: string;
  port: number;
  label: string;
  cred_set: boolean;
  /** False = fobe found it but does not model this protocol. */
  supported: boolean;
}

/** One editable inbound of the probe's own config.json (the editor model,
 *  §9.3 实现修订 2026-09-17b). `credential` is present because this IS the
 *  operator's configuration and he has to be able to change it. */
export interface SingboxInbound {
  type: string;
  tag: string;
  port: number;
  label: string;
  editable: boolean;
  credential?: string;
  has_cred: boolean;
  server_name?: string;
  uuid?: string;
  flow?: string;
  username?: string;
  method?: string;
  /** `pending` until the agent confirms local TCP reachability, then `running`;
   *  a listener the panel removed reads `deleting` until a report drops it. */
  status: 'pending' | 'running' | 'deleting' | string;
  /** Position in the file's inbound array — every edit carries it back. */
  number: number;
}

export interface SingboxConfigPayload {
  inbounds: SingboxInbound[];
  reported: boolean;
  renderable: boolean;
  edited: boolean;
}

export interface SingboxLocalDiscovery {
  present: boolean;
  running: boolean;
  unit_active: boolean;
  /** False = the agent could not ask the service manager (no systemd / no privileges). */
  unit_known: boolean;
  version?: string;
  config_path?: string;
  hash?: string;
  error?: string;
  inbounds: SingboxLocalInbound[];
  /** How many inbounds of the stored config came from a takeover performed
   *  before the editor model (legacy nodes). Always 0 on newer ones. */
  adopted: number;
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
//
// `terminal_query` / `terminal_buffer` are the exception: they exist ONLY on
// this browser ↔ server hop and are never forwarded to the agent (design
// §12.7.1). The server keeps no terminal state, so the XTerm buffer in this
// browser is the only screen model — the server asks for a window of it and the
// browser answers, correlated by a request id.
export type TerminalEnvelopeType =
  | 'terminal_open'
  | 'terminal_input'
  | 'terminal_resize'
  | 'terminal_close'
  | 'terminal_output'
  | 'terminal_closed'
  | 'terminal_query'
  | 'terminal_buffer';

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

/**
 * `terminal_query` (server → browser): hand back a window of the XTerm buffer.
 *
 * The window is measured from the BOTTOM of the buffer, never from the
 * operator's scroll position — scrolling up must not change what the AI reads.
 * `offset` counts lines up from the bottom (0 = the newest line) and `lines`
 * is the window height; `lines <= 0` means "use this browser's viewport height"
 * (`term.rows`).
 */
export interface TerminalQueryPayload {
  /** Correlates the answer; echoed verbatim in `TerminalBufferPayload.id`. */
  id: string;
  offset: number;
  lines: number;
}

/**
 * `terminal_buffer` (browser → server): the answer to `terminal_query`.
 *
 * A failure is a first-class shape rather than an empty window — the server
 * must be able to tell "this terminal has no buffer" (disposed/unmounted) from
 * "the screen really was blank".
 */
export type TerminalBufferPayload =
  | {
      id: string;
      ok: true;
      cols: number;
      rows: number;
      /** Total buffer length the window was taken from. */
      length: number;
      /** The window, oldest first, right-trimmed. */
      lines: string[];
    }
  | { id: string; ok: false; error: string };

/**
 * POST /api/ai/chat body. The response is an SSE stream, not JSON (§12.6) —
 * see `streamAIChat` in api.ts. A conversation is pinned to one (provider,
 * model, protocol) triple because reasoning blocks are protocol-native (§12.5),
 * so provider_id/model_id only take effect when a NEW session is opened.
 */
export interface AIChatRequest {
  node_id: string;
  /**
   * The terminal session the AI's keys must land on. It is resolved by the
   * server at CALL time, never from the id frozen on the session row: the id is
   * minted per browser WS connection, so a page refresh leaves the stored value
   * dangling (§12.7.5). Omitted when no terminal is connected yet.
   */
  terminal_session_id?: string;
  message: string;
  session_id?: string;
  provider_id?: string;
  model_id?: string;
  /** off | minimal | low | medium | high; omitted = the model's own default. */
  reasoning_level?: string;
}

/**
 * POST /api/ai/chat/continue body — the second half of a confirmation (§12.6).
 * The stream ENDS at `needs_confirmation`, so answering it re-enters the loop
 * through this endpoint; `approved: false` is a normal outcome the model gets
 * told about (it can explain or propose something else).
 *
 * Deliberately NOT /api/ai/actions/{id}/confirm: that non-streaming route is
 * the pre-§12.6 path and cannot report the resumed turn.
 */
export interface AIContinueRequest {
  session_id?: string;
  action_id: string;
  approved: boolean;
  /**
   * Same contract as AIChatRequest.terminal_session_id: the LIVE terminal id,
   * re-sent on every request because a resumed turn keeps keying into the PTY
   * and the frozen session-row value may already be a dangling pointer
   * (§12.7.5).
   */
  terminal_session_id?: string;
  reasoning_level?: string;
}

/** `event: session` — first frame; names the conversation before any delta. */
export interface AISessionEvent {
  session_id: string;
  provider_id?: string;
  model_id?: string;
  protocol?: string;
}

/** `event: tool_result` — one tool finished (the authoritative signal; the
 *  backend does not emit a separate tool_call event today). */
export interface AIToolResultEvent {
  id?: string;
  name?: string;
  /** ok | failed | blocked | refused | queued | read (older/newer builds may add more). */
  status?: string;
  command?: string;
  /** null/absent when the tool was not a command or produced no exit code. */
  exit_code?: number | null;
  /** Present on budget/repeat refusals. */
  reason?: string;
}

/** `event: needs_confirmation` — the stream ends right after this (§12.6). */
export interface AINeedsConfirmationEvent {
  id?: string;
  name?: string;
  action_id: string;
  command?: string;
  reason?: string;
  risk?: string;
}

/**
 * Why a turn ended. MUST be surfaced: a turn cut off by a budget would
 * otherwise look like a normal finished answer (§12.6).
 */
export type AITurnEndReason =
  | 'completed'
  | 'turn_budget'
  | 'repeat_call'
  | 'turn_timeout'
  /** More than 10 consecutive read_terminal calls this turn (§12.7.4). */
  | 'read_limit'
  | 'needs_confirmation'
  | 'upstream_error'
  | string;

export interface AITurnEndEvent {
  session_id?: string;
  reason: AITurnEndReason;
  /** How many change-class commands ran this turn. */
  changes?: number;
  action_id?: string;
}

/** `event: error` — mid-stream failure (a JSON error is impossible once 200). */
export interface AIErrorEvent {
  code: string;
  message?: string;
}

/** One restored transcript row from GET /api/ai/sessions/{id}. */
export interface AISessionMessage {
  /**
   * `turn_end` marker rows carry the reason a turn stopped (§12.6), which lives
   * nowhere else — the panel renders them where the live stream shows its
   * turn-end chip.
   */
  turn_reason?: string;
  changes?: number;
  role: 'user' | 'assistant' | 'tool' | string;
  content: string;
  /** Protocol-native blocks, kept opaque; the panel never parses them. */
  raw?: unknown;
  /** Reasoning text, extracted SERVER-side — this is what replays on reload. */
  thinking?: string;
}

export interface AISessionHistory {
  session_id: string;
  node_id: string;
  provider_id?: string;
  model_id?: string;
  protocol?: string;
  messages: AISessionMessage[];
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
  /** §17: decrypted sensitive settings (the snapshot now carries credentials). */
  sensitive_imported: number;
  ai_providers_imported: number;
  ai_models_imported: number;
  ai_provider_models_linked: number;
  /**
   * §17: credentials the restore could NOT read (the snapshot was sealed with a
   * different master key) or deliberately dropped. Reported, never swallowed —
   * a silent skip is indistinguishable from "you never configured it".
   */
  skipped_secrets?: string[];
}

// --- AI providers, models & models.dev metadata (design §12.1/§12.5) ---------

/**
 * One model as seen THROUGH one provider. `effective_levels` is computed
 * server-side (protocol capability ∩ the model's documented levels) and is the
 * only level list the panel may render — re-deriving the rule here is exactly
 * the duplication that drifts (§12.1 takes the same stance on `ai_configured`).
 * An empty array means "this model exposes no adjustable thinking level through
 * this provider"; the UI says so instead of rendering an empty dropdown.
 */
export interface AIProviderModel {
  id: string;
  display_name: string;
  context_window: number;
  max_output_tokens: number;
  effective_levels: string[];
  /**
   * Model-level switch. The settings page lists disabled models (so they can be
   * turned back on); the picker must not offer them. Optional because an older
   * server build omits it — undefined is treated as enabled.
   */
  enabled?: boolean;
}

/** GET /api/ai/catalog → providers[]; secrets are write-only (has_key / header_names). */
export interface AIProvider {
  id: string;
  name: string;
  /** openai-completions | openai-responses | anthropic-messages */
  protocol: string;
  /** ROOT semantics: the adapter appends the protocol path. */
  base_url: string;
  models_dev_slug: string;
  enabled: boolean;
  has_key: boolean;
  has_headers: boolean;
  header_names: string[];
  model_ids: string[];
  models: AIProviderModel[];
  created_at: number;
  updated_at: number;
  /** Final URL the adapter will call; absent on an older server build. */
  endpoint?: string;
}

/** GET /api/ai/catalog → models[]; a model row is global, linked to N providers. */
export interface AIModel {
  id: string;
  display_name: string;
  context_window: number;
  max_output_tokens: number;
  input_modalities: string[];
  output_modalities: string[];
  /** Stored (protocol-independent) levels: off|minimal|low|medium|high. */
  reasoning_levels: string[];
  /** Hand-edited fields; a refresh from models.dev never overwrites these. */
  overridden_fields: string[];
  /** models_dev | manual */
  source: string;
  enabled: boolean;
  provider_ids: string[];
}

export interface AICatalog {
  providers: AIProvider[];
  models: AIModel[];
  default_provider_id: string;
  default_model_id: string;
  /** Applied when the assistant's thinking picker is left at "default (unset)". */
  default_reasoning: string;
}

/**
 * Write shape for POST/PATCH /api/ai/providers. `api_key` / `extra_headers` are
 * optional on purpose: omitting them leaves the stored secret alone, "" clears
 * it. That distinction is what lets the form say "I did not touch the key"
 * without ever echoing a ciphertext back.
 */
export interface AIProviderInput {
  name: string;
  protocol: string;
  base_url: string;
  models_dev_slug: string;
  api_key?: string;
  extra_headers?: string;
  enabled?: boolean;
}

export interface AIModelInput {
  id: string;
  display_name: string;
  context_window: number;
  max_output_tokens: number;
  input_modalities: string[];
  output_modalities: string[];
  reasoning_levels: string[];
  overridden_fields: string[];
  source: string;
  enabled?: boolean;
}

/**
 * Where one model's metadata came from. Without a models.dev slug the server
 * matches the model id across every provider, so `candidates > 1` means the
 * values are a majority reading of several gateways' copies, not a fact.
 */
export interface AIMatchSource {
  slug: string;
  candidates: number;
}

export interface AIModelMatchResult {
  applied: string[];
  unmatched: string[];
  /** "model_id:field" entries a refresh deliberately left alone (frozen). */
  frozen: string[];
  /** model id → provenance; missing on older server builds. */
  sources?: Record<string, AIMatchSource>;
}

/** POST /api/ai/providers/{id}/import-models: match + create + link in one call. */
export interface AIModelImportResult {
  added: string[];
  unmatched: string[];
  frozen: string[];
  sources?: Record<string, AIMatchSource>;
}

export interface ModelsDevSlug {
  slug: string;
  name: string;
  /** May be "" — 26 of 221 providers publish no api base (anthropic, openai, …). */
  base_url: string;
  /** Guessed from the npm hint; "" = beyond the three protocols, operator chooses. */
  protocol: string;
}

export interface ModelsDevStatus {
  loaded: boolean;
  auto_update: boolean;
  providers: number;
  models: number;
  updated_at?: number;
  url?: string;
  last_error?: string;
  slugs: ModelsDevSlug[];
}

/** One entry of a provider's own GET {base}/models listing (fetch-models). */
export interface AIFetchedModel {
  id: string;
  display_name: string;
  /**
   * models.dev has metadata for this id — under the provider's slug when it has
   * one, by model id otherwise (§12.5 修订 2026-09-18). false means the row will
   * be created with hand-fill defaults. Absent on older server builds.
   */
  matched?: boolean;
  /** Already attached to this provider. */
  linked?: boolean;
}

export interface AIFetchedModels {
  models: AIFetchedModel[];
}
