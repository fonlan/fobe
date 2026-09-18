import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import Terminal from '../components/Terminal';
import type {
  AICatalog,
  AINeedsConfirmationEvent,
  AIProvider,
  AIProviderModel,
  AISessionHistory,
  AIToolResultEvent,
  AITurnEndReason,
} from '../types';

/**
 * The assistant transcript, as the panel keeps it.
 *
 * A flat list rather than nested turns: the loop (§12.6) can interleave text →
 * tool → text → tool inside ONE user message, and a nested model would have to
 * invent a grouping the server never sends (a tool result carries no
 * "which assistant turn asked for it" field).
 */
type ChatEntry =
  | { kind: 'user'; text: string }
  | { kind: 'assistant'; text: string; thinking: string }
  | { kind: 'tool'; name: string; status: string; command: string; exitCode: number | null; reason: string }
  | { kind: 'tool_log'; text: string }
  // The reason a turn stopped, replayed from the transcript (§12.6). Without it
  // a reloaded conversation shows a half answer with no explanation, which reads
  // as a broken model rather than "the per-turn budget cut it short".
  | { kind: 'turn_end'; reason: string; changes: number }
  | {
      kind: 'confirm';
      name: string;
      actionId: string;
      command: string;
      reason: string;
      risk: string;
      answered?: 'approved' | 'rejected';
    };

/** One selectable (provider, model) pair — the unit a session is pinned to. */
interface ModelOption {
  provider: AIProvider;
  model: AIProviderModel;
}

// The AI session id is remembered per node in sessionStorage. It must survive a
// reload (that is the whole point of the replay path) but it should NOT outlive
// the tab: a session is pinned to one (provider, model, protocol) triple and its
// reasoning blocks are protocol-native (§12.5), so resurrecting a days-old
// conversation in a fresh tab would either 409 or replay blocks that no longer
// match the selected model.
const AI_SESSION_PREFIX = 'fobe.ai.session.';

function readStoredAISession(nodeId: string): string | null {
  try {
    return sessionStorage.getItem(AI_SESSION_PREFIX + nodeId);
  } catch {
    return null;
  }
}

function writeStoredAISession(nodeId: string, sessionId: string): void {
  try {
    sessionStorage.setItem(AI_SESSION_PREFIX + nodeId, sessionId);
  } catch {
    // storage unavailable (private mode): replay degrades to "no history"
  }
}

function clearStoredAISession(nodeId: string): void {
  try {
    sessionStorage.removeItem(AI_SESSION_PREFIX + nodeId);
  } catch {
    // storage unavailable
  }
}

/**
 * Pairs the picker may offer: only providers that are enabled AND have a key
 * (§12.1 — a provider without a key can never serve a turn). Model ids are
 * taken from the provider's own `models[]`, i.e. the links that exist.
 */
function usableModels(catalog: AICatalog | null): ModelOption[] {
  if (!catalog) return [];
  const out: ModelOption[] = [];
  for (const provider of catalog.providers) {
    if (!provider.enabled || !provider.has_key) continue;
    for (const model of provider.models) {
      // The server also refuses a disabled model at call time (resolveAIModel),
      // so hiding it here is the UI half of a switch that would otherwise be a
      // label with no effect. `undefined` (older server) counts as enabled.
      if (model.enabled === false) continue;
      out.push({ provider, model });
    }
  }
  return out;
}

function pairIsDefault(catalog: AICatalog | null, providerId: string, modelId: string): boolean {
  return !!catalog && catalog.default_provider_id === providerId && catalog.default_model_id === modelId;
}

// A `/` cannot appear inside an encodeURIComponent() result, so splitting on the
// first one is unambiguous even when a model id contains a slash (org/model).
function encodePair(providerId: string, modelId: string): string {
  return `${encodeURIComponent(providerId)}/${encodeURIComponent(modelId)}`;
}

