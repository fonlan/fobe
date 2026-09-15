import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import {
  fmtBytes,
  fmtDuration,
  fmtPct,
  fmtRate,
  fmtTime,
  fmtTimeShort,
  fmtDate,
  datetimeLocalToUnix,
  dateStrToUnix,
  toDatetimeLocal,
  toDateString,
} from '../format';
import type {
  CommandRow,
  LatencySample,
  LatencyTarget,
  MetricsSample,
  NodeDetailData,
  NodeSSHUpdate,
  NodeSSHView,
  SingboxStatus,
  SingboxVersion,
  TrafficResp,
} from '../types';
import Flag from '../components/Flag';
import LineChart, { type ChartPoint, type ChartSeries } from '../components/LineChart';
import Modal from '../components/Modal';
import ProgressBar from '../components/ProgressBar';

const C_CPU = 'var(--accent)';
const C_MEM = 'var(--accent)';
const C_DISK = 'var(--amber)';
const C_RX = 'var(--accent)';
const C_TX = 'var(--green)';
const C_ICMP = 'var(--accent)';
const C_TCP = 'var(--green)';

export default function NodeDetail() {
  const { id = '' } = useParams();
  const { t } = useI18n();
  const navigate = useNavigate();

  const [data, setData] = useState<NodeDetailData | null>(null);
  const [metrics, setMetrics] = useState<MetricsSample[]>([]);
  const [traffic, setTraffic] = useState<TrafficResp | null>(null);
  const [targets, setTargets] = useState<LatencyTarget[]>([]);
  const [err, setErr] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [showSSHCreds, setShowSSHCreds] = useState(false);
  const [ipErr, setIpErr] = useState<string | null>(null);
  const [ipBusy, setIpBusy] = useState(false);

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

  // §16: while the detail page is open, ask the agent for 5s samples.
  // Fire-and-forget both ways: an offline agent just 409s and we ignore it.
  useEffect(() => {
    api.probeMetrics(id, true).catch(() => {});
    return () => {
      api.probeMetrics(id, false).catch(() => {});
    };
  }, [id]);

  // §14 手动主 IP: pin one of the reported addresses; the choice survives the
  // agent's next full report (the store keeps a manual_primary flag).
  const setPrimaryIP = async (ip: string) => {
    if (ipBusy) return;
    setIpBusy(true);
    setIpErr(null);
    try {
      await api.setNodePrimaryIP(id, ip);
      await load();
    } catch (e) {
      setIpErr(apiErrorMessage(e, t));
    } finally {
      setIpBusy(false);
    }
  };

  // metrics + traffic are heavier; load once per node
  useEffect(() => {
    let alive = true;
    const from = Math.floor(Date.now() / 1000) - 7 * 86400;
    api
      .nodeMetrics(id, from)
      .then((r) => alive && setMetrics(r.samples ?? []))
      .catch(() => alive && setMetrics([]));
    api
      .nodeTraffic(id, 31)
      .then((r) => alive && setTraffic(r))
      .catch(() => alive && setTraffic(null));
    return () => {
      alive = false;
    };
  }, [id]);

  // refresh node info on push events
  useEffect(() => {
    const close = api.openEvents((ev) => {
      if (ev.kind.startsWith('node_')) void load();
    });
    return close;
  }, [load]);

  const node = data?.node;

  const cpuSeries = useMemo<ChartSeries[]>(
    () => [{ name: t('cpu'), color: C_CPU, points: metrics.map((m) => ({ x: m.ts, y: m.cpu })) as ChartPoint[] }],
    [metrics, t],
  );
  const memDiskSeries = useMemo<ChartSeries[]>(() => {
    const mem: ChartPoint[] = [];
    const disk: ChartPoint[] = [];
    for (const m of metrics) {
      if (m.mem_total > 0) mem.push({ x: m.ts, y: (m.mem_used / m.mem_total) * 100 });
      if (m.disk_total > 0) disk.push({ x: m.ts, y: (m.disk_used / m.disk_total) * 100 });
    }
    return [
      { name: t('mem_pct'), color: C_MEM, points: mem, area: true },
      { name: t('disk_pct'), color: C_DISK, points: disk },
    ];
  }, [metrics, t]);
  const netSeries = useMemo<ChartSeries[]>(
    () => [
      { name: t('rx'), color: C_RX, points: metrics.map((m) => ({ x: m.ts, y: m.net_rx_rate })), area: true },
      { name: t('tx'), color: C_TX, points: metrics.map((m) => ({ x: m.ts, y: m.net_tx_rate })) },
    ],
    [metrics, t],
  );
  const trafficSeries = useMemo<ChartSeries[]>(() => {
    const daily = traffic?.daily ?? [];
    const pts = daily.map((d) => ({ x: Date.parse(d.date + 'T00:00:00Z') / 1000, y: d.rx_bytes }));
    const tps = daily.map((d) => ({ x: Date.parse(d.date + 'T00:00:00Z') / 1000, y: d.tx_bytes }));
    return [
      { name: t('rx'), color: C_RX, points: pts, area: true },
      { name: t('tx'), color: C_TX, points: tps },
    ];
  }, [traffic, t]);

  if (err) {
    return (
      <div className="card error-card">
        <p>{err}</p>
        <Link className="btn" to="/">
          {t('back_to_overview')}
        </Link>
      </div>
    );
  }
  if (!data || !node) return <div className="loading">{t('loading')}</div>;

  const memPct = node.mem_total > 0 ? (node.mem_used / node.mem_total) * 100 : 0;
  const diskPct = node.disk_total > 0 ? (node.disk_used / node.disk_total) * 100 : 0;
  const last = metrics.length > 0 ? metrics[metrics.length - 1] : null;
  const fmtXTime = (x: number) => fmtTimeShort(x);
  const fmtXTraffic = (x: number) => fmtDate(new Date(x * 1000).toISOString());
  const fmtYPct = (v: number) => `${Math.round(v)}%`;
  const fmtYBytes = (v: number) => fmtBytes(v);

  const doDelete = async () => {
    try {
      await api.deleteNode(id);
      navigate('/');
    } catch (e) {
      setErr(apiErrorMessage(e, t));
      setConfirmDelete(false);
    }
  };

  return (
    <div className="stack-lg">
      <div className="page-head">
        <div className="node-title">
          <Link to="/" className="back-link">
            ← {t('back_to_overview')}
          </Link>
          <h2>
            <Flag cc={node.country_code} />
            {node.name || node.hostname || node.id}
            <span className={'dot ' + (node.online ? 'on' : 'off')} title={t(node.online ? 'online' : 'offline')} />
          </h2>
        </div>
        <div className="row-gap">
          <Link to={`/nodes/${encodeURIComponent(id)}/terminal`} className="btn primary">
            {t('web_ssh')}
          </Link>
          <button type="button" className="btn" onClick={() => setShowSSHCreds(true)}>
            {t('ssh_credentials')}
          </button>
          <button type="button" className="btn danger" onClick={() => setConfirmDelete(true)}>
            {t('delete_node')}
          </button>
        </div>
      </div>

      <section className="card">
        <h3>{t('sec_info')}</h3>
        <div className="tile-grid">
          <Tile label={t('cpu')} value={fmtPct(node.cpu)} />
          <Tile label={t('memory')} value={fmtPct(memPct)} sub={`${fmtBytes(node.mem_used)} / ${fmtBytes(node.mem_total)}`} />
          <Tile label={t('disk')} value={fmtPct(diskPct)} sub={`${fmtBytes(node.disk_used)} / ${fmtBytes(node.disk_total)}`} />
          <Tile label={t('rx')} value={fmtRate(node.net_rx_rate)} />
          <Tile label={t('tx')} value={fmtRate(node.net_tx_rate)} />
          <Tile label={t('hostname')} value={node.hostname || '-'} sub={node.primary_ip} />
          <Tile label={t('os_arch')} value={`${node.os || '?'} / ${node.arch || '?'}`} sub={t('agent_v', { v: node.agent_version || '?' })} />
          <Tile label={t('uptime')} value={last ? fmtDuration(last.uptime) : '-'} sub={t('last_seen', { time: fmtTime(node.last_seen) })} />
        </div>
        {node.quota_bytes != null && (
          <div className="quota-row">
            <ProgressBar
              label={`${t('period_used')} (${node.mode})`}
              pct={node.period_pct}
              text={`${fmtBytes(node.period_used)} / ${fmtBytes(node.quota_bytes ?? 0)}`}
            />
            <span className="hint">{t('period_start')}: {fmtTime(node.period_start)}</span>
          </div>
        )}
      </section>

      <section className="card">
        <h3>{t('sb_card')}</h3>
        <SingboxCard nodeId={id} onChanged={() => void load()} />
      </section>

      <section className="card">
        <h3>{t('sec_charts')}</h3>
        <h4>{t('chart_cpu')}</h4>
        <LineChart series={cpuSeries} yMax={100} fmtY={fmtYPct} fmtX={fmtXTime} emptyText={t('no_chart_data')} />
        <h4>{t('chart_mem_disk')}</h4>
        <LineChart series={memDiskSeries} yMax={100} fmtY={fmtYPct} fmtX={fmtXTime} emptyText={t('no_chart_data')} />
        <h4>{t('chart_net')}</h4>
        <LineChart series={netSeries} fmtY={fmtYBytes} fmtX={fmtXTime} emptyText={t('no_chart_data')} />
      </section>

      <section className="card">
        <div className="row-between">
          <h3>{t('sec_traffic')}</h3>
          {traffic && (
            <span className="hint">
              {t('period_rx')}: <span className="mono">{fmtBytes(traffic.period_rx)}</span> · {t('period_tx')}:{' '}
              <span className="mono">{fmtBytes(traffic.period_tx)}</span>
            </span>
          )}
        </div>
        <LineChart series={trafficSeries} fmtY={fmtYBytes} fmtX={fmtXTraffic} emptyText={t('no_chart_data')} />
      </section>

      <section className="card">
        <h3>{t('sec_latency')}</h3>
        <LatencyPanel nodeId={id} nodeTargets={data.latency_targets ?? []} allTargets={targets} />
      </section>

      <section className="card">
        <h3>{t('sec_ips')}</h3>
        {ipErr && <div className="form-error">{ipErr}</div>}
        {data.ips.length === 0 ? (
          <div className="hint">{t('unknown')}</div>
        ) : (
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
                      <button
                        type="button"
                        className="btn small"
                        disabled={ipBusy}
                        onClick={() => void setPrimaryIP(ip.ip)}
                      >
                        {t('ip_set_primary')}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h3>{t('sec_edit')}</h3>
        <EditForm data={data} allTargets={targets} onSaved={() => void load()} />
      </section>

      <section className="card">
        <h3>{t('sec_commands')}</h3>
        <CommandsPanel nodeId={id} />
      </section>

      {confirmDelete && (
        <Modal title={t('delete_node')} onClose={() => setConfirmDelete(false)}>
          <p>{t('delete_node_confirm', { name: node.name || node.id })}</p>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setConfirmDelete(false)}>
              {t('cancel')}
            </button>
            <button type="button" className="btn danger" onClick={() => void doDelete()}>
              {t('delete')}
            </button>
          </div>
        </Modal>
      )}

      {showSSHCreds && <SSHCredentialsModal nodeId={id} onClose={() => setShowSSHCreds(false)} />}
    </div>
  );
}

function Tile({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value mono">{value}</div>
      {sub && <div className="tile-sub">{sub}</div>}
    </div>
  );
}

// --- latency ----------------------------------------------------------------

function LatencyPanel({
  nodeId,
  nodeTargets,
  allTargets,
}: {
  nodeId: string;
  nodeTargets: LatencyTarget[];
  allTargets: LatencyTarget[];
}) {
  const { t } = useI18n();
  const options = nodeTargets.length > 0 ? nodeTargets : allTargets;
  const [targetId, setTargetId] = useState<number>(options[0]?.id ?? 0);
  const [samples, setSamples] = useState<LatencySample[]>([]);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (targetId === 0 && options.length > 0) setTargetId(options[0].id);
  }, [options, targetId]);

  useEffect(() => {
    if (targetId === 0) return;
    let alive = true;
    setBusy(true);
    api
      .nodeLatency(nodeId, targetId, Math.floor(Date.now() / 1000) - 7 * 86400)
      .then((r) => alive && setSamples(r.samples ?? []))
      .catch(() => alive && setSamples([]))
      .finally(() => alive && setBusy(false));
    return () => {
      alive = false;
    };
  }, [nodeId, targetId]);

  if (options.length === 0) {
    return (
      <p className="hint">
        {t('no_chart_data')} — {t('nav_targets')}: <Link to="/targets">{t('target_new')}</Link>
      </p>
    );
  }

  const icmp: ChartPoint[] = [];
  const tcp: ChartPoint[] = [];
  for (const s of samples) {
    if (typeof s.icmp_ms === 'number' && s.icmp_ms >= 0) icmp.push({ x: s.ts, y: s.icmp_ms });
    if (typeof s.tcp_ms === 'number' && s.tcp_ms >= 0) tcp.push({ x: s.ts, y: s.tcp_ms });
  }
  const series: ChartSeries[] = [];
  if (icmp.length > 0) series.push({ name: 'ICMP', color: C_ICMP, points: icmp });
  if (tcp.length > 0) series.push({ name: 'TCP', color: C_TCP, points: tcp });
  const lossVals = samples.map((s) => s.loss).filter((l) => typeof l === 'number' && l > 0);
  const avgLoss = lossVals.length > 0 ? lossVals.reduce((a, b) => a + b, 0) / lossVals.length : 0;

  return (
    <div className="stack">
      <div className="row-gap">
        <label className="field inline">
          <span>{t('select_target')}</span>
          <select value={targetId} onChange={(e) => setTargetId(Number(e.target.value))}>
            {options.map((tg) => (
              <option key={tg.id} value={tg.id}>
                {tg.name} ({tg.kind} {tg.host}
                {tg.kind === 'tcp' ? ':' + tg.port : ''})
              </option>
            ))}
          </select>
        </label>
        {busy && <span className="hint">{t('loading')}</span>}
        <span className="hint">
          {t('loss')}: <span className="mono">{avgLoss.toFixed(2)}%</span>
        </span>
      </div>
      <LineChart
        series={series}
        fmtY={(v) => `${Math.round(v)}ms`}
        fmtX={fmtTimeShort}
        emptyText={t('no_chart_data')}
        key={targetId}
      />
    </div>
  );
}

