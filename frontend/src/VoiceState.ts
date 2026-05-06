// Shared voice session state — set by VoiceOverlay, read by task list/cards.
import { createSignal } from "solid-js";
import type { TaskNumberMap } from "./TaskNumberMap";

/** Whether a Gemini Live voice session is currently connected. */
export const [voiceConnected, setVoiceConnected] = createSignal(false);

let taskNumberMap: TaskNumberMap | null = null;

/** Called by VoiceOverlay when the session starts/ends with the active TaskNumberMap. */
export function setVoiceTaskNumberMap(map: TaskNumberMap | null): void {
  taskNumberMap = map;
}

/** Returns the voice-mode task number for the given ID, or undefined if not connected/not mapped. */
export function getVoiceTaskNumber(id: string): number | undefined {
  if (!voiceConnected()) return undefined;
  return taskNumberMap?.toNumber(id);
}
