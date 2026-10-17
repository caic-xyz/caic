// Tests CAD corner control keyboard access, axis restoration, reset isolation, and dismissal.
import { afterEach, it } from "node:test";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";
import CadViewControls from "./CadViewControls";

afterEach(cleanup);

function fixture() {
  const [visible, setVisible] = createSignal(true);
  const reset = vi.fn();
  render(() => (
    <>
      <CadViewControls axesVisible={visible()} onToggleAxes={() => setVisible(!visible())} onResetView={reset}>
        <canvas aria-label="Model" />
      </CadViewControls>
      <button>Outside</button>
    </>
  ));
  return { reset };
}

it("restores hidden axes and keeps reset independent through keyboard controls", async () => {
  const { reset } = fixture();
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "View controls" });
  await user.tab();
  expect(trigger).toHaveFocus();
  await user.tab();
  expect(screen.getByRole("button", { name: "Reset view" })).toHaveFocus();
  await user.tab();
  expect(screen.getByRole("button", { name: "Hide axes" })).toHaveFocus();
  await user.keyboard(" ");
  expect(trigger).toHaveFocus();
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  await user.keyboard("{Enter}");
  expect(screen.getByRole("button", { name: "Show axes" })).toBeInTheDocument();
  await user.tab();
  await user.keyboard("{Enter}");
  expect(reset).toHaveBeenCalledTimes(1);
  expect(trigger).toHaveFocus();
  await user.keyboard("{Enter}");
  await user.tab();
  await user.tab();
  expect(screen.getByRole("button", { name: "Show axes" })).toHaveFocus();
  await user.keyboard(" ");
  await user.keyboard("{Enter}");
  expect(screen.getByRole("button", { name: "Hide axes" })).toBeInTheDocument();
});

it("dismisses before workspace Escape and restores focus only from inside the controls", async () => {
  fixture();
  const user = userEvent.setup();
  const trigger = screen.getByRole("button", { name: "View controls" });
  trigger.focus();
  expect(trigger).toHaveAttribute("aria-expanded", "true");
  await user.keyboard("{Enter}");
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  await user.keyboard("{Enter}");
  await user.tab();
  const escaped = vi.fn();
  document.addEventListener("keydown", escaped);
  try {
    await user.keyboard("{Escape}");
    expect(escaped).not.toHaveBeenCalled();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
  } finally {
    document.removeEventListener("keydown", escaped);
  }
  await user.keyboard("{Enter}");
  await user.click(screen.getByRole("button", { name: "Outside" }));
  expect(trigger).toHaveAttribute("aria-expanded", "false");
  expect(screen.getByRole("button", { name: "Outside" })).toHaveFocus();
  await user.tab({ shift: true });
  expect(trigger).toHaveFocus();
  expect(trigger).toHaveAttribute("aria-expanded", "true");
  await user.tab();
  await user.tab();
  await user.tab();
  expect(screen.getByRole("button", { name: "Outside" })).toHaveFocus();
  expect(trigger).toHaveAttribute("aria-expanded", "false");
});
