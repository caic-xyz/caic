// Turn time is the agent's time: a turn's wall time without the time it only waited for the user's answers to its questions.

import type { HistoryRunningTurn, HistoryTurn } from "@sdk/types.gen";

export function turnTimeMs(turn: Pick<HistoryTurn, "durationMs" | "askMs">): number {
  return Math.max(0, turn.durationMs - (turn.askMs ?? 0));
}

// runningAskMs is how long a running turn has only waited for answers once
// elapsedMs of wall time has passed since its start: its past waits and the
// current one.
export function runningAskMs(turn: HistoryRunningTurn, elapsedMs: number): number {
  const open = turn.askingSince
    ? Math.max(0, Date.parse(turn.startedAt) + elapsedMs - Date.parse(turn.askingSince))
    : 0;
  return (turn.askMs ?? 0) + open;
}

// runningTurnTimeMs stops growing while the turn only waits for an answer.
export function runningTurnTimeMs(turn: HistoryRunningTurn, elapsedMs: number): number {
  return Math.max(0, elapsedMs - runningAskMs(turn, elapsedMs));
}
