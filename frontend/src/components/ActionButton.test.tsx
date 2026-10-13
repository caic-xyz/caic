// Tests toolbar action naming, keyboard activation, pressed state, disabled behavior, and safe form use.
import { afterEach, it } from "node:test";
import { createSignal } from "solid-js";
import { cleanup, render, screen } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";
import { expect, vi } from "@tests/expect";
import ActionButton from "./ActionButton";
import ResetIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";

afterEach(cleanup);

it("keeps toolbar actions accessible without submitting their containing form", async () => {
  const user = userEvent.setup();
  const submit = vi.fn();
  const [pressed, setPressed] = createSignal(false);
  const [disabled, setDisabled] = createSignal(false);
  let button: HTMLButtonElement | undefined;
  render(() => (
    <form onSubmit={submit}>
      <ActionButton
        ref={button}
        label="Wireframe"
        aria-pressed={pressed()}
        disabled={disabled()}
        onClick={() => setPressed(!pressed())}
      >
        <ResetIcon width={20} height={20} />
      </ActionButton>
    </form>
  ));
  const action = screen.getByRole("button", { name: "Wireframe" });
  expect(button).toBe(action);
  expect(action).toHaveAttribute("title", "Wireframe");
  await user.tab();
  expect(action).toHaveFocus();
  await user.keyboard(" ");
  expect(action).toHaveAttribute("aria-pressed", "true");
  expect(submit).not.toHaveBeenCalled();
  setDisabled(true);
  await user.click(action);
  expect(action).toBeDisabled();
  expect(action).toHaveAttribute("aria-pressed", "true");
});
