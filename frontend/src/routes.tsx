// Route tree shared by the app entry point and tests: nested layout routes under the shell.

import { Route } from "@solidjs/router";
import type { JSX } from "solid-js";

import App from "./App";
import MainLayout from "./components/MainLayout";
import EmptyPage from "./pages/EmptyPage";
import TaskWorkspace from "./components/TaskWorkspace";
import DiffPage from "./pages/DiffPage";
import FileViewerPage from "./pages/FileViewerPage";
import ProcessesPage from "./pages/ProcessesPage";
import StatsPage from "./pages/StatsPage";
import TaskInfoPage from "./pages/TaskInfoPage";
import VncPage from "./pages/VncPage";
import SettingsPage from "./pages/SettingsPage";
import MetricsPage from "./pages/MetricsPage";
import UsagePage from "./pages/UsagePage";

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
        <Route path="/task/:taskId" component={TaskWorkspace}>
          <Route path="/" component={() => null} />
          <Route path="/view" component={FileViewerPage} />
          <Route path="/diff" component={DiffPage} />
          <Route path="/processes" component={ProcessesPage} />
          <Route path="/stats" component={StatsPage} />
          <Route path="/info" component={TaskInfoPage} />
          <Route path="/vnc" component={VncPage} />
        </Route>
      </Route>
      <Route path="/settings" component={SettingsPage} />
      <Route path="/metrics" component={MetricsPage} />
      <Route path="/usage" component={UsagePage} />
    </Route>
  );
}