function decodePair(value: string): { providerId: string; modelId: string } {
  const slash = value.indexOf('/');
  if (slash < 0) return { providerId: '', modelId: '' };
  return { providerId: decodeURIComponent(value.slice(0, slash)), modelId: decodeURIComponent(value.slice(slash + 1)) };
}

/**
 * Turn a stored transcript into bubbles. Assistant rows with neither text nor
 * thinking are dropped: those are the pure tool-call turns the envelope stores,
 * and an empty bubble would look like a rendering bug.
 */
function historyToEntries(history: AISessionHistory): ChatEntry[] {
  const out: ChatEntry[] = [];
  for (const message of history.messages ?? []) {
    if (message.role === 'user') {
      out.push({ kind: 'user', text: message.content });
    } else if (message.role === 'assistant') {
      const thinking = message.thinking ?? '';
      if (message.content === '' && thinking === '') continue;
      out.push({ kind: 'assistant', text: message.content, thinking });
    } else if (message.role === 'turn_end') {
      out.push({ kind: 'turn_end', reason: message.turn_reason ?? '', changes: message.changes ?? 0 });
    } else if (message.role === 'tool') {
      // The stored shape keeps only the model-facing text (the envelope's
      // tool_results carry call ids, not names/commands), so this renders as a
      // plain result block rather than pretending to be a tool card.
      out.push({ kind: 'tool_log', text: message.content });
    }
  }
  return out;
}

