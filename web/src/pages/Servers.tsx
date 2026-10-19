import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type KeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from 'react';
import { Link } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n, type TFn } from '../i18n';
import type { NodeView, RegTokenInfo } from '../types';
import Modal from '../components/Modal';
import Flag from '../components/Flag';
import { CopyIcon, GripIcon, PencilIcon, TrashIcon } from '../components/Icons';
import { useToast } from '../components/Toast';
import { fmtDueDays, copyText } from '../format';
import { ErrorState } from '../components/ErrorState';

type AddStep = 'closed' | 'form' | 'done';

/** Pointer travel that turns a press on a grip into a drag (px). Below it the
 *  gesture is a click on the handle and must not dim a row for one frame. */
const DRAG_THRESHOLD = 4;

/** Where a dragged row would land: before or after `id`. */
type DropTarget = { id: string; before: boolean };

/** A grip gesture in flight. Lives in a ref because pointerup has to read the
 *  latest target — state would still be the previous render's value there. */
type DragSession = {
  id: string;
  pointerId: number;
  startY: number;
  active: boolean;
  target: DropTarget | null;
};

/**
 * Expiry column (§15): whole days only — "18天", not "18d 3h". Red under a week;
 * the threshold reads the same rounded number the cell prints, so a red "7天" (or
 * a calm "6天") cannot happen. Overdue is red too.
 */
function dueCell(node: NodeView, t: TFn): ReactNode {
  if (!node.billing_configured || node.next_due_at == null) return null;
  const due = fmtDueDays(node.next_due_at);
  if (!due) return null;
  const soon = due.overdue || due.days < 7;
  return (
    <span className={'due-days' + (soon ? ' due-soon' : '')}>
      {due.overdue ? t('due_overdue_for', { d: due.days }) : t('due_in', { d: due.days })}
    </span>
  );
}

