// Material Symbol named for assistive technology and on hover, sized to the surrounding transcript text.

import type { Component, JSX } from "solid-js";
import { Dynamic } from "solid-js/web";

import styles from "./NamedIcon.module.css";

// NamedIcon replaces a word with its symbol; label keeps the word for screen
// readers and the tooltip. The host passes `class` to space it within its row.
export default function NamedIcon(props: {
  icon: Component<JSX.SvgSVGAttributes<SVGSVGElement>>;
  label: string;
  class?: string;
}) {
  return (
    <span
      class={props.class ? `${styles.icon} ${props.class}` : styles.icon}
      role="img"
      aria-label={props.label}
      title={props.label}
    >
      <Dynamic component={props.icon} aria-hidden="true" />
    </span>
  );
}
