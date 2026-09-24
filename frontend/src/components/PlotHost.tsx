// Observable Plot host: measures width, names the figure, rebuilds only on width or data change.

import { createEffect, createSignal, onCleanup, onMount } from "solid-js";

import styles from "./PlotHost.module.css";

// Below this width a plot stops being legible, so the host scrolls instead of
// shrinking the figure further.
const minPlotWidth = 280;

interface PlotHostProps {
  label: string;
  description?: string;
  draw: (width: number) => Element;
}

export default function PlotHost(props: PlotHostProps) {
  const [width, setWidth] = createSignal(0);
  // eslint-disable-next-line no-unassigned-vars -- assigned by SolidJS ref
  let host: HTMLDivElement | undefined;

  onMount(() => {
    if (!host) return;
    const measure = () => setWidth(Math.max(minPlotWidth, Math.floor(host?.getBoundingClientRect().width ?? 0)));
    measure();
    // The host is full width, so the observer already reports every container
    // resize; a window listener would only duplicate its work.
    if (typeof ResizeObserver === "undefined") {
      window.addEventListener("resize", measure);
      onCleanup(() => window.removeEventListener("resize", measure));
      return;
    }
    const observer = new ResizeObserver(measure);
    observer.observe(host);
    onCleanup(() => observer.disconnect());
  });

  createEffect(() => {
    const plotWidth = width();
    if (!host || plotWidth === 0) return;
    // Drawing through the effect tracks the data the draw reads, so a new sample
    // rebuilds the figure while unrelated renders of the parent do not.
    const plot = props.draw(plotWidth);
    // Name the graphic itself. A figure that also carries a legend keeps its
    // readable legend text, and the image states what it shows.
    const graphic = plot.localName === "svg" ? plot : (plot.querySelector("svg") ?? plot);
    graphic.setAttribute("role", "img");
    graphic.setAttribute("aria-label", props.label);
    if (props.description) graphic.setAttribute("aria-description", props.description);
    host.replaceChildren(plot);
  });

  return <div class={styles.chart} ref={host} />;
}
