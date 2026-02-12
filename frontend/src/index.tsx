import "./global.css";
import { render } from "solid-js/web";
import { Router, Route } from "@solidjs/router";
import App from "./App";

const root = document.getElementById("app");
if (root) {
  render(
    () => (
      <Router>
        <Route path="*" component={App} />
      </Router>
    ),
    root,
  );
}
