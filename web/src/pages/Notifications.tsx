import { useCallback, useEffect, useState } from 'react';
import { QRCodeSVG } from 'qrcode.react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { SaveRow, Toggle, useField, type FieldFn } from '../components/SettingsForm';
import { useI18n } from '../i18n';
import type { FeishuQRStatus, NotifyTestResult, SettingView } from '../types';

/**
 * §15 通知 (2026-09-16 修订: 独立成页). One page owns everything about how
 * alerts leave the panel:
 *
 *  - channels (Telegram / 飞书 / 通用 Webhook) at the same level, each with its
 *    own on/off switch and config, and a test button that sends for real;
 *  - event-type switches, because "notify me about probes but not about every
 *    traffic threshold" is the actual operator question;
 *  - the traffic thresholds the traffic events are judged against.
 *
 * Every switch saves on change (they are single keys, and a switch that needs
 * a separate Save press is a switch people forget to press).
 */

const TELEGRAM_KEYS = ['notify.telegram_bot_token', 'notify.telegram_chat_id'] as const;
const WEBHOOK_KEYS = ['notify.webhook_url', 'notify.webhook_secret'] as const;
const FEISHU_APP_KEYS = ['notify.feishu_app_id', 'notify.feishu_app_secret', 'notify.feishu_receive_id', 'notify.feishu_domain'] as const;
const FEISHU_WEBHOOK_KEYS = ['notify.feishu_webhook_url', 'notify.feishu_webhook_secret'] as const;
const TRAFFIC_KEYS = ['alert.traffic_warn_pct', 'alert.traffic_crit_pct'] as const;
/** §15 copy language + timestamp zone of the pushed text (2026-09-19 修订). */
const TEXT_KEYS = ['notify.language', 'notify.timezone'] as const;

/** Event groups, mirroring notify.EventGroups on the server. */
const EVENT_GROUPS = ['node_status', 'traffic', 'billing', 'singbox', 'updates', 'counter_reset'] as const;

/** Channel switch setting keys, mirroring notify.Key*Enabled. */
const CHANNEL_SWITCH = {
  telegram: 'notify.telegram_enabled',
  feishu: 'notify.feishu_enabled',
  webhook: 'notify.webhook_enabled',
} as const;

type ChannelName = keyof typeof CHANNEL_SWITCH;

