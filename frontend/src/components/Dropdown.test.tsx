// Tests for reusable dropdown keyboard dismissal and focus restoration.

import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";

import Dropdown from "./Dropdown";

describe("Dropdown", () => {
  it("opens and navigates menu items with arrow keys", async () => {
    const user = userEvent.setup();

    function DropdownHost() {
      const [open, setOpen] = createSignal(false);
      return (
        <Dropdown
          open={open()}
          onOpenChange={setOpen}
          content={
            <div role="menu">
              <button type="button" role="menuitem">First action</button>
              <button type="button" role="menuitem" disabled>Disabled action</button>
              <button type="button" role="menuitem">Last action</button>
            </div>
          }
        >
          <button type="button" onClick={() => setOpen(true)}>Open menu</button>
        </Dropdown>
      );
    }

    render(() => <DropdownHost />);
    const trigger = screen.getByRole("button", { name: "Open menu" });
    trigger.focus();

    await user.keyboard("{ArrowDown}");
    await screen.findByRole("menu");
    await vi.waitFor(() => expect(screen.getByRole("menuitem", { name: "First action" })).toHaveFocus());

    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "Last action" })).toHaveFocus();

    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "First action" })).toHaveFocus();

    await user.keyboard("{Escape}");
    trigger.focus();
    await user.keyboard("{ArrowUp}");
    await vi.waitFor(() => expect(screen.getByRole("menuitem", { name: "Last action" })).toHaveFocus());
  });

  it("closes on Escape and restores focus to its trigger", async () => {
    const user = userEvent.setup();

    function DropdownHost() {
      const [open, setOpen] = createSignal(false);
      return (
        <Dropdown
          open={open()}
          onOpenChange={setOpen}
          content={<button type="button">Menu action</button>}
        >
          <button type="button" onClick={() => setOpen(true)}>Open menu</button>
        </Dropdown>
      );
    }

    render(() => <DropdownHost />);
    const trigger = screen.getByRole("button", { name: "Open menu" });
    await user.click(trigger);
    const action = screen.getByRole("button", { name: "Menu action" });
    action.focus();

    await user.keyboard("{Escape}");

    expect(screen.queryByRole("button", { name: "Menu action" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});
