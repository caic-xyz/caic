// Projects canonical history into stable completed contexts and incrementally grouped live root output.
import type { EventMessage } from "@sdk/types.gen";
import {
  aggregateToolGroups,
  IncrementalMessageGrouper,
  isChildEvent,
  SessionGrouper,
  type MessageGroup,
  type Session,
} from "./grouping";

export interface RootConversation {
  sessions: Session[];
  pastSessions: Session[];
  groups: MessageGroup[];
  boundary: EventMessage | null;
}

// RootConversationProjection owns root filtering, completed/live partitioning,
// context grouping, and quota-warning state for one selected task incarnation.
export class RootConversationProjection {
  private processed = 0;
  private first: EventMessage | null = null;
  private last: EventMessage | null = null;
  private sections = new SessionGrouper();
  private pending: EventMessage[] = [];
  private live: EventMessage[] = [];
  private firstInit: EventMessage | null = null;
  private grouper = new IncrementalMessageGrouper();
  private grouped: MessageGroup[] | null = null;
  private snapshot: RootConversation = { sessions: [], pastSessions: [], groups: [], boundary: null };

  derive(
    messages: readonly EventMessage[],
    displayedAtRoot: (event: EventMessage) => boolean,
    reset: boolean,
  ): RootConversation {
    if (
      reset ||
      messages.length < this.processed ||
      (this.processed > 0 && (messages[0] !== this.first || messages[this.processed - 1] !== this.last))
    )
      this.reset();
    const completed: EventMessage[] = [];
    let committed = false;
    for (let i = this.processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing canonical history event");
      if (event.kind === "init" && !isChildEvent(event) && this.firstInit === null) this.firstInit = event;
      if (displayedAtRoot(event)) {
        this.pending.push(event);
        if (event.kind !== "init") this.live.push(event);
      }
      if (event.kind === "result" && !isChildEvent(event)) {
        for (const root of this.pending) completed.push(root);
        this.pending = [];
        this.live = [];
        this.firstInit = null;
        committed = true;
      }
    }
    this.processed = messages.length;
    this.first = messages[0] ?? null;
    this.last = messages.at(-1) ?? null;
    const sessions = committed ? this.sections.append(completed) : this.snapshot.sessions;
    if (committed) {
      this.grouper.reset(this.sections.warningPercentages);
      this.grouped = null;
    }
    const grouped = this.grouper.group(this.live);
    const groups = grouped === this.grouped ? this.snapshot.groups : aggregateToolGroups(grouped);
    this.grouped = grouped;
    const pastSessions = this.sections.pastSessions;
    const boundary = sessions.at(-1)?.boundaryEvent ?? this.firstInit;
    if (
      sessions !== this.snapshot.sessions ||
      pastSessions !== this.snapshot.pastSessions ||
      groups !== this.snapshot.groups ||
      boundary !== this.snapshot.boundary
    )
      this.snapshot = { sessions, pastSessions, groups, boundary };
    return this.snapshot;
  }

  // clear releases data also held by the presentation memo during disposal;
  // it does not read any reactive accessor while Solid tears down the owner.
  clear(): void {
    this.snapshot.sessions.length = 0;
    this.snapshot.pastSessions.length = 0;
    this.snapshot.groups.length = 0;
    this.snapshot.boundary = null;
    this.reset();
  }

  private reset(): void {
    this.processed = 0;
    this.first = null;
    this.last = null;
    this.sections = new SessionGrouper();
    this.pending = [];
    this.live = [];
    this.firstInit = null;
    this.grouper = new IncrementalMessageGrouper();
    this.grouped = null;
    this.snapshot = { sessions: [], pastSessions: [], groups: [], boundary: null };
  }
}
