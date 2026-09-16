import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
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
  SingboxStatus,
  SingboxVersion,
} from '../types';
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
        <h3>{t('sec_edit')}</h3>
        <NodeSettingsForm key={JSON.stringify([data.node, data.network, data.traffic_cycle, data.interfaces])} data={data} onSaved={() => void load()} />
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

  const behind = node.agent_target_version !== '' && node.agent_target_version !== node.agent_version;
  const stateKey = node.agent_update_state ? `agent_state_${node.agent_update_state}` : '';
  // "unsupported" is a capability verdict, not a failed attempt: attempts stays
  // 0, no artifact was ever downloaded. Rendering it like a failure made the
  // panel accuse a healthy host of being broken (design §5.5 fallback).
  const unsupported = node.agent_update_state === 'unsupported';

  const retry = async () => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      const r = await api.retryAgentUpdate(node.id);
      setMsg(t('agent_update_retry_done') + (r.pushed ? '' : ' · ' + t('offline')));
      onChanged();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const reinstall = async () => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await api.agentReinstallCommand(node.id);
      setCmd(r.install_command);
    } catch (e) {
      setErr(t('agent_reinstall_failed') + ' · ' + apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
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

      {/* Only an agent that has actually reported caps can be judged: a node
          that never said hello is not evidence of an outdated binary. */}
      {node.agent_caps_seen && !node.agent_self_update && (
        <div className="stack">
          <p className="hint">{t(unsupported ? 'agent_no_supervisor_hint' : 'agent_reinstall_hint')}</p>
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
      )}

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
  year: 'cycle_years',
};
const CYCLE_UNIT_DAYS: Record<string, number> = { day: 1, month: 30, year: 365 };

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
  const [iface, setIface] = useState(net?.iface ?? '');
  const [mode, setMode] = useState<string>(net?.mode ?? 'both');
  const [quotaGb, setQuotaGb] = useState(
    net?.quota_bytes != null ? String(Math.round((net.quota_bytes / 2 ** 30) * 100) / 100) : '',
  );
  const [trafficCycleType, setTrafficCycleType] = useState<'none' | 'month' | 'year'>(trafficCycle?.cycle_type ?? 'none');
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

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    const quotaBytes = quotaGb.trim() === '' ? null : Math.round(parseFloat(quotaGb) * 2 ** 30);
    try {
      await api.updateNode(node.id, {
        name,
        sub_name: subName.trim(),
        note,
        country_code: country.trim().toUpperCase(),
        network: {
          iface: iface.trim(),
          mode,
          quota_bytes: quotaBytes != null && isFinite(quotaBytes) ? quotaBytes : null,
        },
        traffic_cycle: {
          cycle_type: trafficCycleType as 'none' | 'month' | 'year',
          next_reset_at: trafficCycleType === 'none' ? null : datetimeLocalInZoneToUnix(nextReset, node.tz),
        },
        billing: {
          cycle_type: cycleType,
          cycle_days: billingDays.trim() === '' ? null : Number(billingDays),
          next_due_at: nextDue.trim() === '' ? null : dateStrToUnix(nextDue),
          note: billingNote,
        },
      });
      setMsg(t('server_edit_saved'));
      onSaved();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
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
              onChange={(e) => setCountry(e.target.value.toUpperCase().replace(/[^A-Z]/g, ''))}
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
          <span>{t('tz')}</span>
          <input value={node.tz || 'UTC'} readOnly />
          <small className="hint">{t('tz_agent_readonly')}</small>
        </label>
        <label className="field">
          <span>{t('traffic_cycle_type')}</span>
          <select value={trafficCycleType} onChange={(e) => setTrafficCycleType(e.target.value as 'none' | 'month' | 'year')}>
            <option value="none">{t('cycle_none')}</option>
            <option value="month">{t('cycle_month')}</option>
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
          <input value={billingNote} onChange={(e) => setBillingNote(e.target.value)} />
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

  const toggle = (tid: number) => {
    setSelected((prev) => (prev.includes(tid) ? prev.filter((x) => x !== tid) : [...prev, tid]));
    setMsg(null);
  };

  const submit = async () => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      await api.updateNode(data.node.id, { latency_target_ids: selected });
      setMsg(t('server_edit_saved'));
      onSaved();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
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

  const setPrimaryIP = async (ip: string) => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      await api.setNodePrimaryIP(data.node.id, ip);
      onChanged();
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
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
  const [port, setPort] = useState('');
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const [st, vs] = await Promise.all([
        api.getNodeSingbox(nodeId).catch(() => ({ singbox: null })),
        api.singboxVersions().catch(() => ({ versions: [] })),
      ]);
      setSb(st.singbox);
      setVersions(vs.versions ?? []);
      setVersion((cur) => cur || (vs.versions ?? [])[0]?.version || '');
      setPort((cur) => cur || (st.singbox?.port ? String(st.singbox.port) : ''));
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

  const run = async (fn: () => Promise<unknown>, okMsg: string) => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      await fn();
      setMsg(okMsg);
      await load();
      onChanged();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      <div className="tile-grid">
        <Tile label={t('sb_current')} value={sb?.version || '-'} />
        <Tile label={t('sb_desired')} value={sb?.desired_version || '-'} />
        <Tile label={t('sb_port')} value={sb?.desired_version && sb?.port ? ':' + sb.port : '-'} />
        <Tile label={t('alert_status')} value={t(statusKey) === statusKey ? status : t(statusKey)} />
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
        <button
          type="button"
          className="btn"
          disabled={busy || !sb || !sb.desired_version}
          onClick={() => void run(() => api.singboxAction(nodeId, 'start'), t('sb_action_queued'))}
        >
          {t('sb_start')}
        </button>
        <button
          type="button"
          className="btn"
          disabled={busy || !sb || !sb.desired_version}
          onClick={() => void run(() => api.singboxAction(nodeId, 'stop'), t('sb_action_queued'))}
        >
          {t('sb_stop')}
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

      {(!sb || (!sb.desired_version && !sb.version)) && <div className="hint">{t('sb_not_installed')}</div>}

      <div className="cmd-input-row">
        <input
          className="mono"
          type="number"
          min={10000}
          max={60000}
          value={port}
          placeholder={t('sb_port')}
          disabled={!sb?.desired_version}
          onChange={(e) => setPort(e.target.value)}
        />
        <button
          type="button"
          className="btn"
          disabled={
            busy ||
            !sb?.desired_version ||
            !/^\d{4,5}$/.test(port.trim()) ||
            Number(port) < 10000 ||
            Number(port) > 60000
          }
          onClick={() => void run(() => api.singboxSetPort(nodeId, Number(port)), t('sb_desired_pushed'))}
        >
          {t('sb_change_port')}
        </button>
      </div>

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

  const apply = async (fn: () => Promise<ForwardsStatus>, okMsg: string) => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      const st = await fn();
      setStatus(st);
      if (st.queued) {
        setAwaiting(st.reported_at);
        setMsg(t('fw_queued'));
      } else {
        setMsg(okMsg);
      }
      resetForm();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const refresh = async () => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    try {
      const st = await api.nodeForwards(nodeId, true);
      setStatus(st);
      if (st.queued) {
        setAwaiting(st.reported_at);
        setMsg(t('fw_queued'));
      } else {
        setMsg(t('fw_refreshed'));
      }
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
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
                  <td>{r.proto.toUpperCase()}</td>
                  <td className="mono">{r.src_port || t('fw_any')}</td>
                  <td className="mono">{r.iface || t('fw_all_ifaces')}</td>
                  <td className="mono break-anywhere">
                    {r.dst_ip}:{r.dst_port}
                    {r.extra_match && (
                      <span className="chip chip-muted" title={t('fw_tip_extra_match')}>
                        {t('fw_extra_match')}
                      </span>
                    )}
                  </td>
                  <td className="break-anywhere">
                    {r.comment || <span className="hint">{t('none')}</span>}
                  </td>
                  <td>
                    <div className="row-gap">
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
