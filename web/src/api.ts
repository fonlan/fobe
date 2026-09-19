// All backend requests live here. Cookie-session auth (HttpOnly cookie is sent
// automatically for same-origin requests). Errors are returned as ApiError with
// the backend error *code*; pages localize codes via i18n (design §16).

import type {
  AgentUpdateStatus,
  AlertRow,
  AuditPage,
  BlacklistRow,
  CommandRow,
  FeishuQRStatus,
  ForwardsStatus,
  ForwardRuleInput,
  NotifyTestResult,
  GeoIPStatus,
  GeoIPUpdateAccepted,
  ImportStats,
  LatencySample,
  LatencyTarget,
  Me,
  MetricsSample,
  NodeDetailData,
  NodeTrafficCycle,
  NodeView,
  RegTokenInfo,
  RegTokenRow,
  SessionRow,
  SettingView,
  SingboxCache,
  SingboxCacheStatus,
  SingboxConfigPayload,
  SingboxInbound,
  SingboxDownloadAccepted,
  SingboxImpact,
  SingboxReleases,
  SingboxStatus,
  SingboxUpdateJob,
  SingboxVersion,
  SubAccessRow,
  SubscriptionEntries,
  SubscriptionEntry,
  SubscriptionEntryInput,
  SubscriptionRow,
  SubscriptionToken,
  TemplateRow,
  TrafficResp,
  WsEvent,
  AIChatRequest,
  AICatalog,
  AIContinueRequest,
  AIErrorEvent,
  AIFetchedModels,
  AINeedsConfirmationEvent,
  AISessionEvent,
  AISessionHistory,
  AIToolResultEvent,
  AITurnEndEvent,
  AIModel,
  AIModelImportResult,
  AIModelInput,
  AIModelMatchResult,
  AIProvider,
  AIProviderInput,
  ModelsDevStatus,
  TerminalBufferPayload,
  TerminalClosePayload,
  TerminalEnvelope,
  TerminalInputPayload,
  TerminalOpenPayload,
  TerminalResizePayload,
} from './types';

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(code: string, status: number) {
    super(code);
    this.code = code;
    this.status = status;
  }
}

type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;

/** Registered by the AuthProvider: clears auth state so routers redirect to /login. */
export function setUnauthorizedHandler(fn: UnauthorizedHandler | null): void {
  onUnauthorized = fn;
}

interface ReqOpts {
  method?: string;
  body?: unknown;
  /** Skip the global 401 redirect (login / password endpoints handle 401 locally). */
  skipUnauthorizedRedirect?: boolean;
}

/**
 * Shared fetch + error mapping for every backend call (JSON and raw alike).
 * `init` wins over the same-origin credentials default.
 */
async function send<T>(path: string, init: RequestInit = {}, skipUnauthorizedRedirect = false): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(path, { credentials: 'same-origin', ...init });
  } catch {
    throw new ApiError('network_error', 0);
  }

  if (resp.status === 401) {
    if (!skipUnauthorizedRedirect) onUnauthorized?.();
    // 登录页等本地处理 401 的端点需要服务端的真实错误码
    // (bad_credentials / ip_blacklisted / not_initialized),不能一律报 unauthorized
    let code = 'unauthorized';
    try {
      const j = (await resp.json()) as { error?: { code?: string } };
      if (j?.error?.code) code = j.error.code;
    } catch {
      // non-JSON error body
    }
    throw new ApiError(code, 401);
  }
  if (!resp.ok) {
    let code = resp.status === 403 ? 'forbidden' : 'internal';
    try {
      const j = (await resp.json()) as { error?: { code?: string } };
      if (j?.error?.code) code = j.error.code;
    } catch {
      // non-JSON error body
    }
    throw new ApiError(code, resp.status);
  }
  // A 2xx whose body is not JSON does not come from this API's contract: it is
  // an HTML page (a backend whose route table predates the call, a proxy error
  // page) or something else entirely. Letting JSON.parse throw here would label
  // it "network_error" — sending the operator to look for DNS problems while
  // the connection is fine. Name the actual symptom instead.
  const text = await resp.text();
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError('bad_response', resp.status);
  }
}

async function request<T>(path: string, opts: ReqOpts = {}): Promise<T> {
  return send<T>(
    path,
    {
      method: opts.method ?? 'GET',
      headers: opts.body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    },
    opts.skipUnauthorizedRedirect,
  );
}

// --- localized error helper -------------------------------------------------

/** Map an unknown thrown value to a localized message. */
export function apiErrorMessage(e: unknown, t: (key: string, vars?: Record<string, string | number>) => string): string {
  const code = e instanceof ApiError ? e.code : 'network_error';
  const key = 'err_' + code;
  const msg = t(key, { code });
  return msg === key ? t('err_default', { code }) : msg;
}

// --- auth -------------------------------------------------------------------

export function login(password: string): Promise<Me> {
  return request<Me>('/api/login', { method: 'POST', body: { password }, skipUnauthorizedRedirect: true }).then(
    (me) => {
      // ThemeProvider listens for this to pull ui.theme after sign-in (§16).
      window.dispatchEvent(new Event('fobe:auth-ready'));
      return me;
    },
  );
}

export function logout(): Promise<{ ok: boolean }> {
  return request('/api/logout', { method: 'POST' });
}

export function me(): Promise<Me> {
  return request<Me>('/api/me', { skipUnauthorizedRedirect: true });
}

export function changePassword(oldPwd: string, newPwd: string): Promise<{ ok: boolean }> {
  return request('/api/password', { method: 'POST', body: { old: oldPwd, new: newPwd }, skipUnauthorizedRedirect: true });
}