/**
 * Settings → 服务器: the wall of added servers as a table. Monitoring stays on
 * the overview / node pages; this page is the configuration entry point — the
 * last column opens the per-server edit page and the delete confirmation. The
 * "add node" dialog lives here too (moved off the overview page): joining a
 * probe is an onboarding/config action, not a monitoring one.
 *
 * This table is also where the wall order is set (§16, 2026-10-02): rows carry a
 * grip that reorders them by drag, and the order is persisted as nodes.sort_order
 * — the overview card wall renders the same order because both read ListNodes.
 * The drag is pointer-based rather than HTML5 `draggable`: `draggable` never
 * fires on touch, and reordering a server list on a phone is the same gesture as
 * on a desktop.
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
  /** Rendered drag state: which row is being dragged and where it would land. */
  const [drag, setDrag] = useState<{ id: string; target: DropTarget | null } | null>(null);
  const dragRef = useRef<DragSession | null>(null);
  const tableRef = useRef<HTMLTableElement | null>(null);

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

  /**
   * Stores a new order. The local list moves first — the marker already showed
   * the operator exactly this — and a failed save re-reads the server's order
   * instead of leaving the panel displaying one that nobody stored.
   */
  const commitOrder = useCallback(
    async (next: NodeView[]) => {
      setNodes(next);
      try {
        await api.reorderNodes(next.map((n) => n.id));
        setErr(null);
      } catch (e) {
        setErr(apiErrorMessage(e, t));
        await load();
      }
    },
    [load, t],
  );

  /**
   * Row under a viewport y plus the half it fell in. Rows are measured live, and
   * the marker is drawn with box-shadow (see styles.css) so the layout never
   * moves under the pointer — a border-based marker would shift every row 2px
   * and make the target flicker between two rows. Above/below the table clamps
   * to the first/last row: dragging past the edge means "to the end".
   */
  const rowAt = (y: number): DropTarget | null => {
    const rows = Array.from(tableRef.current?.querySelectorAll<HTMLTableRowElement>('tr[data-node-id]') ?? []);
    for (const tr of rows) {
      const id = tr.dataset.nodeId;
      if (!id) continue;
      const r = tr.getBoundingClientRect();
      if (y < r.top) return { id, before: true };
      if (y <= r.bottom) return { id, before: y < r.top + r.height / 2 };
    }
    const last = rows[rows.length - 1]?.dataset.nodeId;
    return last ? { id: last, before: false } : null;
  };

  const applyDrop = (id: string, target: DropTarget) => {
    if (!nodes) return;
    const moved = nodes.find((n) => n.id === id);
    const rest = nodes.filter((n) => n.id !== id);
    let at = rest.findIndex((n) => n.id === target.id);
    if (!moved || at < 0 || target.id === id) return;
    if (!target.before) at += 1;
    void commitOrder([...rest.slice(0, at), moved, ...rest.slice(at)]);
  };

  const onGripDown = (e: ReactPointerEvent<HTMLButtonElement>, id: string) => {
    if (busy || !nodes) return;
    // Capture keeps move/up on this handle even when the pointer leaves the row
    // (and the table); `touch-action: none` on the handle is what stops the page
    // from scrolling underneath the drag on a phone.
    e.currentTarget.setPointerCapture(e.pointerId);
    dragRef.current = { id, pointerId: e.pointerId, startY: e.clientY, active: false, target: null };
  };

  const onGripMove = (e: ReactPointerEvent<HTMLButtonElement>) => {
    const d = dragRef.current;
    if (!d || d.pointerId !== e.pointerId) return;
    if (!d.active) {
      if (Math.abs(e.clientY - d.startY) < DRAG_THRESHOLD) return;
      d.active = true;
    }
    const found = rowAt(e.clientY);
    // Hovering the dragged row itself is not a target: dropping there must be a
    // no-op, not a jump.
    const target = found && found.id !== d.id ? found : null;
    // Only re-render when the landing spot actually changed — a pointermove
    // inside one row half must not re-render the whole table 60 times a second.
    if (target?.id === d.target?.id && target?.before === d.target?.before) return;
    d.target = target;
    setDrag({ id: d.id, target });
  };

  const onGripUp = (e: ReactPointerEvent<HTMLButtonElement>) => {
    const d = dragRef.current;
    // A second finger on another handle owns the session now; its own pointerup
    // decides, this one must not commit someone else's drop.
    if (d && d.pointerId !== e.pointerId) return;
    dragRef.current = null;
    setDrag(null);
    if (d?.active && d.target) applyDrop(d.id, d.target);
  };

  const onGripCancel = () => {
    dragRef.current = null;
    setDrag(null);
  };

  /** Keyboard path to the same order: no pointer needed, and the only way to
   *  reorder at all with a screen reader. */
  const onGripKey = (e: KeyboardEvent<HTMLButtonElement>, id: string) => {
    if ((e.key !== 'ArrowUp' && e.key !== 'ArrowDown') || !nodes) return;
    const i = nodes.findIndex((n) => n.id === id);
    const j = i + (e.key === 'ArrowUp' ? -1 : 1);
    if (i < 0 || j < 0 || j >= nodes.length) return;
    e.preventDefault();
    const next = [...nodes];
    next[i] = nodes[j];
    next[j] = nodes[i];
    void commitOrder(next);
  };

  const rowClass = (id: string): string => {
    if (!drag) return '';
    const parts: string[] = [];
    if (drag.id === id) parts.push('dragging');
    if (drag.target?.id === id) parts.push(drag.target.before ? 'drop-before' : 'drop-after');
    return parts.join(' ');
  };

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
        <ErrorState message={err} onRetry={() => () => void load()} />
      )}

      {nodes !== null && nodes.length === 0 && <div className="empty-hint">{t('servers_empty')}</div>}

      {nodes !== null && nodes.length > 0 && (
        /* The scroll wrapper carries the card: overflow-x on the <table> itself
           never applied (overflow doesn't fire on table boxes), so wide rows
           used to widen the whole page on a phone instead of scrolling here. */
        <div className="card table-card">
          <table className="table" ref={tableRef}>
            <thead>
              <tr>
                {/* Decorative: the accessible name lives on each row's grip. */}
                <th className="drag-col" />
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
                <tr key={n.id} data-node-id={n.id} className={rowClass(n.id)}>
                  <td className="drag-col">
                    <button
                      type="button"
                      className="drag-handle"
                      title={t('drag_to_reorder')}
                      aria-label={t('drag_to_reorder')}
                      onPointerDown={(e) => onGripDown(e, n.id)}
                      onPointerMove={onGripMove}
                      onPointerUp={onGripUp}
                      onPointerCancel={onGripCancel}
                      onKeyDown={(e) => onGripKey(e, n.id)}
                    >
                      <GripIcon />
                    </button>
                  </td>
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
                  <td>{dueCell(n, t)}</td>
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
