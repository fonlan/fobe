import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { copyText, fmtTime } from '../format';
import type { SubAccessRow, SubscriptionEntry, SubscriptionEntryInput, SubscriptionRow, SubscriptionToken, TemplateRow } from '../types';
import Modal from '../components/Modal';
import { useToast } from '../components/Toast';

/** The only placeholder a template must carry (server validates too). */
const NODES_PH = '{{nodes}}';

/**
 * The removed routing-rules placeholder (§10 实现修订 2026-09-16b): rules now
 * live in the template itself, so the editor flags the token instead of letting
 * the server reject it after a round trip.
 */
const LEGACY_RULES_PH = '{{rules}}';

/** §10.2 default relay auto-name; mirrors DefaultRelayNameFormat server-side. */
const DEFAULT_RELAY_FORMAT = '{name} · {relay}:{port}';

export default function Subscriptions() {
  const { t } = useI18n();
  // Page-level feedback for the copy button: the toast is fixed-positioned, so
  // it belongs to the page and not to a card that may scroll out of view.
  const { show: showToast, node: toastNode } = useToast();
  // Templates are owned here: the subscription rows bind them (template picker)
  // and the templates card edits them, so one fetch feeds both.
  const [templates, setTemplates] = useState<TemplateRow[]>([]);
  const [tplErr, setTplErr] = useState<string | null>(null);
  /**
   * §10.2: the global relay name format decides the `auto_name` an open picker
   * displays, so saving that card bumps this counter and makes the picker
   * re-read its rows instead of showing a stale name.
   */
  const [relayVersion, setRelayVersion] = useState(0);

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
      {/* §10.1 实现修订 2026-09-16: the global anytls password is generated and
          owned by the server, so the panel has no field for it. This line is
          here because the honest answer to "where do I set the password?" is
          now "nowhere" — an operator who does not know that will hunt for it. */}
      <p className="hint">{t('subs_anytls_password_hint')}</p>
      <RelayEntryCard onSaved={() => setRelayVersion((v) => v + 1)} />
      <SubscriptionsCard templates={templates} onToast={showToast} relayVersion={relayVersion} />
      <TemplatesCard templates={templates} loadError={tplErr} onReload={loadTemplates} />
      {toastNode}
    </div>
  );
}

// --- subscriptions -----------------------------------------------------------

