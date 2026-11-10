// Tests live-tail following, disclosure anchors, hidden-pane restoration, and stale viewport callbacks.
import assert from "node:assert/strict";
import { afterEach, beforeEach, it } from "node:test";
import { batch, createRoot, createSignal } from "solid-js";
import { vi } from "@tests/expect";
import { createHistoryScroll } from "./historyScroll";
import type { EventMessage } from "@sdk/types.gen";

let frames = new Map<number, FrameRequestCallback>();
let nextFrame = 0;
let resize: ResizeObserverCallback;
let observed: Element[];
beforeEach(() => {
  frames = new Map();
  observed = [];
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(callback: ResizeObserverCallback) {
        resize = callback;
      }
      observe(el: Element) {
        observed.push(el);
      }
      disconnect() {}
    },
  );
  vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => {
    frames.set(++nextFrame, callback);
    return nextFrame;
  });
  vi.stubGlobal("cancelAnimationFrame", (id: number) => frames.delete(id));
});
afterEach(() => {
  vi.unstubAllGlobals();
  document.body.replaceChildren();
});

async function paint() {
  const pending = [...frames.values()];
  frames.clear();
  for (const callback of pending) callback(0);
  for (let i = 0; i < 10; i++) await Promise.resolve();
}

function fixture() {
  const el = document.createElement("div"),
    row = document.createElement("div");
  row.dataset.anchorKey = "visible";
  el.append(row);
  document.body.append(el);
  let visible = true;
  let height = 1_000,
    top = 0,
    rowTop = 160,
    viewportTop = 0,
    width = 400;
  Object.defineProperties(el, {
    clientHeight: { get: () => (visible ? 200 : 0) },
    clientWidth: { get: () => (visible ? width : 0) },
    scrollHeight: { get: () => (visible ? height : 0) },
    scrollTop: {
      get: () => (visible ? top : 0),
      set: (value: number) => {
        top = Math.max(0, Math.min(value, Math.max(0, height - 200)));
      },
    },
  });
  el.getBoundingClientRect = () => new DOMRect(0, viewportTop, 400, 200);
  row.getBoundingClientRect = () => new DOMRect(0, viewportTop + rowTop - top, 400, 30);
  const [messages, setMessages] = createSignal<EventMessage[]>([]),
    [preview, setPreview] = createSignal<string | null>(null),
    [plan, setPlan] = createSignal(""),
    [epoch, setEpoch] = createSignal(0),
    [task, setTask] = createSignal("a");
  const owner = createRoot((dispose) => ({
    dispose,
    scroll: createHistoryScroll(
      () => el,
      () => row,
      task,
      epoch,
      () => [messages(), preview(), plan()],
    ),
  }));
  return {
    el,
    row,
    ...owner,
    hide: () => {
      visible = false;
    },
    show: () => {
      visible = true;
    },
    dropOffset: () => {
      top = 0;
    },
    moveViewport: (position: number) => {
      viewportTop = position;
    },
    setTask,
    setEpoch,
    setMessages,
    reflow: (nextWidth: number, delta: number) => {
      width = nextWidth;
      height += delta;
      resize([], {} as ResizeObserver);
    },
    editPlan: () => {
      height += 40;
      setPlan("revised");
    },
    appendPreview: () => {
      height += 20;
      setPreview("pending output");
    },
    userScroll: (position: number) => {
      el.dispatchEvent(new Event("wheel"));
      el.scrollTop = position;
      el.dispatchEvent(new Event("scroll"));
    },
    grow: () => {
      height += 300;
      if (observed.includes(row)) resize([], {} as ResizeObserver);
    },
    clamp: () => {
      height = 600;
      top = 300;
      setEpoch((value) => value + 1);
      el.dispatchEvent(new Event("scroll"));
    },
    append: () => {
      height += 20;
      setMessages([]);
    },
    backfill: () => {
      height += 300;
      rowTop += 300;
      batch(() => {
        setEpoch((value) => value + 1);
        setMessages([]);
      });
    },
    expand: () => {
      height += 100;
      rowTop += 100;
    },
    collapse: () => {
      height = 100;
      top = 0;
      setMessages([]);
    },
  };
}

