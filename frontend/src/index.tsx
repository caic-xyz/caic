// Application entry point — mounts the router and top-level providers.
import "./global.css";
import { render } from "solid-js/web";
import { Router } from "@solidjs/router";
import { AuthProvider } from "./AuthContext";
import { appRoutes } from "./routes";

const root = document.getElementById("app");
if (root) {
  render(
    () => (
      <AuthProvider>
        <Router explicitLinks>{appRoutes()}</Router>
      </AuthProvider>
    ),
    root,
  );
}