function AssistantPanel({
  nodeId,
  sessionId,
}: {
  nodeId: string;
  sessionId: string | null;
}) {
  const { t } = useI18n();
  const [entries, setEntries] = useState<ChatEntry[]>([]);
  const [aiSessionId, setAiSessionId] = useState<string | null>(null);
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [turnEnd, setTurnEnd] = useState<{ reason: AITurnEndReason; changes?: number } | null>(null);
  const [catalog, setCatalog] = useState<AICatalog | null>(null);
  const [catalogErr, setCatalogErr] = useState<string | null>(null);
  const [historyLoading, setHistoryLoading] = useState(true);
  const [providerId, setProviderId] = useState('');
  const [modelId, setModelId] = useState('');
  const [reasoning, setReasoning] = useState('');
  const [isServerDefault, setIsServerDefault] = useState(false);
  // True when (default_provider_id, default_model_id) was NOT selectable and the
  // picker fell back to the first usable pair. Silent substitution would make
  // the operator believe a different model is answering than the one configured.
  const [defaultUnavailable, setDefaultUnavailable] = useState(false);
  const [sessionNotice, setSessionNotice] = useState<string | null>(null);
  const [savingDefault, setSavingDefault] = useState(false);
  const listRef = useRef<HTMLDivElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // Why the in-flight turn was aborted: the stop button must report itself, a
  // model switch must stay silent (it already reset the panel).
  const abortIntentRef = useRef<'stop' | 'discard' | null>(null);
  // Whether an assistant bubble is open for the CURRENT loop step. A tool result
  // closes it, so text emitted after a tool runs opens a fresh bubble instead of
  // being appended above the tool card.
  const assistantOpenRef = useRef(false);

  const scrollToEnd = (force = false) => {
    const el = listRef.current;
    if (!el) return;
    // Don't yank the viewport while the operator is reading back through the
    // transcript: only auto-follow when already near the bottom.
    if (!force && el.scrollHeight - el.scrollTop - el.clientHeight > 120) return;
    window.requestAnimationFrame(() => {
      el.scrollTo({ top: el.scrollHeight });
    });
  };

  const usable = useMemo(() => usableModels(catalog), [catalog]);

  const groups = useMemo(() => {
    const byProvider = new Map<string, { provider: AIProvider; models: AIProviderModel[] }>();
    for (const option of usable) {
      const existing = byProvider.get(option.provider.id);
      if (existing) existing.models.push(option.model);
      else byProvider.set(option.provider.id, { provider: option.provider, models: [option.model] });
    }
    return [...byProvider.values()];
  }, [usable]);

  const selected = usable.find((o) => o.provider.id === providerId && o.model.id === modelId);
  // The server computes the effective levels per protocol (§12.5); the panel
  // never derives them. An empty list is rendered as a sentence, not a dropdown.
  const levels = selected?.model.effective_levels ?? [];

  const errText = (code: string, detail?: string): string => {
    const key = 'err_' + code;
    const base = t(key) === key ? t('err_default', { code }) : t(key);
    return detail ? `${base}: ${detail}` : base;
  };

  const toolStatusText = (status: string): string => {
    const key = 'ai_tool_status_' + status;
    return t(key) === key ? status : t(key);
  };

  const toolStatusClass = (status: string): string => {
    if (status === 'ok') return 'status-ok';
    if (status === 'queued') return 'status-timeout';
    if (status === 'failed' || status === 'blocked' || status === 'refused') return 'status-failed';
    return '';
  };

  // An explicit mapping (not `'ai_turn_' + reason`) because the server's reason
  // values are not a clean suffix set: turn_budget / turn_timeout would produce
  // ai_turn_turn_budget / ai_turn_turn_timeout.
  const turnReasonText = (reason: AITurnEndReason): string => {
    switch (reason) {
      case 'completed':
        return t('ai_turn_completed');
      case 'turn_budget':
        return t('ai_turn_budget');
      case 'repeat_call':
        return t('ai_turn_repeat');
      case 'turn_timeout':
        return t('ai_turn_timeout');
      case 'read_limit':
        return t('ai_turn_read_limit');
      case 'needs_confirmation':
        return t('ai_turn_confirm');
      case 'upstream_error':
        return t('ai_turn_upstream_error');
      case 'stopped':
        return t('ai_turn_stopped');
      default:
        return t('ai_turn_other', { reason: String(reason) });
    }
  };

  const turnReasonClass = (reason: AITurnEndReason): string => {
    if (reason === 'completed') return 'chip status-ok';
    if (
      reason === 'turn_budget' ||
      reason === 'repeat_call' ||
      reason === 'turn_timeout' ||
      reason === 'read_limit' ||
      reason === 'upstream_error'
    ) {
      return 'chip status-timeout';
    }
    return 'chip';
  };

  // --- catalog + history restore ---------------------------------------------

  useEffect(() => {
    let cancelled = false;
    setHistoryLoading(true);
    (async () => {
      let cat: AICatalog;
      try {
        cat = await api.getAICatalog();
      } catch (e) {
        if (!cancelled) {
          setCatalog(null);
          setCatalogErr(apiErrorMessage(e, t));
          setHistoryLoading(false);
        }
        return;
      }
      if (cancelled) return;
      setCatalog(cat);
      setCatalogErr(null);

      const options = usableModels(cat);
      const fallback = options.find((o) => pairIsDefault(cat, o.provider.id, o.model.id)) ?? options[0];

      // Mount-time replay: the stored session id is the only way a reload can
      // bring back the reasoning blocks (§12.6).
      const stored = readStoredAISession(nodeId);
      let history: AISessionHistory | null = null;
      if (stored) {
        try {
          history = await api.getAISession(stored);
        } catch {
          history = null;
          clearStoredAISession(nodeId);
        }
      }
      if (cancelled) return;

      let chosen = fallback;
      if (history) {
        // Only adopt the stored conversation when the pair it was pinned to is
        // still usable; continuing it under another model would be rejected
        // (409 ai_session_model_changed) or, worse, silently mismatch.
        const match = options.find((o) => o.provider.id === history?.provider_id && o.model.id === history?.model_id);
        if (match) chosen = match;
        else {
          history = null;
          clearStoredAISession(nodeId);
          setSessionNotice(t('ai_session_unavailable'));
        }
      }

      setProviderId(chosen?.provider.id ?? '');
      setModelId(chosen?.model.id ?? '');
      setIsServerDefault(chosen ? pairIsDefault(cat, chosen.provider.id, chosen.model.id) : false);
      setDefaultUnavailable(!fallback ? false : !pairIsDefault(cat, fallback.provider.id, fallback.model.id));
      if (history) {
        setAiSessionId(history.session_id);
        writeStoredAISession(nodeId, history.session_id);
        setEntries(historyToEntries(history));
      }
      setHistoryLoading(false);
      scrollToEnd(true);
    })().catch(() => {
      if (!cancelled) setHistoryLoading(false);
    });
    return () => {
      cancelled = true;
      // Leaving the page (or unmounting on node switch) must not leave a turn
      // running against the old node: aborts the SSE, the server persists the
      // half-turn and stops the loop (§12.6).
      abortIntentRef.current = 'discard';
      abortRef.current?.abort();
    };
    // `t` is intentionally not a dependency: a locale switch must not refetch
    // the catalog or drop the transcript that is already on screen.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodeId]);

  // --- streaming ---------------------------------------------------------------

  const appendDelta = (field: 'text' | 'thinking', delta: string) => {
    if (!assistantOpenRef.current) {
      assistantOpenRef.current = true;
      setEntries((prev) => [
        ...prev,
        { kind: 'assistant', text: field === 'text' ? delta : '', thinking: field === 'thinking' ? delta : '' },
      ]);
      return;
    }
    setEntries((prev) => {
      const next = [...prev];
      for (let i = next.length - 1; i >= 0; i--) {
        const entry = next[i];
        if (entry.kind === 'assistant') {
          next[i] =
            field === 'text'
              ? { ...entry, text: entry.text + delta }
              : { ...entry, thinking: entry.thinking + delta };
          break;
        }
      }
      return next;
    });
  };

  const streamHandlers = () => ({
    onSession: (event: { session_id: string }) => {
      if (!event.session_id) return;
      setAiSessionId(event.session_id);
      writeStoredAISession(nodeId, event.session_id);
    },
    onTextDelta: (text: string) => {
      appendDelta('text', text);
      scrollToEnd();
    },
    onThinkingDelta: (text: string) => {
      appendDelta('thinking', text);
      scrollToEnd();
    },
    onToolResult: (event: AIToolResultEvent) => {
      assistantOpenRef.current = false;
      setEntries((prev) => [
        ...prev,
        {
          kind: 'tool',
          name: event.name ?? '',
          status: event.status ?? '',
          command: event.command ?? '',
          exitCode: typeof event.exit_code === 'number' ? event.exit_code : null,
          reason: event.reason ?? '',
        },
      ]);
      scrollToEnd();
    },
    onNeedsConfirmation: (event: AINeedsConfirmationEvent) => {
      assistantOpenRef.current = false;
      setEntries((prev) => [
        ...prev,
        {
          kind: 'confirm',
          name: event.name ?? '',
          actionId: event.action_id,
          command: event.command ?? '',
          reason: event.reason ?? '',
          risk: event.risk ?? '',
        },
      ]);
      scrollToEnd();
    },
    onTurnEnd: (event: { reason: AITurnEndReason; changes?: number }) => {
      assistantOpenRef.current = false;
      setTurnEnd({ reason: event.reason, changes: event.changes });
      scrollToEnd();
    },
    onError: (event: { code: string; message?: string }) => {
      setErr(errText(event.code, event.message));
    },
  });

  /**
   * Shared tail of every streaming call: abort handling and busy bookkeeping.
   *
   * `onFailure` runs only for errors THROWN before/while opening the stream
   * (bad request, 409, network). A failure after the 200 arrives travels as an
   * `error` event instead, so a throw means the server never accepted the turn.
   */
  const runStream = async (
    controller: AbortController,
    start: () => Promise<void>,
    onFailure?: (e: unknown) => void,
  ): Promise<void> => {
    try {
      await start();
    } catch (e) {
      if (api.isAbortError(e)) {
        // The stop button is a normal outcome (§12.6: the committed half-turn
        // stays); a model switch already reset the panel, so stay silent.
        if (abortIntentRef.current === 'stop') setTurnEnd({ reason: 'stopped' });
        return;
      }
      setErr(apiErrorMessage(e, t));
      onFailure?.(e);
    } finally {
      // A newer stream may already own the ref (model switched mid-turn).
      if (abortRef.current === controller) {
        abortRef.current = null;
        abortIntentRef.current = null;
        setBusy(false);
      }
      scrollToEnd();
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const text = message.trim();
    if (!text || busy) return;
    setBusy(true);
    setErr(null);
    setTurnEnd(null);
    setSessionNotice(null);
    setMessage('');
    setEntries((prev) => [...prev, { kind: 'user', text }]);
    assistantOpenRef.current = false;
    scrollToEnd(true);

    const controller = new AbortController();
    abortRef.current = controller;
    abortIntentRef.current = null;

    const runTurn = (useSession: string | null) =>
      api.streamAIChat(
        {
          node_id: nodeId,
          // Re-sent on every request, not just when the session is created: the
          // id is minted per browser WS connection, so the value frozen on the
          // session row goes dangling the moment the page is refreshed
          // (§12.7.5). Omitted entirely while no terminal is connected.
          terminal_session_id: sessionId ?? undefined,
          message: text,
          session_id: useSession ?? undefined,
          provider_id: providerId || undefined,
          model_id: modelId || undefined,
          reasoning_level: reasoning || undefined,
        },
        streamHandlers(),
        controller.signal,
      );

    await runStream(
      controller,
      async () => {
        try {
          await runTurn(aiSessionId);
        } catch (e) {
          // A session pinned to another model cannot be continued (§12.5). The
          // server answers 409 before any delta, so retrying without the id is
          // safe and loses nothing already streamed.
          if (aiSessionId && e instanceof api.ApiError && e.code === 'ai_session_model_changed') {
            setAiSessionId(null);
            clearStoredAISession(nodeId);
            setSessionNotice(t('ai_session_restarted'));
            await runTurn(null);
            return;
          }
          throw e;
        }
      },
      () => {
        // Nothing was accepted server-side (the user message is persisted only
        // once the stream starts), so put the draft back instead of making the
        // operator retype it.
        setMessage((current) => (current === '' ? text : current));
      },
    );
  };

  const answerConfirmation = async (entry: Extract<ChatEntry, { kind: 'confirm' }>, approved: boolean) => {
    if (busy) return;
    setBusy(true);
    setErr(null);
    setTurnEnd(null);
    // Optimistic so a double click cannot send two decisions; reverted below if
    // the server never accepted it (409 ai_action_not_pending / gate refusal),
    // otherwise the card would claim a decision that never happened.
    setEntries((prev) =>
      prev.map((item) =>
        item.kind === 'confirm' && item.actionId === entry.actionId
          ? { ...item, answered: approved ? 'approved' : 'rejected' }
          : item,
      ),
    );
    assistantOpenRef.current = false;

    const controller = new AbortController();
    abortRef.current = controller;
    abortIntentRef.current = null;

    await runStream(
      controller,
      () =>
        api.streamAIContinue(
          {
            session_id: aiSessionId ?? undefined,
            action_id: entry.actionId,
            approved,
            // A resumed turn keeps keying into the same PTY, so it needs the
            // live terminal id for exactly the same reason as the first
            // request (§12.7.5).
            terminal_session_id: sessionId ?? undefined,
            reasoning_level: reasoning || undefined,
          },
          streamHandlers(),
          controller.signal,
        ),
      () => {
        setEntries((prev) =>
          prev.map((item) =>
            item.kind === 'confirm' && item.actionId === entry.actionId ? { ...item, answered: undefined } : item,
          ),
        );
      },
    );
  };

  const stop = () => {
    abortIntentRef.current = 'stop';
    abortRef.current?.abort();
  };

  // --- picker actions ----------------------------------------------------------

  const selectModel = (value: string) => {
    const next = decodePair(value);
    if (!next.providerId || !next.modelId || (next.providerId === providerId && next.modelId === modelId)) return;
    // Switching the model ENDS the conversation: reasoning blocks are bound to
    // the protocol that produced them (§12.5), so the history cannot be
    // replayed under another model. Abort whatever is in flight and start over.
    abortIntentRef.current = 'discard';
    abortRef.current?.abort();
    setEntries([]);
    setAiSessionId(null);
    clearStoredAISession(nodeId);
    setErr(null);
    setTurnEnd(null);
    setProviderId(next.providerId);
    setModelId(next.modelId);
    setReasoning('');
    setIsServerDefault(pairIsDefault(catalog, next.providerId, next.modelId));
    setDefaultUnavailable(false);
    setSessionNotice(t('ai_model_switched'));
  };

  const saveDefault = async () => {
    if (!providerId || !modelId || savingDefault) return;
    setSavingDefault(true);
    setErr(null);
    try {
      await api.setAIDefaults(providerId, modelId);
      setIsServerDefault(true);
      setDefaultUnavailable(false);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setSavingDefault(false);
    }
  };

  return (
    <section className="ai-panel card">
      <div className="terminal-head">
        <div>
          <h3>{t('ai_panel_title')}</h3>
          <p className="hint">{t('ai_panel_desc')}</p>
        </div>
        <span className="chip">{aiSessionId ? t('ai_session_active') : t('ai_session_new')}</span>
      </div>
      <div className="ai-context">
        <div><span className="hint">{t('terminal_node_label')}</span><span className="mono">{nodeId}</span></div>
        <div><span className="hint">{t('terminal_session_label')}</span><span className="mono">{sessionId || t('terminal_session_pending')}</span></div>
      </div>

      {/* Above the picker on purpose: the notice explains why the transcript is
          empty after a model switch, so it must be read before the controls. */}
      {sessionNotice && <p className="hint warn-hint">{sessionNotice}</p>}
      {defaultUnavailable && <p className="hint warn-hint">{t('ai_default_unavailable')}</p>}
      {catalogErr && <div className="form-error">{catalogErr}</div>}

      {/* The pickers live in the composer row below, next to the send button
          (§12.5): this block only reports the two states in which there is
          nothing to pick. */}
      {catalog === null
        ? catalogErr === null && <p className="hint">{t('ai_loading_models')}</p>
        : usable.length === 0 && <p className="hint">{t('ai_no_models')}</p>}

      <div className="ai-messages" ref={listRef}>
        {entries.length === 0 && (
          <p className="hint">{historyLoading ? t('ai_history_loading') : t('ai_panel_empty')}</p>
        )}
        {entries.map((entry, index) => {
          if (entry.kind === 'user') {
            return (
              <div key={index} className="ai-message ai-user">
                <span className="ai-role">{t('ai_role_user')}</span>
                <p className="ai-text">{entry.text}</p>
              </div>
            );
          }
          if (entry.kind === 'assistant') {
            return (
              <div key={index} className="ai-message ai-assistant">
                <span className="ai-role">{t('ai_role_assistant')}</span>
                {entry.thinking !== '' && (
                  // Collapsed by default (§12.5). <details> keeps its open state
                  // across the re-renders the deltas cause, so expanding it while
                  // the model is still thinking works.
                  <details className="ai-thinking">
                    <summary>{t('ai_thinking')}</summary>
                    <pre className="ai-thinking-body">{entry.thinking}</pre>
                  </details>
                )}
                {/* Omitted while only reasoning is streaming, so a thinking-first
                    turn does not show a blank paragraph above the text. */}
                {entry.text !== '' && <p className="ai-text">{entry.text}</p>}
              </div>
            );
          }
          if (entry.kind === 'tool') {
            return (
              <div key={index} className="ai-tool-card">
                <div className="ai-tool-head">
                  <span className="mono ai-tool-name">{entry.name || t('unknown')}</span>
                  {entry.status && <span className={`chip ${toolStatusClass(entry.status)}`}>{toolStatusText(entry.status)}</span>}
                </div>
                {entry.command !== '' && <pre className="code-block small">{entry.command}</pre>}
                {entry.command !== '' && (
                  <span className="hint">
                    {typeof entry.exitCode === 'number' ? t('ai_exit_code', { code: entry.exitCode }) : t('ai_exit_unknown')}
                  </span>
                )}
                {entry.reason !== '' && <span className="hint">{t('ai_cmd_reason')}: {entry.reason}</span>}
              </div>
            );
          }
          if (entry.kind === 'turn_end') {
            return (
              <div key={index} className="ai-turn-end">
                <span className="hint">{t('ai_turn_end_label')}</span>
                <span className={turnReasonClass(entry.reason as AITurnEndReason)}>
                  {turnReasonText(entry.reason as AITurnEndReason)}
                </span>
                {entry.changes > 0 && <span className="hint">{t('ai_turn_changes', { n: entry.changes })}</span>}
              </div>
            );
          }
          if (entry.kind === 'tool_log') {
            return (
              <div key={index} className="ai-message ai-tool-history">
                <span className="ai-role">{t('ai_tool_result')}</span>
                <pre className="code-block small">{entry.text}</pre>
              </div>
            );
          }
          return (
            <div key={index} className="ai-confirm">
              <div className="ai-tool-head">
                <strong>{t('ai_confirm_title')}</strong>
                {entry.risk !== '' && <span className="chip status-failed">{entry.risk}</span>}
              </div>
              {entry.name !== '' && <span className="hint mono">{entry.name}</span>}
              {entry.reason !== '' && <p className="hint">{t('ai_cmd_reason')}: {entry.reason}</p>}
              {/* The full command, never truncated: judging it is the operator's
                  only job at this point (§12.3). */}
              {entry.command !== '' && <pre className="code-block small">{entry.command}</pre>}
              {entry.answered ? (
                <span className={`chip ${entry.answered === 'approved' ? 'status-ok' : ''}`}>
                  {entry.answered === 'approved' ? t('ai_confirmed') : t('ai_rejected')}
                </span>
              ) : (
                <div className="row-wrap">
                  <button
                    type="button"
                    className="btn primary small"
                    disabled={busy}
                    onClick={() => void answerConfirmation(entry, true)}
                  >
                    {t('ai_confirm')}
                  </button>
                  <button
                    type="button"
                    className="btn small"
                    disabled={busy}
                    onClick={() => void answerConfirmation(entry, false)}
                  >
                    {t('ai_confirm_reject')}
                  </button>
                </div>
              )}
            </div>
          );
        })}
        {busy && <p className="hint">{t('ai_working')}</p>}
      </div>

      {turnEnd && (
        <div className="ai-turn-end">
          {/* The reason is always shown: a turn cut off by the change budget or
              the 10-minute deadline must not read as a finished answer. */}
          <span className={turnReasonClass(turnEnd.reason)}>{turnReasonText(turnEnd.reason)}</span>
          {typeof turnEnd.changes === 'number' && turnEnd.changes > 0 && (
            <span className="hint">{t('ai_turn_changes', { n: turnEnd.changes })}</span>
          )}
        </div>
      )}
      {err && <div className="form-error">{err}</div>}
      <form className="ai-composer" onSubmit={(event) => void submit(event)}>
        <textarea
          value={message}
          rows={3}
          placeholder={t('ai_input_placeholder')}
          onChange={(event) => setMessage(event.target.value)}
        />
        {/* One control bar: the model and thinking-level pickers sit on the
            same line as the button that starts the turn (§12.5).

            Sending and stopping are ONE button, because they are never both
            wanted: while a turn streams there is nothing to send, and while it
            is idle there is nothing to stop. Rendering both — one of them
            always disabled — spends the row on a control that cannot be used,
            which is exactly the room the two pickers now need. The `type` flips
            with the role so the busy state can never submit the draft the
            operator is still editing. */}
        <div className="ai-composer-row">
          {usable.length > 0 && (
            <>
              <label className="ai-select-field">
                <span>{t('ai_model_label')}</span>
                <select
                  value={encodePair(providerId, modelId)}
                  // The panel column is narrow enough to clip a long model id
                  // (§12.5), and this is the one value the operator must be able
                  // to read back before the turn runs.
                  title={selected?.model.display_name || modelId}
                  onChange={(event) => selectModel(event.target.value)}
                >
                  {groups.map((group) => (
                    <optgroup key={group.provider.id} label={group.provider.name}>
                      {group.models.map((model) => (
                        <option key={model.id} value={encodePair(group.provider.id, model.id)}>
                          {model.display_name || model.id}
                        </option>
                      ))}
                    </optgroup>
                  ))}
                </select>
              </label>
              <label className="ai-select-field ai-level-field">
                <span>{t('ai_reasoning_label')}</span>
                {levels.length === 0 ? (
                  // Not an empty dropdown: the honest statement is that this
                  // model exposes no adjustable level through this provider.
                  <span className="hint ai-level-none">{t('ai_reasoning_none')}</span>
                ) : (
                  <select value={reasoning} onChange={(event) => setReasoning(event.target.value)}>
                    <option value="">{t('ai_reasoning_default')}</option>
                    {levels.map((level) => (
                      <option key={level} value={level}>{level}</option>
                    ))}
                  </select>
                )}
              </label>
            </>
          )}
          <button
            type={busy ? 'button' : 'submit'}
            className={busy ? 'btn danger' : 'btn primary'}
            disabled={!busy && !message.trim()}
            onClick={busy ? stop : undefined}
          >
            {busy ? t('ai_stop') : t('ai_send')}
          </button>
          {usable.length > 0 && (
            // Last in the row on purpose: it is the only item here that can
            // wrap onto a line of its own on a narrow panel, and it is the one
            // that matters least while writing a message.
            <button
              type="button"
              className="btn small"
              disabled={savingDefault || isServerDefault}
              onClick={() => void saveDefault()}
            >
              {savingDefault ? t('loading') : isServerDefault ? t('ai_default_is_current') : t('ai_set_default')}
            </button>
          )}
        </div>
      </form>
    </section>
  );
}

export default function TerminalPage() {
  const { id = '' } = useParams();
  const { t } = useI18n();
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [aiReady, setAiReady] = useState(false);

  // The assistant only has something to talk to once base_url / api_key /
  // model are all set (§12.1); otherwise every submit would come back 503
  // ai_not_configured. The server's derived ai_configured flag decides whether
  // the column exists at all — the in-flight window and any failure stay
  // hidden rather than flashing a panel that cannot send.
  useEffect(() => {
    let cancelled = false;
    api
      .getSettings()
      .then((r) => {
        if (!cancelled) setAiReady(r.ai_configured === true);
      })
      .catch(() => {
        if (!cancelled) setAiReady(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!id) {
    return (
      <div className="card error-card">
        <p>{t('err_not_found')}</p>
        <Link to="/" className="btn">{t('back_to_overview')}</Link>
      </div>
    );
  }

  return (
    <div className="terminal-page stack-lg">
      {/* No page-level title: the terminal card's own head row carries
          "Agent Web 终端" plus the session and connection state, so a page
          heading would just repeat it one line above. */}
      <Link to={`/nodes/${encodeURIComponent(id)}`} className="back-link back-link-row">← {t('back_to_node')}</Link>
      <div className={`terminal-workspace${aiReady ? '' : ' solo'}`}>
        <Terminal nodeId={id} onSessionChange={setSessionId} />
        {/* keyed by node: switching targets mounts a fresh panel, so the old
            node's transcript and session id never leak into the new one. */}
        {aiReady && (
          <AssistantPanel
            key={id}
            nodeId={id}
            sessionId={sessionId}
          />
        )}
      </div>
    </div>
  );
}
