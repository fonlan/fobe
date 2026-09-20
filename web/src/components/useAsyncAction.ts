// Shared async-action wrapper for panel pages. Every button handler used to
// hand-roll the same scaffolding: setBusy(true), clear the stale err/msg, run
// the call, apiErrorMessage into err on failure, setBusy(false) in finally.
// The hook owns no state: it binds to a component's existing setters, so
// several runners can share one err/msg pair while keeping separate busy flags
// (SingboxCacheCard's busy vs impactBusy), and forms without a message line
// (IPList) simply pass no msg setter. Normalization: the runner always clears
// err and msg at start — a few sites previously left a stale success message
// or error on screen while the next action was already running.
import { useCallback } from 'react';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';

export type AsyncAction = (
  fn: () => Promise<unknown>,
  /** Set on success when provided; omit when fn reports its own outcome. */
  okMsg?: string | null,
  /** Replaces the default apiErrorMessage catch (custom error mapping). */
  onError?: (e: unknown) => void,
) => Promise<void>;

export function useAsyncAction(
  setBusy: (v: boolean) => void,
  setErr: (v: string | null) => void,
  setMsg?: (v: string | null) => void,
): AsyncAction {
  const { t } = useI18n();
  return useCallback(
    async (fn, okMsg, onError) => {
      setBusy(true);
      setErr?.(null);
      setMsg?.(null);
      try {
        await fn();
        if (okMsg) setMsg?.(okMsg);
      } catch (e) {
        if (onError) onError(e);
        else setErr?.(apiErrorMessage(e, t));
      } finally {
        setBusy(false);
      }
    },
    [t, setBusy, setErr, setMsg],
  );
}
