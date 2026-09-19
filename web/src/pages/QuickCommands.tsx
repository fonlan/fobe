import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import type { QuickCommand } from '../types';
import Modal from '../components/Modal';
import { PencilIcon, TrashIcon } from '../components/Icons';

/**
 * Settings → 常用命令 (2026-09-19): snippets the terminal page's side panel
 * injects into the PTY as keyboard input. Single-line commands auto-execute on
 * click; multi-line ones paste via bracketed paste and wait for Enter. The
 * page is the list itself (same layout as the servers page: add button in the
 * page head, icon buttons per row); both create and edit live in one modal.
 * The validation that matters (name length, 16 KiB total, 4000 bytes per line —
 * the tty line discipline's MAX_CANON) lives on the server; the form keeps
 * only the cheap non-empty checks.
 */
export default function QuickCommands() {
  const { t } = useI18n();
  const [commands, setCommands] = useState<QuickCommand[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<QuickCommand | null>(null);
  const [deleting, setDeleting] = useState<QuickCommand | null>(null);

  const load = useCallback(async () => {
    try {
      const list = await api.listQuickCommands();
      setCommands(list);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  // Up/down moves one row and persists the whole order in one call. Optimistic:
  // the list is tiny, and a failed reorder just re-syncs from the server below.
  const move = async (id: number, dir: -1 | 1) => {
    if (!commands) return;
    const idx = commands.findIndex((c) => c.id === id);
    const next = idx + dir;
    if (idx < 0 || next < 0 || next >= commands.length) return;
    const reordered = [...commands];
    [reordered[idx], reordered[next]] = [reordered[next], reordered[idx]];
    setCommands(reordered);
    try {
      await api.reorderQuickCommands(reordered.map((c) => c.id));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      await load();
    }
  };

  const doDelete = async (c: QuickCommand) => {
    try {
      await api.deleteQuickCommand(c.id);
      setDeleting(null);
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      setDeleting(null);
    }
  };

  return (
    <div className="stack-lg">
      <div className="page-head">
        <h2>{t('qc_title')}</h2>
        <button type="button" className="btn primary" onClick={() => setCreating(true)}>
          + {t('qc_create')}
        </button>
      </div>
      <p className="hint">{t('qc_desc')}</p>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {commands !== null && commands.length === 0 && <div className="empty-hint">{t('qc_empty')}</div>}

      {commands !== null && commands.length > 0 && (
        <div className="card table-card">
          <table className="table">
            <thead>
              <tr>
                <th>{t('name')}</th>
                <th>{t('qc_command')}</th>
                <th />
                <th />
              </tr>
            </thead>
            <tbody>
              {commands.map((c, i) => (
                <tr key={c.id}>
                  <td>{c.name}</td>
                  <td className="mono qc-cell">
                    {firstLine(c.command)}
                    {c.command.includes('\n') ? ' …' : ''}
                  </td>
                  <td className="nowrap">
                    <button
                      type="button"
                      className="btn small"
                      title={t('qc_move_up')}
                      aria-label={t('qc_move_up')}
                      disabled={i === 0}
                      onClick={() => void move(c.id, -1)}
                    >
                      ↑
                    </button>{' '}
                    <button
                      type="button"
                      className="btn small"
                      title={t('qc_move_down')}
                      aria-label={t('qc_move_down')}
                      disabled={i === commands.length - 1}
                      onClick={() => void move(c.id, 1)}
                    >
                      ↓
                    </button>
                  </td>
                  <td className="nowrap">
                    <button
                      type="button"
                      className="icon-btn"
                      title={t('edit')}
                      aria-label={t('edit')}
                      onClick={() => setEditing(c)}
                    >
                      <PencilIcon />
                    </button>
                    <button
                      type="button"
                      className="icon-btn danger"
                      title={t('delete')}
                      aria-label={t('delete')}
                      onClick={() => setDeleting(c)}
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

      {(creating || editing) && (
        <CommandModal
          command={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          onSaved={() => {
            setCreating(false);
            setEditing(null);
            void load();
          }}
        />
      )}

      {deleting && (
        <Modal title={t('delete')} onClose={() => setDeleting(null)}>
          <p>{t('qc_delete_confirm', { name: deleting.name })}</p>
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
 * One modal for create (command=null) and edit — the fields are identical, only
 * the submit call and the title differ.
 */
function CommandModal({
  command,
  onClose,
  onSaved,
}: {
  command: QuickCommand | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [name, setName] = useState(command?.name ?? '');
  const [commandText, setCommandText] = useState(command?.command ?? '');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !commandText.trim()) {
      setErr(t('qc_invalid_form'));
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      if (command) await api.updateQuickCommand(command.id, name.trim(), commandText);
      else await api.createQuickCommand(name.trim(), commandText);
      onSaved();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={command ? t('qc_edit') : t('qc_new')} onClose={onClose}>
      <form onSubmit={save}>
        <div className="form-grid">
          <label className="field">
            <span>{t('name')}</span>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t('qc_name_placeholder')}
            />
          </label>
        </div>
        <label className="field">
          <span>{t('qc_command')}</span>
          <textarea
            value={commandText}
            rows={4}
            className="mono"
            spellCheck={false}
            onChange={(e) => setCommandText(e.target.value)}
            placeholder={t('qc_command_placeholder')}
          />
        </label>
        <p className="hint">{t('qc_command_hint')}</p>
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

function firstLine(command: string): string {
  const idx = command.indexOf('\n');
  return idx >= 0 ? command.slice(0, idx) : command;
}
