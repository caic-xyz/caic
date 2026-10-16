// Tests document Escape precedence against menus, dialogs, tooltips, and full-screen widgets.
import { afterEach, describe, it } from "node:test";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { Route, Router } from "@solidjs/router";
import { createSignal, type JSX } from "solid-js";
import { expect } from "@tests/expect";
import FileViewerHeader from "./FileViewerHeader";
import Dropdown from "./Dropdown";
import Tooltip from "./Tooltip";
import WidgetCard from "./WidgetCard";

function fixture(extra: JSX.Element) {
  window.history.replaceState(null, "", "/task/@abc/view?path=notes.md");
  return render(() => (
    <Router>
      <Route
        path="/*"
        component={() => (
          <>
            <FileViewerHeader taskId="abc" taskPath="/task/@abc" path="notes.md" />
            {extra}
          </>
        )}
      />
    </Router>
  ));
}

afterEach(cleanup);

describe("FileViewerHeader", () => {
  it("lets a capture-phase dropdown close without closing the document", async () => {
    const [open, setOpen] = createSignal(false);
    fixture(
      <Dropdown
        open={open()}
        onOpenChange={setOpen}
        content={
          <div role="menu">
            <button role="menuitem">Action</button>
          </div>
        }
      >
        <button onClick={() => setOpen(true)}>Actions</button>
      </Dropdown>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Actions" }));
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/task/@abc/view");
    await user.keyboard("{Escape}");
    expect(window.location.pathname).toBe("/task/@abc");
  });

  it("yields to an open dialog", async () => {
    fixture(
      <dialog open>
        <button>Dialog action</button>
      </dialog>,
    );
    await userEvent.setup().keyboard("{Escape}");
    expect(window.location.pathname).toBe("/task/@abc/view");
  });

  it("lets a tooltip dismiss before closing the document", async () => {
    fixture(<Tooltip text="Local explanation">Help</Tooltip>);
    const user = userEvent.setup();
    screen.getByRole("button", { name: "Close view" }).focus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Help" })).toHaveFocus();
    expect(screen.getByText("Local explanation")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByText("Local explanation")).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/task/@abc/view");
  });

  it("closes a full-screen widget before closing the document", async () => {
    fixture(
      <WidgetCard
        group={{
          kind: "widget",
          events: [],
          toolCalls: [],
          widgetTitle: "Chart",
          widgetHTML: "<p>Widget</p>",
          widgetDone: true,
        }}
      />,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Fullscreen" }));
    expect(screen.getByRole("button", { name: "Close fullscreen" })).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("button", { name: "Close fullscreen" })).not.toBeInTheDocument();
    expect(window.location.pathname).toBe("/task/@abc/view");
  });
});
