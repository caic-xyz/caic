// Tests for the compact task card summary.

import { render } from "@solidjs/testing-library";
import { describe, expect, it } from "vitest";

import type { TaskCardProps } from "./TaskCard";

import TaskCard from "./TaskCard";

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
    now,
    onClick: () => undefined,
    ...overrides,
  };
}

describe("TaskCard", () => {
  it("shows a quota reset countdown in the summary", () => {
    const { getByTestId } = render(() => (
      <TaskCard
        {...props({
          quotaCountdown: {
            providerLabel: "Anthropic",
            window: "5h",
            resetsAt: "2026-07-08T12:42:00Z",
          },
        })}
      />
    ));

    expect(getByTestId("quota-countdown")).toHaveTextContent("quota resets in 42m");
  });

  it("renders errors as a clamped summary", () => {
    const error = "Error: failed to load extension from a very long runtime path";
    const { getByText } = render(() => <TaskCard {...props({ error })} />);

    expect(getByText(error).className).toContain("errorSummary");
  });
});
