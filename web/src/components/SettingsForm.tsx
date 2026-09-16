import type { Dispatch, ReactNode, SetStateAction } from 'react';
import { useI18n } from '../i18n';
import type { SettingView } from '../types';

/**
 * Shared settings-form primitives (design §16). Both the basic settings page
 * and the notifications page render the same "labelled input backed by a
 * settings key + local draft" pattern; keeping one implementation is what
 * stops the two pages from drifting on sensitive-key handling.
 */

export interface FieldOpts {
  password?: boolean;
  type?: string;
  select?: { value: string; label: string }[];
  defaultValue?: string;
}

export type FieldFn = (key: string, label: string, opts?: FieldOpts) => ReactNode;

/**
 * useField binds a page's settings map + draft state and returns the renderer.
 * Sensitive keys never echo their stored value: they show set/unset and take a
 * fresh value (the server withholds it by design, §4.4).
 *
 * Must be called unconditionally in the component body, like any hook.
 */
export function useField(ctx: {
  settings: Record<string, SettingView>;
  draft: Record<string, string>;
  setDraft: Dispatch<SetStateAction<Record<string, string>>>;
}): FieldFn {
  const { t } = useI18n();
  const { settings, draft, setDraft } = ctx;
  return function field(key, label, opts) {
    const sv = settings[key];
    const value = draft[key] ?? (sv?.sensitive ? '' : sv?.value || opts?.defaultValue || '');
    const sensitiveHint = sv?.sensitive ? (sv.set ? t('sensitive_set') : t('sensitive_unset')) : '';
    return (
      <label key={key} className="field">
        <span>
          {label}
          {sv?.sensitive && <em className={'sensitive-tag' + (sv.set ? ' set' : '')}>{sensitiveHint}</em>}
        </span>
        {opts?.select ? (
          <select value={draft[key] ?? sv?.value ?? opts?.defaultValue ?? ''} onChange={(e) => setDraft((d) => ({ ...d, [key]: e.target.value }))}>
            {opts.select.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        ) : (
          <input
            type={opts?.type ?? (opts?.password ? 'password' : 'text')}
            value={value}
            placeholder={sensitiveHint || sv?.value || opts?.defaultValue || ''}
            onChange={(e) => setDraft((d) => ({ ...d, [key]: e.target.value }))}
          />
        )}
      </label>
    );
  };
}

export function SaveRow({ busy, savedMsg, onSave, label }: { busy: boolean; savedMsg: string | null; onSave: () => void; label: string }) {
  return (
    <div className="row-end">
      {savedMsg && <span className="form-ok">{savedMsg}</span>}
      <button type="button" className="btn primary" disabled={busy} onClick={onSave}>
        {busy ? '…' : label}
      </button>
    </div>
  );
}

/** Checkbox styled as a chip — the on/off control the notifications page uses. */
export function Toggle({
  checked,
  disabled,
  onChange,
  label,
}: {
  checked: boolean;
  disabled?: boolean;
  onChange: (next: boolean) => void;
  label: string;
}) {
  return (
    <label className="check-chip">
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      {label}
    </label>
  );
}
