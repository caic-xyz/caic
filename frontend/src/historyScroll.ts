// Preserves visible conversation anchors through context loading, live output, disclosure, pane visibility, and responsive scroll viewport changes.
import { createEffect, onCleanup, type Accessor } from "solid-js";
import { anchorAt, anchorRow } from "./anchorKeys";

interface Anchor {
  key: string | null;
  top: number | null;
  scroll: number;
  height: number;
  viewport: number;
  width: number;
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
  let hidden = false;
  let viewport: HTMLElement | undefined;

  const nearBottom = (el: HTMLElement) => el.scrollHeight - el.scrollTop - el.clientHeight < 40;

  function anchor(el: HTMLElement): Anchor {
    const position = anchorAt(el);
    return {
      key: position.key,
      top: position.top === null ? null : position.top - el.getBoundingClientRect().top,
      scroll: el.scrollTop,
      height: el.scrollHeight,
      viewport: el.clientHeight,
      width: el.clientWidth,
    };
  }

  function write(el: HTMLElement, top: number) {
    el.scrollTop = top;
    writtenTop = el.scrollTop;
  }

  function schedule() {
    if (frame !== null) cancelAnimationFrame(frame);
    const el = area();
    if (!el) return;
    if (viewport !== el) {
      viewport = el;
      structural = true;
    }
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
      if (el.clientHeight === 0) {
        // Out of layout. Hold a pending structural correction for the frame
        // that brings the pane back instead of letting it wait for an unrelated
        // later frame.
        hidden = true;
        return;
      }
      const returning = hidden;
      hidden = false;
      const resized = saved !== null && saved.width === el.clientWidth && saved.height !== el.scrollHeight;
      const prior = expansion ?? ((structural || returning || resized) && !following ? saved : null);
      if (prior) {
        const row = anchorRow(el, prior.key);
        // A reader at the beginning should also see newly inserted feedback.
        // Explicit disclosure anchors still preserve the clicked row.
        if (prior.scroll === 0 && expansion === null) write(el, 0);
        else if (row && prior.top !== null)
          write(el, el.scrollTop + row.getBoundingClientRect().top - el.getBoundingClientRect().top - prior.top);
        // Without a row, the height delta stands in for the hidden content only
        // while the pane width is unchanged: a reflow folds line wrapping into
        // that delta, and the browser's own scroll anchoring places the text
        // better than the correction does.
        else if (structural && prior.width === el.clientWidth) write(el, prior.scroll + el.scrollHeight - prior.height);
        else if (returning && el.scrollTop === 0) write(el, prior.scroll);
      } else if (following) {
        write(el, el.scrollHeight);
      }
      following = nearBottom(el);
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
      hidden = false;
    } else if (epoch !== nextEpoch) {
      structural = true;
      expansion = null;
    }
    epoch = nextEpoch;
    schedule();
  });

  createEffect(() => {
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
      if (
        (event.target === el || event.target === container?.parentElement) &&
        ["ArrowDown", "ArrowUp", "End", "Home", "PageDown", "PageUp", " "].includes(event.key)
      )
        input();
    };
    const pointerInput = (event: PointerEvent) => {
      const left = el.getBoundingClientRect().left + el.clientLeft;
      if (event.target === el && (event.clientX < left || event.clientX >= left + el.clientWidth)) input();
    };
    el.addEventListener("scroll", scroll);
    el.addEventListener("wheel", input, { passive: true });
    el.addEventListener("touchmove", input, { passive: true });
    el.addEventListener("pointerdown", pointerInput, { passive: true });
    el.addEventListener("keydown", keyInput);
    onCleanup(() => {
      observer.disconnect();
      el.removeEventListener("scroll", scroll);
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
      expansion = {
        key,
        top: top - el.getBoundingClientRect().top,
        scroll: el.scrollTop,
        height: el.scrollHeight,
        viewport: el.clientHeight,
        width: el.clientWidth,
      };
      schedule();
    },
  };

  function scroll() {
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
  }
}