// --- nodes ------------------------------------------------------------------

export function listNodes(): Promise<{ nodes: NodeView[] }> {
  return request('/api/nodes');
}

export function getNode(id: string): Promise<NodeDetailData> {
  return request(`/api/nodes/${encodeURIComponent(id)}`);
}

export interface UpdateNodeBody {
  name?: string;
  /** §10.2 subscription display name; '' clears it back to `name`. */
  sub_name?: string;
  note?: string;
  /** §14 手动国旗: "XX" pins the flag, "" clears the pin (back to auto). */
  country_code?: string;
  network?: {
    iface: string;
    mode: string;
    quota_bytes: number | null;
  };
  traffic_cycle?: NodeTrafficCycle;
  billing?: {
    cycle_type: string;
    cycle_days: number | null;
    next_due_at: number | null;
    note: string;
  };
  latency_target_ids?: number[];
}

export function updateNode(id: string, body: UpdateNodeBody): Promise<{ ok: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}`, { method: 'PATCH', body });
}

export function deleteNode(id: string): Promise<{ ok: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function nodeMetrics(id: string, from: number): Promise<{ samples: MetricsSample[] }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/metrics?from=${Math.floor(from)}`);
}

export function nodeTraffic(id: string, days: number): Promise<TrafficResp> {
  return request(`/api/nodes/${encodeURIComponent(id)}/traffic?days=${days}`);
}

/** target='all' pulls every target enabled on the node, averaged into
 * `bucket`-second buckets (design §13); a numeric target returns raw points. */
export function nodeLatency(
  id: string,
  target: number | 'all',
  from: number,
  bucket?: number,
): Promise<{ samples: LatencySample[] }> {
  const b = bucket && bucket > 0 ? `&bucket=${bucket}` : '';
  return request(
    `/api/nodes/${encodeURIComponent(id)}/latency?target=${target}&from=${Math.floor(from)}${b}`,
  );
}

/** §14 手动主 IP: the server validates that the IP is in the node's reported set. */
export function setNodePrimaryIP(id: string, ip: string): Promise<{ ok: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/primary-ip`, { method: 'PUT', body: { ip } });
}

/**
 * §16 5s high-frequency probe stream: enabled on detail-page mount, disabled
 * on unmount. The server 409s (node_offline) when no agent is connected;
 * callers treat every failure as fire-and-forget.
 */
export function probeMetrics(id: string, enabled: boolean): Promise<{ ok: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/probe`, { method: 'POST', body: { enabled } });
}

// --- agent self-update (design §5.5) ---------------------------------------

/** Cluster-wide view: on/off, why, and how many probes are behind. */
export function getAgentUpdateStatus(): Promise<{ status: AgentUpdateStatus }> {
  return request('/api/agent/update');
}

/** Operator retry: clears the attempt counters and nudges an online agent. */
export function retryAgentUpdate(id: string): Promise<{ ok: boolean; pushed: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/agent/retry`, { method: 'POST' });
}

/**
 * Reinstall command, also the credential-reissue path (§4.2 revision). Mints a
 * single-use token **bound to this node**: the probe may re-register onto the
 * same node either by keeping its machine-id + old credentials (the pre-§5.5
 * reinstall case) or with nothing but the token (config.json lost/overwritten).
 */
export function agentReinstallCommand(
  id: string,
): Promise<{ install_command: string; ttl: number; keeps_node?: boolean; reissues?: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/agent/reinstall-command`, { method: 'POST' });
}

// --- commands ---------------------------------------------------------------
// Read-only: the panel can no longer enqueue shell commands (the detail-page
// commands card was removed). This list feeds the AI panel's result polling.

