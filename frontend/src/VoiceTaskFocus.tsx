// Registers frontend task tools and publishes their account-owned initial data readiness.

import { createEffect, on, onCleanup, type Accessor } from "solid-js";
import { registerFrontendVoiceTool } from "@maruel/gomode/web/FrontendVoiceTools";
import { useHostMode } from "@maruel/gomode/web/HostMode";
import { TaskNumberMap } from "./TaskNumberMap";
import { useAppState } from "./AppState";
import {
  setFocusedVoiceTask,
  voiceConnected,
  getVoiceTaskId,
  setVoiceConnected,
  setVoiceTaskNumberMap,
} from "./voiceTaskState";

export default function VoiceTaskFocus(props: { mobile: Accessor<boolean> }) {
  const s = useAppState();
  const hostMode = useHostMode();
  const nativeTaskNumberMap = new TaskNumberMap();

  // Android owns the voice session in host mode. Mirror its connection state
  // and retain the same active-first numbering as the native voice prompt.
  createEffect(() => {
    if (!hostMode.isGoModeHost()) return;
    const connected = hostMode.nativeVoiceConnected();
    setVoiceConnected(connected);
    if (!connected) {
      setVoiceTaskNumberMap(null);
      return;
    }
    nativeTaskNumberMap.update(s.tasks());
    setVoiceTaskNumberMap(nativeTaskNumberMap);
  });
  onCleanup(() => {
    if (!hostMode.isGoModeHost()) return;
    setVoiceConnected(false);
    setVoiceTaskNumberMap(null);
  });

  function focusTask(id: string) {
    if (voiceConnected()) setFocusedVoiceTask(id);
    if (props.mobile() && voiceConnected()) s.navigate("/");
    else s.navigateToTask(id);
  }

  createEffect(
    on(
      s.taskFocusRequest,
      (id) => {
        if (id !== null) focusTask(id);
      },
      { defer: true },
    ),
  );

  const unregister = registerFrontendVoiceTool({
    declaration: {
      name: "focus_task",
      description:
        "Select a task when switching conversational focus without calling a task-scoped service tool. " +
        "Task-scoped service tools automatically select their target task. " +
        "Use the task number or stable task ID from the task context or service tools. This selects the task in the user's frontend.",
      parameters: {
        type: "object",
        properties: { task_number: { oneOf: [{ type: "integer", minimum: 1 }, { type: "string" }] } },
        required: ["task_number"],
        additionalProperties: false,
      },
    },
    execute(args) {
      const ref = args.task_number;
      const number =
        typeof ref === "number"
          ? ref
          : typeof ref === "string" && /^#?[1-9]\d*$/.test(ref)
            ? Number(ref.replace(/^#/, ""))
            : null;
      const id = number !== null ? getVoiceTaskId(number) : typeof ref === "string" ? ref : undefined;
      if (!voiceConnected() || !id || !s.tasks().some((task) => task.id === id)) {
        throw new Error("Unknown task reference or disconnected voice session. Read the current tasks and try again.");
      }
      focusTask(id);
      return { task_id: id };
    },
  });

  createEffect(() => {
    s.frontendPreparation?.setReady(s.initialTasksReady());
  });

  createEffect(() => {
    // A service focus event may precede its task's upsert. Membership removals
    // clear focus in AppState; absence from the current list is not a removal.
    if (!voiceConnected()) {
      setFocusedVoiceTask(null);
    }
  });

  onCleanup(() => {
    s.frontendPreparation?.setReady(false);
    unregister();
    setFocusedVoiceTask(null);
  });
  return null;
}