function SubscriptionsCard({
  templates,
  onToast,
  relayVersion,
}: {
  templates: TemplateRow[];
  onToast: (message: string, tone?: 'ok' | 'error') => void;
  relayVersion: number;
}) {
  const { t } = useI18n();
  const [subs, setSubs] = useState<SubscriptionRow[] | null>(null);
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
      const s = await api.listSubscriptions();
      setSubs(s.subscriptions ?? []);
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
    <section className="card" style={{ marginTop: 16 }}>
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
                <th>{t('sub_entries_col')}</th>
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
                  <td className="mono">{sub.entry_count}</td>
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
          <EntryPicker
            key={'n' + sub.id}
            sub={sub}
            reloadKey={relayVersion}
            onSaved={() => void load()}
          />
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

/**
 * §10.2: the entry identity as one string — mirrors the server's own key so an
 * edit can be applied to exactly one row without trusting array positions.
 */
function entryKey(e: SubscriptionEntry): string {
  return [e.node_id, e.relay_node_id, e.proto, e.src_port, e.iface].join('|');
}

/** §10.2 unavailable codes; anything unknown falls back to a generic line. */
const ENTRY_REASON_KEYS: Record<string, string> = {
  not_ready: 'sub_entry_reason_not_ready',
  relay_not_ready: 'sub_entry_reason_relay_not_ready',
  relay_gone: 'sub_entry_reason_relay_gone',
};

/** Panel-written booleans use the on/off spelling; unset (empty) means on. */
function relayAutoOff(v: string): boolean {
  switch (v.trim().toLowerCase()) {
    case '0':
    case 'false':
    case 'off':
    case 'no':
      return true;
    default:
      return false;
  }
}

// §10.2 entry picker. A subscription binds *entries*, not nodes: the same node
// can be listed twice — once as its own inbound and once through a relay's
// nftables DNAT — so every row is a checkbox plus its own naming input, grouped
// per node with the direct ingress first (the order the renderer emits).
//
// Entries that cannot render right now (mid-reinstall, relay rule just removed)
// stay listed, muted and still checkable: the binding is real and silently
// dropping it would change what clients fetch with no way to unbind it. Every
// row shown is sent back on save, unchecked ones included — the server turns an
// unchecked bound row into a tombstone, which is what keeps the auto-enrolment
// reconciler from re-adding it.
function EntryPicker({
  sub,
  reloadKey,
  onSaved,
}: {
  sub: SubscriptionRow;
  reloadKey: number;
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [entries, setEntries] = useState<SubscriptionEntry[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      const r = await api.listSubscriptionEntries(sub.id);
      setEntries(r.entries);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      setEntries(null);
    }
  }, [sub.id, t]);

  useEffect(() => {
    void reload();
  }, [reload, reloadKey]);

  const patch = (key: string, fn: (e: SubscriptionEntry) => SubscriptionEntry) => {
    setSaved(false);
    setEntries((prev) => (prev === null ? prev : prev.map((e) => (entryKey(e) === key ? fn(e) : e))));
  };

  const save = async () => {
    if (entries === null || busy) return;
    setBusy(true);
    setErr(null);
    setSaved(false);
    try {
      await api.setSubscriptionEntries(
        sub.id,
        entries.map(
          (e): SubscriptionEntryInput => ({
            node_id: e.node_id,
            relay_node_id: e.relay_node_id,
            proto: e.proto,
            src_port: e.src_port,
            iface: e.iface,
            // Trimmed here because the server bounds aliases but does not trim
            // them: a name of only spaces would render blank in every client.
            alias: e.alias.trim(),
            selected: e.selected,
          }),
        ),
      );
      // Re-read instead of trusting the local copy: the server may have written
      // tombstones, and reading the list is also its auto-enrolment trigger.
      await reload();
      onSaved();
      setSaved(true);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  // Group by target node, direct ingress first, then relays.
  const groups = useMemo(() => {
    const order: string[] = [];
    const byNode = new Map<string, SubscriptionEntry[]>();
    for (const e of entries ?? []) {
      const list = byNode.get(e.node_id);
      if (list) {
        list.push(e);
      } else {
        byNode.set(e.node_id, [e]);
        order.push(e.node_id);
      }
    }
    return order.map((nodeID) => {
      const list = byNode.get(nodeID) ?? [];
      return {
        nodeID,
        nodeName: list[0]?.node_name || nodeID,
        direct: list.filter((e) => e.relay_node_id === ''),
        relayed: list.filter((e) => e.relay_node_id !== ''),
      };
    });
  }, [entries]);

  const anyUnavailable = (entries ?? []).some((e) => !e.available);

  const row = (e: SubscriptionEntry) => {
    const key = entryKey(e);
    const muted = !e.available;
    const effective = e.alias.trim() || e.auto_name;
    const label =
      e.relay_node_id === ''
        ? e.node_name || e.node_id
        : t('sub_entry_via', { relay: e.relay_name || e.relay_node_id, port: e.src_port });
    return (
      <div key={key} className={'sub-entry' + (muted ? ' sub-entry-unavailable' : '')}>
        <div className="row-wrap">
          <label
            className={'check-chip' + (muted ? ' chip-muted' : '')}
            title={e.source || undefined}
          >
            <input
              type="checkbox"
              checked={e.selected}
              onChange={() => patch(key, (x) => ({ ...x, selected: !x.selected }))}
            />
            {label}
          </label>
          {e.relay_node_id === '' ? (
            <span className="chip">{t('sub_entry_direct')}</span>
          ) : null}
          {e.discovered ? <span className="chip">{t('sub_entry_discovered')}</span> : null}
          {e.source !== '' ? <span className="hint">{t('sub_entry_source', { comment: e.source })}</span> : null}
        </div>
        <div className="cmd-input-row sub-entry-name">
          <input
            className="mono"
            value={e.alias}
            maxLength={64}
            placeholder={e.auto_name}
            title={t('sub_entry_alias_ph')}
            aria-label={t('sub_entry_alias')}
            onChange={(ev) => patch(key, (x) => ({ ...x, alias: ev.target.value }))}
          />
          <span className="hint nowrap">{t('sub_entry_effective', { name: effective })}</span>
        </div>
        {!e.available && (
          <p className="hint sub-entry-reason">{t(ENTRY_REASON_KEYS[e.reason] ?? 'sub_entry_reason_unknown')}</p>
        )}
        {e.warning === 'shadowed' && <p className="sub-entry-warn">{t('sub_entry_warning_shadowed')}</p>}
      </div>
    );
  };

  return (
    <div className="card" style={{ marginTop: 8 }}>
      <h4>{t('sub_nodes_of', { name: sub.name })}</h4>
      <p className="hint">{t('sub_entries_hint')}</p>
      {err && <div className="form-error">{err}</div>}
      {entries === null && !err && <div className="hint">{t('loading')}</div>}
      {entries !== null && entries.length === 0 && <div className="hint">{t('sub_entries_empty')}</div>}
      {anyUnavailable && <p className="hint">{t('sub_entries_unavailable_hint')}</p>}
      {groups.map((g) => (
        <div className="sub-entry-group" key={g.nodeID}>
          <h5 className="sub-entry-node">{g.nodeName}</h5>
          {g.direct.map(row)}
          {g.relayed.map(row)}
        </div>
      ))}
      <div className="row-end">
        {saved && <span className="form-ok">{t('server_edit_saved')}</span>}
        <button
          type="button"
          className="btn primary small"
          disabled={busy || entries === null}
          onClick={() => void save()}
        >
          {busy ? t('loading') : t('save')}
        </button>
      </div>
    </div>
  );
}

/**
 * §10.2 global relay settings. They are panel-wide (the auto-name is derived on
 * every read, for every subscription), which is why the card sits above the
 * list instead of inside one subscription's row.
 */
function RelayEntryCard({ onSaved }: { onSaved: () => void }) {
  const { t } = useI18n();
  const [auto, setAuto] = useState(true);
  const [format, setFormat] = useState('');
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    api
      .getSettings()
      .then((r) => {
        if (!alive) return;
        const map: Record<string, string> = {};
        for (const s of r.settings ?? []) map[s.key] = s.set ? s.value : '';
        setAuto(!relayAutoOff(map['sub.relay_auto_include'] ?? ''));
        setFormat(map['sub.relay_name_format'] ?? '');
        setLoaded(true);
      })
      .catch((e) => {
        if (alive) setErr(apiErrorMessage(e, t));
      });
    return () => {
      alive = false;
    };
  }, [t]);

  const save = async () => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setSaved(false);
    const trimmed = format.trim();
    try {
      // Both keys travel together, and an empty format is meaningful (reset to
      // the built-in default) — unlike Settings' "skip empty fields" rule.
      await api.putSettings({
        'sub.relay_auto_include': auto ? 'on' : 'off',
        'sub.relay_name_format': trimmed,
      });
      setFormat(trimmed);
      setSaved(true);
      onSaved();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <h3>{t('sub_relay_card')}</h3>
      <p className="hint">{t('sub_relay_desc')}</p>
      {err && <div className="form-error">{err}</div>}
      <label className="check-chip" style={{ marginTop: 8 }}>
        <input
          type="checkbox"
          checked={auto}
          disabled={busy || !loaded}
          onChange={(e) => {
            setAuto(e.target.checked);
            setSaved(false);
          }}
        />
        {t('sub_relay_auto')}
      </label>
      <p className="hint">{t('sub_relay_auto_hint')}</p>
      <label className="field" style={{ marginTop: 12, marginBottom: 0 }}>
        <span>{t('sub_relay_format')}</span>
        <input
          className="mono"
          value={format}
          placeholder={DEFAULT_RELAY_FORMAT}
          disabled={busy || !loaded}
          onChange={(e) => {
            setFormat(e.target.value);
            setSaved(false);
          }}
        />
        <small className="hint">{t('sub_relay_format_hint')}</small>
        <small className="hint">{t('sub_relay_format_default', { format: DEFAULT_RELAY_FORMAT })}</small>
      </label>
      <div className="row-end">
        {saved && <span className="form-ok">{t('sub_relay_saved')}</span>}
        <button type="button" className="btn primary small" disabled={busy || !loaded} onClick={() => void save()}>
          {busy ? t('loading') : t('save')}
        </button>
      </div>
    </section>
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

/** §10 修订: refusal codes stored in sub_access_logs.reason. */
const ACCESS_REASON_KEYS: Record<string, string> = {
  '': 'sub_access_served',
  ua_mismatch: 'sub_access_ua_mismatch',
  disabled: 'sub_access_disabled',
  render_error: 'sub_access_render_error',
};

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
                <th>{t('sub_access_result')}</th>
                <th>{t('session_ip')}</th>
                <th>{t('session_ua')}</th>
              </tr>
            </thead>
            <tbody>
              {logs.map((l, i) => (
                <tr key={i} className={l.reason === '' ? '' : 'row-muted'}>
                  <td className="mono nowrap">{fmtTime(l.ts)}</td>
                  <td className="nowrap">
                    <span className={'chip ' + (l.reason === '' ? 'status-ok' : 'status-failed')}>
                      {t(ACCESS_REASON_KEYS[l.reason] ?? 'sub_access_refused')}
                    </span>
                  </td>
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

  const placeholdersMissing = !content.includes(NODES_PH);
  const legacyRulesPh = content.includes(LEGACY_RULES_PH);

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
          <p className={'hint' + (placeholdersMissing || legacyRulesPh ? ' form-error' : '')}>
            {legacyRulesPh
              ? t('tpl_obsolete_placeholder')
              : placeholdersMissing
                ? t('tpl_placeholders_missing')
                : t('tpl_placeholders_ok')}
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