export function listCommands(id: string): Promise<{ commands: CommandRow[] }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/commands`);
}

// The commands table rows currently serialize without json tags (PascalCase
// keys, sql.NullInt64 as {Int64,Valid}); normalize defensively to snake_case.

function nullInt64(v: unknown): number | null {
  if (typeof v === 'number') return v;
  if (v && typeof v === 'object') {
    const o = v as Record<string, unknown>;
    if (o['Valid'] === true && typeof o['Int64'] === 'number') return o['Int64'];
  }
  return null;
}

function normCommand(raw: Record<string, unknown>): CommandRow {
  const g = (...keys: string[]): unknown => {
    for (const k of keys) {
      const v = raw[k];
      if (v !== undefined && v !== null) return v;
    }
    return undefined;
  };
  return {
    id: String(g('id', 'ID') ?? ''),
    node_id: String(g('node_id', 'NodeID') ?? ''),
    kind: String(g('kind', 'Kind') ?? ''),
    payload: String(g('payload', 'Payload') ?? ''),
    status: String(g('status', 'Status') ?? ''),
    created_at: Number(g('created_at', 'CreatedAt') ?? 0),
    sent_at: nullInt64(g('sent_at', 'SentAt')),
    finished_at: nullInt64(g('finished_at', 'FinishedAt')),
    result: String(g('result', 'Result') ?? ''),
  };
}

export async function listCommandsNormalized(id: string): Promise<CommandRow[]> {
  const r = await listCommands(id);
  return (r.commands ?? []).map((c) => normCommand(c as unknown as Record<string, unknown>));
}

// --- registration tokens ----------------------------------------------------

export function createRegToken(name: string, note: string): Promise<RegTokenInfo> {
  return request('/api/reg-tokens', { method: 'POST', body: { name, note } });
}

export function listRegTokens(): Promise<{ tokens: RegTokenRow[] }> {
  return request('/api/reg-tokens?all=1');
}

// --- latency targets --------------------------------------------------------

export function listLatencyTargets(): Promise<{ targets: LatencyTarget[] }> {
  return request('/api/latency-targets');
}

export function createLatencyTarget(name: string, kind: string, host: string, port: number): Promise<{ id: number }> {
  return request('/api/latency-targets', { method: 'POST', body: { name, kind, host, port } });
}

/**
 * §13: rewriting a target's endpoint (kind/host/port) also purges the samples
 * recorded against the old one, so the detail chart never splices two hosts
 * into a single line. Renaming alone keeps the history.
 */
export function updateLatencyTarget(
  id: number,
  name: string,
  kind: string,
  host: string,
  port: number,
): Promise<{ ok: boolean; endpoint_changed: boolean; samples_purged: boolean }> {
  return request(`/api/latency-targets/${id}`, { method: 'PATCH', body: { name, kind, host, port } });
}

export function deleteLatencyTarget(id: number): Promise<{ ok: boolean }> {
  return request(`/api/latency-targets/${id}`, { method: 'DELETE' });
}

// --- security: blacklist / sessions -----------------------------------------

export async function listBlacklist(): Promise<BlacklistRow[]> {
  const r = await request<{ entries: Array<Record<string, unknown>> }>('/api/blacklist');
  return (r.entries ?? []).map((e) => {
    const g = (...keys: string[]): unknown => {
      for (const k of keys) {
        const v = e[k];
        if (v !== undefined && v !== null) return v;
      }
      return undefined;
    };
    return {
      ip: String(g('ip', 'IP') ?? ''),
      reason: String(g('reason', 'Reason') ?? ''),
      fail_count: Number(g('fail_count', 'FailCount') ?? 0),
      created_at: Number(g('created_at', 'CreatedAt') ?? 0),
      expires_at: Number(g('expires_at', 'ExpiresAt') ?? 0),
    };
  });
}

export function unblockIP(ip: string): Promise<{ ok: boolean }> {
  return request(`/api/blacklist/${encodeURIComponent(ip)}`, { method: 'DELETE' });
}

export async function listSessions(): Promise<SessionRow[]> {
  const r = await request<{ sessions: Array<Record<string, unknown>> }>('/api/sessions?all=1');
  return (r.sessions ?? []).map((s) => {
    const g = (...keys: string[]): unknown => {
      for (const k of keys) {
        const v = s[k];
        if (v !== undefined && v !== null) return v;
      }
      return undefined;
    };
    return {
      id: String(g('id', 'ID') ?? ''),
      created_at: Number(g('created_at', 'CreatedAt') ?? 0),
      last_seen: Number(g('last_seen', 'LastSeen') ?? 0),
      ua: String(g('ua', 'UA') ?? ''),
      ip: String(g('ip', 'IP') ?? ''),
      revoked: Boolean(g('revoked', 'Revoked')),
    };
  });
}

export function revokeAllSessions(): Promise<{ ok: boolean }> {
  return request('/api/sessions/revoke-all', { method: 'POST' });
}

// --- audit / alerts ---------------------------------------------------------

/**
 * One numbered page of the audit trail, newest first. `page` is 1-based; the
 * server clamps an out-of-range page and echoes the one it actually served.
 */
export function listAudit(limit = 20, page = 1): Promise<AuditPage> {
  return request(`/api/audit?limit=${limit}&page=${page}`);
}

export function listAlerts(limit = 200): Promise<{ alerts: AlertRow[] }> {
  return request(`/api/alerts?limit=${limit}`);
}

// --- settings ---------------------------------------------------------------

/**
 * GET /api/settings. `ai_configured` is derived server-side (§12.1): it is the
 * only thing that decides whether the assistant can actually send, so the
 * terminal page reads it instead of re-deriving "configured" from the keys.
 */
export interface SettingsResponse {
  settings: SettingView[];
  ai_configured: boolean;
}

export function getSettings(): Promise<SettingsResponse> {
  return request('/api/settings');
}

export function putSettings(settings: Record<string, string>): Promise<{ ok: boolean }> {
  return request('/api/settings', { method: 'PUT', body: { settings } });
}

// --- 飞书 notifications (design §15, scan-to-add onboarding) -----------------

export function startFeishuQR(): Promise<FeishuQRStatus> {
  return request('/api/settings/feishu/qr', { method: 'POST' });
}

export function getFeishuQR(): Promise<FeishuQRStatus> {
  return request('/api/settings/feishu/qr');
}

export function cancelFeishuQR(): Promise<FeishuQRStatus> {
  return request('/api/settings/feishu/qr', { method: 'DELETE' });
}

/** ok=false carries a code (+ upstream detail) the channel card renders inline. */
export function testChannel(channel: 'telegram' | 'feishu' | 'webhook'): Promise<NotifyTestResult> {
  return request('/api/settings/notify/test', { method: 'POST', body: { channel } });
}

export function clearFeishuConfig(mode: 'app' | 'webhook'): Promise<{ ok: boolean }> {
  return request(`/api/settings/feishu/config?mode=${mode}`, { method: 'DELETE' });
}

export type ServerTheme = 'light' | 'dark' | 'system';

/**
 * §16 ui.theme server value for the ThemeProvider bootstrapping. Returns null
 * when unset or when not signed in; never triggers the global 401 redirect.
 */
export async function getServerTheme(): Promise<ServerTheme | null> {
  const r = await request<{ settings: SettingView[] }>('/api/settings', { skipUnauthorizedRedirect: true });
  const row = (r.settings ?? []).find((s) => s.key === 'ui.theme');
  if (row && row.set && (row.value === 'light' || row.value === 'dark' || row.value === 'system')) {
    return row.value;
  }
  return null;
}

// --- backup: export / import (design §17) ------------------------------------

/** Download the JSON snapshot as a file (contains no credential material). */
export async function downloadExport(): Promise<void> {
  let resp: Response;
  try {
    resp = await fetch('/api/export', { credentials: 'same-origin' });
  } catch {
    throw new ApiError('network_error', 0);
  }
  if (resp.status === 401) {
    onUnauthorized?.();
    throw new ApiError('unauthorized', 401);
  }
  if (!resp.ok) throw new ApiError('internal', resp.status);
  const blob = await resp.blob();
  const url = URL.createObjectURL(blob);
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = 'fobe-export.json';
    document.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    URL.revokeObjectURL(url);
  }
}

/** POST a parsed snapshot back; the server merges by machine_id / name. */
export function importBackup(data: unknown): Promise<ImportStats> {
  return request('/api/import', { method: 'POST', body: data });
}

// --- GeoIP database: upload, status & update (design §14/§14.1) --------------

/** Upload a GeoLite2-Country MMDB (raw bytes; the server caps at 64MB). */
export function uploadGeoIPMMDB(file: File): Promise<{ ok: boolean }> {
  return send('/api/geoip/mmdb', {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: file,
  });
}

/** The database on disk, the live resolver and the update policy. */
export function geoipStatus(): Promise<GeoIPStatus> {
  return request('/api/geoip/status');
}

/** Start a manual update. It returns immediately (202); the mirror download
 *  reports progress over /ws/events (`geoip_update`) and the outcome through
 *  geoipStatus(). accepted=false means one was already running. */
export function geoipUpdate(): Promise<GeoIPUpdateAccepted> {
  return request('/api/geoip/update', { method: 'POST' });
}

// --- subscriptions & templates (design §10) ---------------------------------

export function listSubscriptions(): Promise<{ subscriptions: SubscriptionRow[] }> {
  return request('/api/subscriptions');
}

/** Create a subscription. The URL stays retrievable later via subscriptionLink. */
export function createSubscription(name: string): Promise<SubscriptionToken> {
  return request('/api/subscriptions', { method: 'POST', body: { name } });
}

export function updateSubscription(
  id: string,
  body: {
    enabled?: boolean;
    name?: string;
    template_id?: string | null;
    ua_filter?: string;
    /** '' = auto, else 'singbox' / 'clash' (§10 修订). */
    format?: string;
  },
): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}`, { method: 'PUT', body });
}

