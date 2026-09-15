// Light/dark theme via CSS variables on <html data-theme>.
// Default follows the system; manual choice persists to localStorage and is
// dual-written to the server (§16 ui.theme) so devices stay consistent.

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import * as api from './api';

export type ThemeMode = 'light' | 'dark' | 'system';

const THEME_KEY = 'fobe.theme';

/** Dispatched by api.login() so the server value can be pulled after sign-in. */
export const AUTH_READY_EVENT = 'fobe:auth-ready';

function detectTheme(): ThemeMode {
  try {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved === 'light' || saved === 'dark' || saved === 'system') return saved;
  } catch {
    // storage unavailable
  }
  return 'system';
}

function systemPrefersDark(): boolean {
  return typeof matchMedia !== 'undefined' && matchMedia('(prefers-color-scheme: dark)').matches;
}

interface ThemeCtx {
  mode: ThemeMode;
  setMode: (m: ThemeMode) => void;
  resolved: 'light' | 'dark';
}

const Ctx = createContext<ThemeCtx | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(detectTheme);
  const [sysDark, setSysDark] = useState<boolean>(systemPrefersDark);
  // set once a signed-in GET /api/settings succeeded; gates the dual-write so
  // anonymous visitors never fire PUTs (§16: only logged-in writes).
  const canSync = useRef(false);

  // Track OS preference so "system" stays live.
  useEffect(() => {
    const mq = matchMedia('(prefers-color-scheme: dark)');
    const onChange = (e: MediaQueryListEvent) => setSysDark(e.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);

  // Pull the server's ui.theme once (and again after login): when a value is
  // stored there it wins over the local default — cross-device consistency.
  // Read-only: this never writes back, so no sync loop is possible.
  useEffect(() => {
    let alive = true;
    const pull = async () => {
      const server = await api.getServerTheme().catch(() => null);
      if (!alive || !server) return;
      canSync.current = true;
      setModeState((cur) => (cur === server ? cur : server));
    };
    void pull();
    window.addEventListener(AUTH_READY_EVENT, pull);
    return () => {
      alive = false;
      window.removeEventListener(AUTH_READY_EVENT, pull);
    };
  }, []);

  const resolved: 'light' | 'dark' = mode === 'system' ? (sysDark ? 'dark' : 'light') : mode;

  useEffect(() => {
    document.documentElement.dataset.theme = resolved;
  }, [resolved]);

  const setMode = useCallback((m: ThemeMode) => {
    setModeState(m);
    try {
      localStorage.setItem(THEME_KEY, m);
    } catch {
      // storage unavailable
    }
    // §16 dual-write on the user's explicit choice only; fire-and-forget and
    // silently ignored when the session is gone.
    if (canSync.current) {
      void api.putSettings({ 'ui.theme': m }).catch(() => {});
    }
  }, []);

  const value = useMemo(() => ({ mode, setMode, resolved }), [mode, setMode, resolved]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme(): ThemeCtx {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error('useTheme must be used inside ThemeProvider');
  return ctx;
}
