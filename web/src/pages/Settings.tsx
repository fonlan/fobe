import { useCallback, useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import Modal from '../components/Modal';
import ProgressBar from '../components/ProgressBar';
import { DownloadIcon, PublishIcon, RefreshIcon, RetryIcon, TrashIcon } from '../components/Icons';
import { useI18n } from '../i18n';
import { useTheme, type ThemeMode } from '../theme';
import { fmtBytes, fmtRate, fmtTime } from '../format';
import type {
  AgentUpdateStatus,
  BlacklistRow,
  GeoIPDownload,
  GeoIPStatus,
  SessionRow,
  SettingView,
  ImportStats,
  SingboxCache,
  SingboxCacheVersion,
  SingboxDownload,
  SingboxImpact,
  SingboxReleases,
  SingboxUpdateJob,
} from '../types';

/** Job states that mean the batch is over (mirrors internal/server/singboxupdate). */
const SINGBOX_JOB_TERMINAL = new Set(['done', 'failed']);

/**
 * How long a failed download keeps its row in the version list (seconds). Long
 * enough to read the reason, short enough that a stale failure from yesterday
 * does not sit next to a healthy list.
 */
const DL_FAILURE_VISIBLE = 30 * 60;

const SERVER_KEYS = ['server.public_url'] as const;
const AI_KEYS = ['ai.base_url', 'ai.model', 'ai.api_key', 'ai.default_policy'] as const;
const NOTIFY_KEYS = ['notify.telegram_bot_token', 'notify.telegram_chat_id', 'notify.webhook_url', 'notify.webhook_secret'] as const;
const PROXY_KEYS = ['anytls_password'] as const;
/** §14.1 database refresh policy; the database itself has its own endpoints. */
const GEOIP_KEYS = ['geoip.auto_update', 'geoip.max_age_days', 'geoip.url'] as const;
const AGENT_KEYS = ['agent.auto_update'] as const;
const LATENCY_KEYS = ['latency.interval_seconds'] as const;

function isHTTPURL(value: string): boolean {
  try {
    const url = new URL(value);
    return (url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== '' && url.username === '' && url.password === '' && url.search === '' && url.hash === '';
  } catch {
    return false;
  }
}

export default function Settings() {
  const { t } = useI18n();
  const { mode, setMode } = useTheme();
  const location = useLocation();
  const isBaseSettings = location.pathname === '/settings';

  const [settings, setSettings] = useState<Record<string, SettingView>>({});
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [err, setErr] = useState<string | null>(null);
  const [savedMsg, setSavedMsg] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const r = await api.getSettings();
      const map: Record<string, SettingView> = {};
      for (const s of r.settings ?? []) map[s.key] = s;
      setSettings(map);
      setDraft({});
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const saveGroup = async (keys: readonly string[]) => {
    if (busy) return;
    const publicURLDraft = draft['server.public_url'];
    if (keys.includes('server.public_url')) {
      if (publicURLDraft === undefined) {
        if (!settings['server.public_url']?.value) {
          setErr(t('err_public_url_required'));
          return;
        }
      } else if (!isHTTPURL(publicURLDraft.trim())) {
        setErr(t('err_invalid_public_url'));
        return;
      }
    }
    const payload: Record<string, string> = {};
    for (const k of keys) {
      const v = draft[k];
      if (v !== undefined && v.trim() !== '') payload[k] = v.trim();
      if (k === 'latency.interval_seconds' && v === undefined && !settings[k]?.value) payload[k] = '5';
    }
    // geoip.url is the one field where empty is meaningful: it means "go back
    // to the built-in mirror chain", so clearing it must reach the server even
    // though every other emptied field is skipped.
    if (keys.includes('geoip.url') && draft['geoip.url'] !== undefined && draft['geoip.url'].trim() === '') {
      payload['geoip.url'] = '';
    }
    if (keys.includes('server.public_url') && draft['server.public_url'] !== undefined && payload['server.public_url'] === undefined) {
      setErr(t('err_invalid_public_url'));
      return;
    }
    if (Object.keys(payload).length === 0) return;
    setBusy(true);
    setSavedMsg(null);
    try {
      await api.putSettings(payload);
      await load();
      setSavedMsg(t('settings_saved'));
      window.setTimeout(() => setSavedMsg(null), 3000);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const field = (
    key: string,
    label: string,
    opts?: { password?: boolean; type?: string; select?: { value: string; label: string }[]; defaultValue?: string },
  ) => {
    const sv = settings[key];
    const value = draft[key] ?? (sv?.sensitive ? '' : sv?.value || opts?.defaultValue || '');
    return (
      <label key={key} className="field">
        <span>
          {label}
          {sv?.sensitive && (
            <em className={'sensitive-tag' + (sv.set ? ' set' : '')}>
              {sv.set ? t('sensitive_set') : t('sensitive_unset')}
            </em>
          )}
        </span>
        {opts?.select ? (
          <select value={draft[key] ?? sv?.value ?? opts?.defaultValue ?? ''} onChange={(e) => setDraft((d) => ({ ...d, [key]: e.target.value }))}>
            {opts.select.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : (
          <input
            type={opts?.type ?? (opts?.password ? 'password' : 'text')}
            value={value}
            placeholder={sv?.sensitive ? (sv.set ? t('sensitive_set') : t('sensitive_unset')) : (sv?.value || opts?.defaultValue || '')}
            onChange={(e) => setDraft((d) => ({ ...d, [key]: e.target.value }))}
          />
        )}
      </label>
    );
  };

  if (!isBaseSettings) {
    return (
      <div className="stack-lg">
        <div className="page-head">
          <h2>{t('settings_title')}</h2>
        </div>
        <nav className="settings-subnav" aria-label={t('settings_subnav_label')}>
          <NavLink to="/settings" end className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('settings_basic')}
          </NavLink>
          <NavLink to="/settings/targets" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('nav_targets')}
          </NavLink>
          <NavLink to="/settings/servers" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('nav_servers')}
          </NavLink>
          <NavLink to="/settings/subscriptions" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('nav_subs')}
          </NavLink>
          <NavLink to="/settings/audit" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('nav_audit')}
          </NavLink>
        </nav>
        <Outlet />
      </div>
    );
  }

  return (
    <div className="stack-lg">
      <div className="page-head">
        <h2>{t('settings_title')}</h2>
      </div>
      <nav className="settings-subnav" aria-label={t('settings_subnav_label')}>
        <NavLink to="/settings" end className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('settings_basic')}
        </NavLink>
        <NavLink to="/settings/targets" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('nav_targets')}
        </NavLink>
        <NavLink to="/settings/servers" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('nav_servers')}
        </NavLink>
        <NavLink to="/settings/subscriptions" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('nav_subs')}
        </NavLink>
        <NavLink to="/settings/audit" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('nav_audit')}
        </NavLink>
      </nav>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      <section className="card">
        <h3>{t('sec_access')}</h3>
        <p className="hint">{t('server_public_url_hint')}</p>
        <div className="form-grid">
          {field('server.public_url', t('server_public_url'), { type: 'url' })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(SERVER_KEYS)} label={t('save')} />
      </section>

      <section className="card">
        <h3>{t('sec_ai')}</h3>
        <div className="form-grid">
          {field('ai.base_url', t('ai_base_url'), {})}
          {field('ai.model', t('ai_model'), {})}
          {field('ai.api_key', t('ai_api_key'), { password: true })}
          {field('ai.default_policy', t('ai_default_policy'), {
            select: [
              { value: 'allow', label: t('policy_allow') },
              { value: 'confirm', label: t('policy_confirm') },
            ],
          })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(AI_KEYS)} label={t('save')} />
      </section>

      <section className="card">
        <h3>{t('sec_notify')}</h3>
        <div className="form-grid">
          {field('notify.telegram_bot_token', t('tg_token'), { password: true })}
          {field('notify.telegram_chat_id', t('tg_chat_id'), {})}
          {field('notify.webhook_url', t('webhook_url'), {})}
          {field('notify.webhook_secret', t('webhook_secret'), { password: true })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(NOTIFY_KEYS)} label={t('save')} />
      </section>

      <section className="card">
        <h3>{t('sec_proxy')}</h3>
        <div className="form-grid">
          {field('anytls_password', t('anytls_password'), { password: true })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(PROXY_KEYS)} label={t('save')} />
      </section>

      <section className="card">
        <h3>{t('sec_latency')}</h3>
        <p className="hint">{t('latency_interval_hint')}</p>
        <div className="form-grid">
          {field('latency.interval_seconds', t('latency_interval_seconds'), { type: 'number', defaultValue: '5' })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(LATENCY_KEYS)} label={t('save')} />
      </section>

      <SingboxCacheCard />
      <BackupCard />
      <GeoIPCard
        field={field}
        busy={busy}
        savedMsg={savedMsg}
        onSave={() => void saveGroup(GEOIP_KEYS)}
        saveLabel={t('save')}
      />
      <AgentUpdateCard
        field={field}
        busy={busy}
        savedMsg={savedMsg}
        onSave={() => void saveGroup(AGENT_KEYS)}
        saveLabel={t('save')}
      />
      <BlacklistCard />
      <SessionsCard />

      <section className="card">
        <h3>{t('sec_theme')}</h3>
        <p className="hint">{t('theme_desc')}</p>
        <div className="row-gap">
          {(['light', 'dark', 'system'] as ThemeMode[]).map((m) => (
            <label key={m} className="check-chip">
              <input type="radio" name="theme" checked={mode === m} onChange={() => setMode(m)} />
              {m === 'light' ? t('theme_light') : m === 'dark' ? t('theme_dark') : t('theme_system')}
            </label>
          ))}
        </div>
      </section>
    </div>
  );
}


