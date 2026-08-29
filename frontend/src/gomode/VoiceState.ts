// Shared voice session state — set by VoiceOverlay, read by task list/cards.

import { createSignal } from "solid-js";

import type { TaskNumberMap } from "../TaskNumberMap";

/** Whether a voice gateway session is currently connected. */
export const [voiceConnected, setVoiceConnected] = createSignal(false);

const [taskNumberMap, setTaskNumberMap] = createSignal<TaskNumberMap | null>(null, { equals: false });

/** Publishes the active map after every voice task-number synchronization. */
export function setVoiceTaskNumberMap(map: TaskNumberMap | null): void {
  setTaskNumberMap(map);
}

/** Returns the voice-mode task number for the given ID, or undefined if not connected/not mapped. */
export function getVoiceTaskNumber(id: string): number | undefined {
  if (!voiceConnected()) return undefined;
  return taskNumberMap()?.toNumber(id);
}
