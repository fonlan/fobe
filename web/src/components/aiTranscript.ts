// The assistant transcript model: the flat entry list the panel keeps, and the
// pure functions that turn it into bubbles (design §12.6).
//
// Nothing here touches React. The list is flat because the loop can interleave
// text → tool → text → tool inside ONE user message, and a nested model would
// have to invent a grouping the server never sends (a tool result carries no
// "which assistant turn asked for it" field). The grouping into per-turn
// bubbles happens at render time, where it stays a pure function of the list.

import type { AISessionHistory } from '../types';

/** One line of the transcript, as the panel keeps it. */
export type ChatEntry =
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

/**
 * A transcript entry the assistant produced — everything but a user message.
 *
 * The exclusion is a real invariant, not a convenience: `groupTurns` hands every
 * user entry to its own bubble, so a turn body can never contain one, and saying
 * so here is what lets the renderer treat a leftover entry as a confirmation.
 */
export type AssistantEntry = Exclude<ChatEntry, { kind: 'user' }>;

/**
 * One rendered bubble: the operator's message, or the assistant's whole turn.
 *
 * One bubble per turn is the point. A turn that reads a log, runs a command and
 * then explains itself is ONE answer, and drawing its three steps as three
 * bubbles made the panel read like three agents had spoken — the "which reply
 * belongs to which question" question was left to the operator. Everything the
 * model said and did between two user messages therefore shares one body.
 */
export interface Turn {
  /**
   * The user message that opened this turn. `null` for a run with no visible
   * prompt — a resumed conversation whose first stored row is an assistant or
   * tool row has no user message to hang the run on, and inventing an empty
   * bubble for it would look like a rendering bug.
   */
  user: Extract<ChatEntry, { kind: 'user' }> | null;
  /** Everything the assistant said and did in reply, in emission order. */
  body: AssistantEntry[];
}

/**
 * Group the flat entry list into bubbles.
 *
 * Every user entry opens a bubble; every non-user entry belongs to the turn
 * opened by the user entry above it. Leading non-user entries (history replayed
 * from the middle of a session) form one promptless turn.
 */
export function groupTurns(entries: readonly ChatEntry[]): Turn[] {
  const turns: Turn[] = [];
  for (const entry of entries) {
    if (entry.kind === 'user') {
      turns.push({ user: entry, body: [] });
      continue;
    }
    const current = turns[turns.length - 1];
    if (current) current.body.push(entry);
    else turns.push({ user: null, body: [entry] });
  }
  return turns;
}

/**
 * Turn a stored transcript into bubbles. Assistant rows with neither text nor
 * thinking are dropped: those are the pure tool-call turns the envelope stores,
 * and an empty bubble would look like a rendering bug.
 */
export function historyToEntries(history: AISessionHistory): ChatEntry[] {
  const out: ChatEntry[] = [];
  for (const message of history.messages ?? []) {
    if (message.role === 'user') {
      out.push({ kind: 'user', text: message.content });
    } else if (message.role === 'assistant') {
      const thinking = message.thinking ?? '';
      if (message.content === '' && thinking === '') continue;
      out.push({ kind: 'assistant', text: message.content, thinking });
    } else if (message.role === 'turn_end') {
      const reason = message.turn_reason ?? '';
      // A natural completion needs no banner — the answer itself is the
      // completion, and replaying "turn completed" under it is pure noise.
      // Abnormal endings (budget / timeout / upstream error…) still replay so
      // a reloaded conversation explains why the answer looks cut short.
      if (reason === 'completed') continue;
      out.push({ kind: 'turn_end', reason, changes: message.changes ?? 0 });
    } else if (message.role === 'tool') {
      // The stored shape keeps only the model-facing text (the envelope's
      // tool_results carry call ids, not names/commands), so this renders as a
      // plain result block rather than pretending to be a tool card.
      out.push({ kind: 'tool_log', text: message.content });
    }
  }
  return out;
}
