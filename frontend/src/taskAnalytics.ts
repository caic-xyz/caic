// Derives compact numeric task summaries and joins prices through weak result identity.
import type { EventChangeStat, EventCost, EventMessage, EventUsage, EventResult } from "@sdk/types.gen";
import { isChildEvent } from "./grouping";
import { ToolTimingTracker, type ToolTimingSummary } from "./taskStats";

// ResultEstimate associates source-owned result identity with mutable pricing.
// This runtime association is never serialized or retained by the numeric fold.
export interface ResultEstimate {
  result: EventResult;
  model: string | null;
  cost: EventCost | null;
}

export interface AnalyticsTurn {
  key: string;
  ts: number;
  model: string;
  usage: EventUsage;
  durationMs: number;
  durationAPIMs: number;
  waitMs: number | null;
  cost: EventCost | null;
  changeStat: EventChangeStat | null;
}

// AnalyticsSnapshot describes completed-turn facts for the folded prefix.
// The Task DTO remains authoritative for current aggregate usage/cost, including
// pending turns. Unknown per-turn estimates must not replace that aggregate.
export interface AnalyticsSnapshot {
  turns: readonly AnalyticsTurn[];
  tools: readonly ToolTimingSummary[];
  costUSD: number;
  costComplete: boolean;
}

// TaskAnalytics owns numbers and necessary pending identity/timing metadata.
// Raw text, tool inputs/results, widgets and messages never enter retained
// state. Result keys are weak; the active history owns their lifetime.
export class TaskAnalytics {
  private model = "";
  private tools = new ToolTimingTracker();
  private turns: AnalyticsTurn[] = [];
  private byResult = new WeakMap<EventResult, number>();
  private waiting = -1;

  append(event: EventMessage): boolean {
    return this.consume(event);
  }

  estimates(estimates: readonly ResultEstimate[]): boolean {
    let changed = false;
    for (const estimate of estimates) {
      const index = this.byResult.get(estimate.result);
      if (index === undefined) continue;
      const turn = this.turns[index];
      if (!turn) throw new Error("Missing analytics turn");
      const model = estimate.model ?? turn.model;
      if (model === turn.model && estimate.cost?.usd === turn.cost?.usd && estimate.cost?.source === turn.cost?.source)
        continue;
      this.turns[index] = { ...turn, model, cost: estimate.cost ? { ...estimate.cost } : null };
      changed = true;
    }
    return changed;
  }

  snapshot(): AnalyticsSnapshot {
    return {
      turns: this.turns.slice(),
      tools: this.tools.snapshot(),
      costUSD: this.turns.reduce((sum, turn) => sum + (turn.cost?.usd ?? 0), 0),
      costComplete: this.turns.every((turn) => turn.cost !== null),
    };
  }

  private consume(event: EventMessage): boolean {
    const toolsChanged = this.tools.append(event);
    if (isChildEvent(event)) return toolsChanged;
    if (event.kind === "init") this.model = event.init?.reportedModel ?? "";
    if (event.usage?.reportedModel) this.model = event.usage.reportedModel;
    if (event.kind === "result" && event.result) {
      const result = event.result;
      const usage = result.usage;
      const turn: AnalyticsTurn = {
        key: `turn:${this.turns.length}`,
        ts: event.ts,
        model: usage.reportedModel || this.model,
        usage: {
          inputTokens: usage.inputTokens,
          outputTokens: usage.outputTokens,
          cacheCreationInputTokens: usage.cacheCreationInputTokens,
          cacheReadInputTokens: usage.cacheReadInputTokens,
          reasoningOutputTokens: usage.reasoningOutputTokens,
          reportedModel: usage.reportedModel,
        },
        durationMs: result.duration * 1000,
        durationAPIMs: result.durationAPI * 1000,
        waitMs: null,
        changeStat: null,
        cost: result.cost ? { ...result.cost } : null,
      };
      this.byResult.set(result, this.turns.length);
      this.turns.push(turn);
      this.waiting = this.turns.length - 1;
      return true;
    } else if (event.commitSnapshot && !event.commitSnapshot.baseline && this.turns.length > 0) {
      const index = this.turns.length - 1;
      const turn = this.turns[index];
      if (turn) {
        this.turns[index] = {
          ...turn,
          changeStat: event.commitSnapshot.changeStat ? { ...event.commitSnapshot.changeStat } : null,
        };
        return true;
      }
    } else if (event.kind === "userInput" && this.waiting >= 0) {
      const turn = this.turns[this.waiting];
      const changed = turn !== undefined && turn.ts > 0 && event.ts > turn.ts;
      if (changed) this.turns[this.waiting] = { ...turn, waitMs: event.ts - turn.ts };
      this.waiting = -1;
      return changed || toolsChanged;
    }
    return toolsChanged;
  }
}