export function deleteSubscription(id: string): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// The server still accepts the legacy `{node_ids}` shape (an old bundle, or a
// §17 snapshot), but the panel deliberately has no wrapper for it any more:
// that path owns the direct entries only, so calling it from an entry-aware UI
// would leave relay rows untouched behind the operator's back.

/**
 * The server tags every empty string `omitempty`, so an empty relay id, alias,
 * reason, warning or source arrives as a *missing key*. Controlled inputs and
 * t() interpolation need real values (`alias || auto_name` would otherwise
 * render "undefined"). Fill them here rather than at every use site.
 */
function normSubscriptionEntry(e: Partial<SubscriptionEntry>): SubscriptionEntry {
  return {
    node_id: e.node_id ?? '',
    node_name: e.node_name ?? '',
    relay_node_id: e.relay_node_id ?? '',
    relay_name: e.relay_name ?? '',
    proto: e.proto ?? '',
    src_port: e.src_port ?? 0,
    iface: e.iface ?? '',
    auto_name: e.auto_name ?? '',
    alias: e.alias ?? '',
    selected: e.selected === true,
    available: e.available !== false,
    reason: e.reason ?? '',
    warning: e.warning ?? '',
    discovered: e.discovered === true,
    source: e.source ?? '',
  };
}

/**
 * §10.2 entry picker rows: every candidate entry of this subscription (direct
 * plus relayed) unioned with what is already bound. The read is also the
 * server's auto-enrolment trigger, so the picker re-reads it after every save
 * instead of trusting its local copy.
 */
export async function listSubscriptionEntries(id: string): Promise<SubscriptionEntries> {
  const r = await request<{
    entries?: Array<Partial<SubscriptionEntry>>;
    relay_auto_include?: boolean;
    relay_name_format?: string;
  }>(`/api/subscriptions/${encodeURIComponent(id)}/entries`);
  return {
    entries: (r.entries ?? []).map(normSubscriptionEntry),
    // Unset means on (design §10.2), so only an explicit false turns it off.
    relay_auto_include: r.relay_auto_include !== false,
    relay_name_format: r.relay_name_format ?? '',
  };
}

/**
 * Bind entries (§10.2). `entries` must contain every row the picker displayed,
 * including the unchecked ones: the server writes a tombstone for a disabled
 * entry that was bound, and skips candidates that were never bound.
 */
export function setSubscriptionEntries(id: string, entries: SubscriptionEntryInput[]): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/nodes`, {
    method: 'PUT',
    body: { entries },
  });
}

/** Rotate the token: the old URL stops resolving, the new one is shown. */
export function rotateSubscription(id: string): Promise<SubscriptionToken> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/rotate`, { method: 'POST' });
}

/**
 * Re-reveal an existing subscription's URL (§10 实现修订 2026-09-16). Fails with
 * `link_unavailable` (409) when the plaintext is no longer recoverable — the
 * panel then points at rotation instead.
 */
export function subscriptionLink(id: string): Promise<SubscriptionToken> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/link`);
}

export function subscriptionAccess(id: string): Promise<{ logs: SubAccessRow[] }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/access`);
}

export function listTemplates(): Promise<{ templates: TemplateRow[] }> {
  return request('/api/templates');
}

