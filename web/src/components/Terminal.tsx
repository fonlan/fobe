import { useEffect, useRef, useState } from 'react';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as XTerm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import * as api from '../api';
import { useI18n } from '../i18n';
import type {
  TerminalClosedPayload,
  TerminalEnvelope,
  TerminalOutputPayload,
} from '../types';

type ConnectionState = 'connecting' | 'connected' | 'disconnected' | 'closed';

/** Agent/server terminal close reasons that get a localized message. */
const localizedReasons: Record<string, string> = {
  terminal_unsupported_mode: 'terminal_unsupported_mode',
  terminal_invalid_size: 'terminal_invalid_size',
  terminal_start_failed: 'terminal_start_failed',
  terminal_exited: 'terminal_exited',
};

function isEnvelope(value: unknown): value is TerminalEnvelope {
  if (!value || typeof value !== 'object') return false;
  const frame = value as { type?: unknown };
  return typeof frame.type === 'string';
}

function payloadRecord(payload: unknown): Record<string, unknown> {
  return payload && typeof payload === 'object' ? (payload as Record<string, unknown>) : {};
}

function sizeFor(term: XTerm): { cols: number; rows: number } {
  return { cols: Math.max(2, term.cols), rows: Math.max(2, term.rows) };
}

export interface TerminalHandle {
  /** Write a raw string into the xterm buffer (used for AI command echo). */
  write: (text: string) => void;
  /** Tear down the current session and open a fresh agent terminal. */
  reconnect: () => void;
}

export interface TerminalProps {
  nodeId: string;
  nodeLabel?: string;
  onSessionChange?: (sessionId: string | null) => void;
  onReady?: (handle: TerminalHandle | null) => void;
}

