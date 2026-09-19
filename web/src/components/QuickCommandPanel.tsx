import { Link } from 'react-router-dom';
import { useI18n } from '../i18n';
import type { TerminalHandle } from './Terminal';
import type { QuickCommand } from '../types';

/**
 * 常用命令 tab of the terminal page's right column (2026-09-19).
 *
 * Clicking a command injects it into the left terminal as keyboard input:
 *   - single-line → the command plus `\r`, i.e. typed and executed;
 *   - multi-line → wrapped in bracketed-paste sequences with NO trailing
 *     newline, so the block lands editable on the command line and the
 *     operator presses Enter to run it. When the shell has not enabled
 *     bracketed paste (busybox ash never does), the only honest alternative
 *     is raw text — whose newlines WOULD execute line-by-line — so the button
 *     is disabled with a hint instead (grill-me 定稿).
 */
export default function QuickCommandPanel({
  commands,
  handle,
  connected,
}: {
  commands: QuickCommand[];
  handle: TerminalHandle | null;
  connected: boolean;
}) {
  const { t } = useI18n();

  const run = (command: QuickCommand) => {
    if (!handle || !connected) return;
    const multi = command.command.includes('\n');
    if (multi && !handle.bracketedPaste()) return;
    // \r, not \n: a real Enter is CR, and the PTY's icrnl turns it into the
    // line feed readline wants. Inside bracketed paste the trailing CR is what
    // keeps the pasted block unexecuted (it sits before the end marker).
    const data = multi ? `\x1b[200~${command.command}\x1b[201~` : `${command.command}\r`;
    handle.sendInput(data);
    handle.focus();
  };

  const disabledReason = (command: QuickCommand): string | undefined => {
    if (!connected) return t('qc_panel_disconnected');
    if (command.command.includes('\n') && !(handle?.bracketedPaste() ?? false)) {
      return t('qc_paste_unavailable');
    }
    return undefined;
  };

  return (
    <div className="qc-panel">
      {commands.length === 0 ? (
        <div className="empty-hint">
          <p>{t('qc_panel_empty')}</p>
          <Link to="/settings/commands" className="btn small">
            {t('qc_panel_go_settings')}
          </Link>
        </div>
      ) : (
        <div className="qc-list">
          {commands.map((command) => {
            const disabled = !connected || (!!handle && command.command.includes('\n') && !handle.bracketedPaste());
            const reason = disabledReason(command);
            return (
              <button
                key={command.id}
                type="button"
                className="btn qc-btn"
                disabled={disabled}
                title={reason ?? command.command}
                onClick={() => run(command)}
              >
                <span className="qc-btn-name">{command.name}</span>
                <span className="qc-btn-preview mono">
                  {firstLine(command.command)}
                  {command.command.includes('\n') ? ' …' : ''}
                </span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

/** The preview shows the first line only; the full command rides on title. */
function firstLine(command: string): string {
  const idx = command.indexOf('\n');
  return idx >= 0 ? command.slice(0, idx) : command;
}
