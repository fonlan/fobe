import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { copyText, fmtTime } from '../format';
import type { NodeView, SubAccessRow, SubscriptionRow, SubscriptionToken, TemplateRow } from '../types';
import Modal from '../components/Modal';
import { useToast } from '../components/Toast';

/** Placeholder pair every template must carry (server validates too). */
const NODES_PH = '{{nodes}}';
const RULES_PH = '{{rules}}';

/** Settings keys behind {@link RulesCard} (mirrors httpapi.SettingRules*). */
const RULES_SINGBOX_KEY = 'sub.rules_singbox';
const RULES_CLASH_KEY = 'sub.rules_clash';

/**
 * Mirrors validateRulesFragment on the server: catching a broken snippet here
 * just saves a round trip — the server still refuses it, because a fragment
 * that cannot be spliced in would hand every client an unloadable config.
 */
function rulesSingboxValid(value: string): boolean {
  if (value.trim() === '') return true;
  try {
    const parsed: unknown = JSON.parse('[' + value + ']');
    return (
      Array.isArray(parsed) &&
      parsed.every((r) => r !== null && typeof r === 'object' && !Array.isArray(r))
    );
  } catch {
    return false;
  }
}

function rulesClashValid(value: string): boolean {
  return value.split('\n').every((line) => {
    const l = line.trim();
    return l === '' || l.startsWith('#') || l.startsWith('-');
  });
}

