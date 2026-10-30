// Builds section summaries for component HTTP fixtures.
import type { EventMessage } from "@sdk/types.gen";
import {
  groupTurns,
  aggregateToolGroups,
  groupMessages,
  isChildEvent,
  isSessionBoundary,
  updateWarningPercentages,
  type Turn,
} from "../src/grouping";
// A session is a segment of the event stream opened by an init, compaction, or clear event.
// Session boundaries are init events (new Claude Code session) and compaction/clear system
// events. The current (last) session is never elided.
export interface Session {
  // The event that opened this session (init, compaction, or clear system event).
  // undefined for an implicit initial segment before the first session event.
  boundaryEvent?: EventMessage;
  turns: Turn[];
  toolCount: number;
  textCount: number;
  // Sum of turn runtimes, excluding time between a result and the next user input.
  durationMs: number;
}

// SessionGrouper consumes root events once and reuses contexts closed by a
// compaction, clear, or changed session identity. Structural changes regroup
// only the active context so cross-turn tool and message identities still join.
export class SessionGrouper {
  private closed: Session[] = [];
  private segment: EventMessage[] = [];
  private boundary: EventMessage | null = null;
  private sessionID: string | undefined;
  private lastResult = -1;
  private active: Session | null = null;
  private activeLength = -1;
  private result: Session[] = [];
  private warnings = new Map<string, number>();

  get pastSessions(): Session[] {
    return this.closed;
  }

  get warningPercentages(): ReadonlyMap<string, number> {
    return this.warnings;
  }

  append(events: readonly EventMessage[]): Session[] {
    let changed = false;
    for (const event of events) {
      updateWarningPercentages(this.warnings, event);
      if (event.kind === "init" && !isChildEvent(event)) {
        const id = event.init?.sessionID;
        if (id !== this.sessionID) {
          const openContext = this.boundary?.kind === "system" && this.lastResult < 0;
          if (!openContext) {
            if (this.boundary !== null) this.close();
            this.boundary = event;
            changed = true;
          }
          this.sessionID = id;
        }
      } else if (event.kind === "system" && isSessionBoundary(event)) {
        if (this.boundary !== null) this.close();
        this.boundary = event;
        this.sessionID = undefined;
        changed = true;
      } else {
        this.segment.push(event);
        if (event.kind === "result" && !isChildEvent(event)) this.lastResult = this.segment.length - 1;
        changed = true;
      }
    }
    if (changed) {
      // A changed boundary or segment always leaves a current context.
      this.result = [...this.closed, this.current()];
    }
    return this.result;
  }

  private close(): void {
    const carry = this.lastResult >= 0 ? this.segment.splice(this.lastResult + 1) : [];
    this.closed = [...this.closed, this.current()];
    this.segment = carry;
    this.boundary = null;
    this.lastResult = -1;
    this.active = null;
    this.activeLength = -1;
  }

  private current(): Session {
    if (
      this.active !== null &&
      this.activeLength === this.segment.length &&
      this.active.boundaryEvent === (this.boundary ?? undefined)
    )
      return this.active;
    const turns = groupTurns(aggregateToolGroups(groupMessages(this.segment)));
    let toolCount = 0;
    let textCount = 0;
    let durationMs = 0;
    for (const turn of turns) {
      toolCount += turn.toolCount;
      textCount += turn.textCount;
      durationMs += turn.durationMs;
    }
    this.activeLength = this.segment.length;
    this.active = { boundaryEvent: this.boundary ?? undefined, turns, toolCount, textCount, durationMs };
    return this.active;
  }
}
