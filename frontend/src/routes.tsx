// Route tree shared by the app entry point and tests: nested layout routes under the shell.
import { Route } from "@solidjs/router";
import type { JSX } from "solid-js";
import App from "./App";
import MainLayout from "./MainLayout";
import EmptyDetail from "./EmptyDetail";
import TaskDetailPane from "./TaskDetailPane";
import DiffPane from "./DiffPane";
import ProcessesPane from "./ProcessesPane";
import VncPane from "./VncPane";
import SettingsPane from "./SettingsPane";

/**
 * The application's route definitions. Returned as JSX (not a component) so it can be
 * embedded directly as <Router> children and rendered with the testing-library
 * `location` option, which supplies a MemoryRouter.
 */
export function appRoutes(): JSX.Element {
  return (
    <Route path="/" component={App}>
      <Route path="/" component={MainLayout}>
        <Route path="/" component={EmptyDetail} />
        <Route path="/task/:taskId" component={TaskDetailPane} />
        <Route path="/task/:taskId/diff" component={DiffPane} />
        <Route path="/task/:taskId/processes" component={ProcessesPane} />
        <Route path="/task/:taskId/vnc" component={VncPane} />
      </Route>
      <Route path="/settings" component={SettingsPane} />
    </Route>
  );
}