export default function Subscriptions() {
  const { t } = useI18n();
  // Page-level feedback for the copy button: the toast is fixed-positioned, so
  // it belongs to the page and not to a card that may scroll out of view.
  const { show: showToast, node: toastNode } = useToast();
  // Templates are owned here: the subscription rows bind them (template picker)
  // and the templates card edits them, so one fetch feeds both.
  const [templates, setTemplates] = useState<TemplateRow[]>([]);
  const [tplErr, setTplErr] = useState<string | null>(null);

  const loadTemplates = useCallback(async () => {
    try {
      const r = await api.listTemplates();
      setTemplates(r.templates ?? []);
      setTplErr(null);
    } catch (e) {
      setTplErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void loadTemplates();
  }, [loadTemplates]);

  return (
    <div>
      <div className="page-head">
        <h2>{t('subs_title')}</h2>
      </div>
      <SubscriptionsCard templates={templates} onToast={showToast} />
      <TemplatesCard templates={templates} loadError={tplErr} onReload={loadTemplates} />
      <RulesCard />
      {toastNode}
    </div>
  );
}

// --- subscriptions -----------------------------------------------------------

function SubscriptionsCard({
  templates,
  onToast,
}: {
  templates: TemplateRow[];
  onToast: (message: string, tone?: 'ok' | 'error') => void;
}) {
  const { t } = useI18n();
  const [subs, setSubs] = useState<SubscriptionRow[] | null>(null);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  /** Plaintext token from a create/rotate response (the link stays available
   *  in the list afterwards, see LinkPanel). */
  const [freshToken, setFreshToken] = useState<SubscriptionToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null); // node picker
  const [uaFor, setUaFor] = useState<string | null>(null); // §10 UA filter editor
  const [tplFor, setTplFor] = useState<string | null>(null); // template binding
  const [linkFor, setLinkFor] = useState<string | null>(null); // §10 修订: URL reveal
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
      setCopyFailed(false);
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
      setCopyFailed(false);
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

  // §10: '' template clears the binding → the built-in default is used;
  // '' format means auto (see TemplatePicker).
  const saveTemplate = async (sub: SubscriptionRow, templateId: string, format: string) => {
    try {
      await api.updateSubscription(sub.id, {
        template_id: templateId === '' ? null : templateId,
        format,
      });
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

  // §10 修订: the panel is the only place the URL can be read again, so the row
  // offers one click instead of a two-step reveal. copyText falls back to
  // execCommand because the panel is usually served over plain http on a LAN
  // IP, where navigator.clipboard does not exist at all.
  const copyLink = async (sub: SubscriptionRow) => {
    if (!sub.link_available) {
      onToast(t('err_link_unavailable'), 'error');
      return;
    }
    try {
      const link = await api.subscriptionLink(sub.id);
      const url = link.url || '/sub/' + link.token;
      if (await copyText(url)) {
        onToast(t('sub_link_copied'));
      } else {
        // both clipboard paths refused: show the URL so it can be selected
        setLinkFor(sub.id);
        onToast(t('sub_link_copy_failed'), 'error');
      }
    } catch (ex) {
      setLinkFor(sub.id);
      onToast(apiErrorMessage(ex, t), 'error');
    }
  };

  const copyToken = async () => {
    if (!freshToken) return;
    if (await copyText(freshToken.url || freshToken.token)) {
      setCopied(true);
      setCopyFailed(false);
    } else {
      setCopyFailed(true);
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
            {copyFailed && <span className="hint">{t('copy_manual')}</span>}
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
        <div className="table-wrap" style={{ marginTop: 12 }}>
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
                      onClick={() => setTplFor(tplFor === sub.id ? null : sub.id)}
                      title={templates.find((tp) => tp.id === sub.template_id)?.name}
                    >
                      {t('sub_template')}
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
                      onClick={() => void copyLink(sub)}
                    >
                      {t('sub_link_copy')}
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
        </div>
      )}

      {subs?.map((sub) =>
        expanded === sub.id ? (
          <NodePicker key={'n' + sub.id} sub={sub} nodes={nodes} onSave={(ids) => void saveNodes(sub, ids)} />
        ) : null,
      )}
      {subs?.map((sub) =>
        tplFor === sub.id ? (
          <TemplatePicker
            key={'t' + sub.id}
            sub={sub}
            templates={templates}
            onSave={(id, format) => void saveTemplate(sub, id, format)}
          />
        ) : null,
      )}
      {subs?.map((sub) =>
        uaFor === sub.id ? <UAFilterEditor key={'u' + sub.id} sub={sub} onSave={(f) => void saveUAFilter(sub, f)} /> : null,
      )}
      {subs?.map((sub) => (linkFor === sub.id ? <LinkPanel key={'l' + sub.id} sub={sub} /> : null))}
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

// §10 node picker. Only nodes the server marks `singbox_ready` are offered —
// that flag is the renderer's own predicate (primary IP + inbound port +
// reported certificate), so checking a node always changes the output. Nodes
// that are bound but currently unrenderable stay listed (muted, still
// checkable) instead of silently disappearing: they are usually mid-reinstall,
// and dropping them from the binding behind the operator's back would be worse
// than showing a node that renders nothing right now.
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

  const ready = nodes.filter((n) => n.singbox_ready);
  const unavailableBound = nodes.filter((n) => !n.singbox_ready && sub.node_ids.includes(n.id));

  const chip = (n: NodeView, muted = false) => (
    <label key={n.id} className={'check-chip' + (muted ? ' chip-muted' : '')}>
      <input type="checkbox" checked={ids.includes(n.id)} onChange={() => toggleNode(n.id)} />
      {n.name || n.id}
      {n.online ? '' : ` (${t('offline')})`}
    </label>
  );

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>{t('sub_nodes_of', { name: sub.name })}</h4>
      <p className="hint">{t('sub_nodes_hint')}</p>
      {ready.length === 0 ? (
        <div className="hint">{nodes.length === 0 ? t('no_nodes') : t('sub_nodes_none_ready')}</div>
      ) : (
        <div className="row-wrap">{ready.map((n) => chip(n))}</div>
      )}
      {unavailableBound.length > 0 && (
        <>
          <p className="hint" style={{ marginTop: 10 }}>{t('sub_nodes_unavailable')}</p>
          <div className="row-wrap">{unavailableBound.map((n) => chip(n, true))}</div>
        </>
      )}
      <div className="row-end">
        <button type="button" className="btn primary small" onClick={() => onSave(ids)}>
          {t('save')}
        </button>
      </div>
    </div>
  );
}

// §10: which template renders this subscription, and which format it comes out
// as. Auto means "the bound template's format", which is why binding a Clash
// template stops a client with an unrecognised User-Agent from silently
// receiving the sing-box default; pinning a format overrides the template for
// what the client asked for (an explicit ?format= still wins over both).
function TemplatePicker({
  sub,
  templates,
  onSave,
}: {
  sub: SubscriptionRow;
  templates: TemplateRow[];
  onSave: (templateId: string, format: string) => void;
}) {
  const { t } = useI18n();
  const [sel, setSel] = useState(sub.template_id ?? '');
  const [format, setFormat] = useState(sub.format);

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>
        {t('sub_template')} — {sub.name}
      </h4>
      <p className="hint">{t('sub_template_hint')}</p>
      <div className="cmd-input-row">
        <select value={sel} onChange={(e) => setSel(e.target.value)}>
          <option value="">{t('sub_template_default')}</option>
          {templates.map((tp) => (
            <option key={tp.id} value={tp.id}>
              {tp.name} ({tp.format})
            </option>
          ))}
        </select>
      </div>
      <label className="field" style={{ marginTop: 12, marginBottom: 0 }}>
        <span>{t('sub_format')}</span>
        <select value={format} onChange={(e) => setFormat(e.target.value)}>
          <option value="">{t('sub_format_auto')}</option>
          <option value="singbox">{t('sub_format_singbox')}</option>
          <option value="clash">{t('sub_format_clash')}</option>
        </select>
      </label>
      <p className="hint">{t('sub_format_hint')}</p>
      <div className="row-end">
        <button type="button" className="btn primary small" onClick={() => onSave(sel, format)}>
          {t('save')}
        </button>
      </div>
    </div>
  );
}

