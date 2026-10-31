// Preserves visible conversation anchors through context loading, live output, disclosure, and pane visibility changes.
import { createEffect, onCleanup, onMount, type Accessor } from "solid-js";

interface Anchor {
  key: string | null;
  top: number | null;
  scroll: number;
  height: number;
  viewport: number;
}

export function createHistoryScroll(
  area: Accessor<HTMLElement | undefined>,
  body: Accessor<HTMLElement | undefined>,
  taskId: Accessor<string>,
  epochValue: Accessor<number>,
  content: Accessor<unknown>,
) {
  let active = true;
  let task = "";
  let epoch = -1;
  let operation = 0;
  let movement = 0;
  let following = true;
  let saved: Anchor | null = null;
  let expansion: Anchor | null = null;
  let writtenTop: number | null = null;
  let frame: number | null = null;
  let structural = false;
  let scrollInput = false;

  const nearBottom = (el: HTMLElement) => el.scrollHeight - el.scrollTop - el.clientHeight < 40;

  function anchor(el: HTMLElement): Anchor {
    const bounds = el.getBoundingClientRect();
    const row = [...el.querySelectorAll<HTMLElement>("[data-anchor-key]")].find((candidate) => {
      const box = candidate.getBoundingClientRect();
      return box.bottom > bounds.top && box.top < bounds.bottom;
    });
    return {
      key: row?.dataset.anchorKey ?? null,
      top: row?.getBoundingClientRect().top ?? null,
      scroll: el.scrollTop,
      height: el.scrollHeight,
      viewport: el.clientHeight,
    };
  }

  function schedule() {
    if (frame !== null) cancelAnimationFrame(frame);
    const el = area();
    if (!el) return;
    const id = taskId(),
      expectedEpoch = epochValue(),
      token = ++operation,
      input = movement;
    frame = requestAnimationFrame(() => {
      if (
        !active ||
        area() !== el ||
        !el.isConnected ||
        taskId() !== id ||
        epochValue() !== expectedEpoch ||
        operation !== token ||
        movement !== input
      )
        return;
      frame = null;
      if (el.clientHeight === 0) return;
      const prior = expansion ?? (structural && !following ? saved : null);
      if (prior) {
        const row = prior.key ? el.querySelector<HTMLElement>(`[data-anchor-key="${CSS.escape(prior.key)}"]`) : null;
        el.scrollTop =
          row && prior.top !== null
            ? el.scrollTop + row.getBoundingClientRect().top - prior.top
            : prior.scroll + el.scrollHeight - prior.height;
        writtenTop = el.scrollTop;
        following = nearBottom(el);
      } else if (following) {
        el.scrollTop = el.scrollHeight;
        writtenTop = el.scrollTop;
      }
      structural = false;
      scrollInput = false;
      expansion = null;
      saved = anchor(el);
    });
  }

  createEffect(() => {
    const id = taskId();
    const nextEpoch = epochValue();
    content();
    if (task !== id) {
      task = id;
      scrollInput = false;
      following = true;
      saved = null;
      expansion = null;
      writtenTop = null;
      structural = false;
    } else if (epoch !== nextEpoch) {
      structural = true;
      expansion = null;
    }
    epoch = nextEpoch;
    schedule();
  });

  onMount(() => {
    const el = area();
    if (!el) return;
    const observer = new ResizeObserver(schedule);
    observer.observe(el);
    const container = body();
    if (container) observer.observe(container);
    // Scroll events also come from native anchoring and layout clamping. Only
    // input may override a pending layout correction or initial tail placement.
    const input = () => {
      scrollInput = true;
    };
    const keyInput = (event: KeyboardEvent) => {
      if (event.target === el && ["ArrowDown", "ArrowUp", "End", "Home", "PageDown", "PageUp", " "].includes(event.key))
        input();
    };
    const pointerInput = (event: PointerEvent) => {
      const left = el.getBoundingClientRect().left + el.clientLeft;
      if (event.target === el && (event.clientX < left || event.clientX >= left + el.clientWidth)) input();
    };
    el.addEventListener("wheel", input, { passive: true });
    el.addEventListener("touchmove", input, { passive: true });
    el.addEventListener("pointerdown", pointerInput, { passive: true });
    el.addEventListener("keydown", keyInput);
    onCleanup(() => {
      observer.disconnect();
      el.removeEventListener("wheel", input);
      el.removeEventListener("touchmove", input);
      el.removeEventListener("pointerdown", pointerInput);
      el.removeEventListener("keydown", keyInput);
    });
  });

  onCleanup(() => {
    active = false;
    operation++;
    if (frame !== null) cancelAnimationFrame(frame);
    saved = null;
    expansion = null;
  });

  return {
    restoreAnchor(key: string, top: number | undefined) {
      const el = area();
      if (!el || top === undefined) return;
      expansion = { key, top, scroll: el.scrollTop, height: el.scrollHeight, viewport: el.clientHeight };
      schedule();
    },
    scroll() {
      const el = area();
      if (!el || el.clientHeight === 0) return;
      if (
        !scrollInput &&
        (!saved || saved.height !== el.scrollHeight || saved.viewport !== el.clientHeight || structural)
      ) {
        schedule();
        return;
      }
      const fromInput = scrollInput;
      scrollInput = false;
      saved = anchor(el);
      if (!fromInput && writtenTop !== null && Math.abs(el.scrollTop - writtenTop) < 1) {
        writtenTop = null;
        return;
      }
      writtenTop = null;
      movement++;
      expansion = null;
      following = nearBottom(el);
    },
  };
}
