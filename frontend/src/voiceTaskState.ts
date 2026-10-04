// Caic task numbering and conversational focus for browser and native voice sessions.

import { createSignal } from "solid-js";

import type { TaskNumberMap } from "./TaskNumberMap";

/** Whether a voice gateway session is currently connected. */
export const [voiceConnected, setVoiceConnected] = createSignal(false);

export const [focusedVoiceTask, setFocusedVoiceTask] = createSignal<string | null>(null);

const [taskNumberMap, setTaskNumberMap] = createSignal<TaskNumberMap | null>(null, {
  equals: false,
});

/** Publishes the active map after every voice task-number synchronization. */
export function setVoiceTaskNumberMap(map: TaskNumberMap | null): void {
  setTaskNumberMap(map);
}

/** Returns the voice-mode task number for the given ID, or undefined if not connected/not mapped. */
export function getVoiceTaskNumber(id: string): number | undefined {
  if (!voiceConnected()) return undefined;
  return taskNumberMap()?.toNumber(id);
}

/** Resolves a voice task number against the current session map. */
export function getVoiceTaskId(number: number): string | undefined {
  if (!voiceConnected()) return undefined;
  return taskNumberMap()?.toId(number);
}
