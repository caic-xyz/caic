// Application entry point — mounts the router and top-level providers.
import "./global.css";
import { render } from "solid-js/web";
import { Router, Route } from "@solidjs/router";
import App from "./App";
import { AuthProvider } from "./AuthContext";

const root = document.getElementById("app");
if (root) {
  render(
    () => (
      <AuthProvider>
        <Router explicitLinks>
          <Route path="*" component={App} />
        </Router>
      </AuthProvider>
    ),
    root,
  );
}