export function createTemplate(body: {
  name: string;
  format: string;
  content: string;
}): Promise<TemplateRow> {
  return request('/api/templates', { method: 'POST', body });
}

export function updateTemplate(
  id: string,
  body: { name: string; format: string; content: string },
): Promise<TemplateRow> {
  return request(`/api/templates/${encodeURIComponent(id)}`, { method: 'PUT', body });
}

export function deleteTemplate(id: string): Promise<{ ok: boolean }> {
  return request(`/api/templates/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// --- sing-box management (design §9) ----------------------------------------

export function singboxVersions(): Promise<{ versions: SingboxVersion[] }> {
  return request('/api/singbox/versions');
}

export function getNodeSingbox(id: string): Promise<{ singbox: SingboxStatus | null }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox`);
}

export function singboxInstall(id: string, version: string): Promise<{ ok: boolean; port: number }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/install`, {
    method: 'POST',
    body: { version },
  });
}

export function singboxAction(id: string, action: 'start' | 'stop' | 'restart'): Promise<{ id: string }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/${action}`, { method: 'POST', body: {} });
}

/**
 * Remove sing-box from a node (§9.2). A desired-state write, not a one-shot
 * command: an offline probe converges on its next handshake, and the node is
 * excluded from every later batch update until it is installed again.
 */
export function singboxUninstall(id: string): Promise<{ ok: boolean }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/uninstall`, { method: 'POST', body: {} });
}

/** The editable inbound list of the probe's own config.json (§9.3 实现修订
 *  2026-09-17b). The file is the source of truth; this is what the panel shows
 *  and edits, and `reported_hash` is the concurrency guard for writes. */
export function singboxConfig(id: string): Promise<SingboxConfigPayload & { hash?: string }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/config`);
}

/** Read-merge-write one batch of edits back onto that file and restart. */
export function singboxConfigEdit(
  id: string,
  body: {
    reported_hash?: string;
    add?: (Partial<SingboxInbound> & { new?: boolean })[];
    update?: Partial<SingboxInbound>[];
    delete?: number[];
  },
): Promise<{ ok: boolean; inbounds: number }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/config`, { method: 'PUT', body });
}

/** Ask the probe to re-read its config now (the panel's "I just edited the
 *  file" path — the subscription renders from the last reported file). */
export function singboxRefresh(id: string): Promise<{ ok: boolean; command_id: string }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/refresh`, { method: 'POST', body: {} });
}

// --- nftables port forwarding (design §21) ----------------------------------

/**
 * The probe's port forwards. Without `live` this reads the server's last
 * snapshot (instant, works while the probe is offline); with it, the probe is
 * asked and the answer replaces the snapshot — the way to see rules an external
 * script (nfpf.sh) added a moment ago.
 */
export function nodeForwards(id: string, live = false): Promise<ForwardsStatus> {
  return request(`/api/nodes/${encodeURIComponent(id)}/forwards${live ? '?live=1' : ''}`);
}

export function addNodeForward(id: string, rule: ForwardRuleInput): Promise<ForwardsStatus> {
  return request(`/api/nodes/${encodeURIComponent(id)}/forwards`, { method: 'POST', body: { rule } });
}

/** Replace one rule. `old` must carry the handle of the rule it replaces. */
export function updateNodeForward(id: string, old: ForwardRuleInput, rule: ForwardRuleInput): Promise<ForwardsStatus> {
  return request(`/api/nodes/${encodeURIComponent(id)}/forwards`, { method: 'PUT', body: { old, rule } });
}

export function deleteNodeForward(id: string, rule: ForwardRuleInput): Promise<ForwardsStatus> {
  return request(`/api/nodes/${encodeURIComponent(id)}/forwards`, { method: 'DELETE', body: { rule } });
}

// --- server artifact cache + one-click batch update (design §9.2) -------------

/** Cached releases, startup download state, mount verdict and the last batch. Local-only. */
export function singboxCache(): Promise<SingboxCache> {
  return request('/api/singbox/cache');
}

/** Manual retry behind a failed startup download; the download runs in the background. */
export function singboxRetryCache(): Promise<{ ok: boolean; status: SingboxCacheStatus }> {
  return request('/api/singbox/cache/retry', { method: 'POST' });
}

/** Affected nodes for a target ("latest" or an explicit cached version). */
export function singboxUpdateImpact(target: string): Promise<SingboxImpact> {
  const v = target.trim() === '' ? 'latest' : target.trim();
  return request(`/api/singbox/update/impact?version=${encodeURIComponent(v)}`);
}

/**
 * Start a distribution batch. confirm is the second gate behind the dialog:
 * the backend refuses without it. Returns the job to render progress from.
 */
export function singboxUpdate(body: { version?: string; latest?: boolean; confirm: true }): Promise<{ job: SingboxUpdateJob }> {
  return request('/api/singbox/update', { method: 'POST', body });
}

/** Progress of one job id (a mismatch 404s, guarding against a stale page). */
export function singboxUpdateStatus(jobId: string): Promise<{ job: SingboxUpdateJob }> {
  return request(`/api/singbox/update/${encodeURIComponent(jobId)}`);
}

/** Delete a cached release; force is required while nodes still reference it. */
export function deleteSingboxVersion(version: string, force = false): Promise<{ ok: boolean; version: string }> {
  const q = force ? '?force=1' : '';
  return request(`/api/singbox/versions/${encodeURIComponent(version)}${q}`, { method: 'DELETE' });
}

/**
 * Upstream releases for the "download a new version" picker. refresh bypasses
 * the server's 10-minute listing cache (the panel's refresh button).
 */
