export default function ProgressBar({
  label,
  pct,
  text,
  tone = 'usage',
}: {
  label: string;
  pct: number;
  text?: string;
  /**
   * "usage" (default) colours the fill by how full it is — right for disk/quota
   * gauges, where 90% is a warning. "plain" keeps the accent colour: a download
   * at 90% is good news, not a risk.
   */
  tone?: 'usage' | 'plain';
}) {
  const p = Math.max(0, Math.min(100, isFinite(pct) ? pct : 0));
  const cls = tone === 'plain' ? '' : p >= 90 ? ' crit' : p >= 75 ? ' warn' : '';
  return (
    <div className="pbar">
      <div className="pbar-head">
        <span className="pbar-label">{label}</span>
        {text !== undefined && <span className="pbar-text mono">{text}</span>}
      </div>
      <div className="pbar-track">
        <div className={'pbar-fill' + cls} style={{ width: `${p}%` }} />
      </div>
    </div>
  );
}
