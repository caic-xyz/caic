// Adapts shared browser voice to caic task updates and conversation numbering.

import { createEffect, onCleanup, Show, untrack, type Accessor } from "solid-js";
import type { Task } from "@sdk/types.gen";
import SharedBrowserVoiceShell from "@maruel/gomode/web/BrowserVoiceShell";
import type { ServiceAuthorization } from "@maruel/gomode/sdk/voicegateway/types.gen.ts";
import type { voiceSession } from "@maruel/gomode/web/VoiceSession";
import { useAppState } from "./AppState";
import { TaskNumberMap } from "./TaskNumberMap";
import { setVoiceConnected, setVoiceTaskNumberMap } from "./voiceTaskState";
import { buildTaskCIContext, buildTaskStateContext } from "./voiceTaskContext";

export default function BrowserVoiceShell() {
  const s = useAppState();
  return (
    <Show when={s.voiceGatewayAvailable()}>
      <SharedBrowserVoiceShell
        service="caic"
        toolGroup="tasks"
        clientName="caic-frontend"
        preparation={s.frontendPreparation}
        authorize={authorizeVoice}
        setupFailed="Voice gateway setup failed. Reload the page to retry."
        onError={(error) => console.error("Could not configure caic voice gateway", error)}
      >
        {({ session }) => <VoiceTaskUpdates tasks={s.tasks} session={session} />}
      </SharedBrowserVoiceShell>
    </Show>
  );
}

export function VoiceTaskUpdates(props: { tasks: Accessor<Task[]>; session: typeof voiceSession }) {
  const numberMap = new TaskNumberMap();
  let prevStates = new Map<string, string>();
  let prevCIStatuses = new Map<string, string | undefined>();
  let wasConnected = false;

  // Seed current items at connection, without replaying them as new changes.
  createEffect(() => {
    const connected = props.session.state.connected;
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
    if (props.session.state.connected) {
      setVoiceTaskNumberMap(numberMap);
      const numbered = currentTasks.length > 1;
      for (const task of currentTasks) {
        const prev = prevStates.get(task.id);
        const taskNumber = numberMap.toNumber(task.id);
        if (prev !== undefined && prev !== task.state && taskNumber !== undefined) {
          const notification = buildTaskStateContext(task, taskNumber, numbered);
          if (notification !== null) props.session.injectText(notification);
        }
        const prevCI = prevCIStatuses.get(task.id);
        if (prevCI !== undefined && prevCI !== "failure" && task.ciStatus === "failure" && taskNumber !== undefined) {
          props.session.injectText(buildTaskCIContext(task, taskNumber, numbered));
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

async function authorizeVoice(endpoint: string): Promise<ServiceAuthorization> {
  const response = await fetch(endpoint, { credentials: "same-origin", cache: "no-store" });
  if (!response.ok) throw new Error(`Voice token request returned HTTP ${response.status}`);
  return (await response.json()) as ServiceAuthorization;
}
