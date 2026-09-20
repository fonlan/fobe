import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useAsyncAction } from '../components/useAsyncAction';
import { useI18n } from '../i18n';
import {
  copyText,
  datetimeLocalInZoneToUnix,
  dateStrToUnix,
  fmtTime,
  toDateString,
  toDatetimeLocalInZone,
} from '../format';
import type {
  ForwardsStatus,
  ForwardRule,
  LatencyTarget,
  NodeDetailData,
  NodeInterface,
  SingboxConfigPayload,
  SingboxInbound,
  SingboxStatus,
  SingboxVersion,
} from '../types';
import type { UpdateNodeBody } from '../api';
import Flag from '../components/Flag';
import Tile from '../components/Tile';

/**
 * Settings → 服务器 → 编辑: everything configurable about one server in one
 * place — node settings, the latency measurement endpoints it probes, its IP
 * list and the sing-box server config. Monitoring (charts, terminal, metrics)
 * stays on the node detail page (design §16).
 */
export default function EditServer() {
  const { id = '' } = useParams();
  const { t } = useI18n();

  const [data, setData] = useState<NodeDetailData | null>(null);
  const [targets, setTargets] = useState<LatencyTarget[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [formSavedMsg, setFormSavedMsg] = useState<string | null>(null);
  const savedTimer = useRef(0);

  const load = useCallback(async () => {
    try {
      const [d, tg] = await Promise.all([api.getNode(id), api.listLatencyTargets().catch(() => ({ targets: [] }))]);
      setData(d);
      setTargets(tg.targets ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [id, t]);

  // NodeSettingsForm 的 key 绑节点数据：保存成功触发的 load 会改变 key、重挂载
  // 表单，把组件内的成功消息连同输入状态一起清掉（失败路径不触发 load，红字不
  // 受影响）。所以「已保存」由父级持有并显示，跨重挂载存活。
  const onNodeFormSaved = () => {
    setFormSavedMsg(t('server_edit_saved'));
    window.clearTimeout(savedTimer.current);
    savedTimer.current = window.setTimeout(() => setFormSavedMsg(null), 3000);
    void load();
  };

  useEffect(() => {
    void load();
  }, [load]);

  if (err) {
    return (
      <div className="card error-card">
        <p>{err}</p>
        <Link className="btn" to="/settings/servers">
          {t('back_to_servers')}
        </Link>
      </div>
    );
  }
  if (!data) return <div className="loading">{t('loading')}</div>;

  const node = data.node;

  return (
    <div className="stack-lg">
      <div className="page-head">
        <div className="node-title">
          <Link to="/settings/servers" className="back-link">
            ← {t('back_to_servers')}
          </Link>
          <h2>
            <Flag cc={node.country_code} />
            {t('edit_server')}: {node.name || node.hostname || node.id}
            <span className={'dot ' + (node.online ? 'on' : 'off')} title={t(node.online ? 'online' : 'offline')} />
          </h2>
        </div>
      </div>

      <section className="card">
        <h3>
          {t('sec_edit')}
          {formSavedMsg && <span className="form-ok"> · {formSavedMsg}</span>}
        </h3>
        <NodeSettingsForm key={JSON.stringify([data.node, data.network, data.traffic_cycle, data.interfaces])} data={data} onSaved={onNodeFormSaved} />
      </section>

      <section className="card">
        <h3>{t('nav_targets')}</h3>
        <LatencyTargetsForm data={data} allTargets={targets} onSaved={() => void load()} />
      </section>

      <section className="card">
        <h3>{t('sec_ips')}</h3>
        <IPList data={data} onChanged={() => void load()} />
      </section>

      <section className="card">
        <h3>{t('sb_card')}</h3>
        <SingboxCard nodeId={id} onlineNow={data.online_now} onChanged={() => void load()} />
      </section>

      <section className="card">
        <h3>{t('sec_forwards')}</h3>
        <ForwardsCard nodeId={id} onlineNow={data.online_now} interfaces={data.interfaces ?? []} />
      </section>

      <section className="card">
        <h3>{t('sec_agent_update')}</h3>
        <AgentUpdatePanel node={node} onChanged={() => void load()} />
      </section>
    </div>
  );
}

/**
 * §5.5 agent self-update, per node: what the panel knows (current vs target,
 * state, plan, last error) plus the two operator actions — retry (unlock a
 * circuit-broken probe) and, for probes whose binary predates the feature, a
 * fresh reinstall command.
 */
function AgentUpdatePanel({ node, onChanged }: { node: NodeDetailData['node']; onChanged: () => void }) {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [cmd, setCmd] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const run = useAsyncAction(setBusy, setErr, setMsg);

  const behind = node.agent_target_version !== '' && node.agent_target_version !== node.agent_version;
  const stateKey = node.agent_update_state ? `agent_state_${node.agent_update_state}` : '';
  // "unsupported" is a capability verdict, not a failed attempt: attempts stays
  // 0, no artifact was ever downloaded. Rendering it like a failure made the
  // panel accuse a healthy host of being broken (design §5.5 fallback).
  const unsupported = node.agent_update_state === 'unsupported';

  const retry = async () => {
    if (busy) return;
    await run(async () => {
      const r = await api.retryAgentUpdate(node.id);
      setMsg(t('agent_update_retry_done') + (r.pushed ? '' : ' · ' + t('offline')));
      onChanged();
    });
  };

  const reinstall = async () => {
    if (busy) return;
    // Minting this token is a credential operation: running the command on the
    // probe rebinds it and invalidates whatever it had before.
    if (!window.confirm(t('agent_reinstall_confirm'))) return;
    await run(
      async () => {
        const r = await api.agentReinstallCommand(node.id);
        setCmd(r.install_command);
      },
      null,
      (e) => setErr(t('agent_reinstall_failed') + ' · ' + apiErrorMessage(e, t)),
    );
  };

  const copy = async () => {
    if (!cmd) return;
    // copyText, not navigator.clipboard: the panel is usually reached over
    // plain http on a LAN IP, where the async clipboard API does not exist.
    if (await copyText(cmd)) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    }
    // refused: the command is selectable text either way
  };

  return (
    <div className="stack">
      <div className="tile-grid">
        <Tile label={t('agent_update_current')} value={node.agent_version || '-'} />
        <Tile label={t('agent_update_target')} value={node.agent_target_version || '-'} />
        <Tile
          label={t('agent_update_state')}
          value={stateKey && t(stateKey as never) !== stateKey ? t(stateKey as never) : node.agent_update_state || '-'}
          sub={node.agent_update_attempts > 0 ? `${t('agent_update_attempts')}: ${node.agent_update_attempts}` : undefined}
        />
        <Tile
          label={t('agent_update_planned')}
          value={!unsupported && node.agent_update_after ? fmtTime(node.agent_update_after) : '-'}
          sub={node.agent_update_done_at ? `${t('agent_update_done')}: ${fmtTime(node.agent_update_done_at)}` : undefined}
        />
      </div>

      {node.agent_update_error && (
        unsupported ? (
          <p className="hint">
            {t('agent_update_reason')}: <span className="mono">{node.agent_update_error}</span>
          </p>
        ) : (
          <p className="form-error">
            {t('agent_update_error')}: <span className="mono">{node.agent_update_error}</span>
          </p>
        )
      )}

      {/* Always offered, because this command has two jobs: it is the manual
          reinstall for an agent that predates §5.5, and it is the only way back
          in for a probe that lost its config.json (overwritten, wiped, moved) —
          the server keeps nothing but the old secret's hash. In that second case
          the node is offline and its caps are stale, so gating the button on
          caps would hide the one path that still works (§4.2 revision). */}
      <div className="stack">
        {/* 只在探针自己报告"不支持自更新"时保留一句解释；重发凭据的来龙去脉
            不在面板里长篇展开（二次确认弹窗与 README 已覆盖），按钮始终给出。 */}
        {node.agent_caps_seen && !node.agent_self_update && (
          <p className="hint">{t(unsupported ? 'agent_no_supervisor_hint' : 'agent_reinstall_hint')}</p>
        )}
        <div className="row-gap">
          <button type="button" className="btn" disabled={busy} onClick={() => void reinstall()}>
            {t('agent_reinstall_title')}
          </button>
        </div>
        {cmd && (
          <div className="stack">
            <pre className="code-block">{cmd}</pre>
            <div className="row-gap">
              <button type="button" className="btn small" onClick={() => void copy()}>
                {copied ? t('copied') : t('copy')}
              </button>
            </div>
          </div>
        )}
      </div>

      {behind && node.agent_self_update && (
        <div className="row-gap">
          <button type="button" className="btn primary" disabled={busy} onClick={() => void retry()}>
            {t('agent_update_retry')}
          </button>
        </div>
      )}

      {msg && <span className="form-ok">{msg}</span>}
      {err && <span className="form-error">{err}</span>}
    </div>
  );
}

// --- node settings (name / note / network quota / traffic cycle / billing) ---

const MODES = ['in', 'out', 'both', 'max'] as const;

// Billing cycle length shares one column (node_billing.cycle_days) whose unit is
// whatever cycle_type names: "3 months" is stored as 3, not 90.
const CYCLE_LEN_LABEL: Record<string, string> = {
  day: 'cycle_days',
  month: 'cycle_months',
  quarter: 'cycle_quarters',
  year: 'cycle_years',
};
// 91d ≈ a calendar quarter (the app's month unit is a flat 30d, so 3 months would
// read as 90d; either converts to "1 quarter" cleanly).
const CYCLE_UNIT_DAYS: Record<string, number> = { day: 1, month: 30, quarter: 91, year: 365 };

// Switching the unit converts the number so it stays meaningful (30 days -> 1
// month); "none" has no unit, so the raw text is kept as typed.
function convertCycleLen(raw: string, from: string, to: string): string {
  const n = Number(raw);
  const fromDays = CYCLE_UNIT_DAYS[from];
  const toDays = CYCLE_UNIT_DAYS[to];
  if (raw.trim() === '' || !Number.isFinite(n) || n <= 0 || !fromDays || !toDays || fromDays === toDays) {
    return raw;
  }
  return String(Math.max(1, Math.round((n * fromDays) / toDays)));
}

function NodeSettingsForm({ data, onSaved }: { data: NodeDetailData; onSaved: () => void }) {
  const { t } = useI18n();
  const node = data.node;
  const net = data.network;
  const trafficCycle = data.traffic_cycle;
  const interfaces = data.interfaces ?? [];
  const billing = data.billing;

  const [name, setName] = useState(node.name);
  // §10.2 name the clients see in subscriptions; '' = fall back to `name`.
  const [subName, setSubName] = useState(node.sub_name ?? '');
  const [note, setNote] = useState(node.note);
  const [country, setCountry] = useState(node.country_code);
  /**
   * §14: the field is pre-filled with the auto-detected flag, so sending it on
   * every save would pin that value as soon as the operator touches anything
   * else — and a pinned flag stops following the primary IP. Only an edit the
   * operator actually made is submitted ('' = back to auto).
   */
  const [countryTouched, setCountryTouched] = useState(false);
  const [iface, setIface] = useState(net?.iface ?? '');
  const [mode, setMode] = useState<string>(net?.mode ?? 'both');
  const [quotaGb, setQuotaGb] = useState(
    net?.quota_bytes != null ? String(Math.round((net.quota_bytes / 2 ** 30) * 100) / 100) : '',
  );
  const [trafficCycleType, setTrafficCycleType] = useState<'none' | 'month' | 'quarter' | 'year'>(trafficCycle?.cycle_type ?? 'none');
  const [nextReset, setNextReset] = useState(
    trafficCycle?.next_reset_at != null ? toDatetimeLocalInZone(trafficCycle.next_reset_at, node.tz) : '',
  );
  const [cycleType, setCycleType] = useState(billing?.cycle_type || 'none');
  const [billingDays, setBillingDays] = useState(billing?.cycle_days != null ? String(billing.cycle_days) : '');
  const [nextDue, setNextDue] = useState(billing?.next_due_at != null ? toDateString(billing.next_due_at) : '');
  const [billingNote, setBillingNote] = useState(billing?.note ?? '');
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const run = useAsyncAction(setBusy, setErr, setMsg);

  // 「已续费」（design §15 实现修订 2026-09-17）：按当前周期把下次到期日顺延。
  // 滚动始终以原到期日为基准整周期推进（不在上一步的结果上累加），与流量周期
  // 的滚动规则同款 —— 月末/2·29 被钳制后不逐次漂移；原到期日已过或没填时从
  // 今天起算。只改表单里的日期字段，和其他改动一样等「保存节点设置」落库。
  const canRenew = cycleType !== 'none' && Number.isFinite(Number(billingDays)) && Number(billingDays) >= 1;
  const renew = () => {
    if (!canRenew) return;
    const len = Number(billingDays);
    const today = dateStrToUnix(toDateString(Date.now() / 1000)) ?? 0;
    const base = dateStrToUnix(nextDue) ?? today;
    const addCycles = (unix: number, k: number): number => {
      const d = new Date(unix * 1000);
      if (cycleType === 'month') d.setMonth(d.getMonth() + k);
      else if (cycleType === 'quarter') d.setMonth(d.getMonth() + 3 * k);
      else if (cycleType === 'year') d.setFullYear(d.getFullYear() + k);
      else d.setDate(d.getDate() + k);
      return Math.floor(d.getTime() / 1000);
    };
    let k = 1;
    let next = addCycles(base, len * k);
    while (next <= today) next = addCycles(base, len * ++k);
    const nextStr = toDateString(next);
    setNextDue(nextStr);
    setMsg(t('billing_renew_applied', { date: nextStr }));
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    await run(async () => {
      const quotaBytes = quotaGb.trim() === '' ? null : Math.round(parseFloat(quotaGb) * 2 ** 30);
      const body: UpdateNodeBody = {
        name,
        sub_name: subName.trim(),
        note,
        network: {
          iface: iface.trim(),
          mode,
          quota_bytes: quotaBytes != null && isFinite(quotaBytes) ? quotaBytes : null,
        },
        traffic_cycle: {
          cycle_type: trafficCycleType as 'none' | 'month' | 'quarter' | 'year',
          next_reset_at: trafficCycleType === 'none' ? null : datetimeLocalInZoneToUnix(nextReset, node.tz),
        },
        billing: {
          cycle_type: cycleType,
          cycle_days: billingDays.trim() === '' ? null : Number(billingDays),
          next_due_at: nextDue.trim() === '' ? null : dateStrToUnix(nextDue),
          note: billingNote,
        },
      };
      // untouched → leave the flag alone (an absent field keeps auto/pin as is)
      if (countryTouched) body.country_code = country.trim().toUpperCase();
      await api.updateNode(node.id, body);
      onSaved();
    });
  };

  return (
    <form onSubmit={submit} className="stack">
      <div className="form-grid">
        <label className="field">
          <span>{t('edit_name')}</span>
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('edit_sub_name')}</span>
          <input value={subName} onChange={(e) => setSubName(e.target.value)} />
          <small className="hint">{t('edit_sub_name_hint')}</small>
        </label>
        <label className="field">
          <span>{t('country_region')}</span>
          <div className="row-gap">
            <Flag cc={country} />
            <input
              className="mono"
              value={country}
              maxLength={2}
              placeholder="HK"
              onChange={(e) => {
                setCountry(e.target.value.toUpperCase().replace(/[^A-Z]/g, ''));
                setCountryTouched(true);
              }}
            />
          </div>
          <small className="hint">
            {t(node.country_manual ? 'country_manual_hint' : 'country_auto_hint')}
          </small>
        </label>
        <label className="field">
          <span>{t('note')}</span>
          <input value={note} onChange={(e) => setNote(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('tz')}</span>
          <input value={node.tz || 'UTC'} readOnly />
          <small className="hint">{t('tz_agent_readonly')}</small>
        </label>
      </div>

      <h4>{t('traffic_cycle')}</h4>
      <div className="form-grid">
        <label className="field">
          <span>{t('iface')}</span>
          <select value={iface} onChange={(e) => setIface(e.target.value)} disabled={interfaces.length === 0}>
            <option value="">{t('iface_auto')}</option>
            {iface !== '' && !interfaces.some((item) => item.name === iface) && (
              <option value={iface}>{t('iface_unavailable', { iface })}</option>
            )}
            {interfaces.map((item) => (
              <option key={item.name} value={item.name}>
                {item.name}{item.default ? ` (${t('iface_default')})` : ''}
              </option>
            ))}
          </select>
          {interfaces.length === 0 && <small className="hint">{t('iface_waiting')}</small>}
        </label>
        <label className="field">
          <span>{t('mode')}</span>
          <select value={mode} onChange={(e) => setMode(e.target.value)}>
            {MODES.map((m) => (
              <option key={m} value={m}>
                {t('mode_' + m)}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>{t('quota_gb')}</span>
          <input type="number" min="0" step="any" value={quotaGb} onChange={(e) => setQuotaGb(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('traffic_cycle_type')}</span>
          <select value={trafficCycleType} onChange={(e) => setTrafficCycleType(e.target.value as 'none' | 'month' | 'quarter' | 'year')}>
            <option value="none">{t('cycle_none')}</option>
            <option value="month">{t('cycle_month')}</option>
            <option value="quarter">{t('cycle_quarter')}</option>
            <option value="year">{t('cycle_year')}</option>
          </select>
        </label>
        <label className="field">
          <span>{t('next_reset_at')}</span>
          <input
            type="datetime-local"
            step="1"
            value={nextReset}
            disabled={trafficCycleType === 'none'}
            onChange={(e) => setNextReset(e.target.value)}
          />
          <small className="hint">{t('next_reset_hint', { tz: node.tz || 'UTC' })}</small>
        </label>
      </div>

      <h4>{t('billing')}</h4>
      <div className="form-grid">
        <label className="field">
          <span>{t('cycle_type')}</span>
          <select
            value={cycleType}
            onChange={(e) => {
              const next = e.target.value;
              setBillingDays((v) => convertCycleLen(v, cycleType, next));
              setCycleType(next);
            }}
          >
            <option value="none">{t('cycle_none')}</option>
            <option value="month">{t('cycle_month')}</option>
            <option value="quarter">{t('cycle_quarter')}</option>
            <option value="day">{t('cycle_day')}</option>
            <option value="year">{t('cycle_year')}</option>
          </select>
        </label>
        <label className="field">
          <span>{t(CYCLE_LEN_LABEL[cycleType] ?? 'cycle_days')}</span>
          <input type="number" min="1" value={billingDays} onChange={(e) => setBillingDays(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('next_due_at')}</span>
          <input type="date" value={nextDue} onChange={(e) => setNextDue(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('billing_note')}</span>
          {/* 已续费按钮挂在费用输入框右侧（design §15 实现修订 2026-09-17）：
              一键把下次到期日按周期顺延，省掉手算日期。 */}
          <div className="row-gap">
            <input value={billingNote} onChange={(e) => setBillingNote(e.target.value)} />
            <button type="button" className="btn" disabled={!canRenew} title={t('billing_renew_title')} onClick={renew}>
              {t('billing_renewed')}
            </button>
          </div>
        </label>
      </div>

      <div className="row-end">
        {msg && <span className="form-ok">{msg}</span>}
        {err && <span className="form-error">{err}</span>}
        <button type="submit" className="btn primary" disabled={busy}>
          {busy ? t('loading') : t('save_node_settings')}
        </button>
      </div>
    </form>
  );
}

// --- latency measurement endpoints --------------------------------------------

function LatencyTargetsForm({
  data,
  allTargets,
  onSaved,
}: {
  data: NodeDetailData;
  allTargets: LatencyTarget[];
  onSaved: () => void;
}) {
  const { t } = useI18n();
  const [selected, setSelected] = useState<number[]>((data.latency_targets ?? []).map((x) => x.id));
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const run = useAsyncAction(setBusy, setErr, setMsg);

  const toggle = (tid: number) => {
    setSelected((prev) => (prev.includes(tid) ? prev.filter((x) => x !== tid) : [...prev, tid]));
    setMsg(null);
  };

  const submit = async () => {
    if (busy) return;
    await run(async () => {
      await api.updateNode(data.node.id, { latency_target_ids: selected });
      setMsg(t('server_edit_saved'));
      onSaved();
    });
  };

  if (allTargets.length === 0) {
    return (
      <p className="hint">
        {t('no_chart_data')} — {t('nav_targets')}: <Link to="/settings/targets">{t('target_new')}</Link>
      </p>
    );
  }

  return (
    <div className="stack">
      <div className="row-wrap">
        {allTargets.map((tg) => (
          <label key={tg.id} className="check-chip">
            <input type="checkbox" checked={selected.includes(tg.id)} onChange={() => toggle(tg.id)} />
            {tg.name} ({tg.kind})
          </label>
        ))}
      </div>
      <div className="row-end">
        {msg && <span className="form-ok">{msg}</span>}
        {err && <span className="form-error">{err}</span>}
        <button type="button" className="btn primary" disabled={busy} onClick={() => void submit()}>
          {busy ? t('loading') : t('save')}
        </button>
      </div>
    </div>
  );
}

// --- IP list (§14 manual primary IP) -------------------------------------------

function IPList({ data, onChanged }: { data: NodeDetailData; onChanged: () => void }) {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const run = useAsyncAction(setBusy, setErr);

  const setPrimaryIP = async (ip: string) => {
    if (busy) return;
    await run(async () => {
      await api.setNodePrimaryIP(data.node.id, ip);
      onChanged();
    });
  };

  if (data.ips.length === 0) return <div className="hint">{t('unknown')}</div>;

  return (
    <div className="stack">
      {err && <div className="form-error">{err}</div>}
      <table className="table">
        <thead>
          <tr>
            <th>{t('ip_col')}</th>
            <th>{t('family_col')}</th>
            <th>{t('scope_col')}</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {data.ips.map((ip) => (
            <tr key={ip.ip}>
              <td className="mono break-anywhere">{ip.ip}</td>
              <td>{ip.family}</td>
              <td>{ip.scope}</td>
              <td>
                {ip.is_primary ? (
                  <span className="chip primary-chip">{t('primary_badge')}</span>
                ) : (
                  <button type="button" className="btn small" disabled={busy} onClick={() => void setPrimaryIP(ip.ip)}>
                    {t('ip_set_primary')}
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// --- sing-box server config (design §9) ----------------------------------------

function SingboxCard({ nodeId, onlineNow, onChanged }: { nodeId: string; onlineNow: boolean; onChanged: () => void }) {
  const { t } = useI18n();
  const [sb, setSb] = useState<SingboxStatus | null>(null);
  const [versions, setVersions] = useState<SingboxVersion[]>([]);
  const [version, setVersion] = useState('');
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  // §9.3 实现修订 2026-09-17b (editor model): the probe's config.json is the
  // truth. `cfg` is that file as the panel last saw it; `edits` are the
  // operator's uncommitted changes keyed by inbound number.
  const [cfg, setCfg] = useState<SingboxConfigPayload | null>(null);
  const [cfgHash, setCfgHash] = useState('');
  const [edits, setEdits] = useState<Record<number, Partial<SingboxInbound>>>({});
  const [newInbound, setNewInbound] = useState<Partial<SingboxInbound> | null>(null);
  const load = useCallback(async () => {
    try {
      const [st, vs, cv] = await Promise.all([
        api.getNodeSingbox(nodeId).catch(() => ({ singbox: null })),
        api.singboxVersions().catch(() => ({ versions: [] })),
        api.singboxConfig(nodeId).catch(() => null),
      ]);
      setSb(st.singbox);
      setVersions(vs.versions ?? []);
      setVersion((cur) => cur || (vs.versions ?? [])[0]?.version || '');
      setCfg(cv);
      setCfgHash((cv as { hash?: string } | null)?.hash ?? '');
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [nodeId, t]);

  useEffect(() => {
    void load();
  }, [load]);

  const status = sb?.status || 'absent';
  const statusKey = 'sb_status_' + status;
  // Convergence is asynchronous (§9.2): the agent applies the desired state and
  // reports back later, so the snapshot taken right after "install" says
  // "installing" and stays that way in the DOM forever. Without this re-read
  // the card kept showing 安装中 next to "desired state pushed" long after the
  // probe had answered — including after a failed install had already landed
  // as 异常 + 最近错误. Only an in-flight change on a reachable probe can move
  // the tile, so nothing else is polled.
  //
  // An uninstall is the same shape of wait: the desired half is already gone
  // (desired_uninstall) while the reported half (version/cert) is still there
  // until the probe removes the files and answers "absent".
  const local = sb?.local ?? null;
  // §9.3 实现修订 2026-09-17f: a sing-box the panel never installed is not
  // "not installed" — the discovery half reports what the probe already runs
  // (one-sing.sh's own instance). While the managed half is silent, the
  // discovered instance answers the status tile and the installed-hint.
  const localOnly = !!local?.present && (!sb || (!sb.desired_version && !sb.version));
  const statusValue = localOnly
    ? t('sb_status_local')
    : t(statusKey) === statusKey
      ? status
      : t(statusKey);
  // The managed status never learns "stopped": recordSingboxState matches
  // Running=true → running, LastError → degraded, uninstall-confirm → absent —
  // a stopped-but-healthy report hits no case and keeps the old value. The
  // discovery half is re-scanned by the same nudge every start/stop command
  // triggers, so local.running is the freshest answer; status is only the
  // fallback while no discovery report has arrived.
  const running = local ? local.running : status === 'running';
  // Discovery is a snapshot the probe refreshes on its own cadence (it reports
  // when the file changed), so the panel re-reads while a local sing-box is
  // present: an operator who edits config.json by hand sees it appear without
  // reloading the page.
  useEffect(() => {
    if (!local?.present || !onlineNow) return;
    const h = window.setInterval(() => void load(), 30000);
    return () => window.clearInterval(h);
  }, [local?.present, onlineNow, load]);

  const installing = status === 'installing';
  const uninstalling = !!sb?.desired_uninstall && !!(sb?.version || sb?.cert_sha256);
  useEffect(() => {
    if (!installing && !uninstalling) {
      setMsg(null); // the probe answered: the pending hint is stale
      return;
    }
    if (!onlineNow) return; // offline: the state cannot change until reconnect
    const h = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(h);
  }, [installing, uninstalling, onlineNow, load]);

  // A new inbound is visible from the desired-state record before the agent's
  // local config report catches up, and a deleted one lingers in the report
  // until the probe applies and re-reports. Re-read quickly while either is in
  // flight, so statuses flip as soon as the agent answers instead of waiting
  // for the normal discovery refresh.
  const hasInFlightInbound =
    cfg?.inbounds.some((inbound) => inbound.status === 'pending' || inbound.status === 'deleting') ?? false;
  useEffect(() => {
    if (!hasInFlightInbound || !onlineNow) return;
    const h = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(h);
  }, [hasInFlightInbound, onlineNow, load]);

  // Every save is a push the probe answers later: it runs its own apply window
  // (§9.2 闸门②/③, up to 30s) and only then re-scans the file, and the table
  // renders what the probe reported. A rename or a credential change leaves no
  // in-flight row behind (the port is unchanged), so without this watch the
  // page would keep showing the pre-edit values until the 30s discovery tick —
  // which reads as "my edit did not save". Watch at the fast cadence for a
  // minute after any action of ours, then fall back to the normal refresh.
  const [watchUntil, setWatchUntil] = useState(0);
  useEffect(() => {
    if (!watchUntil || !onlineNow) return;
    const h = window.setInterval(() => {
      if (Date.now() >= watchUntil) {
        window.clearInterval(h);
        setWatchUntil(0);
        return;
      }
      void load();
    }, 5000);
    return () => window.clearInterval(h);
  }, [watchUntil, onlineNow, load]);

  const action = useAsyncAction(setBusy, setErr, setMsg);
  const run = async (fn: () => Promise<unknown>, okMsg: string) => {
    if (busy) return;
    await action(async () => {
      await fn();
      setMsg(okMsg);
      setWatchUntil(Date.now() + 60000);
      await load();
      onChanged();
    });
  };

  return (
    <div className="stack">
      <div className="tile-grid">
        <Tile label={t('sb_current')} value={sb?.version || '-'} />
        <Tile label={t('sb_desired')} value={sb?.desired_version || '-'} />
        <Tile label={t('alert_status')} value={statusValue} />
      </div>

      {sb?.last_error && (
        <p className="form-error">
          {t('sb_last_error')}: <span className="mono">{sb.last_error}</span>
        </p>
      )}
      {installing && !onlineNow && <span className="hint">{t('sb_installing_offline')}</span>}
      {uninstalling && <span className="hint">{onlineNow ? t('sb_uninstalling') : t('sb_uninstalling_offline')}</span>}
      {sb?.cert_not_after ? (
        <span className="hint">{t('sb_cert_until', { time: fmtTime(sb.cert_not_after) })}</span>
      ) : null}

      {/* §9.3 实现修订 2026-09-17b, 2026-09-17d: the probe's own config.json,
          editable. Nothing here is "adopted" and nothing is selected: the file
          is the source of truth, every inbound in it is already part of the
          node and of the subscription, and an edit is merged back onto it. */}
      {local?.present && (
        <div className="sb-local">
          <div className="row-wrap">
            <strong>{t('sb_local_title')}</strong>
            <span className="hint">
              {t('sb_local_state', {
                version: local.version || '-',
                state: local.running
                  ? t('sb_local_running')
                  : local.unit_known
                    ? t('sb_local_stopped')
                    : t('sb_local_unknown'),
              })}
            </span>
            <button
              type="button"
              className="btn"
              disabled={busy || !onlineNow}
              onClick={() =>
                void run(async () => {
                  await api.singboxRefresh(nodeId);
                }, t('sb_refresh_queued'))
              }
            >
              {t('sb_refresh')}
            </button>
          </div>
          {local.config_path && <span className="hint mono">{local.config_path}</span>}
          {local.error && <span className="hint">{t('sb_local_error')}: {local.error}</span>}

          {!cfg?.reported ? (
            <span className="hint">{t('sb_local_no_config')}</span>
          ) : (
            <div className="table-wrap">
              <table className="sb-inbound-table">
                <thead>
                  <tr>
                    <th>{t('sb_col_type')}</th>
                    <th>{t('sb_col_port')}</th>
                    <th>{t('sb_col_status')}</th>
                    <th>{t('sb_col_tag')}</th>
                    <th>{t('sb_col_cred')}</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {cfg.inbounds.map((ib) => {
                    const edit = { ...ib, ...edits[ib.number] };
                    const dirty = !!edits[ib.number];
                    return (
                      <tr key={`${ib.number}:${ib.port}`}>
                        <td className="mono">{ib.type}</td>
                        <td>
                          <input
                            className="mono sb-num"
                            type="number"
                            min={10000}
                            max={60000}
                            value={edit.port ?? ''}
                            disabled={!ib.editable}
                            onChange={(e) =>
                              setEdits((cur) => ({
                                ...cur,
                                [ib.number]: { ...cur[ib.number], port: Number(e.target.value) },
                              }))
                            }
                          />
                        </td>
                        {/* 状态 chip 是 CJK 短词:不加 nowrap 会被 auto 表格布局
                            压到单字宽折成两行,超出部分由 .table-wrap 横向滚动接住 */}
                        <td className="nowrap">
                          <span className="chip">
                            {ib.status === 'running'
                              ? t('sb_inbound_running')
                              : ib.status === 'deleting'
                                ? t('sb_inbound_deleting')
                                : t('sb_inbound_pending')}
                          </span>
                        </td>
                        <td>
                          <input
                            className="mono sb-text"
                            value={edit.tag ?? ''}
                            disabled={!ib.editable}
                            onChange={(e) =>
                              setEdits((cur) => ({
                                ...cur,
                                [ib.number]: { ...cur[ib.number], tag: e.target.value },
                              }))
                            }
                          />
                        </td>
                        <td>
                          <input
                            className="mono sb-text"
                            type="text"
                            value={edit.credential ?? ''}
                            disabled={!ib.editable}
                            placeholder={t('sb_cred_keep')}
                            onChange={(e) =>
                              setEdits((cur) => ({
                                ...cur,
                                [ib.number]: { ...cur[ib.number], credential: e.target.value },
                              }))
                            }
                          />
                        </td>
                        <td>
                          <div className="row-actions">
                            {dirty && (
                              <button
                                type="button"
                                className="btn primary"
                                disabled={busy}
                                title={t('sb_save_inbound')}
                                onClick={() =>
                                  void run(async () => {
                                    await api.singboxConfigEdit(nodeId, {
                                      reported_hash: cfgHash,
                                      update: [
                                        {
                                          number: ib.number,
                                          type: edit.type,
                                          tag: edit.tag,
                                          port: Number(edit.port),
                                          credential: edit.credential || undefined,
                                          server_name: edit.server_name,
                                          flow: edit.flow,
                                          username: edit.username,
                                        },
                                      ],
                                    });
                                    setEdits((cur) => {
                                      const next = { ...cur };
                                      delete next[ib.number];
                                      return next;
                                    });
                                  }, t('sb_inbound_saved'))
                                }
                              >
                                {t('save')}
                              </button>
                            )}
                            {ib.editable && (
                              <button
                                type="button"
                                className="btn danger"
                                disabled={busy || cfg.inbounds.length <= 1}
                                title={t('sb_inbound_delete')}
                                onClick={() => {
                                  if (!window.confirm(t('sb_inbound_delete_confirm', { port: String(ib.port) })))
                                    return;
                                  void run(
                                    () =>
                                      api.singboxConfigEdit(nodeId, {
                                        reported_hash: cfgHash,
                                        delete: [ib.number],
                                      }),
                                    t('sb_inbound_deleted'),
                                  );
                                }}
                              >
                                {t('delete')}
                              </button>
                            )}
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          <div className="row-wrap">
            <button
              type="button"
              className="btn"
              disabled={busy}
              onClick={() => setNewInbound({ type: 'anytls', port: 0, tag: '', credential: '' })}
            >
              {t('sb_inbound_add')}
            </button>
          </div>

          {newInbound && (
            <div className="row-wrap">
              <label className="field inline">
                <span>{t('sb_col_type')}</span>
                <select
                  value={newInbound.type}
                  onChange={(e) => setNewInbound({ ...newInbound, type: e.target.value })}
                >
                  {['anytls', 'vless', 'shadowsocks', 'socks'].map((ty) => (
                    <option key={ty} value={ty}>
                      {ty}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field inline">
                <span>{t('sb_col_port')}</span>
                <input
                  className="mono sb-num"
                  type="number"
                  min={10000}
                  max={60000}
                  value={newInbound.port || ''}
                  onChange={(e) => setNewInbound({ ...newInbound, port: Number(e.target.value) })}
                />
              </label>
              <label className="field inline">
                <span>{t('sb_col_cred')}</span>
                <input
                  className="mono"
                  value={newInbound.credential ?? ''}
                  placeholder={
                    newInbound.type === 'vless'
                      ? t('sb_cred_uuid')
                      : newInbound.type === 'shadowsocks'
                        ? t('sb_cred_ss')
                        : t('sb_cred_password')
                  }
                  onChange={(e) => setNewInbound({ ...newInbound, credential: e.target.value })}
                />
              </label>
              {newInbound.type === 'vless' && (
                <label className="field inline">
                  <span>SNI</span>
                  <input
                    className="mono"
                    value={newInbound.server_name ?? ''}
                    onChange={(e) => setNewInbound({ ...newInbound, server_name: e.target.value })}
                  />
                </label>
              )}
              {newInbound.type === 'shadowsocks' && (
                <label className="field inline">
                  <span>method</span>
                  <input
                    className="mono"
                    value={newInbound.method ?? '2022-blake3-aes-128-gcm'}
                    onChange={(e) => setNewInbound({ ...newInbound, method: e.target.value })}
                  />
                </label>
              )}
              {newInbound.type === 'socks' && (
                <>
                  <label className="field inline">
                    <span>{t('sb_cred_username')}</span>
                    <input
                      className="mono"
                      value={newInbound.username ?? ''}
                      onChange={(e) => setNewInbound({ ...newInbound, username: e.target.value })}
                    />
                  </label>
                </>
              )}
              <button
                type="button"
                className="btn primary"
                disabled={busy || !newInbound.port}
                onClick={() =>
                  void run(async () => {
                    await api.singboxConfigEdit(nodeId, {
                      reported_hash: cfgHash,
                      add: [{ ...newInbound, new: true }],
                    });
                    setNewInbound(null);
                  }, t('sb_inbound_created'))
                }
              >
                {t('sb_inbound_add_create')}
              </button>
              <button type="button" className="btn" onClick={() => setNewInbound(null)}>
                {t('cancel')}
              </button>
            </div>
          )}
        </div>
      )}

      <div className="row-wrap">
        <label className="field inline">
          <span>{t('sb_version')}</span>
          {versions.length > 0 ? (
            <select value={version} onChange={(e) => setVersion(e.target.value)}>
              {versions.map((v) => (
                <option key={v.version} value={v.version}>
                  {v.version}
                </option>
              ))}
            </select>
          ) : (
            <span className="hint">{t('sb_no_versions')}</span>
          )}
        </label>
        <button
          type="button"
          className="btn primary"
          disabled={busy || versions.length === 0 || version === ''}
          onClick={() => void run(() => api.singboxInstall(nodeId, version), t('sb_desired_pushed'))}
        >
          {t('sb_install')}
        </button>
        {/* One toggle, not two buttons: the action follows the observed state
            (running → stop, else start), so there is no disabled twin to click
            by mistake. */}
        <button
          type="button"
          className="btn"
          disabled={busy || !sb || !sb.desired_version}
          onClick={() =>
            void run(() => api.singboxAction(nodeId, running ? 'stop' : 'start'), t('sb_action_queued'))
          }
        >
          {running ? t('sb_stop') : t('sb_start')}
        </button>
        <button
          type="button"
          className="btn"
          disabled={busy || !sb || !sb.desired_version}
          onClick={() => void run(() => api.singboxAction(nodeId, 'restart'), t('sb_action_queued'))}
        >
          {t('sb_restart')}
        </button>
        {/* Only offered while there is something to remove: the flag survives
            the wait, so a second click while a removal is pending is a no-op
            re-push rather than an error. Confirmation is mandatory — the
            action deletes the binary, the config, the certificate and the
            service unit on the probe. */}
        <button
          type="button"
          className="btn danger"
          disabled={busy || !sb || (!sb.desired_version && !sb.version && !sb.cert_sha256)}
          onClick={() => {
            if (!window.confirm(t('sb_uninstall_confirm', { version: sb?.version || '-' }))) return;
            void run(() => api.singboxUninstall(nodeId), t('sb_uninstall_queued'));
          }}
        >
          {t('sb_uninstall')}
        </button>
      </div>

      {!localOnly && (!sb || (!sb.desired_version && !sb.version)) && <div className="hint">{t('sb_not_installed')}</div>}

      {msg && <span className="form-ok">{msg}</span>}
      {err && <span className="form-error">{err}</span>}
    </div>
  );
}

// --- nftables port forwarding (design §21) ------------------------------------

/** IPv4 only: the whole layout lives in nftables' `table ip` (nfpf.sh too). */
const IPV4_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

function validPort(raw: string): boolean {
  if (!/^\d+$/.test(raw.trim())) return false;
  const n = Number(raw);
  return n >= 1 && n <= 65535;
}

function validIPv4(raw: string): boolean {
  if (!IPV4_RE.test(raw)) return false;
  return raw.split('.').every((part) => Number(part) <= 255);
}

/**
 * Port forwarding lives on the edit page because it is server configuration,
 * not monitoring. The probe's ruleset is the source of truth: the list is the
 * last report (so it renders instantly, and while the probe is offline), and
 * every edit is a one-shot command whose answer replaces it. Rules added
 * outside the panel — nfpf.sh, hand-written nft — are the same rows here.
 */
function ForwardsCard({
  nodeId,
  onlineNow,
  interfaces,
}: {
  nodeId: string;
  onlineNow: boolean;
  interfaces: NodeInterface[];
}) {
  const { t } = useI18n();
  const [status, setStatus] = useState<ForwardsStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [editing, setEditing] = useState<ForwardRule | null>(null);
  // reported_at captured when a command was left queued: while it stays put the
  // probe has not answered yet, so keep re-reading the (cheap) snapshot.
  const [awaiting, setAwaiting] = useState<number | null>(null);

  const [proto, setProto] = useState('tcp');
  const [srcPort, setSrcPort] = useState('');
  const [iface, setIface] = useState('');
  const [dstIP, setDstIP] = useState('');
  const [dstPort, setDstPort] = useState('');
  const [comment, setComment] = useState('');

  const load = useCallback(
    async (live = false) => {
      try {
        const st = await api.nodeForwards(nodeId, live);
        setStatus(st);
        setAwaiting((prev) => (prev !== null && st.reported_at > prev ? null : prev));
        setErr(null);
      } catch (e) {
        setErr(apiErrorMessage(e, t));
      }
    },
    [nodeId, t],
  );

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (awaiting === null) return;
    const h = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(h);
  }, [awaiting, load]);

  const resetForm = () => {
    setEditing(null);
    setProto('tcp');
    setSrcPort('');
    setIface('');
    setDstIP('');
    setDstPort('');
    setComment('');
  };

  const startEdit = (rule: ForwardRule) => {
    setEditing(rule);
    setProto(rule.proto);
    setSrcPort(String(rule.src_port));
    setIface(rule.iface);
    setDstIP(rule.dst_ip);
    setDstPort(String(rule.dst_port));
    setComment(rule.comment);
    setMsg(null);
    setErr(null);
  };

  const action = useAsyncAction(setBusy, setErr, setMsg);

  const apply = async (fn: () => Promise<ForwardsStatus>, okMsg: string) => {
    if (busy) return;
    await action(async () => {
      const st = await fn();
      setStatus(st);
      if (st.queued) {
        setAwaiting(st.reported_at);
        setMsg(t('fw_queued'));
      } else {
        setMsg(okMsg);
      }
      resetForm();
    });
  };

  const refresh = async () => {
    if (busy) return;
    await action(async () => {
      const st = await api.nodeForwards(nodeId, true);
      setStatus(st);
      if (st.queued) {
        setAwaiting(st.reported_at);
        setMsg(t('fw_queued'));
      } else {
        setMsg(t('fw_refreshed'));
      }
    });
  };

  // nft string literals have no escapes: a quote in the comment is not a
  // validation nicety, it is unwritable (the server refuses it too).
  const formValid =
    (proto === 'tcp' || proto === 'udp') &&
    validPort(srcPort) &&
    validPort(dstPort) &&
    validIPv4(dstIP.trim()) &&
    !comment.includes('"');

  const submit = () => {
    if (!formValid) return;
    const rule = {
      proto,
      src_port: Number(srcPort),
      iface,
      dst_ip: dstIP.trim(),
      dst_port: Number(dstPort),
      comment: comment.trim(),
    };
    if (editing) {
      const old = {
        proto: editing.proto,
        src_port: editing.src_port,
        iface: editing.iface,
        dst_ip: editing.dst_ip,
        dst_port: editing.dst_port,
        comment: editing.comment,
        handle: editing.handle,
      };
      void apply(() => api.updateNodeForward(nodeId, old, rule), t('fw_saved'));
    } else {
      void apply(() => api.addNodeForward(nodeId, rule), t('fw_saved'));
    }
  };

  const remove = (rule: ForwardRule) => {
    const desc = `${rule.proto}/${rule.src_port} -> ${rule.dst_ip}:${rule.dst_port}`;
    if (!window.confirm(t('fw_delete_confirm', { rule: desc }))) return;
    void apply(
      () =>
        api.deleteNodeForward(nodeId, {
          proto: rule.proto,
          src_port: rule.src_port,
          iface: rule.iface,
          dst_ip: rule.dst_ip,
          dst_port: rule.dst_port,
          comment: rule.comment,
          handle: rule.handle,
        }),
      t('fw_saved'),
    );
  };

  const rows = status?.forwards ?? [];
  const ifaceOptions = interfaces.map((i) => i.name);
  // A rule may name an interface the probe no longer reports (unplugged NIC):
  // keep it selectable so editing another field cannot silently drop it.
  if (iface !== '' && !ifaceOptions.includes(iface)) ifaceOptions.push(iface);

  const stateHint = (() => {
    if (!status) return null;
    if (status.reported_at === 0) {
      return <p className="hint">{status.agent_supported ? t('fw_never_reported') : t('fw_agent_old')}</p>;
    }
    if (status.code !== '') {
      const key = 'fw_state_' + status.code;
      const text = t(key, { message: status.message });
      if (text !== key) return <p className="form-error">{text}</p>;
      return (
        <p className="form-error">
          <span className="mono">{status.code}</span>
          {status.message ? ': ' + status.message : ''}
        </p>
      );
    }
    if (!status.supported) return null; // no code and unsupported: nothing to say
    if (!status.initialized) return <p className="hint">{t('fw_first_add_hint')}</p>;
    if (rows.length === 0) return <p className="hint">{t('fw_empty')}</p>;
    return null;
  })();

  return (
    <div className="stack">
      <p className="hint">{t('fw_desc')}</p>
      {stateHint}

      {rows.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t('fw_col_proto')}</th>
                <th>{t('fw_col_src')}</th>
                <th>{t('fw_col_iface')}</th>
                <th>{t('fw_col_dst')}</th>
                <th>{t('fw_col_note')}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.proto}/${r.src_port}/${r.iface}/${r.handle}`}>
                  {/* 短字段一律 nowrap:表格宽度超出时由 .table-wrap 横向滚动接住,
                      而不是把每个字段折行 —— 折行会让每条规则都变成两三行高。 */}
                  <td className="nowrap">{r.proto.toUpperCase()}</td>
                  <td className="mono nowrap">{r.src_port || t('fw_any')}</td>
                  <td className="mono nowrap">{r.iface || t('fw_all_ifaces')}</td>
                  <td className="mono nowrap">
                    {r.dst_ip}:{r.dst_port}
                    {r.extra_match && (
                      <span className="chip chip-muted" title={t('fw_tip_extra_match')}>
                        {t('fw_extra_match')}
                      </span>
                    )}
                  </td>
                  {/* 备注是唯一的自由文本:单行 + 省略号,全文在 title 上。 */}
                  <td className="cell-ellipsis" title={r.comment || undefined}>
                    {r.comment || <span className="hint">{t('none')}</span>}
                  </td>
                  <td>
                    <div className="row-actions">
                      <button
                        type="button"
                        className="btn small"
                        disabled={busy || r.extra_match}
                        title={r.extra_match ? t('fw_tip_extra_match') : undefined}
                        onClick={() => startEdit(r)}
                      >
                        {t('fw_edit')}
                      </button>
                      <button type="button" className="btn small danger" disabled={busy} onClick={() => remove(r)}>
                        {t('delete')}
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="row-wrap">
        <label className="field inline">
          <span>{t('fw_col_proto')}</span>
          <select value={proto} onChange={(e) => setProto(e.target.value)}>
            <option value="tcp">TCP</option>
            <option value="udp">UDP</option>
          </select>
        </label>
        <label className="field inline">
          <span>{t('fw_src_port')}</span>
          <input
            className="mono"
            type="number"
            min={1}
            max={65535}
            value={srcPort}
            placeholder="8080"
            onChange={(e) => setSrcPort(e.target.value)}
          />
        </label>
        <label className="field inline">
          <span>{t('fw_col_iface')}</span>
          <select value={iface} onChange={(e) => setIface(e.target.value)}>
            <option value="">{t('fw_all_ifaces')}</option>
            {ifaceOptions.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <label className="field inline">
          <span>{t('fw_dst_ip')}</span>
          <input className="mono" value={dstIP} placeholder="10.0.0.1" onChange={(e) => setDstIP(e.target.value)} />
        </label>
        <label className="field inline">
          <span>{t('fw_dst_port')}</span>
          <input
            className="mono"
            type="number"
            min={1}
            max={65535}
            value={dstPort}
            placeholder="80"
            onChange={(e) => setDstPort(e.target.value)}
          />
        </label>
        <label className="field inline">
          <span>{t('fw_col_note')}</span>
          <input
            value={comment}
            maxLength={128}
            placeholder={t('fw_comment_ph')}
            title={t('fw_comment_hint')}
            onChange={(e) => setComment(e.target.value)}
          />
        </label>
        <button type="button" className="btn primary" disabled={busy || !formValid} onClick={() => void submit()}>
          {editing ? t('fw_save') : t('fw_add')}
        </button>
        {editing && (
          <button type="button" className="btn" disabled={busy} onClick={() => resetForm()}>
            {t('fw_cancel_edit')}
          </button>
        )}
        <button type="button" className="btn" disabled={busy} onClick={() => void refresh()}>
          {t('fw_refresh')}
        </button>
      </div>

      {status && status.reported_at > 0 && status.supported && (
        <span className="hint">
          {t('fw_reported_at', { time: fmtTime(status.reported_at) })}
          {!onlineNow ? ' · ' + t('offline') : ''}
        </span>
      )}
      {(status?.warnings ?? []).map((w) => {
        // The agent reports advisory codes; the ones the panel knows get a real
        // explanation (the rest stay as the raw token, prefix and all).
        const key = 'fw_warning_' + w.split(':')[0].trim();
        const text = t(key);
        return (
          <span key={w} className={text === key ? 'hint' : 'form-error'}>
            {text === key ? (
              <>
                {t('fw_warn')}: <span className="mono">{w}</span>
              </>
            ) : (
              text
            )}
          </span>
        );
      })}
      {msg && <span className="form-ok">{msg}</span>}
      {err && <span className="form-error">{err}</span>}
    </div>
  );
}