export function singboxReleases(refresh = false): Promise<SingboxReleases> {
  return request(`/api/singbox/releases${refresh ? '?refresh=1' : ''}`);
}

/**
 * Fetch one explicitly named version into the server's cache. Returns as soon
 * as the job is accepted; the row it creates is fed by the
 * `singbox_download` event and GET /api/singbox/cache.
 */
export function downloadSingboxVersion(version: string): Promise<SingboxDownloadAccepted> {
  return request(`/api/singbox/versions/${encodeURIComponent(version)}/download`, { method: 'POST' });
}

// --- terminal / AI -----------------------------------------------------------

export function terminalWebSocketURL(nodeID: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}/ws/terminal?node=${encodeURIComponent(nodeID)}`;
}

export function terminalEnvelope<TPayload>(type: TerminalEnvelope<TPayload>['type'], payload?: TPayload): TerminalEnvelope<TPayload> {
  return { v: 1, type, ts: Math.floor(Date.now() / 1000), payload };
}

export type TerminalClientEnvelope =
  | TerminalEnvelope<TerminalOpenPayload>
  | TerminalEnvelope<TerminalInputPayload>
  | TerminalEnvelope<TerminalResizePayload>
  | TerminalEnvelope<TerminalClosePayload>
  // The answer to a `terminal_query` (§12.7.1). It is a client → server frame
  // like the others, but the server never forwards it to the agent.
  | TerminalEnvelope<TerminalBufferPayload>;

// --- AI streaming chat (SSE, design §12.6) -----------------------------------
//
// POST /api/ai/chat answers with `text/event-stream`, so it cannot go through
// `request<T>()` (which parses one JSON body) and cannot use EventSource either:
// EventSource only issues GETs and cannot carry a request body or an abort
// signal. The stream is therefore read with fetch + ReadableStream and parsed
// here, once, so pages never re-implement framing.

/** Typed handlers for the §12.6 event names. Unknown events are ignored. */
export interface AIStreamHandlers {
  /** `session` — first frame; the conversation id to persist for replay. */
  onSession?: (event: AISessionEvent) => void;
  /** `text_delta` — append to the current assistant bubble. */
  onTextDelta?: (text: string) => void;
  /** `thinking_delta` — append to the collapsed thinking block. */
  onThinkingDelta?: (text: string) => void;
  /** `tool_result` — one tool finished (status/command/exit_code). */
  onToolResult?: (event: AIToolResultEvent) => void;
  /** `needs_confirmation` — the stream ends right after; answer via continue. */
  onNeedsConfirmation?: (event: AINeedsConfirmationEvent) => void;
  /** `turn_end` — why the turn stopped; must be shown to the operator. */
  onTurnEnd?: (event: AITurnEndEvent) => void;
  /** `error` — mid-stream failure (upstream/internal/…). */
  onError?: (event: AIErrorEvent) => void;
}

/**
 * True when a thrown value is our own "the caller aborted this" marker. The
 * stop button aborts the fetch, and §12.6 makes that a normal cancellation (the
 * server keeps the committed half-turn) — it must not be rendered as an error.
 */
export function isAbortError(e: unknown): boolean {
  return e instanceof ApiError && e.code === 'aborted';
}

/**
 * Did this failure come from our own AbortController?
 *
 * Two checks on purpose: the abort reason is not standardized across engines
 * (a DOMException in Chrome/Firefox, historically a plain Error elsewhere), and
 * `signal.aborted` alone also covers the race where the stop button lands while
 * the request is failing for an unrelated reason — in which case "the operator
 * stopped it" is the more useful explanation than "network error".
 */
function abortedBy(e: unknown, signal?: AbortSignal): boolean {
  if (signal?.aborted) return true;
  return typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError';
}

/** One SSE frame after framing: the event name plus its joined data lines. */
interface SSEMessage {
  event: string;
  data: string;
}

/**
 * Find the end of the first complete event in `buffer`.
 *
 * SSE separates events by a BLANK LINE, which browsers write as `\n\n` but may
 * write as `\r\n\r\n` (and a proxy may rewrite). Returns the index of the
 * separator plus its length, or null when the buffer holds only a partial
 * event. A trailing lone `\r` legitimately means "wait for the next chunk" —
 * deciding early would split `\r\n`.
 */
function findSSEEventEnd(buffer: string): { index: number; length: number } | null {
  for (let i = 0; i < buffer.length; i++) {
    const c = buffer[i];
    if (c === '\n') {
      if (buffer[i + 1] === '\n') return { index: i, length: 2 };
      if (buffer[i + 1] === '\r' && buffer[i + 2] === '\n') return { index: i, length: 3 };
    } else if (c === '\r') {
      if (buffer[i + 1] === '\r') return { index: i, length: 2 };
      if (buffer[i + 1] === '\n') {
        if (buffer[i + 2] === '\n') return { index: i, length: 3 };
        if (buffer[i + 2] === '\r' && buffer[i + 3] === '\n') return { index: i, length: 4 };
      }
    }
  }
  return null;
}

/** Parse one event block's fields (`event:` / `data:`; comments and unknown
 *  fields are ignored, and several `data:` lines join with `\n` per spec). */
function parseSSEBlock(block: string): SSEMessage | null {
  let event = '';
  const data: string[] = [];
  for (const line of block.split(/\r\n|\n|\r/)) {
    if (line === '' || line.startsWith(':')) continue;
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') event = value;
    else if (field === 'data') data.push(value);
  }
  if (data.length === 0) return null;
  return { event: event || 'message', data: data.join('\n') };
}

/** Route one decoded frame to the typed handler. A frame whose JSON does not
 *  parse is DROPPED: one bad event must not tear down the rest of the turn. */
function dispatchAIEvent(message: SSEMessage, handlers: AIStreamHandlers): void {
  let payload: unknown;
  try {
    payload = JSON.parse(message.data);
  } catch {
    return;
  }
  const obj = payload !== null && typeof payload === 'object' ? (payload as Record<string, unknown>) : {};
  const text = typeof obj.text === 'string' ? obj.text : '';
  switch (message.event) {
    case 'session':
      handlers.onSession?.(obj as unknown as AISessionEvent);
      break;
    case 'text_delta':
      if (text) handlers.onTextDelta?.(text);
      break;
    case 'thinking_delta':
      if (text) handlers.onThinkingDelta?.(text);
      break;
    case 'tool_result':
      handlers.onToolResult?.(obj as unknown as AIToolResultEvent);
      break;
    case 'needs_confirmation':
      handlers.onNeedsConfirmation?.(obj as unknown as AINeedsConfirmationEvent);
      break;
    case 'turn_end':
      handlers.onTurnEnd?.(obj as unknown as AITurnEndEvent);
      break;
    case 'error':
      handlers.onError?.({ code: typeof obj.code === 'string' ? obj.code : 'internal', message: typeof obj.message === 'string' ? obj.message : undefined });
      break;
    case 'tool_call':
      // The backend does not emit this today (§12.6 sends tool_result only).
      // Ignored on purpose: half-rendering a tool before it ran would show a
      // command that may still be refused by the budget gate.
      break;
    default:
      break;
  }
}

/** Consume everything `buffer` already contains, returning the remainder. */
function drainSSEBuffer(buffer: string, handlers: AIStreamHandlers): string {
  let rest = buffer;
  for (;;) {
    const end = findSSEEventEnd(rest);
    if (!end) return rest;
    const block = rest.slice(0, end.index);
    rest = rest.slice(end.index + end.length);
    const message = parseSSEBlock(block);
    if (message) dispatchAIEvent(message, handlers);
  }
}

/**
 * Open one SSE request and pump it into `handlers`.
 *
 * Failure modes are split on purpose: anything before the response headers is a
 * normal JSON error (ApiError with the backend code); anything after is an
 * `error` EVENT inside the stream, because a 200 has already been written.
 */
async function streamAI(path: string, body: unknown, handlers: AIStreamHandlers, signal?: AbortSignal): Promise<void> {
  let resp: Response;
  try {
    resp = await fetch(path, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(body),
      signal,
    });
  } catch (e) {
    if (abortedBy(e, signal)) throw new ApiError('aborted', 0);
    throw new ApiError('network_error', 0);
  }
  if (resp.status === 401 || !resp.ok) {
    if (resp.status === 401) onUnauthorized?.();
    let code = resp.status === 401 ? 'unauthorized' : resp.status === 403 ? 'forbidden' : 'internal';
    try {
      const j = (await resp.json()) as { error?: { code?: string } };
      if (j?.error?.code) code = j.error.code;
    } catch {
      // non-JSON error body
    }
    throw new ApiError(code, resp.status);
  }
  if (!resp.body) throw new ApiError('bad_response', resp.status);

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      // `stream: true` keeps a multi-byte UTF-8 character split across two
      // chunks from being decoded as mojibake — Chinese deltas are the common
      // case here, and they are exactly 3 bytes wide.
      buffer += decoder.decode(value, { stream: true });
      buffer = drainSSEBuffer(buffer, handlers);
    }
    buffer += decoder.decode();
  } catch (e) {
    if (abortedBy(e, signal)) throw new ApiError('aborted', 0);
    throw new ApiError('network_error', 0);
  }
  // A final event may lack its terminating blank line if the connection closed
  // right after it; parse what is left rather than dropping the last frame.
  if (buffer.trim() !== '') {
    const message = parseSSEBlock(buffer);
    if (message) dispatchAIEvent(message, handlers);
  }
}

/** One user message driving the server-side autonomous loop (§12.6). */
export function streamAIChat(body: AIChatRequest, handlers: AIStreamHandlers, signal?: AbortSignal): Promise<void> {
  return streamAI('/api/ai/chat', body, handlers, signal);
}

/**
 * Answer a pending confirmation and resume the turn (§12.6). `approved: false`
 * is a first-class outcome: the model is told the action was rejected by the
 * operator, so it can explain or propose a different plan.
 */
export function streamAIContinue(body: AIContinueRequest, handlers: AIStreamHandlers, signal?: AbortSignal): Promise<void> {
  return streamAI('/api/ai/chat/continue', body, handlers, signal);
}

/**
 * Restore a session's transcript. This is what makes a reload show the
 * reasoning blocks again: `thinking` is extracted server-side per message, so
 * the panel renders it without knowing which dialect produced it.
 */
export function getAISession(id: string): Promise<AISessionHistory> {
  return request<AISessionHistory>(`/api/ai/sessions/${encodeURIComponent(id)}`);
}

// --- AI providers & models (design §12.1/§12.5) ------------------------------

/**
 * Everything the AI settings page and the model picker need in one round trip
 * (providers + models + the default pair). The server computes the per-provider
 * effective reasoning levels, so the panel never re-derives the protocol rule.
 *
 * The server normalizes its lists (nil slices serialize as `[]`), but a `null`
 * array would crash the page on the first `.length`, so the shape is repaired on
 * arrival — the same stance normCommand / normSubscriptionEntry take.
 */
export async function getAICatalog(): Promise<AICatalog> {
  const raw = await request<Partial<AICatalog>>('/api/ai/catalog');
  return {
    providers: (raw.providers ?? []).map((p) => ({
      ...p,
      header_names: p.header_names ?? [],
      model_ids: p.model_ids ?? [],
      models: (p.models ?? []).map((m) => ({ ...m, effective_levels: m.effective_levels ?? [] })),
    })),
    models: (raw.models ?? []).map((m) => ({
      ...m,
      input_modalities: m.input_modalities ?? [],
      output_modalities: m.output_modalities ?? [],
      reasoning_levels: m.reasoning_levels ?? [],
      overridden_fields: m.overridden_fields ?? [],
      provider_ids: m.provider_ids ?? [],
    })),
    default_provider_id: raw.default_provider_id ?? '',
    default_model_id: raw.default_model_id ?? '',
    default_reasoning: raw.default_reasoning ?? '',
  };
}

export function createAIProvider(body: AIProviderInput): Promise<AIProvider> {
  return request('/api/ai/providers', { method: 'POST', body });
}

/**
 * Same body as create. api_key / extra_headers omitted = leave the stored
 * secret alone; "" = clear it. The panel never receives the ciphertext back.
 */
export function updateAIProvider(id: string, body: AIProviderInput): Promise<AIProvider> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}`, { method: 'PATCH', body });
}

