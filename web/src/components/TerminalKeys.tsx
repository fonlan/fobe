import { useI18n } from '../i18n';
import type { NamedKey, StickyModifier, StickyModifiers } from './terminalKeyBytes';

/**
 * On-screen key bar under the terminal screen (design §11, 2026-09-21).
 *
 * Why it exists: a phone keyboard has no Ctrl, Alt or Esc, so `Ctrl+C` — the one
 * keystroke that stops a runaway command — was unreachable on mobile, and the
 * same holds for `Esc` (vim), `Tab` (completion) and the arrows (history). The
 * bar turns those into taps. It stays on desktop too: a click on ^C is a
 * perfectly good interrupt, and the bar is the only place that makes it obvious
 * which combinations the terminal understands.
 *
 * The bar is purely presentational: it names a key or a modifier and lets
 * Terminal.tsx (which owns the xterm instance and the PTY data path) decide the
 * bytes — see terminalKeys.ts. Two rules it must not break:
 *   • pointerdown is prevented for the whole bar, so a tap cannot move focus out
 *     of xterm's hidden textarea — otherwise the soft keyboard would close on
 *     every tap and typing would need a second tap to come back;
 *   • an armed modifier is ONE-SHOT. It is released by the next key it rides on,
 *     so a forgotten Ctrl cannot turn the rest of the session into control
 *     characters (typing `ls` into a shell that still thinks Ctrl is down puts
 *     `^L` on screen).
 */
export interface TerminalKeysProps {
  /** Which modifiers are currently armed (one-shot, see above). */
  armed: StickyModifiers;
  onToggle: (modifier: StickyModifier) => void;
  onNamedKey: (key: NamedKey) => void;
  /** A control character by letter: 'c' means ^C, not a literal `c`. */
  onControlKey: (letter: string) => void;
  /** Nothing is sent while the PTY is not connected — taps would be silent no-ops. */
  disabled: boolean;
}

const MODIFIERS: { id: StickyModifier; label: string }[] = [
  { id: 'ctrl', label: 'Ctrl' },
  { id: 'alt', label: 'Alt' },
];

/** The everyday control characters, in the order a shell makes you want them. */
const CONTROL_KEYS: { letter: string; label: string }[] = [
  { letter: 'c', label: '^C' },
  { letter: 'd', label: '^D' },
  { letter: 'z', label: '^Z' },
  { letter: 'l', label: '^L' },
];

const NAMED_KEYS: { id: NamedKey; label: string }[] = [
  { id: 'escape', label: 'Esc' },
  { id: 'tab', label: 'Tab' },
  { id: 'left', label: '←' },
  { id: 'up', label: '↑' },
  { id: 'down', label: '↓' },
  { id: 'right', label: '→' },
];

export default function TerminalKeys({
  armed,
  onToggle,
  onNamedKey,
  onControlKey,
  disabled,
}: TerminalKeysProps) {
  const { t } = useI18n();
  const armedLabel = [armed.ctrl ? 'Ctrl' : '', armed.alt ? 'Alt' : ''].filter(Boolean).join('+');

  return (
    <div
      className="terminal-keys"
      role="toolbar"
      aria-label={t('terminal_keys_label')}
      title={disabled ? t('terminal_keys_disconnected') : t('terminal_keys_hint')}
      // Keeps focus (and the phone's soft keyboard) on xterm's textarea; a
      // button that steals focus costs the operator a second tap per key.
      onPointerDown={(event) => event.preventDefault()}
    >
      {MODIFIERS.map((modifier) => (
        <button
          key={modifier.id}
          type="button"
          className={`key-chip modifier${armed[modifier.id] ? ' armed' : ''}`}
          aria-pressed={armed[modifier.id]}
          disabled={disabled}
          onClick={() => onToggle(modifier.id)}
        >
          {modifier.label}
        </button>
      ))}
      <span className="key-sep" aria-hidden="true" />
      {NAMED_KEYS.map((named) => (
        <button
          key={named.id}
          type="button"
          className="key-chip mono"
          disabled={disabled}
          onClick={() => onNamedKey(named.id)}
        >
          {named.label}
        </button>
      ))}
      <span className="key-sep" aria-hidden="true" />
      {CONTROL_KEYS.map((control) => (
        <button
          key={control.letter}
          type="button"
          className="key-chip mono ctrl-key"
          // The only keystroke that gets its own tooltip: on a phone this button
          // IS the interrupt, and "which one stops the command" is the question
          // the bar was built to answer.
          title={control.letter === 'c' ? t('terminal_key_intr') : undefined}
          disabled={disabled}
          onClick={() => onControlKey(control.letter)}
        >
          {control.label}
        </button>
      ))}
      {armedLabel && <span className="key-armed hint">{t('terminal_keys_armed', { keys: armedLabel })}</span>}
    </div>
  );
}