it("follows the recent preview, automatic backlog replacement, and later live output", async () => {
  const f = fixture();
  try {
    await paint();
    assert.equal(f.el.scrollTop, 800);
    f.appendPreview();
    await paint();
    assert.equal(f.el.scrollTop, 820);
    f.backfill();
    await paint();
    assert.equal(f.el.scrollTop, 1120);
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 1140);
  } finally {
    f.dispose();
  }
});

it("follows task-owned plan edits without a canonical history replacement", async () => {
  const f = fixture();
  try {
    await paint();
    assert.equal(f.el.scrollTop, 800);
    f.editPlan();
    await paint();
    assert.equal(f.el.scrollTop, 840);
  } finally {
    f.dispose();
  }
});

it("keeps the visible row anchored when backlog arrives while the user reads earlier output", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(100);
    assert.equal(f.row.getBoundingClientRect().top, 60);
    f.backfill();
    await paint();
    assert.equal(f.row.getBoundingClientRect().top, 60);
    assert.equal(f.el.scrollTop, 400);
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 400);
    f.userScroll(1120);
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 1140);
  } finally {
    f.dispose();
  }
});

it("honors a user scroll that happens after a live-follow frame was queued", async () => {
  const f = fixture();
  try {
    await paint();
    f.append();
    f.userScroll(500);
    await paint();
    assert.equal(f.el.scrollTop, 500);
  } finally {
    f.dispose();
  }
});

it("continues following after completed-turn collapse clamps the viewport", async () => {
  const f = fixture();
  try {
    await paint();
    f.collapse();
    f.el.dispatchEvent(new Event("scroll"));
    await paint();
    assert.equal(f.el.scrollTop, 0);
    for (let i = 0; i < 6; i++) f.append();
    await paint();
    assert.equal(f.el.scrollTop, 20);
  } finally {
    f.dispose();
  }
});

for (const position of [0, 100]) {
  it(`preserves a disclosure anchor at ${position}px when a live append shares its render frame`, async () => {
    const f = fixture();
    try {
      await paint();
      f.userScroll(position);
      const before = f.row.getBoundingClientRect().top;
      f.expand();
      f.scroll.restoreAnchor("visible", before);
      f.append();
      await paint();
      assert.equal(f.row.getBoundingClientRect().top, before);
      assert.equal(f.el.scrollTop, position + 100);
    } finally {
      f.dispose();
    }
  });
}

it("keeps the reading position through structural growth at a constant width", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(300);
    f.backfill();
    await paint();
    assert.equal(f.el.scrollTop, 600);
  } finally {
    f.dispose();
  }
});

it("keeps the beginning visible when reconnect feedback pushes the first row down", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(0);
    f.expand();
    f.reflow(400, 0);
    await paint();
    assert.equal(f.el.scrollTop, 0);
    f.backfill();
    await paint();
    assert.equal(f.el.scrollTop, 0);
  } finally {
    f.dispose();
  }
});

it("leaves a reflowed reading position to native scroll anchoring", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(300);
    // A content change landing across a width change wraps lines above and
    // below the viewport, so the height delta no longer locates the text.
    f.setEpoch((value) => value + 1);
    f.reflow(320, 20);
    await paint();
    assert.equal(f.el.scrollTop, 300);
  } finally {
    f.dispose();
  }
});

it("restores the offset when the layout drops it across a hidden pane", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(300);
    f.hide();
    f.reflow(400, 0);
    await paint();
    f.dropOffset();
    f.show();
    f.reflow(400, 0);
    await paint();
    assert.equal(f.el.scrollTop, 300);
  } finally {
    f.dispose();
  }
});

