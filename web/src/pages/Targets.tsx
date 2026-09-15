import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import type { LatencyTarget } from '../types';
import Modal from '../components/Modal';

export default function Targets() {
  const { t } = useI18n();
  const [targets, setTargets] = useState<LatencyTarget[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<LatencyTarget | null>(null);

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
    const p = parseInt(port, 10);
    if (!name.trim() || !host.trim() || !isFinite(p) || p <= 0 || p > 65535) {
      setFormErr(t('err_bad_request'));
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
    <div>
      <div className="page-head">
        <h2>{t('targets_title')}</h2>
      </div>
      <p className="hint">{t('targets_desc')}</p>

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
        <table className="table card">
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
                  <button type="button" className="btn danger small" onClick={() => setDeleting(tg)}>
                    {t('delete')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
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
