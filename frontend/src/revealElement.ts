// Waits for lazily rendered or hidden transcript elements, cancels on user scrolling, and reveals one: opens, scrolls, marks, and optionally focuses.

const HIGHLIGHT_MS = 1_600;
// One element carries the mark at a time: a later reveal moves it.
let marked: { el: HTMLElement; className: string; timer: ReturnType<typeof setTimeout> } | null = null;

const scrollKeys = new Set([" ", "ArrowDown", "ArrowUp", "End", "Home", "PageDown", "PageUp"]);

// abortOnUserScroll aborts a pending reveal once the user scrolls or points
// elsewhere: a wheel, a touch move, a pointer press, or a scroll key. Its
// listeners end with the controller.
export function abortOnUserScroll(abort: AbortController): void {
  const cancel = () => abort.abort();
  const options = { capture: true, passive: true, signal: abort.signal };
  window.addEventListener("wheel", cancel, options);
  window.addEventListener("touchmove", cancel, options);
  window.addEventListener("pointerdown", cancel, options);
  window.addEventListener(
    "keydown",
    (event) => {
      if (scrollKeys.has(event.key)) cancel();
    },
    options,
  );
}

// waitForElement resolves with the first non-null find() under root, or null
// on timeout or abort.
export function waitForElement(
  root: Element,
  find: () => HTMLElement | null,
  signal: AbortSignal,
  timeoutMs: number,
): Promise<HTMLElement | null> {
  if (signal.aborted) return Promise.resolve(null);
  const found = find();
  if (found) return Promise.resolve(found);
  return new Promise((resolve) => {
    const finish = (el: HTMLElement | null) => {
      observer.disconnect();
      clearTimeout(timer);
      signal.removeEventListener("abort", abort);
      resolve(el);
    };
    const observer = new MutationObserver(() => {
      const el = find();
      if (el) finish(el);
    });
    const abort = () => finish(null);
    const timer = setTimeout(abort, timeoutMs);
    observer.observe(root, { childList: true, subtree: true });
    signal.addEventListener("abort", abort);
  });
}

// whenVisible resolves true once visible() holds for el, after two more frames
// so that layout corrections made when el reappears do not move a later
// reveal; false on abort or timeout.
export function whenVisible(
  el: Element | undefined,
  visible: () => boolean,
  signal: AbortSignal,
  timeoutMs: number,
): Promise<boolean> {
  if (!el || signal.aborted) return Promise.resolve(false);
  return new Promise((resolve) => {
    let frame = 0;
    const finish = (shown: boolean) => {
      observer.disconnect();
      clearTimeout(timer);
      cancelAnimationFrame(frame);
      signal.removeEventListener("abort", abort);
      resolve(shown);
    };
    const settle = () => {
      if (frame !== 0 || !visible()) return;
      frame = requestAnimationFrame(() => {
        frame = requestAnimationFrame(() => finish(true));
      });
    };
    const observer = new ResizeObserver(settle);
    const abort = () => finish(false);
    const timer = setTimeout(abort, timeoutMs);
    observer.observe(el);
    signal.addEventListener("abort", abort);
    settle();
  });
}

// RevealOptions selects what a reveal does beyond scrolling and marking.
export interface RevealOptions {
  // focus moves focus to el's summary or button, or el itself.
  focus: boolean;
  // open also opens el when it is a disclosure, such as the tool call or
  // agent card the user asked to see.
  open?: boolean;
  // scroll positions el in place of the default centering, for a caller that
  // owns the viewport.
  scroll?: (target: HTMLElement) => void;
}

// revealElement opens the disclosures around el, and el itself when asked,
// scrolls it into view, and marks it with highlightClass for a short time. The
// default scroll centers el vertically, or puts its top at the top of the view
// when it is taller; smooth scrolling honors reduced motion.
export function revealElement(el: HTMLElement, highlightClass: string, options: RevealOptions): void {
  for (let d = el.parentElement?.closest("details"); d; d = d.parentElement?.closest("details")) d.open = true;
  if (options.open && el.matches("details")) el.open = true;
  (options.scroll ?? scrollToCenter)(el);
  if (options.focus) {
    let control = el.querySelector<HTMLElement>(":scope > summary, :scope > button");
    if (!control) {
      if (!el.hasAttribute("tabindex")) el.tabIndex = -1;
      control = el;
    }
    control.focus({ preventScroll: true });
  }
  if (marked) {
    clearTimeout(marked.timer);
    marked.el.classList.remove(marked.className);
  }
  // Reflow so a repeated reveal of the same element restarts the mark.
  void el.offsetWidth;
  el.classList.add(highlightClass);
  const current = {
    el,
    className: highlightClass,
    timer: setTimeout(() => {
      el.classList.remove(highlightClass);
      if (marked === current) marked = null;
    }, HIGHLIGHT_MS),
  };
  marked = current;
}

// scrollToCenter scrolls only vertically: scrollIntoView would also scroll a
// page sideways to the start of a row wider than a narrow viewport. An element
// taller than the view keeps its top, and so its summary, in view.
function scrollToCenter(el: HTMLElement): void {
  const behavior = window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
  let scroller: Element | null = el.parentElement;
  for (; scroller; scroller = scroller.parentElement) {
    const overflow = getComputedStyle(scroller).overflowY;
    if ((overflow === "auto" || overflow === "scroll") && scroller.scrollHeight > scroller.clientHeight) break;
  }
  const box = el.getBoundingClientRect();
  if (scroller) {
    const view = scroller.getBoundingClientRect();
    scroller.scrollBy({ top: box.top - view.top - Math.max(0, (view.height - box.height) / 2), behavior });
  } else {
    window.scrollBy({ top: box.top - Math.max(0, (window.innerHeight - box.height) / 2), behavior });
  }
}
