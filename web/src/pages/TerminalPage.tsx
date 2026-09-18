import {
  Fragment,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type FormEvent,
  type KeyboardEvent,
  type PointerEvent,
} from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import Terminal from '../components/Terminal';
import { Markdown } from '../components/Markdown';
import { groupTurns, historyToEntries, type ChatEntry } from '../components/aiTranscript';
import type {
  AICatalog,
  AINeedsConfirmationEvent,
  AIProvider,
  AIProviderModel,
  AISessionHistory,
  AIToolResultEvent,
  AITurnEndReason,
} from '../types';

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
  // True when (default_provider_id, default_model_id) was NOT selectable and the
  // picker fell back to the first usable pair. Silent substitution would make
  // the operator believe a different model is answering than the one configured.
  const [defaultUnavailable, setDefaultUnavailable] = useState(false);
  const [sessionNotice, setSessionNotice] = useState<string | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // Why the in-flight turn was aborted: the stop button must report itself, a
  // model switch must stay silent (it already reset the panel).
  const abortIntentRef = useRef<'stop' | 'discard' | null>(null);
  // Whether an assistant bubble is open for the CURRENT loop step. A tool result
  // closes it, so text emitted after a tool runs opens a fresh bubble instead of
  // being appended above the tool card.
  const assistantOpenRef = useRef(false);

  // Auto bottom-lock. A ref, not state: the value is read inside rAF callbacks
  // and inside the scroll handler, where a render-time value would be stale —
  // and toggling it must never re-render the transcript on every scroll frame.
  const stickToBottomRef = useRef(true);

  const scrollToEnd = (force = false) => {
    const el = listRef.current;
    if (!el) return;
    if (force) stickToBottomRef.current = true;
    if (!stickToBottomRef.current) return;
    window.requestAnimationFrame(() => {
      const node = listRef.current;
      if (!node || !stickToBottomRef.current) return;
      node.scrollTop = node.scrollHeight;
    });
  };

  // Re-arm the lock the moment the operator scrolls back to the bottom, and
  // release it as soon as he scrolls up to read back through the transcript —
  // yanking the viewport away from someone reading is worse than a missed
  // auto-follow. 48px absorbs fractional scroll positions and focus jumps.
  const handleMessagesScroll = () => {
    const el = listRef.current;
    if (!el) return;
    stickToBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight <= 48;
  };

  // The single trigger for the lock. Bubbles arrive as React state, so scrolling
  // from the stream handlers would run BEFORE the new bubble is in the DOM and
  // measure the previous scrollHeight; an effect runs after the commit, when the
  // list knows its real height. `busy` is a dependency because the "AI 思考中…"
  // line is itself a bubble that appears and disappears.
  useEffect(() => {
    scrollToEnd();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [entries, turnEnd, busy]);

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
    },
    onThinkingDelta: (text: string) => {
      appendDelta('thinking', text);
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
    },
    onTurnEnd: (event: { reason: AITurnEndReason; changes?: number }) => {
      assistantOpenRef.current = false;
      setTurnEnd({ reason: event.reason, changes: event.changes });
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

  const send = async (): Promise<void> => {
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

  const submit = (event: FormEvent) => {
    event.preventDefault();
    void send();
  };

  // Enter sends, Shift+Enter is a newline. `isComposing` guards the IME path:
  // confirming a candidate in a Chinese/Japanese input method fires a keydown
  // Enter that must commit the composition, not submit the draft — without the
  // guard every IME confirmation would fire the message off half-typed.
  const onComposerKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    void send();
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
    setDefaultUnavailable(false);
    setSessionNotice(t('ai_model_switched'));
  };

  /**
   * Start a brand-new conversation (§12.6): drop the transcript and forget the
   * stored session id, so the next send mints a fresh session. The picker
   * selection survives — "new conversation" is not "switch model".
   *
   * A turn still streaming is aborted first: its deltas would otherwise append
   * to the new, empty transcript and read as the model answering a question
   * nobody asked. `discard` keeps that abort silent (no `stopped` turn_end);
   * the half-turn the server already committed stays in the old session, which
   * is exactly what clearing the transcript means.
   */
  const startNewSession = () => {
    if (entries.length > 0 && !window.confirm(t('ai_new_session_confirm'))) return;
    abortIntentRef.current = 'discard';
    abortRef.current?.abort();
    assistantOpenRef.current = false;
    setEntries([]);
    setAiSessionId(null);
    clearStoredAISession(nodeId);
    setErr(null);
    setTurnEnd(null);
    setSessionNotice(null);
  };

  // Bubbles, not entries: one assistant bubble per turn (§12.6 UI). The
  // regrouping is a pure function of the flat list, so the streaming path keeps
  // appending to the same entries and the open bubble simply grows.
  const turns = useMemo(() => groupTurns(entries), [entries]);

  return (
    <section className="ai-panel card">
      <div className="terminal-head">
        <div>
          <h3>{t('ai_panel_title')}</h3>
          <p className="hint">{t('ai_panel_desc')}</p>
        </div>
        {/* The one per-session control. The "session continuing / new session"
            chip this replaces only reported state the transcript already shows,
            while starting over had no button at all. */}
        <button
          type="button"
          className="btn small"
          title={t('ai_new_session')}
          aria-label={t('ai_new_session')}
          disabled={aiSessionId === null && entries.length === 0}
          onClick={startNewSession}
        >
          +
        </button>
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

      <div className="ai-messages" ref={listRef} onScroll={handleMessagesScroll}>
        {entries.length === 0 && (
          <p className="hint">{historyLoading ? t('ai_history_loading') : t('ai_panel_empty')}</p>
        )}
        {turns.map((turn, turnIndex) => {
          // The live turn is the last one, and the only bubble that may have to
          // exist before the model has said anything: a message that was just
          // sent shows "thinking" inside the assistant bubble it is about to
          // fill, not as a stray hint underneath it.
          const streaming = busy && turnIndex === turns.length - 1;
          // One bubble per turn (§12.6): every step the model took between this
          // message and the next one shares it, so a read → run → explain turn
          // reads as one answer instead of three.
          return (
            <div className="ai-turn" key={turnIndex}>
              {turn.user && (
                <div className="ai-message ai-user">
                  <span className="ai-role">{t('ai_role_user')}</span>
                  {/* Plain text, not Markdown: the operator typed a sentence, and
                      parsing it as Markdown would quietly reformat whatever
                      punctuation it happens to contain. */}
                  <p className="ai-text">{turn.user.text}</p>
                </div>
              )}
              {(turn.body.length > 0 || streaming) && (
                <div className="ai-message ai-assistant">
                  <span className="ai-role">{t('ai_role_assistant')}</span>
                  {turn.body.map((entry, index) => {
                    if (entry.kind === 'assistant') {
                      return (
                        <Fragment key={index}>
                          {entry.thinking !== '' && (
                            // Collapsed by default (§12.5). <details> keeps its
                            // open state across the re-renders the deltas cause,
                            // so expanding it while the model is still thinking
                            // works.
                            <details className="ai-thinking">
                              <summary>{t('ai_thinking')}</summary>
                              <pre className="ai-thinking-body">{entry.thinking}</pre>
                            </details>
                          )}
                          {/* Omitted while only reasoning is streaming, so a
                              thinking-first turn does not show a blank paragraph
                              above the text. Markdown is re-parsed on every
                              delta, so the answer reads formatted as it arrives. */}
                          {entry.text !== '' && <Markdown text={entry.text} />}
                        </Fragment>
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
                      // No .ai-message wrapper: this now sits INSIDE the assistant
                      // bubble, and a card drawn inside a card reads as two
                      // different things.
                      return (
                        <div key={index} className="ai-tool-history">
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
                        {/* The full command, never truncated: judging it is the
                            operator's only job at this point (§12.3). */}
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
                  {streaming && <p className="hint ai-working">{t('ai_working')}</p>}
                </div>
              )}
            </div>
          );
        })}
      </div>

      {/* Only abnormal endings get a banner (§12.6): a turn cut off by the
          change budget or the 10-minute deadline must not read as a finished
          answer. A `completed` turn already ends with the answer itself, so a
          "turn completed" chip under it is noise. */}
      {turnEnd && turnEnd.reason !== 'completed' && (
        <div className="ai-turn-end">
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
          onKeyDown={onComposerKeyDown}
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
        </div>
      </form>
    </section>
  );
}

// --- workspace split -----------------------------------------------------------

// The terminal/AI split is a per-operator preference, not per-node state: it
// lives in localStorage and survives reloads and node switches. The bounds keep
// both columns usable — below 25% the assistant cannot fit its composer row,
// above 75% the terminal is a narrow sliver.
const RATIO_KEY = 'fobe.terminal.ratio';
const RATIO_MIN = 0.25;
const RATIO_MAX = 0.75;
const RATIO_DEFAULT = 0.6;

function loadRatio(): number {
  try {
    const v = Number(localStorage.getItem(RATIO_KEY));
    if (Number.isFinite(v) && v >= RATIO_MIN && v <= RATIO_MAX) return v;
  } catch {
    // storage unavailable: the default split still works, it just won't stick
  }
  return RATIO_DEFAULT;
}

function clampRatio(v: number): number {
  return Math.min(RATIO_MAX, Math.max(RATIO_MIN, v));
}

export default function TerminalPage() {
  const { id = '' } = useParams();
  const { t } = useI18n();
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [aiReady, setAiReady] = useState(false);
  const [ratio, setRatio] = useState<number>(loadRatio);
  const [dragging, setDragging] = useState(false);
  const workspaceRef = useRef<HTMLDivElement | null>(null);
  // Mirror of `ratio` for the drag-end handlers: pointerup fires from a render
  // whose state may lag the last pointermove, and localStorage must record the
  // split the operator actually left on screen.
  const ratioRef = useRef<number>(ratio);

  const applyRatio = (value: number) => {
    const next = clampRatio(value);
    ratioRef.current = next;
    setRatio(next);
  };

  const onDividerDown = (event: PointerEvent<HTMLDivElement>) => {
    event.preventDefault();
    // Pointer capture keeps the drag alive when the cursor leaves the 10px
    // handle — without it a fast drag "drops" the handle mid-move.
    event.currentTarget.setPointerCapture(event.pointerId);
    setDragging(true);
  };

  const onDividerMove = (event: PointerEvent<HTMLDivElement>) => {
    if (!dragging) return;
    const el = workspaceRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    if (rect.width <= 0) return;
    applyRatio((event.clientX - rect.left) / rect.width);
  };

  // Shared by pointerup AND pointercancel: a touch drag can be interrupted by
  // the OS at any moment, and the split must settle + persist either way.
  // releasePointerCapture throws when capture was already lost — that is fine,
  // the drag just ends.
  const endDrag = (event: PointerEvent<HTMLDivElement>) => {
    if (!dragging) return;
    try {
      event.currentTarget.releasePointerCapture(event.pointerId);
    } catch {
      // capture already gone
    }
    setDragging(false);
    try {
      localStorage.setItem(RATIO_KEY, String(ratioRef.current));
    } catch {
      // storage unavailable: non-fatal, the split just resets on reload
    }
  };

  const resetRatio = () => {
    applyRatio(RATIO_DEFAULT);
    try {
      localStorage.removeItem(RATIO_KEY);
    } catch {
      // storage unavailable
    }
  };

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

  // Rounding keeps 1-0.7 from leaking float noise (0.30000000000000004fr is
  // valid CSS, just embarrassing).
  const restRatio = Math.round((1 - ratio) * 1000) / 1000;

  return (
    <div className="terminal-page stack-lg">
      {/* No page-level title: the terminal card's own head row carries
          "Agent Web 终端" plus the session and connection state, so a page
          heading would just repeat it one line above. */}
      <Link to={`/nodes/${encodeURIComponent(id)}`} className="back-link back-link-row">← {t('back_to_node')}</Link>
      {/* The split travels as two literal `Nfr` tokens rather than an inline
          grid-template-columns (the ≤900px single-column media query must be
          able to win) and rather than calc(<number> * 1fr), which some browsers
          drop wholesale — a dead declaration stacks the two panes vertically. */}
      <div
        ref={workspaceRef}
        className={`terminal-workspace${aiReady ? '' : ' solo'}${dragging ? ' dragging' : ''}`}
        style={{ '--tw-col': `${ratio}fr`, '--tw-rest': `${restRatio}fr` } as CSSProperties}
      >
        <Terminal nodeId={id} onSessionChange={setSessionId} />
        {/* keyed by node: switching targets mounts a fresh panel, so the old
            node's transcript and session id never leak into the new one. */}
        {aiReady && (
          <>
            <div
              className="terminal-divider"
              role="separator"
              aria-orientation="vertical"
              title={t('terminal_drag_hint')}
              onPointerDown={onDividerDown}
              onPointerMove={onDividerMove}
              onPointerUp={endDrag}
              onPointerCancel={endDrag}
              onDoubleClick={resetRatio}
            />
            <AssistantPanel
              key={id}
              nodeId={id}
              sessionId={sessionId}
            />
          </>
        )}
      </div>
    </div>
  );
}
