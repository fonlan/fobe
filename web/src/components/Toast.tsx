import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';

/** How long a toast stays on screen before it removes itself. */
const TOAST_MS = 2600;

/**
 * Transient bottom-center notice (design §10 修订: "订阅链接已复制").
 * The copy itself is invisible — without an answer, a click that silently
 * worked is indistinguishable from one that did nothing.
 */
export default function Toast({
  message,
  tone = 'ok',
}: {
  message: ReactNode;
  tone?: 'ok' | 'error';
}) {
  return (
    <div className={`toast${tone === 'error' ? ' toast-error' : ''}`} role="status" aria-live="polite">
      {message}
    </div>
  );
}

/**
 * One-page toast state. Repeat calls replace whatever is on screen and restart
 * the timer, so rapid clicks stack nothing; the returned node is rendered by
 * the page (fixed positioning, so where it sits in the tree does not matter).
 */
export function useToast(durationMs = TOAST_MS) {
  const [toast, setToast] = useState<{ id: number; message: string; tone: 'ok' | 'error' } | null>(null);
  const seq = useRef(0);

  const show = useCallback((message: string, tone: 'ok' | 'error' = 'ok') => {
    seq.current += 1;
    setToast({ id: seq.current, message, tone });
  }, []);

  useEffect(() => {
    if (!toast) return;
    const timer = window.setTimeout(() => setToast(null), durationMs);
    return () => window.clearTimeout(timer);
  }, [toast, durationMs]);

  return {
    show,
    node: toast ? <Toast key={toast.id} message={toast.message} tone={toast.tone} /> : null,
  };
}
