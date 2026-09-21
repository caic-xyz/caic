// Tests compact Git state markers for task repository rows.

import type { JSX } from "solid-js";
import { it } from "node:test";
import { Route, Router } from "@solidjs/router";
import { fireEvent, render, screen, waitFor } from "@solidjs/testing-library";
import { expect, vi } from "@tests/expect";

import RepoStateIcons from "./RepoStateIcons";

function renderWithRouter(ui: () => JSX.Element) {
  return render(() => (
    <Router>
      <Route path="/*" component={ui} />
    </Router>
  ));
}

it("renders dense markers with an accessible state summary", () => {
  renderWithRouter(() => (
    <RepoStateIcons
      state={{
        name: "repo",
        branch: "main",
        ahead: 2,
        behind: 1,
        changedFiles: 4,
        linesAdded: 17,
        linesDeleted: 2,
        uncommittedFiles: 3,
        conflicts: 1,
        operation: "rebase",
      }}
    />
  ));

  expect(screen.getByRole("img")).toHaveAccessibleName(
    "4 changed files, 17 additions, 2 deletions, 1 conflict, rebase in progress, 3 uncommitted files, 2 commits ahead of upstream, 1 commit behind upstream",
  );
  expect(screen.getByText("4f")).toBeInTheDocument();
  expect(screen.getByText("+17")).toBeInTheDocument();
  expect(screen.getByText("−2")).toBeInTheDocument();
  expect(screen.getByText("3")).toBeInTheDocument();
  expect(screen.getByText("2")).toBeInTheDocument();
});

it("does not render a clean repository", () => {
  renderWithRouter(() => (
    <RepoStateIcons
      state={{
        name: "repo",
        branch: "main",
        ahead: 0,
        behind: 0,
        changedFiles: 0,
        linesAdded: 0,
        linesDeleted: 0,
        uncommittedFiles: 0,
        conflicts: 0,
      }}
    />
  ));

  expect(screen.queryByRole("img")).not.toBeInTheDocument();
});

it("elides line totals when its row overflows", async () => {
  renderWithRouter(() => (
    <RepoStateIcons
      state={{
        name: "repo",
        branch: "main",
        ahead: 1,
        behind: 0,
        changedFiles: 4,
        linesAdded: 17,
        linesDeleted: 2,
        uncommittedFiles: 3,
        conflicts: 0,
      }}
    />
  ));

  const marker = screen.getByRole("img");
  const row = marker.parentElement;
  if (!row) throw new Error("repository state marker has no row");
  Object.defineProperties(row, {
    clientWidth: { configurable: true, value: 40 },
    scrollWidth: { configurable: true, value: 100 },
  });
  window.dispatchEvent(new Event("resize"));

  await waitFor(() => expect(marker).toHaveAttribute("data-elide-diff-stats", ""));
  expect(marker).toHaveAccessibleName(
    "4 changed files, 17 additions, 2 deletions, 3 uncommitted files, 1 commit ahead of upstream",
  );
});

it("accepts a parent layout request to elide line totals", () => {
  renderWithRouter(() => (
    <RepoStateIcons
      elideDiffStats
      state={{
        name: "repo",
        branch: "main",
        ahead: 1,
        behind: 0,
        changedFiles: 4,
        linesAdded: 17,
        linesDeleted: 2,
        uncommittedFiles: 0,
        conflicts: 0,
      }}
    />
  ));

  const marker = screen.getByRole("img");
  expect(marker).toHaveAttribute("data-elide-diff-stats", "");
  expect(marker).toHaveAccessibleName("4 changed files, 17 additions, 2 deletions, 1 commit ahead of upstream");
});

it("links a state marker to its repository diff", () => {
  renderWithRouter(() => (
    <RepoStateIcons
      href="/task/@abc+task/diff"
      state={{
        name: "repo",
        branch: "main",
        ahead: 1,
        behind: 0,
        changedFiles: 1,
        linesAdded: 1,
        linesDeleted: 0,
        uncommittedFiles: 0,
        conflicts: 0,
      }}
    />
  ));

  expect(
    screen.getByRole("link", {
      name: "repo: 1 changed file, 1 addition, 1 commit ahead of upstream",
    }),
  ).toHaveAttribute("href", "/task/@abc+task/diff");
  expect(screen.queryByRole("img")).not.toBeInTheDocument();
});

it("reports keyboard and pointer navigation intent", () => {
  const onNavigateIntent = vi.fn();
  renderWithRouter(() => (
    <RepoStateIcons
      href="/task/@abc+task/diff"
      onNavigateIntent={onNavigateIntent}
      state={{
        name: "repo",
        branch: "main",
        ahead: 1,
        behind: 0,
        changedFiles: 1,
        linesAdded: 1,
        linesDeleted: 0,
        uncommittedFiles: 0,
        conflicts: 0,
      }}
    />
  ));
  const link = screen.getByRole("link");
  fireEvent.focus(link);
  fireEvent.pointerEnter(link);
  fireEvent.pointerDown(link);
  expect(onNavigateIntent).toHaveBeenCalledTimes(3);
});