export default function Terminal({
  nodeId,
  nodeLabel,
  onSessionChange,
  onReady,
}: TerminalProps) {
  const { t } = useI18n();
  const hostRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const onSessionChangeRef = useRef(onSessionChange);
  const onReadyRef = useRef(onReady);
  const [state, setState] = useState<ConnectionState>('connecting');
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [closeReason, setCloseReason] = useState<string | null>(null);
  // Bumped by reconnect() to force the session effect to re-run.
  const [epoch, setEpoch] = useState(0);

  useEffect(() => {
    onSessionChangeRef.current = onSessionChange;
    onReadyRef.current = onReady;
  }, [onSessionChange, onReady]);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const terminal = new XTerm({
      cursorBlink: true,
      convertEol: true,
      // Canvas renderer cannot resolve CSS variables — use a concrete stack.
      fontFamily: '"SF Mono", Menlo, Monaco, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      theme: {
        background: '#101722',
        foreground: '#e7ebf3',
        cursor: '#5b8cff',
        selectionBackground: 'rgba(91, 140, 255, 0.35)',
      },
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(host);
    termRef.current = terminal;
    fitRef.current = fitAddon;
    onReadyRef.current?.({
      write: (text: string) => {
        termRef.current?.write(text);
      },
      reconnect: () => {
        setEpoch((current) => current + 1);
      },
    });

    const fit = () => {
      try {
        fitAddon.fit();
      } catch {
        // The container may be hidden while a parent route is transitioning.
      }
    };
    fit();

    let closed = false;
    let lastCols = 0;
    let lastRows = 0;
    const send = (frame: api.TerminalClientEnvelope) => {
      const socket = wsRef.current;
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(frame));
    };
    const sendResize = () => {
      const current = termRef.current;
      if (!current) return;
      const { cols, rows } = sizeFor(current);
      // Only propagate real changes; xterm may emit resize events in bursts.
      if (cols === lastCols && rows === lastRows) return;
      lastCols = cols;
      lastRows = rows;
      send(api.terminalEnvelope('terminal_resize', { cols, rows }));
    };

    const socket = new WebSocket(api.terminalWebSocketURL(nodeId));
    wsRef.current = socket;
    socket.onopen = () => {
      if (closed) return;
      setState('connected');
      setCloseReason(null);
      const { cols, rows } = sizeFor(terminal);
      send(api.terminalEnvelope('terminal_open', { cols, rows }));
      terminal.focus();
    };
    socket.onmessage = (event) => {
      try {
        const value: unknown = JSON.parse(String(event.data));
        if (!isEnvelope(value)) return;
        if (value.type === 'terminal_output') {
          const payload = payloadRecord(value.payload) as unknown as TerminalOutputPayload;
          if (typeof payload.data === 'string') {
            if (payload.session_id) {
              setSessionId(payload.session_id);
              onSessionChangeRef.current?.(payload.session_id);
            }
            terminal.write(payload.data);
          }
        } else if (value.type === 'terminal_closed') {
          const payload = payloadRecord(value.payload) as TerminalClosedPayload;
          const rawReason = typeof payload.reason === 'string' ? payload.reason : null;
          // Never expose unexpected agent implementation errors in the UI.
          const reason = rawReason && localizedReasons[rawReason] ? t(localizedReasons[rawReason]) : t('terminal_closed');
          setCloseReason(reason);
          setState('closed');
          if (payload.session_id) {
            setSessionId(payload.session_id);
            onSessionChangeRef.current?.(payload.session_id);
          }
        }
      } catch {
        // Ignore malformed frames so a bad server frame cannot break the terminal.
      }
    };
    socket.onerror = () => {
      if (!closed) {
        setState('disconnected');
        setCloseReason(t('terminal_connection_error'));
      }
    };
    socket.onclose = () => {
      if (!closed) setState((current) => (current === 'closed' ? current : 'disconnected'));
    };

    const onData = (data: string) => {
      send(api.terminalEnvelope('terminal_input', { data }));
    };
    const dataDisposable = terminal.onData(onData);
    // Never call fit() inside onResize: fit() itself triggers xterm resize
    // events, and the pair recurses until the renderer hangs. fit() runs only
    // from the (debounced) ResizeObserver below.
    const resizeDisposable = terminal.onResize(() => sendResize());
    // Debounce the observer through rAF: container resizes arrive in bursts
    // while fit() reflows the canvas.
    let resizeRAF = 0;
    const observer = new ResizeObserver(() => {
      if (resizeRAF) return;
      resizeRAF = window.requestAnimationFrame(() => {
        resizeRAF = 0;
        sendResize();
      });
    });
    observer.observe(host);

    return () => {
      closed = true;
      if (resizeRAF) window.cancelAnimationFrame(resizeRAF);
      observer.disconnect();
      dataDisposable.dispose();
      resizeDisposable.dispose();
      const current = wsRef.current;
      if (current?.readyState === WebSocket.OPEN) {
        current.send(JSON.stringify(api.terminalEnvelope('terminal_close', {})));
      }
      current?.close();
      wsRef.current = null;
      onSessionChangeRef.current?.(null);
      onReadyRef.current?.(null);
      termRef.current = null;
      fitRef.current = null;
      terminal.dispose();
    };
    // epoch is the explicit reconnect trigger; `t` supplies close messages.
  }, [nodeId, epoch, t]);

  const stateLabel = state === 'connecting' ? t('terminal_connecting') : state === 'connected' ? t('terminal_connected') : state === 'closed' ? t('terminal_closed') : t('terminal_disconnected');
  const stateClass = state === 'connected' ? 'status-ok' : state === 'connecting' ? '' : 'status-failed';
  const canReconnect = state === 'closed' || state === 'disconnected';

  return (
    <section className="terminal-card card">
      <div className="terminal-head">
        <div>
          <h3>{t('terminal_title')}</h3>
          <p className="hint">{nodeLabel || nodeId}</p>
        </div>
        <div className="terminal-head-controls">
          <span className={`chip ${stateClass}`}>{stateLabel}</span>
        </div>
      </div>
      <div ref={hostRef} className="terminal-host" role="application" aria-label={t('terminal_title')} />
      <div className="terminal-foot">
        <span className="hint mono">{t('terminal_node', { id: nodeId })}</span>
        <span className="hint mono">{t('terminal_session', { id: sessionId || t('terminal_session_pending') })}</span>
        {closeReason && <span className="hint">{closeReason}</span>}
        {canReconnect && (
          <button type="button" className="btn small" onClick={() => setEpoch((current) => current + 1)}>
            {t('terminal_reconnect')}
          </button>
        )}
      </div>
    </section>
  );
}
