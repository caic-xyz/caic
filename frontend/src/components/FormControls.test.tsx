// Tests shared form controls' keyboard behavior and harness selection across option reordering.

import { describe, it } from "node:test";
import { vi, expect } from "@tests/expect";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { createSignal } from "solid-js";

import type { HarnessInfo } from "@sdk/types.gen";
import { HarnessControls, ToggleChip } from "./FormControls";

it("preserves the selected harness when quota updates reorder its options", () => {
  const choice = (name: HarnessInfo["name"]): HarnessInfo => ({
    name,
    models: [],
    supportsImages: false,
    supportsCompact: false,
    supportsModelRefresh: false,
  });
  const choices = [choice("claude"), choice("codex"), choice("pi")] as const;
  const [harnesses, setHarnesses] = createSignal<HarnessInfo[]>([...choices]);
  const [harness, setHarness] = createSignal("claude");
  render(() => (
    <HarnessControls
      harnesses={harnesses()}
      harness={harness()}
      model=""
      effort=""
      onHarness={setHarness}
      onModel={() => {}}
      onEffort={() => {}}
    />
  ));
  const select = screen.getByRole("combobox", { name: "Harness" });
  for (const order of [
    [2, 0, 1],
    [1, 2, 0],
    [0, 1, 2],
    [1, 0, 2],
    [2, 1, 0],
  ] as const) {
    setHarnesses(order.map((index) => choices[index]));
    expect(select).toHaveValue("claude");
  }
});

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