// --- edit form ----------------------------------------------------------------

const MODES = ['in', 'out', 'both', 'max'] as const;

function EditForm({
  data,
  allTargets,
  onSaved,
}: {
  data: NodeDetailData;
  allTargets: LatencyTarget[];
  onSaved: () => void;
}) {
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
  const [targetIds, setTargetIds] = useState<number[]>((data.latency_targets ?? []).map((x) => x.id));
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const toggleTarget = (tid: number) => {
    setTargetIds((prev) => (prev.includes(tid) ? prev.filter((x) => x !== tid) : [...prev, tid]));
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    const quotaBytes =
      quotaGb.trim() === '' ? null : Math.round(parseFloat(quotaGb) * 2 ** 30);
    const body: api.UpdateNodeBody = {
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
      latency_target_ids: targetIds,
    };
    try {
      await api.updateNode(node.id, body);
      setMsg(t('settings_saved'));
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

      {allTargets.length > 0 && (
        <>
          <h4>{t('latency_targets')}</h4>
          <div className="row-wrap">
            {allTargets.map((tg) => (
              <label key={tg.id} className="check-chip">
                <input type="checkbox" checked={targetIds.includes(tg.id)} onChange={() => toggleTarget(tg.id)} />
                {tg.name} ({tg.kind})
              </label>
            ))}
          </div>
        </>
      )}

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

// --- sing-box (design §9: install/update/start/stop/restart, port, errors) ---

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

// --- commands -----------------------------------------------------------------

function CommandsPanel({ nodeId }: { nodeId: string }) {
  const { t } = useI18n();
  const [commands, setCommands] = useState<CommandRow[]>([]);
  const [input, setInput] = useState('');
  const [risky, setRisky] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setCommands(await api.listCommandsNormalized(nodeId));
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [nodeId, t]);

  useEffect(() => {
    void load();
  }, [load]);

  const send = async () => {
    const cmd = input.trim();
    if (!cmd || busy) return;
    setBusy(true);
    setErr(null);
    try {
      await api.enqueueCommand(nodeId, 'run_shell', { command: cmd }, risky);
      setInput('');
      window.setTimeout(() => void load(), 1200);
      window.setTimeout(() => void load(), 4000);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const payloadCommand = (payload: string): string => {
    try {
      const j = JSON.parse(payload) as { command?: string };
      if (typeof j.command === 'string') return j.command;
    } catch {
      // not JSON; show raw
    }
    return payload;
  };

  return (
    <div className="stack">
      <div className="cmd-input-row">
        <input
          className="mono"
          value={input}
          placeholder={t('command_placeholder')}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void send();
          }}
        />
        <label className="check-chip">
          <input type="checkbox" checked={risky} onChange={(e) => setRisky(e.target.checked)} />
          {t('command_risky')}
        </label>
        <button type="button" className="btn primary" disabled={busy || input.trim() === ''} onClick={() => void send()}>
          {t('command_send')}
        </button>
      </div>
      {err && <div className="form-error">{err}</div>}

      {commands.length === 0 ? (
        <div className="hint">{t('command_empty')}</div>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>{t('audit_time')}</th>
              <th>{t('cmd_result')}</th>
              <th>{t('status')}</th>
            </tr>
          </thead>
          <tbody>
            {commands.map((c) => (
              <tr key={c.id}>
                <td className="mono nowrap">{fmtTime(c.created_at)}</td>
                <td>
                  <code className="mono cmd-code">{payloadCommand(c.payload)}</code>
                  {c.result && (
                    <details>
                      <summary className="hint">{t('cmd_result')}</summary>
                      <pre className="code-block small">{c.result}</pre>
                    </details>
                  )}
                </td>
                <td>
                  <span className={`chip status-${c.status}`}>{t('cmd_status_' + c.status)}</span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// --- SSH credentials (design §11: server-side injection for SSH terminals) ---

const SSH_CLEAR = '!'; // sentinel the backend understands as "delete stored value"

function SSHCredentialsModal({ nodeId, onClose }: { nodeId: string; onClose: () => void }) {
  const { t } = useI18n();
  const [view, setView] = useState<NodeSSHView | null>(null);
  const [user, setUser] = useState('root');
  const [port, setPort] = useState('22');
  const [password, setPassword] = useState('');
  const [privateKey, setPrivateKey] = useState('');
  const [clearPassword, setClearPassword] = useState(false);
  const [clearKey, setClearKey] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const apply = useCallback((v: NodeSSHView) => {
    setView(v);
    setUser(v.user || 'root');
    setPort(String(v.port || 22));
  }, []);

  useEffect(() => {
    let alive = true;
    api
      .getNodeSSH(nodeId)
      .then((v) => alive && apply(v))
      .catch((e) => alive && setErr(apiErrorMessage(e, t)));
    return () => {
      alive = false;
    };
  }, [nodeId, apply, t]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    setMsg(null);
    const body: NodeSSHUpdate = { user: user.trim(), port: Number(port) || 22 };
    if (clearPassword) body.password = SSH_CLEAR;
    else if (password !== '') body.password = password;
    if (clearKey) body.private_key = SSH_CLEAR;
    else if (privateKey !== '') body.private_key = privateKey;
    try {
      const updated = await api.updateNodeSSH(nodeId, body);
      apply(updated);
      setPassword('');
      setPrivateKey('');
      setClearPassword(false);
      setClearKey(false);
      setMsg(t('ssh_saved_ok'));
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal title={t('ssh_credentials')} onClose={onClose}>
      <p className="hint">{t('ssh_credentials_desc')}</p>
      <form onSubmit={submit} className="stack">
        <div className="form-grid">
          <label className="field">
            <span>{t('ssh_user')}</span>
            <input value={user} onChange={(e) => setUser(e.target.value)} />
          </label>
          <label className="field">
            <span>{t('ssh_port')}</span>
            <input type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
          </label>
        </div>

        <label className="field">
          <span>
            {t('ssh_password')}
            {view?.password_set && <span className="chip status-ok">{t('ssh_set_badge')}</span>}
          </span>
          <input
            type="password"
            value={password}
            placeholder={t('ssh_keep_hint')}
            disabled={clearPassword}
            autoComplete="new-password"
            onChange={(e) => setPassword(e.target.value)}
          />
          {view?.password_set && (
            <label className="check-chip">
              <input type="checkbox" checked={clearPassword} onChange={(e) => setClearPassword(e.target.checked)} />
              {t('ssh_clear_saved')}
            </label>
          )}
        </label>

        <label className="field">
          <span>
            {t('ssh_private_key')}
            {view?.privkey_set && <span className="chip status-ok">{t('ssh_set_badge')}</span>}
          </span>
          <textarea
            rows={5}
            className="mono"
            value={privateKey}
            placeholder={t('ssh_keep_hint')}
            disabled={clearKey}
            onChange={(e) => setPrivateKey(e.target.value)}
          />
          {view?.privkey_set && (
            <label className="check-chip">
              <input type="checkbox" checked={clearKey} onChange={(e) => setClearKey(e.target.checked)} />
              {t('ssh_clear_saved')}
            </label>
          )}
        </label>

        <div className="row-end">
          {msg && <span className="form-ok">{msg}</span>}
          {err && <span className="form-error">{err}</span>}
          <button type="button" className="btn" onClick={onClose}>
            {t('close')}
          </button>
          <button type="submit" className="btn primary" disabled={busy}>
            {busy ? t('loading') : t('save')}
          </button>
        </div>
      </form>
    </Modal>
  );
}
