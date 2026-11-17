// Tests chip names by kind, activation, and that no strip remains when idle.

import { describe, it } from "node:test";
import { render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";

import type { BackgroundItem } from "../backgroundActivity";
import BackgroundStrip from "./BackgroundStrip";

const watch: BackgroundItem = {
  kind: "watch",
  kindName: "Watch",
  id: "w",
  label: "results",
  startedAt: null,
  toolKey: "k",
  ownerAgentID: null,
};

describe("BackgroundStrip", () => {
  it("names each chip, reveals its item on activation, and disappears when idle", async () => {
    const [items, setItems] = createSignal<BackgroundItem[]>([watch]);
    const onReveal = vi.fn();
    render(() => <BackgroundStrip items={items()} onReveal={onReveal} />);
    const chip = screen.getByRole("button", { name: "Watch results" });
    expect(chip).toHaveAttribute("title", "Watch: results");
    chip.focus();
    await userEvent.setup().keyboard("{Enter}");
    expect(onReveal).toHaveBeenCalledWith(watch);
    setItems([]);
    expect(screen.queryByTestId("background-strip")).not.toBeInTheDocument();
  });

  it("names an agent chip by its kind and says a label equal to the kind once", () => {
    const agent = { kind: "agent", id: "a", startedAt: null, toolKey: null, ownerAgentID: null } as const;
    render(() => (
      <BackgroundStrip
        items={[
          { ...agent, kindName: "general-purpose", label: "Review the API handlers" },
          { ...agent, id: "b", kindName: "worker", label: "worker" },
        ]}
        onReveal={vi.fn()}
      />
    ));
    const named = screen.getByRole("button", { name: "general-purpose Review the API handlers" });
    expect(named).toHaveAttribute("title", "general-purpose: Review the API handlers");
    expect(screen.getByRole("button", { name: "worker" })).toHaveAttribute("title", "worker");
  });
});
