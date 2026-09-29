// Tests for mobile voice task ordering, active-state filtering, and live updates.

import { describe, it } from "node:test";
import { render } from "@solidjs/testing-library";
import { MemoryRouter, Route } from "@solidjs/router";
import { createSignal } from "solid-js";
import { expect } from "@tests/expect";

import type { Task } from "@sdk/types.gen";

import MobileVoiceTasks, { type MobileVoiceTasksProps } from "./MobileVoiceTasks";

function task(id: string, state: Task["state"], title: string): Task {
  return { id, state, title } as Task;
}

function renderVoiceTasks(props: MobileVoiceTasksProps) {
  return render(() => (
    <MemoryRouter>
      <Route path="*" component={() => <MobileVoiceTasks {...props} />} />
    </MemoryRouter>
  ));
}

describe("MobileVoiceTasks", () => {
  it("orders active tasks by voice number and omits inactive tasks", () => {
    const tasks = [
      task("6", "pending", "Pending title"),
      task("2", "asking", "Needs an answer"),
      task("3", "stopped", "Stopped title"),
      task("1", "running", "Running title"),
      task("4", "waiting", "Ready for input"),
      task("5", "has_plan", "Plan to review"),
      task("7", "starting", "Starting title"),
      task("8", "pulling", "Pulling title"),
      task("9", "pushing", "Pushing title"),
      task("10", "branching", "Branching title"),
      task("11", "provisioning", "Provisioning title"),
    ];
    const { getByTestId, getByRole, queryByRole } = renderVoiceTasks({
      tasks: () => tasks,
      tasksLoading: () => false,
      settledLoading: () => false,
      getTaskNumber: (id) => Number(id),
    });

    const rows = Array.from(getByTestId("mobile-voice-tasks").querySelectorAll("li"));
    expect(rows.map((row) => row.textContent)).toEqual([
      "#1Running title",
      "#2Needs an answer",
      "#4Ready for input",
      "#5Plan to review",
      "#6Pending title",
      "#7Starting title",
      "#8Pulling title",
      "#9Pushing title",
      "#10Branching title",
      "#11Provisioning title",
    ]);
    expect(rows.map((row) => row.querySelector("a")?.getAttribute("data-state"))).toEqual([
      "running",
      "asking",
      "waiting",
      "has_plan",
      "pending",
      "starting",
      "pulling",
      "pushing",
      "branching",
      "provisioning",
    ]);
    expect(getByRole("link", { name: "Task 2, asking: Needs an answer" })).toHaveAttribute("href", "/task/@2");
    expect(queryByRole("heading")).toBeNull();
    expect(queryByRole("link", { name: /Stopped title/ })).toBeNull();
  });

  it("keeps task rows mounted across live title updates", () => {
    const [tasks, updateTasks] = createSignal([task("1", "running", "Initial title")]);
    renderVoiceTasks({
      tasks,
      tasksLoading: () => false,
      settledLoading: () => false,
      getTaskNumber: () => 1,
    });
    const row = document.querySelector("[data-task-id='1']");
    updateTasks([task("1", "running", "Updated title")]);
    expect(document.querySelector("[data-task-id='1']")).toBe(row);
    expect(row).toHaveTextContent("Updated title");

    updateTasks([task("1", "stopped", "Updated title")]);
    expect(document.querySelector("[data-task-id='1']")).toBeNull();
    expect(document.querySelector("[data-testid='mobile-voice-tasks']")).toHaveTextContent("No active tasks.");
  });
});
