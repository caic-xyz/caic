// Caic browser voice integration: task updates and numbering around the generic Go Mode voice overlay.

import { createEffect, onCleanup, Show, untrack, type Accessor } from "solid-js";
import type { Task } from "@sdk/types.gen";

import { useAppState } from "./AppState";
import { TaskNumberMap } from "./TaskNumberMap";
import { useHostMode } from "./gomode/HostMode";
import VoiceOverlay from "./gomode/VoiceOverlay";
import { voiceSession } from "./gomode/VoiceSession";
import { setVoiceConnected, setVoiceTaskNumberMap } from "./voiceTaskState";
import { buildTaskCIContext, buildTaskStateContext } from "./voiceTaskContext";

/** Browser-owned shell features that Android Go Mode owns natively in host mode. */
export default function BrowserVoiceShell() {
  const s = useAppState();
  const hostMode = useHostMode();

  return (
    <Show when={hostMode.browserVoiceEnabled() && s.voiceGatewayAvailable()}>
      <VoiceTaskUpdates tasks={s.tasks} />
      <VoiceOverlay />
    </Show>
  );
}

export function VoiceTaskUpdates(props: { tasks: Accessor<Task[]> }) {
  const numberMap = new TaskNumberMap();
  let prevStates = new Map<string, string>();
  let prevCIStatuses = new Map<string, string | undefined>();
  let wasConnected = false;

  // Seed current items at connection, without replaying them as new changes.
  createEffect(() => {
    const connected = voiceSession.state.connected;
    setVoiceConnected(connected);
    if (connected && !wasConnected) {
      const tasks = untrack(() => props.tasks());
      setVoiceTaskNumberMap(numberMap);
      prevStates = new Map(tasks.map((t) => [t.id, t.state]));
      prevCIStatuses = new Map(tasks.map((t) => [t.id, t.ciStatus]));
    }
    wasConnected = connected;
  });

  createEffect(() => {
    const currentTasks = props.tasks();
    numberMap.update(currentTasks);
    if (voiceSession.state.connected) {
      setVoiceTaskNumberMap(numberMap);
      for (const task of currentTasks) {
        const prev = prevStates.get(task.id);
        const taskNumber = numberMap.toNumber(task.id);
        if (prev !== undefined && prev !== task.state && taskNumber !== undefined) {
          const notification = buildTaskStateContext(task, taskNumber);
          if (notification !== null) voiceSession.injectText(notification);
        }
        const prevCI = prevCIStatuses.get(task.id);
        if (prevCI !== undefined && prevCI !== "failure" && task.ciStatus === "failure" && taskNumber !== undefined) {
          voiceSession.injectText(buildTaskCIContext(task, taskNumber));
        }
      }
    }
    prevStates = new Map(currentTasks.map((t) => [t.id, t.state]));
    prevCIStatuses = new Map(currentTasks.map((t) => [t.id, t.ciStatus]));
  });

  onCleanup(() => {
    setVoiceConnected(false);
    setVoiceTaskNumberMap(null);
  });
  return null;
}
