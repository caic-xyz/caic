// Incremental task timing derivation: associates completed turns with user-response waits.

import type { EventMessage, EventResult } from "@sdk/types.gen";

export interface TurnTiming {
  event: EventMessage;
  result: EventResult;
  waitMs: number | null;
}

export interface TaskTimings {
  previousEventTs: ReadonlyMap<EventMessage, number>;
  turns: TurnTiming[];
  userWaitMs: ReadonlyMap<EventMessage, number>;
}

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

// IncrementalTaskTimingTracker folds append-only event batches without
// rescanning retained task history. Call reset before replacing the timeline.
export class IncrementalTaskTimingTracker {
  private previousEventTs = new Map<EventMessage, number>();
  private turns: TurnTiming[] = [];
  private userWaitMs = new Map<EventMessage, number>();
  private previousTs = 0;
  private processed = 0;
  private waitingTurn: TurnTiming | null = null;

  derive(messages: readonly EventMessage[], reset: boolean): TaskTimings {
    if (reset || messages.length < this.processed) this.clear();
    for (let i = this.processed; i < messages.length; i++) this.append(messages[i]);
    this.processed = messages.length;
    return {
      previousEventTs: this.previousEventTs,
      turns: this.turns,
      userWaitMs: this.userWaitMs,
    };
  }

  private append(event: EventMessage) {
    if (isVisualTimelineEvent(event) && event.ts > 0) {
      if (this.previousTs > 0 && event.ts >= this.previousTs) {
        this.previousEventTs.set(event, this.previousTs);
      }
      this.previousTs = event.ts;
    }
    if (event.kind === "result" && event.result) {
      const turn = { event, result: event.result, waitMs: null } satisfies TurnTiming;
      this.turns.push(turn);
      this.waitingTurn = turn;
      return;
    }
    if (event.kind !== "userInput" || this.waitingTurn === null) return;

    const waitMs = event.ts - this.waitingTurn.event.ts;
    if (this.waitingTurn.event.ts > 0 && waitMs > 0) {
      this.waitingTurn.waitMs = waitMs;
      this.userWaitMs.set(event, waitMs);
    }
    this.waitingTurn = null;
  }

  private clear() {
    this.previousEventTs = new Map<EventMessage, number>();
    this.turns = [];
    this.userWaitMs = new Map<EventMessage, number>();
    this.previousTs = 0;
    this.processed = 0;
    this.waitingTurn = null;
  }
}

// deriveTaskTimings derives one immutable-history timing snapshot.
export function deriveTaskTimings(messages: readonly EventMessage[]): TaskTimings {
  return new IncrementalTaskTimingTracker().derive(messages, false);
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
    case "diffStat":
    case "init":
    case "log":
    case "stats":
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