// §10 实现修订 2026-09-16: the subscription URL is copyable whenever you want
// it, not only in the create/rotate response. The row button normally copies it
// straight to the clipboard; this panel is the fallback the button opens when
// the clipboard refused (or the reveal failed), because a URL the operator can
// still select by hand beats a toast that only says "copy failed".
function LinkPanel({ sub }: { sub: SubscriptionRow }) {
  const { t } = useI18n();
  const [link, setLink] = useState<SubscriptionToken | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    // legacy row: nothing to fetch, the hint below already explains why
    if (!sub.link_available) return;
    api
      .subscriptionLink(sub.id)
      .then((r) => alive && setLink(r))
      .catch((e) => alive && setErr(apiErrorMessage(e, t)));
    return () => {
      alive = false;
    };
  }, [sub.id, sub.link_available, t]);

  const url = link ? link.url || '/sub/' + link.token : '';

  const copy = async () => {
    if (await copyText(url)) {
      setCopied(true);
      setCopyFailed(false);
    } else {
      // both clipboard paths refused: the field below stays selectable
      setCopyFailed(true);
    }
  };

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>
        {t('sub_link')} — {sub.name}
      </h4>
      <p className="hint">{t('sub_link_hint')}</p>
      {!sub.link_available && <p className="hint">{t('sub_link_unavailable')}</p>}
      {err && <div className="form-error">{err}</div>}
      {link && (
        <div className="cmd-input-row">
          <input
            className="mono"
            readOnly
            value={url}
            onFocus={(e) => e.currentTarget.select()}
            aria-label={t('sub_link')}
          />
          {copyFailed && <span className="hint nowrap">{t('copy_manual')}</span>}
          <button type="button" className="btn small" onClick={() => void copy()}>
            {copied ? t('copied') : t('copy')}
          </button>
        </div>
      )}
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
        <div className="table-wrap">
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
        </div>
      )}
    </div>
  );
}

// --- templates ---------------------------------------------------------------

