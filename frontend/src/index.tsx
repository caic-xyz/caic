// Application entry point — mounts the router and top-level providers.

import { render } from "solid-js/web";
import { Router } from "@solidjs/router";
import { configureMcpClient } from "@maruel/gomode/web/McpClient";

import "./global.css";
import { AuthProvider } from "./AuthContext";
import { appRoutes } from "./routes";

const root = document.getElementById("app");
if (root) {
  configureMcpClient("/api/caic/v1/mcp", "caic-frontend");
  render(
    () => (
      <AuthProvider>
        <Router explicitLinks>{appRoutes()}</Router>
      </AuthProvider>
    ),
    root,
  );
}