/**
 * §5.5 agent self-update: the on/off switch plus the reasons it can be off and
 * how many probes are behind. The panel never triggers an update itself — the
 * only operator actions are "retry" (on the node page) and reinstalling a probe
 * whose binary predates the feature.
 */
function AgentUpdateCard({
  field,
  busy,
  savedMsg,
  onSave,
  saveLabel,
}: {
  field: (
    key: string,
    label: string,
    opts?: { password?: boolean; type?: string; select?: { value: string; label: string }[]; defaultValue?: string },
  ) => ReactNode;
  busy: boolean;
  savedMsg: string | null;
  onSave: () => void;
  saveLabel: string;
}) {
  const { t } = useI18n();
  const [status, setStatus] = useState<AgentUpdateStatus | null>(null);

  const load = useCallback(() => {
    api
      .getAgentUpdateStatus()
      .then((r) => setStatus(r.status))
      .catch(() => setStatus(null));
  }, []);

  useEffect(() => {
    load();
  }, [load, savedMsg]);

  const reasonKey = status?.reason
    ? `agent_update_reason_${status.reason}`
    : 'agent_update_reason_not_wired';

  return (
    <section className="card">
      <h3>{t('sec_agent_update')}</h3>
      <p className="hint">{t('agent_update_desc')}</p>
      <div className="form-grid">
        {field('agent.auto_update', t('agent_auto_update'), {
          select: [
            { value: '1', label: t('agent_auto_on') },
            { value: '0', label: t('agent_auto_off') },
          ],
        })}
      </div>
      {status && (
        <div className="hint">
          <div>
            {t('agent_update_target')}: <span className="mono">{status.server_version || '-'}</span>
            {' · '}
            {status.enabled
              ? t('agent_update_behind', { n: status.nodes_behind, total: status.nodes_total })
              : t('agent_update_off_reason', { reason: t(reasonKey as never) })}
          </div>
          {status.enabled && status.nodes_behind > 0 && (
            <div>{t('agent_update_queue')}</div>
          )}
        </div>
      )}
      <SaveRow busy={busy} savedMsg={savedMsg} onSave={onSave} label={saveLabel} />
    </section>
  );
}

