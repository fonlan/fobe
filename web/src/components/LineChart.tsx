// Self-hosted SVG line chart (no chart library). Supports downsampling for
// large series and an HTML overlay for axis labels + hover tooltip so text is
// never distorted by the stretched viewBox.

import { useMemo, useRef, useState, type MouseEvent as ReactMouseEvent } from 'react';

export interface ChartPoint {
  x: number;
  y: number;
}

export interface ChartSeries {
  name: string;
  color: string;
  points: ChartPoint[];
  area?: boolean;
}

const W = 640;
const H = 220;
const PAD_L = 10;
const PAD_R = 12;
const PAD_T = 12;
const PAD_B = 8;
const MAX_POINTS = 300;

/** Bucket-average downsampling: keeps the shape, bounds the point count. */
export function downsample(points: ChartPoint[], maxPoints: number = MAX_POINTS): ChartPoint[] {
  if (points.length <= maxPoints) return points;
  const out: ChartPoint[] = [];
  const bucket = points.length / maxPoints;
  for (let i = 0; i < maxPoints; i++) {
    const start = Math.floor(i * bucket);
    const end = Math.max(start + 1, Math.min(points.length, Math.floor((i + 1) * bucket)));
    let sx = 0;
    let sy = 0;
    let n = 0;
    for (let j = start; j < end; j++) {
      sx += points[j].x;
      sy += points[j].y;
      n++;
    }
    if (n > 0) out.push({ x: sx / n, y: sy / n });
  }
  const last = points[points.length - 1];
  if (out.length > 0 && out[out.length - 1].x !== last.x) out[out.length - 1] = last;
  return out;
}

function niceTicks(max: number, count = 4): number[] {
  if (max <= 0 || !isFinite(max)) return [0, 1];
  const raw = max / count;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const norm = raw / mag;
  const step = (norm >= 5 ? 10 : norm >= 2 ? 5 : 2) * mag;
  const ticks: number[] = [];
  for (let v = 0; v <= max + step * 0.001; v += step) ticks.push(v);
  return ticks.length > 1 ? ticks : [0, max];
}

interface HoverInfo {
  leftPct: number;
  label: string;
  rows: { name: string; color: string; text: string }[];
}

interface Props {
  series: ChartSeries[];
  height?: number;
  yMax?: number;
  fmtY?: (v: number) => string;
  fmtX?: (x: number) => string;
  emptyText?: string;
}

