// Tests for the compact task card summary.

import { describe, it } from "node:test";
import { fireEvent, render, screen } from "@solidjs/testing-library";
import { MemoryRouter, Route } from "@solidjs/router";
import type { JSX } from "solid-js";
import { createSignal } from "solid-js";
import { expect, vi } from "@tests/expect";

import type { ISOTimestamp } from "@sdk/types.gen";
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
    tabIndex: 0,
    now,
    onClick: () => undefined,
    onError: () => undefined,
    purgeModifierActive: false,
    ...overrides,
  };
}

/**
 * Render a card as a matched route inside a memory router. The card's links are
 * router links: without a router the click falls through to the browser, which
 * jsdom refuses to follow across documents, and the router renders its children
 * as route definitions rather than as arbitrary content.
 */
function renderCard(card: () => JSX.Element) {
  return render(() => (
    <MemoryRouter>
      <Route path="*" component={card} />
    </MemoryRouter>
  ));
}

describe("TaskCard", () => {
  it("renders every repository and branch before Git status is available", () => {
    renderCard(() => (
      <TaskCard
        {...props({
          runtime: undefined,
          repos: [
            { name: "repo/primary", branch: "caic-1" },
            { name: "repo/extra", branch: "caic-2" },
          ],
        })}
      />
    ));

    const rows = screen.getAllByTestId("task-card-repo-state");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("repo/primary · caic-1");
    expect(rows[1]).toHaveTextContent("repo/extra · caic-2");
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("shows the complete repository-state component in the bottom row", async () => {
    renderCard(() => (
      <TaskCard
        {...props({
          repoStates: [
            {
              name: "repo",
              branch: "task-branch",
              ahead: 1,
              behind: 0,
              changedFiles: 2,
              linesAdded: 12,
              linesDeleted: 3,
              uncommittedFiles: 1,
              conflicts: 0,
            },
          ],
        })}
      />
    ));

    expect(await screen.findByRole("img")).toHaveAccessibleName(
      "2 changed files, 12 additions, 3 deletions, 1 uncommitted file, 1 commit ahead of upstream",
    );
    const repoStateRow = await screen.findByTestId("task-card-repo-state");
    expect(repoStateRow).toHaveTextContent("task-branch");
    expect(screen.getAllByText("task-branch")).toHaveLength(1);
    expect(screen.getByText("2f")).toBeInTheDocument();
    expect(screen.getByText("+12")).toBeInTheDocument();
    expect(screen.getByText("−3")).toBeInTheDocument();
  });

  it("updates the repository state when the task stream pushes new values", async () => {
    const [repoStates, setRepoStates] = createSignal<TaskCardProps["repoStates"]>([
      {
        name: "repo",
        branch: "task-branch",
        ahead: 1,
        behind: 0,
        changedFiles: 2,
        linesAdded: 12,
        linesDeleted: 3,
        uncommittedFiles: 1,
        conflicts: 0,
      },
    ]);
    renderCard(() => <TaskCard {...props()} repoStates={repoStates()} />);

    expect(await screen.findByRole("img")).toHaveAccessibleName(
      "2 changed files, 12 additions, 3 deletions, 1 uncommitted file, 1 commit ahead of upstream",
    );

    // The backend probe pushed a newer state; the card re-renders reactively
    // without any client-side fetch.
    setRepoStates([
      {
        name: "repo",
        branch: "task-branch",
        ahead: 3,
        behind: 0,
        changedFiles: 5,
        linesAdded: 20,
        linesDeleted: 4,
        uncommittedFiles: 0,
        conflicts: 0,
      },
    ]);
    expect(await screen.findByRole("img")).toHaveAccessibleName(
      "5 changed files, 20 additions, 4 deletions, 3 commits ahead of upstream",
    );
  });

  it("does not invent a TTL when only a legacy cache expiry is available", () => {
    renderCard(() => (
      <TaskCard
        {...props({
          state: "waiting",
          cacheExpiresAt: "2026-07-08T11:55:00Z",
        })}
      />
    ));

    fireEvent.focus(screen.getByRole("button", { name: "waiting" }));

    expect(screen.getByText("Prompt cache likely expired — continuing may use more tokens")).toBeInTheDocument();
    expect(screen.queryByText(/TTL/)).not.toBeInTheDocument();
  });

  it("shows a quota reset countdown in the summary", () => {
    const { getByTestId } = renderCard(() => (
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

  it("opens quota recovery without selecting the task card", () => {
    const onClick = vi.fn();
    const onQuotaRecovery = vi.fn();
    renderCard(() => (
      <TaskCard
        {...props({
          rateLimit: {
            blocked: true,
            window: "5h",
            resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
          },
          onClick,
          onQuotaRecovery,
        })}
      />
    ));

    fireEvent.click(screen.getByTestId("quota-recovery-card"));

    expect(onQuotaRecovery).toHaveBeenCalledOnce();
    expect(onClick).not.toHaveBeenCalled();
  });

  it("omits the token denominator until the context window is known", () => {
    const { unmount } = renderCard(() => (
      <TaskCard {...props({ activeInputTokens: 12_000, contextWindowLimit: 200_000 })} />
    ));
    expect(screen.getByTestId("task-card-tokens")).toHaveTextContent("12kt/200kt");
    unmount();

    renderCard(() => <TaskCard {...props({ activeInputTokens: 12_000, contextWindowLimit: 0 })} />);
    expect(screen.getByTestId("task-card-tokens")).toHaveTextContent("12kt");
    expect(screen.getByTestId("task-card-tokens")).not.toHaveTextContent("/");
  });

  it("hides quota recovery when the task has no repository", () => {
    renderCard(() => (
      <TaskCard
        {...props({
          repos: undefined,
          rateLimit: {
            blocked: true,
            window: "5h",
            resetsAt: "2026-07-08T12:42:00Z" as ISOTimestamp,
          },
          onQuotaRecovery: vi.fn(),
        })}
      />
    ));

    expect(screen.queryByTestId("quota-recovery-card")).not.toBeInTheDocument();
  });

  it("renders errors as a clamped summary", () => {
    const error = "Error: failed to load extension from a very long runtime path";
    const { getByText } = renderCard(() => <TaskCard {...props({ error })} />);

    expect(getByText(error).className).toContain("errorSummary");
  });

  it("renders a clickable origin task without selecting the child", () => {
    const onClick = vi.fn();
    const { getByRole } = renderCard(() => <TaskCard {...props({ forkedFromTaskID: "3BL0EKDTO000", onClick })} />);

    const link = getByRole("link", { name: "3BL0EKDTO000" });
    expect(link).toHaveAttribute("href", "/task/@3BL0EKDTO000");
    fireEvent.click(link);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("renders a clickable parent task without selecting the child", () => {
    const onClick = vi.fn();
    const { getByRole } = renderCard(() => <TaskCard {...props({ parentTaskID: "3BL0EKDTO001", onClick })} />);

    const link = getByRole("link", { name: "3BL0EKDTO001" });
    expect(link).toHaveAttribute("href", "/task/@3BL0EKDTO001");
    fireEvent.click(link);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("shows a child origin once when it matches the fork source", () => {
    renderCard(() => <TaskCard {...props({ forkedFromTaskID: "parent", parentTaskID: "parent" })} />);

    expect(screen.getByText("child of")).toBeInTheDocument();
    expect(screen.queryByText("forked from")).not.toBeInTheDocument();
  });

  it("selects a stopped task before exposing its inline actions", () => {
    const onClick = vi.fn();
    const onPurge = vi.fn();
    const { container, unmount } = renderCard(() => (
      <TaskCard {...props({ state: "stopped", onClick, onPurge, onRevive: vi.fn() })} />
    ));
    const card = container.querySelector("[data-task-id='1']");
    if (!card) throw new Error("task card not rendered");

    expect(screen.queryByTestId("purge-task")).not.toBeInTheDocument();
    expect(screen.queryByTestId("revive-task")).not.toBeInTheDocument();

    fireEvent.click(card);

    expect(onClick).toHaveBeenCalledOnce();
    expect(onPurge).not.toHaveBeenCalled();

    unmount();
    renderCard(() => <TaskCard {...props({ state: "stopped", selected: true, onPurge, onRevive: vi.fn() })} />);

    expect(screen.getByTestId("purge-task")).toBeInTheDocument();
    expect(screen.getByTestId("revive-task")).toBeInTheDocument();
  });

  it("opens the task actions menu on right click", () => {
    const onClick = vi.fn();
    const onStop = vi.fn();
    const { container } = renderCard(() => (
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

  it("does not repeat stop and confirms before purging from a double-click", () => {
    const onStop = vi.fn();
    const onPurge = vi.fn();
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const { getByTestId } = renderCard(() => <TaskCard {...props({ state: "waiting", onStop, onPurge })} />);
    const stopButton = getByTestId("stop-task");

    fireEvent.click(stopButton, { detail: 1 });
    fireEvent.click(stopButton, { detail: 2 });
    expect(onStop).toHaveBeenCalledOnce();

    fireEvent.dblClick(stopButton);

    expect(confirm).toHaveBeenCalledWith("Purge runtime instance?\n\nTask\nbranch: task-branch");
    expect(onPurge).not.toHaveBeenCalled();
  });

  it("shows the stop icon normally and the purge icon for the Shift modifier", () => {
    const { getByRole, getByTestId, queryByTestId, unmount } = renderCard(() => (
      <TaskCard {...props({ state: "waiting", onStop: vi.fn(), onPurge: vi.fn() })} />
    ));

    expect(getByRole("button", { name: "Stop" })).toBeInTheDocument();
    expect(getByTestId("stop-task-icon")).toBeInTheDocument();
    expect(queryByTestId("purge-task-icon")).not.toBeInTheDocument();
    unmount();

    renderCard(() => (
      <TaskCard
        {...props({
          state: "waiting",
          onStop: vi.fn(),
          onPurge: vi.fn(),
          purgeModifierActive: true,
        })}
      />
    ));

    expect(screen.getByRole("button", { name: "Purge" })).toBeInTheDocument();
    expect(screen.getByTestId("purge-task-icon")).toBeInTheDocument();
    expect(screen.queryByTestId("stop-task-icon")).not.toBeInTheDocument();
  });
});
