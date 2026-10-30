// Tracks preceding visual events and bounded block spans within a loaded context.

import type { EventMessage } from "@sdk/types.gen";
import { isChildEvent } from "./grouping";

export interface EventTimingSummary {
  range: { start: number; end: number } | null;
  firstTimedEvent: EventMessage | null;
  resultEvent: EventMessage | null;
  inputEvent: EventMessage | null;
}

// IncrementalEventTimingTracker summarizes an append-only visual block without
// rescanning streaming deltas already present in the block.
export class IncrementalEventTimingTracker {
  private processed = 0;
  private lastProcessed: EventMessage | null = null;
  private start = Number.POSITIVE_INFINITY;
  private end = 0;
  private firstTimedEvent: EventMessage | null = null;
  private resultEvent: EventMessage | null = null;
  private inputEvent: EventMessage | null = null;

  derive(events: readonly EventMessage[]): EventTimingSummary {
    if (events.length < this.processed || (this.processed > 0 && events[this.processed - 1] !== this.lastProcessed)) {
      this.clear();
    }
    for (let i = this.processed; i < events.length; i++) {
      const event = events[i];
      if (!event) throw new Error("Missing timeline event");
      if (event.ts > 0) {
        if (event.ts < this.start) this.firstTimedEvent = event;
        this.start = Math.min(this.start, event.ts);
        this.end = Math.max(this.end, event.ts);
      }
      if (this.resultEvent === null && event.kind === "result") this.resultEvent = event;
      if (this.inputEvent === null && event.kind === "userInput") this.inputEvent = event;
    }
    this.processed = events.length;
    this.lastProcessed = events.at(-1) ?? null;
    return {
      range: Number.isFinite(this.start) ? { start: this.start, end: this.end } : null,
      firstTimedEvent: this.firstTimedEvent,
      resultEvent: this.resultEvent,
      inputEvent: this.inputEvent,
    };
  }

  private clear() {
    this.processed = 0;
    this.lastProcessed = null;
    this.start = Number.POSITIVE_INFINITY;
    this.end = 0;
    this.firstTimedEvent = null;
    this.resultEvent = null;
    this.inputEvent = null;
  }
}

// VisualTimingTracker processes only appended visual events in one loaded context.
export class VisualTimingTracker {
  private previous = new Map<EventMessage, number>();
  private previousTs = 0;
  private processed = 0;
  derive(messages: readonly EventMessage[], reset: boolean, previousTs: number): ReadonlyMap<EventMessage, number> {
    if (reset || messages.length < this.processed) {
      this.previous = new Map();
      this.previousTs = previousTs;
      this.processed = 0;
    }
    if (this.processed === 0) this.previousTs = previousTs;
    for (let i = this.processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing context event");
      if (isChildEvent(event) || !isVisualTimelineEvent(event) || event.ts <= 0) continue;
      if (this.previousTs > 0 && event.ts >= this.previousTs) this.previous.set(event, this.previousTs);
      this.previousTs = event.ts;
    }
    this.processed = messages.length;
    return this.previous;
  }
}

function isVisualTimelineEvent(event: EventMessage): boolean {
  switch (event.kind) {
    case "ask":
    case "error":
    case "rateLimit":
    case "result":
    case "text":
    case "textDelta":
    case "thinking":
    case "thinkingDelta":
    case "toolOutputDelta":
    case "toolResult":
    case "toolUse":
    case "usage":
    case "userInput":
    case "widget":
    case "widgetDelta":
      return true;
    case "commitSnapshot":
    case "diffStat":
    case "init":
    case "log":
    case "nativeSubagent":
    case "backgroundCommand":
    case "skillLoad":
    case "subagentEnd":
    case "subagentStart":
    case "system":
    case "todo":
      return false;
  }
}

export function formatTimingDuration(ms: number): string {
  if (ms < 500) return `${Math.round(ms)}ms`;

  const seconds = Math.round(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  const remainingSeconds = String(seconds % 60).padStart(2, "0");
  if (minutes < 60) return `${minutes}:${remainingSeconds}`;

  const hours = Math.floor(minutes / 60);
  const remainingMinutes = String(minutes % 60).padStart(2, "0");
  return `${hours}:${remainingMinutes}:${remainingSeconds}`;
}
