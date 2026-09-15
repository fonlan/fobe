import { useCallback, useEffect, useRef, useState, type ChangeEvent } from 'react';
import { NavLink, Outlet, useLocation } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { useTheme, type ThemeMode } from '../theme';
import { fmtTime } from '../format';
import type { BlacklistRow, SessionRow, SettingView, AuditRow, ImportStats } from '../types';

const SERVER_KEYS = ['server.public_url'] as const;
const AI_KEYS = ['ai.base_url', 'ai.model', 'ai.api_key', 'ai.default_policy'] as const;
const NOTIFY_KEYS = ['notify.telegram_bot_token', 'notify.telegram_chat_id', 'notify.webhook_url', 'notify.webhook_secret'] as const;
const PROXY_KEYS = ['anytls_password'] as const;

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
    opts?: { password?: boolean; type?: string; select?: { value: string; label: string }[] },
  ) => {
    const sv = settings[key];
    const value = draft[key] ?? (sv?.sensitive ? '' : sv?.value ?? '');
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
          <select value={draft[key] ?? sv?.value ?? ''} onChange={(e) => setDraft((d) => ({ ...d, [key]: e.target.value }))}>
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
            placeholder={sv?.sensitive ? (sv.set ? t('sensitive_set') : t('sensitive_unset')) : (sv?.value ?? '')}
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
          <NavLink to="/settings/subscriptions" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
            {t('nav_subs')}
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
        <NavLink to="/settings/subscriptions" className={({ isActive }) => 'settings-subnav-link' + (isActive ? ' active' : '')}>
          {t('nav_subs')}
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

      <BackupCard />
      <GeoIPCard />
      <BlacklistCard />
      <SessionsCard />
      <AuditCard />

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

function GeoIPCard() {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const doUpload = async (f: File) => {
    setBusy(true);
    setErr(null);
    setDone(false);
    try {
      await api.uploadGeoIPMMDB(f);
      setDone(true);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <h3>{t('sec_geoip')}</h3>
      <p className="hint">{t('geoip_desc')}</p>
      {err && <div className="form-error">{err}</div>}
      {done && <div className="form-ok">{t('geoip_upload_done')}</div>}
      <div className="row-gap">
        <FileButton label={t('geoip_upload')} accept=".mmdb,application/octet-stream" busy={busy} onFile={(f) => void doUpload(f)} />
      </div>
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
      )}
    </section>
  );
}

function AuditCard() {
  const { t } = useI18n();
  const [entries, setEntries] = useState<AuditRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const r = await api.listAudit(200);
      setEntries(r.entries ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <section className="card">
      <h3>{t('sec_audit')}</h3>
      {err && <div className="form-error">{err}</div>}
      {entries !== null && entries.length === 0 && <div className="hint">{t('audit_empty')}</div>}
      {entries !== null && entries.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>{t('audit_time')}</th>
              <th>{t('audit_actor')}</th>
              <th>{t('audit_action')}</th>
              <th>{t('audit_node')}</th>
              <th>{t('audit_detail')}</th>
              <th>{t('audit_source')}</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e, i) => (
              <tr key={i}>
                <td className="mono nowrap">{fmtTime(e.ts)}</td>
                <td>{e.actor}</td>
                <td>
                  <span className="chip">{e.action}</span>
                  {e.risk === 'risky' && <span className="chip status-failed">risky</span>}
                </td>
                <td className="mono">{e.node_id || '-'}</td>
                <td className="detail-cell" title={e.command}>
                  {e.command || '-'}
                </td>
                <td className="mono">{e.source_ip || '-'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
