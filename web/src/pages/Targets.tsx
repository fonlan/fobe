import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import type { LatencyTarget, SettingView } from '../types';
import Modal from '../components/Modal';
import { SaveRow, useField } from '../components/SettingsForm';

/**
 * Settings → 延迟测量 (design §16 实现修订 2026-09-16): the target list plus the
 * global measurement cadence (`latency.interval_seconds`), which used to sit on
 * the basic settings page. Both halves of "how latency is measured" — how often
 * and towards what — now live on one page. The frequency card owns its own
 * settings state (the page is a route child of Settings, so it cannot borrow
 * the parent's draft map).
 */
export default function Targets() {
  const { t } = useI18n();
  const [targets, setTargets] = useState<LatencyTarget[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<LatencyTarget | null>(null);
  const [editing, setEditing] = useState<LatencyTarget | null>(null);

  const [name, setName] = useState('');
  const [kind, setKind] = useState<'icmp' | 'tcp'>('icmp');
  const [host, setHost] = useState('');
  const [port, setPort] = useState('443');
  const [busy, setBusy] = useState(false);
  const [formErr, setFormErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const r = await api.listLatencyTargets();
      setTargets(r.targets ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    const p = resolveTargetPort(kind, port);
    if (!name.trim() || !host.trim() || p === null) {
      setFormErr(t('target_invalid'));
      return;
    }
    setBusy(true);
    setFormErr(null);
    try {
      await api.createLatencyTarget(name.trim(), kind, host.trim(), p);
      setName('');
      setHost('');
      await load();
    } catch (ex) {
      setFormErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async (tg: LatencyTarget) => {
    try {
      await api.deleteLatencyTarget(tg.id);
      setDeleting(null);
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      setDeleting(null);
    }
  };

  return (
    // stack-lg: the page stacks three cards now (frequency / new target / list);
    // outside a spacing container they would sit flush against one another.
    <div className="stack-lg">
      <div className="page-head">
        <h2>{t('targets_title')}</h2>
      </div>
      <p className="hint">{t('targets_desc')}</p>

      <LatencyFrequencyCard />

      <form className="card target-form" onSubmit={create}>
        <h3>{t('target_new')}</h3>
        <div className="form-grid">
          <label className="field">
            <span>{t('name')}</span>
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Cloudflare" />
          </label>
          <label className="field">
            <span>{t('target_kind')}</span>
            <select value={kind} onChange={(e) => setKind(e.target.value === 'tcp' ? 'tcp' : 'icmp')}>
              <option value="icmp">ICMP</option>
              <option value="tcp">TCP</option>
            </select>
          </label>
          <label className="field">
            <span>{t('target_host')}</span>
            <input value={host} onChange={(e) => setHost(e.target.value)} placeholder="1.1.1.1" />
          </label>
          <label className="field">
            <span>{t('target_port')}</span>
            <input
              type="number"
              min="1"
              max="65535"
              value={port}
              disabled={kind === 'icmp'}
              onChange={(e) => setPort(e.target.value)}
            />
          </label>
        </div>
        {formErr && <div className="form-error">{formErr}</div>}
        <div className="row-end">
          <button type="submit" className="btn primary" disabled={busy}>
            {busy ? t('loading') : t('target_create')}
          </button>
        </div>
      </form>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {targets !== null && targets.length === 0 && <div className="empty-hint">{t('targets_empty')}</div>}

      {targets !== null && targets.length > 0 && (
        /* Scroll wrapper carries the card — overflow-x on the <table> itself
           never applied (table boxes are not scroll containers). */
        <div className="card table-card">
          <table className="table">
            <thead>
              <tr>
                <th>{t('name')}</th>
                <th>{t('target_kind')}</th>
                <th>{t('target_host')}</th>
                <th>{t('target_port')}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {targets.map((tg) => (
                <tr key={tg.id}>
                  <td>{tg.name}</td>
                  <td>
                    <span className="chip">{tg.kind.toUpperCase()}</span>
                  </td>
                  <td className="mono">{tg.host}</td>
                  <td className="mono">{tg.kind === 'tcp' ? tg.port : '-'}</td>
                  <td className="nowrap">
                    <button type="button" className="btn small" onClick={() => setEditing(tg)}>
                      {t('edit')}
                    </button>{' '}
                    <button type="button" className="btn danger small" onClick={() => setDeleting(tg)}>
                      {t('delete')}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {editing && (
        <EditTargetModal
          target={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void load();
          }}
        />
      )}

      {deleting && (
        <Modal title={t('delete')} onClose={() => setDeleting(null)}>
          <p>{t('delete_target_confirm', { name: deleting.name })}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setDeleting(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" onClick={() => void doDelete(deleting)}>
              {t('delete')}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}

/**
 * Global probe cadence (§16, `latency.interval_seconds`, 1–3600s, default 5).
 * Saving syncs online probes through the `latency_config` frame; the validation
 * itself lives on the server (`err_bad_latency_interval`), so the field stays a
 * plain number input.
 */
function LatencyFrequencyCard() {
  const { t } = useI18n();
  const [settings, setSettings] = useState<Record<string, SettingView>>({});
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [savedMsg, setSavedMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

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

  const field = useField({ settings, draft, setDraft });

  const save = async () => {
    if (busy) return;
    const v = draft['latency.interval_seconds'];
    const payload: Record<string, string> = {};
    if (v !== undefined && v.trim() !== '') payload['latency.interval_seconds'] = v.trim();
    // A cleared field with nothing stored yet must still land the default:
    // sending nothing would leave the key absent and the agent on its built-in
    // cadence without the operator ever seeing the value take effect.
    else if (!settings['latency.interval_seconds']?.value) payload['latency.interval_seconds'] = '5';
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

  return (
    <section className="card">
      <h3>{t('sec_latency')}</h3>
      <p className="hint">{t('latency_interval_hint')}</p>
      {err && <div className="form-error">{err}</div>}
      <div className="form-grid">
        {field('latency.interval_seconds', t('latency_interval_seconds'), { type: 'number', defaultValue: '5' })}
      </div>
      <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void save()} label={t('save')} />
    </section>
  );
}

/**
 * The port a target will actually store: icmp carries none (the server
 * normalizes it away), tcp must be 1–65535. Returns null on invalid input so
 * both the create form and the edit dialog share one rule.
 */
function resolveTargetPort(kind: 'icmp' | 'tcp', port: string): number | null {
  if (kind === 'icmp') return 0;
  const p = parseInt(port, 10);
  if (!isFinite(p) || p <= 0 || p > 65535) return null;
  return p;
}

/**
 * §13 (实现修订 2026-09-18): targets were create-or-delete only — fixing a typo
 * in a host meant deleting the target and re-adding it, which also orphaned the
 * registered samples and renumbered the id. Editing keeps the id (and the nodes
 * that select it) but is honest about the endpoint: changing kind/host/port
 * clears that target's history, because the old samples were measured against
 * another endpoint and would otherwise be drawn as one line.
 */
function EditTargetModal({
  target,
  onClose,
  onSaved,
}: {
  target: LatencyTarget;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [name, setName] = useState(target.name);
  const [kind, setKind] = useState<'icmp' | 'tcp'>(target.kind === 'tcp' ? 'tcp' : 'icmp');
  const [host, setHost] = useState(target.host);
  const [port, setPort] = useState(target.kind === 'tcp' ? String(target.port) : '443');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const resolved = resolveTargetPort(kind, port);
  // Mirror of the server's rule: an icmp row never dials a port, so the 443 that
  // older icmp targets stored must not read as an endpoint change (it would warn
  // about purging history on a plain rename).
  const storedPort = target.kind === 'icmp' ? 0 : target.port;
  const endpointChanged =
    kind !== target.kind ||
    host.trim() !== target.host ||
    (resolved !== null && resolved !== storedPort);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !host.trim() || resolved === null) {
      setErr(t('target_invalid'));
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await api.updateLatencyTarget(target.id, name.trim(), kind, host.trim(), resolved);
      onSaved();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={t('target_edit')} onClose={onClose}>
      <form onSubmit={save}>
        <div className="form-grid">
          <label className="field">
            <span>{t('name')}</span>
            <input value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="field">
            <span>{t('target_kind')}</span>
            <select value={kind} onChange={(e) => setKind(e.target.value === 'tcp' ? 'tcp' : 'icmp')}>
              <option value="icmp">ICMP</option>
              <option value="tcp">TCP</option>
            </select>
          </label>
          <label className="field">
            <span>{t('target_host')}</span>
            <input value={host} onChange={(e) => setHost(e.target.value)} />
          </label>
          <label className="field">
            <span>{t('target_port')}</span>
            <input
              type="number"
              min="1"
              max="65535"
              value={port}
              disabled={kind === 'icmp'}
              onChange={(e) => setPort(e.target.value)}
            />
          </label>
        </div>
        {endpointChanged && <p className="hint">{t('target_edit_purge_hint')}</p>}
        {err && <div className="form-error">{err}</div>}
        <div className="row-end">
          <button type="button" className="btn" onClick={onClose}>
            {t('cancel')}
          </button>
          <button type="submit" className="btn primary" disabled={busy}>
            {busy ? t('loading') : t('save')}
          </button>
        </div>
      </form>
    </Modal>
  );
}