function SaveRow({ busy, savedMsg, onSave, label }: { busy: boolean; savedMsg: string | null; onSave: () => void; label: string }) {
  return (
    <div className="row-end">
      {savedMsg && <span className="form-ok">{savedMsg}</span>}
      <button type="button" className="btn primary" disabled={busy} onClick={onSave}>
        {busy ? '…' : label}
      </button>
    </div>
  );
}

/** Hidden-file-input + visible button, so clicks keep the .btn styling. */
function FileButton({
  label,
  accept,
  busy,
  onFile,
}: {
  label: string;
  accept: string;
  busy: boolean;
  onFile: (f: File) => void;
}) {
  const inputRef = useRef<HTMLInputElement>(null);
  return (
    <>
      <button type="button" className="btn" disabled={busy} onClick={() => inputRef.current?.click()}>
        {busy ? '…' : label}
      </button>
      <input
        ref={inputRef}
        type="file"
        accept={accept}
        disabled={busy}
        style={{ display: 'none' }}
        onChange={(e: ChangeEvent<HTMLInputElement>) => {
          const f = e.target.files?.[0];
          e.target.value = ''; // allow re-selecting the same file later
          if (f) onFile(f);
        }}
      />
    </>
  );
}

// --- sing-box artifact cache + one-click batch update (design §9.2) ---

/**
 * Settings section for the server-side artifact cache: one row per local
 * sing-box version, each with the two row actions — publish it to every
 * sing-box node, or delete it from the server's disk.
 *
 * A download started from here (or by the startup auto-download) becomes a row
 * of its own, with its progress bar in place: the list is the single place that
 * answers "what sing-box versions do I have, and what am I fetching".
 *
 * Publishing still goes through the impact list and a second confirmation
 * (§9.5.2): one click here changes every enabled node.
 */
