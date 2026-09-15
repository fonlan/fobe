import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import Terminal, { type TerminalHandle } from '../components/Terminal';
import type { AIToolCall, CommandRow } from '../types';

interface ChatEntry {
  role: 'user' | 'assistant';
  text: string;
  tool?: AIToolCall;
}

function AssistantPanel({
  nodeId,
  sessionId,
  onEcho,
}: {
  nodeId: string;
  sessionId: string | null;
  onEcho: (text: string) => void;
}) {
  const { t } = useI18n();
  const [entries, setEntries] = useState<ChatEntry[]>([]);
  const [aiSessionId, setAiSessionId] = useState<string | null>(null);
  const [message, setMessage] = useState('');
  const [includeLogs, setIncludeLogs] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  const scrollToEnd = () => {
    window.requestAnimationFrame(() => {
      listRef.current?.scrollTo({ top: listRef.current.scrollHeight });
    });
  };

  // Poll the node's command history until the AI-issued command finishes, so
  // the panel (and the terminal echo) show the real execution outcome.
  const awaitCommandResult = async (commandId: string): Promise<CommandRow | null> => {
    for (let attempt = 0; attempt < 15; attempt++) {
      try {
        const commands = await api.listCommandsNormalized(nodeId);
        const cmd = commands.find((c) => c.id === commandId);
        if (cmd && cmd.status !== 'pending' && cmd.status !== 'sent') return cmd;
      } catch {
        // transient: keep polling until the budget runs out
      }
      await new Promise((resolve) => window.setTimeout(resolve, 1000));
    }
    return null;
  };

  const echoCommand = (command: string) => {
    onEcho(`\r\n\x1b[36m[AI] $ ${command}\x1b[0m\r\n`);
  };

  const commandOf = (tool: AIToolCall): string => tool.arguments?.command ?? '';
  const reasonOf = (tool: AIToolCall): string => tool.reason ?? tool.arguments?.reason ?? '';

  const confirm = async (tool: AIToolCall) => {
    if (!tool.action_id) return;
    setBusy(true);
    setErr(null);
    try {
      const result = await api.confirmAIAction(tool.action_id);
      tool.status = 'queued';
      tool.command_id = result.command_id;
      setEntries((prev) => [...prev]);
      const command = commandOf(tool);
      if (command) echoCommand(command);
      const finished = await awaitCommandResult(result.command_id);
      if (finished) {
        onEcho(`\x1b[2m${t('ai_exit_code', { code: exitCodeOf(finished) })}\x1b[0m\r\n`);
        setEntries((prev) => {
          const next = [...prev];
          const last = next[next.length - 1];
          if (last?.tool) last.tool = { ...last.tool, status: `done:${exitCodeOf(finished)}` };
          return next;
        });
      }
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const text = message.trim();
    if (!text || busy) return;
    setBusy(true);
    setErr(null);
    setMessage('');
    setEntries((prev) => [...prev, { role: 'user', text }]);
    scrollToEnd();
    try {
      const reply = await api.chatWithAI({
        node_id: nodeId,
        terminal_session_id: sessionId ?? undefined,
        message: text,
        include_logs: includeLogs,
        session_id: aiSessionId ?? undefined,
      });
      if (reply.session_id) setAiSessionId(reply.session_id);
      const tool = reply.tool_call;
      setEntries((prev) => [...prev, { role: 'assistant', text: reply.message, tool }]);
      scrollToEnd();
      const queuedCommand = tool?.status === 'queued' ? commandOf(tool) : '';
      if (queuedCommand) {
        echoCommand(queuedCommand);
        if (tool?.command_id) {
          const finished = await awaitCommandResult(tool.command_id);
          if (finished) {
            onEcho(`\x1b[2m${t('ai_exit_code', { code: exitCodeOf(finished) })}\x1b[0m\r\n`);
          }
        }
      }
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    } finally {
      setBusy(false);
      scrollToEnd();
    }
  };

  const toolLabel = (tool: AIToolCall): string => {
    switch (tool.status) {
      case 'queued':
        return t('ai_cmd_queued');
      case 'needs_confirmation':
        return t('ai_cmd_needs_confirmation');
      case 'blocked':
        return t('ai_cmd_blocked');
      case 'invalid':
        return t('ai_cmd_invalid');
      default:
        if (tool.status?.startsWith('done:')) return t('ai_cmd_done', { code: tool.status.slice(5) });
        return tool.status ?? '';
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
      <div className="ai-messages" ref={listRef}>
        {entries.length === 0 && (
          <p className="hint">{t('ai_panel_empty')}</p>
        )}
        {entries.map((entry, index) => (
          <div key={index} className={`ai-message ai-${entry.role}`}>
            <span className="ai-role">{entry.role === 'user' ? t('ai_role_user') : t('ai_role_assistant')}</span>
            <p className="ai-text">{entry.text}</p>
            {entry.tool && (
              <div className="ai-tool">
                <span className="hint mono">{toolLabel(entry.tool)}</span>
                {commandOf(entry.tool) && <pre className="code-block">{commandOf(entry.tool)}</pre>}
                {reasonOf(entry.tool) && <p className="hint">{t('ai_cmd_reason')}: {reasonOf(entry.tool)}</p>}
                {entry.tool.status === 'needs_confirmation' && (
                  <button type="button" className="btn primary small" disabled={busy} onClick={() => void confirm(entry.tool!)}>
                    {t('ai_confirm')}
                  </button>
                )}
              </div>
            )}
          </div>
        ))}
        {busy && <p className="hint">{t('ai_working')}</p>}
      </div>
      {err && <div className="form-error">{err}</div>}
      <form className="ai-composer" onSubmit={(event) => void submit(event)}>
        <textarea
          value={message}
          rows={3}
          placeholder={t('ai_input_placeholder')}
          onChange={(event) => setMessage(event.target.value)}
        />
        <div className="row-between">
          <label className="check-chip">
            <input type="checkbox" checked={includeLogs} onChange={(event) => setIncludeLogs(event.target.checked)} />
            {t('ai_include_logs')}
          </label>
          <button type="submit" className="btn primary" disabled={busy || !message.trim()}>
            {t('ai_send')}
          </button>
        </div>
      </form>
    </section>
  );
}

function exitCodeOf(cmd: CommandRow): string {
  if (cmd.status === 'ok') return '0';
  try {
    const parsed = JSON.parse(cmd.result) as { exit_code?: number };
    if (typeof parsed.exit_code === 'number') return String(parsed.exit_code);
  } catch {
    // result is not JSON; fall through to the status string
  }
  return cmd.status;
}

export default function TerminalPage() {
  const { id = '' } = useParams();
  const { t } = useI18n();
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [aiReady, setAiReady] = useState(false);
  const termRef = useRef<TerminalHandle | null>(null);

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
        <Terminal
          nodeId={id}
          onSessionChange={setSessionId}
          onReady={(handle) => { termRef.current = handle; }}
        />
        {aiReady && <AssistantPanel nodeId={id} sessionId={sessionId} onEcho={(text) => termRef.current?.write(text)} />}
      </div>
    </div>
  );
}
