import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n, type TFn } from '../i18n';
import type { NodeView, RegTokenInfo } from '../types';
import Modal from '../components/Modal';
import Flag from '../components/Flag';
import { CopyIcon, PencilIcon, TrashIcon } from '../components/Icons';
import { useToast } from '../components/Toast';
import { fmtDueDuration, copyText } from '../format';

type AddStep = 'closed' | 'form' | 'done';

function dueText(node: NodeView, t: TFn): string {
  if (!node.billing_configured || node.next_due_at == null) return '';
  const due = fmtDueDuration(node.next_due_at);
  if (!due) return '';
  if (due.overdue) return t('due_overdue_for', { time: due.duration });
  return t('due_in', { time: due.duration });
}

/**
 * Settings → 服务器: the wall of added servers as a table. Monitoring stays on
 * the overview / node pages; this page is the configuration entry point — the
 * last column opens the per-server edit page and the delete confirmation. The
 * "add node" dialog lives here too (moved off the overview page): joining a
 * probe is an onboarding/config action, not a monitoring one.
 */
export default function Servers() {
  const { t } = useI18n();
  // Page-level feedback for the per-row IP copy button (same pattern as the
  // subscription URL): a copy is invisible, so a click owes an answer.
  const { show: showToast, node: toastNode } = useToast();
  const [nodes, setNodes] = useState<NodeView[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<NodeView | null>(null);
  const [busy, setBusy] = useState(false);
  const [addStep, setAddStep] = useState<AddStep>('closed');

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

  const doCopyIP = async (ip: string) => {
    if (await copyText(ip)) {
      showToast(t('ip_copied'));
    } else {
      // Non-HTTPS origins have no clipboard: say so instead of failing silently.
      showToast(t('copy_manual'), 'error');
    }
  };

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
        <button type="button" className="btn primary" onClick={() => setAddStep('form')}>
          + {t('add_node')}
        </button>
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
        /* The scroll wrapper carries the card: overflow-x on the <table> itself
           never applied (overflow doesn't fire on table boxes), so wide rows
           used to widen the whole page on a phone instead of scrolling here. */
        <div className="card table-card">
          <table className="table">
            <thead>
              <tr>
                <th>{t('name')}</th>
                <th>{t('col_status')}</th>
                <th>{t('primary_ip_col')}</th>
                <th>{t('expiry_col')}</th>
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
                  <td className="nowrap">
                    <span className={'dot ' + (n.online ? 'on' : 'off')} title={t(n.online ? 'online' : 'offline')} />
                    <span className="status-label">{t(n.online ? 'online' : 'offline')}</span>
                  </td>
                  <td className="mono nowrap">
                    {n.primary_ip ? (
                      <>
                        {n.primary_ip}
                        <button
                          type="button"
                          className="icon-btn ip-copy-btn"
                          title={t('copy_ip')}
                          aria-label={t('copy_ip')}
                          onClick={() => void doCopyIP(n.primary_ip)}
                        >
                          <CopyIcon />
                        </button>
                      </>
                    ) : (
                      '-'
                    )}
                  </td>
                  <td>{dueText(n, t)}</td>
                  <td className="mono">
                    {n.agent_version || '-'}
                    {/* §5.5: the target is shown next to the reported version so a
                        probe that has not followed yet is visible without opening it. */}
                    {n.agent_target_version && n.agent_target_version !== n.agent_version && (
                      <span className="hint"> → {n.agent_target_version}</span>
                    )}
                  </td>
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
        </div>
      )}

      {toastNode}

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

      {addStep !== 'closed' && <AddNodeModal step={addStep} setStep={setAddStep} onCreated={() => void load()} />}
    </div>
  );
}

function AddNodeModal({
  step,
  setStep,
  onCreated,
}: {
  step: AddStep;
  setStep: (s: AddStep) => void;
  onCreated: () => void;
}) {
  const { t } = useI18n();
  const [name, setName] = useState('');
  const [note, setNote] = useState('');
  const [info, setInfo] = useState<RegTokenInfo | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    const trimmedName = name.trim();
    if (!trimmedName) {
      setErr(t('err_name_required'));
      setBusy(false);
      return;
    }
    try {
      const r = await api.createRegToken(trimmedName, note.trim());
      setInfo(r);
      setStep('done');
      onCreated();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const doCopy = async () => {
    if (!info) return;
    const ok = await copyText(info.install_command);
    setCopied(ok);
    if (ok) window.setTimeout(() => setCopied(false), 2000);
  };

  return (
    <Modal title={t('add_node_title')} onClose={() => setStep('closed')}>
      {step === 'form' && (
        <form onSubmit={create} className="stack">
          <label className="field">
            <span>{t('add_node_name_label')}</span>
            <input
              value={name}
              autoFocus
              required
              maxLength={120}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="field">
            <span>{t('add_node_note_label')}</span>
            <input value={note} onChange={(e) => setNote(e.target.value)} />
          </label>
          {err && <div className="form-error">{err}</div>}
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setStep('closed')}>
              {t('cancel')}
            </button>
            <button type="submit" className="btn primary" disabled={busy}>
              {busy ? t('loading') : t('add_node_create')}
            </button>
          </div>
        </form>
      )}
      {step === 'done' && info && (
        <div className="stack">
          <p className="hint">{t('install_cmd_warn', { min: Math.round(info.ttl / 60) })}</p>
          <pre className="code-block">{info.install_command}</pre>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => void doCopy()}>
              {copied ? t('copied') : t('copy')}
            </button>
            <button
              type="button"
              className="btn"
              onClick={() => {
                setInfo(null);
                setName('');
                setNote('');
                setStep('form');
              }}
            >
              {t('add_another')}
            </button>
            <button type="button" className="btn primary" onClick={() => setStep('closed')}>
              {t('close')}
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
