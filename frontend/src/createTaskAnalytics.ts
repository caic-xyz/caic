// Derives view-scoped numeric statistics from the active history with lazy, cancellable pricing.
import { createEffect, createSignal, onCleanup, untrack } from "solid-js";
import { isChildEvent } from "./grouping";
import { TaskAnalytics } from "./taskAnalytics";
import type { TaskHistory } from "./taskEventTimeline";

export function createTaskAnalytics(history: TaskHistory) {
  const [snapshot, setSnapshot] = createSignal(new TaskAnalytics().snapshot());
  const [pricing, setPricing] = createSignal(false);
  const [pricingError, setPricingError] = createSignal<string | null>(null);
  let fold = new TaskAnalytics();
  let epoch = -1;
  let offset = 0;
  let priced = false;
  let active = true;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let request: AbortController | null = null;
  function cancel() {
    request?.abort();
    request = null;
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }
  async function prices() {
    if (timer !== null) clearTimeout(timer);
    timer = null;
    request?.abort();
    const controller = new AbortController();
    request = controller;
    setPricing(true);
    try {
      const estimates = await history.readCosts(controller.signal);
      if (!active || controller.signal.aborted) return;
      if (fold.estimates(estimates)) setSnapshot(fold.snapshot());
      setPricingError(null);
    } catch (error) {
      if (!active || controller.signal.aborted) return;
      setPricingError(error instanceof Error ? error.message : String(error));
    } finally {
      if (active && request === controller) setPricing(false);
    }
  }
  createEffect(() => {
    const current = history.epoch();
    const messages = history.messages();
    const loading = history.loading();
    untrack(() => {
      let changed = false;
      if (epoch !== current) {
        cancel();
        epoch = current;
        offset = 0;
        priced = false;
        fold = new TaskAnalytics();
        setPricing(false);
        setPricingError(null);
        changed = true;
      }
      let results = false;
      for (; offset < messages.length; offset++) {
        const event = messages[offset];
        if (!event) throw new Error("Missing task history event");
        changed = fold.append(event) || changed;
        if (event.result && !isChildEvent(event)) results = true;
      }
      if (changed) setSnapshot(fold.snapshot());
      if (loading) return;
      if (!priced) {
        priced = true;
        void prices();
      } else if (results) {
        cancel();
        setPricing(true);
        timer = setTimeout(() => {
          timer = null;
          void prices();
        }, 250);
      }
    });
  });
  onCleanup(() => {
    active = false;
    cancel();
  });
  return {
    snapshot,
    stats: history.stats,
    loading: history.loading,
    pricing,
    error: () => history.error() ?? pricingError(),
    retry: () => {
      if (history.error()) history.retry();
      else void prices();
    },
  };
}
