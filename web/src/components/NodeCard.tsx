import { Link } from 'react-router-dom';
import { useI18n } from '../i18n';
import { fmtBytes, fmtPct } from '../format';
import type { NodeView } from '../types';
import DistroLogo, { distroName } from './DistroLogo';
import Flag from './Flag';
import ProgressBar from './ProgressBar';

export default function NodeCard({ node }: { node: NodeView }) {
  const { t } = useI18n();
  const memPct = node.mem_total > 0 ? (node.mem_used / node.mem_total) * 100 : 0;
  const diskPct = node.disk_total > 0 ? (node.disk_used / node.disk_total) * 100 : 0;
  const hasQuota = node.period_pct >= 0 && node.quota_bytes != null;

  // 费用 (§16): the operator's free-text cost label, shown as a tag in the head,
  // immediately left of the distro badge ("what this box costs" belongs next to
  // "what this box is"). It is not parsed — "¥30/月", "30 CNY", "300/yr" are all
  // equally valid to the panel.
  const costChip = node.billing_note ? (
    <span className="chip cost-chip" title={t('billing_note')}>
      {node.billing_note}
    </span>
  ) : null;

  const dueChip = (() => {
    if (node.next_due_at == null || node.next_due_at <= 0) return null;
    const days = Math.ceil((node.next_due_at * 1000 - Date.now()) / 86400000);
    return (
      <span className="due-chip">
        {days >= 0 ? t('due_in_days', { d: days }) : t('due_overdue')}
      </span>
    );
  })();

  return (
    // Connectivity is carried by the card itself (.offline: red border +
    // hazard stripes); the head's right slot shows the distro badge instead.
    <Link
      className={'card node-card' + (node.online ? '' : ' offline')}
      to={`/nodes/${encodeURIComponent(node.id)}`}
      title={t(node.online ? 'online' : 'offline')}
    >
      <div className="node-card-head">
        <Flag cc={node.country_code} />
        <span className="node-card-name">{node.name || node.hostname || node.id}</span>
        {costChip}
        <DistroLogo
          id={node.distro_id}
          size={18}
          title={node.distro_id ? distroName(node.distro_id) : undefined}
        />
      </div>
      <div className="node-card-ip mono">{node.primary_ip || t('unknown')}</div>

      <ProgressBar label={t('cpu')} pct={node.cpu} text={fmtPct(node.cpu)} />
      <ProgressBar
        label={t('memory')}
        pct={memPct}
        text={`${fmtBytes(node.mem_used)} / ${fmtBytes(node.mem_total)}`}
      />
      <ProgressBar
        label={t('disk')}
        pct={diskPct}
        text={`${fmtBytes(node.disk_used)} / ${fmtBytes(node.disk_total)}`}
      />

      {hasQuota ? (
        <ProgressBar
          label={t('quota_label')}
          pct={node.period_pct}
          text={t('quota_used_of', {
            used: fmtBytes(node.period_used),
            quota: fmtBytes(node.quota_bytes ?? 0),
          })}
        />
      ) : (
        <div className="node-card-today">
          <span>
            {t('today_rx')} <span className="mono">{fmtBytes(node.today_rx)}</span>
          </span>
          <span>
            {t('today_tx')} <span className="mono">{fmtBytes(node.today_tx)}</span>
          </span>
        </div>
      )}

      <div className="node-card-foot">
        <span>{t('cores', { n: node.cpu_cores })}</span>
        <span>{t('agent_v', { v: node.agent_version || '?' })}</span>
        {dueChip}
      </div>
    </Link>
  );
}
