// Groups one server-selected root context into completed turns and incrementally updated live output.
import type { EventMessage } from "@sdk/types.gen";
import {
  aggregateToolGroups,
  groupTurns,
  IncrementalMessageGrouper,
  isChildEvent,
  isSessionBoundary,
  updateWarningPercentages,
  type MessageGroup,
  type Turn,
} from "./grouping";

export interface RootConversation {
  turns: Turn[];
  groups: MessageGroup[];
}

export class RootConversationProjection {
  private processed = 0;
  private first: EventMessage | null = null;
  private last: EventMessage | null = null;
  private completed: EventMessage[] = [];
  private pending: EventMessage[] = [];
  private warnings = new Map<string, number>();
  private completedGrouper = new IncrementalMessageGrouper();
  private liveGrouper = new IncrementalMessageGrouper();
  private grouped: MessageGroup[] | null = null;
  private snapshot: RootConversation = { turns: [], groups: [] };

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
    let committed = false;
    for (let i = this.processed; i < messages.length; i++) {
      const event = messages[i];
      if (!event) throw new Error("Missing context event");
      if (displayedAtRoot(event) && (isChildEvent(event) || !isSessionBoundary(event))) this.pending.push(event);
      if (event.kind === "result" && !isChildEvent(event)) {
        for (const root of this.pending) {
          this.completed.push(root);
          updateWarningPercentages(this.warnings, root);
        }
        this.pending = [];
        committed = true;
      }
    }
    this.processed = messages.length;
    this.first = messages[0] ?? null;
    this.last = messages.at(-1) ?? null;
    const turns = committed
      ? groupTurns(aggregateToolGroups(this.completedGrouper.group(this.completed)))
      : this.snapshot.turns;
    if (committed) {
      this.liveGrouper.reset(this.warnings);
      this.grouped = null;
    }
    const grouped = this.liveGrouper.group(this.pending);
    const groups = grouped === this.grouped ? this.snapshot.groups : aggregateToolGroups(grouped);
    this.grouped = grouped;
    if (turns !== this.snapshot.turns || groups !== this.snapshot.groups) this.snapshot = { turns, groups };
    return this.snapshot;
  }
  clear(): void {
    this.snapshot.turns.length = 0;
    this.snapshot.groups.length = 0;
    this.reset();
  }
  private reset(): void {
    this.processed = 0;
    this.first = this.last = null;
    this.completed = [];
    this.pending = [];
    this.warnings = new Map();
    this.completedGrouper = new IncrementalMessageGrouper();
    this.liveGrouper = new IncrementalMessageGrouper();
    this.grouped = null;
    this.snapshot = { turns: [], groups: [] };
  }
}
