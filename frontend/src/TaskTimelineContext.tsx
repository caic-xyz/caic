// Owns active conversation history across its persistent workspace subviews.
import { createContext, useContext, type JSX } from "solid-js";
import { createTaskEventTimeline } from "./taskEventTimeline";

const TaskTimelineContext = createContext<ReturnType<typeof createTaskEventTimeline>>();

export function TaskTimelineProvider(props: { taskId: string; children: JSX.Element }) {
  const timeline = createTaskEventTimeline({
    taskId: () => props.taskId,
  });
  return <TaskTimelineContext.Provider value={timeline}>{props.children}</TaskTimelineContext.Provider>;
}

export function useTaskTimeline() {
  const timeline = useContext(TaskTimelineContext);
  if (!timeline) throw new Error("Task timeline requires a task workspace");
  return timeline;
}
