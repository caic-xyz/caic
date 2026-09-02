// Tests for the compact task card summary.

import { fireEvent, render, screen } from "@solidjs/testing-library";
import type { JSX } from "solid-js";
import { describe, expect, it, vi } from "vitest";

import type { ISOTimestamp } from "@sdk/types.gen";
import type { TaskCardProps } from "./TaskCard";

import TaskCard from "./TaskCard";

vi.mock("@solidjs/router", () => ({
  A: (linkProps: { href: string; class?: string; title?: string; onClick?: (event: MouseEvent) => void; children: JSX.Element }) => (
    <a class={linkProps.class} href={linkProps.href} title={linkProps.title} onClick={(event) => linkProps.onClick?.(event)}>{linkProps.children}</a>
  ),
}));

const now = () => Date.parse("2026-07-08T12:00:00Z");

function props(overrides: Partial<TaskCardProps> = {}): TaskCardProps {
  return {
    id: "1",
    title: "Task",
    state: "running",
    stateUpdatedAt: "2026-07-08T11:59:00Z" as TaskCardProps["stateUpdatedAt"],
    repos: [{ name: "repo", branch: "task-branch" }],
    harness: "claude",
    model: "claude-sonnet-4",
    costUSD: 0,
    duration: 0,
    numTurns: 0,
    activeInputTokens: 0,
    activeCacheReadTokens: 0,
    cumulativeInputTokens: 0,
    cumulativeCacheCreationInputTokens: 0,
    cumulativeCacheReadInputTokens: 0,
    cumulativeOutputTokens: 0,
    contextWindowLimit: 200_000,
    runtime: { id: "rt" },
    selected: false,
    tabIndex: 0,
    now,
    onClick: () => undefined,
    onError: () => undefined,
    ...overrides,
  };
}

describe("TaskCard", () => {
  it("does not invent a TTL when only a legacy cache expiry is available", () => {
    render(() => <TaskCard {...props({
      state: "waiting",
      cacheExpiresAt: "2026-07-08T11:55:00Z",
    })} />);

    fireEvent.focus(screen.getByRole("button", { name: "waiting" }));

    expect(screen.getByText("Prompt cache likely expired — continuing may use more tokens")).toBeInTheDocument();
    expect(screen.queryByText(/TTL/)).not.toBeInTheDocument();
  });

  it("shows a quota reset countdown in the summary", () => {
    const { getByTestId } = render(() => (
      <TaskCard
        {...props({
          rateLimit: {
            blocked: true,
            window: "five_hour",
            resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
          },
        })}
      />
    ));

    expect(getByTestId("quota-countdown")).toHaveTextContent("out of quota · resets in 42m");
  });

  it("renders errors as a clamped summary", () => {
    const error = "Error: failed to load extension from a very long runtime path";
    const { getByText } = render(() => <TaskCard {...props({ error })} />);

    expect(getByText(error).className).toContain("errorSummary");
  });

  it("renders a clickable origin task without selecting the child", () => {
    const onClick = vi.fn();
    const { getByRole } = render(() => (
      <TaskCard {...props({ forkedFromTaskID: "3BL0EKDTO000", onClick })} />
    ));

    const link = getByRole("link", { name: "3BL0EKDTO000" });
    expect(link).toHaveAttribute("href", "/task/@3BL0EKDTO000");
    fireEvent.click(link);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("opens the task actions menu on right click", () => {
    const onClick = vi.fn();
    const onStop = vi.fn();
    const { container } = render(() => (
      <TaskCard
        {...props({
          repos: [
            {
              name: "repo",
              branch: "task-branch",
              baseBranch: "main",
              forge: "github",
            },
          ],
          forgePR: 42,
          supportsCompact: true,
          onClick,
          onStop,
          onPurge: vi.fn(),
          onFork: vi.fn(),
        })}
      />
    ));
    const card = container.querySelector("[data-task-id='1']");
    if (!card) throw new Error("task card not rendered");

    fireEvent.contextMenu(card, { clientX: 20, clientY: 30 });

    expect(screen.getByRole("menu")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Push" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Push to main" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Stop" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Purge" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Clear context" })).toBeDisabled();
    expect(screen.getByRole("menuitem", { name: "Compact context" })).toBeDisabled();
    expect(screen.getByRole("menuitem", { name: "Fork" })).toBeInTheDocument();

    const stopButton = screen.getByRole("menuitem", { name: "Stop" });
    fireEvent.pointerDown(stopButton);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    fireEvent.click(stopButton);

    expect(onStop).toHaveBeenCalledOnce();
    expect(onClick).not.toHaveBeenCalled();
  });
});
