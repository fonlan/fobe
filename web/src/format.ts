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

/**
 * Day-granular due gap for the settings list (§15): that column prints whole days
 * ("18天"), not the "18d 3h" the durations elsewhere use. Overdue counts up and is
 * floored at 1 — "已逾期 0天" would read as "not late yet".
 */
export function fmtDueDays(dueAt: number, now = Date.now() / 1000): { overdue: boolean; days: number } | null {
  if (!isFinite(dueAt) || dueAt <= 0) return null;
  const delta = dueAt - now;
  if (delta < 0) return { overdue: true, days: Math.max(1, Math.ceil(-delta / 86400)) };
  return { overdue: false, days: Math.round(delta / 86400) };
}

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

export function toDatetimeLocal(unix: number): string {
  const d = new Date(unix * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** Unix seconds rendered as a seconds-precise datetime-local in an IANA zone. */
export function toDatetimeLocalInZone(unix: number, timeZone: string): string {
  const parts = zonedParts(unix, timeZone);
  return `${parts.year}-${pad(parts.month)}-${pad(parts.day)}T${pad(parts.hour)}:${pad(parts.minute)}:${pad(parts.second)}`;
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

/** Parse a datetime-local value as wall time in an IANA zone. Seconds optional:
 *  browsers serialize the input value canonically and drop ":00" seconds even
 *  with step=1, so a picked "00:00" comes back as "T00:00". */
export function datetimeLocalInZoneToUnix(s: string, timeZone: string): number | null {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(s);
  if (!match) return null;
  const target = {
    year: Number(match[1]), month: Number(match[2]), day: Number(match[3]),
    hour: Number(match[4]), minute: Number(match[5]), second: Number(match[6] ?? 0),
  };
  const targetUTC = Date.UTC(target.year, target.month - 1, target.day, target.hour, target.minute, target.second);
  let unix = Math.floor(targetUTC / 1000);
  // Offset may change around DST. Iteration converges on the instant whose
  // formatted wall time matches the node timezone without relying on browser TZ.
  for (let i = 0; i < 3; i++) {
    const actual = zonedParts(unix, timeZone);
    const actualUTC = Date.UTC(actual.year, actual.month - 1, actual.day, actual.hour, actual.minute, actual.second);
    const next = unix + Math.round((targetUTC - actualUTC) / 1000);
    if (next === unix) return unix;
    unix = next;
  }
  return unix;
}

type ZonedParts = { year: number; month: number; day: number; hour: number; minute: number; second: number };

function zonedParts(unix: number, timeZone: string): ZonedParts {
  const formatter = new Intl.DateTimeFormat('en-CA', {
    timeZone: timeZone || 'UTC', year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
  });
  const values: Record<string, number> = {};
  for (const part of formatter.formatToParts(new Date(unix * 1000))) {
    if (part.type !== 'literal') values[part.type] = Number(part.value);
  }
  return {
    year: values.year, month: values.month, day: values.day,
    hour: values.hour, minute: values.minute, second: values.second,
  };
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
