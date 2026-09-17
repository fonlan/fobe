import { useCallback, useEffect, useState } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { fmtTime } from '../format';
import type { AuditRow } from '../types';

export default function Audit() {
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
    <div>
      <div className="page-head">
        <h2>{t('sec_audit')}</h2>
      </div>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {entries !== null && entries.length === 0 && <div className="empty-hint">{t('audit_empty')}</div>}

      {entries !== null && entries.length > 0 && (
        /* Scroll wrapper carries the card — overflow-x on the <table> itself
           never applied (table boxes are not scroll containers). */
        <div className="card table-card">
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
                  {/* node_name falls back to the raw ID once the node is deleted */}
                  <td>{e.node_name || e.node_id || '-'}</td>
                  <td className="detail-cell" title={e.command}>
                    {e.command || '-'}
                  </td>
                  <td className="mono">{e.source_ip || '-'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