/** Deletes the provider AND every model link it holds — confirm before calling. */
export function deleteAIProvider(id: string): Promise<{ ok: boolean }> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function linkAIProviderModels(id: string, modelIDs: string[]): Promise<{ ok?: boolean }> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}/models`, { method: 'POST', body: { model_ids: modelIDs } });
}

export function unlinkAIProviderModel(id: string, modelID: string): Promise<{ ok?: boolean }> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}/models/${encodeURIComponent(modelID)}`, { method: 'DELETE' });
}

/**
 * Match metadata + create missing rows + link, in ONE call (§12.5): a model the
 * upstream advertises but models.dev does not know must still become a row the
 * operator can fill by hand, and splitting this into two requests would leave a
 * half-applied selection whenever the second one fails.
 *
 * `unmatched` entries were created with hand-fill defaults — the UI must say so.
 */
export function importAIProviderModels(id: string, modelIDs: string[]): Promise<AIModelImportResult> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}/import-models`, { method: 'POST', body: { model_ids: modelIDs } });
}

/**
 * Ask the provider for its own model listing (`GET {base}/models`).
 *
 * The endpoint is part of a later stage and may not exist yet on the running
 * server; callers must treat unknown_endpoint / not_found as "not available
 * here" and fall back to typing model ids by hand, not as a crash.
 */
export function fetchAIProviderModels(id: string): Promise<AIFetchedModels> {
  return request(`/api/ai/providers/${encodeURIComponent(id)}/fetch-models`, { method: 'POST' });
}

/** Create or update a model row. `overridden_fields` marks hand-edited fields. */
export function saveAIModel(body: AIModelInput): Promise<AIModel> {
  return request('/api/ai/models', { method: 'POST', body });
}

export function deleteAIModel(id: string): Promise<{ ok: boolean }> {
  return request(`/api/ai/models/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