function SingboxCacheCard() {
  const { t } = useI18n();
  const [cache, setCache] = useState<SingboxCache | null>(null);
  const [job, setJob] = useState<SingboxUpdateJob | null>(null);
  const [download, setDownload] = useState<SingboxDownload | null>(null);
  const [releases, setReleases] = useState<SingboxReleases | null>(null);
  const [pick, setPick] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [relBusy, setRelBusy] = useState(false);
  const [impact, setImpact] = useState<SingboxImpact | null>(null);
  const [impactBusy, setImpactBusy] = useState(false);
  const [submitting, setSubmitting] = useState(false);

  const load = useCallback(async () => {
    try {
      const c = await api.singboxCache();
      setCache(c);
      setJob(c.last_update ?? null);
      setDownload(c.download ?? null);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  /**
   * The upstream listing is fetched on demand only (an operator pressing the
   * refresh button or opening the picker), never on page render: it is a
   * multi-megabyte API answer. Failures are non-fatal — the version list from
   * disk is what the section is actually about.
   */
  const loadReleases = useCallback(
    async (refresh = false) => {
      setRelBusy(true);
      try {
        setReleases(await api.singboxReleases(refresh));
      } catch (e) {
        setErr(apiErrorMessage(e, t));
      } finally {
        setRelBusy(false);
      }
    },
    [t],
  );

  // Live progress: the server pushes a full job snapshot for every node it
  // handles (SSE singbox_update), one snapshot per download tick
  // (singbox_download) and pings singbox_cache when the disk changes.
  useEffect(() => {
    const close = api.openEvents((ev) => {
      if (ev.kind === 'singbox_update' && ev.data) {
        setJob(ev.data as SingboxUpdateJob);
      } else if (ev.kind === 'singbox_download' && ev.data) {
        setDownload(ev.data as SingboxDownload);
      } else if (ev.kind === 'singbox_cache') {
        void load();
      }
    });
    return close;
  }, [load]);

  const jobId = job?.id;
  const jobState = job?.state;

  // Polling fallback while a job runs: a dropped socket must not freeze the
  // progress table.
  useEffect(() => {
    if (!jobId || !jobState || SINGBOX_JOB_TERMINAL.has(jobState)) return;
    const h = window.setInterval(() => {
      api
        .singboxUpdateStatus(jobId)
        .then((r) => setJob(r.job))
        .catch(() => {
          /* superseded job: the next cache reload picks up the real one */
        });
    }, 2000);
    return () => window.clearInterval(h);
  }, [jobId, jobState]);

  // Same fallback for the download row: a dropped singbox_download event would
  // otherwise leave it stuck at its last tick. Faster than the job poll — the
  // bar moves in sub-second steps.
  const downloadActive = download?.active === true;
  useEffect(() => {
    if (!downloadActive) return;
    const h = window.setInterval(() => {
      api
        .singboxCache()
        .then((c) => {
          setCache(c);
          setDownload(c.download ?? null);
        })
        .catch(() => {
          /* transient: the next tick retries */
        });
    }, 1500);
    return () => window.clearInterval(h);
  }, [downloadActive]);

  // A finished download/job may have added a version on disk: re-read both the
  // cache and the picker's "already cached" flags.
  useEffect(() => {
    if (jobState === 'done' || (download && download.phase === 'done')) void load();
  }, [jobState, download, load]);

  const label = (prefix: string, value: string) => {
    const key = prefix + value;
    const v = t(key);
    return v === key ? value : v;
  };

  const retry = async () => {
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      await api.singboxRetryCache();
      setMsg(t('sb_cache_retry_queued'));
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const startDownload = async (version: string) => {
    if (!version) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      const r = await api.downloadSingboxVersion(version);
      if (r.cached) {
        setMsg(t('sb_cache_download_cached', { version: r.version }));
      } else {
        setMsg(t('sb_cache_download_started', { version: r.version }));
        if (r.download) setDownload(r.download);
      }
      setPick('');
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const delVersion = async (v: SingboxCacheVersion) => {
    const force = v.refs > 0;
    const question = force
      ? t('sb_cache_delete_refs_confirm', { version: v.version, n: v.refs })
      : t('sb_cache_delete_confirm', { version: v.version, size: fmtBytes(v.size) });
    if (!window.confirm(question)) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      await api.deleteSingboxVersion(v.version, force);
      setMsg(t('sb_cache_deleted', { version: v.version }));
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  /** Row action: confirm the blast radius, then distribute one version. */
  const checkImpact = async (version: string) => {
    if (impactBusy || downloadActive) return;
    setImpactBusy(true);
    setErr(null);
    setMsg(null);
    try {
      setImpact(await api.singboxUpdateImpact(version));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setImpactBusy(false);
    }
  };

  const confirmUpdate = async () => {
    if (!impact || submitting || downloadActive) return;
    setSubmitting(true);
    setErr(null);
    try {
      const r = await api.singboxUpdate({ version: impact.target_version, confirm: true });
      setJob(r.job);
      setImpact(null);
      setMsg(t('sb_update_submitted'));
      await load();
    } catch (e) {
      // A running job is not an error to hide: reload so its progress shows.
      if (e instanceof api.ApiError && e.code === 'update_in_progress') void load();
      setErr(apiErrorMessage(e, t));
    } finally {
      setSubmitting(false);
    }
  };

  const versions = cache?.versions ?? [];
  const status = cache?.cache_status ?? null;
  const totalSize = versions.reduce((sum, v) => sum + (v.size || 0), 0);
  const handled = job
    ? job.counts.already_current + job.counts.pushed + job.counts.offline_pending + job.counts.failed
    : 0;
  const jobRunning = !!jobState && !SINGBOX_JOB_TERMINAL.has(jobState);
  const dlPercent = download?.total ? download.percent ?? 0 : 0;
  // Bytes/speed only: the phase is the bar's label, so repeating it in the
  // text would waste the width the row needs.
  const dlText = download
    ? [
        // Bytes only once there are some: the resolving/waiting phases would
        // otherwise read "0 B".
        download.downloaded > 0
          ? download.total
            ? `${fmtBytes(download.downloaded)} / ${fmtBytes(download.total)}`
            : t('sb_download_bytes_so_far', { done: fmtBytes(download.downloaded) })
          : '',
        download.speed ? fmtRate(download.speed) : '',
      ]
        .filter(Boolean)
        .join(' · ')
    : '';

  /**
   * The in-flight download is a row too — but only while its version is not in
   * the disk list yet (once it is published, the real row takes over). A failed
   * install keeps its row so the reason stays visible next to the version that
   * failed, instead of vanishing with the progress bar.
   *
   * The failure row expires: the server keeps the last snapshot until the next
   * download replaces it, and a day-old "failed" line next to a healthy version
   * list is just noise (the on-disk truth never changed).
   */
  const dlVersion = download?.version ?? '';
  const dlFailedFresh =
    download?.phase === 'failed' &&
    (download.updated_at ?? 0) > 0 &&
    Date.now() / 1000 - (download.updated_at ?? 0) < DL_FAILURE_VISIBLE;
  const dlIsRow =
    !!dlVersion && !versions.some((v) => v.version === dlVersion) && (downloadActive || dlFailedFresh);
  // Every upstream release is listed; the ones already on disk are disabled
  // rather than hidden, so "do I already have this?" is answerable in place.
  const relList = releases?.releases ?? [];

  return (
    <section className="card">
      <div className="row-between">
        <h3>{t('sec_singbox_cache')}</h3>
        {cache && (
          <span className={'chip' + (cache.auto_download ? '' : ' status-timeout')}>
            {cache.auto_download ? t('sb_cache_auto_on') : t('sb_cache_auto_off')}
          </span>
        )}
      </div>
      <p className="hint">{t('sb_cache_desc')}</p>
      {err && <div className="form-error">{err}</div>}
      {msg && <div className="form-ok">{msg}</div>}
      {cache?.mount_applicable && !cache.mount_ok && <p className="form-error">{t('sb_mount_warn')}</p>}

      <div className="tile-grid">
        <div className="tile">
          <div className="tile-label">{t('sb_cache_status')}</div>
          <div className="tile-value">
            {status ? label('sb_cache_state_', status.state) : t('sb_cache_state_empty')}
            {status?.version ? ' · ' + status.version : ''}
          </div>
          <div className="tile-sub">{status?.updated_at ? fmtTime(status.updated_at) : '-'}</div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('sb_cache_latest')}</div>
          <div className="tile-value mono">{cache?.latest_cached || '-'}</div>
          <div className="tile-sub">
            {t('sb_cache_col_size')}: {fmtBytes(totalSize)}
          </div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('sb_cache_dir')}</div>
          <div className="tile-value mono">{cache?.dl_dir || '-'}</div>
        </div>
      </div>

      {status && status.state !== 'ok' && status.state !== 'pending' && (
        <div className="row-wrap" style={{ marginTop: 10 }}>
          {status.error && <span className="form-error">{status.error}</span>}
          <button type="button" className="btn small" disabled={busy || downloadActive} onClick={() => void retry()}>
            {busy ? '…' : t('sb_cache_retry')}
          </button>
        </div>
      )}

      <div className="table-wrap" style={{ marginTop: 12 }}>
        <table className="table" style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>{t('sb_cache_col_version')}</th>
              <th>{t('sb_cache_col_size')}</th>
              <th>{t('sb_cache_col_downloaded')}</th>
              <th>{t('sb_cache_col_refs')}</th>
              <th className="col-actions" />
            </tr>
          </thead>
          <tbody>
            {dlIsRow && download && (
              <tr key={'dl-' + dlVersion} className="row-downloading">
                <td className="mono nowrap">
                  {dlVersion}
                  <span className={'chip' + (download.phase === 'failed' ? ' status-failed' : ' primary-chip')}>
                    {download.phase === 'failed' ? t('sb_cache_download_failed') : t('sb_cache_downloading')}
                  </span>
                </td>
                <td colSpan={3}>
                  {download.phase === 'failed' ? (
                    <span className="form-error mono">{download.error || t('err_internal')}</span>
                  ) : (
                    <ProgressBar
                      tone="plain"
                      label={label('sb_download_phase_', download.phase ?? '')}
                      pct={dlPercent}
                      text={dlText}
                    />
                  )}
                </td>
                <td className="nowrap col-actions">
                  {download.phase === 'failed' && (
                    <button
                      type="button"
                      className="icon-btn"
                      title={t('sb_cache_download_retry')}
                      aria-label={t('sb_cache_download_retry')}
                      disabled={busy || downloadActive}
                      onClick={() => void startDownload(dlVersion)}
                    >
                      <RetryIcon />
                    </button>
                  )}
                </td>
              </tr>
            )}
            {versions.map((v) => (
              <tr key={v.version}>
                <td className="mono nowrap">
                  {v.version}
                  {v.is_latest && <span className="chip primary-chip">{t('sb_cache_latest')}</span>}
                </td>
                <td className="mono nowrap">{fmtBytes(v.size)}</td>
                <td className="mono nowrap">{fmtTime(v.downloaded_at)}</td>
                <td className="mono">{v.refs}</td>
                <td className="nowrap col-actions">
                  <button
                    type="button"
                    className="icon-btn"
                    title={t('sb_cache_publish_title', { version: v.version })}
                    aria-label={t('sb_cache_publish_title', { version: v.version })}
                    disabled={busy || impactBusy || downloadActive || jobRunning}
                    onClick={() => void checkImpact(v.version)}
                  >
                    <PublishIcon />
                  </button>
                  <button
                    type="button"
                    className="icon-btn danger"
                    title={t('sb_cache_delete_title', { version: v.version })}
                    aria-label={t('sb_cache_delete_title', { version: v.version })}
                    disabled={busy}
                    onClick={() => void delVersion(v)}
                  >
                    <TrashIcon />
                  </button>
                </td>
              </tr>
            ))}
            {versions.length === 0 && !dlIsRow && (
              <tr>
                <td colSpan={5} className="hint">
                  {t('sb_cache_empty')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="row-wrap" style={{ marginTop: 12 }}>
        <label className="field inline">
          <span>{t('sb_cache_download_new')}</span>
          <select
            value={pick}
            onFocus={() => {
              if (!releases) void loadReleases(false);
            }}
            onChange={(e) => setPick(e.target.value)}
          >
            <option value="">{releases ? t('sb_cache_pick_placeholder') : t('sb_cache_pick_loading')}</option>
            {relList.map((r) => (
              <option key={r.version} value={r.version} disabled={r.cached}>
                {r.version}
                {r.latest_stable ? ' · ' + t('sb_cache_latest') : ''}
                {r.prerelease ? ' · ' + t('sb_cache_prerelease') : ''}
                {r.cached ? ' · ' + t('sb_cache_pick_cached') : ''}
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          className="btn primary"
          disabled={!pick || busy || downloadActive}
          onClick={() => void startDownload(pick)}
        >
          <DownloadIcon />
          {t('sb_cache_download_btn')}
        </button>
        <button
          type="button"
          className="btn ghost small"
          disabled={relBusy}
          onClick={() => void loadReleases(true)}
          title={t('sb_cache_releases_refresh')}
        >
          <RefreshIcon />
          {relBusy ? '…' : t('sb_cache_releases_refresh')}
        </button>
        {releases && (
          <span className="hint">
            {releases.stale
              ? t('sb_cache_releases_stale')
              : t('sb_cache_releases_fetched', { time: fmtTime(releases.fetched_at) })}
          </span>
        )}
      </div>
      {downloadActive && <div className="hint">{t('sb_download_hint')}</div>}

      {job && <SingboxJobView job={job} handled={handled} label={label} />}

      {impact && (
        <Modal title={t('sb_update_impact_title')} onClose={() => setImpact(null)} wide>
          <p>
            <strong className="mono">{impact.target_version}</strong> —{' '}
            {t('sb_update_impact_count', { n: impact.count, online: impact.online, offline: impact.offline })}
          </p>
          {impact.count === 0 && <p className="form-error">{t('err_no_targets')}</p>}
          {impact.already_current > 0 && <p className="hint">{t('sb_update_impact_current', { n: impact.already_current })}</p>}
          {impact.download_needed && <p className="hint">{t('sb_update_download_needed')}</p>}
          {downloadActive && <p className="form-error">{t('sb_update_blocked_download')}</p>}
          {impact.count > 0 && <p className="hint">{t('sb_update_confirm_hint')}</p>}
          {impact.nodes.length > 0 && (
            <div style={{ maxHeight: 240, overflow: 'auto' }}>
              <table className="table">
                <thead>
                  <tr>
                    <th>{t('name')}</th>
                    <th>{t('sb_current')}</th>
                    <th>{t('sb_desired')}</th>
                    <th>{t('alert_status')}</th>
                  </tr>
                </thead>
                <tbody>
                  {impact.nodes.map((n) => (
                    <tr key={n.node_id}>
                      <td>
                        {n.name || n.node_id}
                        <span className="hint mono"> {n.node_id}</span>
                      </td>
                      <td className="mono">{n.version || '-'}</td>
                      <td className="mono">{n.desired_version || '-'}</td>
                      <td className="nowrap">
                        <span className={'chip' + (n.online ? ' status-ok' : '')}>
                          {n.online ? t('sb_update_node_online') : t('sb_update_node_offline')}
                        </span>
                        {n.already_current && <span className="chip">{t('sb_update_outcome_already_current')}</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setImpact(null)}>
              {t('cancel')}
            </button>
            <button
              type="button"
              className="btn primary"
              disabled={submitting || downloadActive || impact.count === 0}
              onClick={() => void confirmUpdate()}
            >
              {submitting ? '…' : t('sb_update_confirm')}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}

function SingboxJobView({
  job,
  handled,
  label,
}: {
  job: SingboxUpdateJob;
  handled: number;
  label: (prefix: string, value: string) => string;
}) {
  const { t } = useI18n();
  const stateCls = job.state === 'failed' ? 'status-failed' : job.state === 'done' ? 'status-ok' : 'status-timeout';
  const outcomeCls = (o: string) =>
    'chip' + (o === 'failed' ? ' status-failed' : o === 'pushed' ? ' status-ok' : o === 'offline_pending' ? ' status-timeout' : '');
  return (
    <div className="stack" style={{ marginTop: 14 }}>
      <div className="row-between">
        <h4>{t('sb_update_job')}</h4>
        <span className={'chip ' + stateCls}>{label('sb_update_job_state_', job.state)}</span>
      </div>
      <div className="hint">
        {job.target_version ? <span className="mono">{job.target_version} · </span> : null}
        {t('sb_update_started', { time: fmtTime(job.started_at) })}
        {(job.deadline ?? 0) > 0 ? ' · ' + t('sb_update_deadline', { time: fmtTime(job.deadline) }) : ''}
      </div>
      {job.state !== 'failed' && job.counts.total > 0 && (
        <div className="hint">
          {t('sb_update_progress_count', { done: handled, total: job.counts.total })} · {t('sb_update_outcome_pushed')}{' '}
          {job.counts.pushed} · {t('sb_update_outcome_offline_pending')} {job.counts.offline_pending} ·{' '}
          {t('sb_update_outcome_failed')} {job.counts.failed}
        </div>
      )}
      {job.error && <div className="form-error mono">{job.error}</div>}
      {job.stale && job.stale.length > 0 && (
        <div className="form-error">{t('sb_update_stale', { nodes: job.stale.join(', ') })}</div>
      )}
      {job.nodes.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t('name')}</th>
                <th>{t('sb_current')}</th>
                <th>{t('alert_status')}</th>
                <th>{t('audit_detail')}</th>
              </tr>
            </thead>
            <tbody>
              {job.nodes.map((n) => (
                <tr key={n.node_id}>
                  <td>
                    {n.name || n.node_id}
                    <span className="hint mono"> {n.node_id}</span>
                  </td>
                  <td className="mono">{n.version || '-'}</td>
                  <td className="nowrap">
                    <span className={outcomeCls(n.outcome)}>{label('sb_update_outcome_', n.outcome)}</span>
                    {job.convergence_checked && n.converged !== undefined && (
                      <span className={'chip' + (n.converged ? ' status-ok' : ' status-failed')}>
                        {n.converged ? t('sb_update_converged') : t('sb_update_not_converged')}
                      </span>
                    )}
                  </td>
                  <td>{n.reason || '-'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {job.state === 'done' && !job.convergence_checked && (
        <div className="hint">{t('sb_update_unchecked')}</div>
      )}
    </div>
  );
}

function BackupCard() {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [stats, setStats] = useState<ImportStats | null>(null);

  const doExport = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.downloadExport();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const doImport = async (f: File) => {
    let data: unknown;
    try {
      data = JSON.parse(await f.text());
    } catch {
      setErr(t('backup_bad_json'));
      return;
    }
    if (!window.confirm(t('backup_import_confirm', { name: f.name }))) return;
    setBusy(true);
    setErr(null);
    setStats(null);
    try {
      setStats(await api.importBackup(data));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <h3>{t('sec_backup')}</h3>
      <p className="hint">{t('backup_desc')}</p>
      {err && <div className="form-error">{err}</div>}
      <div className="row-gap">
        <button type="button" className="btn" disabled={busy} onClick={() => void doExport()}>
          {t('backup_export')}
        </button>
        <FileButton label={t('backup_import')} accept="application/json,.json" busy={busy} onFile={(f) => void doImport(f)} />
      </div>
      {stats && (
        <div className="form-ok">
          <div>{t('backup_import_done')}</div>
          <ul>
            <li>{t('backup_stat_nodes', { created: stats.nodes_created, updated: stats.nodes_updated })}</li>
            <li>{t('backup_stat_targets', { n: stats.latency_targets_created })}</li>
            <li>{t('backup_stat_subs', { created: stats.subscriptions_created, updated: stats.subscriptions_updated })}</li>
            <li>{t('backup_stat_templates', { created: stats.templates_created, updated: stats.templates_updated })}</li>
            <li>{t('backup_stat_settings', { n: stats.settings_imported })}</li>
          </ul>
        </div>
      )}
    </section>
  );
}

function GeoIPCard({
  field,
  busy,
  savedMsg,
  onSave,
  saveLabel,
}: {
  field: (
    key: string,
    label: string,
    opts?: { password?: boolean; type?: string; select?: { value: string; label: string }[]; defaultValue?: string },
  ) => ReactNode;
  busy: boolean;
  savedMsg: string | null;
  onSave: () => void;
  saveLabel: string;
}) {
  const { t } = useI18n();
  const [status, setStatus] = useState<GeoIPStatus | null>(null);
  const [dl, setDl] = useState<GeoIPDownload | null>(null);
  const [uploading, setUploading] = useState(false);
  const [starting, setStarting] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const s = await api.geoipStatus();
      setStatus(s);
      setDl(s.download);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  // Live progress: the server pushes one snapshot per download tick
  // (geoip_update) and pings geoip_updated when the file changed.
  useEffect(() => {
    const close = api.openEvents((ev) => {
      if (ev.kind === 'geoip_update' && ev.data) {
        setDl(ev.data as GeoIPDownload);
      } else if (ev.kind === 'geoip_updated') {
        void load();
      }
    });
    return close;
  }, [load]);

  const active = dl?.active === true;

  // Polling fallback while an update runs: a dropped socket must not freeze
  // the bar at its last tick.
  useEffect(() => {
    if (!active) return;
    const h = window.setInterval(() => {
      api
        .geoipStatus()
        .then((s) => {
          setStatus(s);
          setDl(s.download);
        })
        .catch(() => {
          /* transient: the next tick retries */
        });
    }, 1500);
    return () => window.clearInterval(h);
  }, [active]);

  // A finished update replaced the file: re-read the status so the data date,
  // size and source are the new ones (the snapshot alone cannot say that).
  const wasActive = useRef(false);
  useEffect(() => {
    if (wasActive.current && !active) void load();
    wasActive.current = active;
  }, [active, load]);

  const updateNow = async () => {
    if (starting || active) return;
    setStarting(true);
    setErr(null);
    setMsg(null);
    try {
      const r = await api.geoipUpdate();
      setMsg(r.accepted ? t('geoip_update_started') : t('geoip_update_running'));
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setStarting(false);
    }
  };

  const doUpload = async (f: File) => {
    setUploading(true);
    setErr(null);
    setMsg(null);
    try {
      await api.uploadGeoIPMMDB(f);
      setMsg(t('geoip_upload_done'));
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setUploading(false);
    }
  };

  const label = (prefix: string, value: string) => {
    const key = prefix + value;
    const v = t(key);
    return v === key ? value : v;
  };

  const percent = dl?.total ? dl.percent ?? 0 : 0;
  // Bytes and speed only: the phase is the bar's label, so repeating it here
  // would waste the row's width.
  const dlText = dl
    ? [
        dl.downloaded > 0
          ? dl.total
            ? `${fmtBytes(dl.downloaded)} / ${fmtBytes(dl.total)}`
            : fmtBytes(dl.downloaded)
          : '',
        dl.speed ? fmtRate(dl.speed) : '',
      ]
        .filter(Boolean)
        .join(' · ')
    : '';
  const stateTone = status?.state === 'failed' ? ' status-failed' : status?.state === 'ok' ? ' status-ok' : ' status-timeout';
  // The updater persists the failure reason on status.error, so the card shows
  // it from one place instead of repeating the live snapshot's copy.
  const stateError = status?.error && (status.state === 'failed' || status.state === 'disabled') ? status.error : null;

  return (
    <section className="card">
      <div className="row-between">
        <h3>{t('sec_geoip')}</h3>
        {status && (
          <span className={'chip' + (status.auto_update ? '' : ' status-timeout')}>
            {status.auto_update ? t('geoip_auto_on') : t('geoip_auto_off')}
          </span>
        )}
      </div>
      <p className="hint">{t('geoip_desc')}</p>
      {err && <div className="form-error">{err}</div>}
      {msg && <div className="form-ok">{msg}</div>}
      {status?.env_locked && <p className="form-error">{t('geoip_env_locked')}</p>}

      <div className="tile-grid">
        <div className="tile">
          <div className="tile-label">{t('geoip_state')}</div>
          <div className="tile-value">
            {status?.state ? <span className={'chip' + stateTone}>{label('geoip_state_', status.state)}</span> : '…'}
            {status && !status.exists && ` ${t('geoip_not_installed')}`}
          </div>
          <div className="tile-sub">
            {status?.updated_at ? t('geoip_updated_at', { time: fmtTime(status.updated_at) }) : t('geoip_never_updated')}
          </div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('geoip_data_date')}</div>
          <div className="tile-value mono">{status?.build_epoch ? fmtTime(status.build_epoch) : '-'}</div>
          <div className="tile-sub">
            {status?.database_type || '-'}
            {status?.exists ? ` · ${fmtBytes(status.size_bytes ?? 0)}` : ''}
          </div>
        </div>
      </div>

      {active && dl && (
        <div style={{ marginTop: 10 }}>
          <ProgressBar tone="plain" label={label('geoip_phase_', dl.phase ?? '')} pct={percent} text={dlText} />
        </div>
      )}
      {!active && stateError && <div className="form-error">{stateError}</div>}

      <div className="row-gap" style={{ marginTop: 12 }}>
        <button type="button" className="btn primary" disabled={starting || active} onClick={() => void updateNow()}>
          {active ? t('geoip_updating') : t('geoip_update_now')}
        </button>
        <FileButton
          label={t('geoip_upload')}
          accept=".mmdb,application/octet-stream"
          busy={uploading || active}
          onFile={(f) => void doUpload(f)}
        />
      </div>

      <div className="form-grid" style={{ marginTop: 14 }}>
        {field('geoip.auto_update', t('geoip_auto_update'), {
          select: [
            { value: '1', label: t('geoip_auto_on') },
            { value: '0', label: t('geoip_auto_off') },
          ],
        })}
        {field('geoip.max_age_days', t('geoip_max_age'), { type: 'number' })}
        {field('geoip.url', t('geoip_url'), { type: 'url' })}
      </div>
      <SaveRow busy={busy} savedMsg={savedMsg} onSave={onSave} label={saveLabel} />
    </section>
  );
}

function BlacklistCard() {
  const { t } = useI18n();
  const [entries, setEntries] = useState<BlacklistRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setEntries(await api.listBlacklist());
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const unblock = async (ip: string) => {
    try {
      await api.unblockIP(ip);
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  };

  return (
    <section className="card">
      <h3>{t('sec_security')}</h3>
      <h4>{t('sec_security')} — IP</h4>
      {err && <div className="form-error">{err}</div>}
      {entries !== null && entries.length === 0 && <div className="hint">{t('blacklist_empty')}</div>}
      {entries !== null && entries.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t('session_ip')}</th>
                <th>{t('fail_count')}</th>
                <th>{t('audit_detail')}</th>
                <th>{t('audit_time')}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => {
                const active = e.expires_at > 0;
                return (
                  <tr key={e.ip}>
                    <td className="mono">{e.ip}</td>
                    <td className="mono">{e.fail_count}</td>
                    <td>
                      {active ? t('blocked_until', { time: fmtTime(e.expires_at) }) : t('counter_only')}
                      {e.reason ? ` · ${e.reason}` : ''}
                    </td>
                    <td className="mono nowrap">{fmtTime(e.created_at)}</td>
                    <td className="nowrap">
                      <button type="button" className="btn small" onClick={() => void unblock(e.ip)}>
                        {t('unblock')}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function SessionsCard() {
  const { t } = useI18n();
  const [sessions, setSessions] = useState<SessionRow[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setSessions(await api.listSessions());
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const revokeAll = async () => {
    setBusy(true);
    try {
      await api.revokeAllSessions();
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <div className="row-between">
        <h3>{t('sec_sessions')}</h3>
        <button type="button" className="btn danger small" disabled={busy} onClick={() => void revokeAll()}>
          {t('session_revoke_all')}
        </button>
      </div>
      {err && <div className="form-error">{err}</div>}
      {sessions !== null && sessions.length === 0 && <div className="hint">{t('sessions_empty')}</div>}
      {sessions !== null && sessions.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t('session_ip')}</th>
                <th>{t('session_created')}</th>
                <th>{t('session_last_seen')}</th>
                <th>{t('session_ua')}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr key={s.id} className={s.revoked ? 'row-muted' : ''}>
                  <td className="mono">{s.ip}</td>
                  <td className="mono nowrap">{fmtTime(s.created_at)}</td>
                  <td className="mono nowrap">{fmtTime(s.last_seen)}</td>
                  <td className="ua-cell" title={s.ua}>
                    {s.ua}
                  </td>
                  <td>{s.revoked && <span className="chip">{t('session_revoked')}</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

