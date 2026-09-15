import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { fmtTime } from '../format';
import type { NodeView, SubAccessRow, SubscriptionRow, SubscriptionToken, TemplateRow } from '../types';
import Modal from '../components/Modal';

/** Placeholder pair every template must carry (server validates too). */
const NODES_PH = '{{nodes}}';
const RULES_PH = '{{rules}}';

export default function Subscriptions() {
  const { t } = useI18n();
  return (
    <div>
      <div className="page-head">
        <h2>{t('subs_title')}</h2>
      </div>
      <SubscriptionsCard />
      <TemplatesCard />
    </div>
  );
}

// --- subscriptions -----------------------------------------------------------

function SubscriptionsCard() {
  const { t } = useI18n();
  const [subs, setSubs] = useState<SubscriptionRow[] | null>(null);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  /** id → plaintext token reveal (create/rotate), shown exactly once. */
  const [freshToken, setFreshToken] = useState<SubscriptionToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null); // node picker
  const [uaFor, setUaFor] = useState<string | null>(null); // §10 UA filter editor
  const [accessFor, setAccessFor] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<SubscriptionRow | null>(null);
  const [rotating, setRotating] = useState<SubscriptionRow | null>(null);

  const load = useCallback(async () => {
    try {
      const [s, n] = await Promise.all([api.listSubscriptions(), api.listNodes().catch(() => ({ nodes: [] }))]);
      setSubs(s.subscriptions ?? []);
      setNodes(n.nodes ?? []);
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
    if (busy || !name.trim()) return;
    setBusy(true);
    setErr(null);
    try {
      const tok = await api.createSubscription(name.trim());
      setFreshToken(tok);
      setCopied(false);
      setName('');
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const rotate = async (sub: SubscriptionRow) => {
    setRotating(null);
    try {
      const tok = await api.rotateSubscription(sub.id);
      setFreshToken(tok);
      setCopied(false);
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    }
  };

  const toggle = async (sub: SubscriptionRow) => {
    try {
      await api.updateSubscription(sub.id, { enabled: !sub.enabled });
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    }
  };

  const saveNodes = async (sub: SubscriptionRow, nodeIds: string[]) => {
    try {
      await api.setSubscriptionNodes(sub.id, nodeIds);
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    }
  };

  // §10: the UA allow-list is saved standalone; empty string = unrestricted.
  const saveUAFilter = async (sub: SubscriptionRow, filter: string) => {
    try {
      await api.updateSubscription(sub.id, { ua_filter: filter.trim() });
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    }
  };

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await api.deleteSubscription(deleting.id);
      setDeleting(null);
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
      setDeleting(null);
    }
  };

  const copyToken = async () => {
    if (!freshToken) return;
    try {
      await navigator.clipboard.writeText(freshToken.url || freshToken.token);
      setCopied(true);
    } catch {
      // clipboard unavailable (http): the token stays selectable on screen
    }
  };

  return (
    <section className="card">
      <h3>{t('subs_section')}</h3>
      <p className="hint">{t('subs_desc')}</p>

      <form className="cmd-input-row" onSubmit={create}>
        <input
          value={name}
          placeholder={t('sub_name')}
          onChange={(e) => setName(e.target.value)}
        />
        <button type="submit" className="btn primary" disabled={busy || name.trim() === ''}>
          {t('sub_create')}
        </button>
      </form>

      {err && <div className="form-error">{err}</div>}

      {freshToken && (
        <div className="card warn-card" style={{ marginTop: 12 }}>
          <p>{t('sub_token_once')}</p>
          <p className="mono selectable">{freshToken.url || freshToken.token}</p>
          <div className="row-end">
            <button type="button" className="btn small" onClick={() => void copyToken()}>
              {copied ? t('copied') : t('copy')}
            </button>
            <button type="button" className="btn small" onClick={() => setFreshToken(null)}>
              {t('close')}
            </button>
          </div>
        </div>
      )}

      {subs !== null && subs.length === 0 && <div className="hint" style={{ marginTop: 12 }}>{t('subs_empty')}</div>}

      {subs !== null && subs.length > 0 && (
        <table className="table" style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('sub_status_col')}</th>
              <th>{t('sub_nodes_col')}</th>
              <th>{t('session_created')}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {subs.map((sub) => (
              <tr key={sub.id} className={sub.enabled ? '' : 'row-muted'}>
                <td>{sub.name}</td>
                <td>
                  <span className={'chip ' + (sub.enabled ? 'status-ok' : 'status-failed')}>
                    {sub.enabled ? t('sub_enabled') : t('sub_disabled')}
                  </span>
                </td>
                <td className="mono">{sub.node_ids.length}</td>
                <td className="mono nowrap">{fmtTime(sub.created_at)}</td>
                <td className="nowrap">
                  <button
                    type="button"
                    className="btn small"
                    onClick={() => setExpanded(expanded === sub.id ? null : sub.id)}
                  >
                    {t('sub_nodes')}
                  </button>{' '}
                  <button
                    type="button"
                    className="btn small"
                    onClick={() => setUaFor(uaFor === sub.id ? null : sub.id)}
                    title={sub.ua_filter || undefined}
                  >
                    {t('sub_ua_filter')}
                  </button>{' '}
                  <button
                    type="button"
                    className="btn small"
                    onClick={() => setAccessFor(accessFor === sub.id ? null : sub.id)}
                  >
                    {t('sub_access')}
                  </button>{' '}
                  <button type="button" className="btn small" onClick={() => void toggle(sub)}>
                    {sub.enabled ? t('sub_disable') : t('sub_enable')}
                  </button>{' '}
                  <button type="button" className="btn small" onClick={() => setRotating(sub)}>
                    {t('sub_rotate')}
                  </button>{' '}
                  <button type="button" className="btn danger small" onClick={() => setDeleting(sub)}>
                    {t('delete')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {subs?.map((sub) =>
        expanded === sub.id ? (
          <NodePicker key={'n' + sub.id} sub={sub} nodes={nodes} onSave={(ids) => void saveNodes(sub, ids)} />
        ) : null,
      )}
      {subs?.map((sub) =>
        uaFor === sub.id ? <UAFilterEditor key={'u' + sub.id} sub={sub} onSave={(f) => void saveUAFilter(sub, f)} /> : null,
      )}
      {subs?.map((sub) => (accessFor === sub.id ? <AccessLog key={'a' + sub.id} subId={sub.id} /> : null))}

      {rotating && (
        <Modal title={t('sub_rotate')} onClose={() => setRotating(null)}>
          <p>{t('sub_rotate_confirm')}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setRotating(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" onClick={() => void rotate(rotating)}>
              {t('confirm')}
            </button>
          </div>
        </Modal>
      )}
      {deleting && (
        <Modal title={t('delete')} onClose={() => setDeleting(null)}>
          <p>{t('sub_delete_confirm', { name: deleting.name })}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setDeleting(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" onClick={() => void doDelete()}>
              {t('delete')}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}

function NodePicker({
  sub,
  nodes,
  onSave,
}: {
  sub: SubscriptionRow;
  nodes: NodeView[];
  onSave: (ids: string[]) => void;
}) {
  const { t } = useI18n();
  const [ids, setIds] = useState<string[]>(sub.node_ids);

  const toggleNode = (id: string) => {
    setIds((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));
  };

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>{t('sub_nodes_of', { name: sub.name })}</h4>
      <p className="hint">{t('sub_nodes_hint')}</p>
      {nodes.length === 0 ? (
        <div className="hint">{t('no_nodes')}</div>
      ) : (
        <div className="row-wrap">
          {nodes.map((n) => (
            <label key={n.id} className="check-chip">
              <input type="checkbox" checked={ids.includes(n.id)} onChange={() => toggleNode(n.id)} />
              {n.name || n.id}
              {n.online ? '' : ` (${t('offline')})`}
            </label>
          ))}
        </div>
      )}
      <div className="row-end">
        <button type="button" className="btn primary small" onClick={() => onSave(ids)}>
          {t('save')}
        </button>
      </div>
    </div>
  );
}

// §10: small inline editor for the subscription UA allow-list. Client UAs
// must contain one of the comma-separated substrings, otherwise /sub 404s.
function UAFilterEditor({ sub, onSave }: { sub: SubscriptionRow; onSave: (filter: string) => void }) {
  const { t } = useI18n();
  const [filter, setFilter] = useState(sub.ua_filter);

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>
        {t('sub_ua_filter')} — {sub.name}
      </h4>
      <p className="hint">{t('sub_ua_hint')}</p>
      <div className="cmd-input-row">
        <input
          className="mono"
          value={filter}
          placeholder="clash,sing-box"
          onChange={(e) => setFilter(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') onSave(filter.trim());
          }}
        />
        <button type="button" className="btn primary small" onClick={() => onSave(filter.trim())}>
          {t('save')}
        </button>
      </div>
    </div>
  );
}

function AccessLog({ subId }: { subId: string }) {
  const { t } = useI18n();
  const [logs, setLogs] = useState<SubAccessRow[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    api
      .subscriptionAccess(subId)
      .then((r) => alive && setLogs(r.logs ?? []))
      .catch((e) => alive && setErr(apiErrorMessage(e, t)));
    return () => {
      alive = false;
    };
  }, [subId, t]);

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>{t('sub_access')}</h4>
      {err && <div className="form-error">{err}</div>}
      {logs !== null && logs.length === 0 && <div className="hint">{t('sub_access_empty')}</div>}
      {logs !== null && logs.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>{t('audit_time')}</th>
              <th>{t('session_ip')}</th>
              <th>{t('session_ua')}</th>
            </tr>
          </thead>
          <tbody>
            {logs.map((l, i) => (
              <tr key={i}>
                <td className="mono nowrap">{fmtTime(l.ts)}</td>
                <td className="mono">{l.ip}</td>
                <td className="ua-cell" title={l.ua}>
                  {l.ua || '-'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// --- templates ---------------------------------------------------------------

function TemplatesCard() {
  const { t } = useI18n();
  const [templates, setTemplates] = useState<TemplateRow[] | null>(null);
  const [editing, setEditing] = useState<TemplateRow | null>(null); // null = closed
  const [name, setName] = useState('');
  const [format, setFormat] = useState<'singbox' | 'clash'>('singbox');
  const [content, setContent] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<TemplateRow | null>(null);

  const load = useCallback(async () => {
    try {
      const r = await api.listTemplates();
      setTemplates(r.templates ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const openNew = () => {
    setEditing({ id: '', name: '', format: 'singbox', content: '', created_at: 0, updated_at: 0 });
    setName('');
    setFormat('singbox');
    setContent('');
    setErr(null);
  };

  const openEdit = (tpl: TemplateRow) => {
    setEditing(tpl);
    setName(tpl.name);
    setFormat(tpl.format);
    setContent(tpl.content);
    setErr(null);
  };

  const save = async () => {
    if (busy || !editing) return;
    setBusy(true);
    setErr(null);
    try {
      if (editing.id === '') {
        await api.createTemplate({ name: name.trim(), format, content });
      } else {
        await api.updateTemplate(editing.id, { name: name.trim(), format, content });
      }
      setEditing(null);
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await api.deleteTemplate(deleting.id);
      setDeleting(null);
      await load();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
      setDeleting(null);
    }
  };

  const placeholdersMissing =
    !content.includes(NODES_PH) || !content.includes(RULES_PH);

  return (
    <section className="card" style={{ marginTop: 16 }}>
      <div className="row-between">
        <h3>{t('tpl_section')}</h3>
        <button type="button" className="btn small" onClick={openNew}>
          {t('tpl_new')}
        </button>
      </div>
      <p className="hint">{t('tpl_placeholders_hint')}</p>

      {err && <div className="form-error">{err}</div>}

      {templates !== null && templates.length === 0 && <div className="hint">{t('tpl_empty')}</div>}
      {templates !== null && templates.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>{t('name')}</th>
              <th>{t('tpl_format')}</th>
              <th>{t('audit_time')}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {templates.map((tpl) => (
              <tr key={tpl.id}>
                <td>{tpl.name}</td>
                <td>
                  <span className="chip">{tpl.format}</span>
                </td>
                <td className="mono nowrap">{fmtTime(tpl.updated_at || tpl.created_at)}</td>
                <td className="nowrap">
                  <button type="button" className="btn small" onClick={() => openEdit(tpl)}>
                    {t('tpl_edit')}
                  </button>{' '}
                  <button type="button" className="btn danger small" onClick={() => setDeleting(tpl)}>
                    {t('delete')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {editing && (
        <div className="card" style={{ marginTop: 12 }}>
          <h4>{editing.id === '' ? t('tpl_new') : t('tpl_edit')}</h4>
          <div className="form-grid">
            <label className="field">
              <span>{t('tpl_name')}</span>
              <input value={name} onChange={(e) => setName(e.target.value)} />
            </label>
            <label className="field">
              <span>{t('tpl_format')}</span>
              <select value={format} onChange={(e) => setFormat(e.target.value === 'clash' ? 'clash' : 'singbox')}>
                <option value="singbox">sing-box (JSON)</option>
                <option value="clash">Clash / mihomo (YAML)</option>
              </select>
            </label>
          </div>
          <label className="field">
            <span>{t('tpl_content')}</span>
            <textarea
              className="mono"
              rows={14}
              spellCheck={false}
              value={content}
              onChange={(e) => setContent(e.target.value)}
              style={{ width: '100%', fontFamily: 'monospace' }}
            />
          </label>
          <p className={'hint' + (placeholdersMissing ? ' form-error' : '')}>
            {placeholdersMissing ? t('tpl_placeholders_missing') : t('tpl_placeholders_ok')}
          </p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setEditing(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn primary" disabled={busy || name.trim() === ''} onClick={() => void save()}>
              {busy ? t('loading') : t('save')}
            </button>
          </div>
        </div>
      )}

      {deleting && (
        <Modal title={t('delete')} onClose={() => setDeleting(null)}>
          <p>{t('tpl_delete_confirm', { name: deleting.name })}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setDeleting(null)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" onClick={() => void doDelete()}>
              {t('delete')}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}