export default function Notifications() {
  const { t } = useI18n();
  const [settings, setSettings] = useState<Record<string, SettingView>>({});
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [savedMsg, setSavedMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [switchBusy, setSwitchBusy] = useState<string | null>(null);
  // Settings the master key can no longer open (§15): the channel reads as
  // configured but delivers nothing, so the page has to say so out loud.
  const [brokenSecrets, setBrokenSecrets] = useState<string[]>([]);

  const field = useField({ settings, draft, setDraft });

  const load = useCallback(async () => {
    try {
      const r = await api.getSettings();
      const map: Record<string, SettingView> = {};
      for (const s of r.settings ?? []) map[s.key] = s;
      setSettings(map);
      setDraft({});
      setBrokenSecrets(r.unreadable_secrets ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  const saveGroup = async (keys: readonly string[]) => {
    if (busy) return;
    const payload: Record<string, string> = {};
    for (const k of keys) {
      const v = draft[k];
      if (v !== undefined && v.trim() !== '') payload[k] = v.trim();
    }
    if (Object.keys(payload).length === 0) return;
    setBusy(true);
    setSavedMsg(null);
    try {
      await api.putSettings(payload);
      await load();
      setSavedMsg(t('settings_saved'));
      window.setTimeout(() => setSavedMsg(null), 3000);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  /**
   * A switch is on unless the stored value says otherwise (server-side default
   * is "unset = on"), so an unset switch renders as enabled.
   */
  const isOn = (key: string): boolean => {
    const v = settings[key]?.value;
    if (v === undefined || v === '') return true;
    return !['0', 'false', 'off', 'no'].includes(v.toLowerCase());
  };

  const setSwitch = async (key: string, on: boolean) => {
    setSwitchBusy(key);
    setErr(null);
    try {
      await api.putSettings({ [key]: on ? '1' : '0' });
      await load();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      // Reload so the switch snaps back to the value the server actually has.
      await load();
    } finally {
      setSwitchBusy(null);
    }
  };

  const configured = (key: string) => settings[key]?.set === true;

  return (
    <div className="stack-lg">
      <div className="page-head">
        <h2>{t('notify_title')}</h2>
      </div>
      <p className="hint">{t('notify_page_hint')}</p>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {/* A channel whose secret cannot be decrypted looks "configured" on the
          wire and delivers nothing; the server names the keys (§15). */}
      {brokenSecrets.length > 0 && (
        <div className="card error-card">
          <p>{t('notify_secret_unreadable_banner')}</p>
          <p className="mono">{brokenSecrets.join(', ')}</p>
        </div>
      )}

      <ChannelCard
        name="telegram"
        title="Telegram"
        configured={configured('notify.telegram_bot_token') && configured('notify.telegram_chat_id')}
        enabled={isOn(CHANNEL_SWITCH.telegram)}
        switchBusy={switchBusy === CHANNEL_SWITCH.telegram}
        onToggle={(on) => void setSwitch(CHANNEL_SWITCH.telegram, on)}
        busy={busy}
        savedMsg={savedMsg}
        onSave={() => void saveGroup(TELEGRAM_KEYS)}
        saveLabel={t('save')}
      >
        <div className="form-grid">
          {field('notify.telegram_bot_token', t('tg_token'), { password: true })}
          {field('notify.telegram_chat_id', t('tg_chat_id'), {})}
        </div>
      </ChannelCard>

      <FeishuCard
        settings={settings}
        field={field}
        busy={busy}
        savedMsg={savedMsg}
        enabled={isOn(CHANNEL_SWITCH.feishu)}
        switchBusy={switchBusy === CHANNEL_SWITCH.feishu}
        onToggle={(on) => void setSwitch(CHANNEL_SWITCH.feishu, on)}
        onSaveApp={() => void saveGroup(FEISHU_APP_KEYS)}
        onSaveWebhook={() => void saveGroup(FEISHU_WEBHOOK_KEYS)}
        onChanged={() => void load()}
        saveLabel={t('save')}
      />

      <ChannelCard
        name="webhook"
        title={t('webhook_channel_title')}
        configured={configured('notify.webhook_url')}
        enabled={isOn(CHANNEL_SWITCH.webhook)}
        switchBusy={switchBusy === CHANNEL_SWITCH.webhook}
        onToggle={(on) => void setSwitch(CHANNEL_SWITCH.webhook, on)}
        busy={busy}
        savedMsg={savedMsg}
        onSave={() => void saveGroup(WEBHOOK_KEYS)}
        saveLabel={t('save')}
      >
        <p className="hint">{t('webhook_channel_hint')}</p>
        <div className="form-grid">
          {field('notify.webhook_url', t('webhook_url'), {})}
          {field('notify.webhook_secret', t('webhook_secret'), { password: true })}
        </div>
      </ChannelCard>

      <section className="card">
        <h3>{t('sec_notify_text')}</h3>
        <p className="hint">{t('notify_text_hint')}</p>
        <div className="form-grid">
          {/* Language names stay literal in both dictionaries: 中文 / English are
              self-describing, and a translated language name is the one place
              where a mistranslation is actively confusing. */}
          {field('notify.language', t('notify_language'), {
            select: [
              { value: 'en-US', label: 'English' },
              { value: 'zh-CN', label: '中文' },
            ],
          })}
          {field('notify.timezone', t('notify_timezone'), { defaultValue: 'UTC' })}
        </div>
        <p className="hint">{t('notify_timezone_hint')}</p>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(TEXT_KEYS)} label={t('save')} />
      </section>

      <section className="card">
        <h3>{t('sec_notify_events')}</h3>
        <p className="hint">{t('notify_events_hint')}</p>
        <div className="switch-list">
          {EVENT_GROUPS.map((g) => {
            const key = `notify.event.${g}`;
            return (
              <div key={g} className="switch-row">
                <div>
                  <div className="switch-row-title">{t(`notify_event_${g}` as never)}</div>
                  <div className="hint">{t(`notify_event_${g}_desc` as never)}</div>
                </div>
                <Toggle
                  checked={isOn(key)}
                  disabled={switchBusy === key}
                  onChange={(on) => void setSwitch(key, on)}
                  label={isOn(key) ? t('notify_event_on') : t('notify_event_off')}
                />
              </div>
            );
          })}
        </div>
      </section>

      <section className="card">
        <h3>{t('sec_notify_thresholds')}</h3>
        <p className="hint">{t('notify_thresholds_hint')}</p>
        <div className="form-grid">
          {field('alert.traffic_warn_pct', t('notify_traffic_warn'), { type: 'number', defaultValue: '80' })}
          {field('alert.traffic_crit_pct', t('notify_traffic_crit'), { type: 'number', defaultValue: '100' })}
        </div>
        <SaveRow busy={busy} savedMsg={savedMsg} onSave={() => void saveGroup(TRAFFIC_KEYS)} label={t('save')} />
      </section>
    </div>
  );
}

/**
 * One delivery channel: on/off switch, "configured" state, and whatever config
 * the channel needs. The switch parks a channel without erasing its config, so
 * turning it back on is one click, not a re-entry of credentials.
 */
function ChannelCard({
  name,
  title,
  configured,
  enabled,
  switchBusy,
  onToggle,
  busy,
  savedMsg,
  onSave,
  saveLabel,
  children,
}: {
  name: ChannelName;
  title: string;
  configured: boolean;
  enabled: boolean;
  switchBusy: boolean;
  onToggle: (on: boolean) => void;
  busy: boolean;
  savedMsg: string | null;
  onSave: () => void;
  saveLabel: string;
  children: React.ReactNode;
}) {
  const { t } = useI18n();
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);

  const runTest = async () => {
    setTesting(true);
    setTest(null);
    try {
      setTest(testResult(t, await api.testChannel(name)));
    } catch (e) {
      setTest({ ok: false, text: apiErrorMessage(e, t) });
    } finally {
      setTesting(false);
    }
  };

  return (
    <section className="card">
      <div className="row-between">
        <h3>{title}</h3>
        <div className="row-gap">
          <span className={'chip' + (configured ? ' status-ok' : '')}>{configured ? t('channel_configured') : t('channel_unconfigured')}</span>
          <Toggle checked={enabled} disabled={switchBusy} onChange={onToggle} label={enabled ? t('channel_on') : t('channel_off')} />
        </div>
      </div>
      {!enabled && <p className="hint">{t('channel_parked_hint')}</p>}
      {test && <div className={test.ok ? 'form-ok' : 'form-error'}>{test.text}</div>}
      {children}
      <div className="row-end">
        {savedMsg && <span className="form-ok">{savedMsg}</span>}
        <button type="button" className="btn" disabled={testing} onClick={() => void runTest()}>
          {testing ? '…' : t('feishu_test')}
        </button>
        <button type="button" className="btn primary" disabled={busy} onClick={onSave}>
          {busy ? '…' : saveLabel}
        </button>
      </div>
    </section>
  );
}

/** Shared rendering of the test verdict (ok / not configured / upstream error). */
function testResult(t: ReturnType<typeof useI18n>['t'], r: NotifyTestResult): { ok: boolean; text: string } {
  if (r.ok) return { ok: true, text: t('feishu_test_ok') };
  if (r.code === 'notify_not_configured') return { ok: false, text: t('feishu_test_not_configured') };
  // A stored secret the master key can no longer open is NOT "not configured":
  // saying so would send the operator looking for a missing field that is
  // actually there (§15).
  if (r.code === 'notify_secret_unreadable') return { ok: false, text: t('feishu_test_secret_unreadable') };
  return { ok: false, text: t('feishu_test_fail', { detail: r.detail || r.code || '' }) };
}

/**
 * §15 飞书 (2026-09-16). Three ways in, one channel out:
 *
 *  1. 扫码接入 — a device-flow session runs server-side and the verification
 *     URL is rendered as a QR code; scanning with the 飞书 app provisions a
 *     self-built app in the operator's own tenant and binds the bot to their
 *     open id. Polling is server-driven; this card only reads a status.
 *  2. manual app credentials (an existing self-built app).
 *  3. group custom-bot webhook (no app needed).
 */
function FeishuCard({
  settings,
  field,
  busy,
  savedMsg,
  enabled,
  switchBusy,
  onToggle,
  onSaveApp,
  onSaveWebhook,
  onChanged,
  saveLabel,
}: {
  settings: Record<string, SettingView>;
  field: FieldFn;
  busy: boolean;
  savedMsg: string | null;
  enabled: boolean;
  switchBusy: boolean;
  onToggle: (on: boolean) => void;
  onSaveApp: () => void;
  onSaveWebhook: () => void;
  onChanged: () => void;
  saveLabel: string;
}) {
  const { t } = useI18n();
  const [qr, setQr] = useState<FeishuQRStatus | null>(null);
  const [qrBusy, setQrBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [test, setTest] = useState<{ ok: boolean; text: string } | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const appBound = settings['notify.feishu_app_id']?.set === true;
  const webhookBound = settings['notify.feishu_webhook_url']?.set === true;
  const botName = settings['notify.feishu_bot_name']?.value || '';
  const qrActive = qr !== null && (qr.state === 'qr_ready' || qr.state === 'saving');

  // Poll the session while it is open. The server owns the device flow; this
  // is only a status read, and it stops as soon as the state is terminal.
  useEffect(() => {
    if (!qrActive) return;
    const h = window.setInterval(() => {
      api
        .getFeishuQR()
        .then((st) => {
          setQr(st);
          // A finished binding wrote new settings: refresh so the bound line
          // and the fields stop showing the pre-scan values.
          if (st.state === 'succeeded') onChanged();
        })
        .catch(() => {
          /* transient: the next tick retries */
        });
    }, 2000);
    return () => window.clearInterval(h);
  }, [qrActive, onChanged]);

  const startQR = async () => {
    setQrBusy(true);
    setErr(null);
    setTest(null);
    try {
      setQr(await api.startFeishuQR());
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setQrBusy(false);
    }
  };

  const cancelQR = async () => {
    try {
      setQr(await api.cancelFeishuQR());
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  };

  const runTest = async () => {
    setTesting(true);
    setTest(null);
    try {
      setTest(testResult(t, await api.testChannel('feishu')));
    } catch (e) {
      setTest({ ok: false, text: apiErrorMessage(e, t) });
    } finally {
      setTesting(false);
    }
  };

  const clear = async (mode: 'app' | 'webhook') => {
    const question = mode === 'app' ? t('feishu_clear_app_confirm') : t('feishu_clear_webhook_confirm');
    if (!window.confirm(question)) return;
    setErr(null);
    setTest(null);
    try {
      await api.clearFeishuConfig(mode);
      if (mode === 'app') setQr(null);
      onChanged();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  };

  // Terminal-state copy: succeeded/expired/denied/cancelled/error all read
  // from one state key, with the error code refining the failure case.
  const qrState = qr?.state ?? 'idle';
  const qrMessage = (() => {
    if (!qr || qr.state === 'idle') return null;
    if (qr.state === 'succeeded') return t('feishu_qr_state_succeeded', { bot: qr.bot_name || 'fobe' });
    if (qr.state === 'qr_ready') return t('feishu_qr_state_qr_ready');
    if (qr.state === 'saving') return t('feishu_qr_state_saving');
    if (qr.state === 'error' && qr.error) return t(`feishu_qr_err_${qr.error}` as never);
    return t(`feishu_qr_state_${qr.state}` as never);
  })();

  return (
    <section className="card">
      <div className="row-between">
        <h3>{t('sec_feishu')}</h3>
        <div className="row-gap">
          <span className={'chip' + (appBound || webhookBound ? ' status-ok' : '')}>
            {appBound
              ? t('feishu_app_bound', { bot: botName || 'fobe' })
              : webhookBound
                ? t('feishu_webhook_bound')
                : t('channel_unconfigured')}
          </span>
          <Toggle checked={enabled} disabled={switchBusy} onChange={onToggle} label={enabled ? t('channel_on') : t('channel_off')} />
        </div>
      </div>
      <p className="hint">{t('feishu_desc')}</p>
      {!enabled && <p className="hint">{t('channel_parked_hint')}</p>}
      {err && <div className="form-error">{err}</div>}
      {test && <div className={test.ok ? 'form-ok' : 'form-error'}>{test.text}</div>}

      <div className="row-wrap" style={{ marginTop: 12 }}>
        <button type="button" className="btn primary" disabled={qrBusy || qrActive} onClick={() => void startQR()}>
          {qrBusy ? '…' : appBound || qr ? t('feishu_qr_restart') : t('feishu_qr_start')}
        </button>
        {qrActive && qr?.state === 'qr_ready' && (
          <button type="button" className="btn" onClick={() => void cancelQR()}>
            {t('feishu_qr_cancel')}
          </button>
        )}
        <button type="button" className="btn" disabled={testing} onClick={() => void runTest()}>
          {testing ? '…' : t('feishu_test')}
        </button>
        {appBound && (
          <button type="button" className="btn danger" onClick={() => void clear('app')}>
            {t('feishu_clear_app')}
          </button>
        )}
      </div>

      {qrActive && qr?.qr_url && (
        <div style={{ marginTop: 14, display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 10 }}>
          {/* White plate: the QR must stay scannable in the dark theme too. */}
          <div style={{ background: '#fff', padding: 12, borderRadius: 8, lineHeight: 0 }}>
            <QRCodeSVG value={qr.qr_url} size={168} level="M" />
          </div>
          <div className="hint" style={{ textAlign: 'center' }}>
            {t('feishu_qr_scan_hint')}
          </div>
          <div className="hint">
            {qr.state === 'saving' ? t('feishu_qr_state_saving') : t('feishu_qr_waiting')}
            {qr.state === 'qr_ready' && qr.remaining_seconds !== undefined
              ? ' · ' + t('feishu_qr_remaining', { n: qr.remaining_seconds })
              : ''}
          </div>
        </div>
      )}
      {qrMessage && !qrActive && qrState !== 'idle' && (
        <div className={qrState === 'succeeded' ? 'form-ok' : 'hint'} style={{ marginTop: 10 }}>
          {qrMessage}
        </div>
      )}

      <h4 style={{ marginTop: 16 }}>{t('feishu_manual_hint')}</h4>
      <div className="form-grid">
        {field('notify.feishu_app_id', t('feishu_app_id'), {})}
        {field('notify.feishu_app_secret', t('feishu_app_secret'), { password: true })}
        {field('notify.feishu_receive_id', t('feishu_receive_id'), {})}
        {field('notify.feishu_domain', t('feishu_domain'), {
          select: [
            { value: 'feishu', label: 'feishu.cn' },
            { value: 'lark', label: 'larksuite.com' },
          ],
        })}
      </div>
      <p className="hint">{t('feishu_receive_id_hint')}</p>
      <SaveRow busy={busy} savedMsg={savedMsg} onSave={onSaveApp} label={saveLabel} />

      <div className="row-between" style={{ marginTop: 16 }}>
        <h4>{t('feishu_webhook_title')}</h4>
        {webhookBound && (
          <button type="button" className="btn danger small" onClick={() => void clear('webhook')}>
            {t('feishu_clear_webhook')}
          </button>
        )}
      </div>
      <p className="hint">{t('feishu_webhook_hint')}</p>
      <div className="form-grid">
        {field('notify.feishu_webhook_url', t('webhook_url'), {})}
        {field('notify.feishu_webhook_secret', t('webhook_secret'), { password: true })}
      </div>
      <SaveRow busy={busy} savedMsg={savedMsg} onSave={onSaveWebhook} label={saveLabel} />
    </section>
  );
}
