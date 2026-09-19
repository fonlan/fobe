import { useCallback, useEffect, useState } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import { fmtTime } from '../format';
import type { AuditRow } from '../types';

const PAGE_SIZES = [20, 50, 100, 200];
const DEFAULT_PAGE_SIZE = 20;
/** The chosen page size survives reloads: the operator picked it for a reason. */
const SIZE_KEY = 'fobe.audit_page_size';

function readPageSize(): number {
  try {
    const n = Number(localStorage.getItem(SIZE_KEY));
    return PAGE_SIZES.indexOf(n) >= 0 ? n : DEFAULT_PAGE_SIZE;
  } catch {
    // storage unavailable (private mode): fall back to the default
    return DEFAULT_PAGE_SIZE;
  }
}

/**
 * The page numbers to render: first and last always, plus a window around the
 * current page, with gaps collapsed into an ellipsis. A short trail shows every
 * page. Longer than 7 numbers plus ellipses stops reading as a pager.
 */
function pageNumbers(current: number, pages: number): (number | 'gap')[] {
  if (pages <= 7) {
    const all: number[] = [];
    for (let i = 1; i <= pages; i++) all.push(i);
    return all;
  }
  const wanted = [1, current - 1, current, current + 1, pages].filter((n) => n >= 1 && n <= pages);
  const unique = Array.from(new Set(wanted)).sort((a, b) => a - b);
  const out: (number | 'gap')[] = [];
  let prev = 0;
  for (const n of unique) {
    if (n - prev > 1) out.push('gap');
    out.push(n);
    prev = n;
  }
  return out;
}

export default function Audit() {
  const { t } = useI18n();
  const [entries, setEntries] = useState<AuditRow[] | null>(null);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(readPageSize);
  const [jump, setJump] = useState('');
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const pages = Math.max(1, Math.ceil(total / pageSize));

  const load = useCallback(async (p: number, size: number) => {
    setLoading(true);
    try {
      const r = await api.listAudit(size, p);
      setEntries(r.entries ?? []);
      setTotal(r.total ?? 0);
      setErr(null);
      // The server clamps an out-of-range page (the trail can shrink under a
      // stale page number) and echoes what it served; snap the control to it.
      if (r.page !== p) setPage(r.page);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void load(page, pageSize);
  }, [load, page, pageSize]);

  const goTo = (n: number) => {
    if (!Number.isFinite(n)) return;
    setPage(Math.min(Math.max(1, Math.trunc(n)), pages));
  };

  const changeSize = (n: number) => {
    setPageSize(n);
    setPage(1);
    try {
      localStorage.setItem(SIZE_KEY, String(n));
    } catch {
      // storage unavailable; the choice just does not persist
    }
  };

  const submitJump = (e: React.FormEvent) => {
    e.preventDefault();
    if (jump.trim() === '') return;
    goTo(Number(jump));
    setJump('');
  };

  return (
    <div>
      <div className="page-head">
        <h2>{t('sec_audit')}</h2>
      </div>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load(page, pageSize)}>
            {t('retry')}
          </button>
        </div>
      )}

      {entries !== null && entries.length === 0 && <div className="empty-hint">{t('audit_empty')}</div>}

      {entries !== null && entries.length > 0 && (
        /* Scroll wrapper carries the card — overflow-x on the <table> itself
           never applied (table boxes are not scroll containers). */
        <div className="card table-card">
          <table className="table">
            <thead>
              <tr>
                <th>{t('audit_time')}</th>
                <th>{t('audit_actor')}</th>
                <th>{t('audit_action')}</th>
                <th>{t('audit_node')}</th>
                <th>{t('audit_detail')}</th>
                <th>{t('audit_source')}</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id}>
                  <td className="mono nowrap">{fmtTime(e.ts)}</td>
                  <td>{e.actor}</td>
                  <td>
                    <span className="chip">{e.action}</span>
                    {e.risk === 'risky' && <span className="chip status-failed">risky</span>}
                  </td>
                  {/* node_name falls back to the raw ID once the node is deleted */}
                  <td>{e.node_name || e.node_id || '-'}</td>
                  <td className="detail-cell" title={e.command}>
                    {e.command || '-'}
                  </td>
                  <td className="mono">{e.source_ip || '-'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {entries !== null && entries.length > 0 && (
        <div className="pager">
          <span className="pager-info">
            {t('audit_total', { n: total })} · {t('audit_page_of', { page, pages })}
          </span>

          <label className="pager-size">
            {t('audit_per_page')}
            <select value={pageSize} onChange={(e) => changeSize(Number(e.target.value))} disabled={loading}>
              {PAGE_SIZES.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>

          <button type="button" className="btn" disabled={loading || page <= 1} onClick={() => goTo(page - 1)}>
            {t('audit_prev')}
          </button>

          {pageNumbers(page, pages).map((p, i) =>
            p === 'gap' ? (
              <span key={'gap-' + i} className="pager-gap">
                …
              </span>
            ) : (
              <button
                key={p}
                type="button"
                className={'btn pager-num' + (p === page ? ' active' : '')}
                aria-current={p === page ? 'page' : undefined}
                disabled={loading}
                onClick={() => goTo(p)}
              >
                {p}
              </button>
            ),
          )}

          <button type="button" className="btn" disabled={loading || page >= pages} onClick={() => goTo(page + 1)}>
            {t('audit_next')}
          </button>

          <form className="pager-jump" onSubmit={submitJump}>
            <span>{t('audit_jump')}</span>
            <input
              type="number"
              min={1}
              max={pages}
              value={jump}
              onChange={(e) => setJump(e.target.value)}
              aria-label={t('audit_jump')}
            />
            <span>{t('audit_jump_unit')}</span>
            <button type="submit" className="btn" disabled={loading || jump.trim() === ''}>
              {t('audit_go')}
            </button>
          </form>
        </div>
      )}
    </div>
  );
}
