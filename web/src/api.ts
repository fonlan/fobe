// All backend requests live here. Cookie-session auth (HttpOnly cookie is sent
// automatically for same-origin requests). Errors are returned as ApiError with
// the backend error *code*; pages localize codes via i18n (design §16).

import type {
  AlertRow,
  AuditRow,
  BlacklistRow,
  CommandRow,
  GeoIPStatus,
  GeoIPUpdateAccepted,
  ImportStats,
  LatencySample,
  LatencyTarget,
  Me,
  MetricsSample,
  NodeDetailData,
  NodeView,
  RegTokenInfo,
  RegTokenRow,
  SessionRow,
  SettingView,
  SingboxCache,
  SingboxCacheStatus,
  SingboxDownloadAccepted,
  SingboxImpact,
  SingboxReleases,
  SingboxStatus,
  SingboxUpdateJob,
  SingboxVersion,
  SubAccessRow,
  SubscriptionRow,
  SubscriptionToken,
  TemplateRow,
  TrafficResp,
  WsEvent,
  AIChatRequest,
  AIChatResponse,
  AIConfirmResponse,
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
  note?: string;
  network?: {
    iface: string;
    mode: string;
    quota_bytes: number | null;
    cycle_days: number | null;
    anchor_at: number | null;
    tz: string;
  };
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

export function nodeLatency(id: string, target: number, from: number): Promise<{ samples: LatencySample[] }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/latency?target=${target}&from=${Math.floor(from)}`);
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

// --- commands ---------------------------------------------------------------

export function listCommands(id: string): Promise<{ commands: CommandRow[] }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/commands`);
}

export function enqueueCommand(
  id: string,
  kind: string,
  payload: unknown,
  risky: boolean,
): Promise<{ id: string }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/commands`, {
    method: 'POST',
    body: { kind, payload, risky },
  });
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

export function listAudit(limit = 200): Promise<{ entries: AuditRow[] }> {
  return request(`/api/audit?limit=${limit}`);
}

export function listAlerts(limit = 200): Promise<{ alerts: AlertRow[] }> {
  return request(`/api/alerts?limit=${limit}`);
}

// --- settings ---------------------------------------------------------------

export function getSettings(): Promise<{ settings: SettingView[] }> {
  return request('/api/settings');
}

export function putSettings(settings: Record<string, string>): Promise<{ ok: boolean }> {
  return request('/api/settings', { method: 'PUT', body: { settings } });
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

/** The plaintext token is in the response body exactly once. */
export function createSubscription(name: string): Promise<SubscriptionToken> {
  return request('/api/subscriptions', { method: 'POST', body: { name } });
}

export function updateSubscription(
  id: string,
  body: { enabled?: boolean; name?: string; template_id?: string | null; ua_filter?: string },
): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}`, { method: 'PUT', body });
}

export function deleteSubscription(id: string): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function setSubscriptionNodes(id: string, nodeIds: string[]): Promise<{ ok: boolean }> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/nodes`, {
    method: 'PUT',
    body: { node_ids: nodeIds },
  });
}

/** Rotate the token: the old URL stops resolving, the new one shows once. */
export function rotateSubscription(id: string): Promise<SubscriptionToken> {
  return request(`/api/subscriptions/${encodeURIComponent(id)}/rotate`, { method: 'POST' });
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

export function singboxSetPort(id: string, port: number): Promise<{ ok: boolean; port: number }> {
  return request(`/api/nodes/${encodeURIComponent(id)}/singbox/port`, {
    method: 'PUT',
    body: { port },
  });
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
  | TerminalEnvelope<TerminalClosePayload>;

/** Send a chat message to the node-bound AI assistant (server proxies the configured upstream). */
export function chatWithAI(body: AIChatRequest): Promise<AIChatResponse> {
  return request<AIChatResponse>('/api/ai/chat', { method: 'POST', body });
}

/** Confirm a pending AI action the backend held back (risky / kill switch). */
export function confirmAIAction(actionId: string): Promise<AIConfirmResponse> {
  return request<AIConfirmResponse>(`/api/ai/actions/${encodeURIComponent(actionId)}/confirm`, { method: 'POST' });
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
