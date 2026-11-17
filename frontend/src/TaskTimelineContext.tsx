// Owns active conversation history and transcript links across its persistent workspace subviews.
import { createContext, useContext, type JSX } from "solid-js";
import { createTaskEventTimeline } from "./taskEventTimeline";
import { createTranscriptLinks, type TranscriptLinks } from "./transcriptLinks";

const TaskTimelineContext = createContext<ReturnType<typeof createTaskEventTimeline>>();
const TranscriptLinksContext = createContext<TranscriptLinks>();

export function TaskTimelineProvider(props: { taskId: string; children: JSX.Element }) {
  const timeline = createTaskEventTimeline({
    taskId: () => props.taskId,
  });
  return (
    <TaskTimelineContext.Provider value={timeline}>
      <TranscriptLinksContext.Provider value={createTranscriptLinks()}>
        {props.children}
      </TranscriptLinksContext.Provider>
    </TaskTimelineContext.Provider>
  );
}

export function useTaskTimeline() {
  const timeline = useContext(TaskTimelineContext);
  if (!timeline) throw new Error("Task timeline requires a task workspace");
  return timeline;
}

// useTranscriptLinks connects the conversation and the turn view of one task.
export function useTranscriptLinks(): TranscriptLinks {
  const links = useContext(TranscriptLinksContext);
  if (!links) throw new Error("Transcript links require a task workspace");
  return links;
}