function TemplatesCard({
  templates,
  loadError,
  onReload,
}: {
  templates: TemplateRow[];
  loadError: string | null;
  onReload: () => Promise<void>;
}) {
  const { t } = useI18n();
  const [editing, setEditing] = useState<TemplateRow | null>(null); // null = closed
  const [name, setName] = useState('');
  const [format, setFormat] = useState<'singbox' | 'clash'>('singbox');
  const [content, setContent] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<TemplateRow | null>(null);

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
      await onReload();
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
      await onReload();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
      setDeleting(null);
    }
  };

  const placeholdersMissing = !content.includes(NODES_PH) || !content.includes(RULES_PH);

  return (
    <section className="card" style={{ marginTop: 16 }}>
      <div className="row-between">
        <h3>{t('tpl_section')}</h3>
        <button type="button" className="btn small" onClick={openNew}>
          {t('tpl_new')}
        </button>
      </div>
      <p className="hint">{t('tpl_placeholders_hint')}</p>

      {(err || loadError) && <div className="form-error">{err || loadError}</div>}

      {templates.length === 0 && <div className="hint">{t('tpl_empty')}</div>}
      {templates.length > 0 && (
        <div className="table-wrap">
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
        </div>
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
              className="code-area"
              rows={14}
              spellCheck={false}
              value={content}
              onChange={(e) => setContent(e.target.value)}
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

// --- routing rules (§10 实现修订 2026-09-16) ---------------------------------

/**
 * The panel-side home of {{rules}}. Two snippets (one per output format,
 * because a subscription's format is chosen per request) that the renderer
 * splices into the template verbatim — so, exactly like {{nodes}}, the snippet
 * carries its own list markers and indentation and the template owns the
 * surrounding key.
 */
function RulesCard() {
  const { t } = useI18n();
  const [singbox, setSingbox] = useState('');
  const [clash, setClash] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const load = useCallback(async () => {
    try {
      const r = await api.getSettings();
      const map: Record<string, string> = {};
      for (const s of r.settings ?? []) map[s.key] = s.value;
      setSingbox(map[RULES_SINGBOX_KEY] ?? '');
      setClash(map[RULES_CLASH_KEY] ?? '');
      setLoaded(true);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const save = async () => {
    if (busy) return;
    // Client-side pre-check only: the server validates again and owns the verdict.
    if (!rulesSingboxValid(singbox)) {
      setErr(t('rules_bad_singbox'));
      return;
    }
    if (!rulesClashValid(clash)) {
      setErr(t('rules_bad_clash'));
      return;
    }
    setBusy(true);
    setErr(null);
    setSaved(false);
    try {
      await api.putSettings({ [RULES_SINGBOX_KEY]: singbox, [RULES_CLASH_KEY]: clash });
      setSaved(true);
      window.setTimeout(() => setSaved(false), 3000);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card" style={{ marginTop: 16 }}>
      <h3>{t('rules_section')}</h3>
      <p className="hint">{t('rules_desc')}</p>

      {err && <div className="form-error">{err}</div>}
      {saved && <div className="form-ok">{t('settings_saved')}</div>}

      <label className="field">
        <span>{t('rules_singbox')}</span>
        <textarea
          className="code-area"
          rows={5}
          spellCheck={false}
          disabled={!loaded}
          placeholder={t('rules_singbox_ph')}
          value={singbox}
          onChange={(e) => setSingbox(e.target.value)}
        />
      </label>
      <label className="field">
        <span>{t('rules_clash')}</span>
        <textarea
          className="code-area"
          rows={5}
          spellCheck={false}
          disabled={!loaded}
          placeholder={t('rules_clash_ph')}
          value={clash}
          onChange={(e) => setClash(e.target.value)}
        />
      </label>
      <div className="row-end">
        <button type="button" className="btn primary" disabled={busy || !loaded} onClick={() => void save()}>
          {busy ? t('loading') : t('save')}
        </button>
      </div>
    </section>
  );
}
