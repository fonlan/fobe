import { useCallback, useEffect, useMemo, useState } from 'react';
import * as api from '../api';
import { ApiError, apiErrorMessage } from '../api';
import Modal from '../components/Modal';
import { SaveRow, Toggle } from '../components/SettingsForm';
import { DownloadIcon, PencilIcon, RefreshIcon, TrashIcon } from '../components/Icons';
import { useI18n, type TFn } from '../i18n';
import { fmtTime } from '../format';
import type {
  AICatalog,
  AIFetchedModel,
  AIModel,
  AIModelImportResult,
  AIModelInput,
  AIProvider,
  AIProviderInput,
  ModelsDevSlug,
  ModelsDevStatus,
  SettingView,
} from '../types';
import { ErrorState } from '../components/ErrorState';

/**
 * /settings/ai — multi-provider / multi-model management (design §12.1, §12.5).
 *
 * Three server-side truths shape this page and are deliberately NOT re-derived
 * here:
 *
 *  1. `provider.models[].effective_levels` is the set of thinking levels the
 *     selected pair actually exposes (Anthropic adds `off` because its thinking
 *     field is opt-in). The panel renders that array verbatim; an empty array
 *     is stated in words instead of rendering an empty dropdown.
 *  2. Secrets are write-only. api_key / extra_headers are omitted from the
 *     request when untouched (the server keeps what it has) and sent as "" to
 *     clear them. The form can therefore never echo a ciphertext back.
 *  3. `overridden_fields` means "these fields were hand-edited, a models.dev
 *     refresh must never overwrite them". Every save unions the fields the
 *     operator actually changed into that list, and each frozen field gets a
 *     visible badge plus a per-field restore action.
 */

/** The three protocols the server implements; anything else is refused (bad_protocol). */
const PROTOCOLS = ['openai-completions', 'openai-responses', 'anthropic-messages'] as const;

/** Endpoint path the adapter appends to the ROOT base_url (design §12.5). */
const PROTOCOL_PATHS: Record<string, string> = {
  'openai-completions': '/chat/completions',
  'openai-responses': '/responses',
  'anthropic-messages': '/messages',
};

const PROTOCOL_LABEL_KEY: Record<string, string> = {
  'openai-completions': 'ai_protocol_openai_completions',
  'openai-responses': 'ai_protocol_openai_responses',
  'anthropic-messages': 'ai_protocol_anthropic_messages',
};

/** Unified thinking-level vocabulary (§12.5), the only values the server accepts. */
const REASONING_LEVELS = ['off', 'minimal', 'low', 'medium', 'high'] as const;

/** Fields a manual edit may freeze — mirrors the server's aiModelOverrideFields. */
const OVERRIDE_FIELDS = [
  'display_name',
  'context_window',
  'max_output_tokens',
  'input_modalities',
  'output_modalities',
  'reasoning_levels',
] as const;

const FIELD_LABEL_KEY: Record<string, string> = {
  display_name: 'ai_model_display_name',
  context_window: 'ai_model_context',
  max_output_tokens: 'ai_model_max_output',
  input_modalities: 'ai_model_input_modalities',
  output_modalities: 'ai_model_output_modalities',
  reasoning_levels: 'ai_model_levels',
};

/** Modalities seen in models.dev; stored values outside this set are preserved. */
const MODALITIES = ['text', 'image', 'audio', 'video', 'pdf'] as const;

// --- small pure helpers ------------------------------------------------------

function protocolLabel(t: TFn, protocol: string): string {
  if (!protocol) return t('ai_protocol_unset');
  const key = PROTOCOL_LABEL_KEY[protocol];
  return key ? t(key) : protocol;
}

function fieldLabel(t: TFn, field: string): string {
  const key = FIELD_LABEL_KEY[field];
  return key ? t(key) : field;
}

/** Translate a server error code, falling back to the raw code (never the key). */
function codeText(t: TFn, code: string): string {
  const key = 'err_' + code;
  const v = t(key);
  return v === key ? code : v;
}

/**
 * The URL the adapter will actually call: trim the root, append the protocol
 * path. Shown live in the provider form so a wrong base_url is visible before
 * the first request instead of surfacing as an opaque upstream 404.
 */
function endpointPreview(baseURL: string, protocol: string): string {
  const base = baseURL.trim().replace(/\/+$/, '');
  if (!base) return '';
  const path = PROTOCOL_PATHS[protocol];
  return path ? base + path : base;
}

/** Non-negative integer field: '' means 0, anything unparsable means NaN. */
function numField(value: string): number {
  const trimmed = value.trim();
  if (trimmed === '') return 0;
  const n = Number(trimmed);
  return Number.isInteger(n) && n >= 0 ? n : NaN;
}

function toggleIn(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value];
}

function uniq(list: string[]): string[] {
  return [...new Set(list)];
}

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const s = new Set(b);
  return a.every((v) => s.has(v));
}

