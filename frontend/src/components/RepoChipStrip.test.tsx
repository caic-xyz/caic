// Tests for RepoChipStrip repository selection and keyboard behavior.

import { afterEach, describe, it } from "node:test";
import { vi, expect } from "@tests/expect";
import { render, screen, waitFor } from "@solidjs/testing-library";
import userEvent from "@testing-library/user-event";

import type { Repo } from "@sdk/types.gen";

import { api } from "../api";
import RepoChipStrip from "./RepoChipStrip";

const listRepoBranchesMock = vi.spyOn(api, "listRepoBranches");

const repoA: Repo = {
  path: "repos/a",
  branch: "main",
  baseBranch: { name: "main" },
};
const repoB: Repo = {
  path: "repos/b",
  branch: "main",
  baseBranch: { name: "main" },
};
const repoC: Repo = {
  path: "repos/c",
  branch: "main",
  baseBranch: { name: "main" },
};

describe("RepoChipStrip", () => {
  afterEach(() => {
    vi.resetAllMocks();
  });

  it("shows whether each branch will be adopted or branched off", async () => {
    const user = userEvent.setup();
    listRepoBranchesMock.mockResolvedValue({
      branches: [
        { name: "available", action: "adopt" },
        { name: "occupied", action: "branch_off" },
        { name: "remote-only", remote: "origin", action: "branch_off" },
      ],
    });

    render(() => (
      <RepoChipStrip
        repos={() => [repoA]}
        selectedRepos={() => [{ path: repoA.path, branch: "" }]}
        availableRecent={() => []}
        availableRest={() => []}
        onAdd={vi.fn()}
        onRemove={vi.fn()}
        onSetBranch={vi.fn()}
        showClone={false}
      />
    ));

    await user.click(screen.getByRole("button", { name: `Branch for ${repoA.path}` }));
    expect(await screen.findByRole("option", { name: /available Adopt/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /occupied Branch off/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /remote-only.*origin.*Branch off/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Default.*main.*Branch off/ })).toBeInTheDocument();

    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: `Branch for ${repoA.path}` }));
    await waitFor(() => expect(api.listRepoBranches).toHaveBeenCalledTimes(2));
  });

  it("selects repositories from the manager with ArrowDown and Enter", async () => {
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

    await user.click(screen.getByRole("button", { name: "Manage repositories" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Manage repositories" })).toHaveFocus());
    await user.keyboard("{ArrowDown}{Enter}");

    expect(onAdd).toHaveBeenCalledWith("repos/b");
    expect(screen.queryByRole("listbox", { name: "Manage repositories" })).not.toBeInTheDocument();
  });

  it("omits selected repositories from the add menu", async () => {
    const user = userEvent.setup();
    const onAdd = vi.fn();

    render(() => (
      <RepoChipStrip
        repos={() => [repoA, repoB]}
        selectedRepos={() => [{ path: repoA.path, branch: "" }]}
        availableRecent={() => []}
        availableRest={() => [repoA, repoB]}
        onAdd={onAdd}
        onRemove={vi.fn()}
        onSetBranch={vi.fn()}
        showClone={false}
      />
    ));

    await user.click(screen.getByRole("button", { name: "Manage repositories" }));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Manage repositories" })).toHaveFocus());
    expect(screen.queryByRole("option", { name: repoA.path })).not.toBeInTheDocument();
    expect(screen.getByRole("option", { name: repoB.path })).toBeInTheDocument();
    await user.keyboard("{Enter}");

    expect(onAdd).toHaveBeenCalledWith(repoB.path);
  });
});
