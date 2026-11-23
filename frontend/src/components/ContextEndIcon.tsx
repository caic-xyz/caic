// Material Symbol naming how an archived context section ended: compaction, a context clear, or a new session.

import type { Component, JSX } from "solid-js";
import { Dynamic } from "solid-js/web";
import CompressIcon from "@material-symbols/svg-400/outlined/compress.svg?solid";
import DeleteSweepIcon from "@material-symbols/svg-400/outlined/delete_sweep.svg?solid";
import RestartAltIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";

import { type ContextEndCause, contextEndLabels } from "../contextEnd";
import styles from "./ContextEndIcon.module.css";

const icons: Record<ContextEndCause, Component<JSX.SvgSVGAttributes<SVGSVGElement>>> = {
  compaction: CompressIcon,
  clear: DeleteSweepIcon,
  session: RestartAltIcon,
};

// ContextEndIcon names its cause for assistive technology and on hover.
export default function ContextEndIcon(props: { cause: ContextEndCause }) {
  return (
    <span
      class={styles.icon}
      role="img"
      aria-label={contextEndLabels[props.cause]}
      title={contextEndLabels[props.cause]}
    >
      <Dynamic component={icons[props.cause]} aria-hidden="true" />
    </span>
  );
}