it("keeps the visible row anchored when status content is inserted above it", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(100);
    f.expand();
    f.reflow(400, 0);
    await paint();
    assert.equal(f.row.getBoundingClientRect().top, 60);
    assert.equal(f.el.scrollTop, 200);
  } finally {
    f.dispose();
  }
});

it("restores the reading position relative to a viewport that moved while hidden", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(100);
    f.hide();
    f.reflow(400, 0);
    await paint();
    f.moveViewport(40);
    f.expand();
    f.show();
    f.reflow(400, 0);
    await paint();
    assert.equal(f.row.getBoundingClientRect().top - f.el.getBoundingClientRect().top, 60);
    assert.equal(f.el.scrollTop, 200);
  } finally {
    f.dispose();
  }
});

it("restores a visible row when content above it changes while its pane is hidden", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(100);
    f.hide();
    f.reflow(400, 0);
    await paint();
    f.expand();
    f.show();
    f.reflow(400, 0);
    await paint();
    assert.equal(f.row.getBoundingClientRect().top, 60);
    assert.equal(f.el.scrollTop, 200);
  } finally {
    f.dispose();
  }
});

it("holds a pending content correction until the pane returns", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(300);
    f.setEpoch((value) => value + 1);
    f.hide();
    f.reflow(400, 0);
    await paint();
    f.show();
    f.reflow(400, 40);
    await paint();
    assert.equal(f.el.scrollTop, 340);
  } finally {
    f.dispose();
  }
});

for (const change of ["task", "generation", "dispose", "element"] as const) {
  it(`ignores an old expansion frame after ${change}`, async () => {
    const f = fixture();
    try {
      await paint();
      f.userScroll(100);
      f.expand();
      f.scroll.restoreAnchor("visible", 60);
      const stale = [...frames.values()];
      if (change === "task") f.setTask("b");
      if (change === "generation") f.setEpoch(100);
      if (change === "dispose") f.dispose();
      if (change === "element") f.el.remove();
      f.el.scrollTop = 25;
      for (const callback of stale) callback(0);
      assert.equal(f.el.scrollTop, 25);
    } finally {
      if (change !== "dispose") f.dispose();
    }
  });
}

it("ignores hidden-pane scroll and content observations instead of changing the saved user position", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(300);
    f.hide();
    f.el.dispatchEvent(new Event("scroll"));
    f.append();
    await paint();
    f.show();
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 300);
  } finally {
    f.dispose();
  }
});

it("keeps following when context replacement clamps scrolling before the next paint", async () => {
  const f = fixture();
  try {
    await paint();
    f.clamp();
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 420);
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 440);
  } finally {
    f.dispose();
  }
});

it("follows content resizing after rendering, but leaves earlier reading in place", async () => {
  const f = fixture();
  try {
    await paint();
    f.grow();
    await paint();
    assert.equal(f.el.scrollTop, 1100);
    f.userScroll(300);
    f.grow();
    await paint();
    assert.equal(f.el.scrollTop, 300);
  } finally {
    f.dispose();
  }
});

it("starts following a newly selected task despite layout scrolls from the old task", async () => {
  const f = fixture();
  try {
    await paint();
    f.userScroll(100);
    f.setTask("b");
    f.el.dispatchEvent(new Event("scroll"));
    f.append();
    await paint();
    assert.equal(f.el.scrollTop, 820);
  } finally {
    f.dispose();
  }
});

it("honors keyboard scrolling while a context replacement is pending", async () => {
  const f = fixture();
  try {
    await paint();
    f.backfill();
    f.el.dispatchEvent(new KeyboardEvent("keydown", { key: "PageUp" }));
    f.el.scrollTop = 400;
    f.el.dispatchEvent(new Event("scroll"));
    await paint();
    assert.equal(f.el.scrollTop, 400);
  } finally {
    f.dispose();
  }
});
