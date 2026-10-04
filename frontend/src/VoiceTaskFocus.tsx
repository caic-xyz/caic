// Frontend voice task focus and native task-number synchronization across all routes.

import { createEffect, onCleanup, type Accessor } from "solid-js";
import { registerFrontendVoiceTool } from "@maruel/gomode/web/FrontendVoiceTools";
import { useHostMode } from "@maruel/gomode/web/HostMode";
import { TaskNumberMap } from "./TaskNumberMap";
import { useAppState } from "./AppState";
import {
  focusedVoiceTask,
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

  const unregister = registerFrontendVoiceTool({
    declaration: {
      name: "focus_task",
      description:
        "Call whenever you switch your conversational focus to a specific task, before discussing it or acting on it. " +
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
      setFocusedVoiceTask(id);
      if (props.mobile()) s.navigate("/");
      else s.navigateToTask(id);
      return { task_id: id };
    },
  });

  createEffect(() => {
    const id = focusedVoiceTask();
    if (!voiceConnected() || (id !== null && !s.tasks().some((task) => task.id === id))) {
      setFocusedVoiceTask(null);
    }
  });

  onCleanup(() => {
    unregister();
    setFocusedVoiceTask(null);
  });
  return null;
}