export default function SettingsAI() {
  const { t } = useI18n();
  const [catalog, setCatalog] = useState<AICatalog | null>(null);
  const [modelsDev, setModelsDev] = useState<ModelsDevStatus | null>(null);
  const [settings, setSettings] = useState<Record<string, SettingView>>({});
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [err, setErr] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [rowBusy, setRowBusy] = useState<string | null>(null);
  const [mdBusy, setMdBusy] = useState(false);
  // Modal targets: null = closed, 'new' = creating.
  const [providerEdit, setProviderEdit] = useState<AIProvider | 'new' | null>(null);
  const [fetchFor, setFetchFor] = useState<AIProvider | null>(null);
  const [modelEdit, setModelEdit] = useState<AIModel | 'new' | null>(null);

  const flash = useCallback((text: string) => {
    setMsg(text);
    window.setTimeout(() => setMsg(null), 5000);
  }, []);

  const load = useCallback(async () => {
    try {
      const [c, md, s] = await Promise.all([api.getAICatalog(), api.getModelsDev(), api.getSettings()]);
      setCatalog(c);
      setModelsDev(md);
      const map: Record<string, SettingView> = {};
      for (const row of s.settings ?? []) map[row.key] = row;
      setSettings(map);
      setDraft({});
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  /** Reload after a mutation, then surface the outcome as a flash message. */
  const reload = useCallback(
    async (message?: string) => {
      await load();
      if (message) flash(message);
    },
    [load, flash],
  );

  const saveSettings = async (payload: Record<string, string>) => {
    if (busy || Object.keys(payload).length === 0) return;
    setBusy(true);
    try {
      await api.putSettings(payload);
      await load();
      flash(t('settings_saved'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const refreshModelsDev = async () => {
    setMdBusy(true);
    setErr(null);
    try {
      await api.refreshModelsDev();
      await reload(t('ai_modelsdev_refreshed'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setMdBusy(false);
    }
  };

  const toggleProvider = async (p: AIProvider, on: boolean) => {
    setRowBusy(p.id);
    setErr(null);
    try {
      await api.updateAIProvider(p.id, {
        name: p.name,
        protocol: p.protocol,
        base_url: p.base_url,
        models_dev_slug: p.models_dev_slug,
        enabled: on,
      });
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setRowBusy(null);
    }
  };

  const deleteProvider = async (p: AIProvider) => {
    if (!window.confirm(t('ai_delete_provider_confirm', { name: p.name, n: p.model_ids.length }))) return;
    setRowBusy(p.id);
    setErr(null);
    try {
      await api.deleteAIProvider(p.id);
      await reload(t('ai_provider_deleted'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setRowBusy(null);
    }
  };

  const deleteModel = async (m: AIModel) => {
    if (!window.confirm(t('ai_delete_model_confirm', { id: m.id }))) return;
    setErr(null);
    try {
      await api.deleteAIModel(m.id);
      await reload(t('ai_model_deleted'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  };

  const providerName = useCallback(
    (id: string) => catalog?.providers.find((p) => p.id === id)?.name ?? id,
    [catalog],
  );

  // An unset key behaves as "allow" server-side (§12.3), so that is what the
  // select shows and what Save sends when the operator never touched it.
  const policy = draft['ai.default_policy'] ?? (settings['ai.default_policy']?.value || 'allow');

  if (!catalog) {
    return (
      <div className="stack-lg">
        <div className="page-head">
          <h2>{t('sec_ai')}</h2>
        </div>
        {err ? (
          <ErrorState message={err} onRetry={() => () => void load()} />
        ) : (
          <div className="loading">{t('loading')}</div>
        )}
      </div>
    );
  }

  const slugs = modelsDev?.slugs ?? [];

  return (
    <div className="stack-lg">
      <div className="page-head">
        <h2>{t('sec_ai')}</h2>
      </div>
      <p className="hint">{t('ai_page_hint')}</p>

      {err && (
        <ErrorState message={err} onRetry={() => () => void load()} />
      )}
      {msg && <div className="form-ok">{msg}</div>}

      <section className="card">
        <div className="row-between">
          <h3>{t('ai_sec_providers')}</h3>
          <button type="button" className="btn primary small" onClick={() => setProviderEdit('new')}>
            {t('ai_add_provider')}
          </button>
        </div>
        {catalog.providers.length === 0 ? (
          <p className="hint">{t('ai_providers_empty')}</p>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{t('ai_col_provider')}</th>
                  <th>{t('ai_col_protocol')}</th>
                  <th>{t('ai_col_endpoint')}</th>
                  <th>{t('ai_col_models')}</th>
                  <th>{t('ai_col_credentials')}</th>
                  <th>{t('enabled')}</th>
                  <th className="col-actions" />
                </tr>
              </thead>
              <tbody>
                {catalog.providers.map((p) => (
                  <tr key={p.id}>
                    <td>
                      <div>{p.name}</div>
                      <div className="hint mono">{p.id}</div>
                    </td>
                    <td className="nowrap">{protocolLabel(t, p.protocol)}</td>
                    {/* Long URLs are the norm; break anywhere rather than pushing
                        the auto-layout table past the card. */}
                    <td className="mono break-anywhere">{p.endpoint || '-'}</td>
                    <td className="mono nowrap">{t('ai_models_count', { n: p.model_ids.length })}</td>
                    <td className="nowrap">
                      <span className={'chip' + (p.has_key ? ' status-ok' : '')}>
                        {p.has_key ? t('ai_api_key_set') : t('ai_api_key_unset')}
                      </span>
                      {p.has_headers && (
                        <span className="chip" title={p.header_names.join(', ')}>
                          {t('ai_extra_headers_set', { names: p.header_names.join(', ') })}
                        </span>
                      )}
                    </td>
                    <td className="nowrap">
                      <Toggle
                        checked={p.enabled}
                        disabled={rowBusy === p.id}
                        onChange={(on) => void toggleProvider(p, on)}
                        label={p.enabled ? t('enabled') : t('disabled')}
                      />
                    </td>
                    <td className="nowrap col-actions">
                      <button
                        type="button"
                        className="icon-btn"
                        title={t('edit')}
                        aria-label={t('edit')}
                        disabled={rowBusy === p.id}
                        onClick={() => setProviderEdit(p)}
                      >
                        <PencilIcon />
                      </button>
                      <button
                        type="button"
                        className="icon-btn"
                        title={t('ai_fetch_models')}
                        aria-label={t('ai_fetch_models')}
                        disabled={rowBusy === p.id}
                        onClick={() => setFetchFor(p)}
                      >
                        <DownloadIcon />
                      </button>
                      <button
                        type="button"
                        className="icon-btn danger"
                        title={t('delete')}
                        aria-label={t('delete')}
                        disabled={rowBusy === p.id}
                        onClick={() => void deleteProvider(p)}
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
      </section>

      <section className="card">
        <div className="row-between">
          <h3>{t('ai_sec_models')}</h3>
          <button type="button" className="btn small" onClick={() => setModelEdit('new')}>
            {t('ai_add_model')}
          </button>
        </div>
        {catalog.models.length === 0 ? (
          <p className="hint">{t('ai_models_empty')}</p>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{t('ai_model_id')}</th>
                  <th>{t('ai_model_display_name')}</th>
                  <th>{t('ai_model_context')}</th>
                  <th>{t('ai_model_max_output')}</th>
                  <th>{t('ai_model_levels')}</th>
                  <th>{t('ai_model_providers')}</th>
                  <th>{t('ai_model_source')}</th>
                  <th>{t('enabled')}</th>
                  <th className="col-actions" />
                </tr>
              </thead>
              <tbody>
                {catalog.models.map((m) => {
                  // A models.dev row whose limits are still 0 means the match
                  // never found it (or the first fetch was offline): say so
                  // instead of letting "0 tokens" look like a real limit.
                  const metaPending = m.source === 'models_dev' && (m.context_window === 0 || m.max_output_tokens === 0);
                  return (
                    <tr key={m.id}>
                      <td className="mono break-anywhere">{m.id}</td>
                      <td>{m.display_name}</td>
                      <td className="mono nowrap">{m.context_window || '-'}</td>
                      <td className="mono nowrap">{m.max_output_tokens || '-'}</td>
                      <td className="nowrap">
                        {m.reasoning_levels.length === 0 ? (
                          <span className="hint">{t('ai_model_levels_empty')}</span>
                        ) : (
                          m.reasoning_levels.map((l) => (
                            <span key={l} className="chip">
                              {l}
                            </span>
                          ))
                        )}
                      </td>
                      <td className="nowrap">
                        {m.provider_ids.length === 0
                          ? '-'
                          : m.provider_ids.map((id) => (
                              <span key={id} className="chip">
                                {providerName(id)}
                              </span>
                            ))}
                      </td>
                      <td className="nowrap">
                        <span className="chip">
                          {m.source === 'models_dev' ? t('ai_model_source_models_dev') : t('ai_model_source_manual')}
                        </span>
                        {metaPending && <span className="chip status-timeout">{t('ai_model_meta_pending')}</span>}
                        {m.overridden_fields.length > 0 && (
                          <span className="chip primary-chip">{t('ai_model_frozen', { n: m.overridden_fields.length })}</span>
                        )}
                      </td>
                      <td className="nowrap">
                        <span className={'chip' + (m.enabled ? ' status-ok' : ' status-timeout')}>
                          {m.enabled ? t('enabled') : t('disabled')}
                        </span>
                      </td>
                      <td className="nowrap col-actions">
                        <button
                          type="button"
                          className="icon-btn"
                          title={t('edit')}
                          aria-label={t('edit')}
                          onClick={() => setModelEdit(m)}
                        >
                          <PencilIcon />
                        </button>
                        <button
                          type="button"
                          className="icon-btn danger"
                          title={t('delete')}
                          aria-label={t('delete')}
                          onClick={() => void deleteModel(m)}
                        >
                          <TrashIcon />
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <DefaultCard catalog={catalog} onChanged={reload} onError={setErr} />

      <ModelsDevCard status={modelsDev} busy={mdBusy} onRefresh={() => void refreshModelsDev()} />

      <section className="card">
        <h3>{t('ai_sec_policy')}</h3>
        <p className="hint">{t('ai_policy_hint')}</p>
        <div className="form-grid">
          {/*
            Hand-rolled instead of the shared useField renderer: an unset
            ai.default_policy stores "", and the shared select would then hold a
            value no option matches (a blank control). The server's behaviour
            when the key is absent is "allow" (design §12.3), so that is what an
            unset row must display.
          */}
          <label className="field">
            <span>{t('ai_default_policy')}</span>
            <select
              value={policy}
              onChange={(e) => setDraft((d) => ({ ...d, 'ai.default_policy': e.target.value }))}
            >
              <option value="allow">{t('policy_allow')}</option>
              <option value="confirm">{t('policy_confirm')}</option>
            </select>
          </label>
        </div>
        <SaveRow
          busy={busy}
          savedMsg={null}
          // The effective value always travels, so pressing Save without having
          // touched the select is a real action rather than a silent no-op.
          onSave={() => void saveSettings({ 'ai.default_policy': policy })}
          label={t('save')}
        />
      </section>

      {providerEdit && (
        <ProviderEditor
          provider={providerEdit === 'new' ? null : providerEdit}
          slugs={slugs}
          onClose={() => setProviderEdit(null)}
          onSaved={async (message) => {
            setProviderEdit(null);
            await reload(message);
          }}
        />
      )}

      {fetchFor && (
        <FetchModelsModal
          // Re-read the provider from the live catalog rather than the snapshot
          // taken when the button was pressed: after an import the "attached to
          // this provider" flags have to reflect the new links.
          provider={catalog.providers.find((p) => p.id === fetchFor.id) ?? fetchFor}
          known={new Set(catalog.models.map((m) => m.id))}
          onClose={() => setFetchFor(null)}
          onSaved={reload}
        />
      )}

      {modelEdit && (
        <ModelEditor
          model={modelEdit === 'new' ? null : modelEdit}
          catalog={catalog}
          onClose={() => setModelEdit(null)}
          onSaved={async (message) => {
            setModelEdit(null);
            await reload(message);
          }}
        />
      )}
    </div>
  );
}

// --- provider form -----------------------------------------------------------

/**
 * Create/edit one provider (§12.1). Two behaviours are worth naming:
 *
 *  - The endpoint preview mirrors the server's ROOT + protocol path rule, so a
 *    base_url that already contains /chat/completions is visible as a doubled
 *    path before saving.
 *  - Picking a models.dev slug overwrites protocol + base_url with the slug's
 *    values (both stay editable). When the slug carries no protocol guess
 *    (azure / google / vertex …) the field goes EMPTY on purpose: the panel
 *    will not fill in a protocol that cannot work, it makes the operator choose.
 */
function ProviderEditor({
  provider,
  slugs,
  onClose,
  onSaved,
}: {
  provider: AIProvider | null;
  slugs: ModelsDevSlug[];
  onClose: () => void;
  onSaved: (message: string) => void | Promise<void>;
}) {
  const { t } = useI18n();
  const isNew = provider === null;
  const [name, setName] = useState(provider?.name ?? '');
  const [protocol, setProtocol] = useState(provider?.protocol ?? '');
  const [baseURL, setBaseURL] = useState(provider?.base_url ?? '');
  const [slug, setSlug] = useState(provider?.models_dev_slug ?? '');
  const [apiKey, setAPIKey] = useState('');
  const [headers, setHeaders] = useState('');
  const [clearKey, setClearKey] = useState(false);
  const [clearHeaders, setClearHeaders] = useState(false);
  const [enabled, setEnabled] = useState(provider?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Sorted so the dropdown does not reshuffle between renders (the server
  // emits the slugs in map order).
  const slugOptions = useMemo(() => [...slugs].sort((a, b) => a.slug.localeCompare(b.slug)), [slugs]);
  const pickedSlug = slugOptions.find((s) => s.slug === slug) ?? null;

  const pickSlug = (value: string) => {
    setSlug(value);
    const found = slugOptions.find((s) => s.slug === value);
    if (!found) return;
    setProtocol(found.protocol ?? '');
    setBaseURL(found.base_url ?? '');
  };

  const preview = endpointPreview(baseURL, protocol);
  const canSave = protocol !== '' && baseURL.trim() !== '' && !busy;

  const submit = async () => {
    if (!canSave) return;
    setBusy(true);
    setErr(null);
    const body: AIProviderInput = {
      name: name.trim(),
      protocol,
      base_url: baseURL.trim(),
      models_dev_slug: slug,
      enabled,
    };
    // Omitted = keep whatever is stored; "" = clear. Never both.
    if (clearKey) body.api_key = '';
    else if (apiKey.trim() !== '') body.api_key = apiKey.trim();
    if (clearHeaders) body.extra_headers = '';
    else if (headers.trim() !== '') body.extra_headers = headers.trim();
    try {
      if (isNew) await api.createAIProvider(body);
      else if (provider) await api.updateAIProvider(provider.id, body);
      await onSaved(isNew ? t('ai_provider_created') : t('ai_provider_saved'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={isNew ? t('ai_new_provider') : t('ai_edit_provider')} onClose={onClose} wide>
      {err && <div className="form-error">{err}</div>}
      <div className="form-grid">
        <label className="field">
          <span>{t('ai_provider_name')}</span>
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder={t('ai_provider_name_hint')} />
        </label>
        <label className="field">
          <span>{t('ai_provider_protocol')}</span>
          <select value={protocol} onChange={(e) => setProtocol(e.target.value)}>
            <option value="">{t('ai_protocol_choose')}</option>
            {PROTOCOLS.map((p) => (
              <option key={p} value={p}>
                {protocolLabel(t, p)}
              </option>
            ))}
          </select>
        </label>
      </div>
      {pickedSlug && pickedSlug.protocol === '' && <p className="form-error">{t('ai_provider_slug_no_protocol')}</p>}

      <label className="field">
        <span>{t('ai_provider_base_url')}</span>
        <input value={baseURL} onChange={(e) => setBaseURL(e.target.value)} placeholder="https://api.example.com/v1" />
      </label>
      <p className="hint">{t('ai_provider_base_url_hint')}</p>
      <p className="hint">
        {t('ai_provider_endpoint')}:{' '}
        {preview ? <span className="mono">{preview}</span> : t('ai_provider_endpoint_none')}
      </p>

      <label className="field">
        <span>{t('ai_provider_slug')}</span>
        <select value={slug} onChange={(e) => pickSlug(e.target.value)}>
          <option value="">{t('ai_provider_slug_none')}</option>
          {slugOptions.map((s) => (
            <option key={s.slug} value={s.slug}>
              {s.slug} — {s.name}
            </option>
          ))}
        </select>
      </label>
      <p className="hint">{t('ai_provider_slug_hint')}</p>
      {pickedSlug && pickedSlug.base_url === '' && baseURL.trim() === '' && (
        <p className="form-error">{t('ai_provider_slug_no_base')}</p>
      )}

      <label className="field">
        <span>
          {t('ai_api_key')}
          {provider?.has_key && <em className="sensitive-tag set">{t('ai_api_key_set')}</em>}
        </span>
        <input
          type="password"
          value={apiKey}
          disabled={clearKey}
          placeholder={provider?.has_key ? t('ai_api_key_placeholder_set') : t('ai_api_key_placeholder_unset')}
          onChange={(e) => setAPIKey(e.target.value)}
        />
      </label>
      <p className="hint">{t('ai_api_key_hint')}</p>
      {provider?.has_key && (
        <label className="check-chip">
          <input
            type="checkbox"
            checked={clearKey}
            onChange={(e) => {
              setClearKey(e.target.checked);
              if (e.target.checked) setAPIKey('');
            }}
          />
          {t('ai_clear_key')}
        </label>
      )}

      <label className="field" style={{ marginTop: 12 }}>
        <span>
          {t('ai_extra_headers')}
          {provider?.has_headers && (
            <em className="sensitive-tag set">{t('ai_extra_headers_set', { names: provider.header_names.join(', ') })}</em>
          )}
        </span>
        <textarea
          className="code-area"
          rows={3}
          value={headers}
          disabled={clearHeaders}
          placeholder={'{"HTTP-Referer":"https://x"}'}
          onChange={(e) => setHeaders(e.target.value)}
        />
      </label>
      <p className="hint">{t('ai_extra_headers_hint')}</p>
      {provider?.has_headers && (
        <label className="check-chip">
          <input
            type="checkbox"
            checked={clearHeaders}
            onChange={(e) => {
              setClearHeaders(e.target.checked);
              if (e.target.checked) setHeaders('');
            }}
          />
          {t('ai_clear_headers')}
        </label>
      )}

      <div className="row-between" style={{ marginTop: 12 }}>
        <Toggle checked={enabled} onChange={setEnabled} label={t('ai_provider_enabled')} />
        <div className="row-gap">
          <button type="button" className="btn" onClick={onClose}>
            {t('cancel')}
          </button>
          <button type="button" className="btn primary" disabled={!canSave} onClick={() => void submit()}>
            {busy ? '…' : t('save')}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// --- fetch-models picker -----------------------------------------------------

/**
 * "Fetch models" (§12.5): ask the provider for its own listing, mark each id
 * with whether it is already in the library, and import the ticked ones in one
 * call. The endpoint belongs to a later stage, so a missing route is reported
 * as "not available here" plus the manual alternative — never as a crash.
 */
function FetchModelsModal({
  provider,
  known,
  onClose,
  onSaved,
}: {
  provider: AIProvider;
  known: Set<string>;
  onClose: () => void;
  onSaved: (message?: string) => void | Promise<void>;
}) {
  const { t } = useI18n();
  const [state, setState] = useState<'loading' | 'ready' | 'error'>('loading');
  const [fetched, setFetched] = useState<AIFetchedModel[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [query, setQuery] = useState('');
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<AIModelImportResult | null>(null);

  useEffect(() => {
    let alive = true;
    api
      .fetchAIProviderModels(provider.id)
      .then((r) => {
        if (!alive) return;
        setFetched(r.models ?? []);
        setState('ready');
      })
      .catch((e: unknown) => {
        if (!alive) return;
        // A build whose route table predates this endpoint answers
        // unknown_endpoint (or not_found); that is "not available yet", not a
        // network failure. Manual entry stays open either way.
        if (e instanceof ApiError && (e.code === 'unknown_endpoint' || e.code === 'not_found')) {
          setUnavailable(true);
          setState('error');
          return;
        }
        setErr(apiErrorMessage(e, t));
        setState('error');
      });
    return () => {
      alive = false;
    };
  }, [provider.id, t]);

  const linked = useMemo(() => new Set(provider.model_ids), [provider.model_ids]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return fetched;
    return fetched.filter((m) => m.id.toLowerCase().includes(q) || (m.display_name ?? '').toLowerCase().includes(q));
  }, [fetched, query]);

  const selected = Object.keys(picked).filter((k) => picked[k]);

  // Provenance of the last import: how many ids got metadata at all, and how
  // many of those were resolved among several providers' copies (id matching).
  const sources = result ? Object.values(result.sources ?? {}) : [];
  const ambiguous = sources.filter((s) => s.candidates > 1).length;

  const doImport = async () => {
    if (selected.length === 0 || busy) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await api.importAIProviderModels(provider.id, selected);
      setResult(r);
      setPicked({});
      await onSaved(t('ai_import_added', { n: r.added.length }));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={t('ai_fetch_title', { name: provider.name })} onClose={onClose} wide>
      {err && <div className="form-error">{err}</div>}
      {state === 'loading' && <p className="hint">{t('ai_fetch_loading')}</p>}
      {state === 'error' && (
        <>
          {unavailable ? <p className="form-error">{t('ai_fetch_unavailable')}</p> : null}
          <div className="row-end">
            <button type="button" className="btn" onClick={onClose}>
              {t('close')}
            </button>
          </div>
        </>
      )}

      {state === 'ready' && (
        <>
          {fetched.length === 0 ? (
            <p className="hint">{t('ai_fetch_empty')}</p>
          ) : (
            <>
              <label className="field ai-search">
                <span>{t('ai_fetch_search')}</span>
                <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="gpt-4o" />
              </label>
              <div className="row-wrap">
                <button
                  type="button"
                  className="btn ghost small"
                  onClick={() =>
                    // Merge, not replace: a selection made under an earlier
                    // search term must survive "select all" on the next one.
                    setPicked((prev) => {
                      const next = { ...prev };
                      for (const m of filtered) next[m.id] = true;
                      return next;
                    })
                  }
                >
                  {t('ai_fetch_select_all')}
                </button>
                <button type="button" className="btn ghost small" onClick={() => setPicked({})}>
                  {t('ai_fetch_clear')}
                </button>
                <span className="hint">{t('ai_fetch_selected', { n: selected.length })}</span>
              </div>
              <div className="ai-candidate-list">
                {filtered.length === 0 && <p className="hint">{t('ai_fetch_no_match')}</p>}
                {filtered.map((m) => (
                  <label key={m.id} className="ai-candidate-row">
                    <input
                      type="checkbox"
                      checked={picked[m.id] === true}
                      onChange={(e) => setPicked((prev) => ({ ...prev, [m.id]: e.target.checked }))}
                    />
                    <span className="ai-candidate-id">{m.id}</span>
                    {m.display_name && m.display_name !== m.id && <span className="hint">{m.display_name}</span>}
                    {linked.has(m.id) && <span className="chip status-ok">{t('ai_fetch_linked_here')}</span>}
                    {known.has(m.id) && <span className="chip">{t('ai_fetch_in_library')}</span>}
                    {/* Only the rows that will NOT be auto-filled are marked:
                        that is the actionable half (they need hand-typed
                        limits), and a chip on every matched row would be noise. */}
                    {m.matched === false && <span className="chip">{t('ai_fetch_no_metadata')}</span>}
                  </label>
                ))}
              </div>
            </>
          )}

          {result && (
            <div className="form-ok" style={{ marginTop: 12 }}>
              <div>{t('ai_import_added', { n: result.added.length })}</div>
              {result.unmatched.length > 0 && (
                <div className="form-error">
                  {t('ai_import_unmatched', { n: result.unmatched.length, ids: result.unmatched.join(', ') })}
                  <div className="hint">{t('ai_import_unmatched_hint')}</div>
                </div>
              )}
              {result.frozen.length > 0 && (
                <div className="hint">{t('ai_import_frozen', { n: result.frozen.length, fields: result.frozen.join(', ') })}</div>
              )}
              {sources.length > 0 && <div className="hint">{t('ai_import_matched', { n: sources.length })}</div>}
              {ambiguous > 0 && <div className="hint">{t('ai_import_ambiguous', { n: ambiguous })}</div>}
            </div>
          )}

          <div className="row-end">
            <button type="button" className="btn" onClick={onClose}>
              {t('close')}
            </button>
            <button type="button" className="btn primary" disabled={busy || selected.length === 0} onClick={() => void doImport()}>
              {busy ? '…' : t('ai_fetch_import', { n: selected.length })}
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}

// --- model form --------------------------------------------------------------

/**
 * One model row (§12.5). The row is global; the provider checkboxes create or
 * drop the many-to-many links. Saving unions the fields the operator actually
 * changed into `overridden_fields` — that list is what makes a later refresh
 * leave the correction alone — and each frozen field gets a restore action that
 * unfreezes it and re-runs the match.
 */
function ModelEditor({
  model,
  catalog,
  onClose,
  onSaved,
}: {
  model: AIModel | null;
  catalog: AICatalog;
  onClose: () => void;
  onSaved: (message: string) => void | Promise<void>;
}) {
  const { t } = useI18n();
  const isNew = model === null;
  const [id, setId] = useState(model?.id ?? '');
  const [displayName, setDisplayName] = useState(model?.display_name ?? '');
  const [contextWindow, setContextWindow] = useState(model ? String(model.context_window) : '');
  const [maxOutput, setMaxOutput] = useState(model ? String(model.max_output_tokens) : '');
  const [inputMods, setInputMods] = useState<string[]>(model?.input_modalities ?? ['text']);
  const [outputMods, setOutputMods] = useState<string[]>(model?.output_modalities ?? ['text']);
  const [levels, setLevels] = useState<string[]>(model?.reasoning_levels ?? []);
  const [enabled, setEnabled] = useState(model?.enabled ?? true);
  // Read-only: a restore closes this modal (the reloaded catalog carries the
  // fresh values), so the frozen set is never edited in place.
  const [frozen] = useState<string[]>(model?.overridden_fields ?? []);
  const [linked, setLinked] = useState<string[]>(model?.provider_ids ?? []);
  const [matchProvider, setMatchProvider] = useState(model?.provider_ids[0] ?? '');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const modelID = (model?.id ?? id).trim();

  /** Fields the operator changed in this form (only meaningful when editing). */
  const changedFields = (): string[] => {
    if (!model) return [];
    const changed: Record<string, boolean> = {
      display_name: displayName.trim() !== model.display_name,
      context_window: numField(contextWindow) !== model.context_window,
      max_output_tokens: numField(maxOutput) !== model.max_output_tokens,
      input_modalities: !sameSet(inputMods, model.input_modalities),
      output_modalities: !sameSet(outputMods, model.output_modalities),
      reasoning_levels: !sameSet(levels, model.reasoning_levels),
    };
    // Driven by the allow-list so a new freezable field cannot be forgotten in
    // one place while being accepted in the other.
    return OVERRIDE_FIELDS.filter((f) => changed[f]);
  };

  /** Validate + build the request body; null means "nothing was sent". */
  const collect = (frozenList: string[]): AIModelInput | null => {
    const ctx = numField(contextWindow);
    const max = numField(maxOutput);
    if (!modelID) {
      setErr(t('err_bad_model_id'));
      return null;
    }
    if (Number.isNaN(ctx) || Number.isNaN(max) || (max > 0 && ctx > 0 && max > ctx)) {
      setErr(t('err_bad_limit'));
      return null;
    }
    return {
      id: modelID,
      display_name: displayName.trim() || modelID,
      context_window: ctx,
      max_output_tokens: max,
      input_modalities: inputMods,
      output_modalities: outputMods,
      reasoning_levels: levels,
      overridden_fields: uniq(frozenList),
      // A hand-created row stays "manual" until a match replaces it; editing a
      // models.dev row must not relabel its provenance.
      source: model?.source || 'manual',
      enabled,
    };
  };

  /** Reconcile the provider checkboxes against the links the server holds. */
  const syncLinks = async () => {
    if (!model) return;
    const current = new Set(model.provider_ids);
    for (const p of catalog.providers) {
      const want = linked.includes(p.id);
      if (want && !current.has(p.id)) await api.linkAIProviderModels(p.id, [model.id]);
      if (!want && current.has(p.id)) await api.unlinkAIProviderModel(p.id, model.id);
    }
  };

  const save = async () => {
    if (busy) return;
    setErr(null);
    const body = collect([...frozen, ...changedFields()]);
    if (!body) return;
    setBusy(true);
    try {
      await api.saveAIModel(body);
      await syncLinks();
      await onSaved(t('ai_model_saved'));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  /**
   * "Match from models.dev": the server looks the id up under the chosen
   * provider's slug — or, when that provider has no slug (a relay), by the
   * model id itself — and fills the fields that are NOT frozen.
   *
   * The form's unsaved edits are deliberately dropped rather than saved first:
   * saving would freeze them (that is what an edit means) and the match would
   * then leave exactly the fields the operator asked it to refresh. The hook is
   * spelled out in the hint next to the button.
   */
  const runMatch = async () => {
    if (busy) return;
    setErr(null);
    if (!modelID) {
      setErr(t('err_bad_model_id'));
      return;
    }
    if (!matchProvider) {
      setErr(t('ai_match_no_provider'));
      return;
    }
    setBusy(true);
    try {
      const r = await api.matchAIModels(matchProvider, [modelID]);
      // The match has no opinion about links, but the provider it matched
      // against is the one the operator just chose for this model; leaving it
      // unlinked would produce a model that no picker can see.
      await api.linkAIProviderModels(matchProvider, [modelID]);
      const parts = [t('ai_match_applied', { n: r.applied.length })];
      // Provenance (§12.5 修订 2026-09-18): with no slug the id is matched
      // document-wide, so when several providers publish it the values are a
      // consensus pick — say which copy won instead of presenting it as fact.
      const source = r.sources?.[modelID];
      if (source && source.candidates > 1) {
        parts.push(t('ai_match_source', { slug: source.slug, n: source.candidates }));
      }
      if (r.unmatched.length > 0) {
        parts.push(t('ai_match_unmatched', { n: r.unmatched.length, ids: r.unmatched.join(', ') }));
      }
      if (r.frozen.length > 0) {
        parts.push(t('ai_import_frozen', { n: r.frozen.length, fields: r.frozen.join(', ') }));
      }
      await onSaved(parts.join(' · '));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  /**
   * Restore auto-match for ONE field: drop it from overridden_fields, persist,
   * then re-run the match so models.dev fills it again. The modal closes on
   * success — the fresh values live in the reloaded catalog, and re-seeding a
   * form from a match response is exactly the kind of half-updated state that
   * makes "did my edit stick?" unanswerable.
   */
  const restore = async (field: string) => {
    if (busy || !model) return;
    setErr(null);
    if (model.provider_ids.length === 0) {
      setErr(t('ai_restore_no_provider'));
      return;
    }
    if (!matchProvider) {
      setErr(t('ai_match_no_provider'));
      return;
    }
    const body = collect([...frozen, ...changedFields()].filter((f) => f !== field));
    if (!body) return;
    setBusy(true);
    try {
      await api.saveAIModel(body);
      await api.matchAIModels(matchProvider, [model.id]);
      await onSaved(t('ai_restore_done', { field: fieldLabel(t, field) }));
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const modalityOptions = (current: string[]) => uniq([...MODALITIES, ...current]);
  const modelMetaPending = !!model && model.source === 'models_dev' && (model.context_window === 0 || model.max_output_tokens === 0);

  return (
    <Modal title={isNew ? t('ai_new_model') : t('ai_edit_model')} onClose={onClose} wide>
      {err && <div className="form-error">{err}</div>}
      {modelMetaPending && (
        <div className="card warn-card">
          <p>{t('ai_model_meta_pending_hint')}</p>
        </div>
      )}
      {model && (
        <p className="hint">
          {t('ai_model_source')}:{' '}
          {model.source === 'models_dev' ? t('ai_model_source_models_dev') : t('ai_model_source_manual')}
        </p>
      )}

      <div className="form-grid">
        <label className="field">
          <span>{t('ai_model_id')}</span>
          <input value={modelID} disabled={!isNew} onChange={(e) => setId(e.target.value)} placeholder="gpt-4o" />
        </label>
        <label className="field">
          <span>
            {t('ai_model_display_name')}
            {frozen.includes('display_name') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
          </span>
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder={modelID} />
        </label>
      </div>
      {isNew && <p className="hint">{t('ai_model_id_hint')}</p>}

      <div className="form-grid">
        <label className="field">
          <span>
            {t('ai_model_context')}
            {frozen.includes('context_window') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
          </span>
          <input type="number" min={0} value={contextWindow} onChange={(e) => setContextWindow(e.target.value)} />
        </label>
        <label className="field">
          <span>
            {t('ai_model_max_output')}
            {frozen.includes('max_output_tokens') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
          </span>
          <input type="number" min={0} value={maxOutput} onChange={(e) => setMaxOutput(e.target.value)} />
        </label>
      </div>
      <p className="hint">{t('ai_model_limits_hint')}</p>

      <h4>
        {t('ai_model_input_modalities')}
        {frozen.includes('input_modalities') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
      </h4>
      <div className="row-wrap">
        {modalityOptions(model?.input_modalities ?? []).map((m) => (
          <label key={m} className="check-chip">
            <input type="checkbox" checked={inputMods.includes(m)} onChange={() => setInputMods((l) => toggleIn(l, m))} />
            {m}
          </label>
        ))}
      </div>

      <h4>
        {t('ai_model_output_modalities')}
        {frozen.includes('output_modalities') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
      </h4>
      <div className="row-wrap">
        {modalityOptions(model?.output_modalities ?? []).map((m) => (
          <label key={m} className="check-chip">
            <input type="checkbox" checked={outputMods.includes(m)} onChange={() => setOutputMods((l) => toggleIn(l, m))} />
            {m}
          </label>
        ))}
      </div>

      <h4>
        {t('ai_model_levels')}
        {frozen.includes('reasoning_levels') && <em className="sensitive-tag set">{t('ai_model_field_frozen')}</em>}
      </h4>
      <p className="hint">{t('ai_model_levels_hint')}</p>
      <div className="row-wrap">
        {REASONING_LEVELS.map((l) => (
          <label key={l} className="check-chip">
            <input type="checkbox" checked={levels.includes(l)} onChange={() => setLevels((prev) => toggleIn(prev, l))} />
            {l}
          </label>
        ))}
      </div>

      {!isNew && (
        <>
          <h4>{t('ai_model_providers')}</h4>
          <p className="hint">{t('ai_model_providers_hint')}</p>
          <div className="row-wrap">
            {catalog.providers.length === 0 && <span className="hint">{t('ai_providers_empty')}</span>}
            {catalog.providers.map((p) => {
              // effective_levels is the per-protocol set the server computed;
              // "" (no level) is stated in words, never an empty dropdown.
              const view = p.models.find((m) => m.id === model?.id);
              return (
                <label key={p.id} className="check-chip">
                  <input
                    type="checkbox"
                    checked={linked.includes(p.id)}
                    onChange={() => setLinked((l) => toggleIn(l, p.id))}
                  />
                  {p.name}
                  {view && (
                    <span className="hint">
                      {view.effective_levels.length === 0
                        ? t('ai_model_levels_empty')
                        : t('ai_effective_levels', { levels: view.effective_levels.join(' / ') })}
                    </span>
                  )}
                </label>
              );
            })}
          </div>

          <h4>{t('ai_restore_field')}</h4>
          <p className="hint">{t('ai_restore_hint')}</p>
          <div className="row-wrap">
            <label className="field inline">
              <span>{t('ai_match_provider')}</span>
              <select value={matchProvider} onChange={(e) => setMatchProvider(e.target.value)}>
                <option value="">{t('ai_default_pick_provider')}</option>
                {catalog.providers.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
            <button type="button" className="btn small" disabled={busy || !matchProvider} onClick={() => void runMatch()}>
              {t('ai_match_now')}
            </button>
          </div>
          <p className="hint">{t('ai_match_discard_hint')}</p>
          {frozen.length === 0 ? null : (
            <div className="row-wrap">
              {frozen.map((f) => (
                <button
                  key={f}
                  type="button"
                  className="btn ghost small"
                  disabled={busy}
                  title={t('ai_restore_hint')}
                  onClick={() => void restore(f)}
                >
                  {fieldLabel(t, f)} · {t('ai_restore_field')}
                </button>
              ))}
            </div>
          )}
        </>
      )}

      {isNew && catalog.providers.length > 0 && (
        <>
          <h4>{t('ai_match_now')}</h4>
          <p className="hint">{t('ai_match_new_hint')}</p>
          <div className="row-wrap">
            <label className="field inline">
              <span>{t('ai_match_provider')}</span>
              <select value={matchProvider} onChange={(e) => setMatchProvider(e.target.value)}>
                <option value="">{t('ai_default_pick_provider')}</option>
                {catalog.providers.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name}
                  </option>
                ))}
              </select>
            </label>
            <button type="button" className="btn small" disabled={busy || !matchProvider || !modelID} onClick={() => void runMatch()}>
              {t('ai_match_now')}
            </button>
          </div>
          <p className="hint">{t('ai_match_discard_hint')}</p>
        </>
      )}

      <div className="row-between" style={{ marginTop: 12 }}>
        <Toggle checked={enabled} onChange={setEnabled} label={enabled ? t('enabled') : t('disabled')} />
        <div className="row-gap">
          <button type="button" className="btn" onClick={onClose}>
            {t('cancel')}
          </button>
          <button type="button" className="btn primary" disabled={busy} onClick={() => void save()}>
            {busy ? '…' : t('save')}
          </button>
        </div>
      </div>
    </Modal>
  );
}

// --- default model -----------------------------------------------------------

/**
 * The default is a (provider, model) PAIR (§12.1): the same model can be served
 * through different gateways, so a bare model id cannot say which one to call.
 * Alongside it sits the default thinking level, which applies to any turn whose
 * picker is left at "unset" — independent of the pair by design, since a
 * hand-picked pair benefits from it just as well. A stored default that no
 * longer resolves (provider deleted, model unlinked or disabled) is REPORTED —
 * the panel never silently clears it, because "your default quietly vanished"
 * is indistinguishable from "it was never set".
 */
function DefaultCard({
  catalog,
  onChanged,
  onError,
}: {
  catalog: AICatalog;
  onChanged: (message?: string) => void | Promise<void>;
  onError: (message: string) => void;
}) {
  const { t } = useI18n();
  const [providerID, setProviderID] = useState(catalog.default_provider_id || '');
  const [modelID, setModelID] = useState(catalog.default_model_id || '');
  const [reasoning, setReasoning] = useState(catalog.default_reasoning || '');
  const [busy, setBusy] = useState(false);

  // Follow the server's stored values whenever they change (save, import, a
  // deleted provider). Deps are the ids themselves, so a local selection that
  // has not been saved yet is not clobbered by an unrelated reload.
  useEffect(() => {
    setProviderID(catalog.default_provider_id || '');
    setModelID(catalog.default_model_id || '');
    setReasoning(catalog.default_reasoning || '');
  }, [catalog.default_provider_id, catalog.default_model_id, catalog.default_reasoning]);

  const provider = catalog.providers.find((p) => p.id === providerID) ?? null;
  const storedProvider = catalog.providers.find((p) => p.id === catalog.default_provider_id) ?? null;
  const storedModel = catalog.models.find((m) => m.id === catalog.default_model_id) ?? null;
  const storedAny = catalog.default_provider_id !== '' || catalog.default_model_id !== '';
  const storedBroken =
    storedAny &&
    (!storedProvider ||
      !storedProvider.enabled ||
      !storedProvider.models.some((m) => m.id === catalog.default_model_id) ||
      !storedModel ||
      !storedModel.enabled);

  const selected = provider?.models.find((m) => m.id === modelID) ?? null;

  // The level list is the SELECTED pair's own set, never the global vocabulary
  // (§12.5: effective_levels = protocol capability ∩ the model's
  // reasoning_options) — a level this provider/model cannot take is not a
  // default anyone can use. `effectiveReasoning` is what the card both shows
  // and persists: a draft level the current pair does not offer must never be
  // displayed (React would silently render the first <option> while the state
  // kept the old value, and the save would write it) nor written back.
  const levels = selected?.effective_levels ?? [];
  const effectiveReasoning = levels.includes(reasoning) ? reasoning : '';

  const save = async () => {
    if (busy) return;
    if (!providerID || !modelID) {
      onError(t('ai_default_pair_required'));
      return;
    }
    setBusy(true);
    try {
      await api.setAIDefaults(providerID, modelID, effectiveReasoning);
      await onChanged(t('ai_default_saved'));
    } catch (e) {
      onError(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const clear = async () => {
    if (busy) return;
    if (!window.confirm(t('ai_default_clear_confirm'))) return;
    setBusy(true);
    try {
      // Everything the card configures travels together: "clear the default"
      // resets the pair AND the thinking level, not a half state the operator
      // has to hunt down field by field.
      await api.setAIDefaults('', '', '');
      await onChanged(t('ai_default_cleared'));
    } catch (e) {
      onError(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="card">
      <h3>{t('ai_sec_default')}</h3>
      <p className="hint">{t('ai_default_hint')}</p>

      {storedAny ? (
        <p className="hint">
          {t('ai_default_current', {
            provider: storedProvider?.name ?? catalog.default_provider_id,
            model: catalog.default_model_id,
          })}
        </p>
      ) : (
        <p className="hint">{t('ai_default_none')}</p>
      )}
      {storedBroken && (
        <div className="card warn-card">
          <p>
            {t('ai_default_missing', {
              provider: storedProvider?.name ?? catalog.default_provider_id,
              model: catalog.default_model_id,
            })}
          </p>
        </div>
      )}

      <div className="form-grid">
        <label className="field">
          <span>{t('ai_default_provider')}</span>
          <select
            value={providerID}
            onChange={(e) => {
              setProviderID(e.target.value);
              // A model belongs to one provider in this picker: keeping the id
              // would submit a pair the server rejects (default_not_usable).
              setModelID('');
            }}
          >
            <option value="">{t('ai_default_pick_provider')}</option>
            {catalog.providers.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
                {p.enabled ? '' : ` · ${t('disabled')}`}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>{t('ai_default_model')}</span>
          <select value={modelID} onChange={(e) => setModelID(e.target.value)} disabled={!provider}>
            <option value="">{t('ai_default_pick_model')}</option>
            {(provider?.models ?? []).map((m) => (
              <option key={m.id} value={m.id}>
                {m.display_name || m.id}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>{t('ai_default_reasoning')}</span>
          {selected && levels.length === 0 ? (
            // Never an empty dropdown (same rule as the model card): the honest
            // statement is that this pair exposes no adjustable level.
            <span className="hint">{t('ai_model_levels_empty')}</span>
          ) : (
            <select
              value={effectiveReasoning}
              disabled={!selected}
              onChange={(e) => setReasoning(e.target.value)}
            >
              <option value="">{t('ai_reasoning_default')}</option>
              {levels.map((l) => (
                <option key={l} value={l}>
                  {l}
                </option>
              ))}
            </select>
          )}
        </label>
      </div>
      {provider && provider.models.length === 0 && <p className="hint">{t('ai_default_no_models')}</p>}
      {effectiveReasoning !== '' && <p className="hint">{t('ai_default_reasoning_hint')}</p>}
      {selected && levels.length > 0 && (
        <p className="hint">{t('ai_effective_levels', { levels: levels.join(' / ') })}</p>
      )}

      <div className="row-end">
        <button type="button" className="btn" disabled={busy || !storedAny} onClick={() => void clear()}>
          {t('ai_default_clear')}
        </button>
        <button type="button" className="btn primary" disabled={busy || !providerID || !modelID} onClick={() => void save()}>
          {busy ? '…' : t('ai_default_save')}
        </button>
      </div>
    </section>
  );
}

// --- models.dev cache --------------------------------------------------------

/** Cache state + the manual refresh (§12.5); "not loaded" explains the fallout. */
function ModelsDevCard({ status, busy, onRefresh }: { status: ModelsDevStatus | null; busy: boolean; onRefresh: () => void }) {
  const { t } = useI18n();
  if (!status) return null;
  return (
    <section className="card">
      <div className="row-between">
        <h3>{t('ai_sec_modelsdev')}</h3>
        <span className={'chip' + (status.loaded ? ' status-ok' : ' status-timeout')}>
          {status.loaded ? t('ai_modelsdev_loaded') : t('ai_modelsdev_not_loaded')}
        </span>
      </div>
      <p className="hint">{t('ai_modelsdev_desc')}</p>
      {!status.loaded && <p className="form-error">{t('ai_modelsdev_unavailable')}</p>}
      {status.last_error && <p className="hint">{t('ai_modelsdev_load_error')}: {codeText(t, status.last_error)}</p>}

      <div className="tile-grid">
        <div className="tile">
          <div className="tile-label">{t('ai_modelsdev_updated')}</div>
          <div className="tile-value">{status.updated_at ? fmtTime(status.updated_at) : '-'}</div>
          <div className="tile-sub">{status.auto_update ? t('geoip_auto_on') : t('geoip_auto_off')}</div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('ai_modelsdev_providers')}</div>
          <div className="tile-value mono">{status.providers}</div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('ai_modelsdev_models')}</div>
          <div className="tile-value mono">{status.models}</div>
        </div>
        <div className="tile">
          <div className="tile-label">{t('ai_modelsdev_url')}</div>
          <div className="tile-value mono">{status.url || '-'}</div>
          <div className="tile-sub">
            {t('ai_modelsdev_slugs')}: {status.slugs.length}
          </div>
        </div>
      </div>

      <div className="row-end">
        <button type="button" className="btn" disabled={busy} onClick={onRefresh}>
          <RefreshIcon />
          {busy ? t('ai_modelsdev_refreshing') : t('ai_modelsdev_refresh')}
        </button>
      </div>
    </section>
  );
}
