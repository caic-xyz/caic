// Reads server-owned numeric statistics and lazily refreshes prices by exact history position.
import { createEffect, createMemo, createSignal, onCleanup } from "solid-js";
import type { HistoryTurn, HistoryToolTiming, HistoryTurnEstimate } from "@sdk/types.gen";
import type { TaskHistory } from "./taskEventTimeline";
import { historyPositionKey } from "./historyPosition";

function sameFacts<T extends object>(a: T | undefined, b: T | undefined): boolean {
  if (a === b) return true;
  if (!a || !b) return false;
  for (const key in a) if (a[key] !== b[key]) return false;
  for (const key in b) if (a[key] !== b[key]) return false;
  return true;
}

function sameTurn(a: HistoryTurn, b: HistoryTurn | undefined): boolean {
  return (
    b !== undefined &&
    a.recordID === b.recordID &&
    a.index === b.index &&
    a.sectionID === b.sectionID &&
    a.at === b.at &&
    a.model === b.model &&
    a.durationMs === b.durationMs &&
    a.durationAPIMs === b.durationAPIMs &&
    a.waitMs === b.waitMs &&
    sameFacts(a.waitInput, b.waitInput) &&
    sameFacts(a.usage, b.usage) &&
    sameFacts(a.changeStat, b.changeStat) &&
    sameFacts(a.cost, b.cost)
  );
}

export function createTaskAnalytics(history: TaskHistory) {
  const [estimates, setEstimates] = createSignal<ReadonlyMap<string, HistoryTurnEstimate>>(new Map());
  const [pricing, setPricing] = createSignal(false);
  const [pricingError, setPricingError] = createSignal<string | null>(null);
  let request: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  async function prices() {
    request?.abort();
    const controller = new AbortController();
    request = controller;
    setPricing(true);
    try {
      const values = await history.readCosts(controller.signal);
      if (controller.signal.aborted) return;
      setEstimates(new Map(values.map((value) => [historyPositionKey(value), value])));
      setPricingError(null);
    } catch (error) {
      if (!controller.signal.aborted) setPricingError(error instanceof Error ? error.message : String(error));
    } finally {
      if (request === controller && !controller.signal.aborted) setPricing(false);
    }
  }
  const boundary = createMemo(() => {
    const value = history.manifest();
    const last = value?.metadata.turns.at(-1);
    return value ? `${value.bootstrap.generation}:${last ? historyPositionKey(last) : ""}` : null;
  });
  createEffect(() => {
    const value = boundary();
    request?.abort();
    if (timer !== null) clearTimeout(timer);
    if (value === null) {
      setEstimates(new Map());
      setPricing(false);
      return;
    }
    timer = setTimeout(() => {
      timer = null;
      void prices();
    }, 250);
    onCleanup(() => {
      request?.abort();
      if (timer !== null) clearTimeout(timer);
    });
  });
  const turns = createMemo<readonly HistoryTurn[]>((previous) => {
    const amounts = estimates();
    const next = (history.manifest()?.metadata.turns ?? []).map((turn) => {
      const estimate = amounts.get(historyPositionKey(turn));
      return estimate ? { ...turn, model: estimate.model ?? turn.model, cost: estimate.cost ?? turn.cost } : turn;
    });
    return next.length === previous.length && next.every((turn, index) => sameTurn(turn, previous[index]))
      ? previous
      : next;
  }, []);
  const tools = createMemo<readonly HistoryToolTiming[]>((previous) => {
    const next = history.manifest()?.metadata.tools ?? [];
    return next.length === previous.length && next.every((tool, index) => sameFacts(tool, previous[index]))
      ? previous
      : next;
  }, []);
  const snapshot = createMemo(() => ({
    turns: turns(),
    tools: tools(),
    costUSD: turns().reduce((sum, turn) => sum + (turn.cost?.usd ?? 0), 0),
    costComplete: turns().every((turn) => turn.cost !== undefined),
  }));
  return {
    snapshot,
    stats: history.stats,
    loading: () => history.manifest() === null,
    pricing,
    error: () => history.error() ?? pricingError(),
    retry: () => {
      if (history.error()) history.retry();
      else void prices();
    },
  };
}