export default function LineChart({
  series,
  height = 220,
  yMax,
  fmtY = (v) => String(Math.round(v * 10) / 10),
  fmtX = (x) => String(Math.round(x)),
  emptyText = '-',
}: Props) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<HoverInfo | null>(null);

  const prepared = useMemo(
    () => series.map((s) => ({ ...s, points: downsample(s.points) })),
    [series],
  );

  const extent = useMemo(() => {
    let xMin = Infinity;
    let xMax = -Infinity;
    let yTop = yMax ?? 0;
    for (const s of prepared) {
      for (const p of s.points) {
        if (p.x < xMin) xMin = p.x;
        if (p.x > xMax) xMax = p.x;
        if (yMax === undefined && p.y > yTop) yTop = p.y;
      }
    }
    if (!isFinite(xMin) || !isFinite(xMax)) return null;
    if (xMax <= xMin) xMax = xMin + 1;
    if (yTop <= 0) yTop = 1;
    return { xMin, xMax, yTop };
  }, [prepared, yMax]);

  const ticks = useMemo(() => (extent ? niceTicks(extent.yTop) : []), [extent]);

  if (!extent || prepared.every((s) => s.points.length === 0)) {
    return <div className="chart-empty">{emptyText}</div>;
  }

  const yTopVal = ticks[ticks.length - 1];
  const plotW = W - PAD_L - PAD_R;
  const plotH = H - PAD_T - PAD_B;
  const sx = (x: number) => PAD_L + ((x - extent.xMin) / (extent.xMax - extent.xMin)) * plotW;
  const sy = (y: number) => PAD_T + (1 - Math.min(y, yTopVal) / yTopVal) * plotH;

  const pathFor = (pts: ChartPoint[]): string =>
    pts.map((p, i) => `${i === 0 ? 'M' : 'L'}${sx(p.x).toFixed(2)},${sy(p.y).toFixed(2)}`).join(' ');

  const areaFor = (pts: ChartPoint[]): string => {
    if (pts.length < 2) return '';
    const line = pathFor(pts);
    const first = pts[0];
    const last = pts[pts.length - 1];
    return `${line} L${sx(last.x).toFixed(2)},${(H - PAD_B).toFixed(2)} L${sx(first.x).toFixed(2)},${(H - PAD_B).toFixed(2)} Z`;
  };

  const onMove = (e: ReactMouseEvent<HTMLDivElement>) => {
    const rect = wrapRef.current?.getBoundingClientRect();
    if (!rect || rect.width === 0) return;
    const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    const xVal = extent.xMin + frac * (extent.xMax - extent.xMin);
    const rows: HoverInfo['rows'] = [];
    let bestX: number | null = null;
    for (const s of prepared) {
      if (s.points.length === 0) continue;
      let best = s.points[0];
      for (const p of s.points) {
        if (Math.abs(p.x - xVal) < Math.abs(best.x - xVal)) best = p;
      }
      if (bestX === null || Math.abs(best.x - xVal) < Math.abs(bestX - xVal)) bestX = best.x;
      rows.push({ name: s.name, color: s.color, text: fmtY(best.y) });
    }
    if (bestX === null || rows.length === 0) {
      setHover(null);
      return;
    }
    setHover({
      leftPct: (sx(bestX) / W) * 100,
      label: fmtX(bestX),
      rows,
    });
  };

  const xTicks = [extent.xMin, (extent.xMin + extent.xMax) / 2, extent.xMax];

  return (
    <div className="chart">
      <div className="chart-legend">
        {prepared.map((s) => (
          <span key={s.name} className="chart-legend-item">
            <span className="chart-swatch" style={{ background: s.color }} />
            {s.name}
          </span>
        ))}
      </div>
      <div
        ref={wrapRef}
        className="chart-plot"
        style={{ height }}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
      >
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" width="100%" height="100%">
          {ticks.map((tv) => (
            <line
              key={tv}
              x1={PAD_L}
              x2={W - PAD_R}
              y1={sy(tv)}
              y2={sy(tv)}
              className="chart-grid"
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {prepared.map(
            (s) =>
              s.area &&
              s.points.length >= 2 && (
                <path key={s.name + '-a'} d={areaFor(s.points)} fill={s.color} opacity={0.12} stroke="none" />
              ),
          )}
          {prepared.map((s) => (
            <path
              key={s.name}
              d={pathFor(s.points)}
              fill="none"
              stroke={s.color}
              strokeWidth={2}
              strokeLinejoin="round"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
            />
          ))}
        </svg>

        {/* y labels as HTML so text is not stretched by the non-uniform viewBox */}
        {ticks.map((tv) => (
          <span
            key={'yl' + tv}
            className="chart-label chart-label-y mono"
            style={{
              top: `${((sy(tv) - 6) / H) * 100}%`,
            }}
          >
            {fmtY(tv)}
          </span>
        ))}
        {xTicks.map((xv, i) => (
          <span
            key={'xl' + i}
            className={`chart-label chart-label-x mono${i === 1 ? ' center' : i === 2 ? ' right' : ''}`}
            style={{ left: `${(sx(xv) / W) * 100}%` }}
          >
            {fmtX(xv)}
          </span>
        ))}

        {hover && (
          <>
            <span className="chart-crosshair" style={{ left: `${hover.leftPct}%` }} />
            <div className="chart-tip" style={{ left: `${hover.leftPct}%` }}>
              <div className="chart-tip-label mono">{hover.label}</div>
              {hover.rows.map((r) => (
                <div key={r.name} className="chart-tip-row">
                  <span className="chart-swatch" style={{ background: r.color }} />
                  <span>{r.name}</span>
                  <span className="mono">{r.text}</span>
                </div>
              ))}
            </div>
          </>
        )}
      </div>
    </div>
  );
}