/** Refill model rows from models.dev; fields in overridden_fields are left alone. */
export function matchAIModels(providerID: string, modelIDs: string[]): Promise<AIModelMatchResult> {
  return request('/api/ai/models/match', { method: 'POST', body: { provider_id: providerID, model_ids: modelIDs } });
}

/**
 * The default is a (provider, model) PAIR: the same model can hang off several
 * gateways, so a bare model id could not say which one to call. Both ids must
 * be given together; an empty pair clears the default. `reasoningLevel` is the
 * thinking level a turn gets when its picker is left at "unset" — independent
 * of the pair, so it travels (and clears) on its own.
 */
export function setAIDefaults(providerID: string, modelID: string, reasoningLevel = ''): Promise<{ ok: boolean }> {
  return request('/api/ai/defaults', { method: 'PUT', body: { provider_id: providerID, model_id: modelID, reasoning_level: reasoningLevel } });
}

/** models.dev cache state + the provider slugs the form offers. */
export function getModelsDev(): Promise<ModelsDevStatus> {
  return request('/api/ai/modelsdev');
}

/** Force a metadata fetch (manual only; the server also refreshes daily). */
export function refreshModelsDev(): Promise<{ ok: boolean }> {
  return request('/api/ai/modelsdev/refresh', { method: 'POST' });
}


/**
 * Subscribe to /ws/events. Returns a close function. Auto-reconnects with
 * capped exponential backoff; pages additionally poll as a fallback.
 */
export function openEvents(onEvent: (ev: WsEvent) => void): () => void {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  let closed = false;
  let ws: WebSocket | null = null;
  let attempt = 0;
  let timer: number | undefined;

  const connect = () => {
    if (closed) return;
    try {
      ws = new WebSocket(`${proto}//${location.host}/ws/events`);
    } catch {
      scheduleRetry();
      return;
    }
    ws.onopen = () => {
      attempt = 0;
    };
    ws.onmessage = (e: MessageEvent) => {
      try {
        const ev = JSON.parse(String(e.data)) as WsEvent;
        if (ev && typeof ev.kind === 'string') onEvent(ev);
      } catch {
        // ignore malformed frames
      }
    };
    ws.onclose = () => {
      ws = null;
      scheduleRetry();
    };
    ws.onerror = () => {
      ws?.close();
    };
  };

  const scheduleRetry = () => {
    if (closed) return;
    const delay = Math.min(1000 * 2 ** attempt, 30000);
    attempt++;
    timer = window.setTimeout(connect, delay);
  };

  connect();
  return () => {
    closed = true;
    if (timer !== undefined) window.clearTimeout(timer);
    ws?.close();
    ws = null;
  };
}
