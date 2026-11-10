// Owns conversation reading intent and pending layout corrections through live output, disclosure, and viewport changes.
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

type ReadingIntent = { kind: "following"; anchor: Anchor | null } | { kind: "reading"; anchor: Anchor };
interface Correction {
  disclosure: Anchor | null;
  // A replacement can span a hidden pane. Preserve both fallback reasons if
  // the keyed row disappears before the correction runs.
  replacement: boolean;
  restore: boolean;
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
  let intent: ReadingIntent = { kind: "following", anchor: null };
  let correction: Correction | null = null;
  let writtenTop: number | null = null;
  let frame: { id: number } | null = null;
  let scrollInput = false;
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

  function cancelFrame() {
    if (frame !== null) cancelAnimationFrame(frame.id);
    frame = null;
  }

  function correctLayout(kind: "replacement" | "restore") {
    correction ??= { disclosure: null, replacement: false, restore: false };
    correction[kind] = true;
    return correction;
  }

  function schedule() {
    cancelFrame();
    const el = area();
    if (!el) return;
    if (viewport !== el) {
      viewport = el;
      correctLayout("replacement");
    }
    const id = taskId(),
      expectedEpoch = epochValue();
    const scheduled = { id: 0 };
    frame = scheduled;
    scheduled.id = requestAnimationFrame(() => {
      if (
        !active ||
        area() !== el ||
        !el.isConnected ||
        taskId() !== id ||
        epochValue() !== expectedEpoch ||
        frame !== scheduled
      )
        return;
      frame = null;
      if (el.clientHeight === 0) {
        // Out of layout. Hold a pending structural correction for the frame
        // that brings the pane back instead of letting it wait for an unrelated
        // later frame.
        correctLayout("restore");
        return;
      }
      const saved = intent.anchor;
      const resized = saved !== null && saved.width === el.clientWidth && saved.height !== el.scrollHeight;
      const disclosure = correction?.disclosure;
      const prior =
        disclosure ?? (intent.kind === "reading" && (correction !== null || resized) ? intent.anchor : null);
      if (prior) {
        const row = anchorRow(el, prior.key);
        // A reader at the beginning should also see newly inserted feedback.
        // Explicit disclosure anchors still preserve the clicked row.
        if (prior.scroll === 0 && !disclosure) write(el, 0);
        else if (row && prior.top !== null)
          write(el, el.scrollTop + row.getBoundingClientRect().top - el.getBoundingClientRect().top - prior.top);
        // Without a row, the height delta stands in for the hidden content only
        // while the pane width is unchanged: a reflow folds line wrapping into
        // that delta, and the browser's own scroll anchoring places the text
        // better than the correction does.
        else if (correction?.replacement && prior.width === el.clientWidth)
          write(el, prior.scroll + el.scrollHeight - prior.height);
        else if (correction?.restore && el.scrollTop === 0) write(el, prior.scroll);
      } else if (intent.kind === "following") {
        write(el, el.scrollHeight);
      }
      const measured = anchor(el);
      intent = { kind: nearBottom(el) ? "following" : "reading", anchor: measured };
      scrollInput = false;
      correction = null;
    });
  }

  createEffect(() => {
    const id = taskId();
    const nextEpoch = epochValue();
    content();
    if (task !== id) {
      task = id;
      scrollInput = false;
      intent = { kind: "following", anchor: null };
      correction = null;
      writtenTop = null;
    } else if (epoch !== nextEpoch) {
      correctLayout("replacement").disclosure = null;
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
    cancelFrame();
    intent = { kind: "following", anchor: null };
    correction = null;
  });

  return {
    restoreAnchor(key: string, top: number | undefined) {
      const el = area();
      if (!el || top === undefined) return;
      correction = {
        replacement: correction?.replacement ?? false,
        restore: correction?.restore ?? false,
        disclosure: {
          key,
          top: top - el.getBoundingClientRect().top,
          scroll: el.scrollTop,
          height: el.scrollHeight,
          viewport: el.clientHeight,
          width: el.clientWidth,
        },
      };
      schedule();
    },
  };

  function scroll() {
    const el = area();
    if (!el || el.clientHeight === 0) return;
    const saved = intent.anchor;
    if (
      !scrollInput &&
      (!saved || saved.height !== el.scrollHeight || saved.viewport !== el.clientHeight || correction !== null)
    ) {
      schedule();
      return;
    }
    const fromInput = scrollInput;
    scrollInput = false;
    const measured = anchor(el);
    if (!fromInput && writtenTop !== null && Math.abs(el.scrollTop - writtenTop) < 1) {
      writtenTop = null;
      intent = { ...intent, anchor: measured };
      return;
    }
    writtenTop = null;
    cancelFrame();
    correction = null;
    intent = { kind: nearBottom(el) ? "following" : "reading", anchor: measured };
  }
}
