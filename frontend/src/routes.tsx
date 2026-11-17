// Application routes keep task creation and conversations eager and load optional pages on navigation.

import { Route } from "@solidjs/router";
import { lazy, type JSX } from "solid-js";

import App from "./App";
import { DeviceLoginPage, DeviceApprovalPage } from "./pages/DeviceLoginPage";
import MainLayout from "./components/MainLayout";
import EmptyPage from "./pages/EmptyPage";
import TaskWorkspace from "./components/TaskWorkspace";

const DiffPage = lazy(() => import("./pages/DiffPage"));
const FileViewerPage = lazy(() => import("./pages/FileViewerPage"));
const MonitoringPage = lazy(() => import("./pages/MonitoringPage"));
const ProcessesPage = lazy(() => import("./pages/ProcessesPage"));
const SessionsPage = lazy(() => import("./pages/SessionsPage"));
const SettingsPage = lazy(() => import("./pages/SettingsPage"));
const StatsPage = lazy(() => import("./pages/StatsPage"));
const TaskInfoPage = lazy(() => import("./pages/TaskInfoPage"));
const VncPage = lazy(() => import("./pages/VncPage"));

/**
 * The application's route definitions. Returned as JSX (not a component) so it can be
 * embedded directly as <Router> children and rendered with the testing-library
 * `location` option, which supplies a MemoryRouter.
 */
export function appRoutes(): JSX.Element {
  return (
    <>
      <Route path="/device-login" component={DeviceLoginPage} />
      <Route path="/device" component={DeviceApprovalPage} />
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
        <Route path="/sessions" component={SessionsPage} />
        <Route path="/settings" component={SettingsPage} />
        <Route path="/monitoring" component={MonitoringPage} />
      </Route>
    </>
  );
}
