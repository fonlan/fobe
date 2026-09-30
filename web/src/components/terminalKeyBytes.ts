/**
 * Byte mapping for the terminal's on-screen key bar (design §11, 2026-09-21).
 *
 * A phone keyboard has no Ctrl, Alt or Esc key, so the Web terminal grows a key
 * bar (components/TerminalKeys.tsx) whose taps are injected as `terminal_input`
 * frames — indistinguishable from the operator's own typing, no server-side
 * execution involved. This module is the pure half of that feature: it decides
 * WHICH bytes a tap means, so the mapping can be reasoned about (and tested)
 * without a DOM or a live PTY.
 *
 * Two entry points, one rule — a tap must be byte-identical to the physical key
 * it stands for (xterm's own `evaluateKeyboardEvent` is the reference, so
 * anything that works when typed by hand also works when tapped):
 *   • `namedKeyBytes` — Esc/Tab/arrows, whose answer depends on the shell's
 *     DECSET 1 (application cursor keys) state and on the armed modifiers;
 *   • `applyStickyModifiers` — rewrites the operator's OWN keystrokes (xterm's
 *     onData) while a modifier is armed. That is the path which makes "arm Ctrl,
 *     then press c" work on a soft keyboard as well: phones deliver the letter
 *     through onData no matter whether their key events carry a usable keyCode
 *     (Android reports 229 for every key).
 */

/** The modifiers the key bar can arm. Shift is deliberately absent: soft
 *  keyboards already provide it, and no terminal sequence the bar emits needs
 *  shift alone (`Shift+Tab` is the only one, and it is not on the bar). */
export type StickyModifier = 'ctrl' | 'alt';

export interface StickyModifiers {
  ctrl: boolean;
  alt: boolean;
}

export const NO_MODIFIERS: StickyModifiers = { ctrl: false, alt: false };

/** Special keys the bar can send by name (they have no character of their own). */
export type NamedKey = 'escape' | 'tab' | 'up' | 'down' | 'left' | 'right';

const ESC = '\x1b';

/** CSI final byte per arrow; Home/End would be 'H'/'F' if the bar ever grows them. */
const CSI_FINAL: Record<Exclude<NamedKey, 'escape' | 'tab'>, string> = {
  up: 'A',
  down: 'B',
  right: 'C',
  left: 'D',
};

/**
 * xterm's own parameter for "key pressed with modifiers": 1 + shift(1) + alt(2)
 * + ctrl(4). The bar has no shift, but the formula is written out so the value
 * stays recognizable to anyone comparing it with xterm's source.
 */
export function modifierParameter(modifiers: StickyModifiers): number {
  return 1 + (modifiers.alt ? 2 : 0) + (modifiers.ctrl ? 4 : 0);
}

/**
 * Bytes for a named key. `applicationCursorKeys` is the shell's DECSET 1 state:
 * full-screen programs (vim, less, htop) ask for the SS3 form (`ESC O A`) and
 * would misread the CSI form, so the bar has to ask xterm which mode is on.
 */
export function namedKeyBytes(
  key: NamedKey,
  modifiers: StickyModifiers,
  applicationCursorKeys: boolean,
): string {
  if (key === 'escape') return modifiers.alt ? ESC + ESC : ESC;
  // Tab is Tab: Ctrl/Alt+Tab belong to the window manager everywhere, and no
  // shell or readline binding asks for a modified one.
  if (key === 'tab') return '\x09';
  const final = CSI_FINAL[key];
  const parameter = modifierParameter(modifiers);
  if (parameter === 1) return applicationCursorKeys ? `${ESC}O${final}` : `${ESC}[${final}`;
  // A modified arrow is always CSI: the SS3 form carries no parameter, so
  // application mode does not apply once a modifier is involved (same as xterm).
  return `${ESC}[1;${parameter}${final}`;
}

/**
 * Ctrl/Alt applied to one printable character, mirroring xterm's control-code
 * mapping exactly (`Ctrl+C` → 0x03, `Ctrl+[` → ESC, `Alt+x` → `ESC x`). Getting
 * this right matters: the byte decides whether the shell sees an interrupt, a
 * literal ESC, or a Meta word-jump.
 */
export function modifiedChar(ch: string, modifiers: StickyModifiers): string {
  let out = ch;
  if (modifiers.ctrl) {
    const lower = ch.toLowerCase();
    const code = lower.charCodeAt(0);
    if (code >= 0x61 && code <= 0x7a) out = String.fromCharCode(code - 0x60); // ^A..^Z
    else if (ch === ' ') out = '\x00';
    else if (ch === '[') out = ESC;
    else if (ch === '\\') out = '\x1c';
    else if (ch === ']') out = '\x1d';
    else if (ch === '^') out = '\x1e';
    else if (ch === '_' || ch === '@') out = ch === '_' ? '\x1f' : '\x00';
    else if (code >= 0x33 && code <= 0x37) out = String.fromCharCode(code - 0x33 + 0x1b); // ^3..^7
    else if (code === 0x38) out = '\x7f'; // ^8 = DEL
  }
  return modifiers.alt ? ESC + out : out;
}

/**
 * Rewrite one chunk of the operator's own input while a modifier is armed.
 *
 * Only two shapes are recognised, and everything else passes through untouched
 * (then the modifier is released by the caller): a single printable character —
 * the phone path, where the soft keyboard delivers the letter through onData —
 * and a bare CSI arrow, so arming Ctrl turns a physical arrow into the modified
 * one the bar would have sent.
 */
export function applyStickyModifiers(data: string, modifiers: StickyModifiers): string {
  if (!modifiers.ctrl && !modifiers.alt) return data;
  if (data.length === 1) {
    const code = data.charCodeAt(0);
    // Printable only: a control byte (Tab, Esc, Enter) is already the terminal's
    // own key, and Ctrl+Enter is plain Enter — never rewrite those.
    if (code >= 0x20 && code < 0x7f) return modifiedChar(data, modifiers);
    return modifiers.alt ? ESC + data : data;
  }
  const arrow = /^\x1b\[([ABCD])$/.exec(data);
  if (arrow) {
    const parameter = modifierParameter(modifiers);
    return parameter === 1 ? data : `${ESC}[1;${parameter}${arrow[1]}`;
  }
  return data;
}
