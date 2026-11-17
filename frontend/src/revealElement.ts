// Waits for lazily rendered transcript elements, cancels on user scrolling, and reveals one: opens, scrolls, focuses, marks.

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

// revealElement opens the disclosures around el, scrolls it to the vertical
// center, focuses its summary or button (or el itself), and marks it with
// highlightClass for a short time. Smooth scrolling honors reduced motion.
export function revealElement(el: HTMLElement, highlightClass: string): void {
  for (let d = el.parentElement?.closest("details"); d; d = d.parentElement?.closest("details")) d.open = true;
  scrollToCenter(el);
  let control = el.querySelector<HTMLElement>(":scope > summary, :scope > button");
  if (!control) {
    if (!el.hasAttribute("tabindex")) el.tabIndex = -1;
    control = el;
  }
  control.focus({ preventScroll: true });
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
// page sideways to the start of a row wider than a narrow viewport.
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
    scroller.scrollBy({ top: box.top - view.top - (view.height - box.height) / 2, behavior });
  } else {
    window.scrollBy({ top: box.top - (window.innerHeight - box.height) / 2, behavior });
  }
}
