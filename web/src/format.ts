// Pure formatting helpers shared by pages and charts.

/** ISO 3166-1 alpha-2 -> regional-indicator emoji; empty/invalid -> empty string. */
export function flagEmoji(cc: string): string {
  if (!/^[A-Za-z]{2}$/.test(cc)) return '';
  return String.fromCodePoint(...[...cc.toUpperCase()].map((c) => 0x1f1e6 + c.charCodeAt(0) - 65));
}

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB'];

export function fmtBytes(n: number): string {
  if (!isFinite(n)) return '-';
  const neg = n < 0;
  let v = Math.abs(n);
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const text = i === 0 ? String(Math.round(v)) : v >= 100 ? v.toFixed(0) : v.toFixed(1);
  return (neg ? '-' : '') + text + ' ' + BYTE_UNITS[i];
}

export function fmtRate(n: number): string {
  return fmtBytes(n) + '/s';
}

export function fmtPct(p: number): string {
  if (!isFinite(p)) return '-';
  return (Math.round(p * 10) / 10).toFixed(1) + '%';
}

/** Unix seconds -> local "YYYY-MM-DD HH:mm". */
export function fmtTime(unix: number | null | undefined): string {
  if (unix == null || unix <= 0) return '-';
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** Short time for chart x labels. */
export function fmtTimeShort(unix: number): string {
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** "YYYY-MM-DD" (UTC date of the unix timestamp) for chart labels of daily data. */
export function fmtDate(iso: string): string {
  return iso.slice(5); // MM-DD
}

export function fmtDuration(seconds: number): string {
  if (!isFinite(seconds) || seconds <= 0) return '-';
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

/** Format a due-time gap while preserving whether the date is already past. */
export function fmtDueDuration(dueAt: number, now = Date.now() / 1000): { overdue: boolean; duration: string } | null {
  if (!isFinite(dueAt) || dueAt <= 0) return null;
  const delta = dueAt - now;
  return { overdue: delta < 0, duration: fmtDuration(Math.abs(delta)) };
}

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

export function toDatetimeLocal(unix: number): string {
  const d = new Date(unix * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function toDateString(unix: number): string {
  const d = new Date(unix * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Local-seconds for a "YYYY-MM-DD" date string at local midnight. */
export function dateStrToUnix(s: string): number | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return null;
  const t = new Date(s + 'T00:00:00').getTime();
  return isFinite(t) ? Math.floor(t / 1000) : null;
}

/** Local-seconds for a "YYYY-MM-DDTHH:mm" datetime-local string. */
export function datetimeLocalToUnix(s: string): number | null {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(s)) return null;
  const t = new Date(s).getTime();
  return isFinite(t) ? Math.floor(t / 1000) : null;
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}
