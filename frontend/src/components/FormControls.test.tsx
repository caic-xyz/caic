// Tests shared form controls' keyboard behavior.

import { fireEvent, render, screen } from "@solidjs/testing-library";
import { createSignal } from "solid-js";

import { ToggleChip } from "./FormControls";

describe("ToggleChip", () => {
  it("toggles with Enter without submitting its form", () => {
    const onSubmit = vi.fn((event: SubmitEvent) => event.preventDefault());

    function Host() {
      const [checked, setChecked] = createSignal(false);
      return (
        <form onSubmit={onSubmit}>
          <ToggleChip checked={checked()} title="Enable GitHub token" onChange={setChecked}>
            GitHub
          </ToggleChip>
        </form>
      );
    }

    render(() => <Host />);
    const toggle = screen.getByRole("checkbox", { name: "Enable GitHub token" });
    toggle.focus();

    expect(fireEvent.keyDown(toggle, { key: "Enter" })).toBe(false);

    expect(toggle).toBeChecked();
    expect(onSubmit).not.toHaveBeenCalled();
  });
});
