// Self-hosted SVG stacked bar chart (daily traffic: one bar per day, rx/tx
// stacked bottom-up). Mirrors LineChart's approach: a stretched viewBox for
// the geometry plus an HTML overlay for axis labels and the hover tooltip so
// text is never distorted by the non-uniform scaling.

import { useMemo, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react';
import { niceTicks } from './LineChart';

const W = 640;
const H = 220;
const PAD_L = 10;
const PAD_R = 12;
const PAD_T = 12;
const PAD_B = 4;
// Bar width cap in viewBox units so a sparse month doesn't turn into slabs.
const MAX_BAR_W = 26;

export interface BarStack {
  name: string;
  color: string;
  /** One value per bar; stacked bottom-up in array order. */
  values: number[];
}

interface HoverInfo {
  index: number;
  leftPct: number;
  barLeftPct: number;
  barWidthPct: number;
}

interface Props {
  /** One label per bar ('MM-DD'); doubles as the tooltip title. */
  barLabels: string[];
  stacks: BarStack[];
  height?: number;
  fmtY?: (v: number) => string;
  emptyText?: string;
  /** Legend tooltip; i18n is the caller's job, same as emptyText. */
  legendHint?: string;
  /** Overlay shown while every stack is toggled off. */
  allHiddenText?: string;
}

export default function BarChart({
  barLabels,
  stacks,
  height = 220,
  fmtY = (v) => String(Math.round(v)),
  emptyText = '-',
  legendHint,
  allHiddenText,
}: Props) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<HoverInfo | null>(null);
  // Stacks switched off by clicking their legend entry (see LineChart: keyed by
  // name, per-instance, survives the polling refresh).
  const [hidden, setHidden] = useState<ReadonlySet<string>>(() => new Set());

  const visible = useMemo(() => stacks.filter((s) => !hidden.has(s.name)), [stacks, hidden]);
  const allHidden = stacks.length > 0 && visible.length === 0;

  const toggle = (name: string) =>
    setHidden((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });

  const n = barLabels.length;

  const geom = useMemo(() => {
    const plotW = W - PAD_L - PAD_R;
    const plotH = H - PAD_T - PAD_B;
    // The y scale follows the visible stacks (unchecking "tx" re-scales to the
    // remaining "rx"); with everything off the bar geometry is empty anyway, so
    // scale off the full set to keep the grid — and thus the legend — on screen.
    const scaleStacks = allHidden ? stacks : visible;
    let top = 0;
    for (let i = 0; i < n; i++) {
      let sum = 0;
      for (const s of scaleStacks) sum += s.values[i] ?? 0;
      if (sum > top) top = sum;
    }
    const ticks = niceTicks(top);
    const yTop = ticks[ticks.length - 1] || 1;
    const slot = plotW / Math.max(1, n);
    const barW = Math.min(slot * 0.62, MAX_BAR_W);
    const sy = (y: number) => PAD_T + (1 - Math.min(y, yTop) / yTop) * plotH;
    const cx = (i: number) => PAD_L + slot * (i + 0.5);
    const bars: { key: string; x: number; y: number; w: number; h: number; color: string }[] = [];
    for (let i = 0; i < n; i++) {
      let cum = 0;
      for (const s of visible) {
        const v = s.values[i] ?? 0;
        const base = cum;
        cum += v;
        if (v <= 0) continue;
        // 1 viewBox unit floor keeps trace values visible as a sliver.
        bars.push({
          key: `${s.name}-${i}`,
          x: cx(i) - barW / 2,
          y: sy(cum),
          w: barW,
          h: Math.max(1, sy(base) - sy(cum)),
          color: s.color,
        });
      }
    }
    return { ticks, barW, sy, cx, bars };
  }, [n, stacks, visible, allHidden]);

  if (n === 0) return <div className="chart-empty">{emptyText}</div>;

  const onMove = (e: ReactMouseEvent<HTMLDivElement>) => {
    // Nothing plotted → no hover: the tooltip would be a bare label with no rows.
    if (visible.length === 0) return;
    const rect = wrapRef.current?.getBoundingClientRect();
    if (!rect || rect.width === 0) return;
    const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    const idx = Math.min(n - 1, Math.max(0, Math.floor(((frac * W - PAD_L) / (W - PAD_L - PAD_R)) * n)));
    setHover({
      index: idx,
      leftPct: (geom.cx(idx) / W) * 100,
      barLeftPct: ((geom.cx(idx) - geom.barW / 2) / W) * 100,
      barWidthPct: (geom.barW / W) * 100,
    });
  };

  const tickSel: { i: number; align: 'left' | 'center' | 'right' }[] = [];
  if (n === 1) tickSel.push({ i: 0, align: 'center' });
  else if (n === 2) tickSel.push({ i: 0, align: 'left' }, { i: 1, align: 'right' });
  else tickSel.push({ i: 0, align: 'left' }, { i: Math.floor((n - 1) / 2), align: 'center' }, { i: n - 1, align: 'right' });

  return (
    <div className="chart">
      <div className="chart-legend">
        {stacks.map((s) => {
          const off = hidden.has(s.name);
          return (
            <button
              key={s.name}
              type="button"
              className={`chart-legend-item${off ? ' off' : ''}`}
              aria-pressed={!off}
              title={legendHint}
              onClick={() => toggle(s.name)}
            >
              <span
                className="chart-swatch"
                style={off ? { background: 'transparent', boxShadow: `inset 0 0 0 2px ${s.color}` } : { background: s.color }}
              />
              {s.name}
            </button>
          );
        })}
      </div>
      <div
        ref={wrapRef}
        className="chart-plot"
        style={{ height }}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" width="100%" height="100%">
          {geom.ticks.map((tv) => (
            <line
              key={tv}
              x1={PAD_L}
              x2={W - PAD_R}
              y1={geom.sy(tv)}
              y2={geom.sy(tv)}
              className="chart-grid"
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {geom.bars.map((b) => (
            <rect key={b.key} x={b.x} y={b.y} width={b.w} height={b.h} fill={b.color} />
          ))}
        </svg>

        {allHidden && allHiddenText && <div className="chart-hidden-hint">{allHiddenText}</div>}

        {/* y labels as HTML so text is not stretched by the non-uniform viewBox */}
        {geom.ticks.map((tv) => (
          <span
            key={'yl' + tv}
            className="chart-label chart-label-y mono"
            style={{ top: `${((geom.sy(tv) - 6) / H) * 100}%` }}
          >
            {fmtY(tv)}
          </span>
        ))}

        {hover && (
          <>
            <span
              className="chart-bar-hl"
              style={{ left: `${hover.barLeftPct}%`, width: `${hover.barWidthPct}%` }}
            />
            <div className="chart-tip" style={{ left: `${hover.leftPct}%` }}>
              <div className="chart-tip-label mono">{barLabels[hover.index]}</div>
              {visible.map((s) => (
                <div key={s.name} className="chart-tip-row">
                  <span className="chart-swatch" style={{ background: s.color }} />
                  <span>{s.name}</span>
                  <span className="mono">{fmtY(s.values[hover.index] ?? 0)}</span>
                </div>
              ))}
            </div>
          </>
        )}
      </div>
      <div className="chart-xaxis">
        {tickSel.map(({ i, align }) => (
          <span
            key={'xl' + i}
            className={`chart-label chart-label-x mono${align === 'left' ? ' left' : align === 'right' ? ' right' : ''}`}
            style={{ left: `${(geom.cx(i) / W) * 100}%` }}
          >
            {barLabels[i]}
          </span>
        ))}
      </div>
    </div>
  );
}
