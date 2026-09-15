import type { ReactNode } from 'react';

/** Small labeled value tile used across the node info / sing-box status grids. */
export default function Tile({ label, value, sub }: { label: ReactNode; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value mono">{value}</div>
      {sub && <div className="tile-sub">{sub}</div>}
    </div>
  );
}
