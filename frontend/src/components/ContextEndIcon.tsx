// Material Symbol naming how an archived context section ended: compaction, a context clear, or a new session.

import type { Component, JSX } from "solid-js";
import CompressIcon from "@material-symbols/svg-400/outlined/compress.svg?solid";
import DeleteSweepIcon from "@material-symbols/svg-400/outlined/delete_sweep.svg?solid";
import RestartAltIcon from "@material-symbols/svg-400/outlined/restart_alt.svg?solid";

import { type ContextEndCause, contextEndLabels } from "../contextEnd";
import NamedIcon from "./NamedIcon";
import styles from "./ContextEndIcon.module.css";

const icons: Record<ContextEndCause, Component<JSX.SvgSVGAttributes<SVGSVGElement>>> = {
  compaction: CompressIcon,
  clear: DeleteSweepIcon,
  session: RestartAltIcon,
};

export default function ContextEndIcon(props: { cause: ContextEndCause }) {
  return <NamedIcon icon={icons[props.cause]} label={contextEndLabels[props.cause]} class={styles.icon} />;
}
