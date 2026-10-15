// Shares one task event timeline between the conversation and its workspace subviews.
import { createContext, useContext, type JSX } from "solid-js";
import { createTaskEventTimeline } from "./taskEventTimeline";

const TaskTimelineContext = createContext<ReturnType<typeof createTaskEventTimeline>>();

export function TaskTimelineProvider(props: {
  taskId: string;
  taskState: string;
  onError: (message: string) => void;
  children: JSX.Element;
}) {
  const timeline = createTaskEventTimeline({
    taskId: () => props.taskId,
    taskState: () => props.taskState,
    onError: (message) => props.onError(message),
  });
  return <TaskTimelineContext.Provider value={timeline}>{props.children}</TaskTimelineContext.Provider>;
}

export function useTaskTimeline() {
  const timeline = useContext(TaskTimelineContext);
  if (!timeline) throw new Error("Task timeline requires a task workspace");
  return timeline;
}
