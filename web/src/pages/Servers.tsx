import { useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import type { NodeView } from '../types';
import Modal from '../components/Modal';
import Flag from '../components/Flag';
import { PencilIcon, TrashIcon } from '../components/Icons';

/**
 * Settings → 服务器: the wall of added servers as a table. Monitoring stays on
 * the overview / node pages; this page is the configuration entry point — the
 * last column opens the per-server edit page and the delete confirmation.
 */
export default function Servers() {
  const { t } = useI18n();
  const [nodes, setNodes] = useState<NodeView[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<NodeView | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const r = await api.listNodes();
      setNodes(r.nodes ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const doDelete = async (node: NodeView) => {
    if (busy) return;
    setBusy(true);
    try {
      await api.deleteNode(node.id);
      setDeleting(null);
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      setDeleting(null);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <div className="page-head">
        <h2>
          {t('nav_servers')}
          {nodes && <span className="count-chip">{t('nodes_count', { n: nodes.length })}</span>}
        </h2>
      </div>
      <p className="hint">{t('servers_desc')}</p>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {nodes !== null && nodes.length === 0 && <div className="empty-hint">{t('servers_empty')}</div>}

      {nodes !== null && nodes.length > 0 && (
        <table className="table card">
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('col_status')}</th>
              <th>{t('ip_col')}</th>
              <th>{t('col_version')}</th>
              <th>{t('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => (
              <tr key={n.id}>
                <td>
                  <span className="server-name-cell">
                    <Flag cc={n.country_code} />
                    {n.name || n.hostname || n.id}
                  </span>
                </td>
                <td>
                  <span className={'dot ' + (n.online ? 'on' : 'off')} title={t(n.online ? 'online' : 'offline')} />
                </td>
                <td className="mono">{n.primary_ip || t('unknown')}</td>
                <td className="mono">{n.agent_version || '-'}</td>
                <td className="nowrap">
                  <Link
                    to={`/settings/servers/${encodeURIComponent(n.id)}`}
                    className="icon-btn"
                    title={t('edit_server')}
                    aria-label={t('edit_server')}
                  >
                    <PencilIcon />
                  </Link>
                  <button
                    type="button"
                    className="icon-btn danger"
                    title={t('delete_server')}
                    aria-label={t('delete_server')}
                    onClick={() => setDeleting(n)}
                  >
                    <TrashIcon />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {deleting && (
        <Modal title={t('delete_server')} onClose={() => setDeleting(null)}>
          <p>{t('delete_server_confirm', { name: deleting.name || deleting.id })}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setDeleting(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" disabled={busy} onClick={() => void doDelete(deleting)}>
              {t('delete')}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
