// Tests for RepoChipStrip repository selection and keyboard behavior.

import { render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import type { Repo } from "@sdk/types.gen";

import RepoChipStrip from "./RepoChipStrip";

const repoA: Repo = { path: "repos/a", branch: "main", baseBranch: { name: "main" } };
const repoB: Repo = { path: "repos/b", branch: "main", baseBranch: { name: "main" } };
const repoC: Repo = { path: "repos/c", branch: "main", baseBranch: { name: "main" } };

describe("RepoChipStrip", () => {
  it("selects repositories from the add dropdown with ArrowDown and Enter", async () => {
    const user = userEvent.setup();
    const onAdd = vi.fn();

    render(() => (
      <RepoChipStrip
        repos={() => [repoA, repoB, repoC]}
        selectedRepos={() => []}
        availableRecent={() => []}
        availableRest={() => [repoA, repoB, repoC]}
        onAdd={onAdd}
        onRemove={vi.fn()}
        onSetBranch={vi.fn()}
        showClone={false}
      />
    ));

    await user.click(screen.getByRole("button", { name: "Add a repository" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Add a repository" })).toHaveFocus());
    await user.keyboard("{ArrowDown}{Enter}");

    expect(onAdd).toHaveBeenCalledWith("repos/b");
    expect(screen.queryByRole("listbox", { name: "Add a repository" })).not.toBeInTheDocument();
  });
});
