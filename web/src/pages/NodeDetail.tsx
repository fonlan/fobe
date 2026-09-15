import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { fmtBytes, fmtDuration, fmtPct, fmtRate, fmtTime, fmtTimeShort, fmtDate } from '../format';
import type { CommandRow, LatencySample, LatencyTarget, MetricsSample, NodeDetailData, TrafficResp } from '../types';
import Flag from '../components/Flag';
import LineChart, { type ChartPoint, type ChartSeries } from '../components/LineChart';
import ProgressBar from '../components/ProgressBar';
import Tile from '../components/Tile';

const C_CPU = 'var(--accent)';
const C_MEM = 'var(--accent)';
const C_DISK = 'var(--amber)';
const C_RX = 'var(--accent)';
const C_TX = 'var(--green)';
const C_ICMP = 'var(--accent)';
const C_TCP = 'var(--green)';

/**
 * Node detail = monitoring only: live tiles, charts, traffic, latency history
 * and commands. Everything configurable (node settings, latency endpoints, IP
 * list, sing-box server) lives in Settings → 服务器 → 编辑 (design §16).
 */
export default function NodeDetail() {
  const { id = '' } = useParams();
  const { t } = useI18n();

  const [data, setData] = useState<NodeDetailData | null>(null);
  const [metrics, setMetrics] = useState<MetricsSample[]>([]);
  const [traffic, setTraffic] = useState<TrafficResp | null>(null);
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

  // §16: while the detail page is open, ask the agent for 5s samples.
  // Fire-and-forget both ways: an offline agent just 409s and we ignore it.
  // A hidden tab hands the agent back to its 60s cadence instead of holding
  // the high-frequency stream open forever.
  useEffect(() => {
    const setProbe = (enabled: boolean) => {
      api.probeMetrics(id, enabled).catch(() => {});
    };
    const onVisibility = () => setProbe(!document.hidden);
    setProbe(!document.hidden);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      document.removeEventListener('visibilitychange', onVisibility);
      setProbe(false);
    };
  }, [id]);

  // Live refresh: the probe toggle above puts the agent on a 5s metrics
  // cadence, but no push event fires for plain metric reports — without this
  // poll the tiles froze at whatever was on screen at mount. Poll matches the
  // agent cadence; charts refresh slower (heavier queries). Hidden tabs skip
  // both (browsers throttle them anyway) and catch up on the next visible tick.
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void load();
    }, 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  // metrics + traffic are heavier; poll on a slower cadence than the tiles.
  useEffect(() => {
    let alive = true;
    const pull = () => {
      const from = Math.floor(Date.now() / 1000) - 7 * 86400;
      api
        .nodeMetrics(id, from)
        .then((r) => alive && setMetrics(r.samples ?? []))
        .catch(() => alive && setMetrics([]));
      api
        .nodeTraffic(id, 31)
        .then((r) => alive && setTraffic(r))
        .catch(() => alive && setTraffic(null));
    };
    pull();
    const timer = window.setInterval(() => {
      if (!document.hidden) pull();
    }, 30000);
    return () => {
      alive = false;
      window.clearInterval(timer);
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
            {t('web_terminal')}
          </Link>
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
        <h3>{t('sec_commands')}</h3>
        <CommandsPanel nodeId={id} />
      </section>
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
    const pull = () => {
      api
        .nodeLatency(nodeId, targetId, Math.floor(Date.now() / 1000) - 7 * 86400)
        .then((r) => alive && setSamples(r.samples ?? []))
        .catch(() => alive && setSamples([]))
        .finally(() => alive && setBusy(false));
    };
    pull();
    const timer = window.setInterval(() => {
      if (!document.hidden) pull();
    }, 30000);
    return () => {
      alive = false;
      window.clearInterval(timer);
    };
  }, [nodeId, targetId]);

  if (options.length === 0) {
    return (
      <p className="hint">
        {t('no_chart_data')} — {t('nav_targets')}: <Link to="/settings/targets">{t('target_new')}</Link>
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
