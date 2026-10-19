// MonitoringPage groups lazy usage, system, and metrics views behind URL-addressable navigation.

import { useLocation, useSearchParams } from "@solidjs/router";
import { createMemo, For, lazy, Match, Suspense, Switch } from "solid-js";

import { Layout } from "../components/Layout";
import styles from "./MonitoringPage.module.css";

const UsagePage = lazy(() => import("./UsagePage"));
const SystemPage = lazy(() => import("./SystemPage"));
const MetricsPage = lazy(() => import("./MetricsPage"));
const sections = [
  { id: "usage", label: "Usage" },
  { id: "system", label: "System" },
  { id: "metrics", label: "Metrics" },
];

export default function MonitoringPage() {
  const [search] = useSearchParams();
  const location = useLocation();
  const section = createMemo(() =>
    search.section === "system" || search.section === "metrics" ? search.section : "usage",
  );
  const href = (id: string) => {
    const params = new URLSearchParams(location.search);
    if (id === "usage") params.delete("section");
    else params.set("section", id);
    const query = params.toString();
    return `/monitoring${query ? `?${query}` : ""}`;
  };

  return (
    <Layout>
      <main class={styles.page}>
        <div class={styles.panel}>
          <h1 class={styles.title}>Monitoring</h1>
          <nav class={styles.sections} aria-label="Monitoring sections">
            <For each={sections}>
              {(item) => (
                <a link href={href(item.id)} aria-current={section() === item.id ? "page" : undefined}>
                  {item.label}
                </a>
              )}
            </For>
          </nav>
          <Suspense fallback={<p>Loading monitoring…</p>}>
            <Switch>
              <Match when={section() === "usage"}>
                <UsagePage />
              </Match>
              <Match when={section() === "system"}>
                <SystemPage />
              </Match>
              <Match when={section() === "metrics"}>
                <MetricsPage />
              </Match>
            </Switch>
          </Suspense>
        </div>
      </main>
    </Layout>
  );
}
