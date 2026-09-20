// Pieces shared by LineChart and BarChart: the stretched-viewBox geometry
// constants, the legend toggle state, and the markup that must stay identical
// between the two charts (legend, grid lines, y labels). The tooltip differs
// (crosshair vs bar highlight) and stays with each chart.
import { useCallback, useState } from 'react';

export const W = 640;
export const H = 220;
export const PAD_L = 10;
export const PAD_R = 12;
export const PAD_T = 12;
// PAD_B is deliberately per-chart: the line chart sits closer to its x labels.

/** Series the operator switched off by clicking its legend entry. Keyed by the
 *  series name — the same key the legend and tooltip use — so the choice
 *  survives the polling refresh. Each chart instance owns its own set: hiding
 *  "rx" on one chart must not touch the chart below it. */
export function useHiddenSeries(): { hidden: ReadonlySet<string>; toggle: (name: string) => void } {
  const [hidden, setHidden] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = useCallback((name: string) => {
    setHidden((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }, []);
  return { hidden, toggle };
}

interface LegendItem {
  name: string;
  color: string;
}

/** Clickable legend: click toggles the series off/on. Off = hollow ring in the
 *  series colour: the row stays identifiable, but it no longer claims to be
 *  plotted. */
export function ChartLegend({
  items,
  hidden,
  onToggle,
  hint,
}: {
  items: LegendItem[];
  hidden: ReadonlySet<string>;
  onToggle: (name: string) => void;
  hint?: string;
}) {
  return (
    <div className="chart-legend">
      {items.map((s) => {
        const off = hidden.has(s.name);
        return (
          <button
            key={s.name}
            type="button"
            className={`chart-legend-item${off ? ' off' : ''}`}
            aria-pressed={!off}
            title={hint}
            onClick={() => onToggle(s.name)}
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
  );
}

/** Horizontal grid lines at the y ticks. */
export function ChartGrid({ ticks, sy }: { ticks: number[]; sy: (v: number) => number }) {
  return (
    <>
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
    </>
  );
}

/** y tick labels as HTML so text is not stretched by the non-uniform viewBox. */
export function ChartYLabels({
  ticks,
  sy,
  fmt,
}: {
  ticks: number[];
  sy: (v: number) => number;
  fmt: (v: number) => string;
}) {
  return (
    <>
      {ticks.map((tv) => (
        <span key={'yl' + tv} className="chart-label chart-label-y mono" style={{ top: `${((sy(tv) - 6) / H) * 100}%` }}>
          {fmt(tv)}
        </span>
      ))}
    </>
  );
}
