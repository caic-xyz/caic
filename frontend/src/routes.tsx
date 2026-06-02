// Route tree shared by the app entry point and tests: nested layout routes under the shell.
import { Route } from "@solidjs/router";
import type { JSX } from "solid-js";
import App from "./App";
import MainLayout from "./components/MainLayout";
import EmptyPage from "./pages/EmptyPage";
import TaskDetailPage from "./pages/TaskDetailPage";
import DiffPage from "./pages/DiffPage";
import ProcessesPage from "./pages/ProcessesPage";
import VncPage from "./pages/VncPage";
import SettingsPage from "./pages/SettingsPage";

/**
 * The application's route definitions. Returned as JSX (not a component) so it can be
 * embedded directly as <Router> children and rendered with the testing-library
 * `location` option, which supplies a MemoryRouter.
 */
export function appRoutes(): JSX.Element {
  return (
    <Route path="/" component={App}>
      <Route path="/" component={MainLayout}>
        <Route path="/" component={EmptyPage} />
        <Route path="/task/:taskId" component={TaskDetailPage} />
        <Route path="/task/:taskId/diff" component={DiffPage} />
        <Route path="/task/:taskId/processes" component={ProcessesPage} />
        <Route path="/task/:taskId/vnc" component={VncPage} />
      </Route>
      <Route path="/settings" component={SettingsPage} />
    </Route>
  );
}
