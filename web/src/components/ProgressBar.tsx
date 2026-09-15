export default function ProgressBar({
  label,
  pct,
  text,
}: {
  label: string;
  pct: number;
  text?: string;
}) {
  const p = Math.max(0, Math.min(100, isFinite(pct) ? pct : 0));
  const tone = p >= 90 ? ' crit' : p >= 75 ? ' warn' : '';
  return (
    <div className="pbar">
      <div className="pbar-head">
        <span className="pbar-label">{label}</span>
        {text !== undefined && <span className="pbar-text mono">{text}</span>}
      </div>
      <div className="pbar-track">
        <div className={'pbar-fill' + tone} style={{ width: `${p}%` }} />
      </div>
    </div>
  );
}
