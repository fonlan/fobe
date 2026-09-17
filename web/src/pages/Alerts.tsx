import { useCallback, useEffect, useState } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { kindText, useI18n } from '../i18n';
import { fmtTime } from '../format';
import type { AlertRow } from '../types';

export default function Alerts() {
  const { t } = useI18n();
  const [alerts, setAlerts] = useState<AlertRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const r = await api.listAlerts();
      setAlerts(r.alerts ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div>
      <div className="page-head">
        <h2>{t('alerts_title')}</h2>
      </div>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {alerts !== null && alerts.length === 0 && <div className="empty-hint">{t('alerts_empty')}</div>}

      {alerts !== null && alerts.length > 0 && (
        /* Scroll wrapper carries the card — overflow-x on the <table> itself
           never applied (table boxes are not scroll containers). */
        <div className="card table-card">
          <table className="table">
            <thead>
              <tr>
                <th>{t('audit_time')}</th>
                <th>{t('alert_kind')}</th>
                <th>{t('alert_node')}</th>
                <th>{t('alert_delivered')}</th>
                <th>{t('alert_status')}</th>
              </tr>
            </thead>
            <tbody>
              {alerts.map((a) => (
                <tr key={a.id} className={a.recovered_at == null ? 'alert-active' : ''}>
                  <td className="mono nowrap">{fmtTime(a.created_at)}</td>
                  <td>{kindText(t, a.kind)}</td>
                  <td className="mono">{a.node_id || '-'}</td>
                  <td>{a.delivered_at != null ? t('alert_yes') : t('alert_no')}</td>
                  <td>
                    {a.recovered_at != null ? (
                      <span className="chip status-ok">{t('alert_recovered')}</span>
                    ) : (
                      <span className="chip status-failed">{t('alert_active')}</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
