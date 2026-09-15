import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { datetimeLocalToUnix, dateStrToUnix, fmtTime, toDateString, toDatetimeLocal } from '../format';
import type { LatencyTarget, NodeDetailData, SingboxStatus, SingboxVersion } from '../types';
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
        <NodeSettingsForm data={data} onSaved={() => void load()} />
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
        <SingboxCard nodeId={id} onChanged={() => void load()} />
      </section>
    </div>
  );
}

// --- node settings (name / note / network quota / billing) -------------------

const MODES = ['in', 'out', 'both', 'max'] as const;

function NodeSettingsForm({ data, onSaved }: { data: NodeDetailData; onSaved: () => void }) {
  const { t } = useI18n();
  const node = data.node;
  const net = data.network;
  const billing = data.billing;

  const [name, setName] = useState(node.name);
  const [note, setNote] = useState(node.note);
  const [iface, setIface] = useState(net?.iface ?? '');
  const [mode, setMode] = useState<string>(net?.mode ?? 'both');
  const [quotaGb, setQuotaGb] = useState(
    net?.quota_bytes != null ? String(Math.round((net.quota_bytes / 2 ** 30) * 100) / 100) : '',
  );
  const [cycleDays, setCycleDays] = useState(net?.cycle_days != null ? String(net.cycle_days) : '30');
  const [anchor, setAnchor] = useState(net?.anchor_at != null ? toDatetimeLocal(net.anchor_at) : '');
  const [tz, setTz] = useState(net?.tz ?? '');
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
        note,
        network: {
          iface: iface.trim(),
          mode,
          quota_bytes: quotaBytes != null && isFinite(quotaBytes) ? quotaBytes : null,
          cycle_days: cycleDays.trim() === '' ? null : Number(cycleDays),
          anchor_at: anchor.trim() === '' ? null : datetimeLocalToUnix(anchor),
          tz: tz.trim(),
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
          <span>{t('note')}</span>
          <input value={note} onChange={(e) => setNote(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('iface')}</span>
          <input value={iface} placeholder="eth0" onChange={(e) => setIface(e.target.value)} />
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
          <span>{t('cycle_days')}</span>
          <input type="number" min="1" value={cycleDays} onChange={(e) => setCycleDays(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('anchor_at')}</span>
          <input type="datetime-local" value={anchor} onChange={(e) => setAnchor(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('tz')}</span>
          <input value={tz} placeholder="Asia/Shanghai" onChange={(e) => setTz(e.target.value)} />
        </label>
      </div>

      <h4>{t('billing')}</h4>
      <div className="form-grid">
        <label className="field">
          <span>{t('cycle_type')}</span>
          <select value={cycleType} onChange={(e) => setCycleType(e.target.value)}>
            <option value="none">{t('cycle_none')}</option>
            <option value="month">{t('cycle_month')}</option>
            <option value="day">{t('cycle_day')}</option>
          </select>
        </label>
        <label className="field">
          <span>{t('cycle_days')}</span>
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
              <td className="mono">{ip.ip}</td>
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

function SingboxCard({ nodeId, onChanged }: { nodeId: string; onChanged: () => void }) {
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

  const status = sb?.status || 'absent';
  const statusKey = 'sb_status_' + status;

  return (
    <div className="stack">
      <div className="tile-grid">
        <Tile label={t('sb_current')} value={sb?.version || '-'} />
        <Tile label={t('sb_desired')} value={sb?.desired_version || '-'} />
        <Tile label={t('sb_port')} value={sb?.port ? ':' + sb.port : '-'} />
        <Tile
          label={t('alert_status')}
          value={t(statusKey) === statusKey ? status : t(statusKey)}
          sub={sb?.rollback_version ? t('sb_rollback', { v: sb.rollback_version }) : undefined}
        />
      </div>

      {sb?.last_error && (
        <p className="form-error">
          {t('sb_last_error')}: <span className="mono">{sb.last_error}</span>
        </p>
      )}
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
      </div>

      {!sb && <div className="hint">{t('sb_not_installed')}</div>}

      <div className="cmd-input-row">
        <input
          className="mono"
          type="number"
          min={10000}
          max={60000}
          value={port}
          placeholder={t('sb_port')}
          onChange={(e) => setPort(e.target.value)}
        />
        <button
          type="button"
          className="btn"
          disabled={busy || !/^\d{4,5}$/.test(port.trim()) || Number(port) < 10000 || Number(port) > 60000}
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
